package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
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

func TestBinary_MockSpilloverMatchesDocs(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(root, "examples", "config.mock.yaml")) // #nosec G304 -- root is the repository root found from this test's source directory.
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
	go func() { wait <- command.Wait() }()
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			<-wait
		}
	})
	baseURL := "http://" + addr
	waitForBinaryServer(t, baseURL, wait)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL+"/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"spillover demo"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer sk-test-spillway-mock")
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close mock response: %v", err)
		}
	}()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("x-spillway-route") != "auto; tier demo; candidate 3/3" || !bytes.Contains(body, []byte("spillover demo")) {
		t.Fatalf("mock response = %d route %q body %s", response.StatusCode, response.Header.Get("x-spillway-route"), body)
	}
}

func TestBinary_VersionShowsCommit(t *testing.T) {
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
	commit := exec.CommandContext(t.Context(), "git", "rev-parse", "--short", "HEAD") //nolint:gosec // The git arguments are fixed.
	commit.Dir = root
	shortSHA, gitErr := commit.Output()
	if gitErr == nil {
		if !strings.Contains(line, " commit "+strings.TrimSpace(string(shortSHA))+" ") {
			t.Errorf("version output = %q, want current commit %q", line, strings.TrimSpace(string(shortSHA)))
		}
	}
	format := `^spillway \S+`
	if gitErr == nil {
		format += ` commit [0-9a-f]+ built \d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ(?: \[modified\])?`
	}
	format += ` ` + regexp.QuoteMeta(runtime.Version()+" "+runtime.GOOS+"/"+runtime.GOARCH) + `$`
	if !regexp.MustCompile(format).MatchString(line) {
		t.Fatalf("version output = %q, want ticket 06 version field format", line)
	}
}

func TestBinary_LdflagsVersionIsPrinted(t *testing.T) {
	output, err := exec.CommandContext(t.Context(), spillwayBinary, "version").CombinedOutput() //nolint:gosec // TestMain built this executable from the current source tree.
	if err != nil {
		t.Fatalf("spillway version: %v\n%s", err, output)
	}
	if !strings.HasPrefix(string(output), "spillway "+binaryVersion+" ") {
		t.Errorf("version output = %q, want prefix %q", output, "spillway "+binaryVersion+" ")
	}
}

func TestBinary_InitServeQuickstart(t *testing.T) {
	workDir := t.TempDir()
	configPath := filepath.Join(workDir, "config.yaml")
	initOutput, initErr := runBinary(t, workDir, "init", "-yes", "-providers", "mock", "-store", "-config", configPath)
	if initErr != nil {
		t.Fatalf("spillway init: %v\n%s", initErr, initOutput)
	}
	for _, want := range []string{"1. Provider environment variables", "2. Client apps", "key entry `default`", "model `auto`", "docs/clients.md", "3. Dashboard", "key entry `dashboard`", "http://127.0.0.1:8080/dashboard"} {
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
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal("start spillway serve:", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			<-wait
		}
	})
	baseURL := "http://" + addr
	waitForBinaryServer(t, baseURL, wait)
	for _, want := range []string{"msg=\"next steps\"", "dashboard_key_configured=true", "store_enabled=true", "api=" + baseURL + "/v1", "models=" + baseURL + "/v1/models", "dashboard=" + baseURL + "/dashboard"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("serve startup output = %q, want %q", stderr.String(), want)
		}
	}

	requestBinaryAPI(t, baseURL, http.MethodGet, "/v1/models", "", appKey, http.StatusOK, "")
	requestBinaryAPI(t, baseURL, http.MethodPost, "/v1/chat/completions", `{"model":"auto","messages":[{"role":"user","content":"binary quickstart"}]}`, appKey, http.StatusOK, "binary quickstart")
	stream := requestBinaryAPI(t, baseURL, http.MethodPost, "/v1/chat/completions", `{"model":"auto","stream":true,"messages":[{"role":"user","content":"binary stream"}]}`, appKey, http.StatusOK, "[DONE]")
	if !strings.HasSuffix(strings.TrimSpace(stream), "data: [DONE]") {
		t.Errorf("stream response ends with %q, want data: [DONE]", stream)
	}
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

func runBinary(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	command := exec.CommandContext(t.Context(), spillwayBinary, args...) //nolint:gosec // TestMain built this executable; callers use fixed CLI arguments.
	command.Dir = dir
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
	request.Header.Set("Authorization", "Bearer "+key)
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
