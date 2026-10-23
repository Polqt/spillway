package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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

func TestProviderListTUIFiltersAndInspectsWithoutRevealingKeys(t *testing.T) {
	path := writeInspectConfig(t, "https://example.invalid/v1")
	lookup := func(name string) (string, bool) {
		if name == "GROQ_CUSTOM_KEY" {
			return "gsk_test-provider-secret-canary", true
		}
		return "", false
	}
	code, stdout, stderr := runInteractiveCLI(t, []string{"provider", "list", "-config", path}, lookup, "provider status", "/groq\r\rq")
	if code != 0 {
		t.Fatalf("provider TUI exit = %d; stderr: %q", code, stderr)
	}
	for _, want := range []string{"groq", "GROQ_CUSTOM_KEY", "set in this terminal", "attested"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("provider TUI output does not contain %q", want)
		}
	}
	if strings.Contains(stdout+stderr, "gsk_test-provider-secret-canary") || strings.Contains(stdout+stderr, "sk-test-local-key-canary") {
		t.Fatal("provider TUI output revealed a credential")
	}
}

func TestProviderShowTUIKeepsCredentialsHidden(t *testing.T) {
	path := writeInspectConfig(t, "https://example.invalid/v1")
	lookup := func(name string) (string, bool) {
		if name == "GROQ_CUSTOM_KEY" {
			return "gsk_test-provider-secret-canary", true
		}
		return "", false
	}
	code, stdout, stderr := runInteractiveCLI(t, []string{"provider", "show", "groq", "-config", path}, lookup, "provider details", "\rq")
	if code != 0 {
		t.Fatalf("provider show TUI exit = %d; stderr: %q", code, stderr)
	}
	if !strings.Contains(stdout, "GROQ_CUSTOM_KEY") || strings.Contains(stdout+stderr, "gsk_test-provider-secret-canary") {
		t.Errorf("provider show TUI output is missing safe details or contains a credential: %q", stdout+stderr)
	}
}

func TestModelListTUIFiltersAndInspectsConfiguredModels(t *testing.T) {
	path := writeModelTierConfig(t)
	code, stdout, stderr := runInteractiveCLI(t, []string{"model", "list", "-config", path}, noEnv, "model catalog", "/gemini-old\r\rq")
	if code != 0 {
		t.Fatalf("model TUI exit = %d; stderr: %q", code, stderr)
	}
	for _, want := range []string{"gemini/gemini-old-flash", "Config zero-spend eligible: yes", "Tier: free"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("model TUI output does not contain %q", want)
		}
	}
	if strings.Contains(stdout, "sk-test-local-model-tier") {
		t.Fatal("model TUI output revealed the local key")
	}
}

func TestTierCommandTUIShowsModelOrder(t *testing.T) {
	path := writeModelTierConfig(t)
	code, stdout, stderr := runInteractiveCLI(t, []string{"tier", "list", "-config", path}, noEnv, "model tiers", "/free\r\rq")
	if code != 0 {
		t.Fatalf("tier TUI exit = %d; stderr: %q", code, stderr)
	}
	for _, want := range []string{"free", "Auto order: position 1", "gemini/gemini-old-flash", "groq/llama-3.3-70b-versatile"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("tier TUI output does not contain %q", want)
		}
	}
}

func TestConfigListTUIShowsPathsWithoutReadingContentsToScreen(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	configPath := filepath.Join(directory, "named-config.yaml")
	contents := "server: {}\nproviders: {}\nprivate_note: never-display-this\n"
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runInteractiveCLI(t, []string{"config", "list"}, noEnv, "config files", "/named-config\r\rq")
	if code != 0 {
		t.Fatalf("config TUI exit = %d; stderr: %q", code, stderr)
	}
	if !strings.Contains(stdout, "named-config.yaml") || !strings.Contains(stdout, "Path:") {
		t.Errorf("config TUI did not show config metadata: %q", stdout)
	}
	if strings.Contains(stdout+stderr, "never-display-this") {
		t.Fatal("config TUI output displayed config contents")
	}
}

func TestCommandGroupTUIOpensForBareModelCommand(t *testing.T) {
	path := writeModelTierConfig(t)
	t.Chdir(filepath.Dir(path))
	code, stdout, stderr := runInteractiveCLI(t, []string{"model"}, noEnv, "model catalog", "/gemini-old\r\rq")
	if code != 0 {
		t.Fatalf("model group TUI exit = %d; stderr: %q", code, stderr)
	}
	if !strings.Contains(stdout, "gemini/gemini-old-flash") {
		t.Errorf("model group TUI did not show configured models: %q", stdout)
	}
}

func TestCheckTUIShowsRedactedConfigWithoutExposingKeys(t *testing.T) {
	path := writeConfig(t, `server:
  listen: 127.0.0.1:8080
  api_keys:
    - name: default
      key: sk-test-local-check-tui-key-0123456789
providers:
  mock: {}
tiers:
  free:
    models: [mock/echo]
router:
  auto: [free]
`)
	code, stdout, stderr := runInteractiveCLI(t, []string{"check", "-config", path}, noEnv, "config check", "/validation\r\rq")
	if code != 0 {
		t.Fatalf("check TUI exit = %d; stderr: %q", code, stderr)
	}
	if !strings.Contains(stdout, "[REDACTED]") {
		t.Errorf("check TUI did not show the redacted config: %q", stdout)
	}
	if strings.Contains(stdout+stderr, "sk-test-local-check-tui-key-0123456789") {
		t.Fatal("check TUI output revealed a local key")
	}
}

func TestDoctorTUIShowsDiagnosticReport(t *testing.T) {
	path := writeDoctorConfig(t, "gsk_test-provider-key-long-enough-for-shape-check", "sk-test-local-doctor-key-0123456789ABCDEF", "")
	lookup := func(name string) (string, bool) {
		if name == "GROQ_API_KEY" {
			return "gsk_test-provider-key-long-enough-for-shape-check", true
		}
		return "", false
	}
	code, stdout, stderr := runInteractiveCLI(t, []string{"doctor", "-config", path, "--no-probe"}, lookup, "spillway doctor", "/report\r\rq")
	if code != 0 {
		t.Fatalf("doctor TUI exit = %d; stderr: %q", code, stderr)
	}
	for _, want := range []string{"Diagnostic report", "Config"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("doctor TUI output does not contain %q", want)
		}
	}
	if strings.Contains(stdout+stderr, "gsk_test-provider-key-long-enough-for-shape-check") || strings.Contains(stdout+stderr, "sk-test-local-doctor-key-0123456789ABCDEF") {
		t.Fatal("doctor TUI output revealed a credential")
	}
}
