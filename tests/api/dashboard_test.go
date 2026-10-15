package api_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	handler, err := api.NewHandler(api.Options{
		Providers: set,
		LocalKeys: []api.LocalKey{
			{Name: "admin", Key: redact.NewSecret(dashboardKey), Dashboard: true},
			{Name: "app", Key: redact.NewSecret(chatOnlyKey)},
		},
		AllowedHosts: api.LoopbackHosts("127.0.0.1"), Logger: slog.New(slog.DiscardHandler),
		UsageRecorder: db, DashboardStore: db, Tracker: tracker, BudgetGate: gate, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, db, now
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
			response := dashboardRequest(t, handler, "/api/quotas", test.key, test.host)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.want, response.Body)
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", response.Header().Get("Cache-Control"))
			}
			if strings.Contains(response.Body.String(), dashboardKey) || strings.Contains(response.Body.String(), chatOnlyKey) {
				t.Errorf("response contains a configured key: %s", response.Body)
			}
			if test.want != http.StatusOK && !strings.Contains(response.Body.String(), `"error"`) {
				t.Errorf("error response is not OpenAI-shaped: %s", response.Body)
			}
			if test.key == chatOnlyKey && !strings.Contains(response.Body.String(), `"code":"permission_denied"`) {
				t.Errorf("chat-only key error code = %s, want permission_denied", response.Body)
			}
		})
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
