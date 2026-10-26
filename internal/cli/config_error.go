package cli

import (
	"errors"
	"io"
	"os"
	"strings"

	"github.com/Polqt/spillway/internal/config"
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
	var missing *config.MissingEnvironmentError
	if errors.As(err, &missing) {
		if strings.HasPrefix(missing.Field, "providers.") && strings.HasSuffix(missing.Field, ".api_key") {
			name := strings.TrimSuffix(strings.TrimPrefix(missing.Field, "providers."), ".api_key")
			if config.ValidProviderInstanceName(name) {
				report(stderr, "spillway: providers.%s.api_key (line %d): provider key is not set.\nRun `spillway provider key set %s -config %s` to enter it securely.\n", name, missing.Line, name, configPath)
				return
			}
		}
		report(stderr, "spillway: %v\n%s\n", err, environmentRecoveryCommand(missing.Variable))
		return
	}
	report(stderr, "spillway: %v\nCheck the config syntax and values with `spillway check -config %s`.\n", err, configPath)
}
