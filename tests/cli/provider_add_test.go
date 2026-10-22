package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/cli"
)

func TestProviderAdd_AddsGeminiToGroqConfig(t *testing.T) {
	t.Parallel()
	path, before := providerAddFixture(t, "groq")
	localKeysBefore := configSection(before, "  api_keys:", "providers:")
	providerSecret := "AIza" + "SyTest" + "ProviderCanaryValue"
	lookup := func(name string) (string, bool) {
		if name == "GROQ_API_KEY" || name == "GEMINI_API_KEY" {
			return providerSecret, true
		}
		return "", false
	}
	var stdout, stderr bytes.Buffer
	args := []string{"provider", "add", "gemini", "--attest-free-tier", "--tier", "free", "--config", path}
	if code := cli.Run(t.Context(), args, nil, &stdout, &stderr, lookup); code != 0 {
		t.Fatalf("exit = %d; stdout = %q; stderr = %q", code, stdout.String(), stderr.String())
	}
	// #nosec G304 -- path is returned by providerAddFixture and remains inside t.TempDir.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := configSection(after, "  api_keys:", "providers:"); got != localKeysBefore {
		t.Errorf("server.api_keys changed:\nbefore %q\nafter  %q", localKeysBefore, got)
	}
	if !strings.Contains(string(after), `api_key: "${GEMINI_API_KEY}"`) || !strings.Contains(string(after), "gemini/gemini-2.5-flash") {
		t.Errorf("updated config lacks Gemini reference and model: %s", after)
	}
	var checkOut, checkErr bytes.Buffer
	if code := cli.Run(t.Context(), []string{"check", "-config", path}, nil, &checkOut, &checkErr, lookup); code != 0 {
		t.Fatalf("check exit = %d; stderr = %q", code, checkErr.String())
	}
	backups, err := filepath.Glob(path + ".*.bak")
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups = %v; err = %v", backups, err)
	}
	backup, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	allOutput := string(after) + string(backup) + stdout.String() + stderr.String() + checkOut.String() + checkErr.String()
	if strings.Contains(allOutput, providerSecret) {
		t.Fatal("provider key value was disclosed")
	}
}

func TestProviderAdd_SecondInstanceRefusedWithGuidance(t *testing.T) {
	t.Parallel()
	path, before := providerAddFixture(t, "groq")
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"provider", "add", "groq", "--key-env", "GROQ_API_KEY_2", "--config", path}, nil, &stdout, &stderr, noEnv)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr = %q", code, stderr.String())
	}
	for _, want := range []string{"groq is already configured", "line", "never rotates provider instances", "--name <safe-alias>", "different provider service"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q lacks %q", stderr.String(), want)
		}
	}
	// #nosec G304 -- path is returned by providerAddFixture and remains inside t.TempDir.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("refused second provider instance changed the config")
	}
}

func TestProviderAdd_AddsNamedAuthorizedInstanceWithoutChangingRoutes(t *testing.T) {
	t.Parallel()
	path, before := providerAddFixture(t, "groq")
	var stdout, stderr bytes.Buffer
	lookup := func(name string) (string, bool) {
		return "sk-test-provider-add-secret", name == "GEMINI_RESEARCH_KEY" || name == "GROQ_API_KEY"
	}
	args := []string{"provider", "add", "gemini", "--name", "gemini-research", "--key-env", "GEMINI_RESEARCH_KEY", "--quota-domain", "project=research-team", "--yes", "--config", path}
	if code := cli.Run(t.Context(), args, nil, &stdout, &stderr, lookup); code != 0 {
		t.Fatalf("exit = %d; stderr = %q; stdout = %q", code, stderr.String(), stdout.String())
	}
	// #nosec G304 -- path is returned by providerAddFixture and remains inside t.TempDir.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "gemini-research:\n") || !strings.Contains(string(after), "type: gemini") || !strings.Contains(string(after), `api_key: "${GEMINI_RESEARCH_KEY}"`) || !strings.Contains(string(after), "project: research-team") {
		t.Fatalf("named instance was not written using an env reference: %s", after)
	}
	if configSection(after, "tiers:", "router:") != configSection(before, "tiers:", "router:") || !strings.Contains(stdout.String(), "restart spillway serve") {
		t.Fatalf("legacy route/config changed or restart guidance missing; output=%q", stdout.String())
	}
	if strings.Contains(stdout.String()+stderr.String()+string(after), "sk-test-provider-add-secret") {
		t.Fatal("provider key value was disclosed")
	}
	if strings.Contains(stdout.String()+stderr.String(), "research-team") {
		t.Fatal("quota-domain alias was disclosed in the preview or diagnostics")
	}
}

func TestProviderAdd_WritesOnlyEnvReference(t *testing.T) {
	t.Parallel()
	path, _ := providerAddFixture(t, "mock")
	const secret = "gsk_test-provider-add-canary"
	lookup := func(name string) (string, bool) { return secret, name == "GROQ_API_KEY" }
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"provider", "add", "groq", "--yes", "--config", path}, nil, &stdout, &stderr, lookup)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	// #nosec G304 -- path is returned by providerAddFixture and remains inside t.TempDir.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `api_key: "${GROQ_API_KEY}"`) {
		t.Errorf("config = %q, want environment reference", data)
	}
	if strings.Contains(string(data)+stdout.String()+stderr.String(), secret) {
		t.Fatal("provider key value was disclosed")
	}
}

func TestProviderAdd_KeyLikeKeyEnvRefusedWithoutEcho(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"sk-test-provider-key-canary", "gsk_test-key-canary", "AIzaSyCanary", strings.Repeat("K", 65)} {
		path, before := providerAddFixture(t, "mock")
		var stdout, stderr bytes.Buffer
		code := cli.Run(t.Context(), []string{"provider", "add", "gemini", "--key-env", value, "--config", path}, nil, &stdout, &stderr, noEnv)
		if code != 1 {
			t.Errorf("value case exit = %d, want 1", code)
		}
		if strings.Contains(stdout.String()+stderr.String(), value) {
			t.Errorf("output echoed rejected value %q", value)
		}
		// #nosec G304 -- path is returned by providerAddFixture and remains inside t.TempDir.
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Errorf("rejected value changed config")
		}
	}
}

func TestProviderAdd_TierPlacementFollowsADR0005(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, provider, flags, model string
		wantModel, wantWarning       bool
	}{
		{"gemini requires attestation", "gemini", "", "gemini/gemini-2.5-flash", false, true},
		{"groq requires attestation", "groq", "", "groq/llama-3.3-70b-versatile", false, true},
		{"openrouter free preset", "openrouter", "--key-env OPENROUTER_API_KEY", "openrouter/meta-llama/llama-3.3-70b-instruct:free", true, false},
		{"ollama", "ollama", "", "ollama/llama3.2:3b", true, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path, _ := providerAddFixture(t, "mock")
			var stdout, stderr bytes.Buffer
			args := []string{"provider", "add", test.provider, "--tier", "free", "--yes", "--config", path}
			if test.flags != "" {
				args = append(args, strings.Fields(test.flags)...)
			}
			lookup := func(string) (string, bool) { return "test-key", true }
			if code := cli.Run(t.Context(), args, nil, &stdout, &stderr, lookup); code != 0 {
				t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
			}
			// #nosec G304 -- path is returned by providerAddFixture and remains inside t.TempDir.
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			contains := strings.Contains(string(data), test.model)
			if contains != test.wantModel {
				t.Errorf("model present = %t, want %t", contains, test.wantModel)
			}
			if strings.Contains(stdout.String(), "was not added") != test.wantWarning {
				t.Errorf("warning output = %q", stdout.String())
			}
		})
	}
	path, _ := providerAddFixture(t, "groq")
	var stdout, stderr bytes.Buffer
	args := []string{"provider", "add", "mock", "--tier", "free", "--yes", "--config", path}
	if code := cli.Run(t.Context(), args, nil, &stdout, &stderr, noEnv); code != 0 {
		t.Fatalf("mock exit = %d; stderr = %q", code, stderr.String())
	}
	// #nosec G304 -- path is returned by providerAddFixture and remains inside t.TempDir.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "mock/echo") {
		t.Error("mock suggested model was not added")
	}
}

func TestProviderAdd_InteractiveChoicesMatchFlags(t *testing.T) {
	t.Parallel()
	interactivePath, _ := providerAddFixture(t, "groq")
	flagsPath, _ := providerAddFixture(t, "groq")
	var interactiveOut, interactiveErr bytes.Buffer
	options := cli.Options{LookupEnv: noEnv, StdinTerminal: true, StdoutTerminal: true}
	answers := strings.NewReader("1\nGEMINI_API_KEY\ny\ny\ny\n")
	code := cli.RunWithOptions(t.Context(), []string{"provider", "add", "-config", interactivePath}, answers, &interactiveOut, &interactiveErr, options)
	if code != 0 {
		t.Fatalf("interactive exit = %d; stdout = %q; stderr = %q", code, interactiveOut.String(), interactiveErr.String())
	}
	var flagsOut, flagsErr bytes.Buffer
	args := []string{"provider", "add", "gemini", "--key-env", "GEMINI_API_KEY", "--attest-free-tier", "--tier", "free", "--config", flagsPath}
	if code := cli.Run(t.Context(), args, nil, &flagsOut, &flagsErr, noEnv); code != 0 {
		t.Fatalf("flags exit = %d; stderr = %q", code, flagsErr.String())
	}
	// #nosec G304 -- path is returned by providerAddFixture and remains inside t.TempDir.
	interactiveData, err := os.ReadFile(interactivePath)
	if err != nil {
		t.Fatal(err)
	}
	// #nosec G304 -- path is returned by providerAddFixture and remains inside t.TempDir.
	flagsData, err := os.ReadFile(flagsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(interactiveData, flagsData) {
		t.Errorf("interactive and flag configs differ\ninteractive:\n%s\nflags:\n%s", interactiveData, flagsData)
	}
}

func TestProviderAdd_YesWithoutNameIsUsageError(t *testing.T) {
	t.Parallel()
	path, before := providerAddFixture(t, "mock")
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"provider", "add", "--yes", "--config", path}, nil, &stdout, &stderr, noEnv)
	if code != 2 || !strings.Contains(stderr.String(), "--yes requires a provider name") {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	// #nosec G304 -- path is returned by providerAddFixture and remains inside t.TempDir.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("usage error changed config")
	}
}

func TestProviderAdd_DryRunWritesNothing(t *testing.T) {
	t.Parallel()
	path, before := providerAddFixture(t, "mock")
	var stdout, stderr bytes.Buffer
	args := []string{"provider", "add", "gemini", "--dry-run", "--yes", "--config", path}
	if code := cli.Run(t.Context(), args, nil, &stdout, &stderr, noEnv); code != 0 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	// #nosec G304 -- path is returned by providerAddFixture and remains inside t.TempDir.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("dry run changed config")
	}
	backups, _ := filepath.Glob(path + ".*.bak")
	if len(backups) != 0 {
		t.Errorf("dry run created backups: %v", backups)
	}
	if !strings.Contains(stdout.String(), "Dry run only") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestProviderAdd_FlowStyleConfigPrintsSnippet(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "flow.yaml")
	before := []byte("server:\n  listen: 127.0.0.1:8080\n  api_keys: [\"sk-test-local-flow\"]\nlogging:\n  level: info\nproviders: {groq: {api_key: \"${GROQ_API_KEY}\", free_tier: true}}\ntiers:\n  free:\n    models: [groq/llama-3.3-70b-versatile]\n")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	args := []string{"provider", "add", "gemini", "--yes", "--config", path}
	if code := cli.Run(t.Context(), args, nil, &stdout, &stderr, noEnv); code != 1 {
		t.Fatalf("exit = %d, want 1; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "add this by hand:") || !strings.Contains(stderr.String(), "gemini:") {
		t.Errorf("stderr lacks paste-ready snippet: %q", stderr.String())
	}
	// #nosec G304 -- path is created under t.TempDir in this test.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("flow-style refusal changed config")
	}
}

func TestProviderAdd_ReportsUnsetVariableAndRestart(t *testing.T) {
	t.Parallel()
	path, _ := providerAddFixture(t, "mock")
	var stdout, stderr bytes.Buffer
	args := []string{"provider", "add", "gemini", "--yes", "--config", path}
	if code := cli.Run(t.Context(), args, nil, &stdout, &stderr, noEnv); code != 0 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	for _, want := range []string{"GEMINI_API_KEY is not set in this terminal", "spillway doctor", "restart spillway serve -config"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout %q lacks %q", stdout.String(), want)
		}
	}
}

func TestProviderAdd_HelpMarksExperimental(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := cli.Run(t.Context(), []string{"provider", "add", "--help"}, nil, &stdout, &stderr, noEnv); code != 0 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "experimental") {
		t.Errorf("help = %q", stdout.String())
	}
}

func TestInit_ExistingConfigSuggestsProviderAdd(t *testing.T) {
	t.Parallel()
	path, before := providerAddFixture(t, "mock")
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"init", "-yes", "-providers", "mock", "-config", path}, nil, &stdout, &stderr, noEnv)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "spillway provider add") {
		t.Errorf("stderr = %q", stderr.String())
	}
	// #nosec G304 -- path is returned by providerAddFixture and remains inside t.TempDir.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("init overwrote an existing config")
	}
}

func providerAddFixture(t *testing.T, provider string) (string, []byte) {
	t.Helper()
	fixture := map[string]string{
		"groq": filepath.Join("..", "config", "testdata", "edit", "init-groq.yaml"),
	}[provider]
	var data []byte
	if provider == "mock" {
		data = []byte("server:\n  listen: 127.0.0.1:8080\n  api_keys:\n    - name: default\n      key: \"sk-test-provider-add-local\"\nlogging:\n  level: info\nproviders:\n  mock: {}\ntiers:\n  free:\n    models:\n      - mock/echo\nrouter:\n  auto:\n    - free\n")
	} else {
		var err error
		// #nosec G304 -- fixture is selected from the fixed map above.
		data, err = os.ReadFile(fixture)
		if err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	//nolint:gosec // Path is constructed under t.TempDir in providerAddFixture.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, data
}

func configSection(data []byte, start, end string) string {
	text := string(data)
	from := strings.Index(text, start)
	if from < 0 {
		return ""
	}
	to := strings.Index(text[from:], end)
	if to < 0 {
		return text[from:]
	}
	return text[from : from+to]
}
