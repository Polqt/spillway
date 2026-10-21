package cli_test

import (
	"bytes"
	"testing"

	"github.com/Polqt/spillway/internal/cli"
)

func TestRun_V1CommandSurfaceUnchanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		code int
	}{
		{"version", []string{"version"}, 0},
		{"version single dash", []string{"version", "-verbose"}, 0},
		{"version double dash", []string{"version", "--verbose"}, 0},
		{"version alias", []string{"-version"}, 0},
		{"init help", []string{"init", "--help"}, 0},
		{"serve help", []string{"serve", "--help"}, 0},
		{"check help", []string{"check", "--help"}, 0},
		{"catalog help", []string{"catalog", "--help"}, 0},
		{"eval help", []string{"eval", "--help"}, 0},
		{"eval score help", []string{"eval-score", "--help"}, 0},
		{"eval report help", []string{"eval-report", "--help"}, 0},
		{"unknown command usage", []string{"unknown-command"}, 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if code := cli.Run(t.Context(), test.args, nil, &stdout, &stderr, noEnv); code != test.code {
				t.Fatalf("exit = %d, want %d; stderr = %q", code, test.code, stderr.String())
			}
		})
	}
}
