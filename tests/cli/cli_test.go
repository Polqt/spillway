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
