package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	"github.com/Polqt/spillway/internal/api"
	"github.com/Polqt/spillway/internal/fakenet"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/providers/openaicompat"
	"github.com/Polqt/spillway/internal/quota"
	"github.com/Polqt/spillway/internal/redact"
)

const echoStreamBody = `{"model":"mock/echo","stream":true,"messages":[{"role":"user","content":"hello spillway"}]}`
const fakeStreamBody = `{"model":"fake/x","stream":true,"messages":[{"role":"user","content":"hello spillway"}]}`

// readEvents parses an SSE body into its data payloads, reading line by line
// as an incremental client does.
func readEvents(t *testing.T, r io.Reader) []string {
	t.Helper()
	var events []string
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if errors.Is(err, io.EOF) && line == "" {
			return events
		}
		if err != nil {
			t.Fatalf("read SSE line %q: %v", line, err)
		}
		switch {
		case line == "\n":
		case strings.HasPrefix(line, "data: "):
			events = append(events, strings.TrimSuffix(strings.TrimPrefix(line, "data: "), "\n"))
		default:
			t.Fatalf("unexpected SSE line %q", line)
		}
	}
}

func TestStream_RelaysChunksAndDone(t *testing.T) {
	t.Parallel()
	e := newEnv(t, api.LoopbackHosts("127.0.0.1"))

	rec := e.do(t, request{auth: "Bearer " + keyCI, body: echoStreamBody})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	for header, want := range map[string]string{
		"Content-Type":        "text/event-stream",
		"Cache-Control":       "no-cache",
		"X-Spillway-Provider": "mock",
		"X-Spillway-Model":    "echo",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: {") || !strings.HasSuffix(body, "}\n\ndata: [DONE]\n\n") {
		t.Errorf("body is not data events ending in [DONE]:\n%s", body)
	}
	events := readEvents(t, strings.NewReader(body))
	if len(events) != 6 || events[5] != "[DONE]" {
		t.Fatalf("events = %q, want 5 chunks then [DONE]", events)
	}
	var content strings.Builder
	for _, ev := range events[:5] {
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct{ Content string } `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(ev), &chunk); err != nil || chunk.Object != "chat.completion.chunk" || len(chunk.Choices) != 1 {
			t.Fatalf("event %s is not a chunk (err %v)", ev, err)
		}
		content.WriteString(chunk.Choices[0].Delta.Content)
	}
	if content.String() != "You said: hello spillway" {
		t.Errorf("streamed content = %q, want %q", content.String(), "You said: hello spillway")
	}
	if strings.Contains(body, keyCI) {
		t.Error("stream contains the local key")
	}
}

func TestStream_UpstreamErrorMidStream(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)

	rec := e.do(t, request{auth: "Bearer " + keyCI, body: `{"model":"mock/stream-error","stream":true,"messages":[{"role":"user","content":"hello spillway"}]}`})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (headers were sent before the failure)", rec.Code)
	}
	events := readEvents(t, rec.Body)
	if len(events) != 3 {
		t.Fatalf("events = %q, want two chunks then an error", events)
	}
	var last openAIError
	if err := json.Unmarshal([]byte(events[2]), &last); err != nil {
		t.Fatalf("last event %s is not an OpenAI error: %v", events[2], err)
	}
	if last.Error.Type != "api_error" || last.Error.Code != "upstream_unavailable" || last.Error.Message == "" {
		t.Errorf("error event = %+v, want api_error upstream_unavailable", last.Error)
	}
	if !strings.Contains(events[2], `"param":null`) {
		t.Errorf("error event %s, want \"param\":null", events[2])
	}
	body := rec.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Error("a failed stream ends with [DONE]")
	}
	// The provider's own error text goes to the log, never to the client.
	if strings.Contains(body, "mid-stream") || strings.Contains(body, keyCI) {
		t.Errorf("stream leaks internal text or the key:\n%s", body)
	}
	if !strings.Contains(e.logs.String(), "failed mid-stream") {
		t.Errorf("log lacks the provider error:\n%s", e.logs)
	}
}

func TestStream_ErrorBeforeFirstChunk(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.fake.stream = func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return nil, fmt.Errorf("fake: %w", providers.ErrUpstreamUnavailable)
	}
	cases := []struct {
		model, typ, code string
		status           int
	}{
		{"mock/nope", "invalid_request_error", "model_not_found", http.StatusNotFound},
		{"fake/x", "api_error", "upstream_unavailable", http.StatusBadGateway},
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			t.Parallel()
			body := `{"model":"` + c.model + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			rec := e.do(t, request{auth: "Bearer " + keyCI, body: body})
			assertError(t, rec, c.status, c.typ, c.code)
			if rec.Header().Get("X-Spillway-Provider") == "" {
				t.Error("X-Spillway-Provider missing on error")
			}
		})
	}
}

func TestStream_OpenAICompatErrorAndTruncatedStreamsOmitDone(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "error event after chunks",
			body: "data: {\"id\":\"one\",\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n" +
				"data: {\"id\":\"two\",\"choices\":[{\"delta\":{\"content\":\"two\"}}]}\n\n" +
				"data: {\"error\":{\"message\":\"upstream failed\",\"code\":503}}\n\n",
		},
		{
			name: "truncated stream",
			body: "data: {\"id\":\"one\",\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n" +
				"data: {\"id\":\"two\",\"choices\":[{\"delta\":{\"content\":\"two\"}}]}\n\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tt.body)
			}))
			defer upstream.Close()
			provider, err := openaicompat.New(openaicompat.Options{
				Name: "groq", BaseURL: upstream.URL, APIKey: redact.NewSecret("sk-test-groq-stream"), Timeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			set, err := providers.NewSet(provider)
			if err != nil {
				t.Fatal(err)
			}
			handler, err := api.NewHandler(api.Options{
				Providers: set,
				LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
				Logger:    slog.New(slog.DiscardHandler),
			})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"groq/upstream","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Authorization", "Bearer "+keyCI)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
			}
			events := readEvents(t, rec.Body)
			if len(events) != 3 {
				t.Fatalf("events = %q, want two chunks and one SSE error", events)
			}
			var upstreamChunk map[string]any
			if err := json.Unmarshal([]byte(events[0]), &upstreamChunk); err != nil || upstreamChunk["id"] != "one" {
				t.Errorf("first event = %q, want first upstream chunk (decode error %v)", events[0], err)
			}
			var streamError openAIError
			if err := json.Unmarshal([]byte(events[2]), &streamError); err != nil || streamError.Error.Type != "api_error" || streamError.Error.Code != "upstream_unavailable" {
				t.Errorf("last event = %q, want OpenAI-shaped upstream error (decode error %v)", events[2], err)
			}
			if strings.Contains(rec.Body.String(), "[DONE]") {
				t.Errorf("failed stream ends with [DONE]: %s", rec.Body)
			}
		})
	}
}

// failingStream fails on its first Recv, before any chunk exists.
type failingStream struct{}

func (failingStream) Recv() (providers.ChatChunk, error) {
	return providers.ChatChunk{}, fmt.Errorf("fake: %w", providers.ErrUpstreamUnavailable)
}
func (failingStream) Close() error { return nil }

// Nothing has reached the client before the first chunk, so a failure there
// is still a plain HTTP error that a later spillover could replace.
func TestStream_FirstRecvFailureIsHTTPError(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.fake.stream = func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return failingStream{}, nil
	}

	rec := e.do(t, request{auth: "Bearer " + keyCI, body: `{"model":"fake/x","stream":true,"messages":[{"role":"user","content":"hi"}]}`})

	assertError(t, rec, http.StatusBadGateway, "api_error", "upstream_unavailable")
}

func TestStream_TierSpillsBeforeFirstChunk(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		chatStream bool
	}{
		{name: "ChatStream failure", chatStream: true},
		{name: "first Recv failure"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			firstClosed := false
			first := &countingProvider{name: "first"}
			first.stream = func(context.Context, providers.ChatRequest) (providers.Stream, error) {
				if tt.chatStream {
					return nil, providers.ErrRateLimited
				}
				return &trackedStream{recvErr: providers.ErrRateLimited, closed: func() { firstClosed = true }}, nil
			}
			second := &countingProvider{name: "second", stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
				return &trackedStream{chunks: []string{`{"model":"second"}`}}, nil
			}}
			h := newTierStreamHandler(t, first, second)
			rec := serveTierStream(t, h)
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
				t.Fatalf("response = %d %v %s, want SSE 200", rec.Code, rec.Header(), rec.Body)
			}
			if got := rec.Header().Get("X-Spillway-Route"); got != "tier demo; candidate 2/2" {
				t.Errorf("route = %q, want tier demo; candidate 2/2", got)
			}
			if got := rec.Header().Get("X-Spillway-Model"); got != "candidate" {
				t.Errorf("model = %q, want candidate from the second provider after the stream spill", got)
			}
			if got := readEvents(t, rec.Body); !slices.Equal(got, []string{`{"model":"second"}`, "[DONE]"}) {
				t.Errorf("events = %q, want only second candidate and [DONE]", got)
			}
			assertTierStreamHasNoKeys(t, rec.Body.String())
			if first.calls.Load() != 1 {
				t.Errorf("first candidate calls = %d, want 1", first.calls.Load())
			}
			if second.calls.Load() != 1 {
				t.Errorf("second candidate calls = %d, want 1", second.calls.Load())
			}
			if !tt.chatStream && !firstClosed {
				t.Error("failed first stream was not closed")
			}
		})
	}
}

func TestStream_TierFirstOpenAIErrorEventSpills(t *testing.T) {
	t.Parallel()
	fixture, err := os.ReadFile("../providers/openaicompat/testdata/stream-error-first.sse")
	if err != nil {
		t.Fatal(err)
	}
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(fixture)
	}))
	defer upstream.Close()
	first, err := openaicompat.New(openaicompat.Options{Name: "first", BaseURL: upstream.URL, APIKey: redact.NewSecret("sk-test-provider"), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	second := &countingProvider{name: "second", stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &trackedStream{chunks: []string{`{"model":"second"}`}}, nil
	}}
	h := newTierStreamHandler(t, first, second)
	rec := serveTierStream(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	if got := readEvents(t, rec.Body); !slices.Equal(got, []string{`{"model":"second"}`, "[DONE]"}) {
		t.Errorf("events = %q, want only second candidate and [DONE]", got)
	}
	assertTierStreamHasNoKeys(t, rec.Body.String())
	if strings.Contains(rec.Body.String(), "OpenRouter quota exhausted") {
		t.Errorf("response leaks upstream error: %s", rec.Body)
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Errorf("first candidate calls = %d, want 1", got)
	}
}

func TestStream_TierFailureAfterChunkDoesNotSpill(t *testing.T) {
	t.Parallel()
	first := &countingProvider{name: "first", stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &trackedStream{chunks: []string{`{"model":"first"}`}, afterChunks: providers.ErrRateLimited}, nil
	}}
	second := &countingProvider{name: "second", stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &trackedStream{chunks: []string{`{"model":"second"}`}}, nil
	}}
	rec := serveTierStream(t, newTierStreamHandler(t, first, second))
	events := readEvents(t, rec.Body)
	if rec.Code != http.StatusOK || len(events) != 2 || events[0] != `{"model":"first"}` {
		t.Fatalf("response = %d %q, want first chunk and an error event", rec.Code, events)
	}
	var streamError openAIError
	if err := json.Unmarshal([]byte(events[1]), &streamError); err != nil || streamError.Error.Code != "rate_limit_exceeded" {
		t.Errorf("error event = %q, %v; want rate_limit_exceeded", events[1], err)
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Error("failed stream ends with [DONE]")
	}
	assertTierStreamHasNoKeys(t, rec.Body.String())
	if second.calls.Load() != 0 {
		t.Errorf("second candidate calls = %d, want 0", second.calls.Load())
	}
}

func TestStream_TierMidStreamFailuresOpenCircuit(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	first := &countingProvider{name: "first", stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &trackedStream{chunks: []string{`{"model":"first"}`}, afterChunks: providers.ErrUpstreamUnavailable}, nil
	}}
	second := &countingProvider{name: "second", stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &trackedStream{chunks: []string{`{"model":"second"}`}}, nil
	}}
	set, err := providers.NewSet(first, second)
	if err != nil {
		t.Fatal(err)
	}
	refs := []providers.ModelRef{{Provider: "first", Model: "candidate"}, {Provider: "second", Model: "candidate"}}
	tracker, err := quota.New(quota.Options{Now: func() time.Time { return now }, Candidates: refs})
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.NewHandler(api.Options{
		Providers: set,
		Tracker:   tracker,
		Now:       func() time.Time { return now },
		LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger:    slog.New(slog.DiscardHandler),
		Tiers:     map[string][]providers.ModelRef{"demo": refs},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		rec := serveTierStream(t, h)
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"model":"second"`) {
			t.Fatalf("stream response = %d %s; want committed first candidate failure", rec.Code, rec.Body)
		}
	}
	if first.calls.Load() != 3 || second.calls.Load() != 0 {
		t.Fatalf("provider calls = %d/%d, want 3/0", first.calls.Load(), second.calls.Load())
	}
	if available, until, reason := tracker.Available(refs[0]); available || !until.Equal(now.Add(30*time.Second)) || reason != "circuit open" {
		t.Errorf("first candidate state = %v, %v, %q; want open for 30s", available, until, reason)
	}
}

func TestStream_TierSuccessAppliesQuotaHint(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	first := &countingProvider{name: "first", stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &quotaHintStream{
			trackedStream: trackedStream{chunks: []string{`{"model":"first"}`}},
			hint:          providers.RateLimit{Known: true, RemainingRequests: 0, ResetRequests: 2*time.Minute + 59*time.Second + 560*time.Millisecond},
		}, nil
	}}
	second := &countingProvider{name: "second", stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &trackedStream{chunks: []string{`{"model":"second"}`}}, nil
	}}
	set, err := providers.NewSet(first, second)
	if err != nil {
		t.Fatal(err)
	}
	refs := []providers.ModelRef{{Provider: "first", Model: "candidate"}, {Provider: "second", Model: "candidate"}}
	tracker, err := quota.New(quota.Options{Now: func() time.Time { return now }, Candidates: refs})
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.NewHandler(api.Options{
		Providers: set,
		Tracker:   tracker,
		Now:       func() time.Time { return now },
		LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger:    slog.New(slog.DiscardHandler),
		Tiers:     map[string][]providers.ModelRef{"demo": refs},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec := serveTierStream(t, h); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"model":"first"`) {
		t.Fatalf("first stream response = %d %s", rec.Code, rec.Body)
	}
	if available, until, _ := tracker.Available(refs[0]); available || !until.Equal(now.Add(2*time.Minute+59*time.Second+560*time.Millisecond)) {
		t.Fatalf("first candidate = %v, %v; want quota cooldown until 2m59.56s", available, until)
	}
	if rec := serveTierStream(t, h); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"model":"second"`) {
		t.Fatalf("spillover stream response = %d %s; want second candidate", rec.Code, rec.Body)
	}
	if first.calls.Load() != 1 || second.calls.Load() != 1 {
		t.Fatalf("provider calls = %d/%d, want 1/1", first.calls.Load(), second.calls.Load())
	}
}

func TestStream_TierExhaustionIsPlainHTTPError(t *testing.T) {
	t.Parallel()
	first := &countingProvider{name: "first", stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return nil, providers.ErrRateLimited
	}}
	second := &countingProvider{name: "second", stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return nil, providers.ErrUpstreamUnavailable
	}}
	rec := serveTierStream(t, newTierStreamHandler(t, first, second))
	assertError(t, rec, http.StatusServiceUnavailable, "api_error", "no_candidate_available")
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", got)
	}
	if strings.Contains(rec.Body.String(), "data:") || strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("exhaustion response is SSE: %s", rec.Body)
	}
}

func TestStream_TierClientDisconnectClosesPendingStream(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	closed := make(chan struct{})
	first := &countingProvider{name: "first", stream: func(ctx context.Context, _ providers.ChatRequest) (providers.Stream, error) {
		return &pendingStream{ctx: ctx, started: started, closed: closed}, nil
	}}
	second := &countingProvider{name: "second", stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &trackedStream{chunks: []string{`{"model":"second"}`}}, nil
	}}
	h := newTierStreamHandler(t, first, second)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"spillway/demo","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			done <- err
			return
		}
		req.Header.Set("Authorization", "Bearer "+keyCI)
		resp, err := srv.Client().Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	<-started
	cancel()
	<-closed
	<-done
	if second.calls.Load() != 0 {
		t.Errorf("second candidate calls = %d after disconnect, want 0", second.calls.Load())
	}
}

type trackedStream struct {
	chunks      []string
	afterChunks error
	recvErr     error
	closed      func()
	index       int
}

type quotaHintStream struct {
	trackedStream
	hint providers.RateLimit
}

func (s *quotaHintStream) RateLimit() providers.RateLimit { return s.hint }

func (s *trackedStream) Recv() (providers.ChatChunk, error) {
	if s.index < len(s.chunks) {
		chunk := providers.ChatChunk{Data: []byte(s.chunks[s.index])}
		s.index++
		return chunk, nil
	}
	if s.recvErr != nil {
		return providers.ChatChunk{}, s.recvErr
	}
	if s.afterChunks != nil {
		return providers.ChatChunk{}, s.afterChunks
	}
	return providers.ChatChunk{}, io.EOF
}

func (s *trackedStream) Close() error {
	if s.closed != nil {
		s.closed()
	}
	return nil
}

type pendingStream struct {
	ctx     context.Context
	started chan<- struct{}
	closed  chan<- struct{}
}

func (s *pendingStream) Recv() (providers.ChatChunk, error) {
	close(s.started)
	<-s.ctx.Done()
	return providers.ChatChunk{}, s.ctx.Err()
}

func (s *pendingStream) Close() error {
	close(s.closed)
	return nil
}

func newTierStreamHandler(t *testing.T, first, second providers.Provider) http.Handler {
	t.Helper()
	set, err := providers.NewSet(first, second)
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.NewHandler(api.Options{
		Providers: set,
		LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger:    slog.New(slog.DiscardHandler),
		Tiers: map[string][]providers.ModelRef{"demo": {
			{Provider: first.Name(), Model: "candidate"}, {Provider: second.Name(), Model: "candidate"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func serveTierStream(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"spillway/demo","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+keyCI)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func assertTierStreamHasNoKeys(t *testing.T, body string) {
	t.Helper()
	for _, key := range []string{keyCI, "sk-test-provider"} {
		if strings.Contains(body, key) {
			t.Errorf("SSE response leaks key %q: %s", key, body)
		}
	}
}

// A client that is already gone gets nothing written, streaming or not.
func TestChat_ClientGoneWritesNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	for name, body := range map[string]string{"chat": echoBody, "stream": echoStreamBody} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			req.Host = "127.0.0.1:8080"
			req.Header.Set("Authorization", "Bearer "+keyCI)
			rec := httptest.NewRecorder()

			e.handler.ServeHTTP(rec, req)

			if rec.Body.Len() != 0 {
				t.Errorf("body = %s, want nothing written for a gone client", rec.Body)
			}
		})
	}
}

func TestChat_UpstreamUnavailable(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)

	rec := e.do(t, request{auth: "Bearer " + keyCI, body: `{"model":"mock/stream-error","messages":[{"role":"user","content":"hi"}]}`})

	assertError(t, rec, http.StatusBadGateway, "api_error", "upstream_unavailable")
}

func TestStream_AccessLog(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)

	e.do(t, request{auth: "Bearer " + keyCursor, body: echoStreamBody})

	log := e.logs.String()
	for _, want := range []string{"status=200", "client_app=cursor", "provider=mock", "model=echo", "stream=true"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	for _, never := range []string{"hello spillway", "You", keyCursor} {
		if strings.Contains(log, never) {
			t.Errorf("log contains %q:\n%s", never, log)
		}
	}
}

// gatedStream yields its chunks, holding every Recv after the first until
// gate is closed or its context ends. It reports how it ended.
type gatedStream struct {
	ctx       context.Context
	chunks    []string
	gate      chan struct{}
	ctxErr    chan error // receives the context error Recv returned
	closed    chan struct{}
	closeOnce sync.Once
	sent      int
}

func newGatedStream(ctx context.Context, chunks ...string) *gatedStream {
	return &gatedStream{ctx: ctx, chunks: chunks, gate: make(chan struct{}), ctxErr: make(chan error, 1), closed: make(chan struct{})}
}

func (s *gatedStream) Recv() (providers.ChatChunk, error) {
	if s.sent > 0 {
		select {
		case <-s.gate:
		case <-s.ctx.Done():
			s.ctxErr <- s.ctx.Err()
			return providers.ChatChunk{}, s.ctx.Err()
		}
	}
	if s.sent == len(s.chunks) {
		return providers.ChatChunk{}, io.EOF
	}
	s.sent++
	return providers.ChatChunk{Data: json.RawMessage(s.chunks[s.sent-1])}, nil
}

func (s *gatedStream) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

// serveGated serves a handler over a real connection, with the fake provider
// streaming chunks through a gatedStream, and starts a streaming request to it.
func serveGated(ctx context.Context, t *testing.T, chunks ...string) (*gatedStream, io.Reader) {
	t.Helper()
	e := newEnv(t, api.LoopbackHosts("127.0.0.1"))
	streams := make(chan *gatedStream, 1)
	e.fake.stream = func(ctx context.Context, _ providers.ChatRequest) (providers.Stream, error) {
		s := newGatedStream(ctx, chunks...)
		streams <- s
		return s, nil
	}
	srv := httptest.NewServer(e.handler)
	t.Cleanup(srv.Close)

	body := postStream(ctx, t, srv, `{"model":"fake/x","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	return <-streams, body
}

// postStream sends a streaming request and returns the response body, which
// is closed when the test ends.
func postStream(ctx context.Context, t *testing.T, srv *httptest.Server, body string) io.Reader {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+keyCI)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	return resp.Body
}

func TestStream_FlushesEachChunk(t *testing.T) {
	t.Parallel()
	s, body := serveGated(t.Context(), t, `{"n":1}`, `{"n":2}`)
	br := bufio.NewReader(body)

	// The provider is holding chunk two, so chunk one can only arrive if it
	// was flushed on its own.
	if line, err := br.ReadString('\n'); err != nil || line != "data: {\"n\":1}\n" {
		t.Fatalf("first line = %q, %v; want chunk one", line, err)
	}
	close(s.gate)
	rest := readEvents(t, br)
	if want := []string{`{"n":2}`, "[DONE]"}; !slices.Equal(rest, want) {
		t.Errorf("remaining events = %q, want %q", rest, want)
	}
}

func TestStream_OutlivesServerIdleTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, api.LoopbackHosts("127.0.0.1"))
		e.fake.stream = func(context.Context, providers.ChatRequest) (providers.Stream, error) {
			return &delayedStream{delays: []time.Duration{121 * time.Second}, chunks: []string{`{"n":1}`, `{"n":2}`}}, nil
		}
		listener := fakenet.Listen()
		server := api.NewServer(e.handler, slog.New(slog.DiscardHandler))
		serveDone := make(chan error, 1)
		go func() { serveDone <- server.Serve(listener) }()
		t.Cleanup(func() { _ = server.Close(); _ = listener.Close() })
		client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return listener.DialContext(ctx, "", "")
		}}}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1/v1/chat/completions", strings.NewReader(fakeStreamBody))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+keyCI)
		type result struct {
			body []byte
			err  error
		}
		done := make(chan result, 1)
		go func() {
			resp, err := client.Do(req)
			if err != nil {
				done <- result{err: err}
				return
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			done <- result{body: body, err: err}
		}()
		got := <-done
		if got.err != nil || !strings.Contains(string(got.body), `data: [DONE]`) {
			t.Fatalf("stream response = %q, %v; want complete events", got.body, got.err)
		}
		_ = server.Close()
		_ = listener.Close()
		if err := <-serveDone; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve() = %v, want http.ErrServerClosed", err)
		}
	})
}

type delayedStream struct {
	chunks []string
	delays []time.Duration
	index  int
}

func (s *delayedStream) Recv() (providers.ChatChunk, error) {
	if s.index > 0 && s.index-1 < len(s.delays) {
		<-time.After(s.delays[s.index-1])
	}
	if s.index >= len(s.chunks) {
		return providers.ChatChunk{}, io.EOF
	}
	chunk := providers.ChatChunk{Data: []byte(s.chunks[s.index])}
	s.index++
	return chunk, nil
}

func (s *delayedStream) Close() error { return nil }

func TestStream_StalledClientStopsAtWriteDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnvWithTimeouts(t, api.LoopbackHosts("127.0.0.1"), time.Second, time.Second)
		stream := &largeStream{data: []byte(strings.Repeat("x", 2<<20)), closed: make(chan struct{})}
		e.fake.stream = func(context.Context, providers.ChatRequest) (providers.Stream, error) { return stream, nil }
		listener := fakenet.Listen()
		server := api.NewServer(e.handler, slog.New(slog.DiscardHandler))
		serveDone := make(chan error, 1)
		go func() { serveDone <- server.Serve(listener) }()
		t.Cleanup(func() { _ = server.Close(); _ = listener.Close() })
		client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return listener.DialContext(ctx, "", "")
		}}}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1/v1/chat/completions", strings.NewReader(fakeStreamBody))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+keyCI)
		responseDone := make(chan struct {
			response *http.Response
			err      error
		}, 1)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		go func() {
			response, err := client.Do(req)
			if response != nil {
				defer func() { _ = response.Body.Close() }()
			}
			responseDone <- struct {
				response *http.Response
				err      error
			}{response: response, err: err}
			<-release
		}()
		result := <-responseDone
		if result.err != nil {
			t.Fatal(result.err)
		}
		<-time.After(2 * time.Second)
		select {
		case <-stream.closed:
		default:
			t.Fatal("provider stream remains open after write deadline")
		}
		_ = server.Close()
		_ = listener.Close()
		if err := <-serveDone; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve() = %v, want http.ErrServerClosed", err)
		}
	})
}

func TestStream_ProviderTimeoutAfterFirstEventIsErrorEvent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, api.LoopbackHosts("127.0.0.1"))
		e.fake.stream = func(context.Context, providers.ChatRequest) (providers.Stream, error) {
			return &idleErrorStream{}, nil
		}
		listener := fakenet.Listen()
		server := api.NewServer(e.handler, slog.New(slog.DiscardHandler))
		serveDone := make(chan error, 1)
		go func() { serveDone <- server.Serve(listener) }()
		t.Cleanup(func() { _ = server.Close(); _ = listener.Close() })
		client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return listener.DialContext(ctx, "", "")
		}}}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1/v1/chat/completions", strings.NewReader(fakeStreamBody))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+keyCI)
		type result struct {
			status int
			body   []byte
			err    error
		}
		done := make(chan result, 1)
		go func() {
			response, err := client.Do(req)
			if err != nil {
				done <- result{err: err}
				return
			}
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(response.Body)
			done <- result{status: response.StatusCode, body: body, err: err}
		}()
		got := <-done
		if got.err != nil || got.status != http.StatusOK || !strings.Contains(string(got.body), `"code":"upstream_unavailable"`) || strings.Contains(string(got.body), "[DONE]") {
			t.Fatalf("stream response = %d %q, %v; want an OpenAI error event without [DONE]", got.status, got.body, got.err)
		}
		_ = server.Close()
		_ = listener.Close()
		if err := <-serveDone; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve() = %v, want http.ErrServerClosed", err)
		}
	})
}

type idleErrorStream struct{ sent bool }

func (s *idleErrorStream) Recv() (providers.ChatChunk, error) {
	if !s.sent {
		s.sent = true
		return providers.ChatChunk{Data: []byte(`{"n":1}`)}, nil
	}
	<-time.After(2 * time.Second)
	return providers.ChatChunk{}, providers.ErrUpstreamUnavailable
}

func (*idleErrorStream) Close() error { return nil }

type largeStream struct {
	data   []byte
	closed chan struct{}
	once   sync.Once
	sent   bool
}

func (s *largeStream) Recv() (providers.ChatChunk, error) {
	if s.sent {
		return providers.ChatChunk{}, io.EOF
	}
	s.sent = true
	return providers.ChatChunk{Data: s.data}, nil
}

func (s *largeStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// A client may receive an event split across any number of reads; framing
// must not depend on read boundaries.
func TestStream_PartialReads(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	whole := readEvents(t, e.do(t, request{auth: "Bearer " + keyCI, body: echoStreamBody}).Body)
	srv := httptest.NewServer(e.handler)
	t.Cleanup(srv.Close)

	for name, wrap := range map[string]func(io.Reader) io.Reader{
		"one byte": iotest.OneByteReader,
		"half":     iotest.HalfReader,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body := postStream(t.Context(), t, srv, echoStreamBody)
			if got := readEvents(t, wrap(body)); !slices.Equal(got, whole) {
				t.Errorf("events read in pieces = %q, want %q", got, whole)
			}
		})
	}
}

func TestStream_ClientDisconnectCancelsProvider(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	s, body := serveGated(ctx, t, `{"n":1}`, `{"n":2}`)
	if line, err := bufio.NewReader(body).ReadString('\n'); err != nil || line != "data: {\"n\":1}\n" {
		t.Fatalf("first line = %q, %v; want chunk one", line, err)
	}

	// The provider is now blocked on chunk two; the client walks away.
	cancel()

	// Blocks, and so fails at the test timeout, if the stream is never closed.
	<-s.closed
	// Close runs after the handler's Recv loop, so Recv has already returned.
	select {
	case err := <-s.ctxErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("provider saw %v, want context.Canceled", err)
		}
	default:
		t.Error("provider Recv did not end on the cancelled context")
	}
}
