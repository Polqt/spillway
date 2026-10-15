package catalog_test

import (
	"bytes"
	"embed"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Polqt/spillway/internal/catalog"
	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/redact"
	"go.yaml.in/yaml/v3"
)

//go:embed testdata/*.json
var testdata embed.FS

func TestDiscover_ProvidesSuggestionsAndDiff(t *testing.T) {
	fixtures := map[string][]byte{
		"openrouter": mustFixture(t, "openrouter-models.json"),
		"groq":       mustFixture(t, "groq-models.json"),
		"ollama":     mustFixture(t, "ollama-show.json"),
	}
	var seen = map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")
		seen[key] = r.URL.Path
		if r.URL.Path == "/api/show" {
			_, _ = w.Write(fixtures["ollama"])
			return
		}
		name := "groq"
		if key == "Bearer sk-test-openrouter" {
			name = "openrouter"
		}
		_, _ = w.Write(fixtures[name])
	}))
	defer server.Close()
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"openrouter": {APIKey: redact.NewSecret("sk-test-openrouter"), BaseURL: server.URL, Timeout: time.Second},
			"groq":       {APIKey: redact.NewSecret("sk-test-groq"), BaseURL: server.URL, Timeout: time.Second},
			"ollama":     {BaseURL: server.URL, Timeout: time.Second},
			"gemini":     {APIKey: redact.NewSecret("sk-test-gemini"), Timeout: time.Second},
			"mock":       {Timeout: time.Second},
		},
		Tiers:   map[string]config.Tier{"free": {Models: []string{"openrouter/vendor/vision:free", "groq/llama-3.3-70b-versatile", "ollama/llama3.2:3b", "gemini/gemini-2.5-flash", "mock/echo"}}},
		Models:  map[string]config.ModelDeclaration{"openrouter/vendor/vision:free": {Capabilities: []string{"tools"}}},
		Pricing: map[string]config.Price{"openrouter/vendor/vision:free": {InputMicroUSDPerMillion: 500_000, OutputMicroUSDPerMillion: 1_000_000}},
	}
	var out bytes.Buffer
	err := catalog.Discover(t.Context(), cfg, &out, catalog.Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"models:", "pricing:", "input_usd_per_mtok: 0.02", "output_usd_per_mtok: 2", "input_usd_per_mtok: 0 # source: Ollama /api/show", "declared input_usd_per_mtok: 0.5", "context_window: 131072", "catalog adds", "vision", "Gemini skipped", "mock skipped", "WARNING: openrouter/vendor/vision:free has non-zero catalog price"} {
		if !strings.Contains(text, want) {
			t.Errorf("catalog output missing %q:\n%s", want, text)
		}
	}
	var decoded any
	if err := yaml.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("output is not YAML: %v", err)
	}
	for _, secret := range []string{"sk-test-openrouter", "sk-test-groq", "sk-test-gemini"} {
		if strings.Contains(text, secret) {
			t.Errorf("output leaked %q", secret)
		}
	}
	if seen["Bearer sk-test-openrouter"] != "/models" || seen["Bearer sk-test-groq"] != "/models" || seen[""] != "/api/show" {
		t.Errorf("provider authorization mismatch: %#v", seen)
	}
}

func TestDiscover_BoundsResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(bytes.Repeat([]byte("x"), 8<<20+1)) }))
	defer server.Close()
	cfg := config.Config{Providers: map[string]config.Provider{"groq": {APIKey: redact.NewSecret("sk-test-groq"), BaseURL: server.URL, Timeout: time.Second}}, Tiers: map[string]config.Tier{"free": {Models: []string{"groq/model"}}}}
	err := catalog.Discover(t.Context(), cfg, &bytes.Buffer{}, catalog.Options{HTTPClient: server.Client()})
	if err == nil || !strings.Contains(err.Error(), "exceeds 8 MiB") {
		t.Fatalf("Discover error = %v, want response bound", err)
	}
	if strings.Contains(err.Error(), "sk-test-groq") {
		t.Fatalf("error leaked key: %v", err)
	}
}

func TestDiscover_UsesProviderTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}
		cfg := config.Config{
			Providers: map[string]config.Provider{"groq": {APIKey: redact.NewSecret("sk-test-groq"), BaseURL: "https://groq.example/api/v1", Timeout: time.Second}},
			Tiers:     map[string]config.Tier{"paid": {AllowSpend: true, Models: []string{"groq/model"}}},
		}
		err := catalog.Discover(t.Context(), cfg, &bytes.Buffer{}, catalog.Options{HTTPClient: client})
		if err == nil || !strings.Contains(err.Error(), "provider catalog request failed") {
			t.Fatalf("Discover error = %v, want sanitized timeout error", err)
		}
		if strings.Contains(err.Error(), "sk-test-groq") {
			t.Fatalf("Discover error leaked provider key: %v", err)
		}
	})
}

func TestDiscover_SkipsProvidersWithoutCatalogSupport(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, nil
	})}
	cfg := config.Config{
		Providers: map[string]config.Provider{"gemini": {APIKey: redact.NewSecret("sk-test-gemini")}, "mock": {}},
		Tiers:     map[string]config.Tier{"free": {Models: []string{"gemini/model", "mock/echo"}}},
	}
	var out bytes.Buffer
	if err := catalog.Discover(t.Context(), cfg, &out, catalog.Options{HTTPClient: client}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("HTTP calls = %d, want 0", calls)
	}
	for _, note := range []string{"Gemini skipped", "mock skipped"} {
		if !strings.Contains(out.String(), note) {
			t.Errorf("output missing skip note %q: %s", note, out.String())
		}
	}
}

func TestDiscover_DoesNotFollowRedirectToUnconfiguredHost(t *testing.T) {
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls++ }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test-groq-redirect" {
			t.Errorf("origin authorization = %q", got)
		}
		http.Redirect(w, r, target.URL+"/models", http.StatusFound)
	}))
	defer origin.Close()
	cfg := config.Config{
		Providers: map[string]config.Provider{"groq": {APIKey: redact.NewSecret("sk-test-groq-redirect"), BaseURL: origin.URL, Timeout: time.Second}},
		Tiers:     map[string]config.Tier{"paid": {AllowSpend: true, Models: []string{"groq/model"}}},
	}
	err := catalog.Discover(t.Context(), cfg, &bytes.Buffer{}, catalog.Options{HTTPClient: origin.Client()})
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("Discover error = %v, want redirect response error", err)
	}
	if targetCalls != 0 {
		t.Errorf("unconfigured redirect target calls = %d, want 0", targetCalls)
	}
	if strings.Contains(err.Error(), "sk-test-groq-redirect") {
		t.Errorf("Discover error leaked key: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mustFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := testdata.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
