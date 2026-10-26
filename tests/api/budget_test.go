package api_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Polqt/spillway/internal/api"
	"github.com/Polqt/spillway/internal/budget"
	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/redact"
	"github.com/Polqt/spillway/internal/router"
	fakeprovider "github.com/Polqt/spillway/tests/testutil/fakeprovider"
)

func TestChat_BudgetSkipsPaidCandidateAndServesFreeCandidate(t *testing.T) {
	now := time.Date(2026, time.June, 12, 8, 0, 0, 0, time.UTC)
	paid := &countingProvider{name: "openrouter", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
		t.Fatal("budget-blocked paid provider was called")
		return providers.ChatResponse{}, nil
	}}
	free, err := fakeprovider.New(fakeprovider.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	paidRef, freeRef := routerRef("openrouter", "vendor/model"), routerRef("mock", "echo")
	set, err := providers.NewSet(paid, free)
	if err != nil {
		t.Fatal(err)
	}
	planner, err := router.New(router.Options{Tiers: map[string][]providers.ModelRef{"mixed": {paidRef, freeRef}}, Auto: []string{"mixed"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Providers: map[string]config.Provider{"openrouter": {}, "mock": {Type: "ollama"}},
		Pricing:   map[string]config.Price{"openrouter/vendor/model": {InputMicroUSDPerMillion: 1_000_000, OutputMicroUSDPerMillion: 1_000_000}},
	}
	gate, err := budget.NewGate(t.Context(), budget.GateOptions{Limits: config.Budgets{DailyMicroUSD: 1}, Providers: cfg.Providers, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	gate.Charge(now, 1)
	h, err := api.NewHandler(api.Options{
		Providers: set, Router: planner,
		LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger:    slog.New(slog.DiscardHandler), CostEstimator: budget.NewPricer(cfg), BudgetGate: gate,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ model, route string }{
		{"spillway/mixed", "tier mixed; candidate 2/2"},
		{"auto", "auto; tier mixed; candidate 2/2"},
	} {
		rec := routerRequest(t, h, `{"model":"`+tt.model+`","messages":[{"role":"user","content":"hello"}]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s response = %d %s, want free candidate success", tt.model, rec.Code, rec.Body)
		}
		if got := rec.Header().Get("X-Spillway-Route"); got != tt.route {
			t.Errorf("%s route = %q, want %q", tt.model, got, tt.route)
		}
		if !strings.Contains(rec.Body.String(), `"model":"echo"`) {
			t.Errorf("%s response = %s, want mock response", tt.model, rec.Body)
		}
	}
	if paid.calls.Load() != 0 {
		t.Errorf("paid calls = %d, want no paid call", paid.calls.Load())
	}
}

func TestChat_RulesBudgetExhaustionKeepsSelectedDifficulty(t *testing.T) {
	now := time.Date(2026, time.June, 12, 8, 0, 0, 0, time.UTC)
	paid := &countingProvider{name: "openrouter", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
		t.Fatal("budget-blocked paid provider was called")
		return providers.ChatResponse{}, nil
	}}
	ref := routerRef("openrouter", "vendor/model")
	set, err := providers.NewSet(paid)
	if err != nil {
		t.Fatal(err)
	}
	planner, err := router.New(router.Options{
		Tiers: map[string][]providers.ModelRef{"free": {ref}, "paid": {ref}}, Auto: []string{"free", "paid"},
		DifficultyTiers: map[string][]string{"simple": {"free"}, "moderate": {"paid"}, "hard": {"paid"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Providers: map[string]config.Provider{"openrouter": {}},
		Pricing:   map[string]config.Price{"openrouter/vendor/model": {InputMicroUSDPerMillion: 1_000_000, OutputMicroUSDPerMillion: 1_000_000}},
	}
	gate, err := budget.NewGate(t.Context(), budget.GateOptions{Limits: config.Budgets{DailyMicroUSD: 1}, Providers: cfg.Providers, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	gate.Charge(now, 1)
	h, err := api.NewHandler(api.Options{
		Providers: set, Router: planner,
		LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger:    slog.New(slog.DiscardHandler), CostEstimator: budget.NewPricer(cfg), BudgetGate: gate, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := routerRequest(t, h, `{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), `"code":"rate_limit_exceeded"`) {
		t.Fatalf("response = %d %s, want OpenAI budget exhaustion", rec.Code, rec.Body)
	}
	if got, want := rec.Header().Get("X-Spillway-Route"), "rules; default; difficulty moderate; tier paid; exhausted (1 eligible)"; got != want {
		t.Errorf("route = %q, want %q", got, want)
	}
	if paid.calls.Load() != 0 {
		t.Errorf("paid calls = %d, want 0", paid.calls.Load())
	}
}

func TestChat_BudgetExhaustionReturnsRetryAfter(t *testing.T) {
	now := time.Date(2026, time.June, 12, 8, 0, 0, 0, time.UTC)
	paid := &countingProvider{name: "openrouter", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
		t.Fatal("budget-blocked provider was called")
		return providers.ChatResponse{}, nil
	}}
	ref := routerRef("openrouter", "vendor/model")
	set, err := providers.NewSet(paid)
	if err != nil {
		t.Fatal(err)
	}
	planner, err := router.New(router.Options{Tiers: map[string][]providers.ModelRef{"paid": {ref}}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Providers: map[string]config.Provider{"openrouter": {}},
		Pricing:   map[string]config.Price{"openrouter/vendor/model": {InputMicroUSDPerMillion: 1_000_000, OutputMicroUSDPerMillion: 1_000_000}},
	}
	gate, err := budget.NewGate(t.Context(), budget.GateOptions{Limits: config.Budgets{DailyMicroUSD: 1}, Providers: cfg.Providers, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	gate.Charge(now, 1)
	h, err := api.NewHandler(api.Options{
		Providers: set, Router: planner, LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger: slog.New(slog.DiscardHandler), CostEstimator: budget.NewPricer(cfg), BudgetGate: gate, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := routerRequest(t, h, `{"model":"spillway/paid","messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), `"code":"rate_limit_exceeded"`) {
		t.Fatalf("response = %d %s, want 429 exhaustion", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "57600" {
		t.Errorf("Retry-After = %q, want 57600", got)
	}
	if got := rec.Header().Get("X-Spillway-Route"); got != "tier paid; exhausted (1 eligible)" {
		t.Errorf("route = %q, want unchanged exhaustion grammar", got)
	}
}

func TestChat_DirectBudgetRejectsUnpricedAndOverCapPaidModels(t *testing.T) {
	for _, tt := range []struct {
		name       string
		priced     bool
		wantStatus int
		wantCode   string
		wantRetry  bool
	}{
		{name: "unknown price", wantStatus: http.StatusBadRequest, wantCode: "model_price_unknown"},
		{name: "over cap", priced: true, wantStatus: http.StatusTooManyRequests, wantCode: "insufficient_quota", wantRetry: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, time.June, 12, 8, 0, 0, 0, time.UTC)
			upstream := &countingProvider{name: "openrouter", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
				return providers.ChatResponse{Body: json.RawMessage(`{"ok":true}`)}, nil
			}}
			set, err := providers.NewSet(upstream)
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.Config{Providers: map[string]config.Provider{"openrouter": {}}}
			if tt.priced {
				cfg.Pricing = map[string]config.Price{"openrouter/vendor/model": {InputMicroUSDPerMillion: 1_000_000, OutputMicroUSDPerMillion: 1_000_000}}
			}
			gate, err := budget.NewGate(t.Context(), budget.GateOptions{Limits: config.Budgets{DailyMicroUSD: 10}, Providers: cfg.Providers, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			gate.Charge(now, 10)
			h, err := api.NewHandler(api.Options{
				Providers: set, LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
				Logger: slog.New(slog.DiscardHandler), CostEstimator: budget.NewPricer(cfg), BudgetGate: gate,
				Now: func() time.Time { return now },
			})
			if err != nil {
				t.Fatal(err)
			}
			rec := routerRequest(t, h, `{"model":"openrouter/vendor/model","messages":[{"role":"user","content":"hello"}]}`)
			if rec.Code != tt.wantStatus || !strings.Contains(rec.Body.String(), `"code":"`+tt.wantCode+`"`) {
				t.Fatalf("response = %d %s, want status %d code %q", rec.Code, rec.Body, tt.wantStatus, tt.wantCode)
			}
			if got := rec.Header().Get("Retry-After"); (got != "") != tt.wantRetry {
				t.Errorf("Retry-After = %q, want present %v", got, tt.wantRetry)
			}
			if upstream.calls.Load() != 0 {
				t.Errorf("paid provider calls = %d, want 0", upstream.calls.Load())
			}
		})
	}
}

func TestChat_BudgetChargesReportedCostBeforeNextPaidRequest(t *testing.T) {
	now := time.Date(2026, time.June, 12, 8, 0, 0, 0, time.UTC)
	upstream := &countingProvider{name: "openrouter", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{Body: json.RawMessage(`{"model":"vendor/model","usage":{"prompt_tokens":1000000,"completion_tokens":0}}`)}, nil
	}}
	set, err := providers.NewSet(upstream)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Providers: map[string]config.Provider{"openrouter": {}},
		Pricing:   map[string]config.Price{"openrouter/vendor/model": {InputMicroUSDPerMillion: 1_000_000, OutputMicroUSDPerMillion: 1_000_000}},
	}
	gate, err := budget.NewGate(t.Context(), budget.GateOptions{Limits: config.Budgets{DailyMicroUSD: 1_000_000}, Providers: cfg.Providers, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.NewHandler(api.Options{
		Providers: set, LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger: slog.New(slog.DiscardHandler), CostEstimator: budget.NewPricer(cfg), BudgetGate: gate, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"openrouter/vendor/model","messages":[{"role":"user","content":"hello"}]}`
	if first := routerRequest(t, h, body); first.Code != http.StatusOK {
		t.Fatalf("first response = %d %s, want success", first.Code, first.Body)
	}
	second := routerRequest(t, h, body)
	if second.Code != http.StatusTooManyRequests || !strings.Contains(second.Body.String(), `"code":"insufficient_quota"`) {
		t.Fatalf("second response = %d %s, want budget rejection", second.Code, second.Body)
	}
	if upstream.calls.Load() != 1 {
		t.Errorf("provider calls = %d, want exactly one paid attempt", upstream.calls.Load())
	}
}

func TestChat_DirectFreeReferenceStillServesOverBudget(t *testing.T) {
	provider, err := fakeprovider.New(fakeprovider.Options{Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	set, err := providers.NewSet(provider)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Providers: map[string]config.Provider{"mock": {Type: "ollama"}}}
	gate, err := budget.NewGate(t.Context(), budget.GateOptions{Limits: config.Budgets{DailyMicroUSD: 1}, Providers: cfg.Providers})
	if err != nil {
		t.Fatal(err)
	}
	gate.Charge(time.Now(), 1)
	h, err := api.NewHandler(api.Options{Providers: set, LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}}, Logger: slog.New(slog.DiscardHandler), CostEstimator: budget.NewPricer(cfg), BudgetGate: gate})
	if err != nil {
		t.Fatal(err)
	}
	rec := routerRequest(t, h, `{"model":"mock/echo","messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("response = %d %s, want free model success", rec.Code, rec.Body)
	}
}
