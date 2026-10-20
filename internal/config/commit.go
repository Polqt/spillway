package config

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

// ErrConcurrentEdit reports that the config changed after it was read.
var ErrConcurrentEdit = errors.New("config changed since it was read")

// CommitOptions controls backup retention and timestamps.
type CommitOptions struct {
	Now         func() time.Time
	KeepBackups int
}

// CommitResult reports where the previous config was saved.
type CommitResult struct {
	Path, BackupPath string
	ModeTightened    bool
}

// Commit backs up the current file and atomically replaces it with c.
func (d *Document) Commit(ctx context.Context, c Change, options CommitOptions) (CommitResult, error) {
	if err := ctx.Err(); err != nil {
		return CommitResult{}, err
	}
	if c.beforePath != d.path || c.beforeDigest != d.digest {
		return CommitResult{}, errors.New("config: change does not belong to this document")
	}
	file, err := os.Open(d.path) // #nosec G304 -- path came from the operator's config argument.
	if err != nil {
		return CommitResult{}, explainWriteError(d.path, err)
	}
	current, readErr := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return CommitResult{}, explainWriteError(d.path, readErr)
	}
	if closeErr != nil {
		return CommitResult{}, explainWriteError(d.path, closeErr)
	}
	if len(current) > maxFileBytes {
		return CommitResult{}, fmt.Errorf("%w; rerun the command", ErrConcurrentEdit)
	}
	if sha256.Sum256(current) != d.digest {
		return CommitResult{}, fmt.Errorf("%w; rerun the command", ErrConcurrentEdit)
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.KeepBackups <= 0 {
		options.KeepBackups = 5
	}
	dir := filepath.Dir(d.path)
	base := filepath.Base(d.path)
	backup := d.path + "." + options.Now().UTC().Format("20060102T150405Z") + ".bak"
	if err := writeBackup(backup, d.data); err != nil {
		return CommitResult{}, explainWriteError(d.path, err)
	}
	if err := copySecurityDescriptor(d.path, backup); err != nil {
		_ = os.Remove(backup)
		return CommitResult{}, fmt.Errorf("config: preserve backup permissions for %s: %w", d.path, err)
	}
	temp, err := writeTemp(dir, base, c.after)
	if err != nil {
		return CommitResult{}, explainWriteError(d.path, err)
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(temp)
		}
	}()
	if err := ctx.Err(); err != nil {
		return CommitResult{}, err
	}
	if err := replaceFile(ctx, temp, d.path); err != nil {
		return CommitResult{}, explainWriteError(d.path, err)
	}
	removeTemp = false
	if err := syncDirectory(dir); err != nil {
		return CommitResult{}, fmt.Errorf("config: sync directory %s after replacement: %w", dir, err)
	}
	if err := pruneBackups(d.path, options.KeepBackups); err != nil {
		return CommitResult{}, fmt.Errorf("config: prune backups for %s: %w", d.path, err)
	}
	return CommitResult{Path: d.path, BackupPath: backup, ModeTightened: runtime.GOOS != "windows" && d.mode.Perm()&0o077 != 0}, nil
}

func writeBackup(path string, data []byte) error {
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
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write backup: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync backup: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close backup: %w", err)
	}
	complete = true
	return nil
}

func writeTemp(dir, base string, data []byte) (string, error) {
	file, err := os.CreateTemp(dir, "."+base+".spillway-*")
	if err != nil {
		return "", fmt.Errorf("create temporary config: %w", err)
	}
	path := file.Name()
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", fmt.Errorf("protect temporary config: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return "", fmt.Errorf("write temporary config: %w", err)
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("sync temporary config: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close temporary config: %w", err)
	}
	complete = true
	return path, nil
}

func pruneBackups(path string, keep int) error {
	dir, base := filepath.Dir(path), filepath.Base(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	prefix := base + "."
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".bak") {
			continue
		}
		timestamp := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".bak")
		if len(timestamp) != 16 || timestamp[8] != 'T' || timestamp[15] != 'Z' || !allDigits(timestamp[:8]+timestamp[9:15]) {
			continue
		}
		names = append(names, name)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, name := range names[min(keep, len(names)):] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func allDigits(value string) bool {
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func explainWriteError(path string, err error) error {
	if errors.Is(err, syscall.EROFS) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EBUSY) {
		return fmt.Errorf("config: %s is read-only or mounted into a container; edit it on the host with the host's spillway: %w", path, err)
	}
	return fmt.Errorf("config: write %s: %w", path, err)
}
