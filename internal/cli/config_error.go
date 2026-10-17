package cli

import (
	"errors"
	"io"
	"os"
)

const defaultConfigPath = "config.yaml"

func reportConfigLoadError(stderr io.Writer, configPath string, err error) {
	if configPath == defaultConfigPath && errors.Is(err, os.ErrNotExist) {
		report(stderr, "spillway: %v\nRun `spillway init` to create a config file.\n", err)
		return
	}
	report(stderr, "spillway: %v\n", err)
}
