package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func addProviderCommands(ctx context.Context, parent *cobra.Command, options Options) {
	//nolint:contextcheck // Cobra supplies the invocation context through cmd.Context().
	provider := &cobra.Command{Use: "provider", Aliases: []string{"providers"}, Short: "inspect and manage configured providers", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if commandTUIEnabled(cmd, options.interactive()) {
			if err := runProviderBrowser(cmd.Context(), defaultConfigPath, options); err != nil {
				return commandFailure{code: 1, err: err}
			}
			return nil
		}
		_, _ = fmt.Fprint(cmd.OutOrStdout(), "usage: spillway provider <command>\n\ncommands:\n  list             list configured instances\n  show <instance>  show a configured instance\n  key set <name>   save a provider key with masked input\n")
		return commandError{code: 2}
	}}
	provider.SetContext(ctx)
	//nolint:contextcheck // Cobra supplies the invocation context through cmd.Context().
	provider.AddCommand(&cobra.Command{Use: "list", Short: "list configured providers", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		path, _ := cmd.Flags().GetString("config")
		if commandTUIEnabled(cmd, options.interactive()) {
			if err := runProviderBrowser(ctx, path, options); err != nil {
				return commandFailure{code: 1, err: err}
			}
			return nil
		}
		if err := writeProviderList(ctx, cmd.OutOrStdout(), path, options.LookupEnv, options.PersistentEnv); err != nil {
			return commandFailure{code: 1, err: err}
		}
		return nil
	}})
	//nolint:contextcheck // Cobra supplies the invocation context through cmd.Context().
	show := &cobra.Command{Use: "show <instance>", Short: "show a configured provider instance", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		path, _ := cmd.Flags().GetString("config")
		if commandTUIEnabled(cmd, options.interactive()) {
			detail := new(bytes.Buffer)
			if err := writeProviderShow(ctx, detail, path, args[0], options.LookupEnv, options.PersistentEnv, options.GOOS); err != nil {
				return commandFailure{code: 1, err: err}
			}
			entry := helpEntry{name: args[0], group: "Provider instance", short: "Configured provider status", detail: strings.TrimSpace(detail.String())}
			if err := runBrowser(ctx, "provider details", "Inspect provider settings and credential presence.", []helpEntry{entry}, options.Stdin, options.Stdout); err != nil {
				return commandFailure{code: 1, err: err}
			}
			return nil
		}
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
	key := &cobra.Command{Use: "key", Short: "set a provider API key", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		_, _ = fmt.Fprint(cmd.OutOrStdout(), "usage: spillway provider key set <instance>\n")
		return commandError{code: 2}
	}}
	setKey := &cobra.Command{Use: "set <instance>", Short: "save a provider key with masked input", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !options.StdinTerminal || !options.StdoutTerminal {
			return commandFailure{code: 1, err: fmt.Errorf("provider key input requires an interactive terminal; run this command in your terminal")}
		}
		file, ok := options.Stdin.(*os.File)
		if !ok {
			return commandFailure{code: 1, err: fmt.Errorf("provider key input requires a terminal stdin")}
		}
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Provider key for %s (masked): ", args[0]); err != nil {
			return commandFailure{code: 1, err: err}
		}
		secret, err := readMaskedProviderKey(file, cmd.ErrOrStderr())
		if _, writeErr := fmt.Fprintln(cmd.ErrOrStderr()); writeErr != nil {
			return commandFailure{code: 1, err: writeErr}
		}
		if err != nil {
			return commandFailure{code: 1, err: err}
		}
		defer clear(secret)
		path, _ := cmd.Flags().GetString("config")
		doc, err := config.OpenDocument(ctx, path)
		if err != nil {
			return commandFailure{code: 1, err: err}
		}
		edit := doc.Edit()
		if err := edit.SetProviderAPIKey(args[0], string(secret)); err != nil {
			return commandFailure{code: 1, err: err}
		}
		change, err := edit.Result()
		if err != nil {
			return commandFailure{code: 1, err: err}
		}
		if _, err := doc.Commit(ctx, change, config.CommitOptions{Now: options.Now}); err != nil {
			return commandFailure{code: 1, err: err}
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Saved the key for provider %s in %s. Keep this file private.\n", args[0], doc.Path())
		return err
	}}
	setKey.Flags().String("config", defaultConfigPath, "path to the config file")
	key.AddCommand(setKey)
	provider.AddCommand(key)
	parent.AddCommand(provider)
}

func readMaskedProviderKey(input *os.File, output io.Writer) ([]byte, error) {
	fd := int(input.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, fmt.Errorf("enable masked provider key input: %w", err)
	}
	defer func() { _ = term.Restore(fd, state) }()

	secret := make([]byte, 0, 256)
	var one [1]byte
	for {
		n, err := input.Read(one[:])
		if err != nil {
			clear(secret)
			return nil, fmt.Errorf("read provider key: %w", err)
		}
		if n == 0 {
			continue
		}
		switch one[0] {
		case '\r', '\n':
			if len(secret) == 0 {
				return nil, errors.New("provider key input was empty")
			}
			return secret, nil
		case 3:
			clear(secret)
			return nil, errors.New("provider key input canceled")
		case '\b', 0x7f:
			if len(secret) > 0 {
				secret = secret[:len(secret)-1]
				if _, err := io.WriteString(output, "\b \b"); err != nil {
					clear(secret)
					return nil, fmt.Errorf("update masked input: %w", err)
				}
			}
		default:
			if len(secret) >= 4096 {
				clear(secret)
				return nil, errors.New("provider key is longer than 4096 bytes")
			}
			secret = append(secret, one[0])
			if _, err := io.WriteString(output, "*"); err != nil {
				clear(secret)
				return nil, fmt.Errorf("update masked input: %w", err)
			}
		}
	}
}

func runProviderBrowser(ctx context.Context, path string, options Options) error {
	cfg, err := loadProviderConfig(path, options.LookupEnv)
	if err != nil {
		return err
	}
	names := sortedProviderNames(cfg.Providers)
	tiersByProvider := providerTiers(cfg)
	entries := make([]helpEntry, 0, len(names))
	for _, name := range names {
		entry := cfg.Providers[name]
		detail := new(bytes.Buffer)
		if err := writeProviderShow(ctx, detail, path, name, options.LookupEnv, options.PersistentEnv, options.GOOS); err != nil {
			return err
		}
		tiers := tiersByProvider[name]
		summary := configuredServiceType(name, entry)
		if len(tiers) > 0 {
			summary += " · tiers " + strings.Join(tiers, ", ")
		}
		entries = append(entries, helpEntry{name: name, group: "Provider instance", short: summary, detail: strings.TrimSpace(detail.String())})
	}
	if len(entries) == 0 {
		return fmt.Errorf("no provider instances are configured in %s", path)
	}
	return runBrowser(ctx, "provider status", "Inspect configured providers and credential presence.", entries, options.Stdin, options.Stdout)
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
	if _, err := fmt.Fprintln(out, "INSTANCE          TYPE        KEY SOURCE         KEY STATUS       FREE TIER  BASE URL  IN TIERS"); err != nil {
		return err
	}
	for _, name := range sortedProviderNames(cfg.Providers) {
		entry := cfg.Providers[name]
		preset, _ := providers.LookupPreset(configuredServiceType(name, entry))
		localKey := preset.RequiresKey && entry.APIKeyEnv == "" && !entry.APIKey.IsZero()
		keyName := ""
		if !localKey {
			keyName, err = config.ProviderKeyVariable(ctx, path, name)
			if err != nil {
				return err
			}
			if keyName == "" && preset.RequiresKey {
				keyName = preset.KeyEnv
			}
		}
		presence := "not required"
		if localKey {
			presence = "stored in config"
		} else if keyName != "" {
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
		if _, err := fmt.Fprintf(out, "%-17s %-11s %-17s %-17s %-10s %-9s %s\n", name, configuredServiceType(name, entry), emptyAsDash(keyName), presence, attest, base, strings.Join(providersInTiers[name], ",")); err != nil {
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
	serviceType := configuredServiceType(name, entry)
	preset, _ := providers.LookupPreset(serviceType)
	localKey := preset.RequiresKey && entry.APIKeyEnv == "" && !entry.APIKey.IsZero()
	keyName := ""
	if !localKey {
		keyName, err = config.ProviderKeyVariable(ctx, path, name)
		if err != nil {
			return err
		}
		if keyName == "" && preset.RequiresKey {
			keyName = preset.KeyEnv
		}
	}
	identity := "preset"
	if entry.Type != "" {
		identity = "instance; type " + serviceType
	}
	if _, err := fmt.Fprintf(out, "%s (%s)\n", name, identity); err != nil {
		return err
	}
	if localKey {
		if _, err := fmt.Fprintln(out, "  key           stored locally in config"); err != nil {
			return err
		}
	} else if keyName != "" {
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
	if len(entry.QuotaDomains) > 0 {
		scopes := make([]string, 0, len(entry.QuotaDomains))
		for scope := range entry.QuotaDomains {
			scopes = append(scopes, scope)
		}
		sort.Strings(scopes)
		if _, err := fmt.Fprintf(out, "  quota domains  %s (labels hidden)\n", strings.Join(scopes, ", ")); err != nil {
			return err
		}
	}
	if entry.Budget != nil {
		if _, err := fmt.Fprintf(out, "  instance budget daily=%s monthly=%s\n", budgetDisplay(entry.Budget.DailyMicroUSD), budgetDisplay(entry.Budget.MonthlyMicroUSD)); err != nil {
			return err
		}
	}
	base := entry.BaseURL
	if base == "" {
		base = preset.DefaultBaseURL + " (default)"
	} else {
		base = "custom endpoint configured (details hidden)"
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

func configuredServiceType(name string, entry config.Provider) string {
	if entry.Type != "" {
		return entry.Type
	}
	return name
}

func budgetDisplay(microUSD int64) string {
	if microUSD <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("$%d.%06d", microUSD/1_000_000, microUSD%1_000_000)
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
