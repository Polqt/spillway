package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Polqt/spillway/internal/api"
	"github.com/Polqt/spillway/internal/cache"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/redact"
	"github.com/Polqt/spillway/internal/store"
)

func TestChat_ExactCacheReplaysResponsePerAppAndBody(t *testing.T) {
	c, err := cache.New(1<<20, time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	provider := &cacheTestProvider{calls: &calls}
	recorder := &cacheUsageRecorder{}
	set, err := providers.NewSet(provider)
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.NewHandler(api.Options{
		Providers: set,
		LocalKeys: []api.LocalKey{
			{Name: "app-one", Key: redact.NewSecret("sk-test-one")},
			{Name: "app-two", Key: redact.NewSecret("sk-test-two")},
		},
		Logger: slog.New(slog.DiscardHandler), ExactCache: c,
		UsageRecorder: recorder,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"cache/echo","messages":[{"role":"user","content":"hello"}]}`
	first := sendCacheRequest(t, h, "sk-test-one", body, "")
	second := sendCacheRequest(t, h, "sk-test-one", body, "")
	if first.Code != http.StatusOK || first.Header().Get("X-Spillway-Cache") != "miss" {
		t.Fatalf("first response = %d cache=%q body=%s", first.Code, first.Header().Get("X-Spillway-Cache"), first.Body)
	}
	if second.Code != http.StatusOK || second.Header().Get("X-Spillway-Cache") != "hit" {
		t.Fatalf("second response = %d cache=%q body=%s", second.Code, second.Header().Get("X-Spillway-Cache"), second.Body)
	}
	if second.Body.String() != first.Body.String() || calls.Load() != 1 {
		t.Fatalf("second body/calls = %q/%d, want original body and one provider call", second.Body, calls.Load())
	}
	if got := recorder.requests[1]; got.Cache != "hit" || got.PromptTokens != 3 || got.CompletionTokens != 2 || got.CostMicroUSD != 0 {
		t.Errorf("cache hit usage = %+v, want hit, original tokens and zero cost", got)
	}
	for _, header := range []string{"X-Spillway-Provider", "X-Spillway-Model", "X-Spillway-Route"} {
		if second.Header().Get(header) != first.Header().Get(header) {
			t.Errorf("%s = %q, want %q", header, second.Header().Get(header), first.Header().Get(header))
		}
	}
	otherApp := sendCacheRequest(t, h, "sk-test-two", body, "")
	reordered := sendCacheRequest(t, h, "sk-test-one", `{"messages":[{"content":"hello","role":"user"}],"model":"cache/echo"}`, "")
	if otherApp.Header().Get("X-Spillway-Cache") != "miss" || reordered.Header().Get("X-Spillway-Cache") != "miss" || calls.Load() != 3 {
		t.Fatalf("app/body isolation: app=%q reordered=%q calls=%d", otherApp.Header().Get("X-Spillway-Cache"), reordered.Header().Get("X-Spillway-Cache"), calls.Load())
	}
}

func TestChat_ExactCacheBypassAndDisabledHeaders(t *testing.T) {
	c, err := cache.New(1<<20, time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	calls := new(atomic.Int32)
	provider := &cacheTestProvider{calls: calls}
	set, err := providers.NewSet(provider)
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.NewHandler(api.Options{Providers: set, LocalKeys: []api.LocalKey{{Name: "app", Key: redact.NewSecret("sk-test-one")}}, Logger: slog.New(slog.DiscardHandler), ExactCache: c})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"cache/echo","messages":[{"role":"user","content":"hello"}]}`
	bypassed := sendCacheRequest(t, h, "sk-test-one", body, "bypass")
	if bypassed.Header().Get("X-Spillway-Cache") != "bypass" {
		t.Errorf("bypass cache header = %q", bypassed.Header().Get("X-Spillway-Cache"))
	}
	if repeated := sendCacheRequest(t, h, "sk-test-one", body, "bypass"); repeated.Header().Get("X-Spillway-Cache") != "bypass" || calls.Load() != 2 {
		t.Errorf("repeated bypass = cache %q, calls %d", repeated.Header().Get("X-Spillway-Cache"), calls.Load())
	}
	if _, ok := c.Get("app", []byte(body)); ok {
		t.Fatal("bypassed response was stored")
	}
	if normal := sendCacheRequest(t, h, "sk-test-one", body, ""); normal.Header().Get("X-Spillway-Cache") != "miss" {
		t.Fatalf("request after bypass cache header = %q, want miss", normal.Header().Get("X-Spillway-Cache"))
	}
	if normal := sendCacheRequest(t, h, "sk-test-one", body, ""); normal.Header().Get("X-Spillway-Cache") != "hit" || calls.Load() != 3 {
		t.Fatalf("repeat after miss cache header = %q, calls %d", normal.Header().Get("X-Spillway-Cache"), calls.Load())
	}
	streamBody := `{"model":"cache/echo","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	for range 2 {
		if streamed := sendCacheRequest(t, h, "sk-test-one", streamBody, ""); streamed.Header().Get("X-Spillway-Cache") != "bypass" {
			t.Errorf("stream cache header = %q", streamed.Header().Get("X-Spillway-Cache"))
		}
	}
	if calls.Load() != 5 {
		t.Errorf("provider calls after bypass and streaming = %d, want 5", calls.Load())
	}
	if _, ok := c.Get("app", []byte(streamBody)); ok {
		t.Fatal("streaming response was stored")
	}
	plain, err := api.NewHandler(api.Options{Providers: set, LocalKeys: []api.LocalKey{{Name: "app", Key: redact.NewSecret("sk-test-one")}}, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	response := sendCacheRequest(t, plain, "sk-test-one", `{"model":"cache/echo","messages":[{"role":"user","content":"hello"}]}`, "")
	if response.Header().Get("X-Spillway-Cache") != "" {
		t.Errorf("disabled cache header = %q, want absent", response.Header().Get("X-Spillway-Cache"))
	}
}

func TestChat_ExactCacheDoesNotStorePlanErrors(t *testing.T) {
	c, err := cache.New(1<<20, time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	provider := &cacheTestProvider{calls: new(atomic.Int32)}
	set, err := providers.NewSet(provider)
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.NewHandler(api.Options{Providers: set, LocalKeys: []api.LocalKey{{Name: "app", Key: redact.NewSecret("sk-test-one")}}, Logger: slog.New(slog.DiscardHandler), ExactCache: c})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"spillway/unknown","messages":[{"role":"user","content":"hello"}]}`
	for range 2 {
		response := sendCacheRequest(t, h, "sk-test-one", body, "")
		if response.Code != http.StatusNotFound || response.Header().Get("X-Spillway-Cache") != "miss" {
			t.Fatalf("plan error response = %d cache=%q body=%s", response.Code, response.Header().Get("X-Spillway-Cache"), response.Body)
		}
	}
	if _, ok := c.Get("app", []byte(body)); ok {
		t.Fatal("plan error was stored")
	}
}

func TestChat_ExactCacheDoesNotStoreLargeResponses(t *testing.T) {
	c, err := cache.New(2<<20, time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	calls := new(atomic.Int32)
	provider := &cacheTestProvider{calls: calls, response: json.RawMessage(strings.Repeat("x", (1<<20)+1))}
	set, err := providers.NewSet(provider)
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.NewHandler(api.Options{Providers: set, LocalKeys: []api.LocalKey{{Name: "app", Key: redact.NewSecret("sk-test-one")}}, Logger: slog.New(slog.DiscardHandler), ExactCache: c})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"cache/echo","messages":[{"role":"user","content":"large"}]}`
	for range 2 {
		_ = sendCacheRequest(t, h, "sk-test-one", body, "")
	}
	if calls.Load() != 2 {
		t.Fatalf("provider calls = %d, want 2 for oversized responses", calls.Load())
	}
}

func TestChat_ExactCacheDoesNotStoreProviderErrors(t *testing.T) {
	c, err := cache.New(1<<20, time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	calls := new(atomic.Int32)
	provider := &cacheTestProvider{calls: calls, failure: errors.New("upstream failure")}
	set, err := providers.NewSet(provider)
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.NewHandler(api.Options{Providers: set, LocalKeys: []api.LocalKey{{Name: "app", Key: redact.NewSecret("sk-test-one")}}, Logger: slog.New(slog.DiscardHandler), ExactCache: c})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"cache/echo","messages":[{"role":"user","content":"failure"}]}`
	for range 2 {
		response := sendCacheRequest(t, h, "sk-test-one", body, "")
		if response.Code != http.StatusInternalServerError || response.Header().Get("X-Spillway-Cache") != "miss" {
			t.Fatalf("provider error response = %d cache=%q body=%s", response.Code, response.Header().Get("X-Spillway-Cache"), response.Body)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("provider calls = %d, want 2 after uncached errors", calls.Load())
	}
}

func sendCacheRequest(t *testing.T, h http.Handler, key, body, cacheMode string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	if cacheMode != "" {
		req.Header.Set("X-Spillway-Cache", cacheMode)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type cacheTestProvider struct {
	calls    *atomic.Int32
	response json.RawMessage
	failure  error
}

func (*cacheTestProvider) Name() string                                      { return "cache" }
func (*cacheTestProvider) Models(context.Context) ([]providers.Model, error) { return nil, nil }
func (p *cacheTestProvider) Chat(context.Context, providers.ChatRequest) (providers.ChatResponse, error) {
	p.calls.Add(1)
	if p.failure != nil {
		return providers.ChatResponse{}, p.failure
	}
	response := p.response
	if response == nil {
		response = json.RawMessage(`{"id":"answer","usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}
	return providers.ChatResponse{Body: response}, nil
}

func (p *cacheTestProvider) ChatStream(context.Context, providers.ChatRequest) (providers.Stream, error) {
	p.calls.Add(1)
	return nil, errors.New("stream not configured")
}

type cacheUsageRecorder struct{ requests []store.Request }

func (r *cacheUsageRecorder) Record(_ context.Context, request store.Request) error {
	r.requests = append(r.requests, request)
	return nil
}
