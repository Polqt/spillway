package cli_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/cli"
)

func noEnv(string) (string, bool) { return "", false }

func TestRun_Version(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"version"}, &stdout, &stderr, noEnv)

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

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("stdout closed") }

func TestRun_VersionWriteFails(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"version"}, failingWriter{}, &stderr, noEnv)

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

			code := cli.Run(t.Context(), tt.args, &stdout, &stderr, noEnv)

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
	path := writeConfig(t, "server:\n  api_keys: [\"${LOCAL_KEY}\"]\nproviders:\n  groq:\n    api_key: \"${GROQ_API_KEY}\"\n")
	lookup := func(name string) (string, bool) {
		values := map[string]string{"LOCAL_KEY": "sk-test-local-check", "GROQ_API_KEY": "sk-test-groq-check"}
		value, ok := values[name]
		return value, ok
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"check", "-config", path}, &stdout, &stderr, lookup)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "[REDACTED]") || !strings.Contains(stdout.String(), "key-1") {
		t.Errorf("stdout = %q, want redacted keys and key name", stdout.String())
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
	code := cli.Run(t.Context(), []string{"check", "-config", path}, &stdout, &stderr, lookup)
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

func TestRun_CheckInvalidNeverPrintsConfigToStdout(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, "server:\n  api_keys: [\"${MISSING_KEY}\"]\nproviders:\n  mock: {}\n")
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"check", "-config", path}, &stdout, &stderr, noEnv)
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
	code := cli.Run(t.Context(), []string{"check", "-config"}, &stdout, &stderr, noEnv)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}
