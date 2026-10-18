package cli_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Polqt/spillway/internal/cli"
	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/store"
)

const e2eKey = "sk-test-local-e2e"

func TestRun_MissingDefaultConfigSuggestsInit(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, command := range []string{"serve", "check"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr strings.Builder
			code := cli.Run(t.Context(), []string{command}, nil, &stdout, &stderr, noEnv)
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if !strings.Contains(stderr.String(), "spillway init") {
				t.Errorf("stderr = %q, want init guidance", stderr.String())
			}
		})
	}
}

func TestRun_ConfigRecoveryGuidance(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{name: "default path", args: []string{"check"}, want: "spillway init"},
		{name: "explicit path", args: []string{"check", "-config", "config.yaml"}, want: "-config path is correct"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			if code := cli.Run(t.Context(), tt.args, nil, &stdout, &stderr, noEnv); code != 1 {
				t.Fatalf("exit code = %d, want 1; stderr = %q", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("stderr = %q, want recovery guidance %q", stderr.String(), tt.want)
			}
		})
	}
	const localSecret = "sk-test-" + "local-secret"
	const providerSecret = "sk-test-" + "provider-secret"
	path := writeConfig(t, "server:\n  api_keys: [\"${LOCAL_KEY}\"]\nproviders:\n  groq:\n    api_key: \"${GROQ_API_KEY}\"\n")
	var stdout, stderr strings.Builder
	lookupEnv := func(name string) (string, bool) {
		if name == "LOCAL_KEY" {
			return localSecret, true
		}
		if name == "GROQ_API_KEY" {
			return providerSecret, false
		}
		return "", false
	}
	if code := cli.Run(t.Context(), []string{"check", "-config", path}, nil, &stdout, &stderr, lookupEnv); code != 1 {
		t.Fatalf("missing environment variable exit = %d; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Set the named environment variable") || strings.Contains(stderr.String(), localSecret) || strings.Contains(stderr.String(), providerSecret) {
		t.Errorf("missing environment guidance = %q", stderr.String())
	}
	var invalidOut, invalidErr strings.Builder
	invalidPath := writeConfig(t, "unknown: value\n")
	if code := cli.Run(t.Context(), []string{"check", "-config", invalidPath}, nil, &invalidOut, &invalidErr, noEnv); code != 1 {
		t.Fatalf("invalid config exit = %d; stderr = %q", code, invalidErr.String())
	}
	if !strings.Contains(invalidErr.String(), "Check the config syntax and values") {
		t.Errorf("invalid config guidance = %q", invalidErr.String())
	}
}

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

			code := cli.Run(t.Context(), []string{"serve", "-config", path}, nil, &stdout, &stderr, noEnv)

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

	code := cli.Run(t.Context(), []string{"serve", "-config", path}, nil, &stdout, &stderr, noEnv)

	if code != 1 {
		t.Errorf("exit code = %d, want 1 (bind to a non-local address fails)", code)
	}
	if !strings.Contains(stderr.String(), "level=WARN") || !strings.Contains(stderr.String(), "reachable from the network") {
		t.Errorf("stderr = %q, want a WARN that the API is reachable from the network", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Choose an available address in server.listen") {
		t.Errorf("stderr = %q, want listen recovery guidance", stderr.String())
	}
	if strings.Contains(stderr.String(), e2eKey) {
		t.Errorf("stderr contains the local key: %q", stderr.String())
	}
}

func TestRun_ServeHonorsLoggingLevel(t *testing.T) {
	t.Parallel()
	for i, tt := range []struct {
		level         string
		wantListening bool
	}{
		{level: "debug", wantListening: true},
		{level: "info", wantListening: true},
		{level: "warn"},
		{level: "error"},
	} {
		t.Run(tt.level, func(t *testing.T) {
			t.Parallel()
			addr := testListenAddress(t, 20000+i*1000)

			path := writeConfig(t, "server:\n  listen: \""+addr+"\"\n  api_keys: ["+e2eKey+"]\nlogging:\n  level: "+tt.level+"\nproviders:\n  mock: {}\n")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			pr, pw := io.Pipe()
			logs := readLogs(pr)
			exit := make(chan int, 1)
			go func() {
				var stdout strings.Builder
				exit <- cli.Run(ctx, []string{"serve", "-config", path}, nil, &stdout, pw, noEnv)
				_ = pw.Close()
			}()

			if tt.wantListening {
				select {
				case <-logs.addr:
				case code := <-exit:
					t.Fatalf("serve exited with %d before listening:\n%s", code, logs)
				}
			} else {
				waitForListener(t, addr, exit, logs)
			}

			cancel()
			if code := <-exit; code != 0 {
				t.Errorf("serve exit code = %d, want 0; logs:\n%s", code, logs)
			}
			<-logs.done

			gotListening := strings.Contains(logs.String(), "msg=listening")
			if gotListening != tt.wantListening {
				t.Errorf("listening log present = %t, want %t; logs:\n%s", gotListening, tt.wantListening, logs)
			}
		})
	}
}

func testListenAddress(t *testing.T, startPort int) string {
	t.Helper()
	// Avoid ephemeral ports because unrelated outbound connections can claim a
	// just-released ephemeral port before serve binds it. Each parallel subtest
	// gets a separate range so it cannot select a peer's released port.
	var listenConfig net.ListenConfig
	for port := startPort; port < startPort+1000; port++ {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		probe, err := listenConfig.Listen(t.Context(), "tcp", addr)
		if err != nil {
			continue
		}
		if err := probe.Close(); err != nil {
			t.Fatal(err)
		}
		return addr
	}
	t.Fatalf("no available loopback port in test range %d-%d", startPort, startPort+999)
	return ""
}

func waitForListener(t *testing.T, addr string, exit <-chan int, logs *logLines) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return
		}
		select {
		case code := <-exit:
			t.Fatalf("serve exited with %d before listening:\n%s", code, logs)
		case <-timer.C:
			t.Fatalf("serve did not listen on %s:\n%s", addr, logs)
		default:
		}
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
			code := cli.Run(t.Context(), tt.args, nil, &stdout, &stderr, noEnv)
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

	code := cli.Run(t.Context(), []string{"serve"}, nil, &stdout, &stderr, noEnv)

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
	path := writeConfig(t, "server:\n  listen: \"127.0.0.1:0\"\n  api_keys:\n    - name: e2e\n      key: "+e2eKey+"\nproviders:\n  mock: {}\ntiers:\n  free:\n    models: [mock/echo]\nmodels:\n  mock/echo:\n    capabilities: [tools]\nrouter:\n  auto: [free]\n")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pr, pw := io.Pipe()
	logs := readLogs(pr)
	exit := make(chan int, 1)
	go func() {
		var stdout strings.Builder
		exit <- cli.Run(ctx, []string{"serve", "-config", path}, nil, &stdout, pw, noEnv)
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
	autoBody := `{"model":"auto","messages":[{"role":"user","content":"use a tool"}],"tools":[{"type":"function","function":{"name":"lookup"}}]}`
	auto := send(http.MethodPost, "/v1/chat/completions", "", e2eKey, autoBody)
	if auto.status != http.StatusOK || !strings.Contains(auto.body, `"model":"echo"`) {
		t.Errorf("auto chat = %d %s, want mock/echo success", auto.status, auto.body)
	}
	models := send(http.MethodGet, "/v1/models", "", e2eKey, "")
	if models.status != http.StatusOK || !strings.Contains(models.body, `"id":"auto"`) {
		t.Errorf("model list = %d %s, want auto", models.status, models.body)
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

func TestRun_ServeCreatesStoreOnlyWhenConfigured(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	storePath := filepath.Join(dir, "usage.db")
	configPath := writeConfig(t, "server:\n  listen: \"127.0.0.1:0\"\n  api_keys: [sk-test-store-e2e]\nproviders:\n  mock: {}\nstore:\n  path: '"+storePath+"'\n  retention: 1d\n")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pr, pw := io.Pipe()
	logs := readLogs(pr)
	exit := make(chan int, 1)
	go func() {
		var stdout strings.Builder
		exit <- cli.Run(ctx, []string{"serve", "-config", configPath}, nil, &stdout, pw, noEnv)
		_ = pw.Close()
	}()
	select {
	case <-logs.addr:
	case code := <-exit:
		t.Fatalf("serve exited with %d before listening:\n%s", code, logs)
	}
	if _, err := os.Stat(storePath); err != nil {
		t.Fatalf("configured store file: %v", err)
	}
	unusedPath := filepath.Join(dir, "unused.db")
	if _, err := os.Stat(unusedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unconfigured store path exists: %v", err)
	}
	cancel()
	if code := <-exit; code != 0 {
		t.Errorf("serve exit code = %d, want 0; logs:\n%s", code, logs)
	}
	<-logs.done
}

func TestRun_ServeShutdownDrainsInFlightStreamAndRecordsUsage(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	storePath := filepath.Join(t.TempDir(), "usage.db")
	path := writeConfig(t, "server:\n  listen: \"127.0.0.1:0\"\n  api_keys: [sk-test-shutdown-drain]\nproviders:\n  openrouter:\n    api_key: \"${OPENROUTER_API_KEY}\"\n    base_url: \""+upstream.URL+"/v1\"\nstore:\n  path: '"+storePath+"'\n")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pr, pw := io.Pipe()
	logs := readLogs(pr)
	exit := make(chan int, 1)
	go func() {
		var stdout strings.Builder
		exit <- cli.Run(ctx, []string{"serve", "-config", path}, nil, &stdout, pw, func(name string) (string, bool) {
			return "sk-test-provider", name == "OPENROUTER_API_KEY"
		})
		_ = pw.Close()
	}()
	var addr string
	select {
	case addr = <-logs.addr:
	case code := <-exit:
		t.Fatalf("serve exited with %d before listening:\n%s", code, logs)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+"/v1/chat/completions", strings.NewReader(`{"model":"openrouter/test-model","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer sk-test-shutdown-drain")
	request.Header.Set("Content-Type", "application/json")
	responseCh := make(chan struct {
		status int
		body   []byte
		err    error
	}, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			responseCh <- struct {
				status int
				body   []byte
				err    error
			}{err: err}
			return
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr == nil {
			readErr = closeErr
		}
		responseCh <- struct {
			status int
			body   []byte
			err    error
		}{status: response.StatusCode, body: body, err: readErr}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not receive the streaming request")
	}
	cancel()
	close(release)
	result := <-responseCh
	if result.err != nil {
		t.Fatalf("stream request: %v", result.err)
	}
	if result.status != http.StatusOK {
		t.Errorf("stream status = %d, want 200", result.status)
	}
	if !strings.Contains(string(result.body), "data: [DONE]") {
		t.Errorf("stream body = %q, want data: [DONE]", result.body)
	}
	if code := <-exit; code != 0 {
		t.Errorf("serve exit code = %d, want 0; logs:\n%s", code, logs)
	}
	<-logs.done
	usageStore, err := store.Open(t.Context(), store.Options{Path: storePath, Retention: 90 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := usageStore.Recent(t.Context(), store.RecentQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := usageStore.Close(); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != http.StatusOK {
		t.Errorf("usage rows = %+v, want one status 200 row", rows)
	}
}

func TestRun_ServeShutdownTimeoutCancelsAndStillRecords(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"waiting\"},\"finish_reason\":null}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(release)
		upstream.Close()
	}()
	storePath := filepath.Join(t.TempDir(), "usage.db")
	path := writeConfig(t, "server:\n  listen: \"127.0.0.1:0\"\n  shutdown_timeout: 1s\n  api_keys: [sk-test-shutdown-timeout]\nproviders:\n  openrouter:\n    api_key: \"${OPENROUTER_API_KEY}\"\n    base_url: \""+upstream.URL+"/v1\"\nstore:\n  path: '"+storePath+"'\n")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pr, pw := io.Pipe()
	logs := readLogs(pr)
	exit := make(chan int, 1)
	go func() {
		var stdout strings.Builder
		exit <- cli.Run(ctx, []string{"serve", "-config", path}, nil, &stdout, pw, func(name string) (string, bool) {
			return "sk-test-provider", name == "OPENROUTER_API_KEY"
		})
		_ = pw.Close()
	}()
	var addr string
	select {
	case addr = <-logs.addr:
	case code := <-exit:
		t.Fatalf("serve exited with %d before listening:\n%s", code, logs)
	}
	requestCtx, cancelRequest := context.WithCancel(t.Context())
	defer cancelRequest()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, "http://"+addr+"/v1/chat/completions", strings.NewReader(`{"model":"openrouter/test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer sk-test-shutdown-timeout")
	request.Header.Set("Content-Type", "application/json")
	releaseResponse := make(chan struct{})
	clientDone := make(chan struct{})
	defer func() {
		cancelRequest()
		close(releaseResponse)
		<-clientDone
	}()
	responseCh := make(chan struct {
		status int
		err    error
	}, 1)
	go func() {
		defer close(clientDone)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			responseCh <- struct {
				status int
				err    error
			}{err: err}
			return
		}
		defer func() { _ = response.Body.Close() }()
		responseCh <- struct {
			status int
			err    error
		}{status: response.StatusCode}
		<-releaseResponse
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not receive the streaming request")
	}
	select {
	case result := <-responseCh:
		if result.err != nil {
			t.Fatalf("stream request: %v", result.err)
		}
		if result.status != http.StatusOK {
			t.Fatalf("stream status = %d, want 200", result.status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream response headers were not returned")
	}
	cancel()
	select {
	case code := <-exit:
		if code != 1 {
			t.Errorf("serve exit code = %d, want 1; logs:\n%s", code, logs)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("serve did not honor the 1s shutdown timeout")
	}
	<-logs.done
	if !strings.Contains(logs.String(), "cancelled 1 in-flight request") {
		t.Errorf("logs = %s, want cancellation count", logs)
	}
	if strings.Contains(logs.String(), "database is closed") {
		t.Errorf("logs contain store close race: %s", logs)
	}
	usageStore, err := store.Open(t.Context(), store.Options{Path: storePath, Retention: 90 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := usageStore.Recent(t.Context(), store.RecentQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := usageStore.Close(); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("usage rows = %d, want 1", len(rows))
	} else if rows[0].ErrorKind == "" {
		t.Errorf("usage row = %+v, want an error kind for the cancelled upstream", rows[0])
	}
}

func TestRun_ServeWithoutStoreCreatesNoAdditionalFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("server:\n  listen: \"127.0.0.1:0\"\n  api_keys: [{name: admin, key: sk-test-no-store, dashboard: true}]\nproviders:\n  mock: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pr, pw := io.Pipe()
	logs := readLogs(pr)
	exit := make(chan int, 1)
	go func() {
		var stdout strings.Builder
		exit <- cli.Run(ctx, []string{"serve", "-config", configPath}, nil, &stdout, pw, noEnv)
		_ = pw.Close()
	}()
	var base string
	select {
	case addr := <-logs.addr:
		base = "http://" + addr
	case code := <-exit:
		t.Fatalf("serve exited with %d before listening:\n%s", code, logs)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/api/quotas", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-test-no-store")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "{\"candidates\":[]}\n" {
		t.Fatalf("GET /api/quotas = %d %s, want empty candidates", response.StatusCode, body)
	}
	cancel()
	if code := <-exit; code != 0 {
		t.Errorf("serve exit code = %d, want 0; logs:\n%s", code, logs)
	}
	<-logs.done
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.yaml" {
		t.Errorf("files beside config = %v, want only config.yaml", entries)
	}
}

func TestRun_ServeRejectsStorePathThatCannotOpen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	storePath := filepath.Join(dir, "not-a-database")
	if err := os.WriteFile(storePath, []byte("invalid sqlite file"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, "server:\n  listen: \"127.0.0.1:0\"\n  api_keys: [sk-test-store-bad-path]\nproviders:\n  mock: {}\nstore:\n  path: '"+storePath+"'\n")
	var stdout, stderr strings.Builder
	code := cli.Run(t.Context(), []string{"serve", "-config", path}, nil, &stdout, &stderr, noEnv)
	if code != 1 || !strings.Contains(stderr.String(), "spillway: ") || strings.Contains(stderr.String(), "listening") {
		t.Errorf("serve = %d, stderr %q; want startup failure before listening", code, stderr.String())
	}
}

func TestRun_ServeLogsAttestedFreeTierProviders(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, "server:\n  listen: \"127.0.0.1:0\"\n  api_keys: [sk-test-local-attestation]\nproviders:\n  groq:\n    api_key: \"${GROQ_API_KEY}\"\n    free_tier: true\n  gemini:\n    api_key: \"${GEMINI_API_KEY}\"\n    free_tier: true\n")
	lookup := func(name string) (string, bool) {
		return "sk-test-" + strings.ToLower(name), true
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pr, pw := io.Pipe()
	logs := readLogs(pr)
	exit := make(chan int, 1)
	go func() {
		var stdout strings.Builder
		exit <- cli.Run(ctx, []string{"serve", "-config", path}, nil, &stdout, pw, lookup)
		_ = pw.Close()
	}()

	select {
	case <-logs.addr:
	case code := <-exit:
		t.Fatalf("serve exited with %d before listening:\n%s", code, logs)
	}
	cancel()
	if code := <-exit; code != 0 {
		t.Errorf("serve exit code = %d, want 0; logs:\n%s", code, logs)
	}
	<-logs.done
	if !strings.Contains(logs.String(), `msg="free-tier providers are user-attested" providers="[gemini groq]"`) {
		t.Errorf("serve logs = %q, want sorted attested provider list", logs.String())
	}
	if strings.Contains(logs.String(), "sk-test-GROQ_API_KEY") || strings.Contains(logs.String(), "sk-test-GEMINI_API_KEY") {
		t.Errorf("serve logs contain provider key: %s", logs.String())
	}
}

func TestRun_ServeGroqEndToEnd(t *testing.T) {
	t.Parallel()
	const providerKey = "sk-test-groq-cli"
	var upstreamAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl-cli","object":"chat.completion","created":1,"model":"llama-3.1-8b-instant","choices":[{"index":0,"message":{"role":"assistant","content":"Groq wired"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	path := writeConfig(t, "server:\n  listen: \"127.0.0.1:0\"\n  api_keys: [\"${SPILLWAY_KEY}\"]\nproviders:\n  groq:\n    api_key: \"${GROQ_API_KEY}\"\n    base_url: \""+upstream.URL+"/v1\"\n")
	lookup := func(name string) (string, bool) {
		values := map[string]string{"SPILLWAY_KEY": e2eKey, "GROQ_API_KEY": providerKey}
		value, ok := values[name]
		return value, ok
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pr, pw := io.Pipe()
	logs := readLogs(pr)
	exit := make(chan int, 1)
	go func() {
		var stdout strings.Builder
		exit <- cli.Run(ctx, []string{"serve", "-config", path}, nil, &stdout, pw, lookup)
		_ = pw.Close()
	}()
	var base string
	select {
	case addr := <-logs.addr:
		base = "http://" + addr
	case code := <-exit:
		t.Fatalf("serve exited with %d before listening:\n%s", code, logs)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/chat/completions", strings.NewReader(`{"model":"groq/llama-3.1-8b-instant","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e2eKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Groq wired") || upstreamAuth != "Bearer "+providerKey {
		t.Errorf("chat = %d %s, upstream auth = %q", resp.StatusCode, body, upstreamAuth)
	}
	cancel()
	if code := <-exit; code != 0 {
		t.Errorf("exit code after cancel = %d, want 0", code)
	}
	<-logs.done
}

func TestRun_ServeGeminiEndToEnd(t *testing.T) {
	t.Parallel()
	const geminiKey = "sk-test-gemini-cli"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+geminiKey {
			t.Errorf("upstream Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Query().Get("key") != "" {
			t.Errorf("Gemini key appeared in URL query")
		}
		switch r.URL.Path {
		case "/v1beta/openai/models":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"models/gemini-2.5-flash","owned_by":"google"}]}`)
		case "/v1beta/openai/chat/completions":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `[{"error":{"code":429,"message":"`+geminiKey+` quota exhausted","status":"RESOURCE_EXHAUSTED"}}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	path := writeConfig(t, "server:\n  listen: \"127.0.0.1:0\"\n  api_keys: [\"${SPILLWAY_KEY}\"]\nproviders:\n  gemini:\n    api_key: \"${GEMINI_API_KEY}\"\n    base_url: \""+upstream.URL+"/v1beta/openai\"\n")
	lookup := func(name string) (string, bool) {
		values := map[string]string{"SPILLWAY_KEY": e2eKey, "GEMINI_API_KEY": geminiKey}
		value, ok := values[name]
		return value, ok
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pr, pw := io.Pipe()
	logs := readLogs(pr)
	exit := make(chan int, 1)
	go func() {
		var stdout strings.Builder
		exit <- cli.Run(ctx, []string{"serve", "-config", path}, nil, &stdout, pw, lookup)
		_ = pw.Close()
	}()
	var base string
	select {
	case addr := <-logs.addr:
		base = "http://" + addr
	case code := <-exit:
		t.Fatalf("serve exited with %d before listening:\n%s", code, logs)
	}

	modelsReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	modelsReq.Header.Set("Authorization", "Bearer "+e2eKey)
	modelsResp, err := http.DefaultClient.Do(modelsReq)
	if err != nil {
		t.Fatal(err)
	}
	modelBody, err := io.ReadAll(modelsResp.Body)
	_ = modelsResp.Body.Close()
	if err != nil || modelsResp.StatusCode != http.StatusOK || !strings.Contains(string(modelBody), `"id":"gemini/gemini-2.5-flash"`) || strings.Contains(string(modelBody), "models/") {
		t.Fatalf("models response = %d %s, %v; want prefix-free gemini model reference", modelsResp.StatusCode, modelBody, err)
	}

	chatReq, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/chat/completions", strings.NewReader(`{"model":"gemini/gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	chatReq.Header.Set("Authorization", "Bearer "+e2eKey)
	chatReq.Header.Set("Content-Type", "application/json")
	chatResp, err := http.DefaultClient.Do(chatReq)
	if err != nil {
		t.Fatal(err)
	}
	chatBody, err := io.ReadAll(chatResp.Body)
	_ = chatResp.Body.Close()
	if err != nil || chatResp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(chatBody), `"type":"api_error"`) || !strings.Contains(string(chatBody), `"code":"rate_limit_exceeded"`) || strings.Contains(string(chatBody), geminiKey) {
		t.Errorf("chat response = %d %s, %v; want the OpenAI error object with 429 rate_limit_exceeded", chatResp.StatusCode, chatBody, err)
	}
	cancel()
	if code := <-exit; code != 0 {
		t.Errorf("exit code after cancel = %d, want 0", code)
	}
	<-logs.done
	if strings.Contains(logs.String(), geminiKey) {
		t.Errorf("serve logs contain Gemini key: %s", logs)
	}
}

func TestRun_ServeOpenRouterAndOllamaPresets(t *testing.T) {
	t.Parallel()
	const openrouterKey = "sk-test-openrouter-cli"
	const openrouterModel = "meta-llama/llama-3.1-8b-instruct:free"
	const ollamaModel = "llama3.2:3b"
	gotModels := map[string]string{}
	gotAuth := map[string]string{}
	var gotMu sync.Mutex
	newUpstream := func(name, model string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMu.Lock()
			gotAuth[name] = r.Header.Get("Authorization")
			gotMu.Unlock()
			switch r.URL.Path {
			case "/v1/models":
				_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"`+model+`","object":"model","owned_by":"fixture"}]}`)
			case "/v1/chat/completions":
				body, _ := io.ReadAll(r.Body)
				var request struct {
					Model string `json:"model"`
				}
				if err := json.Unmarshal(body, &request); err != nil {
					t.Errorf("decode %s chat request: %v", name, err)
				}
				gotMu.Lock()
				gotModels[name] = request.Model
				gotMu.Unlock()
				_, _ = io.WriteString(w, `{"id":"chatcmpl-`+name+`","object":"chat.completion","created":1,"model":"`+model+`","choices":[{"index":0,"message":{"role":"assistant","content":"`+name+` wired"},"finish_reason":"stop"}]}`)
			default:
				http.NotFound(w, r)
			}
		}))
	}
	openrouter := newUpstream("openrouter", openrouterModel)
	defer openrouter.Close()
	ollama := newUpstream("ollama", ollamaModel)
	defer ollama.Close()
	path := writeConfig(t, "server:\n  listen: \"127.0.0.1:0\"\n  api_keys: [\"${SPILLWAY_KEY}\"]\nproviders:\n  openrouter:\n    api_key: \"${OPENROUTER_API_KEY}\"\n    base_url: \""+openrouter.URL+"/v1\"\n  ollama:\n    base_url: \""+ollama.URL+"\"\n")
	lookup := func(name string) (string, bool) {
		values := map[string]string{"SPILLWAY_KEY": e2eKey, "OPENROUTER_API_KEY": openrouterKey}
		value, ok := values[name]
		return value, ok
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pr, pw := io.Pipe()
	logs := readLogs(pr)
	exit := make(chan int, 1)
	go func() {
		var stdout strings.Builder
		exit <- cli.Run(ctx, []string{"serve", "-config", path}, nil, &stdout, pw, lookup)
		_ = pw.Close()
	}()
	var base string
	select {
	case addr := <-logs.addr:
		base = "http://" + addr
	case code := <-exit:
		t.Fatalf("serve exited with %d before listening:\n%s", code, logs)
	}
	if !strings.Contains(logs.String(), "msg=\"next steps\"") || !strings.Contains(logs.String(), "dashboard=http://") || !strings.Contains(logs.String(), "/v1/models") {
		t.Errorf("startup guidance = %q, want API and dashboard next steps", logs.String())
	}

	modelsReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	modelsReq.Header.Set("Authorization", "Bearer "+e2eKey)
	modelsResp, err := http.DefaultClient.Do(modelsReq)
	if err != nil {
		t.Fatal(err)
	}
	modelBody, err := io.ReadAll(modelsResp.Body)
	_ = modelsResp.Body.Close()
	if err != nil || modelsResp.StatusCode != http.StatusOK {
		t.Fatalf("models response = %d %s, %v", modelsResp.StatusCode, modelBody, err)
	}
	for _, modelRef := range []string{"openrouter/" + openrouterModel, "ollama/" + ollamaModel} {
		if !strings.Contains(string(modelBody), modelRef) {
			t.Errorf("model list %s does not contain %q", modelBody, modelRef)
		}
	}

	for name, model := range map[string]string{"openrouter": openrouterModel, "ollama": ollamaModel} {
		body := `{"model":"` + name + `/` + model + `","messages":[{"role":"user","content":"hi"}]}`
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+e2eKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		chatBody, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(chatBody), name+" wired") {
			t.Errorf("%s chat = %d %s, %v", name, resp.StatusCode, chatBody, err)
		}
	}
	gotMu.Lock()
	defer gotMu.Unlock()
	if gotModels["openrouter"] != openrouterModel || gotAuth["openrouter"] != "Bearer "+openrouterKey {
		t.Errorf("OpenRouter request model/auth = %q/%q", gotModels["openrouter"], gotAuth["openrouter"])
	}
	if gotModels["ollama"] != ollamaModel || gotAuth["ollama"] != "" {
		t.Errorf("Ollama request model/auth = %q/%q", gotModels["ollama"], gotAuth["ollama"])
	}
	cancel()
	if code := <-exit; code != 0 {
		t.Errorf("exit code after cancel = %d, want 0", code)
	}
	<-logs.done
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
	if len(keys) != 2 || keys[0].Name != "demo" || !strings.HasPrefix(keys[0].Key.Reveal(), "sk-test-") || keys[1].Name != "dashboard" || !keys[1].Dashboard {
		t.Errorf("api_keys = %v, want fake demo and dashboard keys with dashboard access enabled", keys)
	}
	if _, ok := cfg.Providers["mock"]; !ok || len(cfg.Providers) != 1 {
		t.Errorf("providers = %v, want only mock", cfg.Providers)
	}
	if cfg.Store == nil || cfg.Store.Path != "./spillway-mock.db" || cfg.Budgets == nil || cfg.Budgets.MonthlyMicroUSD <= 0 || cfg.Cache == nil || !cfg.Cache.Exact {
		t.Errorf("v0.4 config = store:%+v budgets:%+v cache:%+v, want all observability features enabled", cfg.Store, cfg.Budgets, cfg.Cache)
	}
	if _, ok := cfg.Pricing["mock/echo"]; !ok {
		t.Errorf("pricing = %v, want a mock/echo example price", cfg.Pricing)
	}
}
