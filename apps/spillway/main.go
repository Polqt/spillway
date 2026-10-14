// Command spillway is a self-hosted, OpenAI-compatible LLM gateway.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/Polqt/spillway/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.LookupEnv)
	stop()
	os.Exit(code)
}
