package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/spf13/cobra"
)

func addModelCommands(ctx context.Context, root *cobra.Command, options Options) {
	//nolint:contextcheck // Cobra supplies the invocation context through cmd.Context().
	model := &cobra.Command{Use: "model", Aliases: []string{"models"}, Short: "list and update configured models", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if commandTUIEnabled(cmd, options.interactive()) {
			if err := runModelBrowser(cmd.Context(), defaultConfigPath, "", "", options); err != nil {
				return commandFailure{code: 1, err: err}
			}
			return nil
		}
		return cmd.Help()
	}}
	model.AddGroup(&cobra.Group{ID: "manage", Title: "Model commands"})
	//nolint:contextcheck // Cobra supplies the invocation context through cmd.Context().
	list := &cobra.Command{Use: "list", Short: "list models configured in tiers", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		path, _ := cmd.Flags().GetString("config")
		tier, _ := cmd.Flags().GetString("tier")
		provider, _ := cmd.Flags().GetString("provider")
		if commandTUIEnabled(cmd, options.interactive()) {
			if err := runModelBrowser(cmd.Context(), path, tier, provider, options); err != nil {
				return commandFailure{code: 1, err: err}
			}
			return nil
		}
		cfg, err := loadProviderConfig(path, options.LookupEnv)
		if err == nil {
			err = writeModelList(cmd.OutOrStdout(), cfg, tier, provider)
		}
		if err != nil {
			return commandFailure{code: 1, err: err}
		}
		return nil
	}}
	list.Flags().String("config", defaultConfigPath, "path to the config file")
	list.Flags().String("tier", "", "show only models in this tier")
	list.Flags().String("provider", "", "show only models from this provider")
	list.GroupID = "manage"
	replace := &cobra.Command{Use: "replace <old-model> <new-model>", Short: "replace a model reference throughout the config", Args: cobra.ExactArgs(2), Example: "  spillway model replace gemini/gemini-2.5-flash gemini/gemini-3.5-flash -config config.yaml", RunE: func(cmd *cobra.Command, args []string) error {
		path, _ := cmd.Flags().GetString("config")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		doc, err := config.OpenDocument(ctx, path)
		if err != nil {
			return commandFailure{code: 1, err: err}
		}
		edit := doc.Edit()
		if err := edit.ReplaceModelReference(args[0], args[1]); err != nil {
			return commandFailure{code: 1, err: err}
		}
		change, err := edit.Result()
		if err != nil {
			return commandFailure{code: 1, err: err}
		}
		var result bytes.Buffer
		if err := change.WritePreview(&result); err != nil {
			return commandFailure{code: 1, err: fmt.Errorf("write preview: %w", err)}
		}
		if dryRun {
			_, err = fmt.Fprintf(&result, "Dry run only; %s was not changed.\n", doc.Path())
			if err != nil {
				return commandFailure{code: 1, err: err}
			}
		} else {
			updated, commitErr := doc.Commit(ctx, change, config.CommitOptions{Now: options.Now})
			if commitErr != nil {
				return commandFailure{code: 1, err: commitErr}
			}
			_, err = fmt.Fprintf(&result, "Updated %s; backup at %s.\nRestart Spillway to apply the change.\n", updated.Path, updated.BackupPath)
			if err != nil {
				return commandFailure{code: 1, err: err}
			}
		}
		if err := writeCommandResult(ctx, cmd, "spillway model replace", result.String(), options.interactive(), options.Stdin, cmd.OutOrStdout()); err != nil {
			return commandFailure{code: 1, err: fmt.Errorf("show replacement result: %w", err)}
		}
		return nil
	}}
	replace.Flags().String("config", defaultConfigPath, "path to the config file")
	replace.Flags().Bool("dry-run", false, "preview the replacement without writing")
	replace.GroupID = "manage"
	model.AddCommand(list, replace)
	model.GroupID = "manage"
	root.AddCommand(model)
}

func addTierCommands(ctx context.Context, root *cobra.Command, options Options) {
	tier := &cobra.Command{Use: "tier", Aliases: []string{"tiers"}, Short: "list configured model tiers", Args: cobra.NoArgs}
	tier.SetContext(ctx)
	tier.AddGroup(&cobra.Group{ID: "manage", Title: "Tier commands"})
	//nolint:contextcheck // Cobra supplies the invocation context through cmd.Context().
	tier.RunE = func(cmd *cobra.Command, _ []string) error {
		if commandTUIEnabled(cmd, options.interactive()) {
			if err := runTierBrowser(cmd.Context(), defaultConfigPath, options); err != nil {
				return commandFailure{code: 1, err: err}
			}
			return nil
		}
		return cmd.Help()
	}
	//nolint:contextcheck // Cobra supplies the invocation context through cmd.Context().
	list := &cobra.Command{Use: "list", Short: "list tiers and their routing order", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		path, _ := cmd.Flags().GetString("config")
		if commandTUIEnabled(cmd, options.interactive()) {
			if err := runTierBrowser(cmd.Context(), path, options); err != nil {
				return commandFailure{code: 1, err: err}
			}
			return nil
		}
		cfg, err := loadProviderConfig(path, options.LookupEnv)
		if err == nil {
			err = writeTierList(cmd.OutOrStdout(), cfg)
		}
		if err != nil {
			return commandFailure{code: 1, err: err}
		}
		return nil
	}}
	list.Flags().String("config", defaultConfigPath, "path to the config file")
	list.GroupID = "manage"
	tier.AddCommand(list)
	tier.GroupID = "manage"
	root.AddCommand(tier)
}

func runModelBrowser(ctx context.Context, path, onlyTier, onlyProvider string, options Options) error {
	cfg, err := loadProviderConfig(path, options.LookupEnv)
	if err != nil {
		return err
	}
	if onlyTier != "" {
		if _, ok := cfg.Tiers[onlyTier]; !ok {
			return fmt.Errorf("tier %q is not configured", onlyTier)
		}
	}
	entries := make([]helpEntry, 0)
	tierNames := make([]string, 0, len(cfg.Tiers))
	for name := range cfg.Tiers {
		tierNames = append(tierNames, name)
	}
	sort.Strings(tierNames)
	for _, tierName := range tierNames {
		if onlyTier != "" && tierName != onlyTier {
			continue
		}
		for _, model := range cfg.Tiers[tierName].Models {
			provider, _, _ := strings.Cut(model, "/")
			if onlyProvider != "" && provider != onlyProvider {
				continue
			}
			ref, err := providers.ParseModelRef(model)
			if err != nil {
				return err
			}
			zeroSpend := "no"
			if config.IsZeroSpendEligible(ref, cfg.Providers[provider]) {
				zeroSpend = "yes"
			}
			price := "not declared"
			if value, ok := cfg.Pricing[model]; ok {
				price = fmt.Sprintf("%s / %s USD per million tokens", formatMicroUSD(value.InputMicroUSDPerMillion), formatMicroUSD(value.OutputMicroUSDPerMillion))
			}
			entries = append(entries, helpEntry{
				name:   model,
				group:  "Tier " + tierName,
				short:  fmt.Sprintf("zero-spend eligible: %s · price: %s", zeroSpend, price),
				detail: fmt.Sprintf("Model: %s\nTier: %s\nConfig zero-spend eligible: %s\nPrice (input / output): %s", model, tierName, zeroSpend, price),
			})
		}
	}
	return runBrowser(ctx, "model catalog", "Browse configured models and their tier pricing.", entries, options.Stdin, options.Stdout)
}

func runTierBrowser(ctx context.Context, path string, options Options) error {
	cfg, err := loadProviderConfig(path, options.LookupEnv)
	if err != nil {
		return err
	}
	order := make(map[string]int, len(cfg.Router.Auto))
	for index, name := range cfg.Router.Auto {
		order[name] = index + 1
	}
	names := make([]string, 0, len(cfg.Tiers))
	for name := range cfg.Tiers {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]helpEntry, 0, len(names))
	for _, name := range names {
		tier := cfg.Tiers[name]
		spending, auto := "no", "not in auto"
		if tier.AllowSpend {
			spending = "yes"
		}
		if position, ok := order[name]; ok {
			auto = fmt.Sprintf("position %d", position)
		}
		detail := fmt.Sprintf("Tier: %s\nAllows paid models: %s\nAuto order: %s\n\nModels:\n%s", name, spending, auto, strings.Join(tier.Models, "\n"))
		entries = append(entries, helpEntry{
			name:   name,
			group:  "Model tier",
			short:  fmt.Sprintf("%d models · paid models allowed: %s · %s", len(tier.Models), spending, auto),
			detail: detail,
		})
	}
	return runBrowser(ctx, "model tiers", "Inspect tier contents and auto-routing order.", entries, options.Stdin, options.Stdout)
}

func writeModelList(out io.Writer, cfg config.Config, onlyTier, onlyProvider string) error {
	if onlyTier != "" {
		if _, ok := cfg.Tiers[onlyTier]; !ok {
			return fmt.Errorf("tier %q is not configured", onlyTier)
		}
	}
	if _, err := fmt.Fprintln(out, "MODEL\tTIER\tCONFIG ZERO-SPEND\tPRICE (USD / MTOK)"); err != nil {
		return err
	}
	names := make([]string, 0, len(cfg.Tiers))
	for name := range cfg.Tiers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, tierName := range names {
		if onlyTier != "" && tierName != onlyTier {
			continue
		}
		for _, model := range cfg.Tiers[tierName].Models {
			provider, _, _ := strings.Cut(model, "/")
			if onlyProvider != "" && provider != onlyProvider {
				continue
			}
			ref, err := providers.ParseModelRef(model)
			if err != nil {
				return err
			}
			zeroSpend := "no"
			if config.IsZeroSpendEligible(ref, cfg.Providers[provider]) {
				zeroSpend = "yes"
			}
			price := "not declared"
			if value, ok := cfg.Pricing[model]; ok {
				price = fmt.Sprintf("%s / %s", formatMicroUSD(value.InputMicroUSDPerMillion), formatMicroUSD(value.OutputMicroUSDPerMillion))
			}
			if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", model, tierName, zeroSpend, price); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeTierList(out io.Writer, cfg config.Config) error {
	order := make(map[string]int, len(cfg.Router.Auto))
	for i, name := range cfg.Router.Auto {
		order[name] = i + 1
	}
	names := make([]string, 0, len(cfg.Tiers))
	for name := range cfg.Tiers {
		names = append(names, name)
	}
	sort.Strings(names)
	if _, err := fmt.Fprintln(out, "TIER\tALLOW SPEND\tMODELS\tAUTO ORDER"); err != nil {
		return err
	}
	for _, name := range names {
		t := cfg.Tiers[name]
		spending, auto := "no", "-"
		if t.AllowSpend {
			spending = "yes"
		}
		if position, ok := order[name]; ok {
			auto = fmt.Sprint(position)
		}
		if _, err := fmt.Fprintf(out, "%s\t%s\t%d\t%s\n", name, spending, len(t.Models), auto); err != nil {
			return err
		}
	}
	return nil
}

func formatMicroUSD(value int64) string {
	whole, fraction := value/1_000_000, value%1_000_000
	if fraction == 0 {
		return fmt.Sprint(whole)
	}
	return fmt.Sprintf("%d.%s", whole, strings.TrimRight(fmt.Sprintf("%06d", fraction), "0"))
}
