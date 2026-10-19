package api_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Polqt/spillway/apps/spillway/web"
	"github.com/Polqt/spillway/internal/api"
	"github.com/Polqt/spillway/internal/budget"
	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/providers/mock"
	"github.com/Polqt/spillway/internal/quota"
	"github.com/Polqt/spillway/internal/redact"
	"github.com/Polqt/spillway/internal/store"
)

const (
	dashboardKey = "sk-test-dashboard-secret"
	chatOnlyKey  = "sk-test-chat-only-secret"
)

func dashboardEnv(t *testing.T) (http.Handler, *store.Store, time.Time) {
	t.Helper()
	now := time.Date(2026, time.October, 15, 12, 0, 0, 0, time.UTC)
	db, err := store.Open(t.Context(), store.Options{Path: t.TempDir() + "/usage.db", Retention: 24 * time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	provider, err := mock.New(mock.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	set, err := providers.NewSet(provider)
	if err != nil {
		t.Fatal(err)
	}
	ref := providers.ModelRef{Provider: "mock", Model: "echo"}
	tracker, err := quota.New(quota.Options{Now: func() time.Time { return now }, Candidates: []providers.ModelRef{ref}})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := budget.NewGate(t.Context(), budget.GateOptions{Limits: config.Budgets{DailyMicroUSD: 5_000_000}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	tracker.Observe(ref, providers.ErrRateLimited, providers.RateLimit{})
	tracker.Observe(ref, providers.ErrUpstreamUnavailable, providers.RateLimit{})
	handler, err := api.NewHandler(api.Options{
		Providers: set,
		LocalKeys: []api.LocalKey{
			{Name: "admin", Key: redact.NewSecret(dashboardKey), Dashboard: true},
			{Name: "app", Key: redact.NewSecret(chatOnlyKey)},
		},
		AllowedHosts: api.LoopbackHosts("127.0.0.1"), Logger: slog.New(slog.DiscardHandler),
		UsageRecorder: db, DashboardStore: db, Dashboard: web.Handler(), Tracker: tracker, BudgetGate: gate,
		CostEstimator: budget.NewPricer(config.Config{}), Now: func() time.Time { return now },
		DashboardPricing: map[string]config.Price{
			"groq/paid-model": {InputMicroUSDPerMillion: 500_000, OutputMicroUSDPerMillion: 1_000_000},
			"mock/echo":       {InputMicroUSDPerMillion: 7_000_000, OutputMicroUSDPerMillion: 9_000_000},
		},
		DashboardProviders: []api.DashboardProvider{
			{Name: "mock"},
			{Name: "groq", CredentialRequired: true, CredentialPresent: true},
			{Name: "ollama"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, db, now
}

func TestDashboardPricing_RequiresDashboardKeyAndReturnsDeclaredPrices(t *testing.T) {
	handler, _, _ := dashboardEnv(t)

	for _, test := range []struct {
		name string
		key  string
		want int
	}{
		{name: "dashboard key", key: dashboardKey, want: http.StatusOK},
		{name: "chat key", key: chatOnlyKey, want: http.StatusForbidden},
		{name: "missing key", want: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := dashboardRequest(t, handler, "/api/pricing", test.key, "127.0.0.1")
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.want, response.Body)
			}
			if test.want == http.StatusOK {
				for _, expected := range []string{`"groq/paid-model"`, `"input_micro_usd_per_million":500000`, `"output_micro_usd_per_million":1000000`} {
					if !strings.Contains(response.Body.String(), expected) {
						t.Errorf("pricing response does not contain %s: %s", expected, response.Body)
					}
				}
				if strings.Contains(response.Body.String(), dashboardKey) || strings.Contains(response.Body.String(), "Authorization") {
					t.Errorf("pricing response contains sensitive data: %s", response.Body)
				}
				if strings.Contains(response.Body.String(), "mock/echo") {
					t.Errorf("pricing response includes a price for the always-free mock provider: %s", response.Body)
				}
				if response.Header().Get("Cache-Control") != "no-store" {
					t.Errorf("Cache-Control = %q, want no-store", response.Header().Get("Cache-Control"))
				}
			}
		})
	}
}

func TestRequestPlayground_UsesMockModelsAndDoesNotPersistPromptOrKey(t *testing.T) {
	handler, db, _ := dashboardEnv(t)
	models := dashboardRequest(t, handler, "/v1/models", dashboardKey, "127.0.0.1")
	if models.Code != http.StatusOK || !strings.Contains(models.Body.String(), `"id":"mock/echo"`) {
		t.Fatalf("models response = %d %s, want configured mock model", models.Code, models.Body)
	}
	const prompt = "playground-private-prompt-c26e"
	body := []byte(`{"model":"mock/echo","messages":[{"role":"user","content":"` + prompt + `"}]}`)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+dashboardKey)
	request.Header.Set("Content-Type", "application/json")
	request.Host = "127.0.0.1"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "You said: "+prompt) {
		t.Fatalf("chat response = %d %s, want successful mock echo", response.Code, response.Body)
	}
	if strings.Contains(response.Body.String(), dashboardKey) {
		t.Fatal("chat response contains local key")
	}
	requests, _, err := db.Recent(t.Context(), store.RecentQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(requests)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), prompt) || strings.Contains(string(encoded), dashboardKey) {
		t.Fatalf("stored playground metadata contains prompt or key: %s", encoded)
	}
}

func TestDashboardPage_HostCheckedButUnauthenticated(t *testing.T) {
	handler, db, now := dashboardEnv(t)
	const shellMarker = "dashboard-shell-private-model-marker"
	if err := db.Record(t.Context(), store.Request{
		At: now, ClientApp: "dashboard-shell-private-client-marker", RequestedModel: shellMarker,
		Route: "dashboard-shell-private-route-marker", Status: http.StatusOK, UsageSource: "reported", CostSource: "declared",
	}); err != nil {
		t.Fatal(err)
	}
	shell := dashboardRequest(t, handler, "/dashboard", "", "127.0.0.1")
	assets := regexp.MustCompile(`(?:src|href)="(/assets/[^"]+)"`).FindAllStringSubmatch(shell.Body.String(), -1)
	paths := []string{"/dashboard"}
	for _, asset := range assets {
		if !slices.Contains(paths, asset[1]) {
			paths = append(paths, asset[1])
		}
	}
	for _, path := range paths {
		test := struct {
			name string
			path string
			host string
			want int
		}{name: path, path: path, host: "127.0.0.1", want: http.StatusOK}
		t.Run(test.name, func(t *testing.T) {
			response := dashboardRequest(t, handler, test.path, "", test.host)
			if response.Code != test.want {
				t.Fatalf("GET %s status = %d, want %d: %s", test.path, response.Code, test.want, response.Body)
			}
			if test.path == "/dashboard" {
				for _, marker := range []string{shellMarker, "dashboard-shell-private-client-marker", "dashboard-shell-private-route-marker"} {
					if strings.Contains(response.Body.String(), marker) {
						t.Errorf("unauthenticated dashboard shell contains stored usage marker %q", marker)
					}
				}
			}
		})
	}
	foreign := dashboardRequest(t, handler, "/dashboard", "", "attacker.example")
	if foreign.Code != http.StatusForbidden {
		t.Errorf("foreign Host status = %d, want 403", foreign.Code)
	}
}

func dashboardRequest(t *testing.T, handler http.Handler, path, key, host string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	req.Host = host
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func TestDashboardAPI_AuthenticationAndHostCheck(t *testing.T) {
	handler, _, _ := dashboardEnv(t)
	for _, test := range []struct {
		name string
		key  string
		host string
		want int
	}{
		{name: "missing key", want: http.StatusUnauthorized},
		{name: "chat only", key: chatOnlyKey, want: http.StatusForbidden},
		{name: "dashboard key", key: dashboardKey, want: http.StatusOK},
		{name: "foreign host precedes auth", host: "attacker.example", want: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, path := range []string{"/api/quotas", "/api/providers"} {
				response := dashboardRequest(t, handler, path, test.key, test.host)
				if response.Code != test.want {
					t.Fatalf("GET %s status = %d, want %d: %s", path, response.Code, test.want, response.Body)
				}
				if response.Header().Get("Cache-Control") != "no-store" {
					t.Errorf("GET %s Cache-Control = %q, want no-store", path, response.Header().Get("Cache-Control"))
				}
				if strings.Contains(response.Body.String(), dashboardKey) || strings.Contains(response.Body.String(), chatOnlyKey) {
					t.Errorf("GET %s response contains a configured key: %s", path, response.Body)
				}
				if test.want != http.StatusOK && !strings.Contains(response.Body.String(), `"error"`) {
					t.Errorf("GET %s error response is not OpenAI-shaped: %s", path, response.Body)
				}
				if test.key == chatOnlyKey && !strings.Contains(response.Body.String(), `"code":"permission_denied"`) {
					t.Errorf("GET %s chat-only key error code = %s, want permission_denied", path, response.Body)
				}
			}
		})
	}
}

func TestDashboardAPI_ProviderStatusIsSafeAndBounded(t *testing.T) {
	handler, _, _ := dashboardEnv(t)
	response := dashboardRequest(t, handler, "/api/providers", dashboardKey, "127.0.0.1")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", response.Header().Get("Cache-Control"))
	}
	var result struct {
		Providers []struct {
			Name               string `json:"name"`
			Configured         bool   `json:"configured"`
			CredentialRequired bool   `json:"credential_required"`
			CredentialPresent  bool   `json:"credential_present"`
			Signals            []struct {
				Model            string `json:"model"`
				Reason           string `json:"reason"`
				UpstreamFailures int    `json:"upstream_failures"`
			} `json:"signals"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Providers) != 3 {
		t.Fatalf("providers = %+v, want only the three configured providers", result.Providers)
	}
	if result.Providers[0].Name != "groq" || !result.Providers[0].CredentialRequired || !result.Providers[0].CredentialPresent {
		t.Errorf("credential provider status = %+v, want configured groq with credential present", result.Providers[0])
	}
	if result.Providers[1].Name != "mock" || !result.Providers[1].Configured || result.Providers[1].CredentialRequired || result.Providers[1].CredentialPresent || len(result.Providers[1].Signals) != 1 {
		t.Fatalf("provider status = %+v, want configured mock with one observed cooldown", result.Providers[1])
	}
	provider := result.Providers[1]
	if provider.Signals[0].Model != "echo" || provider.Signals[0].Reason != "rate limited" || provider.Signals[0].UpstreamFailures != 1 {
		t.Errorf("observed signal = %+v, want the fixed rate limit reason for echo", provider.Signals[0])
	}
	if result.Providers[2].Name != "ollama" || result.Providers[2].CredentialRequired || result.Providers[2].CredentialPresent {
		t.Errorf("credential-free provider status = %+v, want configured ollama without credential requirement", result.Providers[2])
	}
	for _, forbidden := range []string{dashboardKey, chatOnlyKey, "GEMINI_API_KEY", "https://", "Authorization"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Errorf("provider status contains forbidden value %q: %s", forbidden, response.Body)
		}
	}
}

func TestDashboardAPI_UsageDecisionsBudgetAndQuotas(t *testing.T) {
	handler, db, now := dashboardEnv(t)
	model := `"><script>alert(1)</script>`
	if err := db.Record(t.Context(), store.Request{
		At: now.Add(-time.Hour), ClientApp: "demo", RequestedModel: model, Route: "auto; candidate 1/1",
		Provider: "mock", Model: model, Status: 200, PromptTokens: 12, CompletionTokens: 5,
		UsageSource: "estimated", CostMicroUSD: 1234, CostSource: "unknown", Cache: "hit",
		Skips:    []store.Skip{{Provider: "mock", Model: "other", Reason: "cooldown"}},
		Attempts: []store.Attempt{{Provider: "mock", Model: model, Outcome: "success", CostSource: "unknown"}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/usage?group=model",
		"/api/decisions?limit=10",
		"/api/budget",
		"/api/quotas",
	} {
		response := dashboardRequest(t, handler, path, dashboardKey, "127.0.0.1")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d: %s", path, response.Code, response.Body)
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("GET %s Cache-Control = %q", path, response.Header().Get("Cache-Control"))
		}
		if strings.Contains(response.Body.String(), dashboardKey) || strings.Contains(response.Body.String(), chatOnlyKey) {
			t.Errorf("GET %s exposed configured key", path)
		}
	}
	decisions := dashboardRequest(t, handler, "/api/decisions", dashboardKey, "127.0.0.1")
	var page struct {
		Data []store.Request `json:"data"`
	}
	if err := json.Unmarshal(decisions.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 1 || page.Data[0].Model != model || len(page.Data[0].Attempts) != 1 || len(page.Data[0].Skips) != 1 {
		t.Fatalf("decision page = %+v, want model text, one attempt and one skip", page)
	}
	usage := dashboardRequest(t, handler, "/api/usage?group=model", dashboardKey, "127.0.0.1")
	if !strings.Contains(usage.Body.String(), `"cost_micro_usd":1234`) || !strings.Contains(usage.Body.String(), `"cost_usd":"0.001234"`) || !strings.Contains(usage.Body.String(), `"estimated_rows":1`) {
		t.Errorf("usage totals missing expected cost or estimate fields: %s", usage.Body)
	}
	budgetResponse := dashboardRequest(t, handler, "/api/budget", dashboardKey, "127.0.0.1")
	if !strings.Contains(budgetResponse.Body.String(), `"includes_estimated_costs":true`) || !strings.Contains(budgetResponse.Body.String(), `"includes_unknown_costs":true`) {
		t.Errorf("budget quality flags missing: %s", budgetResponse.Body)
	}
	quotaResponse := dashboardRequest(t, handler, "/api/quotas", dashboardKey, "127.0.0.1")
	if !strings.Contains(quotaResponse.Body.String(), `"provider":"mock"`) || !strings.Contains(quotaResponse.Body.String(), `"breaker_state":"closed"`) {
		t.Errorf("quota snapshot missing candidate state: %s", quotaResponse.Body)
	}
}

func TestDashboardAPI_QueryBoundsUseOpenAIErrorShape(t *testing.T) {
	handler, _, _ := dashboardEnv(t)
	for _, path := range []string{
		"/api/decisions?limit=201",
		"/api/usage?group=provider&since=2020-01-01T00:00:00Z&until=2022-01-01T00:00:00Z",
		"/api/usage?since=nope",
	} {
		response := dashboardRequest(t, handler, path, dashboardKey, "127.0.0.1")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"type":"invalid_request_error"`) || !strings.Contains(response.Body.String(), `"error"`) {
			t.Errorf("GET %s = %d %s, want OpenAI 400", path, response.Code, response.Body)
		}
	}
}

func TestDashboardAPI_DecisionCursorExcludesConcurrentInserts(t *testing.T) {
	handler, db, now := dashboardEnv(t)
	for i := range 2 {
		model := "older"
		if i == 1 {
			model = "newer"
		}
		if err := db.Record(t.Context(), store.Request{At: now.Add(time.Duration(i) * time.Minute), ClientApp: "demo", Model: model}); err != nil {
			t.Fatal(err)
		}
	}
	first := dashboardRequest(t, handler, "/api/decisions?limit=1", dashboardKey, "127.0.0.1")
	var page struct {
		Data       []store.Request `json:"data"`
		NextCursor string          `json:"next_cursor"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.NextCursor == "" || page.Data[0].Model != "newer" {
		t.Fatalf("first page = %+v, want one row and cursor", page)
	}
	if err := db.Record(t.Context(), store.Request{At: now.Add(time.Hour), ClientApp: "demo", Model: "inserted later"}); err != nil {
		t.Fatal(err)
	}
	second := dashboardRequest(t, handler, "/api/decisions?limit=1&before="+page.NextCursor, dashboardKey, "127.0.0.1")
	if second.Code != http.StatusOK || strings.Contains(second.Body.String(), "inserted later") || !strings.Contains(second.Body.String(), `"model":"older"`) {
		t.Fatalf("second page = %d %s, concurrent insert must not shift cursor", second.Code, second.Body)
	}
}
