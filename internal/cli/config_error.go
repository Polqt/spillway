package cli

import (
	"errors"
	"io"
	"os"
	"strings"
)

const defaultConfigPath = "config.yaml"

func reportConfigLoadError(stderr io.Writer, configPath string, explicit bool, err error) {
	if !explicit && configPath == defaultConfigPath && errors.Is(err, os.ErrNotExist) {
		report(stderr, "spillway: %v\nRun `spillway init` to create a config file.\n", err)
		return
	}
	if errors.Is(err, os.ErrNotExist) {
		report(stderr, "spillway: %v\nCheck that the -config path is correct, or run `spillway init -config %s` to create it.\n", err, configPath)
		return
	}
	if strings.Contains(err.Error(), "environment variable") {
		report(stderr, "spillway: %v\nSet the named environment variable(s), then rerun the command.\n", err)
		return
	}
	report(stderr, "spillway: %v\nCheck the config syntax and values with `spillway check -config %s`.\n", err, configPath)
}
