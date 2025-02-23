package config

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/cloudquery/cloudquery/internal/logging"
	"github.com/cloudquery/cq-provider-sdk/provider/diag"
	"github.com/spf13/viper"
	"github.com/xeipuuv/gojsonschema"
	"gopkg.in/yaml.v3"
)

//go:embed schema.json
var configSchemaYAML []byte

func (p *Parser) LoadConfigFromSource(data []byte) (*Config, diag.Diagnostics) {
	newData := os.Expand(string(data), p.getVariableValue)
	config, diags := decodeConfig(strings.NewReader(newData))

	if diags.HasErrors() {
		return nil, diags
	}

	diags = diags.Add(ProcessConfig(config))
	return config, diags
}

func (p *Parser) LoadConfigFile(path string) (*Config, diag.Diagnostics) {
	contents, diags := p.LoadFile(path)
	if diags.HasErrors() {
		return nil, diags
	}
	return p.LoadConfigFromSource(contents)
}

// ProcessConfig handles the configuration after it was loaded and parsed
// 1. Assigns defaults after decoding the raw configuration format
// 2. Overrides configuration values from CLI flags
// 3. Validates the configuration provided by the user
// 4. Normalizes the configuration to make it easier to use
func ProcessConfig(config *Config) diag.Diagnostics {
	assignDefaults(config)
	overrideFromCLIFlags(config)
	diags := validate(config)
	if diags.HasErrors() {
		return diags
	}

	normalize(config)
	return diags
}

func ParseVersion(version string) (*semver.Version, error) {
	return semver.NewVersion(version)
}

func FormatVersion(version *semver.Version) string {
	return "v" + version.String()
}

func isVersionLatest(version string) bool {
	return version == "latest"
}

func validate(config *Config) diag.Diagnostics {
	var diags diag.Diagnostics

	diags = diags.Add(validateCloudQueryProviders(config.CloudQuery.Providers))
	diags = diags.Add(validateConnection(config.CloudQuery.Connection))

	return diags.Add(validateProvidersBlock(config))
}

func assignDefaults(config *Config) {
	// TODO: decode in a more generic way
	if config.CloudQuery.Connection == nil {
		config.CloudQuery.Connection = &Connection{}
	}
}

func overrideFromCLIFlags(config *Config) {
	datadir := viper.GetString("data-dir")
	if datadir != "" {
		config.CloudQuery.PluginDirectory = filepath.Join(datadir, "providers")
	}

	if datadir != "" {
		config.CloudQuery.PolicyDirectory = filepath.Join(datadir, "policies")
	}

	if ds := viper.GetString("dsn"); ds != "" {
		config.CloudQuery.Connection.DSN = ds
	}
}

func validateCloudQueryProviders(providers RequiredProviders) diag.Diagnostics {
	var diags diag.Diagnostics

	for _, cp := range providers {
		if isVersionLatest(cp.Version) {
			continue
		}

		_, err := ParseVersion(cp.Version)
		if err != nil {
			diags = diags.Add(diag.FromError(fmt.Errorf("Provider %q version %q is invalid. Please set to 'latest' a or valid semantic version", cp.Name, cp.Version), diag.USER))
		}
	}

	return diags
}

func decodeConfig(r io.Reader) (*Config, diag.Diagnostics) {
	var yc struct {
		CloudQuery CloudQuery  `yaml:"cloudquery" json:"cloudquery"`
		Providers  []*Provider `yaml:"providers" json:"providers"`
	}

	lgc := logging.GlobalConfig
	yc.CloudQuery.Logger = &lgc
	d := yaml.NewDecoder(r)
	d.KnownFields(true)
	if err := d.Decode(&yc); err != nil {
		return nil, diag.FromError(err, diag.USER, diag.WithSummary("Failed to parse yaml"))
	}

	schemaLoader := gojsonschema.NewBytesLoader(configSchemaYAML)
	documentLoader := gojsonschema.NewGoLoader(yc)
	result, err := gojsonschema.Validate(schemaLoader, documentLoader)
	if err != nil {
		return nil, diag.FromError(err, diag.USER, diag.WithSummary("Failed to validate config"))
	}
	if !result.Valid() {
		errs := result.Errors()
		if len(errs) == 0 {
			return nil, diag.FromError(errors.New("Failed to validate config with schema"), diag.USER, diag.WithSummary("Invalid configuration"))
		}
		var diags diag.Diagnostics
		for _, e := range errs {
			diags = diags.Add(
				diag.FromError(errors.New(e.String()), diag.USER, diag.WithDetails("%s", e.Description()), diag.WithSummary("Config field %q has error of type %s", e.Field(), e.Type())),
			)
		}
		return nil, diags
	}

	providers := yc.Providers
	for _, p := range providers {
		p.ConfigBytes, err = yaml.Marshal(p.Configuration)
		if err != nil {
			return nil, diag.FromError(err, diag.INTERNAL, diag.WithSummary("Configuration marshal failed"))
		}
	}

	return &Config{
		CloudQuery: yc.CloudQuery,
		Providers:  providers,
	}, nil
}

func validateConnection(connection *Connection) diag.Diagnostics {
	var diags diag.Diagnostics

	dsnFromFlag := viper.GetString("dsn")

	if dsnFromFlag == "" && connection.DSNFile != "" {
		if connection.DSN != "" {
			return diags.Add(diag.NewBaseError(
				fmt.Errorf("invalid connection configuration"),
				diag.USER, diag.WithOptionalSeverity(diag.ERROR),
				diag.WithDetails("DSN file specified along with literal DSN, only one type is supported")),
			)
		}

		dsnBytes, err := ioutil.ReadFile(connection.DSNFile)
		if err != nil {
			return diags.Add(diag.FromError(err, diag.USER, diag.WithSummary("Failed to read DSN file")))
		}

		connection.DSN = string(bytes.TrimSpace(dsnBytes))
	}

	// We support both a `dsn` string for backwards compatibility, a dsn file, or connection configuration parameters (host, database, etc.)
	// If a user configured multiple, we error out unless the dsn was configured via a CLI flag (e.g. `cloudquery fetch --dsn <dsn>`)
	if connection.DSN != "" {
		// allow using a DSN flag even if the config file has explicitly attributes (user, password, host, database, etc.)
		if dsnFromFlag == "" && connection.IsAnyConnParamsSet() {
			diags = append(diags, diag.NewBaseError(
				fmt.Errorf("invalid connection configuration"),
				diag.USER, diag.WithOptionalSeverity(diag.ERROR),
				diag.WithDetails("DSN specified along with explicit attributes, only one type is supported")),
			)
		}
		return diags
	}

	if connection.Host == "" {
		diags = append(diags, diag.NewBaseError(
			fmt.Errorf("invalid connection configuration"),
			diag.USER, diag.WithOptionalSeverity(diag.ERROR),
			diag.WithDetails("missing host")),
		)
	}
	if connection.Database == "" {
		diags = append(diags, diag.NewBaseError(
			fmt.Errorf("invalid connection configuration"),
			diag.USER, diag.WithOptionalSeverity(diag.ERROR),
			diag.WithDetails("missing database")),
		)
	}

	return diags
}

// Validates the `cloudquery.providers` configuration block
func validateProvidersBlock(config *Config) diag.Diagnostics {
	var diags diag.Diagnostics
	existingProviders := make(map[string]bool, len(config.Providers))

	// We don't allow duplicate provider names or aliases
	for _, provider := range config.Providers {
		if provider.Alias != "" {
			_, aliasExists := existingProviders[provider.Alias]
			if aliasExists {
				diags = diags.Add(diag.FromError(fmt.Errorf("provider with alias %s for provider %s already exists, give it a different alias", provider.Alias, provider.Name), diag.USER, diag.WithSummary("Duplicate Alias")))
				continue
			}
			existingProviders[provider.Alias] = true
		} else {
			_, nameExists := existingProviders[provider.Name]
			if nameExists {
				diags = diags.Add(diag.FromError(fmt.Errorf("provider with name %s already exists, use alias in provider configuration block", provider.Name), diag.USER, diag.WithSummary("Provider Alias Required")))
				continue
			}
			existingProviders[provider.Name] = true
		}
	}
	return diags
}

func normalize(config *Config) {
	for _, cloudqueryProvider := range config.CloudQuery.Providers {
		if isVersionLatest(cloudqueryProvider.Version) {
			continue
		}

		ver, _ := ParseVersion(cloudqueryProvider.Version)
		// convert partial versions such as "0.10" to "v0.10.0"
		cloudqueryProvider.Version = FormatVersion(ver)
	}

	// Backwards compatibility. Don't override DSN if was provided by the user
	if config.CloudQuery.Connection.DSN == "" {
		config.CloudQuery.Connection.BuildFromConnParams()
	}
	for _, provider := range config.Providers {
		// Alias should default to provider name
		if provider.Alias == "" {
			provider.Alias = provider.Name
		}
	}
}
