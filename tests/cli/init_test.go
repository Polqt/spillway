package cli_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/cli"
	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
)

func readInitFile(path string) ([]byte, error) {
	// #nosec G304 -- tests pass config paths created under t.TempDir.
	return os.ReadFile(path)
}

func writeInitFile(path string, data []byte) error {
	// #nosec G703 -- tests pass config paths created under t.TempDir.
	return os.WriteFile(path, data, 0o600)
}

type promptWriter struct {
	wrote chan struct{}
	want  string
}

func (w promptWriter) Write(data []byte) (int, error) {
	if bytes.Contains(data, []byte(w.want)) {
		select {
		case w.wrote <- struct{}{}:
		default:
		}
	}
	return len(data), nil
}

type closeSignalReader struct {
	*io.PipeReader
	closed chan struct{}
}

func (r closeSignalReader) Close() error {
	close(r.closed)
	return r.PipeReader.Close()
}

func TestRun_InitEveryProviderCombinationValidates(t *testing.T) {
	t.Parallel()
	presets := providers.Presets()
	for mask := 1; mask < 1<<len(presets); mask++ {
		var names []string
		for i, preset := range presets {
			if mask&(1<<i) != 0 {
				names = append(names, preset.Name)
			}
		}
		t.Run(strings.Join(names, ","), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.yaml")
			var stdout, stderr bytes.Buffer
			code := cli.Run(t.Context(), []string{"init", "-yes", "-providers", strings.Join(names, ","), "-config", path}, strings.NewReader(""), &stdout, &stderr, func(name string) (string, bool) {
				return "sk-test-value", true
			})
			if code != 0 {
				t.Fatalf("init exit = %d; stderr: %s", code, stderr.String())
			}
			for _, nextStep := range []string{"spillway check -config " + path, "spillway serve -config " + path, "http://127.0.0.1:8080/v1", "http://127.0.0.1:8080/dashboard"} {
				if !strings.Contains(stdout.String(), nextStep) {
					t.Errorf("init output = %q, want next step %q", stdout.String(), nextStep)
				}
			}
			data, err := readInitFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := config.ParseWithEnv(data, func(string) (string, bool) { return "sk-test-value", true }); err != nil {
				t.Fatalf("generated config does not validate: %v\n%s", err, data)
			}
		})
	}
}

func TestRun_InitInteractiveAnswers(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"init", "-config", path}, strings.NewReader("0\n0\nn\n"), &stdout, &stderr, noEnv)
	if code != 0 {
		t.Fatalf("init exit = %d; stderr: %s", code, stderr.String())
	}
	want := "server:\n  listen: 127.0.0.1:8080\n  api_keys:\n    - name: default\n      key: GENERATED_KEY\nproviders:\n  mock: {}\n# Suggested model IDs may be outdated; check current provider documentation.\ntiers:\n  free:\n    models:\n      - mock/echo\nrouter:\n  auto:\n    - free\n"
	got, err := readInitFile(path)
	if err != nil {
		t.Fatal(err)
	}
	keyPattern := regexp.MustCompile(`sk-spillway-[A-Za-z0-9_-]{43}`)
	key := keyPattern.Find(got)
	if len(key) == 0 {
		t.Fatalf("config has no generated local key: %s", got)
	}
	if normalized := strings.Replace(string(got), string(key), "GENERATED_KEY", 1); normalized != strings.Replace(want, "key: GENERATED_KEY", `key: "GENERATED_KEY"`, 1) {
		t.Errorf("config =\n%s\nwant =\n%s", normalized, want)
	}
}

func TestRun_InitPromptsForConfigAndSelectableModels(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "wizard.yaml")
	input := strings.Join([]string{path, "1", "2", "0", "y", "y", "2", "0", "n", ""}, "\n")
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"init"}, strings.NewReader(input), &stdout, &stderr, noEnv)
	if code != 0 {
		t.Fatalf("init = %d; stdout %q; stderr %q", code, stdout.String(), stderr.String())
	}
	data, err := readInitFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(stdout.String(), "Config file") || !strings.Contains(stdout.String(), "Choose models for auto") {
		t.Errorf("init output = %q; want config path and model selection prompts", stdout.String())
	}
	if !strings.Contains(content, "gemini/gemini-2.5-flash") || !strings.Contains(content, "mock/echo") || strings.Contains(content, "groq/llama-3.3-70b-versatile") {
		t.Errorf("generated config models = %q; want selected Gemini and mock, without deselected Groq", content)
	}
}

func TestRun_InitExistingInteractivePathPromptsForAnotherFile(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	existing := filepath.Join(directory, "config.yaml")
	newPath := filepath.Join(directory, "config-test.yaml")
	before := []byte("keep this config\n")
	if err := os.WriteFile(existing, before, 0o600); err != nil {
		t.Fatal(err)
	}
	input := strings.Join([]string{existing, newPath, "0", "0", "n", ""}, "\n")
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"init"}, strings.NewReader(input), &stdout, &stderr, noEnv)
	if code != 0 {
		t.Fatalf("init = %d; stdout %q; stderr %q", code, stdout.String(), stderr.String())
	}
	after, err := readInitFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Errorf("existing config changed: got %q, want %q", after, before)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("new config was not created: %v", err)
	}
}

func TestRun_InitInteractiveHuhChoicesPreserveSemanticsAndHideSecrets(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	const secret = "sk-test-" + "gemini-secret"
	lookup := func(name string) (string, bool) {
		return secret, name == "GEMINI_API_KEY"
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"init", "-config", path}, strings.NewReader("1\n0\ny\n0\ny\n"), &stdout, &stderr, lookup)
	if code != 0 {
		t.Fatalf("init = %d; stdout %q; stderr %q", code, stdout.String(), stderr.String())
	}
	data, err := readInitFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "gemini:") || !strings.Contains(content, "free_tier: true") || !strings.Contains(content, "store:") || !strings.Contains(content, "${GEMINI_API_KEY}") {
		t.Errorf("generated config = %q; want selected provider, attestation, usage storage and env reference", content)
	}
	if strings.Contains(content+stdout.String()+stderr.String(), secret) {
		t.Errorf("provider secret was disclosed by init")
	}
	if _, err := config.ParseWithEnv(data, lookup); err != nil {
		t.Fatalf("generated config does not validate: %v", err)
	}
}

func TestRun_InitInteractiveEOFDoesNotCreateConfig(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"init", "-config", path}, strings.NewReader(""), &stdout, &stderr, noEnv)
	if code != 1 || !strings.Contains(stderr.String(), "needs more input") || !strings.Contains(stderr.String(), "no config file was created") || !strings.Contains(stderr.String(), "spillway init -yes") {
		t.Fatalf("init = %d; stderr %q; want an input error", code, stderr.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config stat error = %v; want no config file", err)
	}
}

func TestRun_InitNeverOverwrites(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "existing.yaml")
	before := []byte("keep these bytes\n")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"init", "-yes", "-providers", "mock", "-config", path}, strings.NewReader(""), &stdout, &stderr, noEnv)
	after, err := readInitFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 || !bytes.Equal(after, before) || !strings.Contains(stderr.String(), path+" already exists") || !strings.Contains(stderr.String(), "never overwrites existing configs") || !strings.Contains(stderr.String(), "spillway check -config "+path) || !strings.Contains(stderr.String(), "new-path") {
		t.Errorf("init = %d, stderr %q, contents %q; want exit 1, recovery guidance, unchanged bytes", code, stderr.String(), after)
	}
}

func TestRun_InitRejectsMissingConfigDirectory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing", "config.yaml")
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"init", "-config", path}, strings.NewReader(""), &stdout, &stderr, noEnv)
	if code != 1 || !strings.Contains(stderr.String(), "does not exist; create it first") || !strings.Contains(stderr.String(), "choose a path in an existing folder") {
		t.Fatalf("init = %d; stderr %q; want clear guidance for a missing config directory", code, stderr.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config stat error = %v; want no config file", err)
	}
}

func TestRun_InitFileIsOwnerOnly(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	var stdout, stderr bytes.Buffer
	if code := cli.Run(t.Context(), []string{"init", "-yes", "-providers", "mock", "-config", path}, strings.NewReader(""), &stdout, &stderr, noEnv); code != 0 {
		t.Fatalf("init = %d; stderr: %s", code, stderr.String())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %o, want 600", info.Mode().Perm())
	}
}

func TestRun_InitWritesOnlyEnvReferencesForProviderKeys(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	const secret = "sk-test-" + "groq-secret"
	lookup := func(name string) (string, bool) {
		if name == "GROQ_API_KEY" {
			return secret, true
		}
		return "", false
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"init", "-yes", "-providers", "groq", "-attest-free-tier", "groq", "-config", path}, strings.NewReader(""), &stdout, &stderr, lookup)
	if code != 0 {
		t.Fatalf("init = %d; stdout %q; stderr %q", code, stdout.String(), stderr.String())
	}
	data, err := readInitFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || !strings.Contains(string(data), "${GROQ_API_KEY}") || strings.Contains(string(data), secret) {
		t.Errorf("init = %d; config %q; want env reference and no provider secret", code, data)
	}
}

func TestRun_InitNeverPrintsLocalKey(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"init", "-yes", "-providers", "mock", "-config", path}, strings.NewReader(""), &stdout, &stderr, noEnv)
	data, err := readInitFile(path)
	if err != nil {
		t.Fatal(err)
	}
	key := regexp.MustCompile(`sk-spillway-[A-Za-z0-9_-]{43}`).Find(data)
	if code != 0 || len(key) == 0 || strings.Contains(stdout.String()+stderr.String(), string(key)) {
		t.Errorf("init = %d; stdout %q; stderr %q; key %q", code, stdout.String(), stderr.String(), key)
	}
}

func TestRun_InitReportsVariableStatusWithoutValues(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	const secret = "sk-test-" + "groq-secret"
	lookup := func(name string) (string, bool) {
		return secret, name == "GROQ_API_KEY"
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"init", "-yes", "-providers", "groq", "-store", "-config", path}, strings.NewReader(""), &stdout, &stderr, lookup)
	if code != 0 || !strings.Contains(stdout.String(), "GROQ_API_KEY: set") || !strings.Contains(stdout.String(), "spillway key -config") || !strings.Contains(stdout.String(), "model `auto`") || !strings.Contains(stdout.String(), "dashboard`.") || strings.Contains(stdout.String()+stderr.String(), secret) {
		t.Errorf("init = %d; stdout %q; stderr %q", code, stdout.String(), stderr.String())
	}
	path = filepath.Join(t.TempDir(), "unset.yaml")
	stdout.Reset()
	stderr.Reset()
	code = cli.Run(t.Context(), []string{"init", "-yes", "-providers", "groq", "-config", path}, strings.NewReader(""), &stdout, &stderr, noEnv)
	recoveryCommand := "read -rs GROQ_API_KEY && export GROQ_API_KEY"
	if runtime.GOOS == "windows" {
		recoveryCommand = "$env:GROQ_API_KEY = [Net.NetworkCredential]::new('', (Read-Host -AsSecureString 'provider key')).Password"
	}
	if code != 0 || !strings.Contains(stdout.String(), "GROQ_API_KEY: not set") || !strings.Contains(stdout.String(), recoveryCommand) {
		t.Errorf("unset variable output: init = %d; stdout %q; stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestRun_InitUnattestedGroqStaysOutOfFreeTier(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"init", "-yes", "-providers", "groq", "-config", path}, strings.NewReader(""), &stdout, &stderr, noEnv)
	data, err := readInitFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || strings.Contains(string(data), "groq/") || !strings.Contains(stdout.String(), "attest") {
		t.Errorf("init = %d; config %q; stdout %q; stderr %q", code, data, stdout.String(), stderr.String())
	}
}

func TestRun_InitYesWithoutRequiredChoiceFails(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"init", "-yes"}, strings.NewReader(""), &stdout, &stderr, noEnv)
	if code != 2 || !strings.Contains(stderr.String(), "-providers") {
		t.Errorf("init = %d; stderr %q; want usage error naming -providers", code, stderr.String())
	}
}

func TestRun_InitPromptStopsWhenContextIsCancelled(t *testing.T) {
	t.Parallel()
	pipeReader, writer := io.Pipe()
	reader := closeSignalReader{PipeReader: pipeReader, closed: make(chan struct{})}
	defer func() {
		if err := writer.Close(); err != nil {
			t.Errorf("close prompt pipe: %v", err)
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	prompted := make(chan struct{}, 1)
	exit := make(chan int, 1)
	var stderr bytes.Buffer
	go func() {
		exit <- cli.Run(ctx, []string{"init"}, reader, promptWriter{wrote: prompted, want: "Config file"}, &stderr, noEnv)
	}()
	select {
	case <-prompted:
	case <-t.Context().Done():
		t.Fatal("init did not prompt")
	}
	cancel()
	<-reader.closed
	code := <-exit
	if code != 1 || !strings.Contains(stderr.String(), "Setup was cancelled") || !strings.Contains(stderr.String(), "No config file was created") {
		t.Errorf("init = %d; stderr %q; want cancellation error", code, stderr.String())
	}
}

func TestRun_InitMockQuickstartServes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"init", "-yes", "-providers", "mock", "-config", path}, strings.NewReader(""), &stdout, &stderr, noEnv)
	if code != 0 {
		t.Fatalf("init = %d; stderr %s", code, stderr.String())
	}
	data, err := readInitFile(path)
	if err != nil {
		t.Fatal(err)
	}
	withEphemeralPort := bytes.Replace(data, []byte("listen: 127.0.0.1:8080"), []byte("listen: 127.0.0.1:0"), 1)
	if bytes.Equal(data, withEphemeralPort) {
		t.Fatal("generated config has no default listen address")
	}
	if err := writeInitFile(path, withEphemeralPort); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadWithEnv(path, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	key := cfg.Server.APIKeys[0].Key.Reveal()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pr, pw := io.Pipe()
	logs := readLogs(pr)
	exit := make(chan int, 1)
	go func() {
		var serveOut bytes.Buffer
		exit <- cli.Run(ctx, []string{"serve", "-config", path}, strings.NewReader(""), &serveOut, pw, noEnv)
		_ = pw.Close()
	}()
	var base string
	select {
	case addr := <-logs.addr:
		base = "http://" + addr
	case result := <-exit:
		t.Fatalf("serve exited with %d before listening: %s", result, logs)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/chat/completions", strings.NewReader(`{"model":"mock/echo","messages":[{"role":"user","content":"hello init"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("hello init")) {
		t.Errorf("chat = %d %s, %v", response.StatusCode, body, err)
	}
	cancel()
	if result := <-exit; result != 0 {
		t.Errorf("serve exit = %d", result)
	}
	<-logs.done
}

func TestRun_InitCanceledContextDoesNotCreateConfig(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var stdout, stderr bytes.Buffer
	code := cli.Run(ctx, []string{"init", "-yes", "-providers", "mock", "-config", path}, strings.NewReader(""), &stdout, &stderr, noEnv)
	if code != 1 || !strings.Contains(stderr.String(), "context canceled") {
		t.Fatalf("init = %d; stderr = %q; want cancellation error", code, stderr.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config stat error = %v; want file not to exist", err)
	}
}

func TestRun_InitCanceledBeforeWritingDoesNotCreateConfig(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	path := filepath.Join(t.TempDir(), "config.yaml")
	var stdout, stderr bytes.Buffer
	lookupEnv := func(string) (string, bool) {
		cancel()
		return "sk-test-groq", true
	}

	code := cli.Run(ctx, []string{"init", "-yes", "-providers", "groq", "-attest-free-tier", "groq", "-config", path}, strings.NewReader(""), &stdout, &stderr, lookupEnv)
	if code != 1 || !strings.Contains(stderr.String(), "context canceled") {
		t.Fatalf("init = %d; stderr = %q; want cancellation error", code, stderr.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config exists after cancellation; stat error = %v", err)
	}
}
