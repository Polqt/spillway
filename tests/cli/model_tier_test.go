package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/cli"
)

const modelTierConfig = `server:
  listen: 127.0.0.1:8080
  api_keys:
    - name: default
      key: sk-test-local-model-tier
providers:
  gemini:
    api_key: "${GEMINI_API_KEY}"
    free_tier: true
  groq:
    api_key: "${GROQ_API_KEY}"
    free_tier: true
tiers:
  free:
    models:
      - gemini/gemini-old-flash
      - groq/llama-3.3-70b-versatile
  paid:
    allow_spend: true
    models:
      - gemini/gemini-paid-model
models:
  gemini/gemini-old-flash:
    capabilities: [vision]
pricing:
  gemini/gemini-old-flash:
    input_usd_per_mtok: 0
    output_usd_per_mtok: 0
router:
  auto: [free, paid]
`

func TestModelListShowsConfiguredModelsWithoutProviderKeys(t *testing.T) {
	t.Parallel()
	path := writeModelTierConfig(t)
	var stdout, stderr bytes.Buffer
	if code := cli.Run(t.Context(), []string{"model", "list", "-config", path, "-provider", "gemini"}, nil, &stdout, &stderr, noEnv); code != 0 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	for _, want := range []string{"MODEL", "gemini/gemini-old-flash", "CONFIG ZERO-SPEND", "0 / 0"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output %q does not contain %q", stdout.String(), want)
		}
	}
	if strings.Contains(stdout.String(), "groq/") || strings.Contains(stdout.String(), "sk-test-local-model-tier") {
		t.Errorf("filtered output contains another provider or a secret: %q", stdout.String())
	}
}

func TestTierListShowsSpendPermissionAndAutoOrder(t *testing.T) {
	t.Parallel()
	path := writeModelTierConfig(t)
	var stdout, stderr bytes.Buffer
	if code := cli.Run(t.Context(), []string{"tier", "list", "-config", path}, nil, &stdout, &stderr, noEnv); code != 0 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	for _, want := range []string{"TIER", "ALLOW SPEND", "free\tno\t2\t1", "paid\tyes\t1\t2"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output %q does not contain %q", stdout.String(), want)
		}
	}
}

func TestModelReplaceWritesBackupAndKeepsLocalKeyPrivate(t *testing.T) {
	t.Parallel()
	path := writeModelTierConfig(t)
	var stdout, stderr bytes.Buffer
	args := []string{"model", "replace", "gemini/gemini-old-flash", "gemini/gemini-new-flash", "-config", path}
	if code := cli.Run(t.Context(), args, nil, &stdout, &stderr, noEnv); code != 0 {
		t.Fatalf("exit = %d; stdout = %q; stderr = %q", code, stdout.String(), stderr.String())
	}
	// #nosec G304 -- path is created under t.TempDir.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "gemini/gemini-old-flash") || strings.Count(string(after), "gemini/gemini-new-flash") != 3 {
		t.Errorf("updated config has incorrect model references:\n%s", after)
	}
	backups, err := filepath.Glob(path + ".*.bak")
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups = %v; error = %v", backups, err)
	}
	if strings.Contains(stdout.String()+stderr.String(), "sk-test-local-model-tier") {
		t.Fatal("command output disclosed the local key")
	}
}

func writeModelTierConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(modelTierConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
