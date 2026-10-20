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
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}
