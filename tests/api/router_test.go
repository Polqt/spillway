package api_test

import (
	"context"
	"encoding/json"
	"errors"
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
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") || strings.Contains(got, "text/event-stream") {
		t.Errorf("content type = %q, want plain JSON", got)
	}
	if provider.calls.Load() != 0 {
		t.Errorf("provider calls = %d, want 0", provider.calls.Load())
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
