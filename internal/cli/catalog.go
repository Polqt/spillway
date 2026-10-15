package cli

import (
	"context"
	"flag"
	"io"
	"net/http"

	"github.com/Polqt/spillway/internal/catalog"
	"github.com/Polqt/spillway/internal/config"
)

func catalogCommand(ctx context.Context, args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	fs := flag.NewFlagSet("catalog", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "config.yaml", "path to the config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		report(stderr, "spillway: catalog: unexpected argument %q\n\n%s", fs.Arg(0), usage)
		return 2
	}
	cfg, err := config.LoadWithEnv(*configPath, lookupEnv)
	if err != nil {
		report(stderr, "spillway: %v\n", err)
		return 1
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if err := catalog.Discover(ctx, cfg, stdout, catalog.Options{HTTPClient: client}); err != nil {
		report(stderr, "spillway: catalog: %v\n", err)
		return 1
	}
	return 0
}
