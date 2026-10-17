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

type promptWriter struct{ wrote chan struct{} }

func (w promptWriter) Write(data []byte) (int, error) {
	select {
	case w.wrote <- struct{}{}:
	default:
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
	code := cli.Run(t.Context(), []string{"init", "-config", path}, strings.NewReader("mock\nn\nn\n"), &stdout, &stderr, noEnv)
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
	if code != 1 || !bytes.Equal(after, before) || !strings.Contains(stderr.String(), path) {
		t.Errorf("init = %d, stderr %q, contents %q; want exit 1, path in error, unchanged bytes", code, stderr.String(), after)
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
	code := cli.Run(t.Context(), []string{"init", "-yes", "-providers", "groq", "-config", path}, strings.NewReader(""), &stdout, &stderr, lookup)
	if code != 0 || !strings.Contains(stdout.String(), "GROQ_API_KEY: set") || strings.Contains(stdout.String()+stderr.String(), secret) {
		t.Errorf("init = %d; stdout %q; stderr %q", code, stdout.String(), stderr.String())
	}
	path = filepath.Join(t.TempDir(), "unset.yaml")
	stdout.Reset()
	stderr.Reset()
	code = cli.Run(t.Context(), []string{"init", "-yes", "-providers", "groq", "-config", path}, strings.NewReader(""), &stdout, &stderr, noEnv)
	if code != 0 || !strings.Contains(stdout.String(), "GROQ_API_KEY: not set") {
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
		exit <- cli.Run(ctx, []string{"init"}, reader, promptWriter{wrote: prompted}, &stderr, noEnv)
	}()
	select {
	case <-prompted:
	case <-t.Context().Done():
		t.Fatal("init did not prompt")
	}
	cancel()
	<-reader.closed
	code := <-exit
	if code != 1 || !strings.Contains(stderr.String(), "context canceled") {
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
