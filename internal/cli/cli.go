// Package cli implements the spillway command line: subcommand dispatch,
// and the wiring from config to providers and the HTTP server.
package cli

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// version is set at build time with
// -ldflags "-X github.com/Polqt/spillway/internal/cli.version=...".
var version = "dev"

const usage = `usage: spillway <command>

commands:
  serve     serve the OpenAI-compatible API (flags: -config path, default config.yaml)
  check     validate config and print it with secrets redacted (flags: -config path)
  catalog   suggest model declarations and pricing from configured providers (flags: -config path)
  init      create a zero-spend config and local key (flags: -config, -providers, -attest-free-tier, -store, -yes)
  eval      run a paired routing evaluation against an OpenAI-compatible endpoint
  eval-score summarize a completed blinded evaluation score sheet
  version   print the version and exit
`

// Run executes one CLI invocation and returns the process exit code:
// 0 on success, 1 on failure, 2 on a usage error.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	if len(args) == 0 {
		report(stderr, "%s", usage)
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		_, err := fmt.Fprint(stdout, usage)
		if err != nil {
			report(stderr, "spillway: write help: %v\n", err)
			return 1
		}
		return 0
	case "serve":
		return serve(ctx, args[1:], stderr, lookupEnv)
	case "check":
		return check(args[1:], stdout, stderr, lookupEnv)
	case "catalog":
		return catalogCommand(ctx, args[1:], stdout, stderr, lookupEnv)
	case "init":
		return initCommand(ctx, args[1:], stdin, stdout, stderr, lookupEnv)
	case "eval":
		return evalCommand(ctx, args[1:], stdout, stderr, lookupEnv)
	case "eval-score":
		return evalScoreCommand(args[1:], stdout, stderr)
	case "version", "-version", "--version":
		_, err := fmt.Fprintln(stdout, versionLine())
		if err != nil {
			report(stderr, "spillway: write version: %v\n", err)
			return 1
		}
		return 0
	default:
		report(stderr, "spillway: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func versionLine() string {
	fields := []string{"spillway", version}
	if info, ok := debug.ReadBuildInfo(); ok {
		if version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			fields[1] = info.Main.Version
		}
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

// report writes a diagnostic to stderr. A failed write is ignored because
// stderr is the last channel left to report it on; the exit code still
// signals the failure.
func report(stderr io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(stderr, format, a...)
}
