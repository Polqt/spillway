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
	go func() {
		<-ctx.Done()
		stop()
	}()
	code := cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.LookupEnv)
	stop()
	os.Exit(code)
}
