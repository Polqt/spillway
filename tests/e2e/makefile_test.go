package e2e_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMakeBuildDoesNotExecuteGitVersion(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "tag-command-ran")
	version := "v1.1.0`touch '" + marker + "'`"
	command := exec.CommandContext(t.Context(), "make", "build", "VERSION="+version) //nolint:gosec // Test invokes Make with fixed targets and a generated fake version string.
	command.Dir = root
	_, _ = command.CombinedOutput()
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("version text ran as a shell command")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat marker: %v", err)
	}
}
