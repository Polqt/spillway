package cli

import (
	"errors"
	"io"
	"os"
	"runtime"

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
		report(stderr, "spillway: %v\n%s\n", err, environmentRecoveryCommand(missing.Variable))
		return
	}
	report(stderr, "spillway: %v\nCheck the config syntax and values with `spillway check -config %s`.\n", err, configPath)
}

func environmentRecoveryCommand(name string) string {
	if runtime.GOOS == "windows" {
		return "$env:" + name + " = [Net.NetworkCredential]::new('', (Read-Host -AsSecureString 'provider key')).Password"
	}
	return "read -rs " + name + " && export " + name
}
