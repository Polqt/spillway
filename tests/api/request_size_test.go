package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Polqt/spillway/internal/api"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/quota"
	"github.com/Polqt/spillway/internal/redact"
)

const (
	sizeProviderMarker = "org_test_marker"
	sizePromptMarker   = "PROMPT-MARKER-7f3a"
)

var (
	sizeFirst  = providers.ModelRef{Provider: "first", Model: "one"}
	sizeSecond = providers.ModelRef{Provider: "second", Model: "two"}
	sizeError  = fmt.Errorf("first: upstream status 413: Request too large in organization %s, key sk-test-local-cursor: %w", sizeProviderMarker, providers.ErrRequestTooLarge)
)

type sizeEnv struct {
	handler http.Handler
	tracker *quota.Tracker
	logs    *syncBuffer
	first   *countingProvider
	second  *countingProvider
}

func newSizeEnv(t *testing.T, first, second *countingProvider) sizeEnv {
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
	h, err := api.NewHandler(api.Options{
		Providers: set,
		LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger:    slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Tiers:     map[string][]providers.ModelRef{"demo": {sizeFirst, sizeSecond}},
		Tracker:   tracker,
		Now:       time.Now,
	})
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
			return providers.ChatResponse{}, sizeError
		},
		stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) { return nil, sizeError },
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

type sizeAfterChunkStream struct{ sent bool }

func (s *sizeAfterChunkStream) Recv() (providers.ChatChunk, error) {
	if !s.sent {
		s.sent = true
		return providers.ChatChunk{Data: json.RawMessage(`{"id":"chatcmpl-part","object":"chat.completion.chunk","created":1760000000,"model":"one","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`)}, nil
	}
	return providers.ChatChunk{}, sizeError
}

func (*sizeAfterChunkStream) Close() error { return nil }

func TestStream_TierRequestTooLargeFirstEventSpillsButAfterChunkDoesNot(t *testing.T) {
	t.Parallel()
	firstEvent := &countingProvider{name: "first", stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &sizeAfterChunkStream{sent: true}, nil
	}}
	e := newSizeEnv(t, firstEvent, servingProvider("second"))
	if rec := e.post(t, "spillway/demo", true); rec.Code != http.StatusOK || rec.Header().Get("X-Spillway-Model") != "two" {
		t.Fatalf("first-event size error = %d %v %s, want a spill to second", rec.Code, rec.Header(), rec.Body)
	}

	afterChunk := &countingProvider{name: "first", stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &sizeAfterChunkStream{}, nil
	}}
	e = newSizeEnv(t, afterChunk, servingProvider("second"))
	rec := e.post(t, "spillway/demo", true)
	body := rec.Body.String()
	if !strings.Contains(body, "partial") || !strings.Contains(body, `"code":"context_length_exceeded"`) || strings.Contains(body, "[DONE]") || e.second.calls.Load() != 0 {
		t.Fatalf("stream = %s, second calls %d; want the chunk, a context_length_exceeded event, no [DONE] and no spill", body, e.second.calls.Load())
	}
	assertNoSizeLeak(t, "stream", body)
}

func TestStream_TierAllRequestTooLargeIsPlainHTTPError(t *testing.T) {
	t.Parallel()
	e := newSizeEnv(t, sizeRejectingProvider("first"), sizeRejectingProvider("second"))
	rec := e.post(t, "spillway/demo", true)
	if rec.Code != http.StatusBadRequest || strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") || errorCode(t, rec) != "invalid_request_error/context_length_exceeded" {
		t.Fatalf("response = %d %v %s, want a JSON 400 context_length_exceeded", rec.Code, rec.Header(), rec.Body)
	}
}

var _ io.Closer = (*sizeAfterChunkStream)(nil)
