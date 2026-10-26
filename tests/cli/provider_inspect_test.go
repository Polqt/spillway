package cli_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/cli"
)

const inspectConfig = `server:
  listen: "127.0.0.1:8080"
  api_keys:
    - name: default
      key: "sk-test-local-key-canary"
logging:
  level: info
providers:
  groq:
    api_key: "${GROQ_CUSTOM_KEY}"
    free_tier: true
    base_url: "%s"
tiers:
  free:
    models:
      - groq/llama-3.3-70b-versatile
`

func TestProviderList_ShowsVariablePresenceAttestationBaseURLAndTiers(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	lookup := func(name string) (string, bool) {
		if name == "GROQ_CUSTOM_KEY" {
			return "gsk_test-provider-secret-canary", true
		}
		return "", false
	}
	code := cli.RunWithOptions(t.Context(), []string{"provider", "list", "-config", writeInspectConfig(t, "https://example.invalid/v1")}, nil, &stdout, &stderr, cli.Options{LookupEnv: lookup})
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	for _, want := range []string{"groq", "GROQ_CUSTOM_KEY", "set", "attested", "custom", "free"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output %q does not contain %q", stdout.String(), want)
		}
	}
	for _, secret := range []string{"gsk_test-provider-secret-canary", "sk-test-local-key-canary", "gsk_test", "sha256"} {
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Errorf("output contains sensitive value %q", secret)
		}
	}
}

func TestProviderShow_ShowsReferencesWithoutValues(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	lookup := func(name string) (string, bool) { return "", false }
	options := cli.Options{LookupEnv: lookup, GOOS: "windows", PersistentEnv: func(name string) (string, bool) {
		if name == "GROQ_CUSTOM_KEY" {
			return "user", true
		}
		return "", false
	}}
	code := cli.RunWithOptions(t.Context(), []string{"provider", "show", "groq", "-config", writeInspectConfig(t, "https://example.invalid/v1")}, nil, &stdout, &stderr, options)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	for _, want := range []string{"groq (preset)", "${GROQ_CUSTOM_KEY}", "set for your Windows user", "attested", "tier"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output %q does not contain %q", stdout.String(), want)
		}
	}
	for _, secret := range []string{"sk-test-local-key-canary", "gsk_test-provider-secret-canary"} {
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Errorf("output contains secret %q", secret)
		}
	}
}

func TestProviderShow_NamedInstanceHidesSensitiveLabelsAndEndpointDetails(t *testing.T) {
	t.Parallel()
	base := `server: {api_keys: [sk-test-local-key-canary]}
providers:
  groq-staging:
    type: groq
    api_key: "${GROQ_STAGING_API_KEY}"
    base_url: "https://upstream.example.invalid/tenant/private-path-canary"
    quota_domains:
      organization: team-internal-canary
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := cli.RunWithOptions(t.Context(), []string{"provider", "show", "groq-staging", "-config", path}, nil, &stdout, &stderr, cli.Options{LookupEnv: func(name string) (string, bool) { return "sk-test-hidden", name == "GROQ_STAGING_API_KEY" }})
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	for _, want := range []string{"groq-staging (instance; type groq)", "${GROQ_STAGING_API_KEY}", "custom endpoint configured", "organization (labels hidden)"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output %q lacks %q", stdout.String(), want)
		}
	}
	for _, hidden := range []string{"private-path-canary", "team-internal-canary", "sk-test-hidden", "sk-test-local-key-canary"} {
		if strings.Contains(stdout.String()+stderr.String(), hidden) {
			t.Errorf("output leaked %q", hidden)
		}
	}
}

func TestProviderShow_UnknownProviderExits1WithConfiguredNames(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := cli.RunWithOptions(t.Context(), []string{"provider", "show", "gemini", "-config", writeInspectConfig(t, "")}, nil, &stdout, &stderr, cli.Options{LookupEnv: noEnv})
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "configured providers: groq") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestProviderInspect_MakesNoNetworkCalls(t *testing.T) {
	t.Parallel()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	code := cli.RunWithOptions(t.Context(), []string{"provider", "list", "-config", writeInspectConfig(t, server.URL)}, nil, &stdout, &stderr, cli.Options{LookupEnv: noEnv})
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

func TestProviderList_WorksOnEveryHistoricalFixture(t *testing.T) {
	t.Parallel()
	fixtures, err := filepath.Glob(filepath.Join("..", "config", "testdata", "compat", "v*", "config*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no historical config fixtures found")
	}
	for _, fixture := range fixtures {
		t.Run(filepath.Base(filepath.Dir(fixture))+"/"+filepath.Base(fixture), func(t *testing.T) {
			contents, err := os.ReadFile(fixture) // #nosec G304 -- fixture paths come from a fixed repository testdata glob.
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(contents), "mock/") {
				t.Skip("historical fixture depends on the removed shipped mock provider")
			}
			var stdout, stderr bytes.Buffer
			lookup := func(string) (string, bool) { return "test-placeholder", true }
			code := cli.RunWithOptions(t.Context(), []string{"provider", "list", "-config", fixture}, nil, &stdout, &stderr, cli.Options{LookupEnv: lookup})
			if code != 0 {
				t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
			}
		})
	}
}

func TestManage_SingleDashConfigOnNestedCommands(t *testing.T) {
	t.Parallel()
	for _, command := range [][]string{{"provider", "list"}, {"provider", "show", "groq"}} {
		var stdout, stderr bytes.Buffer
		args := append(append([]string(nil), command...), "-config", writeInspectConfig(t, ""))
		if code := cli.RunWithOptions(t.Context(), args, nil, &stdout, &stderr, cli.Options{LookupEnv: noEnv}); code != 0 {
			t.Errorf("%v exit = %d; stderr = %q", command, code, stderr.String())
		}
	}
}

func TestManage_ProviderWithoutSubcommandIsUsageError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := cli.Run(t.Context(), []string{"provider"}, nil, &stdout, &stderr, noEnv); code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "usage: spillway provider") || !strings.Contains(stdout.String(), "show <instance>") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestManage_HelpShowsDiagnoseAndManageGroups(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := cli.Run(t.Context(), []string{"--help"}, nil, &stdout, &stderr, noEnv); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"Diagnose", "Manage", "provider", "Quick start:"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("help %q does not contain %q", stdout.String(), want)
		}
	}
}

func writeInspectConfig(t *testing.T, baseURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(strings.ReplaceAll(inspectConfig, "%s", baseURL))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
