package cli_test

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/cli"
)

func noEnv(string) (string, bool) { return "", false }

func TestRun_Version(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"version"}, nil, &stdout, &stderr, noEnv)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %q", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "spillway dev ") {
		t.Errorf("stdout = %q, want prefix %q", stdout.String(), "spillway dev ")
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestRun_VersionAliases(t *testing.T) {
	t.Parallel()
	var expected string
	for _, args := range [][]string{{"version"}, {"--version"}, {"-version"}} {
		var stdout, stderr bytes.Buffer
		code := cli.Run(t.Context(), args, nil, &stdout, &stderr, noEnv)
		if code != 0 {
			t.Errorf("args %q: exit code = %d, want 0; stderr: %q", args, code, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Errorf("args %q: stderr = %q, want empty", args, stderr.String())
		}
		if expected == "" {
			expected = stdout.String()
		} else if stdout.String() != expected {
			t.Errorf("args %q: stdout = %q, want %q", args, stdout.String(), expected)
		}
	}
}

func TestRun_HelpAliases(t *testing.T) {
	t.Parallel()
	var expected string
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}} {
		var stdout, stderr bytes.Buffer
		code := cli.Run(t.Context(), args, nil, &stdout, &stderr, noEnv)
		if code != 0 {
			t.Errorf("args %q: exit code = %d, want 0; stderr: %q", args, code, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Errorf("args %q: stderr = %q, want empty", args, stderr.String())
		}
		if !strings.Contains(stdout.String(), "usage: spillway <command>") {
			t.Errorf("args %q: stdout = %q, want usage", args, stdout.String())
		}
		if expected == "" {
			expected = stdout.String()
		} else if stdout.String() != expected {
			t.Errorf("args %q: stdout = %q, want %q", args, stdout.String(), expected)
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("stdout closed") }

func TestRun_VersionWriteFails(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"version"}, nil, failingWriter{}, &stderr, noEnv)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "spillway: write version: stdout closed") {
		t.Errorf("stderr = %q, want the write error", stderr.String())
	}
}

func TestRun_UsageError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{name: "no args", args: nil},
		{name: "unknown command", args: []string{"bogus"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer

			code := cli.Run(t.Context(), tt.args, nil, &stdout, &stderr, noEnv)

			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), "usage: spillway <command>") {
				t.Errorf("stderr = %q, want usage text", stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
		})
	}
}

func TestRun_CheckValidRedactsSecrets(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, "server:\n  shutdown_timeout: 15s\n  api_keys: [\"${LOCAL_KEY}\"]\nproviders:\n  groq:\n    api_key: \"${GROQ_API_KEY}\"\n")
	lookup := func(name string) (string, bool) {
		values := map[string]string{"LOCAL_KEY": "sk-test-local-check", "GROQ_API_KEY": "sk-test-groq-check"}
		value, ok := values[name]
		return value, ok
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"check", "-config", path}, nil, &stdout, &stderr, lookup)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "[REDACTED]") || !strings.Contains(stdout.String(), "key-1") {
		t.Errorf("stdout = %q, want redacted keys and key name", stdout.String())
	}
	if !strings.Contains(stdout.String(), "shutdown_timeout: 15s") {
		t.Errorf("check output = %q, want shutdown timeout", stdout.String())
	}
	if stderr.String() != "config OK\n" {
		t.Errorf("stderr = %q, want config OK", stderr.String())
	}
	for _, secret := range []string{"sk-test-local-check", "sk-test-groq-check"} {
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Errorf("check output contains secret %q", secret)
		}
	}
}

func TestRun_CheckPrintsTierAndFreeTierWithoutKeys(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, "server:\n  api_keys: [\"${LOCAL_KEY}\"]\nproviders:\n  groq:\n    api_key: \"${GROQ_API_KEY}\"\n    free_tier: true\ntiers:\n  cheap:\n    models: [groq/model]\n")
	lookup := func(name string) (string, bool) {
		values := map[string]string{"LOCAL_KEY": "sk-test-local-check-tier", "GROQ_API_KEY": "sk-test-groq-check-tier"}
		value, ok := values[name]
		return value, ok
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"check", "-config", path}, nil, &stdout, &stderr, lookup)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %q", code, stderr.String())
	}
	for _, want := range []string{"tiers:", "cheap:", "models:", "allow_spend: false", "free_tier: true", "[REDACTED]"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("check output = %q, want %q", stdout.String(), want)
		}
	}
	for _, secret := range []string{"sk-test-local-check-tier", "sk-test-groq-check-tier"} {
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Errorf("check output contains secret %q", secret)
		}
	}
}

func TestRun_CheckPrintsStoreConfig(t *testing.T) {
	t.Parallel()
	storePath := filepath.Join(t.TempDir(), "usage.db")
	path := writeConfig(t, "server:\n  api_keys: [sk-test-check-store]\nproviders:\n  mock: {}\nstore:\n  path: '"+storePath+"'\n  retention: 30d\n")
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"check", "-config", path}, nil, &stdout, &stderr, noEnv)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %q", code, stderr.String())
	}
	for _, want := range []string{"store:", storePath, "retention: 30d"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("check output = %q, want %q", stdout.String(), want)
		}
	}
}

func TestRun_CheckPrintsDashboardKeyFlagWithoutKey(t *testing.T) {
	path := writeConfig(t, "server:\n  api_keys: [{name: admin, key: sk-test-check-dashboard, dashboard: true}]\nproviders:\n  mock: {}\n")
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"check", "-config", path}, nil, &stdout, &stderr, noEnv)
	if code != 0 {
		t.Fatalf("check exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "dashboard: true") {
		t.Errorf("check output = %q, want dashboard flag", stdout.String())
	}
	if strings.Contains(stdout.String(), "sk-test-check-dashboard") {
		t.Errorf("check output exposed local key: %q", stdout.String())
	}
}

func TestRun_CheckInvalidNeverPrintsConfigToStdout(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, "server:\n  api_keys: [\"${MISSING_KEY}\"]\nproviders:\n  mock: {}\n")
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"check", "-config", path}, nil, &stdout, &stderr, noEnv)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if !strings.HasPrefix(stderr.String(), "spillway: ") || !strings.Contains(stderr.String(), "MISSING_KEY") {
		t.Errorf("stderr = %q, want prefixed variable diagnostic", stderr.String())
	}
}

func TestRun_CheckMissingFlagValue(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"check", "-config"}, nil, &stdout, &stderr, noEnv)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRun_CatalogUsesConfiguredProviderAndSkipsMock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.Header.Get("Authorization") != "Bearer sk-test-groq-catalog" {
			t.Errorf("catalog request = %s %s authorization=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"llama-test","context_window":32768}]}`))
	}))
	defer server.Close()
	path := writeConfig(t, "server:\n  api_keys: [sk-test-local-catalog]\nproviders:\n  groq:\n    api_key: ${GROQ_API_KEY}\n    base_url: "+server.URL+"\n    free_tier: true\n  mock: {}\ntiers:\n  free:\n    models: [groq/llama-test, mock/echo]\n")
	lookup := func(name string) (string, bool) {
		if name == "GROQ_API_KEY" {
			return "sk-test-groq-catalog", true
		}
		return "", false
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"catalog", "-config", path}, nil, &stdout, &stderr, lookup)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	for _, want := range []string{"models:", "context_window: 32768", "mock skipped"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout missing %q: %s", want, stdout.String())
		}
	}
	for _, secret := range []string{"sk-test-local-catalog", "sk-test-groq-catalog"} {
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Errorf("catalog leaked %q", secret)
		}
	}
}
