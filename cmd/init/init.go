package init

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cloudquery/cloudquery/cmd/utils"
	"github.com/cloudquery/cloudquery/pkg/config"
	"github.com/cloudquery/cloudquery/pkg/core"
	"github.com/cloudquery/cloudquery/pkg/plugin/registry"
	"github.com/cloudquery/cloudquery/pkg/ui"
	"github.com/cloudquery/cloudquery/pkg/ui/console"
	"github.com/cloudquery/cq-provider-sdk/provider/diag"
	"github.com/google/uuid"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const (
	initShort   = "Generate initial cloudquery.yml for fetch command"
	initExample = `
  # Downloads aws provider and generates cloudquery.yml for aws provider
  cloudquery init aws

  # Downloads aws,gcp providers and generates one cloudquery.yml with both providers
  cloudquery init aws gcp`
)

func NewCmdInit() *cobra.Command {
	initCmd := &cobra.Command{
		Use:     "init [choose one or more providers (aws gcp azure okta ...)]",
		Short:   initShort,
		Long:    initShort,
		Example: initExample,
		Args:    cobra.MinimumNArgs(1),
		RunE:    initialize,
	}
	return initCmd
}

func initialize(cmd *cobra.Command, providers []string) error {
	fs := afero.NewOsFs()
	ctx := cmd.Context()

	configPath := utils.GetConfigFile() // by definition, this will get us an existing file if possible

	if info, _ := fs.Stat(configPath); info != nil {
		ui.ColorizedOutput(ui.ColorError, "Error: Config file %s already exists\n", configPath)
		// We don't want to print the error twice, so we set the `SilenceErrors` flag to true
		cmd.SilenceErrors = true
		return diag.FromError(fmt.Errorf("config file %q already exists", configPath), diag.USER)
	}

	if strings.ToLower(filepath.Ext(configPath)) == ".hcl" {
		ui.ColorizedOutput(ui.ColorError, "Error: HCL config format is deprecated and should not be used for new installations\n")
		return diag.FromError(fmt.Errorf("deprecated format %q", configPath), diag.USER)
	}

	requiredProviders := make([]*config.RequiredProvider, len(providers))
	for i, p := range providers {
		organization, providerName, provVersion, err := ParseProviderCLIArg(p)
		if err != nil {
			return fmt.Errorf("could not parse requested provider: %w", err)
		}
		rp := config.RequiredProvider{
			Name:    providerName,
			Version: provVersion,
		}
		if organization != registry.DefaultOrganization {
			source := fmt.Sprintf("%s/%s", organization, providerName)
			rp.Source = &source
		}
		requiredProviders[i] = &rp
		providers[i] = providerName // overwrite "provider@version" with just "provider"
	}

	mainConfig := config.Config{
		CloudQuery: config.CloudQuery{
			Providers: requiredProviders,
			Connection: &config.Connection{
				Username: "postgres",
				Password: "pass",
				Host:     "localhost",
				Port:     5432,
				Database: "postgres",
				SSLMode:  "disable",
			},
		},
	}
	if diags := config.ProcessConfig(&mainConfig); diags.HasErrors() {
		return diags
	}

	cCfg := mainConfig
	cCfg.CloudQuery.Connection.DSN = "" // Don't connect
	c, err := console.CreateClientFromConfig(ctx, &cCfg, uuid.Nil)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.DownloadProviders(ctx); err != nil {
		return err
	}

	b, err := generateConfig(ctx, c, providers, mainConfig)
	if err != nil {
		return err
	}
	_ = afero.WriteFile(fs, configPath, b, 0644)
	ui.ColorizedOutput(ui.ColorSuccess, "configuration generated successfully to %s\n", configPath)
	return nil
}

func generateConfig(ctx context.Context, c *console.Client, providers []string, mainConfig config.Config) ([]byte, error) {
	cqConfig := struct {
		CloudQuery config.CloudQuery `yaml:"cloudquery" json:"cloudquery"`
	}{
		CloudQuery: mainConfig.CloudQuery,
	}
	b, err := yaml.Marshal(cqConfig)
	if err != nil {
		return nil, diag.WrapError(err)
	}

	var cqConfigRaw = struct {
		CQ yaml.Node `yaml:"cloudquery"`
	}{}
	if err := yaml.Unmarshal(b, &cqConfigRaw); err != nil {
		return nil, diag.WrapError(err)
	}

	provNode := &yaml.Node{
		Kind:        yaml.SequenceNode,
		HeadComment: "provider configurations",
	}

	for _, p := range providers {
		pCfg, diags := core.GetProviderConfiguration(ctx, c.PluginManager, &core.GetProviderConfigOptions{
			Provider: c.ConvertRequiredToRegistry(p),
		})
		if pCfg != nil && pCfg.Format != 1 /* YAML */ {
			diags = diags.Add(diag.FromError(fmt.Errorf("provider %s doesn't support YAML config. Please upgrade provider", p), diag.USER))
		}
		if diags.HasErrors() {
			return nil, diags
		}

		var yCfg yaml.Node
		if err := yaml.Unmarshal(pCfg.Config, &yCfg); err != nil {
			return nil, diag.WrapError(err)
		}

		provNode.Content = append(provNode.Content, &yaml.Node{
			Kind: yaml.MappingNode,
			Content: append([]*yaml.Node{
				{
					Kind:  yaml.ScalarNode,
					Value: "name",
				},
				{
					Kind:  yaml.ScalarNode,
					Value: p,
				},
			}, yCfg.Content[0].Content...),
		})
	}

	nd := struct {
		Data map[string]*yaml.Node `yaml:",inline"`
	}{
		Data: map[string]*yaml.Node{
			"cloudquery": &cqConfigRaw.CQ,
			"providers":  provNode,
		},
	}

	return yaml.Marshal(&nd)
}

func ParseProviderCLIArg(providerCLIArg string) (org string, name string, version string, err error) {
	argParts := strings.Split(providerCLIArg, "@")

	l := len(argParts)

	// e.g. aws@latest@0.1.0
	if l > 2 {
		return "", "", "", fmt.Errorf("invalid provider name@version %q", providerCLIArg)
	}

	// e.g. aws@latest
	if l == 2 && argParts[1] == "latest" {
		org, name, err = registry.ParseProviderName(argParts[0])
		return org, name, "latest", err
	}

	// e.g. aws
	if l == 1 {
		org, name, err = registry.ParseProviderName(argParts[0])
		return org, name, "latest", err
	}

	// e.g. aws@0.12.0
	org, name, err = registry.ParseProviderName(argParts[0])
	if err != nil {
		return "", "", "", err
	}

	ver, err := config.ParseVersion(argParts[1])
	if err != nil {
		return "", "", "", fmt.Errorf("invalid version %q: %w", argParts[1], err)
	}

	return org, name, config.FormatVersion(ver), nil
}
