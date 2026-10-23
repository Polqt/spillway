// Package cli implements the spillway command line: subcommand dispatch,
// and the wiring from config to providers and the HTTP server.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/Polqt/spillway/apps/spillway/web"
	"github.com/spf13/cobra"
)

// version is set at build time with
// -ldflags "-X github.com/Polqt/spillway/internal/cli.version=...".
var version = "dev"

const usage = "usage: spillway <command>\nRun 'spillway --help' for usage.\n"

// Run executes one CLI invocation and returns the process exit code:
// 0 on success, 1 on failure, 2 on a usage error.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	return RunWithOptions(ctx, args, stdin, stdout, stderr, Options{LookupEnv: lookupEnv})
}

// RunWithOptions executes the CLI with injectable environment and platform details.
func RunWithOptions(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, options Options) int {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	if len(args) > 0 && args[0] == "-version" {
		args = append([]string{"version"}, args[1:]...)
	}

	exitCode := 0
	options = options.withDefaults(stdin, stdout, stderr)
	interactive := options.interactive()
	root := newRootCommand(ctx, args, stdin, stdout, stderr, options.LookupEnv, interactive, &exitCode)
	addManagementCommands(ctx, root, options)
	root.SetArgs(normalizeLegacyFlags(root, args))
	if err := root.ExecuteContext(ctx); err != nil {
		var coded commandError
		if errors.As(err, &coded) {
			return coded.code
		}
		var failed commandFailure
		if errors.As(err, &failed) {
			report(stderr, "spillway: %v\n", failed.err)
			return failed.code
		}
		report(stderr, "spillway: %v\n", err)
		if _, _, findErr := root.Find(args); findErr != nil {
			_, _ = fmt.Fprintln(stderr, "Run 'spillway --help' to list commands.")
			return 2
		}
		_, _ = fmt.Fprint(stderr, root.UsageString())
		return 2
	}
	return exitCode
}

func tuiAllowed(lookupEnv func(string) (string, bool)) bool {
	if value, ok := lookupEnv("TERM"); ok && value == "dumb" {
		return false
	}
	if value, ok := lookupEnv("SPILLWAY_ACCESSIBLE"); ok && value == "1" {
		return false
	}
	return true
}

func normalizeLegacyFlags(root *cobra.Command, args []string) []string {
	command, _, err := root.Find(args)
	if err != nil || command == root {
		return args
	}
	normalized := slices.Clone(args)
	for i, arg := range normalized {
		if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
			continue
		}
		name := strings.TrimPrefix(arg, "-")
		if equal := strings.IndexByte(name, '='); equal >= 0 {
			name = name[:equal]
		}
		if command.Flags().Lookup(name) != nil || command.InheritedFlags().Lookup(name) != nil {
			normalized[i] = "-" + arg
		}
	}
	return normalized
}

// currentVersion is the -X version, else the go install module version, else dev.
func currentVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

func versionLine() string {
	fields := []string{"spillway", currentVersion()}
	if info, ok := debug.ReadBuildInfo(); ok {
		settings := make(map[string]string, len(info.Settings))
		for _, setting := range info.Settings {
			settings[setting.Key] = setting.Value
		}
		if revision := settings["vcs.revision"]; revision != "" {
			if len(revision) > 7 {
				revision = revision[:7]
			}
			fields = append(fields, "commit", revision)
		}
		if built, err := time.Parse(time.RFC3339, settings["vcs.time"]); err == nil {
			fields = append(fields, "built", built.Format(time.RFC3339))
		}
		if settings["vcs.modified"] == "true" {
			fields = append(fields, "[modified]")
		}
	}
	fields = append(fields, runtime.Version(), runtime.GOOS+"/"+runtime.GOARCH)
	return strings.Join(fields, " ")
}

// versionDetails returns one "key: value" line each for the executable, the
// module, its VCS settings and the dashboard assets, so a user can tell which
// copy of spillway is running.
func versionDetails() []string {
	executable, err := os.Executable()
	if err != nil {
		executable = "unknown"
	}
	lines := []string{"executable: " + executable}
	if info, ok := debug.ReadBuildInfo(); ok {
		moduleVersion := info.Main.Version
		if moduleVersion == "" {
			moduleVersion = "(devel)"
		}
		lines = append(lines, "module: "+info.Main.Path+" "+moduleVersion)
		for _, setting := range info.Settings {
			if strings.HasPrefix(setting.Key, "vcs.") {
				lines = append(lines, setting.Key+": "+setting.Value)
			}
		}
	}
	return append(lines, "dashboard_assets: "+web.Assets())
}

// report writes a diagnostic to stderr. A failed write is ignored because
// stderr is the last channel left to report it on; the exit code still
// signals the failure.
func report(stderr io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(stderr, format, a...)
}
