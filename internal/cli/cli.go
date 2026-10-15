// Package cli implements the spillway command line: subcommand dispatch,
// and the wiring from config to providers and the HTTP server.
package cli

import (
	"context"
	"fmt"
	"io"
	"runtime"
)

// version is set at build time with
// -ldflags "-X github.com/Polqt/spillway/internal/cli.version=...".
var version = "dev"

const usage = `usage: spillway <command>

commands:
  serve     serve the OpenAI-compatible API (flags: -config path, default config.yaml)
  check     validate config and print it with secrets redacted (flags: -config path)
  catalog   suggest model declarations and pricing from configured providers (flags: -config path)
  eval      run a paired routing evaluation against an OpenAI-compatible endpoint
  eval-score summarize a completed blinded evaluation score sheet
  version   print the version and exit
`

// Run executes one CLI invocation and returns the process exit code:
// 0 on success, 1 on failure, 2 on a usage error.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	if len(args) == 0 {
		report(stderr, "%s", usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:], stderr, lookupEnv)
	case "check":
		return check(args[1:], stdout, stderr, lookupEnv)
	case "catalog":
		return catalogCommand(ctx, args[1:], stdout, stderr, lookupEnv)
	case "eval":
		return evalCommand(ctx, args[1:], stdout, stderr, lookupEnv)
	case "eval-score":
		return evalScoreCommand(args[1:], stdout, stderr)
	case "version":
		_, err := fmt.Fprintf(stdout, "spillway %s %s %s/%s\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
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

// report writes a diagnostic to stderr. A failed write is ignored because
// stderr is the last channel left to report it on; the exit code still
// signals the failure.
func report(stderr io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(stderr, format, a...)
}
