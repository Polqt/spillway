package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/cli"
)

func TestMain(m *testing.M) {
	if target := os.Getenv("SPILLWAY_TEST_EDITOR_TARGET"); target != "" {
		if len(os.Args) != 2 || os.Args[1] != target {
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestRun_ConfigListFindsNamedConfigsWithoutPrintingContents(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	for name, contents := range map[string]string{
		"jepoy":        "server: {}\nproviders: {}\napi_key: sk-test-private-value\n",
		"pol.yaml":     "server: {}\nproviders: {}\n",
		"settings.yml": "theme: dark\n",
		"broken.yaml":  "server: [\nproviders: {}\n",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(directory, "folder.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "folder.yaml", "nested.yaml"), []byte("server: {}\nproviders: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{"config", "list"}, nil, &stdout, &stderr, noEnv)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %q", code, stderr.String())
	}
	if got, want := stdout.String(), "jepoy\npol.yaml\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if strings.Contains(stdout.String()+stderr.String(), "sk-test-private-value") {
		t.Error("config list printed config contents")
	}
}

func TestRun_ConfigListExplainsEmptyDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	var stdout, stderr bytes.Buffer

	code := cli.Run(context.Background(), []string{"config", "list"}, nil, &stdout, &stderr, noEnv)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No Spillway config files found") {
		t.Errorf("stdout = %q, want empty-directory guidance", stdout.String())
	}
}

func TestRun_ConfigHelpListsCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := cli.Run(context.Background(), []string{"config", "--help"}, nil, &stdout, &stderr, noEnv)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %q", code, stderr.String())
	}
	for _, want := range []string{"list", "open", "Config files"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("config help missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRun_ConfigOpenReportsMissingPath(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	var stdout, stderr bytes.Buffer

	code := cli.Run(context.Background(), []string{"config", "open", "-config", "missing.yaml"}, nil, &stdout, &stderr, noEnv)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "missing.yaml") || !strings.Contains(stderr.String(), "spillway init -config") {
		t.Errorf("stderr = %q, want missing-path guidance", stderr.String())
	}
}

func TestRun_ConfigOpenReportsUnavailableEditor(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	path := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(path, []byte("server: {}\nproviders: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) (string, bool) {
		if name == "VISUAL" {
			return filepath.Join(directory, "missing-editor"), true
		}
		return "", false
	}
	var stdout, stderr bytes.Buffer

	code := cli.Run(context.Background(), []string{"config", "open"}, nil, &stdout, &stderr, lookup)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "editor") || strings.Contains(stderr.String(), "server:") {
		t.Errorf("stderr = %q, want editor guidance without config contents", stderr.String())
	}
}

func TestRun_ConfigOpenLaunchesSelectedEditorWithConfigPath(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	path := filepath.Join(directory, "named-config")
	if err := os.WriteFile(path, []byte("server: {}\nproviders: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPILLWAY_TEST_EDITOR_TARGET", path)
	lookup := func(name string) (string, bool) {
		if name == "VISUAL" {
			return executable, true
		}
		return "", false
	}
	var stdout, stderr bytes.Buffer

	code := cli.Run(context.Background(), []string{"config", "open", "-config", path}, nil, &stdout, &stderr, lookup)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %q", code, stderr.String())
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("editor output leaked to CLI streams: stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}
