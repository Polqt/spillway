//go:build !windows

package config

import (
	"context"
	"fmt"
	"os"
)

func prepareBackup(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// #nosec G304 -- path is generated beside the already-opened config file.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create backup: %w", err)
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("protect backup: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write backup: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync backup: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close backup: %w", err)
	}
	complete = true
	return nil
}

func replaceFile(ctx context.Context, source, target, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(source, target); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

func syncDirectory(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, err := os.Open(path) // #nosec G304 -- the absolute config directory is opened only to sync after replacement.
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	if err := ctx.Err(); err != nil {
		return err
	}
	return directory.Sync()
}
