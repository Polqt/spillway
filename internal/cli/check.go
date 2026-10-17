package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/Polqt/spillway/internal/config"
)

func check(args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath, "path to the config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		report(stderr, "spillway: check: unexpected argument %q\n\n%s", fs.Arg(0), usage)
		return 2
	}
	cfg, err := config.LoadWithEnv(*configPath, lookupEnv)
	if err != nil {
		reportConfigLoadError(stderr, *configPath, err)
		return 1
	}
	if err := cfg.WriteRedacted(stdout); err != nil {
		report(stderr, "spillway: write redacted config: %v\n", err)
		return 1
	}
	if _, err := fmt.Fprintln(stderr, "config OK"); err != nil {
		return 1
	}
	return 0
}
