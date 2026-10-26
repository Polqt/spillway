package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/Polqt/spillway/internal/cli/provideraddui"
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
			interactive := commandTUIEnabled(cmd, options.interactive()) && !yes
			if serviceType == "" && !interactive {
				return usageFailure(cmd, "provider name is required when prompts are unavailable; choose one of: "+strings.Join(availableProviderNames(cfg), ", "))
			}
			if serviceType != "" {
				if _, ok := providers.LookupPreset(serviceType); !ok {
					return commandFailure{code: 1, err: fmt.Errorf("%q is not a supported provider; choose one of %s", serviceType, strings.Join(allProviderNames(), ", "))}
				}
			}
			if instanceName != "" && !config.ValidProviderInstanceName(instanceName) {
				return commandFailure{code: 1, err: errors.New("--name must be a safe alias matching [a-z][a-z0-9-]{0,31}; do not use an account or project identifier")}
			}

			askProvider := serviceType == ""
			askKeyEnv := interactive && !cmd.Flags().Changed("key-env")
			askAttestation := interactive && !cmd.Flags().Changed("attest-free-tier")
			askTier := interactive && tier == "" && !noTier && hasSuggestedTier(cfg)
			needsWizard := interactive && (askProvider || askKeyEnv || askAttestation || askTier)
			initialKeyEnv := keyEnv
			if initialKeyEnv == "" && serviceType != "" {
				preset, _ := providers.LookupPreset(serviceType)
				if preset.RequiresKey {
					initialKeyEnv = preset.KeyEnv
					if instanceName != "" && instanceName != serviceType {
						initialKeyEnv = strings.ToUpper(strings.ReplaceAll(instanceName, "-", "_")) + "_API_KEY"
					}
				}
			}

			var committed config.CommitResult
			var warning, finalKeyEnv, finalPath string
			prepare := func(draft provideraddui.Draft) (provideraddui.Prepared, error) {
				service := draft.ServiceType
				preset, ok := providers.LookupPreset(service)
				if !ok {
					return provideraddui.Prepared{}, fmt.Errorf("%q is not a supported provider; choose one of %s", service, strings.Join(allProviderNames(), ", "))
				}
				name := service
				if draft.InstanceName != "" {
					name = draft.InstanceName
				}
				if _, exists := cfg.Providers[name]; exists {
					line, _ := providerConfigLine(ctx, path, name)
					if name == service {
						return provideraddui.Prepared{}, duplicateProviderMessage(path, name, line)
					}
					return provideraddui.Prepared{}, fmt.Errorf("provider instance %s is already configured in %s (line %d)", name, path, line)
				}
				keyName := draft.KeyEnv
				if preset.RequiresKey {
					if keyName == "" {
						keyName = preset.KeyEnv
						if name != service {
							keyName = strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_API_KEY"
						}
					}
					if !environmentVariableName.MatchString(keyName) || looksLikeKeyValue(keyName) {
						return provideraddui.Prepared{}, errors.New("--key-env takes a variable name such as GEMINI_API_KEY, not a key value")
					}
				} else if keyName != "" {
					return provideraddui.Prepared{}, fmt.Errorf("%s does not take a provider key", service)
				}
				if draft.AttestFreeTier && !preset.FreeTierAttestable {
					return provideraddui.Prepared{}, fmt.Errorf("--attest-free-tier is supported only for groq and gemini, not %s", service)
				}
				domains, err := parseQuotaDomainArgs(quotaDomainArgs)
				if err != nil {
					return provideraddui.Prepared{}, err
				}
				doc, err := config.OpenDocument(ctx, path)
				if err != nil {
					return provideraddui.Prepared{}, err
				}
				edit := doc.Edit()
				entry := config.ProviderEntry{Name: name, KeyEnv: keyName, BaseURL: baseURL, FreeTier: draft.AttestFreeTier, QuotaDomains: domains}
				if name != service {
					entry.Type = service
				}
				if err := edit.AddProvider(entry); err != nil {
					return provideraddui.Prepared{}, err
				}
				warning = ""
				selectedTierName := draft.Tier
				if !draft.AddSuggestedModel {
					selectedTierName = ""
				}
				if selectedTierName != "" {
					selectedTier, exists := cfg.Tiers[selectedTierName]
					if !exists {
						return provideraddui.Prepared{}, fmt.Errorf("tiers.%s does not exist", selectedTierName)
					}
					if selectedTier.AllowSpend {
						return provideraddui.Prepared{}, fmt.Errorf("provider add can place a suggested model only in a zero-spend tier; %s allows spending", selectedTierName)
					}
					modelRef := name + "/" + preset.SuggestedModel
					ref, err := providers.ParseModelRef(modelRef)
					if err != nil {
						return provideraddui.Prepared{}, err
					}
					selectedProvider := config.Provider{Type: service, FreeTier: draft.AttestFreeTier}
					if !config.IsZeroSpendEligible(ref, selectedProvider) && service != "gemini" && service != "groq" {
						warning = fmt.Sprintf("Suggested model %s was not added to tier %q because it is not known to be zero-spend eligible (ADR 0005).\n", modelRef, selectedTierName)
					} else if (service == "gemini" || service == "groq") && !draft.AttestFreeTier {
						warning = fmt.Sprintf("Suggested model %s was not added to tier %q because %s is not attested as having no paid plan.\n", modelRef, selectedTierName, name)
					} else if err := edit.AppendTierModel(selectedTierName, modelRef); err != nil {
						return provideraddui.Prepared{}, err
					}
				}
				change, err := edit.Result()
				if err != nil {
					return provideraddui.Prepared{}, err
				}
				var preview strings.Builder
				if err := change.WritePreview(&preview); err != nil {
					return provideraddui.Prepared{}, fmt.Errorf("write preview: %w", err)
				}
				finalKeyEnv, finalPath = keyName, doc.Path()
				prepared := provideraddui.Prepared{
					Preview: preview.String(),
					Success: "Dry run complete. No config changes were written.",
				}
				if !dryRun {
					prepared.Success = "Provider change saved. Restart Spillway to load the updated config."
					prepared.Save = func(saveCtx context.Context) error {
						result, err := doc.Commit(saveCtx, change, config.CommitOptions{Now: options.Now})
						if err != nil {
							return err
						}
						committed = result
						return nil
					}
				}
				return prepared, nil
			}

			var draft provideraddui.Draft
			var result bytes.Buffer
			resultOutput := cmd.OutOrStdout()
			if needsWizard {
				choiceNames := allProviderNames()
				if askProvider {
					choiceNames = availableProviderNames(cfg)
				}
				if len(choiceNames) == 0 {
					return commandFailure{code: 1, err: errors.New("all supported providers are already configured")}
				}
				_, err := provideraddui.Run(ctx, provideraddui.Options{
					ConfigPath: path, Providers: providerAddUIChoices(choiceNames), InitialService: serviceType,
					InstanceName: instanceName, InitialKeyEnv: initialKeyEnv, InitialAttested: attest,
					InitialTier: tier, AskProvider: askProvider, AskKeyEnv: askKeyEnv,
					AskAttestation: askAttestation, AskTier: askTier, DryRun: dryRun,
					ValidateKeyEnv: func(value string) error {
						if !environmentVariableName.MatchString(value) || looksLikeKeyValue(value) {
							return errors.New("enter an environment-variable name, not a key value")
						}
						return nil
					},
					Prepare: prepare,
				}, cmd.InOrStdin(), cmd.OutOrStdout())
				if err != nil {
					if errors.Is(err, context.Canceled) {
						return commandFailure{code: 1, err: errors.New("provider add cancelled; config was not changed")}
					}
					return commandFailure{code: 1, err: err}
				}
			} else {
				if commandTUIEnabled(cmd, options.interactive()) {
					resultOutput = &result
				}
				draft = provideraddui.Draft{
					ConfigPath: path, ServiceType: serviceType, InstanceName: instanceName,
					KeyEnv: initialKeyEnv, AttestFreeTier: attest, Tier: tier,
					AddSuggestedModel: tier != "" && !noTier,
				}
				prepared, err := prepare(draft)
				if err != nil {
					return commandFailure{code: 1, err: err}
				}
				if _, err := io.WriteString(resultOutput, prepared.Preview); err != nil {
					return commandFailure{code: 1, err: fmt.Errorf("write preview: %w", err)}
				}
				if dryRun {
					_, err := fmt.Fprintf(resultOutput, "Dry run only; %s was not changed.\n", finalPath)
					if err != nil {
						return commandFailure{code: 1, err: err}
					}
				} else if prepared.Save != nil {
					if err := prepared.Save(ctx); err != nil {
						return commandFailure{code: 1, err: err}
					}
				}
			}

			if warning != "" {
				if _, err := io.WriteString(resultOutput, warning); err != nil {
					return commandFailure{code: 1, err: err}
				}
			}
			backupNote := "owner-only; contains local keys"
			if options.GOOS == "windows" {
				backupNote = "contains local keys; original Windows permissions kept"
			}
			if !dryRun {
				if _, err := fmt.Fprintf(resultOutput, "Updated %s; backup at %s (%s).\n", committed.Path, committed.BackupPath, backupNote); err != nil {
					return commandFailure{code: 1, err: err}
				}
				if committed.ModeTightened {
					if _, err := fmt.Fprintln(resultOutput, "Config permissions were tightened to owner-only."); err != nil {
						return commandFailure{code: 1, err: err}
					}
				}
				if finalKeyEnv != "" {
					if _, ok := options.LookupEnv(finalKeyEnv); !ok {
						if _, err := fmt.Fprintf(resultOutput, "%s is not set in this terminal. Run spillway doctor for shell-specific setup guidance.\n", finalKeyEnv); err != nil {
							return commandFailure{code: 1, err: err}
						}
					}
				}
				if _, err := fmt.Fprintf(resultOutput, "Spillway reads config only at startup; restart spillway serve -config %s to apply.\n", committed.Path); err != nil {
					return commandFailure{code: 1, err: err}
				}
			}
			if result.Len() > 0 {
				if err := writeCommandResult(ctx, cmd, "spillway provider add", result.String(), options.interactive(), options.Stdin, cmd.OutOrStdout()); err != nil {
					return commandFailure{code: 1, err: fmt.Errorf("show provider result: %w", err)}
				}
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

func providerAddUIChoices(names []string) []provideraddui.Provider {
	choices := make([]provideraddui.Provider, 0, len(names))
	for _, name := range names {
		preset, ok := providers.LookupPreset(name)
		if !ok {
			continue
		}
		choices = append(choices, provideraddui.Provider{
			Name: preset.Name, Hint: providerAddHint(name), KeyEnv: preset.KeyEnv,
			SuggestedModel: preset.SuggestedModel, RequiresKey: preset.RequiresKey,
			FreeTierAttestable: preset.FreeTierAttestable,
		})
	}
	return choices
}

func providerAddHint(name string) string {
	switch name {
	case "gemini":
		return "Google AI"
	case "groq":
		return "fast inference"
	case "ollama":
		return "local Ollama server"
	case "openrouter":
		return "hosted model catalog"
	default:
		return "provider service"
	}
}

func hasSuggestedTier(cfg config.Config) bool {
	tier, exists := cfg.Tiers["free"]
	return exists && !tier.AllowSpend
}
