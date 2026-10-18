package cli

import (
	"flag"
	"io"

	"charm.land/lipgloss/v2"
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
	explicitConfig := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicitConfig = true
		}
	})
	cfg, err := config.LoadWithEnv(*configPath, lookupEnv)
	if err != nil {
		reportConfigLoadError(stderr, *configPath, explicitConfig, err)
		return 1
	}
	if err := cfg.WriteRedacted(stdout); err != nil {
		report(stderr, "spillway: write redacted config: %v\n", err)
		return 1
	}
	status := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")).Render("config OK")
	if _, err := lipgloss.Fprintln(stderr, status); err != nil {
		return 1
	}
	return 0
}
