package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

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

func newRootCommand(ctx context.Context, originalArgs []string, stdin io.Reader, stdout, stderr io.Writer, lookupEnv func(string) (string, bool), exitCode *int) *cobra.Command {
	root := &cobra.Command{
		Use:   "spillway",
		Short: "A local OpenAI-compatible LLM gateway",
		Long: strings.TrimSpace(`Spillway routes OpenAI-compatible requests across the providers you configure.

Quick start:
  spillway init
  spillway check -config config.yaml
  spillway serve -config config.yaml

Offline mock path:
  spillway init -yes -providers mock -store
  spillway serve -config config.yaml

Flags accept one or two dashes: -config or --config.`),
		Example: `  spillway init
  spillway init -yes -providers mock -store
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
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}
`)
	root.AddGroup(
		&cobra.Group{ID: "start", Title: "Get started"},
		&cobra.Group{ID: "diagnose", Title: "Diagnose"},
		&cobra.Group{ID: "manage", Title: "Manage"},
		&cobra.Group{ID: "tools", Title: "Tools"},
		&cobra.Group{ID: "experimental", Title: "Experimental"},
	)
	root.SetHelpCommandGroupID("tools")
	root.SetCompletionCommandGroupID("tools")
	root.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
		out := cmd.OutOrStdout()
		if _, err := fmt.Fprint(styledWriter(out), "\n  "+brandHeader(version)+"\n\n"+cmd.UsageString()); err != nil {
			report(cmd.ErrOrStderr(), "spillway: write help: %v\n", err)
			*exitCode = 1
		}
	})
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return err })
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
		_, _ = fmt.Fprint(cmd.ErrOrStderr(), cmd.UsageString())
		return commandError{code: 2}
	}

	addLegacyCommand(root, exitCode, "serve", "serve the OpenAI-compatible API", `  spillway serve -config config.yaml`, func(cmd *cobra.Command, args []string) int {
		return serve(ctx, legacyArgs(cmd, originalArgs), stderr, lookupEnv)
	}, func(flags *pflag.FlagSet) {
		flags.String("config", defaultConfigPath, "path to the config file")
	})
	addLegacyCommand(root, exitCode, "check", "validate config and print it with secrets redacted", `  spillway check -config config.yaml`, func(cmd *cobra.Command, args []string) int {
		return check(legacyArgs(cmd, originalArgs), stdout, stderr, lookupEnv)
	}, func(flags *pflag.FlagSet) {
		flags.String("config", defaultConfigPath, "path to the config file")
	})
	addLegacyCommand(root, exitCode, "key", "print one local key by name", `  spillway key default
  spillway key dashboard`, func(cmd *cobra.Command, args []string) int {
		return keyCommand(legacyArgs(cmd, originalArgs), stdout, stderr, lookupEnv)
	}, func(flags *pflag.FlagSet) {
		flags.String("config", defaultConfigPath, "path to the config file")
	})
	addLegacyCommand(root, exitCode, "dashboard", "watch a running gateway in a live terminal dashboard", `  spillway dashboard
  spillway dashboard -config config.yaml -interval 5s`, func(cmd *cobra.Command, args []string) int {
		return dashboardCommand(ctx, legacyArgs(cmd, originalArgs), stdin, stdout, stderr, lookupEnv)
	}, func(flags *pflag.FlagSet) {
		flags.String("config", defaultConfigPath, "path to the config file holding the dashboard key")
		flags.String("endpoint", "http://127.0.0.1:8080", "running Spillway base URL")
		flags.Duration("interval", 2*time.Second, "refresh interval (1s-1m)")
	})
	addLegacyCommand(root, exitCode, "catalog", "suggest model declarations and pricing from configured providers", `  spillway catalog -config config.yaml`, func(cmd *cobra.Command, args []string) int {
		return catalogCommand(ctx, legacyArgs(cmd, originalArgs), stdout, stderr, lookupEnv)
	}, func(flags *pflag.FlagSet) {
		flags.String("config", defaultConfigPath, "path to the config file")
	})
	addLegacyCommand(root, exitCode, "init", "create a zero-spend config and local key", `  spillway init
  spillway init -providers groq,gemini -yes`, func(cmd *cobra.Command, args []string) int {
		return initCommand(ctx, legacyArgs(cmd, originalArgs), stdin, stdout, stderr, lookupEnv)
	}, func(flags *pflag.FlagSet) {
		flags.String("config", defaultConfigPath, "path for the new config file")
		flags.String("providers", "", "comma-separated providers to configure")
		flags.String("attest-free-tier", "", "comma-separated providers attested as zero-spend")
		flags.Bool("store", false, "enable the local usage store and dashboard key")
		flags.Bool("yes", false, "accept defaults and run without prompts")
	})
	addLegacyCommand(root, exitCode, "eval", "run a paired routing evaluation (experimental)", `  spillway eval -dataset prompts.json -arms current=auto -out results`, func(cmd *cobra.Command, args []string) int {
		return evalCommand(ctx, legacyArgs(cmd, originalArgs), stdout, stderr, lookupEnv)
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
	})
	addLegacyCommand(root, exitCode, "eval-score", "summarize a completed blinded evaluation score sheet (experimental)", `  spillway eval-score -run results`, func(cmd *cobra.Command, args []string) int {
		return evalScoreCommand(ctx, legacyArgs(cmd, originalArgs), stdout, stderr)
	}, func(flags *pflag.FlagSet) {
		flags.String("run", "", "evaluation artifact directory")
	})
	addLegacyCommand(root, exitCode, "eval-report", "compare run artifacts and rater sheets against ADR 0014 gates (experimental)", `  spillway eval-report -run results -baseline current`, func(cmd *cobra.Command, args []string) int {
		return evalReportCommand(ctx, legacyArgs(cmd, originalArgs), stdout, stderr)
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
		if _, err := fmt.Fprintln(stdout, strings.Join(lines, "\n")); err != nil {
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

func addManagementCommands(ctx context.Context, root *cobra.Command, options Options) {
	addProviderCommands(ctx, root, options)
	addModelCommands(ctx, root, options)
	addTierCommands(root, options)
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
	command := &cobra.Command{
		Use:     name,
		Short:   short,
		Example: example,
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
