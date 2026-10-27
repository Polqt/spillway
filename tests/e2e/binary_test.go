package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var spillwayBinary string

// binaryVersion is stamped the way release builds stamp it, through -X.
const binaryVersion = "v9.9.9"

func TestMain(m *testing.M) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		fmt.Fprintln(os.Stderr, "find repository root:", err)
		os.Exit(1)
	}
	buildDir, err := os.MkdirTemp("", "spillway-binary-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create binary test directory:", err)
		os.Exit(1)
	}
	spillwayBinary = filepath.Join(buildDir, "spillway")
	if runtime.GOOS == "windows" {
		spillwayBinary += ".exe"
	}
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 2*time.Minute)
	build := exec.CommandContext(buildCtx, "go", "build", "-trimpath", "-ldflags", "-X github.com/Polqt/spillway/internal/cli.version="+binaryVersion, "-o", spillwayBinary, "./apps/spillway") //nolint:gosec // The test invokes the Go tool with fixed build arguments.
	build.Dir = root
	build.Env = setEnv(os.Environ(), "CGO_ENABLED", "0")
	output, buildErr := build.CombinedOutput()
	cancelBuild()
	if buildErr != nil {
		fmt.Fprintf(os.Stderr, "build spillway binary: %v\n%s", buildErr, output)
		_ = os.RemoveAll(buildDir)
		os.Exit(1)
	}
	code := m.Run()
	if err := os.RemoveAll(buildDir); err != nil {
		fmt.Fprintln(os.Stderr, "remove binary test directory:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func TestBinary_EvaluationConfigStarts(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(root, "examples", "config.eval.yaml")) // #nosec G304 -- root is the repository root found from this test's source directory.
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	addr := freeLoopbackAddr(t)
	config = bytes.Replace(config, []byte("127.0.0.1:8080"), []byte(addr), 1)
	configPath := filepath.Join(workDir, "config.yaml")
	if err := os.WriteFile(configPath, config, 0o600); err != nil { // #nosec G703 -- configPath is inside t.TempDir.
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), spillwayBinary, "serve", "-config", configPath) //nolint:gosec // TestMain built this executable and configPath is inside t.TempDir.
	command.Dir = workDir
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal("start spillway serve:", err)
	}
	wait := make(chan error, 1)
	waitDone := make(chan struct{})
	go func() {
		wait <- command.Wait()
		close(waitDone)
	}()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		<-waitDone
	})
	baseURL := "http://" + addr
	waitForBinaryServer(t, baseURL, wait)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close health response: %v", err)
		}
	}()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("health response = %d body %s", response.StatusCode, body)
	}
}

func TestBinary_VersionShowsOnlyVersionByDefault(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), spillwayBinary, "version") //nolint:gosec // TestMain built this executable from the current source tree.
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("spillway version: %v\n%s", err, output)
	}
	line := strings.TrimSpace(string(output))
	if want := binaryVersion; line != want {
		t.Fatalf("version output = %q, want %q", line, want)
	}
}

func TestBinary_LdflagsVersionIsPrinted(t *testing.T) {
	output, err := exec.CommandContext(t.Context(), spillwayBinary, "version").CombinedOutput() //nolint:gosec // TestMain built this executable from the current source tree.
	if err != nil {
		t.Fatalf("spillway version: %v\n%s", err, output)
	}
	if got, want := strings.TrimSpace(string(output)), binaryVersion; got != want {
		t.Errorf("version output = %q, want %q", got, want)
	}
}

func TestBinary_InitServeQuickstart(t *testing.T) {
	workDir := t.TempDir()
	configPath := filepath.Join(workDir, "config.yaml")
	initOutput, initErr := runBinary(t, workDir, "init", "-yes", "-providers", "ollama", "-store", "-config", configPath)
	if initErr != nil {
		t.Fatalf("spillway init: %v\n%s", initErr, initOutput)
	}
	for _, want := range []string{"Client: base URL http://127.0.0.1:8080/v1", "model `auto`", "spillway key -config", "Dashboard: page http://127.0.0.1:8080/dashboard"} {
		if !strings.Contains(initOutput, want) {
			t.Errorf("spillway init output = %q, want %q", initOutput, want)
		}
	}

	config, err := os.ReadFile(configPath) //nolint:gosec // configPath is inside t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	keys := regexp.MustCompile(`key: "(sk-spillway-[A-Za-z0-9_-]+)"`).FindAllSubmatch(config, -1)
	if len(keys) != 2 {
		t.Fatalf("generated config has %d local keys, want app and dashboard keys", len(keys))
	}
	appKey, dashboardKey := string(keys[0][1]), string(keys[1][1])
	addr := freeLoopbackAddr(t)
	config = bytes.Replace(config, []byte("127.0.0.1:8080"), []byte(addr), 1)
	if err := os.WriteFile(configPath, config, 0o600); err != nil { //nolint:gosec // configPath is inside t.TempDir.
		t.Fatal(err)
	}

	checkOutput, checkErr := runBinary(t, workDir, "check", "-config", configPath)
	if checkErr != nil {
		t.Fatalf("spillway check: %v\n%s", checkErr, checkOutput)
	}

	command := exec.CommandContext(t.Context(), spillwayBinary, "serve", "-config", configPath) //nolint:gosec // TestMain built this executable and configPath is inside t.TempDir.
	command.Dir = workDir
	var stdout, stderr synchronizedBuffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal("start spillway serve:", err)
	}
	wait := make(chan error, 1)
	waitDone := make(chan struct{})
	go func() {
		wait <- command.Wait()
		close(waitDone)
	}()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		<-waitDone
	})
	baseURL := "http://" + addr
	waitForBinaryServer(t, baseURL, wait)
	for _, want := range []string{"msg=\"next steps\"", "dashboard_key_configured=true", "store_enabled=true", "api=" + baseURL + "/v1", "models=" + baseURL + "/v1/models", "dashboard=" + baseURL + "/dashboard"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("serve startup output = %q, want %q", stderr.String(), want)
		}
	}

	requestBinaryAPI(t, baseURL, http.MethodGet, "/v1/models", "", appKey, http.StatusOK, "")
	requestBinaryAPI(t, baseURL, http.MethodGet, "/api/usage", "", appKey, http.StatusForbidden, "")
	requestBinaryAPI(t, baseURL, http.MethodGet, "/api/usage", "", dashboardKey, http.StatusOK, "")

	if err := command.Process.Kill(); err != nil {
		t.Fatalf("stop spillway serve: %v", err)
	}
	select {
	case err := <-wait:
		if err == nil {
			t.Error("spillway serve exited successfully after forced termination")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("spillway serve did not stop within 5s")
	}
	combinedOutput := initOutput + checkOutput + stdout.String() + stderr.String()
	if strings.Contains(combinedOutput, appKey) || strings.Contains(combinedOutput, dashboardKey) {
		t.Error("generated local key appeared in command output")
	}
}

func TestBinary_ProviderAddDoctorJourney(t *testing.T) {
	workDir := t.TempDir()
	configPath := filepath.Join(workDir, "config.yaml")
	providerKey := "gsk_test-provider-canary-do-not-print-0123456789"
	environment := setEnv(os.Environ(), "GROQ_API_KEY", providerKey)
	initOutput, initErr := runBinaryWithEnv(t, workDir, environment, "init", "-yes", "-providers", "groq", "-attest-free-tier", "groq", "-config", configPath)
	if initErr != nil {
		t.Fatalf("spillway init: %v\n%s", initErr, initOutput)
	}
	configData, err := os.ReadFile(configPath) //nolint:gosec // configPath is inside t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	localKeys := regexp.MustCompile(`key: "(sk-spillway-[A-Za-z0-9_-]+)"`).FindAllSubmatch(configData, -1)
	if len(localKeys) != 1 {
		t.Fatalf("generated config has %d local keys, want one", len(localKeys))
	}
	localKey := string(localKeys[0][1])

	addr := freeLoopbackAddr(t)
	configData = bytes.Replace(configData, []byte("127.0.0.1:8080"), []byte(addr), 1)
	if err := os.WriteFile(configPath, configData, 0o600); err != nil { //nolint:gosec // configPath is inside t.TempDir.
		t.Fatal(err)
	}
	addOutput, addErr := runBinaryWithEnv(t, workDir, environment, "provider", "add", "ollama", "--tier", "free", "--yes", "-config", configPath)
	if addErr != nil {
		t.Fatalf("spillway provider add: %v\n%s", addErr, addOutput)
	}
	if !strings.Contains(addOutput, "@@ providers @@") || !strings.Contains(addOutput, "backup at") {
		t.Errorf("provider add output = %q, want preview and backup guidance", addOutput)
	}
	checkOutput, checkErr := runBinaryWithEnv(t, workDir, environment, "check", "-config", configPath)
	if checkErr != nil {
		t.Fatalf("spillway check: %v\n%s", checkErr, checkOutput)
	}

	command := exec.CommandContext(t.Context(), spillwayBinary, "serve", "-config", configPath) //nolint:gosec // TestMain built this executable and configPath is inside t.TempDir.
	command.Dir = workDir
	command.Env = environment
	var serverOutput synchronizedBuffer
	command.Stdout, command.Stderr = &serverOutput, &serverOutput
	if err := command.Start(); err != nil {
		t.Fatal("start spillway serve:", err)
	}
	wait := make(chan error, 1)
	waitDone := make(chan struct{})
	go func() {
		wait <- command.Wait()
		close(waitDone)
	}()
	t.Cleanup(func() {
		select {
		case <-waitDone:
		default:
			_ = command.Process.Kill()
			<-waitDone
		}
	})
	baseURL := "http://" + addr
	waitForBinaryServer(t, baseURL, wait)
	runningOutput, runningErr := runBinaryWithEnv(t, workDir, environment, "doctor", "-config", configPath)
	if runningErr != nil || !strings.Contains(runningOutput, "answers like Spillway") {
		t.Fatalf("doctor against running server: %v\n%s", runningErr, runningOutput)
	}
	if !strings.Contains(serverOutput.String(), `providers="[groq ollama]"`) {
		t.Errorf("serve startup summary = %q, want loaded providers", serverOutput.String())
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal("stop spillway serve:", err)
	}
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("spillway serve did not stop within 5s")
	}
	stoppedOutput, stoppedErr := runBinaryWithEnv(t, workDir, environment, "doctor", "-config", configPath)
	if stoppedErr != nil || !strings.Contains(stoppedOutput, "nothing listening") {
		t.Fatalf("doctor after server stopped: %v\n%s", stoppedErr, stoppedOutput)
	}
	allOutput := strings.Join([]string{initOutput, addOutput, checkOutput, serverOutput.String(), runningOutput, stoppedOutput}, "\n")
	for _, canary := range []string{providerKey, localKey} {
		if strings.Contains(allOutput, canary) {
			t.Errorf("credential canary appeared in a command stream")
		}
	}
}

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *synchronizedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func runBinary(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	command := exec.CommandContext(t.Context(), spillwayBinary, args...) //nolint:gosec // TestMain built this executable; callers use fixed CLI arguments.
	command.Dir = dir
	output, err := command.CombinedOutput()
	return string(output), err
}

func runBinaryWithEnv(t *testing.T, dir string, environment []string, args ...string) (string, error) {
	t.Helper()
	command := exec.CommandContext(t.Context(), spillwayBinary, args...) //nolint:gosec // TestMain built this executable; callers use fixed CLI arguments.
	command.Dir = dir
	command.Env = environment
	output, err := command.CombinedOutput()
	return string(output), err
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("reserve loopback port:", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal("release loopback port:", err)
	}
	return addr
}

func waitForBinaryServer(t *testing.T, baseURL string, wait <-chan error) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	client := &http.Client{Timeout: time.Second}
	for {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case err := <-wait:
			t.Fatalf("spillway exited before listening: %v", err)
		case <-deadline.C:
			t.Fatal("spillway did not listen within 10s")
		case <-ticker.C:
		}
	}
}

func requestBinaryAPI(t *testing.T, baseURL, method, path, body, key string, wantStatus int, wantBody string) string {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, baseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if path == "/v1/messages" {
		request.Header.Set("x-api-key", key)
		request.Header.Set("anthropic-version", "2023-06-01")
	} else {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close %s %s response: %v", method, path, err)
		}
	}()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	if response.StatusCode != wantStatus || !bytes.Contains(data, []byte(wantBody)) {
		t.Errorf("%s %s = %d %s, want status %d containing %q", method, path, response.StatusCode, data, wantStatus, wantBody)
	}
	return string(data)
}

func TestBinary_ResponsesAndAnthropicClientsReachFakeProvider(t *testing.T) {
	var upstreamRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unexpected provider request", http.StatusNotFound)
			return
		}
		upstreamRequests.Add(1)
		var request struct {
			Stream bool `json:"stream"`
			Tools  []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid chat request", http.StatusBadRequest)
			return
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "streaming unsupported", http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-e2e\",\"created\":1,\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Stream hello.\"},\"finish_reason\":null}]}\n\n")
			flusher.Flush()
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-e2e\",\"created\":1,\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if len(request.Tools) > 0 {
			_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-e2e","object":"chat.completion","created":1,"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call-e2e","type":"function","function":{"name":%q,"arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`, request.Tools[0].Function.Name)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl-e2e","object":"chat.completion","created":1,"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"Hello from the fake provider."},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)
	}))
	t.Cleanup(upstream.Close)

	workDir := t.TempDir()
	configPath := filepath.Join(workDir, "config.yaml")
	localKey := "sk-test-client-compat-e2e-local-key"
	addr := freeLoopbackAddr(t)
	config := fmt.Sprintf("server:\n  listen: %q\n  api_keys:\n    - name: default\n      key: %q\nproviders:\n  ollama:\n    base_url: %q\ntiers:\n  demo:\n    models: [ollama/test-model]\nmodels:\n  ollama/test-model:\n    capabilities: [tools]\nrouter:\n  auto: [demo]\n", addr, localKey, upstream.URL+"/v1")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil { // #nosec G703 -- configPath is inside t.TempDir.
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), spillwayBinary, "serve", "-config", configPath) //nolint:gosec // TestMain built this executable and configPath is inside t.TempDir.
	command.Dir = workDir
	var output synchronizedBuffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal("start spillway serve:", err)
	}
	wait := make(chan error, 1)
	waitDone := make(chan struct{})
	go func() {
		wait <- command.Wait()
		close(waitDone)
	}()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		<-waitDone
	})
	baseURL := "http://" + addr
	waitForBinaryServer(t, baseURL, wait)

	responses := requestBinaryAPI(t, baseURL, http.MethodPost, "/v1/responses", `{"model":"auto","input":[{"type":"message","id":"msg_codex_1","role":"user","content":[{"type":"input_text","text":"Say hello"}]}],"store":false,"include":["reasoning.encrypted_content"],"client_metadata":{"app":"codex"},"prompt_cache_key":"codex-session","service_tier":"priority","parallel_tool_calls":true,"reasoning":{"effort":"medium","summary":"auto"},"text":{"verbosity":"medium"},"tool_choice":"auto"}`, localKey, http.StatusOK, `"object":"response"`)
	if !strings.Contains(responses, "Hello from the fake provider.") {
		t.Fatalf("Responses API response = %s", responses)
	}
	messages := requestBinaryAPI(t, baseURL, http.MethodPost, "/v1/messages", `{"model":"ollama/test-model","max_tokens":32,"messages":[{"role":"user","content":"Say hello"}]}`, localKey, http.StatusOK, `"type":"message"`)
	if !strings.Contains(messages, "Hello from the fake provider.") {
		t.Fatalf("Anthropic Messages response = %s", messages)
	}
	responsesTool := requestBinaryAPI(t, baseURL, http.MethodPost, "/v1/responses", `{"model":"auto","input":"Use a tool","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]}`, localKey, http.StatusOK, `"type":"function_call"`)
	if !strings.Contains(responsesTool, `"name":"lookup"`) {
		t.Fatalf("Responses tool response = %s", responsesTool)
	}
	responsesNamespacedTool := requestBinaryAPI(t, baseURL, http.MethodPost, "/v1/responses", `{"model":"auto","input":"Use a tool","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]},{"type":"web_search"}]}`, localKey, http.StatusOK, `"namespace":"functions"`)
	if !strings.Contains(responsesNamespacedTool, `"name":"lookup"`) {
		t.Fatalf("Responses namespaced tool response = %s", responsesNamespacedTool)
	}
	messagesTool := requestBinaryAPI(t, baseURL, http.MethodPost, "/v1/messages", `{"model":"ollama/test-model","max_tokens":32,"messages":[{"role":"user","content":"Use a tool"}],"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{}}}]}`, localKey, http.StatusOK, `"type":"tool_use"`)
	if !strings.Contains(messagesTool, `"name":"lookup"`) {
		t.Fatalf("Anthropic tool response = %s", messagesTool)
	}
	responsesStream := requestBinaryAPI(t, baseURL, http.MethodPost, "/v1/responses", `{"model":"auto","input":"Stream hello","stream":true}`, localKey, http.StatusOK, "event: response.completed")
	if !strings.Contains(responsesStream, "Stream hello.") {
		t.Fatalf("Responses stream = %s", responsesStream)
	}
	messagesStream := requestBinaryAPI(t, baseURL, http.MethodPost, "/v1/messages", `{"model":"ollama/test-model","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"Stream hello"}]}`, localKey, http.StatusOK, "event: message_stop")
	if !strings.Contains(messagesStream, "Stream hello.") {
		t.Fatalf("Anthropic Messages stream = %s", messagesStream)
	}
	if got := upstreamRequests.Load(); got != 7 {
		t.Fatalf("fake provider requests = %d, want 7", got)
	}
}

func TestBinary_SetupCodingClientsWritesPrivateGlobalConfig(t *testing.T) {
	for _, client := range []string{"opencode", "claude", "codex"} {
		t.Run(client, func(t *testing.T) {
			workDir := t.TempDir()
			home := filepath.Join(workDir, "home")
			configPath := filepath.Join(workDir, "config.yaml")
			localKey := "sk-test-" + client + "-setup-binary-key"
			configText := "server:\n  listen: 127.0.0.1:8080\n  api_keys:\n    - name: default\n      key: " + localKey + "\nproviders: {}\n"
			if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil { // #nosec G703 -- configPath is inside t.TempDir.
				t.Fatal(err)
			}
			environment := setEnv(os.Environ(), "USERPROFILE", home)
			environment = setEnv(environment, "HOME", home)
			environment = setEnv(environment, "CODEX_HOME", filepath.Join(home, ".codex"))
			output, err := runBinaryWithEnv(t, workDir, environment, "setup", client, "-config", configPath)
			if err != nil {
				t.Fatalf("spillway setup %s: %v\n%s", client, err, output)
			}
			if strings.Contains(output, localKey) {
				t.Fatal("setup output revealed the local key")
			}
			var settingsPath string
			switch client {
			case "opencode":
				settingsPath = filepath.Join(home, ".config", "opencode", "opencode.json")
			case "claude":
				settingsPath = filepath.Join(home, ".claude", "settings.json")
			case "codex":
				settingsPath = filepath.Join(home, ".codex", "spillway.config.toml")
			}
			settings, err := os.ReadFile(settingsPath) // #nosec G304 -- settingsPath is inside t.TempDir.
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(settings), localKey) {
				t.Fatalf("%s settings contain the local key", client)
			}
			if client == "opencode" {
				if !strings.Contains(string(settings), "spillway/auto") || !strings.Contains(string(settings), "{file:./spillway.key}") {
					t.Fatalf("OpenCode settings = %s", settings)
				}
				keyFile, err := os.ReadFile(filepath.Join(filepath.Dir(settingsPath), "spillway.key")) // #nosec G304 -- adjacent private file is inside t.TempDir.
				if err != nil || string(keyFile) != localKey {
					t.Fatalf("OpenCode key file = %q, err=%v", keyFile, err)
				}
			} else if client == "codex" && !strings.Contains(string(settings), "args = [\"key\", \"-config\", ") {
				t.Fatalf("Codex profile does not invoke the Spillway key command: %s", settings)
			}
		})
	}
}

func setEnv(environment []string, key, value string) []string {
	prefix := key + "="
	updated := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			updated = append(updated, entry)
		}
	}
	return append(updated, prefix+value)
}
