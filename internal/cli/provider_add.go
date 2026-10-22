package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"charm.land/huh/v2"
	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/spf13/cobra"
)

var environmentVariableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func providerAddCommand(ctx context.Context, options Options) *cobra.Command {
	var keyEnv, baseURL, tier, instanceName string
	var quotaDomainArgs []string
	var attest, noTier, dryRun, yes bool
	command := &cobra.Command{
		Use:   "add [type] [--name instance]",
		Short: "add a provider to the config (experimental)",
		Args:  cobra.MaximumNArgs(1),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if cmd.Flags().Changed("name") {
				return allProviderNames(), cobra.ShellCompDirectiveNoFileComp
			}
			path, _ := cmd.Flags().GetString("config")
			lookup := func(string) (string, bool) { return "spillway-completion-placeholder", true }
			cfg, err := config.LoadWithEnv(path, lookup)
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return availableProviderNames(cfg), cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("config")
			if noTier && tier != "" {
				return usageFailure(cmd, "--tier and --no-tier cannot be used together")
			}
			cfg, err := loadProviderConfig(path, options.LookupEnv)
			if err != nil {
				return commandFailure{code: 1, err: err}
			}
			if len(args) == 0 && yes {
				return usageFailure(cmd, "--yes requires a provider name")
			}
			serviceType := ""
			if len(args) == 1 {
				serviceType = args[0]
			}
			interactive := options.StdinTerminal && options.StdoutTerminal && !yes
			prompted := false
			if serviceType == "" {
				if !interactive {
					return usageFailure(cmd, "provider name is required when prompts are unavailable; choose one of: "+strings.Join(availableProviderNames(cfg), ", "))
				}
				serviceType, err = chooseProviderToAdd(ctx, options, cfg)
				if err != nil {
					return commandFailure{code: 1, err: err}
				}
				prompted = true
			}
			preset, ok := providers.LookupPreset(serviceType)
			if !ok {
				return commandFailure{code: 1, err: fmt.Errorf("%q is not a supported provider; choose one of %s", serviceType, strings.Join(allProviderNames(), ", "))}
			}
			name := serviceType
			if instanceName != "" {
				if !config.ValidProviderInstanceName(instanceName) {
					return commandFailure{code: 1, err: errors.New("--name must be a safe alias matching [a-z][a-z0-9-]{0,31}; do not use an account or project identifier")}
				}
				name = instanceName
			}
			if _, exists := cfg.Providers[name]; exists {
				line, _ := providerConfigLine(ctx, path, name)
				if name == serviceType {
					return commandFailure{code: 1, err: duplicateProviderMessage(path, name, line)}
				}
				return commandFailure{code: 1, err: fmt.Errorf("provider instance %s is already configured in %s (line %d)", name, path, line)}
			}
			if preset.RequiresKey {
				if keyEnv == "" {
					keyEnv = preset.KeyEnv
					if name != serviceType {
						keyEnv = strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_API_KEY"
					}
				}
				if interactive && !cmd.Flags().Changed("key-env") {
					keyEnv, err = chooseKeyEnvironment(ctx, options, keyEnv)
					if err != nil {
						return commandFailure{code: 1, err: err}
					}
					prompted = true
				}
				if !environmentVariableName.MatchString(keyEnv) || looksLikeKeyValue(keyEnv) {
					return commandFailure{code: 1, err: errors.New("--key-env takes a variable name such as GEMINI_API_KEY, not a key value")}
				}
			}
			if !preset.RequiresKey && keyEnv != "" {
				return commandFailure{code: 1, err: fmt.Errorf("%s does not take a provider key", serviceType)}
			}
			if attest && !preset.FreeTierAttestable {
				return commandFailure{code: 1, err: fmt.Errorf("--attest-free-tier is supported only for groq and gemini, not %s", serviceType)}
			}
			if interactive && preset.FreeTierAttestable && !cmd.Flags().Changed("attest-free-tier") {
				attest, err = chooseFreeTierAttestation(ctx, options, name)
				if err != nil {
					return commandFailure{code: 1, err: err}
				}
				prompted = true
			}
			doc, err := config.OpenDocument(ctx, path)
			if err != nil {
				return commandFailure{code: 1, err: err}
			}
			edit := doc.Edit()
			domains, domainErr := parseQuotaDomainArgs(quotaDomainArgs)
			if domainErr != nil {
				return commandFailure{code: 1, err: domainErr}
			}
			entry := config.ProviderEntry{Name: name, KeyEnv: keyEnv, BaseURL: baseURL, FreeTier: attest, QuotaDomains: domains}
			if name != serviceType {
				entry.Type = serviceType
			}
			if err := edit.AddProvider(entry); err != nil {
				return commandFailure{code: 1, err: err}
			}
			if tier == "" && !noTier && interactive {
				if _, exists := cfg.Tiers["free"]; exists && preset.SuggestedModel != "" {
					shouldAdd, promptErr := chooseTierPlacement(ctx, options, name)
					if promptErr != nil {
						return commandFailure{code: 1, err: promptErr}
					}
					if shouldAdd {
						tier = "free"
					}
					prompted = true
				}
			}
			warning := ""
			if tier != "" {
				selectedTier, exists := cfg.Tiers[tier]
				if !exists {
					return commandFailure{code: 1, err: fmt.Errorf("tiers.%s does not exist", tier)}
				}
				if selectedTier.AllowSpend {
					return commandFailure{code: 1, err: fmt.Errorf("provider add can place a suggested model only in a zero-spend tier; %s allows spending", tier)}
				}
				modelRef := name + "/" + preset.SuggestedModel
				ref, parseErr := providers.ParseModelRef(modelRef)
				if parseErr != nil {
					return commandFailure{code: 1, err: parseErr}
				}
				selectedProvider := config.Provider{Type: serviceType, FreeTier: attest}
				if !config.IsZeroSpendEligible(ref, selectedProvider) && serviceType != "gemini" && serviceType != "groq" {
					warning = fmt.Sprintf("Suggested model %s was not added to tier %q because it is not known to be zero-spend eligible (ADR 0005).\n", modelRef, tier)
				} else if (serviceType == "gemini" || serviceType == "groq") && !attest {
					warning = fmt.Sprintf("Suggested model %s was not added to tier %q because %s is not attested as having no paid plan.\n", modelRef, tier, name)
				} else if err := edit.AppendTierModel(tier, modelRef); err != nil {
					return commandFailure{code: 1, err: err}
				}
			}
			change, err := edit.Result()
			if err != nil {
				return commandFailure{code: 1, err: err}
			}
			if err := change.WritePreview(cmd.OutOrStdout()); err != nil {
				return commandFailure{code: 1, err: fmt.Errorf("write preview: %w", err)}
			}
			if prompted && !dryRun {
				confirmed, promptErr := confirmProviderWrite(ctx, options)
				if promptErr != nil {
					return commandFailure{code: 1, err: promptErr}
				}
				if !confirmed {
					return commandFailure{code: 1, err: errors.New("provider add cancelled; config was not changed")}
				}
			}
			if dryRun {
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "Dry run only; %s was not changed.\n", doc.Path())
				if err != nil {
					return commandFailure{code: 1, err: err}
				}
				return nil
			}
			result, err := doc.Commit(ctx, change, config.CommitOptions{Now: options.Now})
			if err != nil {
				return commandFailure{code: 1, err: err}
			}
			if warning != "" {
				if _, err := io.WriteString(cmd.OutOrStdout(), warning); err != nil {
					return commandFailure{code: 1, err: err}
				}
			}
			backupNote := "owner-only; contains local keys"
			if options.GOOS == "windows" {
				backupNote = "contains local keys; original Windows permissions kept"
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Updated %s; backup at %s (%s).\n", result.Path, result.BackupPath, backupNote); err != nil {
				return commandFailure{code: 1, err: err}
			}
			if result.ModeTightened {
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Config permissions were tightened to owner-only."); err != nil {
					return commandFailure{code: 1, err: err}
				}
			}
			if keyEnv != "" {
				if _, ok := options.LookupEnv(keyEnv); !ok {
					if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s is not set in this terminal. Run spillway doctor for shell-specific setup guidance.\n", keyEnv); err != nil {
						return commandFailure{code: 1, err: err}
					}
				}
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Spillway reads config only at startup; restart spillway serve -config %s to apply.\n", result.Path)
			if err != nil {
				return commandFailure{code: 1, err: err}
			}
			return nil
		},
	}
	command.Flags().String("config", defaultConfigPath, "path to the config file")
	command.Flags().StringVar(&instanceName, "name", "", "safe alias for an additional provider instance")
	command.Flags().StringArrayVar(&quotaDomainArgs, "quota-domain", nil, "declare a shared quota scope alias as SCOPE=ALIAS (repeatable)")
	command.Flags().StringVar(&keyEnv, "key-env", "", "environment variable holding the provider key")
	command.Flags().BoolVar(&attest, "attest-free-tier", false, "attest that the Groq account or Gemini project has no paid plan")
	command.Flags().StringVar(&baseURL, "base-url", "", "custom provider base URL")
	command.Flags().StringVar(&tier, "tier", "", "zero-spend tier for the suggested model")
	command.Flags().BoolVar(&noTier, "no-tier", false, "do not add the suggested model to a tier")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "preview the edit without writing")
	command.Flags().BoolVar(&yes, "yes", false, "run without prompts using safe defaults")
	return command
}

func parseQuotaDomainArgs(values []string) (map[string]string, error) {
	domains := make(map[string]string, len(values))
	for _, value := range values {
		scope, alias, ok := strings.Cut(value, "=")
		if !ok || scope == "" || !config.ValidProviderInstanceName(alias) {
			return nil, errors.New("--quota-domain must use SCOPE=SAFE_ALIAS; alias values are hidden from diagnostics")
		}
		if _, exists := domains[scope]; exists {
			return nil, fmt.Errorf("--quota-domain repeats the %s scope", scope)
		}
		domains[scope] = alias
	}
	return domains, nil
}

func usageFailure(cmd *cobra.Command, message string) error {
	report(cmd.ErrOrStderr(), "spillway: provider add: %s\n", message)
	return commandError{code: 2}
}

func looksLikeKeyValue(value string) bool {
	return len(value) > 64 || strings.Contains(value, "sk-") || strings.Contains(value, "gsk_") || strings.Contains(value, "AIza")
}

func allProviderNames() []string {
	presets := providers.Presets()
	names := make([]string, 0, len(presets))
	for _, preset := range presets {
		names = append(names, preset.Name)
	}
	return names
}

func availableProviderNames(cfg config.Config) []string {
	var names []string
	for _, preset := range providers.Presets() {
		if _, exists := cfg.Providers[preset.Name]; !exists {
			names = append(names, preset.Name)
		}
	}
	return names
}

func providerConfigLine(ctx context.Context, path, name string) (int, error) {
	doc, err := config.OpenDocument(ctx, path)
	if err != nil {
		return 0, err
	}
	content, err := os.ReadFile(doc.Path()) // #nosec G304 -- path came from the operator's config argument.
	if err != nil {
		return 0, err
	}
	for line, text := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(text)
		if strings.TrimSuffix(trimmed, ":") == name {
			return line + 1, nil
		}
	}
	return 0, nil
}

func duplicateProviderMessage(path, name string, line int) error {
	lineText := ""
	if line > 0 {
		lineText = fmt.Sprintf(" (line %d)", line)
	}
	others := make([]string, 0, len(allProviderNames())-1)
	for _, provider := range allProviderNames() {
		if provider != name {
			others = append(others, provider)
		}
	}
	return fmt.Errorf("%s is already configured in %s%s. Spillway never rotates provider instances to bypass limits. A separately authorized instance for legitimate workload isolation can be added with `spillway provider add %s --name <safe-alias>`; otherwise add a different provider service: %s", name, path, lineText, name, strings.Join(others, ", "))
}

func chooseProviderToAdd(ctx context.Context, options Options, cfg config.Config) (string, error) {
	names := availableProviderNames(cfg)
	if len(names) == 0 {
		return "", errors.New("all supported providers are already configured")
	}
	choices := make([]huh.Option[string], 0, len(names))
	for _, name := range names {
		choices = append(choices, huh.NewOption(name, name))
	}
	selected := names[0]
	field := huh.NewSelect[string]().Title("Choose a provider to add").Options(choices...).Value(&selected)
	if err := runProviderForm(ctx, options, huh.NewForm(huh.NewGroup(field))); err != nil {
		return "", err
	}
	return selected, nil
}

func chooseKeyEnvironment(ctx context.Context, options Options, initial string) (string, error) {
	value := initial
	field := huh.NewInput().Title("Environment variable for the provider key").Value(&value).Validate(func(value string) error {
		if !environmentVariableName.MatchString(value) || looksLikeKeyValue(value) {
			return errors.New("enter an environment-variable name, not a key value")
		}
		return nil
	})
	if err := runProviderForm(ctx, options, huh.NewForm(huh.NewGroup(field))); err != nil {
		return "", err
	}
	return value, nil
}

func chooseFreeTierAttestation(ctx context.Context, options Options, provider string) (bool, error) {
	accepted := false
	field := huh.NewConfirm().Title("Attest that " + provider + " has no paid plan?").Description("Only then can it join a zero-spend tier.").Affirmative("Yes").Negative("No").Value(&accepted)
	if err := runProviderForm(ctx, options, huh.NewForm(huh.NewGroup(field))); err != nil {
		return false, err
	}
	return accepted, nil
}

func chooseTierPlacement(ctx context.Context, options Options, provider string) (bool, error) {
	add := false
	field := huh.NewConfirm().Title("Add the suggested model to tier \"free\"?").Description("The model id is a suggestion and may be outdated.").Affirmative("Yes").Negative("No").Value(&add)
	if err := runProviderForm(ctx, options, huh.NewForm(huh.NewGroup(field))); err != nil {
		return false, err
	}
	return add, nil
}

func confirmProviderWrite(ctx context.Context, options Options) (bool, error) {
	confirmed := false
	field := huh.NewConfirm().Title("Write these changes?").Description("A backup is saved first.").Affirmative("Yes").Negative("No").Value(&confirmed)
	if err := runProviderForm(ctx, options, huh.NewForm(huh.NewGroup(field))); err != nil {
		return false, err
	}
	return confirmed, nil
}

func runProviderForm(ctx context.Context, options Options, form *huh.Form) error {
	inputCloser, _ := options.Stdin.(io.Closer)
	return runInitForm(ctx, inputCloser, options.Stdin, options.Stdout, form)
}
