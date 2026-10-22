package cli_test

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Polqt/spillway/internal/cli"
)

const dashboardConfig = `server:
  api_keys:
    - name: default
      key: sk-test-local-default
    - name: dashboard
      key: sk-test-local-dashboard
      dashboard: true
providers:
  mock: {}
`

func fakeDashboardAPI(t *testing.T, usage string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test-local-dashboard" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid API key.","code":"invalid_api_key"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/gateway":
			_, _ = w.Write([]byte(`{"version":"v9.9.9","dashboard_assets":"embedded"}`))
		case "/api/providers":
			_, _ = w.Write([]byte(`{"providers":[{"name":"groq","configured":true,"credential_required":true,"credential_present":false,"signals":[]},{"name":"mock","configured":true,"signals":[]}]}`))
		case "/api/budget":
			_, _ = w.Write([]byte(`{"daily":{"limit_micro_usd":1000000,"spent_micro_usd":250000,"remaining_micro_usd":750000,"reset_at":"2026-10-23T00:00:00Z"}}`))
		case "/api/usage":
			if r.URL.Query().Get("group") != "provider" || r.URL.Query().Get("since") == "" {
				t.Errorf("usage query = %q, want provider group with a since bound", r.URL.RawQuery)
			}
			if usage == "" {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":{"message":"Usage storage is not configured.","code":"store_disabled"}}`))
				return
			}
			_, _ = w.Write([]byte(usage))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestRun_DashboardPrintsSnapshotWhenNotATerminal(t *testing.T) {
	t.Parallel()
	server := fakeDashboardAPI(t, `{"data":[{"key":"mock","requests":42,"errors":1,"cache_hits":3,"prompt_tokens":12000,"completion_tokens":800,"cost_micro_usd":250000}]}`)
	var stdout, stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"dashboard", "-config", writeConfig(t, dashboardConfig), "-endpoint", server.URL}, nil, &stdout, &stderr, noEnv)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %q", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"dashboard · v9.9.9", "groq", "key missing", "mock", "ready", "42", "12.0k / 800", "$0.2500", "daily", "$0.2500 / $1.0000"} {
		if !strings.Contains(out, want) {
			t.Errorf("snapshot missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out+stderr.String(), "sk-test-local") {
		t.Errorf("output leaks a local key:\n%s\n%s", out, stderr.String())
	}
}

func TestRun_DashboardExplainsDisabledUsageHistory(t *testing.T) {
	t.Parallel()
	server := fakeDashboardAPI(t, "")
	var stdout, stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"dashboard", "-config", writeConfig(t, dashboardConfig), "-endpoint", server.URL}, nil, &stdout, &stderr, noEnv)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage history is off") {
		t.Errorf("stdout = %q, want disabled usage notice", stdout.String())
	}
}

func TestRun_DashboardReportsUnreachableGatewayWithoutKey(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.NotFoundHandler())
	endpoint := server.URL
	server.Close()
	var stdout, stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"dashboard", "-config", writeConfig(t, dashboardConfig), "-endpoint", endpoint}, nil, &stdout, &stderr, noEnv)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not answering") || !strings.Contains(stderr.String(), "spillway serve") {
		t.Errorf("stderr = %q, want unreachable guidance", stderr.String())
	}
	if strings.Contains(stderr.String(), "sk-test-local") {
		t.Errorf("stderr leaks a local key: %q", stderr.String())
	}
}

func TestRun_DashboardRejectsWrongKey(t *testing.T) {
	t.Parallel()
	server := fakeDashboardAPI(t, "")
	config := strings.Replace(dashboardConfig, "sk-test-local-dashboard", "sk-test-local-stale", 1)
	var stdout, stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"dashboard", "-config", writeConfig(t, config), "-endpoint", server.URL}, nil, &stdout, &stderr, noEnv)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "rejected the dashboard key") || strings.Contains(stderr.String(), "sk-test-local") {
		t.Errorf("stderr = %q, want key rejection without the key", stderr.String())
	}
}

func TestRun_DashboardNeedsDashboardKey(t *testing.T) {
	t.Parallel()
	config := strings.Replace(dashboardConfig, "      dashboard: true\n", "", 1)
	var stdout, stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"dashboard", "-config", writeConfig(t, config)}, nil, &stdout, &stderr, noEnv)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "dashboard: true") {
		t.Errorf("stderr = %q, want guidance to add a dashboard key", stderr.String())
	}
}

func TestRun_DashboardValidatesFlags(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"-endpoint", "ftp://127.0.0.1"},
		{"-interval", "10ms"},
	} {
		var stdout, stderr bytes.Buffer
		code := cli.Run(t.Context(), append([]string{"dashboard", "-config", writeConfig(t, dashboardConfig)}, args...), nil, &stdout, &stderr, noEnv)
		if code != 2 {
			t.Errorf("args %v: exit code = %d, want 2; stderr: %q", args, code, stderr.String())
		}
	}
}

// Security property: without -endpoint the key goes only to the gateway the config describes.
func TestRun_DashboardDefaultsToConfiguredListenAddress(t *testing.T) {
	t.Parallel()
	server := fakeDashboardAPI(t, "")
	listen := strings.TrimPrefix(server.URL, "http://")
	port := listen[strings.LastIndex(listen, ":")+1:]
	for name, address := range map[string]string{
		"loopback":       listen,
		"all interfaces": "0.0.0.0:" + port,
		"localhost":      "localhost:" + port,
	} {
		t.Run(name, func(t *testing.T) {
			config := strings.Replace(dashboardConfig, "server:\n", "server:\n  listen: \""+address+"\"\n", 1)
			var stdout, stderr bytes.Buffer

			code := cli.Run(t.Context(), []string{"dashboard", "-config", writeConfig(t, config)}, nil, &stdout, &stderr, noEnv)

			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %q", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "v9.9.9") {
				t.Errorf("stdout = %q, want data from the configured gateway", stdout.String())
			}
		})
	}
}

// Security property: the dashboard key never crosses a network in cleartext.
func TestRun_DashboardRefusesPlainHTTPToRemoteHost(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"dashboard", "-config", writeConfig(t, dashboardConfig), "-endpoint", "http://192.0.2.10:8080"}, nil, &stdout, &stderr, noEnv)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "refusing to send the dashboard key over plain http") || strings.Contains(stderr.String(), "sk-test-local") {
		t.Errorf("stderr = %q, want refusal without the key", stderr.String())
	}
}

// Security property: gateway strings cannot carry terminal escape sequences.
func TestRun_DashboardNeutralizesControlCharacters(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/gateway":
			_, _ = w.Write([]byte(`{"version":"v1\u001b]52;c;Zm9v\u0007"}`))
		case "/api/providers":
			_, _ = w.Write([]byte(`{"providers":[{"name":"evil\u001b[2Jname","signals":[]}]}`))
		case "/api/budget":
			_, _ = w.Write([]byte(`{}`))
		case "/api/usage":
			_, _ = w.Write([]byte(`{"data":[{"key":"row\u009bkey","requests":1}]}`))
		}
	}))
	t.Cleanup(server.Close)
	var stdout, stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"dashboard", "-config", writeConfig(t, dashboardConfig), "-endpoint", server.URL}, nil, &stdout, &stderr, noEnv)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %q", code, stderr.String())
	}
	for _, want := range []string{"v1?]52;c;Zm9v?", "evil?[2Jname", "row?key"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("snapshot missing neutralized %q:\n%s", want, stdout.String())
		}
	}
}

func TestRun_DashboardMapsIPv6WildcardToIPv6Loopback(t *testing.T) {
	t.Parallel()
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "tcp", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	ipv6 := httptest.NewUnstartedServer(fakeDashboardAPI(t, "").Config.Handler)
	ipv6.Listener = listener
	ipv6.Start()
	t.Cleanup(ipv6.Close)
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	config := strings.Replace(dashboardConfig, "server:\n", "server:\n  listen: \"[::]:"+port+"\"\n", 1)
	var stdout, stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"dashboard", "-config", writeConfig(t, config)}, nil, &stdout, &stderr, noEnv)

	if code != 0 || !strings.Contains(stdout.String(), "v9.9.9") {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q; want data from [::1]", code, stdout.String(), stderr.String())
	}
}

func TestRun_DashboardRefusesNonLoopbackListenAddress(t *testing.T) {
	t.Parallel()
	config := strings.Replace(dashboardConfig, "server:\n", "server:\n  listen: \"192.0.2.10:8080\"\n", 1)
	var stdout, stderr bytes.Buffer

	code := cli.Run(t.Context(), []string{"dashboard", "-config", writeConfig(t, config)}, nil, &stdout, &stderr, noEnv)

	if code != 2 || !strings.Contains(stderr.String(), "refusing to send the dashboard key over plain http") {
		t.Errorf("exit code = %d, stderr = %q; want plain http refusal", code, stderr.String())
	}
}

// Security property: proxy environment variables never receive the dashboard key.
// Not parallel: it sets process-wide proxy variables.
func TestRun_DashboardIgnoresProxyEnvironment(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer

	_ = cli.Run(ctx, []string{"dashboard", "-config", writeConfig(t, dashboardConfig), "-endpoint", "https://192.0.2.10"}, nil, &stdout, &stderr, noEnv)

	if proxied.Load() != 0 {
		t.Errorf("proxy received %d requests, want 0", proxied.Load())
	}
}
