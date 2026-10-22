package cli_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	for name, address := range map[string]string{
		"loopback":       listen,
		"all interfaces": "0.0.0.0:" + listen[strings.LastIndex(listen, ":")+1:],
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
