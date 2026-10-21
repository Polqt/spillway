package cli

import (
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
	model := &cobra.Command{Use: "model", Aliases: []string{"models"}, Short: "list and update configured models", Args: cobra.NoArgs}
	model.AddGroup(&cobra.Group{ID: "manage", Title: "Model commands"})
	list := &cobra.Command{Use: "list", Short: "list models configured in tiers", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		path, _ := cmd.Flags().GetString("config")
		tier, _ := cmd.Flags().GetString("tier")
		provider, _ := cmd.Flags().GetString("provider")
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
	replace := &cobra.Command{Use: "replace <old-model> <new-model>", Short: "replace a model reference throughout the config", Args: cobra.ExactArgs(2), Example: "  spillway model replace gemini/gemini-2.5-flash gemini/gemini-3-flash-preview -config config.yaml", RunE: func(cmd *cobra.Command, args []string) error {
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
		if err := change.WritePreview(cmd.OutOrStdout()); err != nil {
			return commandFailure{code: 1, err: fmt.Errorf("write preview: %w", err)}
		}
		if dryRun {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Dry run only; %s was not changed.\n", doc.Path())
			if err != nil {
				return commandFailure{code: 1, err: err}
			}
			return nil
		}
		result, err := doc.Commit(ctx, change, config.CommitOptions{Now: options.Now})
		if err != nil {
			return commandFailure{code: 1, err: err}
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Updated %s; backup at %s.\nRestart Spillway to apply the change.\n", result.Path, result.BackupPath)
		if err != nil {
			return commandFailure{code: 1, err: err}
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

func addTierCommands(root *cobra.Command, options Options) {
	tier := &cobra.Command{Use: "tier", Aliases: []string{"tiers"}, Short: "list configured model tiers", Args: cobra.NoArgs}
	tier.AddGroup(&cobra.Group{ID: "manage", Title: "Tier commands"})
	list := &cobra.Command{Use: "list", Short: "list tiers and their routing order", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		path, _ := cmd.Flags().GetString("config")
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
