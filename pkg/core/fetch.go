package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/cloudquery/cloudquery/internal/analytics"
	cqsort "github.com/cloudquery/cloudquery/internal/sort"
	"github.com/cloudquery/cloudquery/pkg/config"
	"github.com/cloudquery/cloudquery/pkg/core/database"
	"github.com/cloudquery/cloudquery/pkg/core/state"
	cqerrors "github.com/cloudquery/cloudquery/pkg/errors"
	"github.com/cloudquery/cloudquery/pkg/plugin"
	"github.com/cloudquery/cloudquery/pkg/plugin/registry"
	"github.com/cloudquery/cq-provider-sdk/cqproto"
	"github.com/cloudquery/cq-provider-sdk/database/dsn"
	"github.com/cloudquery/cq-provider-sdk/provider/diag"
	"github.com/cloudquery/cq-provider-sdk/provider/schema"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/thoas/go-funk"
)

type FetchStatus int

// ProviderFetchSummary represents a request for the FetchFinishCallback
type ProviderFetchSummary struct {
	Name                  string                          `json:"name,omitempty"`
	Alias                 string                          `json:"alias,omitempty"`
	Version               string                          `json:"version,omitempty"`
	TotalResourcesFetched uint64                          `json:"total_resources_fetched,omitempty"`
	FetchedResources      map[string]ResourceFetchSummary `json:"fetch_resources,omitempty"`
	Status                FetchStatus                     `json:"status,omitempty"`
	Duration              time.Duration                   `json:"duration,omitempty"`
}

type ResourceFetchSummary struct {
	// Execution status of resource
	Status string `json:"status,omitempty"`
	// Total Amount of resources collected by this resource
	ResourceCount uint64 `json:"resource_count,omitempty"`
	// Diagnostics of failed resource fetch, the diagnostic provides insights such as severity, summary and
	// details on how to solve this issue
	Diagnostics diag.Diagnostics `json:"-"`
	// TelemetryEvents is a list of telemetry events that occurred during the fetch
	TelemetryEvents []analytics.TelemetryEvent `json:"-"`
	// Duration in seconds
	Duration time.Duration `json:"duration,omitempty"`
}

type FetchUpdateCallback func(update FetchUpdate)

type FetchUpdate struct {
	Name    string
	Alias   string
	Version string
	// Map of resources that have finished fetching
	FinishedResources map[string]bool
	// Amount of resources collected so far
	ResourceCount uint64
	// Error if any returned by the provider
	Error string
	// Diagnostic count
	DiagnosticCount int
}

// FetchResponse is returned after a successful fetch execution, it holds a fetch summary for each provider that was executed.
type FetchResponse struct {
	FetchId              uuid.UUID                        `json:"fetch_id,omitempty"`
	ProviderFetchSummary map[string]*ProviderFetchSummary `json:"provider_fetch_summary,omitempty"`
	TotalFetched         uint64                           `json:"total_fetched,omitempty"`
	Duration             time.Duration                    `json:"total_fetch_time,omitempty"`
	TelemetryEvents      []analytics.TelemetryEvent       `json:"-"`
}

type ProviderInfo struct {
	Provider registry.Provider
	Config   *config.Provider
}

// FetchOptions is provided to the Client to execute a fetch on one or more providers
type FetchOptions struct {
	// UpdateCallback allows gets called when the client receives updates on fetch.
	UpdateCallback FetchUpdateCallback
	// Providers list of providers to call for fetching
	ProvidersInfo []ProviderInfo
	// Optional: Adds extra fields to the provider
	ExtraFields map[string]interface{}
	// Optional: unique identifier for the fetch, if this isn't given, a random one is generated.
	FetchId uuid.UUID
}

type fetchResult struct {
	summary *ProviderFetchSummary
	diags   diag.Diagnostics
}

const (
	FetchFailed FetchStatus = iota + 1
	FetchConfigureFailed
	FetchCanceled
	FetchFinished
	FetchPartial
)

func (fs FetchStatus) String() string {
	switch fs {
	case FetchFailed:
		return "failed"
	case FetchCanceled:
		return "canceled"
	case FetchFinished:
		return "successful"
	case FetchPartial:
		return "partial"
	case FetchConfigureFailed:
		return "configure_failed"
	default:
		return "unknown"
	}
}

func (p ProviderFetchSummary) Resources() []string {
	rr := make([]string, 0, len(p.FetchedResources))
	for r := range p.FetchedResources {
		rr = append(rr, r)
	}
	return rr
}

func (p ProviderFetchSummary) String() string {
	if p.Alias != "" {
		return fmt.Sprintf("%s(%s)", p.Name, p.Alias)
	}
	return p.Name
}

func (p ProviderFetchSummary) Diagnostics() diag.Diagnostics {
	var allDiags diag.Diagnostics
	for _, s := range p.FetchedResources {
		allDiags = append(allDiags, s.Diagnostics...)
	}
	return allDiags
}

func (p ProviderFetchSummary) Properties() map[string]interface{} {
	rd := make(map[string]float64, len(p.FetchedResources))
	for rn, r := range p.FetchedResources {
		rd[rn] = math.Round(r.Duration.Seconds()*100) / 100
	}
	return map[string]interface{}{
		"fetch_provider":              p.Name,
		"fetch_provider_version":      p.Version,
		"fetch_resources":             p.Resources(),
		"fetch_total_resources_count": p.TotalResourcesFetched,
		"fetch_resources_durations":   rd,
		"fetch_duration":              math.Round(p.Duration.Seconds()*100) / 100,
		"fetch_diags":                 analytics.SummarizeDiagnostics(p.Diagnostics()),
		"fetch_status":                p.Status.String(),
	}
}

func (f FetchUpdate) AllDone() bool {
	for _, v := range f.FinishedResources {
		if !v {
			return false
		}
	}
	return true
}

func (f FetchUpdate) DoneCount() int {
	count := 0
	for _, v := range f.FinishedResources {
		if v {
			count++
		}
	}
	return count
}

func (fr FetchResponse) HasErrors() bool {
	for _, p := range fr.ProviderFetchSummary {
		if p.Diagnostics().HasErrors() {
			return true
		}
	}
	return false
}

func Fetch(ctx context.Context, sta *state.Client, storage database.Storage, pm *plugin.Manager, opts *FetchOptions) (res *FetchResponse, diagnostics diag.Diagnostics) {
	var err error
	fetchId := opts.FetchId
	if fetchId == uuid.Nil {
		fetchId, err = uuid.NewUUID()
		if err != nil {
			return nil, diag.FromError(err, diag.INTERNAL)
		}
	}
	// set metadata we want to pass to
	metadata := map[string]interface{}{schema.FetchIdMetaKey: fetchId}
	log.Info().Interface("extra_fields", opts.ExtraFields).Msg("Received fetch request")

	var (
		diags          diag.Diagnostics
		fetchSummaries = make(chan fetchResult, len(opts.ProvidersInfo))
		wg             sync.WaitGroup
		start          = time.Now()
	)

	dsnURI, err := parseDSN(storage)
	if err != nil {
		return nil, diag.FromError(err, diag.INTERNAL)
	}
	for _, providerInfo := range opts.ProvidersInfo {
		if len(providerInfo.Config.Resources) == 0 {
			log.Warn().Str("provider", providerInfo.Config.Name).Str("alias", providerInfo.Config.Alias).Msg("Skipping provider which configured with 0 resources to fetch")
			diags = diags.Add(diag.FromError(nil, diag.INTERNAL, diag.WithSeverity(diag.WARNING), diag.WithSummary("skipping provider %s which configured with 0 resources to fetch", providerInfo.Config.Name)))
			continue
		}
		wg.Add(1)
		go func(info ProviderInfo) {
			defer wg.Done()
			start := time.Now()
			s, d := runProviderFetch(ctx, pm, info, dsnURI, metadata, opts)
			if _, ok := ctx.Deadline(); ok {
				fetchSummaries <- fetchResult{s, d}
				return
			}
			// TODO: if context deadline exceeds in fetch, do we still want to run the save?
			if err := sta.SaveFetchSummary(ctx, createFetchSummary(fetchId, start, s)); err != nil {
				d = d.Add(diag.FromError(err, diag.INTERNAL))
			}
			fetchSummaries <- fetchResult{s, d}
		}(providerInfo)
	}
	wg.Wait()
	response := &FetchResponse{
		FetchId:              fetchId,
		ProviderFetchSummary: make(map[string]*ProviderFetchSummary, len(opts.ProvidersInfo)),
		Duration:             time.Since(start),
	}
	close(fetchSummaries)
	for ps := range fetchSummaries {
		response.ProviderFetchSummary[ps.summary.String()] = ps.summary
		if ps.diags.HasDiags() {
			diags = diags.Add(ps.diags)
		}
		response.TotalFetched += ps.summary.TotalResourcesFetched
	}
	events, filtered := analytics.FilterTelemetryEvents(diags)
	response.TelemetryEvents = events
	return response, filtered
}

func runProviderFetch(ctx context.Context, pm *plugin.Manager, info ProviderInfo, dsnURI string, metadata map[string]interface{}, opts *FetchOptions) (*ProviderFetchSummary, diag.Diagnostics) {
	var diags diag.Diagnostics
	cfg := info.Config
	pLog := log.With().Str("provider", cfg.Name).Str("alias", cfg.Alias).Logger()

	pLog.Debug().Str("name", info.Provider.String()).Str("alias", cfg.Alias).Msg("Creating provider plugin")
	providerPlugin, err := pm.CreatePlugin(&plugin.CreationOptions{
		Provider: info.Provider,
		Alias:    cfg.Alias,
		Env:      cfg.Env,
	})
	if err != nil {
		pLog.Error().Err(err).Msg("Failed to create provider plugin")
		return nil, diag.FromError(err, diag.INTERNAL)
	}
	defer pm.ClosePlugin(providerPlugin)

	pLog.Info().Msg("Requesting provider to configure")
	resp, err := providerPlugin.Provider().ConfigureProvider(ctx, &cqproto.ConfigureProviderRequest{
		CloudQueryVersion: Version,
		Connection: cqproto.ConnectionDetails{
			DSN: dsnURI,
		},
		Config: cfg.ConfigBytes,
	})
	if err != nil {
		pLog.Error().Err(err).Msg("Failed to configure provider")
		var (
			d   diag.Diagnostics
			sts FetchStatus
		)

		if cqerrors.IsCancelation(err) {
			d = cqerrors.CancelationDiag(err)
			sts = FetchCanceled
		} else {
			d = diag.FromError(err, diag.INTERNAL)
			sts = FetchConfigureFailed
		}

		return &ProviderFetchSummary{
			Name:             info.Provider.Name,
			Alias:            info.Config.Alias,
			Version:          providerPlugin.Version(),
			FetchedResources: make(map[string]ResourceFetchSummary),
			Status:           sts,
		}, d
	}
	diags = diags.Add(convertToConfigureDiags(resp.Diagnostics))
	if diags.HasErrors() {
		return &ProviderFetchSummary{
			Name:             info.Provider.Name,
			Alias:            info.Config.Alias,
			Version:          providerPlugin.Version(),
			FetchedResources: make(map[string]ResourceFetchSummary),
			Status:           FetchConfigureFailed,
		}, diags
	}

	pLog.Info().Msg("Provider configured successfully")
	summary, fetchDiags := executeFetch(ctx, pLog, providerPlugin, info, metadata, opts.UpdateCallback)
	diags = diags.Add(convertToFetchDiags(fetchDiags, info.Provider.Name, providerPlugin.Version()))

	return summary, diags
}

func executeFetch(ctx context.Context, pLog zerolog.Logger, providerPlugin plugin.Plugin, info ProviderInfo, metadata map[string]interface{}, callback FetchUpdateCallback) (*ProviderFetchSummary, diag.Diagnostics) {
	var (
		start   = time.Now()
		summary = &ProviderFetchSummary{
			Name:                  info.Provider.Name,
			Alias:                 info.Config.Alias,
			Version:               providerPlugin.Version(),
			FetchedResources:      make(map[string]ResourceFetchSummary),
			Status:                FetchFinished,
			TotalResourcesFetched: 0,
		}
		diags diag.Diagnostics
	)
	// Set fetch duration one function end
	defer func() {
		summary.Duration = time.Since(start)
	}()

	var resources []string
	resources, diags = normalizeResources(ctx, providerPlugin, info.Config.Resources, info.Config.SkipResources)
	if diags.HasErrors() {
		summary.Status = FetchFailed
		return summary, diags
	}

	pLog.Info().Msg("Provider started fetching resources")
	stream, err := providerPlugin.Provider().FetchResources(ctx,
		&cqproto.FetchResourcesRequest{
			Resources:             resources,
			ParallelFetchingLimit: info.Config.MaxParallelResourceFetchLimit,
			MaxGoroutines:         info.Config.MaxGoroutines,
			Timeout:               time.Duration(info.Config.ResourceTimeout) * time.Second,
			Metadata:              metadata,
		})
	if err != nil {
		summary.Status = FetchFailed
		return summary, diag.FromError(err, diag.INTERNAL)
	}

	for {
		resp, err := stream.Recv()
		switch err {
		case nil:
			// We didn't receive an error we received a response
			pLog.Debug().Str("resource", resp.ResourceName).Uint64("fetched", resp.ResourceCount).Msg("Resource fetched successfully")
			if callback != nil {
				callback(FetchUpdate{
					Name:              info.Provider.Name,
					Alias:             info.Config.Alias,
					Version:           providerPlugin.Version(),
					FinishedResources: resp.FinishedResources,
					ResourceCount:     resp.ResourceCount,
					DiagnosticCount:   diags.BySeverity(diag.WARNING, diag.ERROR, diag.PANIC).Len(),
				})
				// pLog.Debug().Str("resource", resp.ResourceName).Uint64("finishedCount", resp.ResourceCount).
				//	Bool("finished", update.AllDone()).Int("finishCount", update.DoneCount()).Msg("received fetch update")
			}
			summary.TotalResourcesFetched += resp.ResourceCount
			events, rdiags := analytics.FilterTelemetryEvents(resp.Summary.Diagnostics)
			summary.FetchedResources[resp.ResourceName] = ResourceFetchSummary{
				resp.Summary.Status.String(),
				resp.Summary.ResourceCount,
				rdiags,
				events,
				time.Since(start),
			}
			if resp.Error != "" {
				pLog.Warn().Err(err).Str("resource", resp.ResourceName).Msg("Received resource fetch error")
				diags = diags.Add(diag.FromError(errors.New(resp.Error), diag.RESOLVING, diag.WithResourceName(resp.ResourceName)))
			}
			// TODO: print diags, specific to resource into log?
			if rdiags.HasDiags() {
				pLog.Warn().Str("resource", resp.ResourceName).Msg("Received resource fetch diagnostics")
				diags = diags.Add(rdiags)
			}
		case io.EOF:
			// This case means the stream closed peacefully, i.e the provider finished without any error
			pLog.Info().TimeDiff("execution", time.Now(), start).Msg("Provider finished fetch")
			return summary, diags
		default:
			if callback != nil {
				callback(FetchUpdate{
					Name:            info.Provider.Name,
					Alias:           info.Config.Alias,
					Version:         providerPlugin.Version(),
					Error:           err.Error(),
					DiagnosticCount: diags.Len(),
				})
			}
			// We received an error, first lets check if we got canceled, if not we log the error and add to diags
			if cqerrors.IsCancelation(err) {
				pLog.Warn().TimeDiff("execution", time.Now(), start).Msg("Provider fetch was canceled")
				summary.Status = FetchCanceled
				return summary, diags.Add(cqerrors.CancelationDiag(err))
			}
			pLog.Error().Err(err).Msg("Received unexpected provider fetch error")
			summary.Status = FetchFailed
			return summary, diags.Add(diag.FromError(err, diag.INTERNAL))
		}
	}
}

// NormalizeResources walks over all given providers and in place normalizes their resources list:
//
// * wildcard expansion
// * verify no unknown resources
// * verify no duplicate resources
func normalizeResources(ctx context.Context, provider plugin.Plugin, resources, skip []string) ([]string, diag.Diagnostics) {
	s, err := provider.Provider().GetProviderSchema(ctx, &cqproto.GetProviderSchemaRequest{})
	if err != nil {
		return nil, diag.FromError(err, diag.INTERNAL)
	}

	return doNormalizeResources(resources, skip, s.ResourceTables)
}

// doNormalizeResources matches the given two resource lists to all provider resources and returns the requested resources (excluding skip resources) as another list.
func doNormalizeResources(resources, skip []string, all map[string]*schema.Table) ([]string, diag.Diagnostics) {
	useRes, diags := doGlobResources(resources, false, all)
	skipRes, dd := doGlobResources(skip, true, all)
	return funk.Subtract(useRes, skipRes).([]string), diags.Add(dd)
}

// doGlobResources returns a canonical list of resources given a list of requested and all known resources.
// It replaces wildcard resource with all resources in non-wild mode. Error is returned if:
//
// * wildcard is present and other explicit resource is requested;
// * one of explicitly requested resources is not present in all known;
// * some resource is specified more than once (duplicate).
func doGlobResources(requested []string, allowWild bool, all map[string]*schema.Table) ([]string, diag.Diagnostics) {
	if allowWild {
		for _, s := range requested {
			if s == "*" {
				return nil, diag.FromError(fmt.Errorf("wildcard resource can only be in the requested resources list"), diag.USER, diag.WithDetails("you can only use * in the resources part of the configuration"))
			}
		}
	} else if len(requested) == 1 && requested[0] == "*" {
		requested = make([]string, 0, len(all))
		for k := range all {
			requested = append(requested, k)
		}
	}

	result := make([]string, 0, len(requested))
	seen := make(map[string]struct{})
	for _, r := range requested {
		if r == "" {
			return nil, diag.FromError(errors.New("invalid resource"), diag.USER, diag.WithDetails("empty resource names are not allowed"))
		}

		if _, ok := seen[r]; ok {
			return nil, diag.FromError(fmt.Errorf("resource %q is duplicate", r), diag.USER, diag.WithDetails("configuration has duplicate resources"))
		}
		seen[r] = struct{}{}

		if _, ok := all[r]; ok {
			result = append(result, r)
			continue
		}

		if r == "*" {
			return nil, diag.FromError(fmt.Errorf("wildcard resource must be the only one in the list"), diag.USER, diag.WithDetails("you can only use * or a list of resources in configuration, but not both"))
		}

		switch globMatches, diags := matchResourceGlob(r, all); {
		case diags.HasDiags():
			return nil, diags
		case len(globMatches) == 0:
			return nil, diag.FromError(fmt.Errorf("resource %q does not exist", r), diag.USER, diag.WithDetails("configuration refers to a non-existing resource. Maybe you recently downgraded the provider but kept the config, or a typo perhaps?"))
		default:
			result = append(result, globMatches...)
		}
	}

	return cqsort.Unique(result), nil
}

// matchResourceGlob matches pattern to the given resources, returns matched resources or diags
// pattern should end with .*, exact matches are not handled.
func matchResourceGlob(pattern string, all map[string]*schema.Table) ([]string, diag.Diagnostics) {
	var result []string
	wildPos := strings.Index(pattern, ".*")

	if wildPos > 0 {
		if wildPos != len(pattern)-2 { // make sure it ends with .*
			return nil, diag.FromError(errors.New("invalid wildcard syntax"), diag.USER, diag.WithDetails("resource match should end with `.*`"))
		}
		for k := range all {
			if strings.HasPrefix(k, pattern[:wildPos+1]) { // include the "." in the match
				result = append(result, k)
			}
		}
	} else if wildPos == 0 || strings.Contains(pattern, "*") {
		return nil, diag.FromError(errors.New("invalid wildcard syntax"), diag.USER, diag.WithDetails("you can only use `*` or `resource.*` or full resource name"))
	}

	return result, nil
}

func parseDSN(storage database.Storage) (string, error) {
	parsed, err := dsn.ParseConnectionString(storage.DSN())
	if err != nil {
		return "", err
	}
	return parsed.String(), nil
}

func createFetchSummary(fetchId uuid.UUID, start time.Time, ps *ProviderFetchSummary) *state.FetchSummary {
	return &state.FetchSummary{
		FetchId:            fetchId,
		CreatedAt:          time.Now().UTC(),
		Start:              start,
		Finish:             time.Now().UTC(),
		IsSuccess:          !ps.Diagnostics().HasErrors(),
		TotalResourceCount: ps.TotalResourcesFetched,
		TotalErrorsCount:   ps.Diagnostics().Errors(),
		ProviderName:       ps.Name,
		ProviderAlias:      ps.Alias,
		ProviderVersion:    ps.Version,
		CoreVersion:        Version,
		Resources:          parseFetchedResources(ps.FetchedResources),
	}
}

func parseFetchedResources(resources map[string]ResourceFetchSummary) []state.ResourceFetchSummary {
	rfs := make([]state.ResourceFetchSummary, 0, len(resources))
	for k, v := range resources {
		rfs = append(rfs, state.ResourceFetchSummary{
			ResourceName:  k,
			Status:        v.Status,
			Error:         v.Diagnostics.Error(),
			ResourceCount: v.ResourceCount,
		})
	}
	return rfs
}
