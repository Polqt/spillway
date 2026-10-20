package cli

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/spf13/cobra"
)

func addProviderCommands(ctx context.Context, parent *cobra.Command, options Options) {
	provider := &cobra.Command{Use: "provider", Aliases: []string{"providers"}, Short: "inspect and manage configured providers", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		_, _ = fmt.Fprint(cmd.OutOrStdout(), "usage: spillway provider <command>\n\ncommands:\n  list         list configured providers\n  show <name>  show a configured provider\n")
		return commandError{code: 2}
	}}
	provider.SetContext(ctx)
	//nolint:contextcheck // Cobra supplies the invocation context through cmd.Context().
	provider.AddCommand(&cobra.Command{Use: "list", Short: "list configured providers", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		path, _ := cmd.Flags().GetString("config")
		if err := writeProviderList(ctx, cmd.OutOrStdout(), path, options.LookupEnv, options.PersistentEnv); err != nil {
			return commandFailure{code: 1, err: err}
		}
		return nil
	}})
	//nolint:contextcheck // Cobra supplies the invocation context through cmd.Context().
	show := &cobra.Command{Use: "show <name>", Short: "show a configured provider", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		path, _ := cmd.Flags().GetString("config")
		if err := writeProviderShow(ctx, cmd.OutOrStdout(), path, args[0], options.LookupEnv, options.PersistentEnv, options.GOOS); err != nil {
			return commandFailure{code: 1, err: err}
		}
		return nil
	}}
	show.ValidArgsFunction = providerCompletion
	list := provider.Commands()[0]
	for _, subcommand := range []*cobra.Command{list, show} {
		subcommand.Flags().String("config", defaultConfigPath, "path to the config file")
	}
	provider.AddCommand(show)
	parent.AddCommand(provider)
}

func loadProviderConfig(path string, lookupEnv func(string) (string, bool)) (config.Config, error) {
	placeholderLookup := func(name string) (string, bool) {
		if value, ok := lookupEnv(name); ok {
			return value, true
		}
		return "spillway-unset-placeholder", true
	}
	cfg, err := config.LoadWithEnv(path, placeholderLookup)
	if err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func writeProviderList(ctx context.Context, out io.Writer, path string, lookupEnv, persistentEnv func(string) (string, bool)) error {
	cfg, err := loadProviderConfig(path, lookupEnv)
	if err != nil {
		return err
	}
	providersInTiers := providerTiers(cfg)
	if _, err := fmt.Fprintln(out, "PROVIDER  KEY VARIABLE       IN THIS TERMINAL  FREE TIER  BASE URL  IN TIERS"); err != nil {
		return err
	}
	for _, name := range sortedProviderNames(cfg.Providers) {
		preset, _ := providers.LookupPreset(name)
		entry := cfg.Providers[name]
		keyName, err := config.ProviderKeyVariable(ctx, path, name)
		if err != nil {
			return err
		}
		if keyName == "" && preset.RequiresKey {
			keyName = preset.KeyEnv
		}
		presence := "not required"
		if keyName != "" {
			if _, ok := lookupEnv(keyName); ok {
				presence = "set"
			} else if scope, ok := persistentEnv(keyName); ok {
				presence = "set for " + scope
			} else {
				presence = "not set anywhere"
			}
		}
		attest := "no"
		if entry.FreeTier {
			attest = "attested"
		}
		base := entry.BaseURL
		if base == "" {
			base = "default"
		} else if parsed, parseErr := url.Parse(base); parseErr == nil && parsed.Host != "" {
			base = "custom"
		}
		if _, err := fmt.Fprintf(out, "%-9s %-17s %-17s %-10s %-9s %s\n", name, emptyAsDash(keyName), presence, attest, base, strings.Join(providersInTiers[name], ",")); err != nil {
			return err
		}
	}
	return nil
}

func writeProviderShow(ctx context.Context, out io.Writer, path, name string, lookupEnv, persistentEnv func(string) (string, bool), goos string) error {
	cfg, err := loadProviderConfig(path, lookupEnv)
	if err != nil {
		return err
	}
	entry, ok := cfg.Providers[name]
	if !ok {
		configured := sortedProviderNames(cfg.Providers)
		if len(configured) == 0 {
			configured = []string{"none"}
		}
		return fmt.Errorf("provider %s is not configured; configured providers: %s", name, strings.Join(configured, ", "))
	}
	preset, _ := providers.LookupPreset(name)
	keyName, err := config.ProviderKeyVariable(ctx, path, name)
	if err != nil {
		return err
	}
	if keyName == "" && preset.RequiresKey {
		keyName = preset.KeyEnv
	}
	if _, err := fmt.Fprintf(out, "%s (preset)\n", name); err != nil {
		return err
	}
	if keyName != "" {
		keyState := providerKeyEnvironmentState(keyName, lookupEnv, persistentEnv, goos)
		if _, err := fmt.Fprintf(out, "  key variable  ${%s}, %s\n", keyName, keyState); err != nil {
			return err
		}
	}
	if entry.FreeTier {
		_, err = fmt.Fprintln(out, "  free tier     attested; Spillway cannot verify billing state")
	} else {
		_, err = fmt.Fprintln(out, "  free tier     not attested")
	}
	if err != nil {
		return err
	}
	base := entry.BaseURL
	if base == "" {
		base = preset.DefaultBaseURL + " (default)"
	}
	if _, err := fmt.Fprintf(out, "  base URL      %s\n  timeout       %s\n", base, entry.Timeout); err != nil {
		return err
	}
	refs := providerTiers(cfg)[name]
	if len(refs) == 0 {
		_, err = fmt.Fprintln(out, "  tiers         none")
	} else {
		_, err = fmt.Fprintf(out, "  tiers         %s\n", strings.Join(refs, ", "))
	}
	return err
}

func providerKeyEnvironmentState(keyName string, lookupEnv, persistentEnv func(string) (string, bool), goos string) string {
	if _, ok := lookupEnv(keyName); ok {
		return "set in this terminal"
	}
	if scope, ok := persistentEnv(keyName); ok {
		if goos == "windows" {
			return "not set in this terminal; set for your Windows " + scope
		}
		return "not set in this terminal; configured persistently"
	}
	return "not set anywhere"
}

func providerTiers(cfg config.Config) map[string][]string {
	refs := make(map[string][]string)
	for tierName, tier := range cfg.Tiers {
		for _, model := range tier.Models {
			provider, _, ok := strings.Cut(model, "/")
			if ok {
				refs[provider] = append(refs[provider], tierName)
			}
		}
	}
	for name := range refs {
		sort.Strings(refs[name])
		refs[name] = slicesCompact(refs[name])
	}
	return refs
}

func sortedProviderNames(values map[string]config.Provider) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func providerCompletion(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	path, _ := cmd.Flags().GetString("config")
	lookup := func(string) (string, bool) { return "spillway-completion-placeholder", true }
	cfg, err := config.LoadWithEnv(path, lookup)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return sortedProviderNames(cfg.Providers), cobra.ShellCompDirectiveNoFileComp
}

func slicesCompact(values []string) []string {
	if len(values) < 2 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

func emptyAsDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
