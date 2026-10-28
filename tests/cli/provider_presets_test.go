package cli_test

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/cli"
)

func TestRun_InitMockProviderIsNotSelectable(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	path := filepath.Join(t.TempDir(), "config.yaml")
	code := cli.Run(t.Context(), []string{"init", "-yes", "-providers", "mock", "-config", path}, strings.NewReader(""), &stdout, &stderr, noEnv)
	if code != 1 || !strings.Contains(stderr.String(), "unknown provider \"mock\"") {
		t.Fatalf("init = %d; stderr %q; want unknown provider error", code, stderr.String())
	}
}
