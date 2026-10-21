package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/cli"
)

func TestInstallReplacesPATHExecutableAfterConfirmation(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	source := filepath.Join(t.TempDir(), executableName())
	target := filepath.Join(binDir, executableName())
	writeInstallFile(t, source, "new release", 0o750)
	writeInstallFile(t, target, "old release", 0o600)
	var stdout, stderr bytes.Buffer
	options := installTestOptions(source, binDir)
	options.StdinTerminal = true
	if code := cli.RunWithOptions(t.Context(), []string{"install"}, strings.NewReader("yes\n"), &stdout, &stderr, options); code != 0 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	// #nosec G304 -- target is created in this test's temporary directory.
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "new release" {
		t.Fatalf("installed contents = %q", contents)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o750 {
		t.Errorf("installed permissions = %o, want 750", info.Mode().Perm())
	}
	if !strings.Contains(stdout.String(), target) || !strings.Contains(stdout.String(), "Installed this version") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestInstallDeclineLeavesPATHExecutableUnchanged(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	source := filepath.Join(t.TempDir(), executableName())
	target := filepath.Join(binDir, executableName())
	writeInstallFile(t, source, "new release", 0o700)
	writeInstallFile(t, target, "old release", 0o600)
	var stdout, stderr bytes.Buffer
	options := installTestOptions(source, binDir)
	options.StdinTerminal = true
	if code := cli.RunWithOptions(t.Context(), []string{"install"}, strings.NewReader("n\n"), &stdout, &stderr, options); code != 0 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	// #nosec G304 -- target is created in this test's temporary directory.
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "old release" {
		t.Fatalf("declined install changed target to %q", contents)
	}
	if !strings.Contains(stdout.String(), "No changes made") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestInstallRequiresTerminal(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	source := filepath.Join(t.TempDir(), executableName())
	target := filepath.Join(binDir, executableName())
	writeInstallFile(t, source, "new release", 0o700)
	writeInstallFile(t, target, "old release", 0o600)
	var stdout, stderr bytes.Buffer
	if code := cli.RunWithOptions(t.Context(), []string{"install"}, strings.NewReader("yes\n"), &stdout, &stderr, installTestOptions(source, binDir)); code != 1 {
		t.Fatalf("exit = %d, want 1; stderr = %q", code, stderr.String())
	}
	// #nosec G304 -- target is created in this test's temporary directory.
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "old release" {
		t.Fatalf("non-interactive install changed target to %q", contents)
	}
}

func TestInstallWithoutPATHMatchMakesNoChanges(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	source := filepath.Join(t.TempDir(), executableName())
	writeInstallFile(t, source, "new release", 0o700)
	var stdout, stderr bytes.Buffer
	options := installTestOptions(source, binDir)
	options.StdinTerminal = true
	if code := cli.RunWithOptions(t.Context(), []string{"install"}, strings.NewReader("yes\n"), &stdout, &stderr, options); code != 0 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No Spillway executable was found on PATH") {
		t.Errorf("stdout = %q", stdout.String())
	}
	if entries, err := os.ReadDir(binDir); err != nil || len(entries) != 0 {
		t.Fatalf("PATH directory changed: entries=%v err=%v", entries, err)
	}
}

func TestInstallAlreadyAtPATHTargetMakesNoChanges(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	target := filepath.Join(binDir, executableName())
	writeInstallFile(t, target, "current release", 0o700)
	var stdout, stderr bytes.Buffer
	options := installTestOptions(target, binDir)
	options.StdinTerminal = true
	if code := cli.RunWithOptions(t.Context(), []string{"install"}, strings.NewReader("yes\n"), &stdout, &stderr, options); code != 0 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "already at") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func installTestOptions(source, path string) cli.Options {
	return cli.Options{
		GOOS:       runtime.GOOS,
		Executable: func() (string, error) { return source, nil },
		LookupEnv: func(name string) (string, bool) {
			if name == "PATH" {
				return path, true
			}
			return "", false
		},
	}
}

func executableName() string {
	if runtime.GOOS == "windows" {
		return "spillway.exe"
	}
	return "spillway"
}

func writeInstallFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
