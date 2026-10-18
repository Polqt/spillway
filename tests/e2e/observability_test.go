package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Polqt/spillway/internal/api"
	"github.com/Polqt/spillway/internal/budget"
	"github.com/Polqt/spillway/internal/cache"
	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/providers/mock"
	"github.com/Polqt/spillway/internal/redact"
	"github.com/Polqt/spillway/internal/router"
	"github.com/Polqt/spillway/internal/store"
)

const observabilityLocalKey = "sk-test-observability-e2e"

func TestObservability_CachedPaidAnswerSurvivesBudgetAndMixedConcurrencyRecordsEveryRequest(t *testing.T) {
	now := time.Date(2026, time.July, 3, 12, 0, 0, 0, time.UTC)
	storePath := t.TempDir() + "/usage.db"
	usageStore, err := store.Open(t.Context(), store.Options{Path: storePath, Retention: 90 * 24 * time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := usageStore.Close(); err != nil {
			t.Errorf("close usage store: %v", err)
		}
	}()
	paid := &observabilityProvider{}
	free, err := mock.New(mock.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	providersSet, err := providers.NewSet(paid, free)
	if err != nil {
		t.Fatal(err)
	}
	paidRef, freeRef := providers.ModelRef{Provider: "openrouter", Model: "fixture/paid"}, providers.ModelRef{Provider: "mock", Model: "echo"}
	planner, err := router.New(router.Options{Tiers: map[string][]providers.ModelRef{"mixed": {paidRef, freeRef}}, Auto: []string{"mixed"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Providers: map[string]config.Provider{"openrouter": {}, "mock": {}},
		Pricing:   map[string]config.Price{"openrouter/fixture/paid": {InputMicroUSDPerMillion: 500_000, OutputMicroUSDPerMillion: 500_000}},
	}
	gate, err := budget.NewGate(t.Context(), budget.GateOptions{Limits: config.Budgets{MonthlyMicroUSD: 2}, Providers: cfg.Providers, Store: usageStore, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	exactCache, err := cache.New(1<<20, time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.Options{
		Providers: providersSet, Router: planner,
		LocalKeys: []api.LocalKey{{Name: "e2e", Key: redact.NewSecret(observabilityLocalKey), Dashboard: true}},
		Logger:    slog.New(slog.DiscardHandler), UsageRecorder: usageStore, DashboardStore: usageStore,
		CostEstimator: budget.NewPricer(cfg), BudgetGate: gate, ExactCache: exactCache, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	paidBody := `{"model":"openrouter/fixture/paid","messages":[{"role":"user","content":"same paid query"}]}`
	first := observabilityRequest(t, server, paidBody)
	second := observabilityRequest(t, server, paidBody)
	if first.StatusCode != http.StatusOK || first.Header.Get("X-Spillway-Cache") != "miss" {
		t.Fatalf("first paid response = %d cache=%q body=%s", first.StatusCode, first.Header.Get("X-Spillway-Cache"), first.Body)
	}
	if second.StatusCode != http.StatusOK || second.Header.Get("X-Spillway-Cache") != "hit" || second.Body != first.Body {
		t.Fatalf("cached paid response = %d cache=%q body=%s", second.StatusCode, second.Header.Get("X-Spillway-Cache"), second.Body)
	}
	if paid.calls.Load() != 1 {
		t.Fatalf("paid provider calls = %d, want one despite the reached budget", paid.calls.Load())
	}

	const parallelRequests = 50
	start := make(chan struct{})
	errs := make(chan error, parallelRequests)
	for i := range parallelRequests {
		model := []string{"openrouter/fixture/paid", "spillway/mixed", "auto"}[i%3]
		go func(i int, model string) {
			<-start
			body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}]}`, model, fmt.Sprintf("parallel-%d", i))
			response, err := observabilityRequestWithContext(t.Context(), server, body)
			if err != nil {
				errs <- err
				return
			}
			wantStatus := http.StatusOK
			if model == "openrouter/fixture/paid" {
				wantStatus = http.StatusTooManyRequests
				if !strings.Contains(response.Body, `"code":"insufficient_quota"`) {
					errs <- fmt.Errorf("request %d (%s) error = %s, want insufficient_quota", i, model, response.Body)
					return
				}
			}
			if response.StatusCode != wantStatus {
				errs <- fmt.Errorf("request %d (%s) = %d %s", i, model, response.StatusCode, response.Body)
				return
			}
			errs <- nil
		}(i, model)
	}
	close(start)
	for range parallelRequests {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	if failures := usageStore.RecordFailures(); failures != 0 {
		t.Fatalf("usage record failures = %d, want zero", failures)
	}

	requests, _, err := usageStore.Recent(t.Context(), store.RecentQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(requests), parallelRequests+2; got != want {
		t.Fatalf("stored request rows = %d, want %d", got, want)
	}
	var cacheHit, paidBudgetSkip, freeServe bool
	for _, request := range requests {
		cacheHit = cacheHit || request.Cache == "hit" && request.CostMicroUSD == 0 && len(request.Attempts) == 0
		for _, skip := range request.Skips {
			paidBudgetSkip = paidBudgetSkip || skip.Provider == "openrouter" && skip.Reason == "budget_exceeded"
		}
		freeServe = freeServe || request.Provider == "mock"
	}
	if !cacheHit || !paidBudgetSkip || !freeServe {
		t.Errorf("cross-feature records: cacheHit=%v paidBudgetSkip=%v freeServe=%v", cacheHit, paidBudgetSkip, freeServe)
	}
	if err := usageStore.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(t.Context(), store.Options{Path: storePath, Retention: 90 * 24 * time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("close reopened usage store: %v", err)
		}
	}()
	restartedGate, err := budget.NewGate(t.Context(), budget.GateOptions{Limits: config.Budgets{MonthlyMicroUSD: 2}, Providers: cfg.Providers, Store: reopened, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if got := restartedGate.Status().Monthly.SpentMicroUSD; got != 2 {
		t.Errorf("restarted monthly spend = %d micro-USD, want persisted 2", got)
	}
}

type observabilityProvider struct{ calls atomic.Int32 }

func (*observabilityProvider) Name() string { return "openrouter" }
func (*observabilityProvider) Models(context.Context) ([]providers.Model, error) {
	return []providers.Model{{ID: "fixture/paid"}}, nil
}
func (p *observabilityProvider) Chat(_ context.Context, req providers.ChatRequest) (providers.ChatResponse, error) {
	p.calls.Add(1)
	response, err := providers.NewChatResponse(providers.Completion{
		ID: "chatcmpl-paid-e2e", Model: req.Model, Content: "paid answer", FinishReason: "stop",
		Usage: providers.Usage{PromptTokens: 2, CompletionTokens: 2, TotalTokens: 4},
	})
	return response, err
}
func (*observabilityProvider) ChatStream(context.Context, providers.ChatRequest) (providers.Stream, error) {
	return nil, errors.New("stream not used by this scenario")
}

type observabilityReply struct {
	StatusCode int
	Header     http.Header
	Body       string
}

func observabilityRequest(t *testing.T, server *httptest.Server, body string) observabilityReply {
	t.Helper()
	response, err := observabilityRequestWithContext(t.Context(), server, body)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func observabilityRequestWithContext(ctx context.Context, server *httptest.Server, body string) (observabilityReply, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return observabilityReply{}, err
	}
	request.Header.Set("Authorization", "Bearer "+observabilityLocalKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		return observabilityReply{}, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return observabilityReply{}, err
	}
	return observabilityReply{StatusCode: response.StatusCode, Header: response.Header, Body: string(data)}, nil
}
