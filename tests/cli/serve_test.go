package cli_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/Polqt/spillway/internal/cli"
	"github.com/Polqt/spillway/internal/config"
)

const e2eKey = "sk-test-local-e2e"

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Security property: serve never listens without a local key, loopback included.
func TestRun_ServeRejectsMissingLocalKey(t *testing.T) {
	t.Parallel()
	for _, listen := range []string{"127.0.0.1:0", "0.0.0.0:0"} {
		t.Run(listen, func(t *testing.T) {
			t.Parallel()
			path := writeConfig(t, "server:\n  listen: \""+listen+"\"\nproviders:\n  mock: {}\n")
			var stdout, stderr strings.Builder

			code := cli.Run(t.Context(), []string{"serve", "-config", path}, &stdout, &stderr, noEnv)

			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if !strings.HasPrefix(stderr.String(), "spillway: ") || !strings.Contains(stderr.String(), "server.api_keys") {
				t.Errorf("stderr = %q, want a spillway: error naming server.api_keys", stderr.String())
			}
			if strings.Contains(stderr.String(), "listening") {
				t.Errorf("stderr = %q, serve started listening", stderr.String())
			}
		})
	}
}

// A non-loopback listen exposes the API to the network with the Host check
// off, so serve says so before binding. 192.0.2.1 (TEST-NET-1) is never a
// local address, so the bind fails and no socket is opened.
func TestRun_ServeWarnsOnNonLoopbackListen(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, "server:\n  listen: \"192.0.2.1:0\"\n  api_keys: [sk-test-local-e2e]\nproviders:\n  mock: {}\n")
	var stdout, stderr strings.Builder

	code := cli.Run(t.Context(), []string{"serve", "-config", path}, &stdout, &stderr, noEnv)

	if code != 1 {
		t.Errorf("exit code = %d, want 1 (bind to a non-local address fails)", code)
	}
	if !strings.Contains(stderr.String(), "level=WARN") || !strings.Contains(stderr.String(), "reachable from the network") {
		t.Errorf("stderr = %q, want a WARN that the API is reachable from the network", stderr.String())
	}
	if strings.Contains(stderr.String(), e2eKey) {
		t.Errorf("stderr contains the local key: %q", stderr.String())
	}
}

func TestRun_ServeErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		code int
		want string
	}{
		{name: "missing flag value", args: []string{"serve", "-config"}, code: 2, want: "flag needs an argument"},
		{name: "unexpected argument", args: []string{"serve", "extra"}, code: 2, want: "unexpected argument"},
		{name: "missing file", args: []string{"serve", "-config", filepath.Join(t.TempDir(), "nope.yaml")}, code: 1, want: "nope.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			code := cli.Run(t.Context(), tt.args, &stdout, &stderr, noEnv)
			if code != tt.code {
				t.Errorf("exit code = %d, want %d", code, tt.code)
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.want)
			}
		})
	}
}

func TestRun_ServeDefaultConfigPath(t *testing.T) {
	// Chdir affects the whole process, so this test is not parallel.
	t.Chdir(t.TempDir())
	var stdout, stderr strings.Builder

	code := cli.Run(t.Context(), []string{"serve"}, &stdout, &stderr, noEnv)

	if code != 1 || !strings.Contains(stderr.String(), "config.yaml") {
		t.Errorf("exit %d, stderr %q; want 1 and an error naming config.yaml", code, stderr.String())
	}
}

// logLines collects serve's stderr line by line and reports the bound address.
type logLines struct {
	mu    sync.Mutex
	lines []string
	addr  chan string
	done  chan struct{}
}

var listeningAddr = regexp.MustCompile(`msg=listening addr=(\S+)`)

func readLogs(r io.Reader) *logLines {
	l := &logLines{addr: make(chan string, 1), done: make(chan struct{})}
	go func() {
		defer close(l.done)
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			l.mu.Lock()
			l.lines = append(l.lines, sc.Text())
			l.mu.Unlock()
			if m := listeningAddr.FindStringSubmatch(sc.Text()); m != nil {
				l.addr <- m[1]
			}
		}
	}()
	return l
}

func (l *logLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func TestRun_ServeMockEndToEnd(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, "server:\n  listen: \"127.0.0.1:0\"\n  api_keys:\n    - name: e2e\n      key: "+e2eKey+"\nproviders:\n  mock: {}\n")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pr, pw := io.Pipe()
	logs := readLogs(pr)
	exit := make(chan int, 1)
	go func() {
		var stdout strings.Builder
		exit <- cli.Run(ctx, []string{"serve", "-config", path}, &stdout, pw, noEnv)
		_ = pw.Close()
	}()

	var base string
	select {
	case addr := <-logs.addr:
		base = "http://" + addr
	case code := <-exit:
		t.Fatalf("serve exited with %d before listening:\n%s", code, logs)
	}

	send := func(method, path, host, key, body string) reply {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if host != "" {
			req.Host = host
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return reply{status: resp.StatusCode, header: resp.Header, body: string(b)}
	}
	const chat = `{"model":"mock/echo","messages":[{"role":"user","content":"hello spillway"}]}`

	if r := send(http.MethodGet, "/healthz", "", "", ""); r.status != http.StatusOK || r.body != "ok" {
		t.Errorf("/healthz: %d %q, want 200 ok", r.status, r.body)
	}
	if r := send(http.MethodPost, "/v1/chat/completions", "", "", chat); r.status != http.StatusUnauthorized {
		t.Errorf("chat without key: status %d, want 401", r.status)
	}
	if r := send(http.MethodPost, "/v1/chat/completions", "evil.example", e2eKey, chat); r.status != http.StatusForbidden {
		t.Errorf("chat with foreign Host: status %d, want 403", r.status)
	}

	r := send(http.MethodPost, "/v1/chat/completions", "", e2eKey, chat)
	if r.status != http.StatusOK {
		t.Fatalf("chat: status %d, want 200; body %s", r.status, r.body)
	}
	if r.header.Get("X-Spillway-Provider") != "mock" || r.header.Get("X-Spillway-Model") != "echo" {
		t.Errorf("chat headers = %v", r.header)
	}
	var completion struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(r.body), &completion); err != nil {
		t.Fatal(err)
	}
	if completion.Object != "chat.completion" || len(completion.Choices) != 1 || completion.Choices[0].Message.Content != "You said: hello spillway" {
		t.Errorf("completion = %+v", completion)
	}

	cancel()
	if code := <-exit; code != 0 {
		t.Errorf("exit code after cancel = %d, want 0", code)
	}
	<-logs.done

	out := logs.String()
	if strings.Contains(out, "level=WARN") {
		t.Errorf("loopback listen logged a warning:\n%s", out)
	}
	if !strings.Contains(out, "client_app=e2e") {
		t.Errorf("log lacks client_app=e2e:\n%s", out)
	}
	// Security property: serve's log output never contains a local key.
	if strings.Contains(out, e2eKey) {
		t.Errorf("log contains the local key:\n%s", out)
	}
}

type reply struct {
	status int
	header http.Header
	body   string
}

func TestExamples_MockConfigValidates(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(filepath.Join("..", "..", "examples", "config.mock.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Listen != "127.0.0.1:8080" {
		t.Errorf("Listen = %q, want 127.0.0.1:8080", cfg.Server.Listen)
	}
	keys := cfg.Server.APIKeys
	if len(keys) != 1 || keys[0].Name != "demo" || !strings.HasPrefix(keys[0].Key.Reveal(), "sk-test-") {
		t.Errorf("api_keys = %v, want one clearly fake sk-test- key named demo", keys)
	}
	if _, ok := cfg.Providers["mock"]; !ok || len(cfg.Providers) != 1 {
		t.Errorf("providers = %v, want only mock", cfg.Providers)
	}
}
