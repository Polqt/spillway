//go:build !windows

package config

import (
	"context"
	"fmt"
	"os"
)

func replaceFile(_ context.Context, source, target string) error {
	if err := os.Rename(source, target); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path) // #nosec G304 -- the absolute config directory is opened only to sync after replacement.
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func copySecurityDescriptor(string, string) error { return nil }
