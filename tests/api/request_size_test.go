package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Polqt/spillway/internal/api"
	"github.com/Polqt/spillway/internal/budget"
	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/quota"
	"github.com/Polqt/spillway/internal/redact"
	"github.com/Polqt/spillway/internal/router"
	"github.com/Polqt/spillway/internal/store"
)

const (
	sizeProviderMarker = "org_test_marker"
	sizePromptMarker   = "PROMPT-MARKER-7f3a"
)

var (
	sizeFirst  = providers.ModelRef{Provider: "first", Model: "one"}
	sizeSecond = providers.ModelRef{Provider: "second", Model: "two"}
	errSize    = fmt.Errorf("first: upstream status 413: Request too large in organization %s, key sk-test-local-cursor: %w", sizeProviderMarker, providers.ErrRequestTooLarge)
)

type sizeEnv struct {
	handler http.Handler
	tracker *quota.Tracker
	logs    *syncBuffer
	first   *countingProvider
	second  *countingProvider
}

// newSizeEnv serves tier demo of first then second; adjust may add options such as a budget gate or usage recorder.
func newSizeEnv(t *testing.T, first, second *countingProvider, adjust ...func(*api.Options)) sizeEnv {
	t.Helper()
	set, err := providers.NewSet(first, second)
	if err != nil {
		t.Fatal(err)
	}
	tracker, err := quota.New(quota.Options{Now: time.Now, Candidates: []providers.ModelRef{sizeFirst, sizeSecond}})
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	options := api.Options{
		Providers: set,
		LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger:    slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Tiers:     map[string][]providers.ModelRef{"demo": {sizeFirst, sizeSecond}},
		Tracker:   tracker,
		Now:       time.Now,
	}
	for _, change := range adjust {
		change(&options)
	}
	h, err := api.NewHandler(options)
	if err != nil {
		t.Fatal(err)
	}
	return sizeEnv{handler: h, tracker: tracker, logs: logs, first: first, second: second}
}

func (e sizeEnv) post(t *testing.T, model string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"%s"}]}`, model, stream, sizePromptMarker)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+keyCI)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func sizeRejectingProvider(name string) *countingProvider {
	return &countingProvider{
		name: name,
		chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
			return providers.ChatResponse{}, errSize
		},
		stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) { return nil, errSize },
	}
}

func servingProvider(name string) *countingProvider {
	return &countingProvider{
		name: name,
		chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
			return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-ok","object":"chat.completion","created":1760000000,"model":"two","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)}, nil
		},
		stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
			return &compatTestStream{chunks: []providers.ChatChunk{
				{Data: json.RawMessage(`{"id":"chatcmpl-ok","object":"chat.completion.chunk","created":1760000000,"model":"two","choices":[{"index":0,"delta":{"content":"O"},"finish_reason":null}]}`)},
				{Data: json.RawMessage(`{"id":"chatcmpl-ok","object":"chat.completion.chunk","created":1760000000,"model":"two","choices":[{"index":0,"delta":{"content":"K"},"finish_reason":"stop"}]}`)},
			}}, nil
		},
	}
}

func assertNoSizeLeak(t *testing.T, label, text string) {
	t.Helper()
	for _, secret := range []string{sizeProviderMarker, "sk-test-local-cursor", sizePromptMarker} {
		if strings.Contains(text, secret) {
			t.Errorf("%s contains %q: %s", label, secret, text)
		}
	}
}

// errorCode returns the OpenAI error as type/code.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %s: %v", rec.Body, err)
	}
	return body.Error.Type + "/" + body.Error.Code
}

func TestChat_TierRequestTooLargeSpillsWithoutCooldown(t *testing.T) {
	t.Parallel()
	e := newSizeEnv(t, sizeRejectingProvider("first"), servingProvider("second"))
	for attempt := 1; attempt <= 2; attempt++ {
		rec := e.post(t, "spillway/demo", false)
		if rec.Code != http.StatusOK || rec.Header().Get("X-Spillway-Provider") != "second" || rec.Header().Get("X-Spillway-Model") != "two" || rec.Header().Get("X-Spillway-Route") != "tier demo; candidate 2/2" {
			t.Fatalf("attempt %d = %d %v %s, want second/two via tier demo; candidate 2/2", attempt, rec.Code, rec.Header(), rec.Body)
		}
	}
	if got := e.first.calls.Load(); got != 2 {
		t.Fatalf("first calls = %d, want 2 because a size rejection must not cool it down", got)
	}
	if available, _, reason := e.tracker.Available(sizeFirst); !available {
		t.Fatalf("first unavailable (%s) after request-size rejections", reason)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, `msg="tier candidate failed"`) || !strings.Contains(logs, "error_kind=request_too_large") || !strings.Contains(logs, "attempts=2") {
		t.Fatalf("logs = %s, want the size attempt and two attempts", logs)
	}
	assertNoSizeLeak(t, "logs", logs)
}

func TestChat_DirectRequestTooLargeDoesNotSpill(t *testing.T) {
	t.Parallel()
	e := newSizeEnv(t, sizeRejectingProvider("first"), servingProvider("second"))
	rec := e.post(t, "first/one", false)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_request_error/context_length_exceeded" || rec.Header().Get("X-Spillway-Route") != "direct" {
		t.Fatalf("response = %d %v %s, want direct 400 context_length_exceeded", rec.Code, rec.Header(), rec.Body)
	}
	if got := e.second.calls.Load(); got != 0 {
		t.Fatalf("second calls = %d, want 0 because direct references never spill", got)
	}
	assertNoSizeLeak(t, "body", rec.Body.String())
}

func TestChat_TierAllRequestTooLargeIsContextLengthExceeded(t *testing.T) {
	t.Parallel()
	e := newSizeEnv(t, sizeRejectingProvider("first"), sizeRejectingProvider("second"))
	rec := e.post(t, "spillway/demo", false)
	body := rec.Body.String()
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_request_error/context_length_exceeded" || rec.Header().Get("Retry-After") != "" || rec.Header().Get("X-Spillway-Route") != "tier demo; exhausted (2 eligible)" {
		t.Fatalf("response = %d %v %s, want 400 context_length_exceeded without Retry-After", rec.Code, rec.Header(), body)
	}
	if !strings.Contains(body, "first/one request too large") || !strings.Contains(body, "second/two request too large") || len(body) > 2048 {
		t.Fatalf("body = %s, want each candidate's fixed reason", body)
	}
	assertNoSizeLeak(t, "body", body)
	assertNoSizeLeak(t, "logs", e.logs.String())
}

func TestChat_TierRequestTooLargeMixedExhaustionStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		secondErr      error
		wantStatus     int
		wantCode       string
		wantRetryAfter string
	}{
		{"with a provider failure", providers.ErrUpstreamUnavailable, http.StatusServiceUnavailable, "api_error/no_candidate_available", ""},
		{"with a rate limit", providers.RetryAfterError{After: 7 * time.Second, Kind: providers.ErrRateLimited}, http.StatusTooManyRequests, "api_error/rate_limit_exceeded", "7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				second := &countingProvider{name: "second", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
					return providers.ChatResponse{}, tt.secondErr
				}}
				e := newSizeEnv(t, sizeRejectingProvider("first"), second)
				rec := e.post(t, "spillway/demo", false)
				if rec.Code != tt.wantStatus || errorCode(t, rec) != tt.wantCode || rec.Header().Get("Retry-After") != tt.wantRetryAfter {
					t.Fatalf("response = %d Retry-After %q %s, want %d %s Retry-After %q", rec.Code, rec.Header().Get("Retry-After"), rec.Body, tt.wantStatus, tt.wantCode, tt.wantRetryAfter)
				}
			})
		})
	}
}

func TestChat_TierRequestTooLargeWithCoolingCandidateIsRateLimited(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newSizeEnv(t, sizeRejectingProvider("first"), servingProvider("second"))
		e.tracker.Observe(sizeSecond, providers.RetryAfterError{After: 20 * time.Second, Kind: providers.ErrRateLimited}, providers.RateLimit{})
		rec := e.post(t, "spillway/demo", false)
		if rec.Code != http.StatusTooManyRequests || errorCode(t, rec) != "api_error/rate_limit_exceeded" || rec.Header().Get("Retry-After") != "20" {
			t.Fatalf("response = %d Retry-After %q %s, want 429 Retry-After 20 from the cooling candidate", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
		}
		if e.second.calls.Load() != 0 {
			t.Fatal("cooling candidate was called")
		}
	})
}

func TestChat_TierRequestTooLargeWithBudgetBlockedCandidateIsBudgetExceeded(t *testing.T) {
	now := time.Date(2026, time.June, 12, 8, 0, 0, 0, time.UTC)
	cfg := config.Config{
		Providers: map[string]config.Provider{"first": {Type: "ollama"}, "second": {}},
		Pricing:   map[string]config.Price{"second/two": {InputMicroUSDPerMillion: 1_000_000, OutputMicroUSDPerMillion: 1_000_000}},
	}
	gate, err := budget.NewGate(t.Context(), budget.GateOptions{Limits: config.Budgets{DailyMicroUSD: 1}, Providers: cfg.Providers, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	gate.Charge(now, 1)
	var recorded store.Request
	e := newSizeEnv(t, sizeRejectingProvider("first"), servingProvider("second"), func(o *api.Options) {
		o.BudgetGate, o.CostEstimator, o.Now = gate, budget.NewPricer(cfg), func() time.Time { return now }
		o.UsageRecorder = usageRecorderFunc(func(_ context.Context, request store.Request) error {
			recorded = request
			return nil
		})
	})
	rec := e.post(t, "spillway/demo", false)
	if rec.Code != http.StatusTooManyRequests || errorCode(t, rec) != "api_error/rate_limit_exceeded" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("response = %d Retry-After %q %s, want 429 until the budget resets", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
	if recorded.ErrorKind != "budget_exceeded" || e.second.calls.Load() != 0 {
		t.Fatalf("usage error_kind = %q, second calls %d; want budget_exceeded and no paid call", recorded.ErrorKind, e.second.calls.Load())
	}
}

// A candidate cooled by a concurrent request still reports why it failed this one.
func TestChat_TierRequestTooLargeReasonSurvivesConcurrentCooldown(t *testing.T) {
	t.Parallel()
	var tracker *quota.Tracker
	first := &countingProvider{name: "first", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
		tracker.Observe(sizeFirst, providers.RetryAfterError{After: 30 * time.Second, Kind: providers.ErrRateLimited}, providers.RateLimit{})
		return providers.ChatResponse{}, errSize
	}}
	e := newSizeEnv(t, first, sizeRejectingProvider("second"))
	tracker = e.tracker
	rec := e.post(t, "spillway/demo", false)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_request_error/context_length_exceeded" || !strings.Contains(rec.Body.String(), "first/one request too large") {
		t.Fatalf("response = %d %s, want 400 context_length_exceeded naming first/one request too large", rec.Code, rec.Body)
	}
}

func TestChat_RequestTooLargeUsageRecord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		second        *countingProvider
		wantStatus    int
		wantErrorKind string
		wantOutcomes  []string
	}{
		{"spill then success", servingProvider("second"), http.StatusOK, "", []string{"request_too_large", "success"}},
		{"all too large", sizeRejectingProvider("second"), http.StatusBadRequest, "context_length_exceeded", []string{"request_too_large", "request_too_large"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var recorded store.Request
			e := newSizeEnv(t, sizeRejectingProvider("first"), tt.second, func(o *api.Options) {
				o.UsageRecorder = usageRecorderFunc(func(_ context.Context, request store.Request) error {
					recorded = request
					return nil
				})
			})
			rec := e.post(t, "spillway/demo", false)
			if rec.Code != tt.wantStatus || recorded.ErrorKind != tt.wantErrorKind {
				t.Fatalf("status %d error_kind %q, want %d %q", rec.Code, recorded.ErrorKind, tt.wantStatus, tt.wantErrorKind)
			}
			outcomes := make([]string, 0, len(recorded.Attempts))
			for _, attempt := range recorded.Attempts {
				outcomes = append(outcomes, attempt.Outcome)
			}
			if !slices.Equal(outcomes, tt.wantOutcomes) {
				t.Fatalf("attempt outcomes = %v, want %v", outcomes, tt.wantOutcomes)
			}
			if tt.wantStatus != http.StatusOK && rec.Header().Get("X-Spillway-Provider") != "" {
				t.Fatalf("X-Spillway-Provider = %q on an exhausted tier", rec.Header().Get("X-Spillway-Provider"))
			}
		})
	}
}

func TestStream_TierRequestTooLargeOnOpenSpills(t *testing.T) {
	t.Parallel()
	e := newSizeEnv(t, sizeRejectingProvider("first"), servingProvider("second"))
	rec := e.post(t, "spillway/demo", true)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") || rec.Header().Get("X-Spillway-Route") != "tier demo; candidate 2/2" || !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("stream = %d %v %s, want second candidate's stream", rec.Code, rec.Header(), rec.Body)
	}
	if available, _, _ := e.tracker.Available(sizeFirst); !available {
		t.Fatal("first cooled down after a request-size rejection on stream open")
	}
}

// chunkThenErrorStream sends one "partial" chunk unless sent is already true, then fails with err.
type chunkThenErrorStream struct {
	sent   bool
	err    error
	closed atomic.Bool
}

func (s *chunkThenErrorStream) Recv() (providers.ChatChunk, error) {
	if !s.sent {
		s.sent = true
		return providers.ChatChunk{Data: json.RawMessage(`{"id":"chatcmpl-part","object":"chat.completion.chunk","created":1760000000,"model":"one","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`)}, nil
	}
	return providers.ChatChunk{}, s.err
}

func (s *chunkThenErrorStream) Close() error {
	s.closed.Store(true)
	return nil
}

func TestStream_TierRequestTooLargeFirstEventSpillsButAfterChunkDoesNot(t *testing.T) {
	t.Parallel()
	rejected := &chunkThenErrorStream{sent: true, err: errSize}
	firstEvent := &countingProvider{name: "first", stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return rejected, nil
	}}
	e := newSizeEnv(t, firstEvent, servingProvider("second"))
	if rec := e.post(t, "spillway/demo", true); rec.Code != http.StatusOK || rec.Header().Get("X-Spillway-Model") != "two" {
		t.Fatalf("first-event size error = %d %v %s, want a spill to second", rec.Code, rec.Header(), rec.Body)
	}
	if !rejected.closed.Load() {
		t.Fatal("the size-rejected first stream was not closed before spilling")
	}

	afterChunk := &countingProvider{name: "first", stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &chunkThenErrorStream{err: errSize}, nil
	}}
	e = newSizeEnv(t, afterChunk, servingProvider("second"))
	rec := e.post(t, "spillway/demo", true)
	body := rec.Body.String()
	if !strings.Contains(body, "partial") || !strings.Contains(body, `"code":"context_length_exceeded"`) || strings.Contains(body, "[DONE]") || e.second.calls.Load() != 0 {
		t.Fatalf("stream = %s, second calls %d; want the chunk, a context_length_exceeded event, no [DONE] and no spill", body, e.second.calls.Load())
	}
	assertNoSizeLeak(t, "stream", body)
}

func TestStream_TierUpstreamErrorMidStreamAfterSizeSpill(t *testing.T) {
	t.Parallel()
	second := &countingProvider{name: "second", stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &chunkThenErrorStream{err: providers.ErrUpstreamUnavailable}, nil
	}}
	e := newSizeEnv(t, sizeRejectingProvider("first"), second)
	rec := e.post(t, "spillway/demo", true)
	body := rec.Body.String()
	if rec.Header().Get("X-Spillway-Route") != "tier demo; candidate 2/2" || !strings.Contains(body, "partial") || !strings.Contains(body, `"error"`) || strings.Contains(body, "[DONE]") {
		t.Fatalf("stream = %v %s, want second's chunk then an error event without [DONE]", rec.Header(), body)
	}
	assertNoSizeLeak(t, "stream", body)
}

func TestStream_ClientDisconnectAfterSizeRejectionStopsSpilling(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first := &countingProvider{name: "first", stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		cancel()
		return nil, errSize
	}}
	var recorded store.Request
	e := newSizeEnv(t, first, servingProvider("second"), func(o *api.Options) {
		o.UsageRecorder = usageRecorderFunc(func(_ context.Context, request store.Request) error {
			recorded = request
			return nil
		})
	})
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"spillway/demo","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+keyCI)
	e.handler.ServeHTTP(httptest.NewRecorder(), req)
	if e.second.calls.Load() != 0 || recorded.ErrorKind != "client_disconnected" {
		t.Fatalf("second calls %d, usage error_kind %q; want no spill and client_disconnected", e.second.calls.Load(), recorded.ErrorKind)
	}
}

func TestChat_AutoRequestTooLargeSpillRoute(t *testing.T) {
	t.Parallel()
	planner, err := router.New(router.Options{Tiers: map[string][]providers.ModelRef{"demo": {sizeFirst, sizeSecond}}, Auto: []string{"demo"}})
	if err != nil {
		t.Fatal(err)
	}
	e := newSizeEnv(t, sizeRejectingProvider("first"), servingProvider("second"), func(o *api.Options) { o.Router = planner })
	rec := e.post(t, "auto", false)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Spillway-Route") != "auto; tier demo; candidate 2/2" {
		t.Fatalf("auto = %d %v %s, want auto; tier demo; candidate 2/2", rec.Code, rec.Header(), rec.Body)
	}
}

func TestChat_AutoPlanSizeSkipPlusRuntimeRequestTooLarge(t *testing.T) {
	t.Parallel()
	planner, err := router.New(router.Options{
		Tiers:        map[string][]providers.ModelRef{"demo": {sizeFirst, sizeSecond}},
		Auto:         []string{"demo"},
		Declarations: map[providers.ModelRef]router.Declaration{sizeFirst: {ContextWindow: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := newSizeEnv(t, servingProvider("first"), sizeRejectingProvider("second"), func(o *api.Options) { o.Router = planner })
	rec := e.post(t, "auto", false)
	body := rec.Body.String()
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_request_error/context_length_exceeded" || rec.Header().Get("X-Spillway-Route") != "auto; exhausted (1 eligible)" {
		t.Fatalf("auto = %d %v %s, want 400 context_length_exceeded with one eligible candidate", rec.Code, rec.Header(), body)
	}
	if !strings.Contains(body, "context too small") || !strings.Contains(body, "second/two request too large") || e.first.calls.Load() != 0 {
		t.Fatalf("body = %s, first calls %d; want the plan skip and the runtime rejection, first never called", body, e.first.calls.Load())
	}
}

func TestStream_TierAllRequestTooLargeIsPlainHTTPError(t *testing.T) {
	t.Parallel()
	e := newSizeEnv(t, sizeRejectingProvider("first"), sizeRejectingProvider("second"))
	rec := e.post(t, "spillway/demo", true)
	if rec.Code != http.StatusBadRequest || strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") || errorCode(t, rec) != "invalid_request_error/context_length_exceeded" {
		t.Fatalf("response = %d %v %s, want a JSON 400 context_length_exceeded", rec.Code, rec.Header(), rec.Body)
	}
}
