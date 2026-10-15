package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Polqt/spillway/internal/api"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/providers/mock"
	"github.com/Polqt/spillway/internal/quota"
	"github.com/Polqt/spillway/internal/redact"
	"github.com/Polqt/spillway/internal/router"
)

func newRouterHandler(t *testing.T, ps []providers.Provider, tiers map[string][]providers.ModelRef, auto []string, declarations map[providers.ModelRef]router.Declaration, tracker *quota.Tracker, now func() time.Time) http.Handler {
	t.Helper()
	set, err := providers.NewSet(ps...)
	if err != nil {
		t.Fatal(err)
	}
	planner, err := router.New(router.Options{Tiers: tiers, Auto: auto, Declarations: declarations})
	if err != nil {
		t.Fatal(err)
	}
	opts := api.Options{
		Providers: set,
		Router:    planner,
		Now:       now,
		LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger:    slog.New(slog.DiscardHandler),
	}
	if tracker != nil {
		opts.Tracker = tracker
	}
	h, err := api.NewHandler(opts)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func newRulesHandler(t *testing.T, ps []providers.Provider, tiers map[string][]providers.ModelRef, auto []string, difficulty map[string][]string, rules []router.Rule, declarations map[providers.ModelRef]router.Declaration) http.Handler {
	t.Helper()
	set, err := providers.NewSet(ps...)
	if err != nil {
		t.Fatal(err)
	}
	planner, err := router.New(router.Options{Tiers: tiers, Auto: auto, DifficultyTiers: difficulty, Rules: rules, Declarations: declarations})
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.NewHandler(api.Options{
		Providers: set, Router: planner,
		LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func routerRequest(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+keyCI)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func routerRef(provider, model string) providers.ModelRef {
	return providers.ModelRef{Provider: provider, Model: model}
}

func TestChat_AutoSelectsFirstDeclaredCapableCandidate(t *testing.T) {
	undeclared := &countingProvider{name: "undeclared", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{}, errors.New("undeclared candidate was called")
	}}
	textOnly := &countingProvider{name: "textonly", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{}, errors.New("text-only candidate was called")
	}}
	mockProvider, err := mock.New(mock.Options{Now: func() time.Time { return time.Unix(1760400000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	refs := []providers.ModelRef{routerRef("undeclared", "first"), routerRef("textonly", "second"), routerRef("mock", "echo")}
	h := newRouterHandler(t, []providers.Provider{undeclared, textOnly, mockProvider}, map[string][]providers.ModelRef{"free": refs}, []string{"free"}, map[providers.ModelRef]router.Declaration{
		refs[1]: {Capabilities: []router.Capability{router.JSONMode}},
		refs[2]: {Capabilities: []router.Capability{router.Tools}},
	}, nil, nil)
	body := `{"model":"auto","messages":[{"role":"user","content":"use a tool"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`
	rec := routerRequest(t, h, body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"model":"echo"`) {
		t.Fatalf("response = %d %s, want mock/echo success", rec.Code, rec.Body)
	}
	if undeclared.calls.Load() != 0 || textOnly.calls.Load() != 0 {
		t.Errorf("skipped candidate calls = %d/%d, want 0/0", undeclared.calls.Load(), textOnly.calls.Load())
	}
	if got := rec.Header().Get("X-Spillway-Route"); got != "auto; needs tools; tier free; candidate 1/1" {
		t.Errorf("route = %q, want auto; needs tools; tier free; candidate 1/1", got)
	}
}

func TestChat_AutoAccessLog(t *testing.T) {
	t.Parallel()
	provider, err := mock.New(mock.Options{Now: func() time.Time { return time.Unix(1760400000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	set, err := providers.NewSet(provider)
	if err != nil {
		t.Fatal(err)
	}
	planner, err := router.New(router.Options{Tiers: map[string][]providers.ModelRef{"free": {routerRef("mock", "echo")}}, Auto: []string{"free"}})
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	h, err := api.NewHandler(api.Options{
		Providers: set,
		Router:    planner,
		LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger:    slog.New(slog.NewTextHandler(logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := routerRequest(t, h, `{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	for _, want := range []string{"provider=mock", "model=echo", "tier=auto", "attempts=1", `route="auto; tier free; candidate 1/1"`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("access log lacks %q: %s", want, logs.String())
		}
	}
}

func TestChat_RouteHeaderUsesDocumentedGrammar(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		model     string
		tiers     map[string][]providers.ModelRef
		auto      []string
		decls     map[providers.ModelRef]router.Declaration
		providers []providers.Provider
		body      string
		wantRoute string
		wantModel string
		wantCode  int
	}{
		{
			name: "direct", model: "plain/direct", providers: []providers.Provider{&countingProvider{name: "plain", chat: okChat("direct")}},
			body:      `{"model":"plain/direct","messages":[{"role":"user","content":"hello"}]}`,
			wantRoute: "direct", wantModel: "direct", wantCode: http.StatusOK,
		},
		{
			name: "tier second of three", model: "spillway/free",
			tiers: map[string][]providers.ModelRef{"free": {routerRef("first", "a"), routerRef("second", "b"), routerRef("third", "c")}},
			providers: []providers.Provider{
				&countingProvider{name: "first", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
					return providers.ChatResponse{}, providers.ErrRateLimited
				}},
				&countingProvider{name: "second", chat: okChat("b")},
				&countingProvider{name: "third", chat: okChat("c")},
			},
			body:      `{"model":"spillway/free","messages":[{"role":"user","content":"hello"}]}`,
			wantRoute: "tier free; candidate 2/3", wantModel: "b", wantCode: http.StatusOK,
		},
		{
			name: "tier needs tools", model: "spillway/free",
			tiers:     map[string][]providers.ModelRef{"free": {routerRef("plain", "a"), routerRef("tools", "b"), routerRef("tools", "c")}},
			decls:     map[providers.ModelRef]router.Declaration{routerRef("plain", "a"): {Capabilities: []router.Capability{}}, routerRef("tools", "b"): {Capabilities: []router.Capability{router.Tools}}, routerRef("tools", "c"): {Capabilities: []router.Capability{router.Tools}}},
			providers: []providers.Provider{&countingProvider{name: "plain", chat: okChat("a")}, &countingProvider{name: "tools", chat: okChat("b")}},
			body:      `{"model":"spillway/free","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup"}}]}`,
			wantRoute: "tier free; needs tools; candidate 1/2", wantModel: "b", wantCode: http.StatusOK,
		},
		{
			name: "auto crosses tiers", model: "auto",
			tiers: map[string][]providers.ModelRef{"free": {routerRef("first", "a"), routerRef("second", "b")}, "smart": {routerRef("third", "c")}}, auto: []string{"free", "smart"},
			decls: map[providers.ModelRef]router.Declaration{routerRef("first", "a"): {Capabilities: []router.Capability{router.Tools, router.Vision}}, routerRef("second", "b"): {Capabilities: []router.Capability{router.Tools, router.Vision}}, routerRef("third", "c"): {Capabilities: []router.Capability{router.Tools, router.Vision}}},
			providers: []providers.Provider{
				&countingProvider{name: "first", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
					return providers.ChatResponse{}, providers.ErrRateLimited
				}},
				&countingProvider{name: "second", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
					return providers.ChatResponse{}, providers.ErrRateLimited
				}},
				&countingProvider{name: "third", chat: okChat("c")},
			},
			body:      `{"model":"auto","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/x"}}]}],"tools":[{"type":"function","function":{"name":"lookup"}}]}`,
			wantRoute: "auto; needs tools, vision; tier smart; candidate 3/3", wantModel: "c", wantCode: http.StatusOK,
		},
		{
			name: "no capable model", model: "auto",
			tiers: map[string][]providers.ModelRef{"free": {routerRef("plain", "a")}}, auto: []string{"free"},
			providers: []providers.Provider{&countingProvider{name: "plain", chat: okChat("a")}},
			body:      `{"model":"auto","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"secret-prompt"}}]}]}`,
			wantRoute: "auto; needs vision; no capable model", wantModel: "", wantCode: http.StatusBadRequest,
		},
		{
			name: "eligible candidates exhausted", model: "auto",
			tiers: map[string][]providers.ModelRef{"free": {routerRef("first", "a"), routerRef("plain", "b"), routerRef("second", "c")}}, auto: []string{"free"},
			decls: map[providers.ModelRef]router.Declaration{routerRef("first", "a"): {Capabilities: []router.Capability{router.Tools}}, routerRef("plain", "b"): {Capabilities: []router.Capability{}}, routerRef("second", "c"): {Capabilities: []router.Capability{router.Tools}}},
			providers: []providers.Provider{
				&countingProvider{name: "first", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
					return providers.ChatResponse{}, providers.ErrRateLimited
				}},
				&countingProvider{name: "second", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
					return providers.ChatResponse{}, providers.ErrRateLimited
				}},
				&countingProvider{name: "plain", chat: okChat("b")},
			},
			body:      `{"model":"auto","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup"}}]}`,
			wantRoute: "auto; needs tools; exhausted (2 eligible)", wantModel: "", wantCode: http.StatusServiceUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newRouterHandler(t, tt.providers, tt.tiers, tt.auto, tt.decls, nil, nil)
			rec := routerRequest(t, h, tt.body)
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.wantCode, rec.Body)
			}
			if got := rec.Header().Get("X-Spillway-Route"); got != tt.wantRoute {
				t.Errorf("route = %q, want %q", got, tt.wantRoute)
			}
			if got := rec.Header().Get("X-Spillway-Model"); got != tt.wantModel {
				t.Errorf("model = %q, want %q", got, tt.wantModel)
			}
			if got := rec.Header().Get("X-Spillway-Route"); len(got) > 256 || strings.Contains(got, "secret-prompt") || strings.Contains(got, keyCI) {
				t.Errorf("unsafe route header %q (%d bytes)", got, len(got))
			}
		})
	}
}

func TestChat_RulesDifficultyRouting(t *testing.T) {
	inputThreshold := 1
	outputThreshold := 100
	tools := true
	tests := []struct {
		name      string
		rules     []router.Rule
		body      string
		failFirst bool
		wantRoute string
		wantModel string
	}{
		{
			name:      "default moderate",
			body:      `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`,
			wantRoute: "rules; default; difficulty moderate; tier balanced; candidate 1/1",
			wantModel: "m",
		},
		{
			name:      "first matching rule wins",
			rules:     []router.Rule{{EstimatedInputTokensGTE: &inputThreshold, Difficulty: "simple"}, {ToolsPresent: &tools, Difficulty: "hard"}},
			body:      `{"model":"auto","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup"}}]}`,
			wantRoute: "rules; rule 1; difficulty simple; tier cheap; candidate 1/1",
			wantModel: "m",
		},
		{
			name:      "requested output threshold",
			rules:     []router.Rule{{RequestedOutputTokensGTE: &outputThreshold, Difficulty: "hard"}},
			body:      `{"model":"auto","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":200}`,
			wantRoute: "rules; rule 1; difficulty hard; tier free; candidate 1/1",
			wantModel: "m",
		},
		{
			name:      "fallback tier after spillover",
			rules:     []router.Rule{{ToolsPresent: &tools, Difficulty: "hard"}},
			body:      `{"model":"auto","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup"}}]}`,
			failFirst: true,
			wantRoute: "rules; rule 1; difficulty hard; tier free; fallback strong; candidate 1/1",
			wantModel: "m",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			providersList := []providers.Provider{
				&countingProvider{name: "cheap", chat: okChat("cheap")},
				&countingProvider{name: "balanced", chat: okChat("balanced")},
			}
			freeChat := okChat("free")
			if tt.failFirst {
				freeChat = func(providers.ChatRequest) (providers.ChatResponse, error) {
					return providers.ChatResponse{}, providers.ErrRateLimited
				}
			}
			providersList = append(providersList,
				&countingProvider{name: "free", chat: freeChat},
				&countingProvider{name: "strong", chat: okChat("strong")},
			)
			tiers := map[string][]providers.ModelRef{
				"cheap": {routerRef("cheap", "m")}, "balanced": {routerRef("balanced", "m")}, "free": {routerRef("free", "m")}, "strong": {routerRef("strong", "m")},
			}
			auto := []string{"cheap", "balanced", "free", "strong"}
			difficulty := map[string][]string{"simple": {"cheap"}, "moderate": {"balanced"}, "hard": {"free", "strong"}}
			declarations := make(map[providers.ModelRef]router.Declaration)
			for _, candidates := range tiers {
				for _, ref := range candidates {
					declarations[ref] = router.Declaration{Capabilities: []router.Capability{router.Tools}}
				}
			}
			h := newRulesHandler(t, providersList, tiers, auto, difficulty, tt.rules, declarations)
			rec := routerRequest(t, h, tt.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, http.StatusOK, rec.Body)
			}
			if got := rec.Header().Get("X-Spillway-Route"); got != tt.wantRoute {
				t.Errorf("route = %q, want %q", got, tt.wantRoute)
			}
			if got := rec.Header().Get("X-Spillway-Model"); got != tt.wantModel {
				t.Errorf("model = %q, want %q", got, tt.wantModel)
			}
		})
	}
}

func TestChat_RulesNoEligibleTierReportsSelectedDifficulty(t *testing.T) {
	tools := true
	ref := routerRef("plain", "text")
	provider := &countingProvider{name: "plain", chat: okChat("text")}
	h := newRulesHandler(t, []providers.Provider{provider}, map[string][]providers.ModelRef{
		"cheap": {ref}, "balanced": {ref}, "strong": {ref},
	}, []string{"cheap", "balanced", "strong"}, map[string][]string{
		"simple": {"cheap"}, "moderate": {"balanced"}, "hard": {"strong"},
	}, []router.Rule{{ToolsPresent: &tools, Difficulty: "hard"}}, map[providers.ModelRef]router.Declaration{
		ref: {Capabilities: []router.Capability{}},
	})
	rec := routerRequest(t, h, `{"model":"auto","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup"}}]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"no_capable_model"`) {
		t.Fatalf("response = %d %s, want OpenAI no-capable-model error", rec.Code, rec.Body)
	}
	if got, want := rec.Header().Get("X-Spillway-Route"), "rules; rule 1; difficulty hard; tier strong; no capable model"; got != want {
		t.Errorf("route = %q, want %q", got, want)
	}
	if provider.calls.Load() != 0 {
		t.Errorf("provider calls = %d, want 0", provider.calls.Load())
	}
}

func okChat(model string) func(providers.ChatRequest) (providers.ChatResponse, error) {
	return func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{Body: json.RawMessage(`{"model":"` + model + `"}`)}, nil
	}
}

func TestChat_AutoNoCapableModelListsSafeReasonsWithoutCallingProviders(t *testing.T) {
	first := &countingProvider{name: "first", chat: func(providers.ChatRequest) (providers.ChatResponse, error) { return providers.ChatResponse{}, nil }}
	second := &countingProvider{name: "second", chat: func(providers.ChatRequest) (providers.ChatResponse, error) { return providers.ChatResponse{}, nil }}
	refs := []providers.ModelRef{routerRef("first", "plain"), routerRef("second", "text")}
	h := newRouterHandler(t, []providers.Provider{first, second}, map[string][]providers.ModelRef{"free": refs}, []string{"free"}, map[providers.ModelRef]router.Declaration{
		refs[0]: {Capabilities: []router.Capability{router.Tools}},
		refs[1]: {Capabilities: []router.Capability{}},
	}, nil, nil)
	rec := routerRequest(t, h, `{"model":"auto","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,secret"}}]}]}`)
	assertError(t, rec, http.StatusBadRequest, "invalid_request_error", "no_capable_model")
	for _, want := range []string{"first/plain lacks vision", "second/text lacks vision"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("error lacks %q: %s", want, rec.Body)
		}
	}
	if len(rec.Body.Bytes()) >= 2048 || strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), keyCI) {
		t.Errorf("error leaks data or exceeds 2 KiB: %d bytes %s", rec.Body.Len(), rec.Body)
	}
	if first.calls.Load() != 0 || second.calls.Load() != 0 {
		t.Errorf("provider calls = %d/%d, want 0/0", first.calls.Load(), second.calls.Load())
	}
}

func TestChat_AutoCoolingOnlyEligibleCandidatesSetsEarliestRetryAfter(t *testing.T) {
	first := &countingProvider{name: "first", chat: func(providers.ChatRequest) (providers.ChatResponse, error) { return providers.ChatResponse{}, nil }}
	second := &countingProvider{name: "second", chat: func(providers.ChatRequest) (providers.ChatResponse, error) { return providers.ChatResponse{}, nil }}
	plain := &countingProvider{name: "plain", chat: func(providers.ChatRequest) (providers.ChatResponse, error) { return providers.ChatResponse{}, nil }}
	refs := []providers.ModelRef{routerRef("first", "tools"), routerRef("plain", "text"), routerRef("second", "tools")}
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	tracker, err := quota.New(quota.Options{Now: func() time.Time { return now }, Candidates: refs})
	if err != nil {
		t.Fatal(err)
	}
	tracker.Observe(refs[0], providers.RetryAfterError{After: 34 * time.Second, Kind: providers.ErrRateLimited}, providers.RateLimit{})
	tracker.Observe(refs[2], providers.RetryAfterError{After: 7 * time.Second, Kind: providers.ErrRateLimited}, providers.RateLimit{})
	h := newRouterHandler(t, []providers.Provider{first, second, plain}, map[string][]providers.ModelRef{"free": refs}, []string{"free"}, map[providers.ModelRef]router.Declaration{
		refs[0]: {Capabilities: []router.Capability{router.Tools}},
		refs[1]: {Capabilities: []router.Capability{}},
		refs[2]: {Capabilities: []router.Capability{router.Tools}},
	}, tracker, func() time.Time { return now })
	rec := routerRequest(t, h, `{"model":"auto","messages":[{"role":"user","content":"tool"}],"tools":[{"type":"function","function":{"name":"lookup"}}]}`)
	assertError(t, rec, http.StatusTooManyRequests, "api_error", "rate_limit_exceeded")
	if got := rec.Header().Get("Retry-After"); got != strconv.Itoa(7) {
		t.Errorf("Retry-After = %q, want 7", got)
	}
	if first.calls.Load()+second.calls.Load()+plain.calls.Load() != 0 {
		t.Errorf("provider calls = %d, want 0", first.calls.Load()+second.calls.Load()+plain.calls.Load())
	}
	if !strings.Contains(rec.Body.String(), "plain/text lacks tools") {
		t.Errorf("exhaustion error lacks capability skip: %s", rec.Body)
	}
}

func TestChat_AutoSpillsAfterRateLimit(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	first := &countingProvider{name: "first", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
		firstCalls.Add(1)
		return providers.ChatResponse{}, providers.ErrRateLimited
	}}
	second := &countingProvider{name: "second", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
		secondCalls.Add(1)
		return providers.ChatResponse{Body: json.RawMessage(`{"model":"second"}`)}, nil
	}}
	refs := []providers.ModelRef{routerRef("first", "tools"), routerRef("second", "tools")}
	h := newRouterHandler(t, []providers.Provider{first, second}, map[string][]providers.ModelRef{"free": refs}, []string{"free"}, map[providers.ModelRef]router.Declaration{
		refs[0]: {Capabilities: []router.Capability{router.Tools}},
		refs[1]: {Capabilities: []router.Capability{router.Tools}},
	}, nil, nil)
	rec := routerRequest(t, h, `{"model":"auto","messages":[{"role":"user","content":"tool"}],"tools":[{"type":"function","function":{"name":"lookup"}}]}`)
	if rec.Code != http.StatusOK || firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Errorf("response = %d %s, provider calls = %d/%d; want success and one call each", rec.Code, rec.Body, firstCalls.Load(), secondCalls.Load())
	}
}

func TestChat_AutoNotConfiguredExplainsConfiguration(t *testing.T) {
	provider := &countingProvider{name: "plain", chat: func(providers.ChatRequest) (providers.ChatResponse, error) { return providers.ChatResponse{}, nil }}
	h := newRouterHandler(t, []providers.Provider{provider}, nil, nil, nil, nil, nil)
	rec := routerRequest(t, h, `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)
	assertError(t, rec, http.StatusNotFound, "invalid_request_error", "model_not_found")
	if !strings.Contains(rec.Body.String(), "set router.auto") {
		t.Errorf("message = %s, want router.auto guidance", rec.Body)
	}
}

func TestChat_PlanErrorsStayOpenAIJSONForStreamingRequests(t *testing.T) {
	provider := &countingProvider{name: "vision", chat: func(providers.ChatRequest) (providers.ChatResponse, error) { return providers.ChatResponse{}, nil }}
	ref := routerRef("vision", "model")
	h := newRouterHandler(t, []providers.Provider{provider}, map[string][]providers.ModelRef{"free": {ref}}, []string{"free"}, map[providers.ModelRef]router.Declaration{
		ref: {Capabilities: []router.Capability{router.Tools}},
	}, nil, nil)
	rec := routerRequest(t, h, `{"model":"auto","stream":true,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/image.png"}}]}]}`)
	assertError(t, rec, http.StatusBadRequest, "invalid_request_error", "no_capable_model")
	if got := rec.Header().Get("X-Spillway-Route"); got != "auto; needs vision; no capable model" {
		t.Errorf("route = %q, want auto; needs vision; no capable model", got)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") || strings.Contains(got, "text/event-stream") {
		t.Errorf("content type = %q, want plain JSON", got)
	}
	if provider.calls.Load() != 0 {
		t.Errorf("provider calls = %d, want 0", provider.calls.Load())
	}
}

func TestChat_RouteHeaderIsBoundedForInvalidTierNames(t *testing.T) {
	t.Parallel()
	name := strings.Repeat("a", 300)
	provider := &countingProvider{name: "plain", chat: okChat("text")}
	h := newRouterHandler(t, []providers.Provider{provider}, map[string][]providers.ModelRef{name: {routerRef("plain", "text")}}, nil, nil, nil, nil)
	rec := routerRequest(t, h, `{"model":"spillway/`+name+`","messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("X-Spillway-Route"); len(got) > 256 || !strings.Contains(got, "tier unknown") {
		t.Errorf("route = %q (%d bytes), want bounded safe tier name", got, len(got))
	}
}

func TestChat_RouteHeaderDoesNotIncludeProviderErrorText(t *testing.T) {
	t.Parallel()
	provider := &countingProvider{name: "plain", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{}, fmt.Errorf("secret-upstream-message: %w", providers.ErrBadRequest)
	}}
	h := newRouterHandler(t, []providers.Provider{provider}, map[string][]providers.ModelRef{"free": {routerRef("plain", "text")}}, nil, nil, nil, nil)
	rec := routerRequest(t, h, `{"model":"spillway/free","messages":[{"role":"user","content":"hello"}]}`)
	if got := rec.Header().Get("X-Spillway-Route"); got != "tier free; candidate 1/1" {
		t.Errorf("route = %q, want tier free; candidate 1/1", got)
	}
	if got := rec.Header().Get("X-Spillway-Route"); len(got) > 256 || strings.Contains(got, "secret-upstream-message") || strings.Contains(got, keyCI) {
		t.Errorf("route leaks sensitive data or exceeds 256 bytes: %q", got)
	}
}

func TestChat_PlanErrorRouteUsesDocumentedGrammar(t *testing.T) {
	t.Parallel()
	provider := &countingProvider{name: "plain", chat: okChat("text")}
	tests := []struct {
		name, model, body, wantRoute string
		wantStatus                   int
		auto                         []string
	}{
		{
			name: "unknown tier", model: "spillway/missing",
			body:      `{"model":"spillway/missing","messages":[{"role":"user","content":"hello"}]}`,
			wantRoute: "tier missing; no capable model", wantStatus: http.StatusNotFound, auto: []string{"free"},
		},
		{
			name: "auto not configured", model: "auto",
			body:      `{"model":"auto","messages":[{"role":"user","content":"hello"}]}`,
			wantRoute: "auto; no capable model", wantStatus: http.StatusNotFound,
		},
		{
			name: "invalid request field", model: "auto",
			body:      `{"model":"auto","messages":[{"role":"user","content":"hello"}],"tools":"bad"}`,
			wantRoute: "auto; no capable model", wantStatus: http.StatusBadRequest, auto: []string{"free"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var tiers map[string][]providers.ModelRef
			if len(tt.auto) > 0 {
				tiers = map[string][]providers.ModelRef{"free": {routerRef("plain", "text")}}
			}
			h := newRouterHandler(t, []providers.Provider{provider}, tiers, tt.auto, nil, nil, nil)
			rec := routerRequest(t, h, tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if got := rec.Header().Get("X-Spillway-Route"); got != tt.wantRoute {
				t.Errorf("route = %q, want %q", got, tt.wantRoute)
			}
		})
	}
}

func TestChat_AutoInvalidInspectedFieldMapsToInvalidRequest(t *testing.T) {
	provider := &countingProvider{name: "plain", chat: func(providers.ChatRequest) (providers.ChatResponse, error) { return providers.ChatResponse{}, nil }}
	ref := routerRef("plain", "model")
	h := newRouterHandler(t, []providers.Provider{provider}, map[string][]providers.ModelRef{"free": {ref}}, []string{"free"}, nil, nil, nil)
	rec := routerRequest(t, h, `{"model":"auto","messages":[{"role":"user","content":"hi"}],"tools":"not-an-array"}`)
	assertError(t, rec, http.StatusBadRequest, "invalid_request_error", "invalid_request")
	if !strings.Contains(rec.Body.String(), `\"tools\" must be an array`) {
		t.Errorf("error = %s, want invalid tools field", rec.Body)
	}
	if provider.calls.Load() != 0 {
		t.Errorf("provider calls = %d, want 0", provider.calls.Load())
	}
}

func TestChat_AutoAllContextSkipsUsesOpenAIContextCode(t *testing.T) {
	provider := &countingProvider{name: "small", chat: func(providers.ChatRequest) (providers.ChatResponse, error) { return providers.ChatResponse{}, nil }}
	ref := routerRef("small", "context")
	h := newRouterHandler(t, []providers.Provider{provider}, map[string][]providers.ModelRef{"free": {ref}}, []string{"free"}, map[providers.ModelRef]router.Declaration{
		ref: {Capabilities: []router.Capability{}, ContextWindow: 1},
	}, nil, nil)
	rec := routerRequest(t, h, `{"model":"auto","messages":[{"role":"user","content":"a prompt beyond one token"}]}`)
	assertError(t, rec, http.StatusBadRequest, "invalid_request_error", "context_length_exceeded")
	if provider.calls.Load() != 0 {
		t.Errorf("provider calls = %d, want 0", provider.calls.Load())
	}
}

func TestChat_DirectModelForwardsBodyByteForByte(t *testing.T) {
	const body = "{ \"model\" : \"plain/direct\", \"messages\" : [ { \"role\" : \"user\", \"content\" : \"hi\" } ], \"unknown\" : {\"kept\":true} }"
	var got string
	provider := &countingProvider{name: "plain", chat: func(req providers.ChatRequest) (providers.ChatResponse, error) {
		got = string(req.RawBody)
		return providers.ChatResponse{Body: json.RawMessage(`{"ok":true}`)}, nil
	}}
	h := newRouterHandler(t, []providers.Provider{provider}, nil, nil, nil, nil, nil)
	rec := routerRequest(t, h, body)
	if rec.Code != http.StatusOK || got != body {
		t.Errorf("status/body = %d/%q, want 200 and exact request bytes %q", rec.Code, got, body)
	}
}

func TestModels_ListsAutoWhenConfigured(t *testing.T) {
	provider := &countingProvider{name: "plain", models: func(context.Context) ([]providers.Model, error) { return []providers.Model{{ID: "text"}}, nil }, chat: func(providers.ChatRequest) (providers.ChatResponse, error) { return providers.ChatResponse{}, nil }}
	ref := routerRef("plain", "text")
	h := newRouterHandler(t, []providers.Provider{provider}, map[string][]providers.ModelRef{"free": {ref}}, []string{"free"}, nil, nil, nil)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+keyCI)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"auto"`) {
		t.Errorf("models response = %d %s, want auto", rec.Code, rec.Body)
	}
}

func TestChat_AutoHandlesFiftyConcurrentMixedRequests(t *testing.T) {
	plain := &countingProvider{name: "plain", chat: func(req providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{Body: json.RawMessage(`{"model":"` + req.Model + `"}`)}, nil
	}}
	capable := &countingProvider{name: "capable", chat: func(req providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{Body: json.RawMessage(`{"model":"` + req.Model + `"}`)}, nil
	}}
	refs := []providers.ModelRef{routerRef("plain", "text"), routerRef("capable", "tools")}
	h := newRouterHandler(t, []providers.Provider{plain, capable}, map[string][]providers.ModelRef{"free": refs}, []string{"free"}, map[providers.ModelRef]router.Declaration{
		refs[1]: {Capabilities: []router.Capability{router.Tools}},
	}, nil, nil)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := `{"model":"auto","messages":[{"role":"user","content":"hello"}]}`
			if i%2 == 0 {
				body = `{"model":"auto","messages":[{"role":"user","content":"tool"}],"tools":[{"type":"function","function":{"name":"lookup"}}]}`
			}
			rec := routerRequest(t, h, body)
			if rec.Code != http.StatusOK {
				t.Errorf("request %d: status = %d, body = %s", i, rec.Code, rec.Body)
			}
		}(i)
	}
	wg.Wait()
}
