// Command spillway is a self-hosted, OpenAI-compatible LLM gateway.
package main

import (
	"context"
	"os"

	"github.com/Polqt/spillway/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, os.LookupEnv))
}
