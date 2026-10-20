package cli_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/cli"
)

func TestDoctor_ListenProbeRecognizesExpectedUnauthenticatedResponses(t *testing.T) {
	requests := make([]string, 0, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		requests = append(requests, r.Method+" "+r.URL.Path+" Host="+r.Host)
		switch r.URL.Path {
		case "/healthz":
			if r.Host == "doctor.invalid" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"code":"host_not_allowed"}}`))
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("ok"))
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","code":"invalid_api_key"}}`))
		}
	}))
	defer server.Close()
	listen := strings.TrimPrefix(server.URL, "http://")
	path := writeDoctorConfigWithListen(t, listen)
	var stdout, stderr strings.Builder
	options := cli.Options{LookupEnv: doctorHealthyEnv}
	if code := cli.RunWithOptions(t.Context(), []string{"doctor", "-config", path}, nil, &stdout, &stderr, options); code != 0 {
		t.Fatalf("exit = %d; stdout = %q; stderr = %q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "answers like Spillway") || !strings.Contains(stdout.String(), "startup summary") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if len(requests) != 3 {
		t.Fatalf("requests = %v, want health, models and invalid-host health probes", requests)
	}
}

func TestDoctor_ListenProbeDoesNotFollowRedirectOrPrintBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/secret-canary", http.StatusFound)
	}))
	defer server.Close()
	path := writeDoctorConfigWithListen(t, strings.TrimPrefix(server.URL, "http://"))
	var stdout, stderr strings.Builder
	if code := cli.RunWithOptions(t.Context(), []string{"doctor", "-config", path}, nil, &stdout, &stderr, cli.Options{LookupEnv: doctorHealthyEnv}); code != 1 {
		t.Fatalf("exit = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "not Spillway (HTTP 302") || strings.Contains(stdout.String(), "secret-canary") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestDoctor_NoProbeMakesNoListenRequests(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	path := writeDoctorConfigWithListen(t, strings.TrimPrefix(server.URL, "http://"))
	var stdout, stderr strings.Builder
	if code := cli.RunWithOptions(t.Context(), []string{"doctor", "-config", path, "--no-probe"}, nil, &stdout, &stderr, cli.Options{LookupEnv: doctorHealthyEnv}); code != 0 {
		t.Fatalf("exit = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if requests != 0 || !strings.Contains(stdout.String(), "probe skipped (--no-probe)") {
		t.Fatalf("requests = %d; stdout = %q", requests, stdout.String())
	}
}

func TestDoctor_PortGuidanceIsLoopbackOnlyAndOSSpecific(t *testing.T) {
	tests := []struct{ goos string }{
		{"windows"},
		{"darwin"},
		{"linux"},
	}
	for _, test := range tests {
		t.Run(test.goos, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte("private response body"))
			}))
			defer server.Close()
			listen := strings.TrimPrefix(server.URL, "http://")
			_, port, err := net.SplitHostPort(listen)
			if err != nil {
				t.Fatal(err)
			}
			path := writeDoctorConfigWithListen(t, listen)
			var stdout, stderr strings.Builder
			options := cli.Options{GOOS: test.goos, LookupEnv: doctorHealthyEnv}
			if code := cli.RunWithOptions(t.Context(), []string{"doctor", "-config", path}, nil, &stdout, &stderr, options); code != 1 {
				t.Fatalf("exit = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			text := stdout.String()
			want := "ss -ltnp 'sport = :" + port + "'"
			switch test.goos {
			case "windows":
				want = "Get-NetTCPConnection -LocalPort " + port + " -State Listen"
			case "darwin":
				want = "lsof -nP -iTCP:" + port + " -sTCP:LISTEN"
			}
			if !strings.Contains(text, want) || !strings.Contains(text, "127.0.0.1:8787") || strings.Contains(text, "0.0.0.0") || strings.Contains(text, "private response body") {
				t.Errorf("guidance = %q", text)
			}
		})
	}
}

func writeDoctorConfigWithListen(t *testing.T, listen string) string {
	t.Helper()
	path := writeDoctorConfig(t, "", "sk-test-local-doctor-key-0123456789ABCDEF", "")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "127.0.0.1:0", listen, 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(path)
}

func doctorHealthyEnv(name string) (string, bool) {
	if name == "GROQ_API_KEY" {
		return "gsk_test-doctor-listen-key-long-enough", true
	}
	return "", false
}
