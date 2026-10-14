// Command spillway is a self-hosted, OpenAI-compatible LLM gateway.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: spillway <command>

commands:
  version   print the version and exit
`

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, os.LookupEnv))
}

// run executes one CLI invocation and returns the process exit code:
// 0 on success, 1 on failure, 2 on a usage error.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	if len(args) == 0 {
		report(stderr, "%s", usage)
		return 2
	}
	switch args[0] {
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
