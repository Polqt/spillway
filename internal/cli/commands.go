package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"charm.land/lipgloss/v2"
	evalcmd "github.com/Polqt/spillway/internal/cli/eval"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type commandError struct {
	code int
}

func (e commandError) Error() string { return "command failed" }

type commandFailure struct {
	code int
	err  error
}

func (e commandFailure) Error() string { return e.err.Error() }
func (e commandFailure) Unwrap() error { return e.err }

func newRootCommand(ctx context.Context, originalArgs []string, stdin io.Reader, stdout, stderr io.Writer, lookupEnv func(string) (string, bool), interactive bool, exitCode *int) *cobra.Command {
	root := &cobra.Command{
		Use:   "spillway",
		Short: "A local OpenAI-compatible LLM gateway",
		Long: strings.TrimSpace(`Spillway routes OpenAI-compatible requests across the providers you configure.

Quick start:
  spillway init
  spillway check -config config.yaml
  spillway serve -config config.yaml

Local Ollama path:
  spillway init -yes -providers ollama -store
  spillway serve -config config.yaml

Flags accept one or two dashes: -config or --config.`),
		Example: `  spillway init
	  spillway init -yes -providers ollama -store
  spillway check -config config.yaml
  spillway serve -config config.yaml`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetUsageTemplate(`{{if .HasParent}}usage: {{.CommandPath}} [flags]{{else}}usage: spillway <command>{{end}}
{{if .Short}}
{{.Short}}{{end}}
{{if .Long}}
{{.Long}}{{end}}
{{if .HasAvailableSubCommands}}
commands:
{{range .Groups}}{{$group := .}}{{.Title}}
{{range $.Commands}}{{if (and (eq .GroupID $group.ID) (not .Hidden) (not .Deprecated))}}  {{rpad .Name .NamePadding }} {{.Short}}
{{end}}{{end}}{{end}}{{end}}{{if .HasExample}}
Examples:
{{.Example}}{{end}}{{if .HasAvailableLocalFlags}}
flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if not .HasParent}}
Run 'spillway <command> --help' for details.
{{end}}
`)
	root.AddGroup(
		&cobra.Group{ID: "start", Title: "Get started"},
		&cobra.Group{ID: "diagnose", Title: "Diagnose"},
		&cobra.Group{ID: "manage", Title: "Manage"},
		&cobra.Group{ID: "tools", Title: "Tools"},
		&cobra.Group{ID: "experimental", Title: "Experimental"},
	)
	addConfigCommands(root, exitCode, ctx, stdin, stdout, stderr, lookupEnv, interactive)
	root.SetHelpCommandGroupID("tools")
	root.SetCompletionCommandGroupID("tools")
	var noTUI bool
	root.PersistentFlags().BoolVar(&noTUI, "no-tui", false, "use linear command output instead of interactive screens")
	writeHelp := func(cmd *cobra.Command, out io.Writer) {
		if _, err := lipgloss.Fprintln(styledWriter(out), brandHeader("help")+"\n"+cmd.UsageString()); err != nil {
			report(cmd.ErrOrStderr(), "spillway: write help: %v\n", err)
			*exitCode = 1
		}
	}
	root.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
		if cmd == root && interactive && !noTUI {
			if err := runHelpExplorer(ctx, root, stdin, stdout); err != nil {
				report(stderr, "spillway: help: %v\n", err)
				*exitCode = 1
			}
			return
		}
		writeHelp(cmd, cmd.OutOrStdout())
	})
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return err })
	root.AddGroup(
		&cobra.Group{ID: "start", Title: "Get started"},
		&cobra.Group{ID: "tools", Title: "Tools"},
		&cobra.Group{ID: "experimental", Title: "Experimental"},
	)
	showVersion := false
	root.Flags().BoolVar(&showVersion, "version", false, "print version information")
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		if showVersion {
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), versionLine()); err != nil {
				report(cmd.ErrOrStderr(), "spillway: write version: %v\n", err)
				*exitCode = 1
			}
			return nil
		}
		if interactive && !noTUI {
			if err := runHelpExplorer(ctx, root, stdin, stdout); err != nil {
				report(stderr, "spillway: help: %v\n", err)
				*exitCode = 1
			}
			return nil
		}
		writeHelp(cmd, cmd.ErrOrStderr())
		return commandError{code: 2}
	}

	addLegacyCommand(root, exitCode, "serve", "serve the OpenAI-compatible API", `  spillway serve -config config.yaml`, func(cmd *cobra.Command, args []string) int {
		return serve(ctx, legacyArgs(cmd, originalArgs), stderr, lookupEnv, commandTUIEnabled(cmd, interactive))
	}, func(flags *pflag.FlagSet) {
		flags.String("config", defaultConfigPath, "path to the config file")
	})
	addLegacyCommand(root, exitCode, "check", "validate config and print it with secrets redacted", `  spillway check -config config.yaml`, func(cmd *cobra.Command, _ []string) int {
		args := legacyArgs(cmd, originalArgs)
		if !commandTUIEnabled(cmd, interactive) {
			return check(args, stdout, stderr, lookupEnv)
		}
		var result, diagnostics bytes.Buffer
		code := check(args, &result, &diagnostics, lookupEnv)
		if code == 2 {
			_, _ = stderr.Write(diagnostics.Bytes())
			return code
		}
		detail := strings.TrimSpace(result.String() + "\n" + diagnostics.String())
		entry := helpEntry{name: "Config validation", group: "Diagnostics", short: "Validate config with secrets redacted", detail: detail}
		if err := runBrowser(ctx, "config check", "Review the redacted configuration and validation status.", []helpEntry{entry}, stdin, stdout); err != nil {
			report(stderr, "spillway: check: %v\n", err)
			return 1
		}
		return code
	}, func(flags *pflag.FlagSet) {
		flags.String("config", defaultConfigPath, "path to the config file")
	})
	addLegacyCommand(root, exitCode, "key", "print one local key by name", `  spillway key default
  spillway key dashboard`, func(cmd *cobra.Command, args []string) int {
		return keyCommand(legacyArgs(cmd, originalArgs), stdout, stderr, lookupEnv)
	}, func(flags *pflag.FlagSet) {
		flags.String("config", defaultConfigPath, "path to the config file")
	})
	addLegacyCommand(root, exitCode, "catalog", "suggest model declarations and pricing from configured providers", `  spillway catalog -config config.yaml`, func(cmd *cobra.Command, args []string) int {
		if commandTUIEnabled(cmd, interactive) {
			_, _ = lipgloss.Fprintln(styledWriter(stderr), brandHeader("catalog")+"  "+mutedStyle.Render("Querying configured providers…"))
		}
		var result bytes.Buffer
		code := catalogCommand(ctx, legacyArgs(cmd, originalArgs), &result, stderr, lookupEnv)
		if code != 0 {
			if result.Len() > 0 {
				_, _ = io.Copy(stdout, &result)
			}
			return code
		}
		if err := writeCommandResult(ctx, cmd, "spillway catalog", result.String(), interactive, stdin, stdout); err != nil {
			report(stderr, "spillway: catalog: show result: %v\n", err)
			return 1
		}
		return 0
	}, func(flags *pflag.FlagSet) {
		flags.String("config", defaultConfigPath, "path to the config file")
	})
	addLegacyCommand(root, exitCode, "init", "create a local config and key", `  spillway init
  spillway init -providers groq,gemini -yes`, func(cmd *cobra.Command, args []string) int {
		return initCommand(ctx, legacyArgs(cmd, originalArgs), stdin, stdout, stderr, lookupEnv)
	}, func(flags *pflag.FlagSet) {
		flags.String("config", defaultConfigPath, "path for the new config file")
		flags.String("providers", "", "comma-separated providers to configure")
		flags.String("attest-free-tier", "", "comma-separated providers attested as zero-spend")
		flags.Bool("store", false, "enable the local usage store and dashboard key")
		flags.Bool("yes", false, "accept defaults and run without prompts")
		flags.Bool("overwrite", false, "replace an existing config after saving a backup")
	})
	addLegacyCommand(root, exitCode, "dashboard", "watch a running gateway in a live terminal dashboard", `  spillway dashboard
  spillway dashboard -config config.yaml -interval 5s`, func(cmd *cobra.Command, args []string) int {
		return dashboardCommand(ctx, legacyArgs(cmd, originalArgs), stdin, stdout, stderr, lookupEnv)
	}, func(flags *pflag.FlagSet) {
		flags.String("config", defaultConfigPath, "path to the config file holding the dashboard key")
		flags.String("endpoint", "http://127.0.0.1:8080", "running Spillway base URL")
		flags.Duration("interval", 2*time.Second, "refresh interval (1s-1m)")
	})
	addLegacyCommand(root, exitCode, "eval", "run a paired routing evaluation (experimental)", `  spillway eval -dataset prompts.json -arms current=auto -out results`, func(cmd *cobra.Command, args []string) int {
		if commandTUIEnabled(cmd, interactive) {
			_, _ = lipgloss.Fprintln(styledWriter(stderr), brandHeader("eval")+"  "+mutedStyle.Render("Evaluation running · this may take a while"))
		}
		var result bytes.Buffer
		code := evalcmd.Run(ctx, legacyArgs(cmd, originalArgs), &result, stderr, lookupEnv)
		if code != 0 {
			return code
		}
		if err := writeCommandResult(ctx, cmd, "spillway eval", result.String(), interactive, stdin, stdout); err != nil {
			report(stderr, "spillway: eval: show result: %v\n", err)
			return 1
		}
		return 0
	}, func(flags *pflag.FlagSet) {
		flags.String("dataset", "", "versioned prompt set JSON path")
		flags.String("endpoint", "http://127.0.0.1:8080", "Spillway OpenAI-compatible base URL")
		flags.String("api-key-env", "", "environment variable holding the local API key")
		flags.String("arms", "", "comma-separated name=model[@endpoint] arms, for example current=auto")
		flags.Int("repeats", 1, "repetitions per prompt and arm (1-100)")
		flags.Int64("seed", 0, "randomization seed; zero chooses a random seed")
		flags.Duration("timeout", 2*time.Hour, "whole-run deadline")
		flags.String("prices", "", "optional JSON map of model references to declared prices")
		flags.String("out", "", "directory for evaluation artifacts")
		flags.Bool("rules-benchmark", false, "validate sample size, repeats, seed and arms for a rules benchmark")
		flags.String("candidate-set-sha256", "", "operator-attested shared candidate-set SHA-256 digest; verify both routing configs match")
		flags.Bool("benchmark-pilot", false, "run a zero-spend mock rules benchmark pilot")
		flags.Bool("benchmark-live", false, "run a paid rules benchmark after required approvals")
		flags.Bool("approve-live-run", false, "record separate owner approval for this live benchmark run")
		flags.Bool("approve-candidate-set", false, "record separate owner approval for the candidate set")
		flags.Bool("approve-t19-recovery", false, "record resolution of T19 or approval of its documented recovery path")
		flags.String("mock-pilot-report", "", "completed zero-spend mock pilot report required for a live benchmark")
		flags.Bool("mock-pilot-inspected", false, "confirm the zero-spend mock pilot was inspected")
		flags.Float64("cost-ceiling-usd", 0, "separately approved estimated-cost ceiling for the live run in USD")
	})
	addLegacyCommand(root, exitCode, "eval-score", "summarize a completed blinded evaluation score sheet (experimental)", `  spillway eval-score -run results`, func(cmd *cobra.Command, args []string) int {
		var result bytes.Buffer
		code := evalcmd.RunScore(ctx, legacyArgs(cmd, originalArgs), &result, stderr)
		if code != 0 {
			return code
		}
		if err := writeCommandResult(ctx, cmd, "spillway eval-score", result.String(), interactive, stdin, stdout); err != nil {
			report(stderr, "spillway: eval-score: show result: %v\n", err)
			return 1
		}
		return 0
	}, func(flags *pflag.FlagSet) {
		flags.String("run", "", "evaluation artifact directory")
	})
	addLegacyCommand(root, exitCode, "eval-report", "compare run artifacts and rater sheets against ADR 0014 gates (experimental)", `  spillway eval-report -run results -baseline current`, func(cmd *cobra.Command, args []string) int {
		evalArgs := legacyArgs(cmd, originalArgs)
		var result bytes.Buffer
		code := evalcmd.RunReport(ctx, evalArgs, &result, stderr)
		if code != 0 {
			return code
		}
		useTUI := interactive && !legacyFlagEnabled(evalArgs, "json")
		if err := writeCommandResult(ctx, cmd, "spillway eval-report", result.String(), useTUI, stdin, stdout); err != nil {
			report(stderr, "spillway: eval-report: show result: %v\n", err)
			return 1
		}
		return 0
	}, func(flags *pflag.FlagSet) {
		flags.String("run", "", "evaluation artifact directory")
		flags.String("scores", "", "comma-separated completed rater CSV sheets")
		flags.String("baseline", "", "baseline arm name")
		flags.Int64("seed", 1, "paired bootstrap random seed")
		flags.Int("replicates", 10000, "paired prompt-cluster bootstrap replicates")
	})
	addLegacyCommand(root, exitCode, "version", "print the version and exit", "  spillway version\n  spillway version -verbose", func(cmd *cobra.Command, _ []string) int {
		lines := []string{versionLine()}
		if verbose, _ := cmd.Flags().GetBool("verbose"); verbose {
			lines = append(lines, versionDetails()...)
		}
		if err := writeCommandResult(ctx, cmd, "spillway version", strings.Join(lines, "\n")+"\n", interactive, stdin, stdout); err != nil {
			report(stderr, "spillway: write version: %v\n", err)
			return 1
		}
		return 0
	}, func(flags *pflag.FlagSet) {
		flags.Bool("verbose", false, "also print the executable path, module, VCS settings and dashboard assets")
	})
	for _, command := range root.Commands() {
		switch command.Name() {
		case "init", "serve":
			command.GroupID = "start"
		case "eval", "eval-score", "eval-report":
			command.GroupID = "experimental"
		default:
			command.GroupID = "tools"
		}
	}
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	if help := root.Commands(); len(help) > 0 {
		for _, command := range help {
			switch command.Name() {
			case "help":
				command.Short = "help about any command"
			case "completion":
				command.Short = "generate shell completion scripts"
			}
		}
	}

	return root
}

func writeCommandResult(ctx context.Context, cmd *cobra.Command, title, content string, interactive bool, input io.Reader, output io.Writer) error {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	if commandTUIEnabled(cmd, interactive) {
		return runResultScreen(ctx, title, content, input, output)
	}
	_, err := io.WriteString(output, content)
	return err
}

func legacyFlagEnabled(args []string, name string) bool {
	for _, arg := range args {
		arg = strings.TrimLeft(arg, "-")
		flagName, value, hasValue := strings.Cut(arg, "=")
		if flagName != name {
			continue
		}
		return !hasValue || (value != "false" && value != "0")
	}
	return false
}

func addManagementCommands(ctx context.Context, root *cobra.Command, options Options) {
	addProviderCommands(ctx, root, options)
	addModelCommands(ctx, root, options)
	addTierCommands(ctx, root, options)
	root.AddCommand(setupCommand(ctx, options))
	install := installCommand(ctx, options)
	install.GroupID = "manage"
	root.AddCommand(install)
	doctor := doctorCommand(ctx, options)
	doctor.GroupID = "diagnose"
	root.AddCommand(doctor)
	for _, command := range root.Commands() {
		if command.Name() == "provider" {
			command.GroupID = "manage"
			command.AddCommand(providerAddCommand(ctx, options))
		}
	}
}

func addLegacyCommand(root *cobra.Command, exitCode *int, name, short, example string, run func(*cobra.Command, []string) int, addFlags func(*pflag.FlagSet)) {
	group := "tools"
	switch name {
	case "init", "check", "serve":
		group = "start"
	case "eval", "eval-score", "eval-report":
		group = "experimental"
	}
	command := &cobra.Command{
		Use:     name,
		Short:   short,
		Example: example,
		GroupID: group,
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			*exitCode = normalizeExitCode(run(cmd, args))
			return nil
		},
	}
	if addFlags != nil {
		addFlags(command.Flags())
	}
	root.AddCommand(command)
}

func normalizeExitCode(code int) int {
	if code < 0 || code > 2 {
		return 1
	}
	return code
}

func legacyArgs(cmd *cobra.Command, originalArgs []string) []string {
	parsedCommand, args, err := cmd.Root().Find(originalArgs)
	if err == nil && parsedCommand == cmd {
		return args
	}
	return cmd.Flags().Args()
}
