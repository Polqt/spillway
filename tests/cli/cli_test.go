package cli_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Polqt/spillway/apps/spillway/web"
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

func TestVersion_VerboseShowsExecutableAndAssets(t *testing.T) {
	t.Parallel()
	lookup := func(name string) (string, bool) {
		return "sk-test-" + name + "-canary", true
	}
	var plain strings.Builder
	if code := cli.Run(t.Context(), []string{"version"}, nil, &plain, io.Discard, lookup); code != 0 {
		t.Fatalf("version exit code = %d", code)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"-verbose", "--verbose"} {
		var stdout, stderr strings.Builder
		if code := cli.Run(t.Context(), []string{"version", flag}, nil, &stdout, &stderr, lookup); code != 0 {
			t.Fatalf("version %s exit code = %d; stderr: %s", flag, code, stderr.String())
		}
		lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
		if lines[0]+"\n" != plain.String() {
			t.Errorf("version %s first line = %q, want the plain version line %q", flag, lines[0], plain.String())
		}
		fields := map[string]string{}
		for _, line := range lines[1:] {
			key, value, ok := strings.Cut(line, ": ")
			if !ok {
				t.Errorf("version %s line %q is not key: value", flag, line)
				continue
			}
			fields[key] = value
		}
		if fields["executable"] != executable {
			t.Errorf("executable = %q, want %q", fields["executable"], executable)
		}
		if !strings.HasPrefix(fields["module"], "github.com/Polqt/spillway ") {
			t.Errorf("module = %q, want the spillway module path and version", fields["module"])
		}
		if fields["dashboard_assets"] != web.Assets() {
			t.Errorf("dashboard_assets = %q, want %q", fields["dashboard_assets"], web.Assets())
		}
		localCanary, _ := lookup("LOCAL_KEY")
		providerCanary, _ := lookup("PROVIDER_KEY")
		for _, secret := range []string{localCanary, providerCanary} {
			if strings.Contains(stdout.String(), secret) {
				t.Errorf("version output contains key canary %q", secret)
			}
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

func TestRun_RootHelpShowsQuickPathAndCommandGroups(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := cli.Run(t.Context(), nil, nil, &stdout, &stderr, noEnv); code != 2 {
		t.Fatalf("bare command exit code = %d, want 2; stderr: %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "spillway init -yes -providers mock -store") {
		t.Errorf("bare command output = %q, want offline mock quick path", stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := cli.Run(t.Context(), []string{"--help"}, nil, &stdout, &stderr, noEnv); code != 0 {
		t.Fatalf("help exit code = %d, want 0; stderr: %q", code, stderr.String())
	}
	help := stdout.String()
	for _, want := range []string{
		"Quick start:",
		"spillway init -yes -providers mock -store",
		"Get started",
		"Tools",
		"Experimental",
		"completion",
		"help",
		"Flags accept one or two dashes: -config or --config.",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("root help = %q, want %q", help, want)
		}
	}
	for _, want := range []string{"Get started\n  init", "Tools\n  catalog", "Experimental\n  eval"} {
		if !strings.Contains(help, want) {
			t.Errorf("root help = %q, want grouped commands at %q", help, want)
		}
	}
	for _, want := range []string{"print the version", "help about any command", "generate shell completion"} {
		if !strings.Contains(help, want) {
			t.Errorf("root help = %q, want lowercase description %q", help, want)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestRun_CommandHelp(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"serve", "--help"}, nil, &stdout, &stderr, noEnv)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %q", code, stderr.String())
	}
	for _, want := range []string{"usage: spillway serve", "-config", "OpenAI-compatible API", "Examples:"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("help = %q, want %q", stdout.String(), want)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestRun_UnknownCommandSuggestsValidCommand(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"ser"}, nil, &stdout, &stderr, noEnv)

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown command "ser" for "spillway"`) || !strings.Contains(stderr.String(), "Did you mean this?") || !strings.Contains(stderr.String(), "serve") {
		t.Errorf("stderr = %q, want a command suggestion", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Run 'spillway --help' to list commands.") || strings.Contains(stderr.String(), "commands:") {
		t.Errorf("stderr = %q, want a one-line help pointer without full usage", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}

func TestRun_GeneratesBashCompletion(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"completion", "bash"}, nil, &stdout, &stderr, noEnv)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "__start_spillway") {
		t.Errorf("completion output does not contain bash completion function: %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestRun_ParseErrorUsesUsageExitCode(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"serve", "-config"}, nil, &stdout, &stderr, noEnv)

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "flag needs an argument") {
		t.Errorf("stderr = %q, want parse diagnostic", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}

func TestRun_DoubleDashKeepsFollowingFlagLikeArgumentPositional(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, "server:\n  api_keys: [sk-test-double-dash]\nproviders:\n  mock: {}\n")
	var stdout, stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"check", "--", "-config=" + path}, nil, &stdout, &stderr, noEnv)

	if code != 2 {
		t.Errorf("exit code = %d, want 2; stderr: %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "unexpected argument") {
		t.Errorf("stderr = %q, want unexpected-argument diagnostic", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}

func TestRun_RedirectedHelpHasNoANSI(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "")
	var stdout, stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"--help"}, nil, &stdout, &stderr, noEnv)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if strings.Contains(stdout.String()+stderr.String(), "\x1b[") {
		t.Errorf("redirected help contains ANSI escapes: %q", stdout.String())
	}
}

func TestRun_RedirectedStatusHasNoANSI(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "")
	path := writeConfig(t, "server:\n  api_keys: [sk-test-redirected-status]\nproviders:\n  mock: {}\n")
	var stdout, stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"check", "-config", path}, nil, &stdout, &stderr, noEnv)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "config OK") {
		t.Errorf("stderr = %q, want status output", stderr.String())
	}
	if strings.Contains(stderr.String(), "\x1b[") {
		t.Errorf("redirected status contains ANSI escapes: %q", stderr.String())
	}
}

func TestRun_HelpRespectsNoColor(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CLICOLOR_FORCE", "1")
	t.Setenv("NO_COLOR", "1")
	var stdout, stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"--help"}, nil, &stdout, &stderr, noEnv)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, colorCode := range []string{"\x1b[30m", "\x1b[31m", "\x1b[32m", "\x1b[33m", "\x1b[34m", "\x1b[35m", "\x1b[36m", "\x1b[37m", "\x1b[38;"} {
		if strings.Contains(stdout.String(), colorCode) {
			t.Errorf("help contains color escape %q: %q", colorCode, stdout.String())
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
			if tt.name == "no args" && !strings.Contains(stderr.String(), "usage: spillway <command>") {
				t.Errorf("stderr = %q, want usage text", stderr.String())
			}
			if tt.name == "unknown command" && (!strings.Contains(stderr.String(), "Run 'spillway --help' to list commands.") || strings.Contains(stderr.String(), "usage: spillway <command>")) {
				t.Errorf("stderr = %q, want a concise help pointer", stderr.String())
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
	if !strings.Contains(stderr.String(), "config OK") || !strings.Contains(stderr.String(), "spillway serve -config "+path) {
		t.Errorf("stderr = %q, want config OK and serve next step", stderr.String())
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
