//go:build linux || darwin

package config_test

import (
	"os"
	"testing"

	"github.com/Polqt/spillway/internal/config"
)

func TestCommit_FileAndBackupOwnerOnly(t *testing.T) {
	t.Parallel()
	doc, change, _ := commitFixture(t)
	result, err := doc.Commit(t.Context(), change, config.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{result.Path, result.BackupPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", path, got)
		}
	}
	if !result.ModeTightened {
		t.Error("ModeTightened = false, want true for 0644 source")
	}
}
