package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/Polqt/spillway/internal/config"
)

func keyCommand(args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	fs := flag.NewFlagSet("key", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath, "path to the config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		report(stderr, "usage: spillway key [-config path] <name>\n")
		return 2
	}

	keys, err := config.LoadLocalKeysWithEnv(*configPath, lookupEnv)
	if err != nil {
		report(stderr, "spillway: key: %v\n", err)
		return 1
	}
	name := fs.Arg(0)
	var value string
	found := false
	for _, key := range keys {
		if key.Name != name {
			continue
		}
		if found {
			report(stderr, "spillway: key: multiple local keys named %q\n", name)
			return 1
		}
		found = true
		value = key.Key.Reveal()
	}
	if !found || value == "" {
		report(stderr, "spillway: key: local key %q not found\n", name)
		return 1
	}
	if _, err := fmt.Fprintln(stdout, value); err != nil {
		report(stderr, "spillway: key: write output: %v\n", err)
		return 1
	}
	return 0
}
