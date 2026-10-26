package api_test

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Polqt/spillway/internal/api"
	"github.com/Polqt/spillway/internal/budget"
	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/providers/openaicompat"
	"github.com/Polqt/spillway/internal/redact"
	"github.com/Polqt/spillway/internal/router"
	"github.com/Polqt/spillway/internal/store"
	fakeprovider "github.com/Polqt/spillway/tests/testutil/fakeprovider"
)

func TestChat_RecordsUsageAndRoutingMetadata(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	usageStore, err := store.Open(t.Context(), store.Options{Path: dbPath, Retention: 90 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = usageStore.Close() }()

	mockProvider, err := fakeprovider.New(fakeprovider.Options{Now: func() time.Time { return time.Unix(1760400000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	failing := &countingProvider{name: "bad", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{}, providers.ErrBadRequest
	}}
	plain := &countingProvider{name: "plain", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{Body: json.RawMessage(`{"object":"chat.completion","choices":[{"message":{"content":"distinctive-response-should-not-persist"}}]}`)}, nil
	}}
	set, err := providers.NewSet(mockProvider, failing, plain)
	if err != nil {
		t.Fatal(err)
	}
	planner, err := router.New(router.Options{
		Tiers: map[string][]providers.ModelRef{
			"demo":      {{Provider: "mock", Model: "unavailable"}, {Provider: "mock", Model: "echo"}},
			"auto-tier": {{Provider: "mock", Model: "echo"}},
			"exhausted": {{Provider: "mock", Model: "unauthorized"}},
		},
		Auto: []string{"auto-tier"},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.Options{
		Providers:     set,
		Router:        planner,
		LocalKeys:     []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger:        slog.New(slog.DiscardHandler),
		UsageRecorder: usageStore,
	})
	if err != nil {
		t.Fatal(err)
	}

	requests := []struct {
		body       string
		status     int
		route      string
		model      string
		provider   string
		attempts   int
		usage      string
		prompt     int64
		completion int64
		errorKind  string
	}{
		{body: echoBody, status: http.StatusOK, route: "direct", model: "mock/echo", provider: "mock", attempts: 1, usage: "reported", prompt: 2, completion: 4},
		{body: `{"model":"spillway/demo","messages":[{"role":"user","content":"hello"}]}`, status: http.StatusOK, route: "tier demo; candidate 2/2", model: "spillway/demo", provider: "mock", attempts: 2, usage: "estimated", prompt: 1, completion: 3},
		{body: `{"model":"auto","messages":[{"role":"user","content":"hello"}]}`, status: http.StatusOK, route: "auto; tier auto-tier; candidate 1/1", model: "auto", provider: "mock", attempts: 1, usage: "reported", prompt: 1, completion: 3},
		{body: `{"model":"auto","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"x"}}]}`, status: http.StatusBadRequest, route: "auto; needs tools; no capable model", model: "auto", usage: "none", errorKind: "no_capable_model"},
		{body: `{"model":"spillway/exhausted","messages":[{"role":"user","content":"hello"}]}`, status: http.StatusServiceUnavailable, route: "tier exhausted; exhausted (1 eligible)", model: "spillway/exhausted", attempts: 1, usage: "estimated", prompt: 2, errorKind: "no_candidate_available"},
		{body: `{"model":"bad/request","messages":[{"role":"user","content":"hello"}]}`, status: http.StatusBadRequest, route: "direct", model: "bad/request", provider: "bad", attempts: 1, usage: "estimated", prompt: 2, errorKind: "bad_request"},
		{body: `{"model":"plain/model","messages":[{"role":"user","content":"distinctive-prompt-should-not-persist"}]}`, status: http.StatusOK, route: "direct", model: "plain/model", provider: "plain", attempts: 1, usage: "none"},
	}
	for _, test := range requests {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(test.body))
		req.Header.Set("Authorization", "Bearer "+keyCI)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != test.status {
			t.Errorf("request %s status = %d, want %d: %s", test.model, rec.Code, test.status, rec.Body)
		}
		if got := rec.Header().Get("X-Spillway-Route"); got != test.route {
			t.Errorf("request %s route = %q, want %q", test.model, got, test.route)
		}
	}

	reader, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	rows, err := reader.QueryContext(t.Context(), `SELECT id, requested_model, route, provider, model, status, error_kind, prompt_tokens, completion_tokens, usage_source, skips_json FROM requests ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for i, test := range requests {
		if !rows.Next() {
			t.Fatalf("missing row %d: %v", i, rows.Err())
		}
		var id int64
		var model, route, provider, served, errorKind, usage string
		var status int
		var prompt, completion int64
		var skipsJSON string
		if err := rows.Scan(&id, &model, &route, &provider, &served, &status, &errorKind, &prompt, &completion, &usage, &skipsJSON); err != nil {
			t.Fatal(err)
		}
		if model != test.model || route != test.route || provider != test.provider || served != expectedModel(test.provider, test.model) || status != test.status ||
			prompt != test.prompt || completion != test.completion || usage != test.usage || errorKind != test.errorKind {
			t.Errorf("row %d = model %q route %q provider %q served %q status %d tokens %d/%d source %q", i, model, route, provider, served, status, prompt, completion, usage)
		}
		if test.errorKind == "no_capable_model" {
			var skips []store.Skip
			if err := json.Unmarshal([]byte(skipsJSON), &skips); err != nil || len(skips) != 1 || skips[0].Reason != "capabilities_undeclared" {
				t.Errorf("plan error skips = %s, want one fixed capabilities_undeclared reason (err %v)", skipsJSON, err)
			}
		}
		var attemptCount int
		if err := reader.QueryRowContext(t.Context(), `SELECT count(*) FROM attempts WHERE request_id = ?`, id).Scan(&attemptCount); err != nil {
			t.Fatal(err)
		}
		if attemptCount != test.attempts {
			t.Errorf("request %s has %d attempts, want %d", test.model, attemptCount, test.attempts)
		}
		attemptRows, err := reader.QueryContext(t.Context(), `SELECT provider, model, outcome, latency_ms FROM attempts WHERE request_id = ? ORDER BY id`, id)
		if err != nil {
			t.Fatal(err)
		}
		for attemptRows.Next() {
			var provider, model, outcome string
			var latency int64
			if err := attemptRows.Scan(&provider, &model, &outcome, &latency); err != nil {
				_ = attemptRows.Close()
				t.Fatal(err)
			}
			if provider == "" || model == "" || outcome == "" || latency < 0 {
				t.Errorf("request %s has incomplete attempt metadata: %q %q %q %d", test.model, provider, model, outcome, latency)
			}
		}
		if err := attemptRows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if rows.Next() {
		t.Fatal("got more usage rows than requests")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(dbPath + suffix) // #nosec G304 -- dbPath is inside t.TempDir.
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"distinctive-prompt-should-not-persist", "distinctive-response-should-not-persist", keyCI} {
			if strings.Contains(string(data), forbidden) {
				t.Errorf("usage database file %q contains %q", dbPath+suffix, forbidden)
			}
		}
	}
}

func TestChat_PriceFailedAttemptsAndSumRequestCost(t *testing.T) {
	tests := []struct {
		name           string
		providerBody   []byte
		wantCost       int64
		wantCostSource string
		wantUsage      string
		useAdapter     bool
	}{
		{name: "failed paid attempt without usage is estimated", wantCost: 6, wantCostSource: "mixed", wantUsage: "estimated"},
		{name: "failed paid attempt with OpenRouter usage uses reported cost", providerBody: readOpenRouterCostFixture(t), wantCost: 124, wantCostSource: "mixed", wantUsage: "reported", useAdapter: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "usage.db")
			usageStore, err := store.Open(t.Context(), store.Options{Path: dbPath, Retention: 90 * 24 * time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = usageStore.Close() }()
			freeProvider, err := fakeprovider.New(fakeprovider.Options{Now: time.Now})
			if err != nil {
				t.Fatal(err)
			}
			var paid providers.Provider
			if tt.useAdapter {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write(tt.providerBody)
				}))
				t.Cleanup(upstream.Close)
				paid, err = openaicompat.New(openaicompat.Options{
					Name: "openrouter", BaseURL: upstream.URL, APIKey: redact.NewSecret("sk-test-provider"), Timeout: time.Second,
				})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				paid = &costTestProvider{name: "openrouter", body: tt.providerBody, err: providers.ErrUpstreamUnavailable}
			}
			set, err := providers.NewSet(paid, freeProvider)
			if err != nil {
				t.Fatal(err)
			}
			estimator := budget.NewPricer(config.Config{
				Providers: map[string]config.Provider{"openrouter": {}, "mock": {Type: "ollama"}},
				Pricing:   map[string]config.Price{"openrouter/anthropic/claude-sonnet-4.5": {InputMicroUSDPerMillion: 3_000_000, OutputMicroUSDPerMillion: 15_000_000}},
			})
			handler, err := api.NewHandler(api.Options{
				Providers: set,
				Tiers: map[string][]providers.ModelRef{"demo": {
					{Provider: "openrouter", Model: "anthropic/claude-sonnet-4.5"},
					{Provider: "mock", Model: "echo"},
				}},
				LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}}, Logger: slog.New(slog.DiscardHandler),
				UsageRecorder: usageStore, CostEstimator: estimator,
			})
			if err != nil {
				t.Fatal(err)
			}
			rec := sendUsageStreamRequest(t.Context(), t, handler, `{"model":"spillway/demo","messages":[{"role":"user","content":"hello"}]}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
			reader, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reader.Close() }()
			var cost int64
			var source, usageSource string
			if err := reader.QueryRowContext(t.Context(), "SELECT cost_micro_usd, cost_source, usage_source FROM requests").Scan(&cost, &source, &usageSource); err != nil {
				t.Fatal(err)
			}
			if cost != tt.wantCost || source != tt.wantCostSource || usageSource != tt.wantUsage {
				t.Errorf("request cost = %d/%s usage=%s, want %d/%s usage=%s", cost, source, usageSource, tt.wantCost, tt.wantCostSource, tt.wantUsage)
			}
			rows, err := reader.QueryContext(t.Context(), "SELECT provider, cost_micro_usd, cost_source, usage_source FROM attempts ORDER BY id")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rows.Close() }()
			var provider string
			var failedCost int64
			var failedSource, failedUsage string
			if !rows.Next() || rows.Scan(&provider, &failedCost, &failedSource, &failedUsage) != nil {
				t.Fatal("missing failed paid attempt")
			}
			wantAttemptSource := "declared"
			if tt.useAdapter {
				wantAttemptSource = "reported"
			}
			if provider != "openrouter" || failedCost != tt.wantCost || failedSource != wantAttemptSource {
				t.Errorf("failed attempt = %s %d/%s, want openrouter %d/%s", provider, failedCost, failedSource, tt.wantCost, wantAttemptSource)
			}
			if !rows.Next() {
				t.Fatal("missing successful free attempt")
			}
			var freeCost int64
			var freeSource string
			if err := rows.Scan(&provider, &freeCost, &freeSource, &failedUsage); err != nil {
				t.Fatal(err)
			}
			if provider != "mock" || freeCost != 0 || freeSource != "free" {
				t.Errorf("free attempt = %s %d/%s, want mock 0/free", provider, freeCost, freeSource)
			}
		})
	}
}

func readOpenRouterCostFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/openrouter-cost-response.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type costTestProvider struct {
	name string
	body []byte
	err  error
}

func (p *costTestProvider) Name() string                                    { return p.name }
func (*costTestProvider) Models(context.Context) ([]providers.Model, error) { return nil, nil }
func (p *costTestProvider) Chat(context.Context, providers.ChatRequest) (providers.ChatResponse, error) {
	return providers.ChatResponse{Body: p.body}, p.err
}
func (*costTestProvider) ChatStream(context.Context, providers.ChatRequest) (providers.Stream, error) {
	return nil, providers.ErrUpstreamUnavailable
}

func expectedModel(provider, requested string) string {
	if provider == "" {
		return ""
	}
	if strings.HasPrefix(requested, "spillway/") || requested == "auto" {
		return "echo"
	}
	if strings.Contains(requested, "/") {
		return requested[strings.IndexByte(requested, '/')+1:]
	}
	return ""
}

func TestChat_UsageWriteContextSurvivesClientCancellation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	usageStore, err := store.Open(t.Context(), store.Options{Path: dbPath, Retention: 90 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = usageStore.Close() }()
	started := make(chan struct{})
	provider := &cancelingProvider{started: started}
	set, err := providers.NewSet(provider)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.Options{
		Providers:     set,
		LocalKeys:     []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger:        slog.New(slog.DiscardHandler),
		UsageRecorder: usageStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"cancel/echo","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer "+keyCI)
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	<-started
	cancel()
	<-done
	var count int
	if err := usageStoreDBCount(t, dbPath, &count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("recorded rows = %d, want 1 after client cancellation", count)
	}
}

func usageStoreDBCount(t *testing.T, path string, count *int) error {
	t.Helper()
	reader, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	return reader.QueryRowContext(t.Context(), "SELECT count(*) FROM requests").Scan(count)
}

func TestStream_UsageCaptureInjectsAndHidesReportedUsage(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	usageStore, err := store.Open(t.Context(), store.Options{Path: dbPath, Retention: 90 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = usageStore.Close() }()

	var upstreamBody string
	provider := &usageStreamProvider{stream: func(_ context.Context, req providers.ChatRequest) (providers.Stream, error) {
		upstreamBody = string(req.RawBody)
		return &usageTestStream{chunks: []providers.ChatChunk{
			{Data: json.RawMessage(`{"choices":[{"delta":{"content":"hello"}}],"usage":{"prompt_tokens":7,"completion_tokens":5}}`), Usage: &providers.Usage{PromptTokens: 7, CompletionTokens: 5}},
			{Data: json.RawMessage(`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":5}}`), Usage: &providers.Usage{PromptTokens: 7, CompletionTokens: 5}},
		}}, nil
	}}
	handler := newUsageStreamHandler(t, provider, usageStore)
	request := `{"model":"capture/model","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	rec := sendUsageStreamRequest(t.Context(), t, handler, request)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var forwarded struct {
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal([]byte(upstreamBody), &forwarded); err != nil || !forwarded.StreamOptions.IncludeUsage {
		t.Fatalf("upstream body = %s, %v; want stream_options.include_usage", upstreamBody, err)
	}
	if strings.Contains(rec.Body.String(), `"choices":[]`) || strings.Contains(rec.Body.String(), `"usage"`) {
		t.Fatalf("client received injected usage data: %s", rec.Body)
	}
	var prompt, completion int
	var source string
	if err := usageStoreDBUsage(t, dbPath, &prompt, &completion, &source); err != nil {
		t.Fatal(err)
	}
	if prompt != 7 || completion != 5 || source != "reported" {
		t.Errorf("recorded usage = %d/%d %q; want 7/5 reported", prompt, completion, source)
	}
}

func TestStream_ClientUsageRequestIsRelayedUnchanged(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	usageStore, err := store.Open(t.Context(), store.Options{Path: dbPath, Retention: 90 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = usageStore.Close() }()
	usage, err := json.Marshal(providers.Usage{PromptTokens: 4, CompletionTokens: 3, TotalTokens: 7})
	if err != nil {
		t.Fatal(err)
	}
	usageChunk := json.RawMessage(`{"choices":[],"usage":` + string(usage) + `}`)
	var upstreamBody string
	provider := &usageStreamProvider{stream: func(_ context.Context, req providers.ChatRequest) (providers.Stream, error) {
		upstreamBody = string(req.RawBody)
		return &usageTestStream{chunks: []providers.ChatChunk{{Data: usageChunk, Usage: &providers.Usage{PromptTokens: 4, CompletionTokens: 3, TotalTokens: 7}}}}, nil
	}}
	handler := newUsageStreamHandler(t, provider, usageStore)
	request := `{"model":"capture/model","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hello"}]}`
	rec := sendUsageStreamRequest(t.Context(), t, handler, request)
	if upstreamBody != request {
		t.Errorf("upstream body = %s, want original %s", upstreamBody, request)
	}
	if !strings.Contains(rec.Body.String(), string(usageChunk)) {
		t.Errorf("client did not receive requested usage chunk: %s", rec.Body)
	}
	var prompt, completion int
	var source string
	if err := usageStoreDBUsage(t, dbPath, &prompt, &completion, &source); err != nil {
		t.Fatal(err)
	}
	if prompt != 4 || completion != 3 || source != "reported" {
		t.Errorf("recorded usage = %d/%d %q; want 4/3 reported", prompt, completion, source)
	}
}

func TestStream_MissingUsageEstimatesRelayedDelta(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	usageStore, err := store.Open(t.Context(), store.Options{Path: dbPath, Retention: 90 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = usageStore.Close() }()
	provider := &usageStreamProvider{stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &usageTestStream{chunks: []providers.ChatChunk{
			{Data: json.RawMessage(`{"choices":[{"delta":{"content":"abc"}}]}`)},
			{Data: json.RawMessage(`{"choices":[{"delta":{"content":[{"type":"text","text":"defg"},{"type":"image"}]}}]}`)},
			{Data: json.RawMessage(`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"hij"}}]}}]}`)},
		}}, nil
	}}
	handler := newUsageStreamHandler(t, provider, usageStore)
	request := `{"model":"capture/model","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	_ = sendUsageStreamRequest(t.Context(), t, handler, request)
	var prompt, completion int
	var source string
	if err := usageStoreDBUsage(t, dbPath, &prompt, &completion, &source); err != nil {
		t.Fatal(err)
	}
	if prompt != 2 || completion != 29 || source != "estimated" {
		t.Errorf("recorded usage = %d/%d %q; want 2/29 estimated", prompt, completion, source)
	}
}

func TestStream_MissingUsageEstimatesToolCallName(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	usageStore, err := store.Open(t.Context(), store.Options{Path: dbPath, Retention: 90 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = usageStore.Close() }()
	provider := &usageStreamProvider{stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &usageTestStream{chunks: []providers.ChatChunk{{Data: json.RawMessage(`{"choices":[{"delta":{"tool_calls":[{"function":{"name":"run"}}]}}]}`)}}}, nil
	}}
	handler := newUsageStreamHandler(t, provider, usageStore)
	request := `{"model":"capture/model","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	_ = sendUsageStreamRequest(t.Context(), t, handler, request)
	var prompt, completion int
	var source string
	if err := usageStoreDBUsage(t, dbPath, &prompt, &completion, &source); err != nil {
		t.Fatal(err)
	}
	if prompt != 2 || completion != 10 || source != "estimated" {
		t.Errorf("recorded usage = %d/%d %q; want 2/10 estimated", prompt, completion, source)
	}
}

func TestStream_PartialSSEUsageChunkIsCapturedAndHidden(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	usageStore, err := store.Open(t.Context(), store.Options{Path: dbPath, Retention: 90 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = usageStore.Close() }()

	includeUsage := make(chan bool, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			StreamOptions struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		includeUsage <- body.StreamOptions.IncludeUsage
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream writer does not support flushing")
			return
		}
		for _, part := range []string{
			`data: {"choices":[{"delta":{"content":"ok"}}]}`,
			"\n\n",
			`data: {"choices":[],"usage":{"prompt_tokens":7,`,
			`"completion_tokens":5,"total_tokens":12}}`,
			"\n\n",
			"data: [DONE]\n\n",
		} {
			if _, err := io.WriteString(w, part); err != nil {
				t.Errorf("write upstream event: %v", err)
				return
			}
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)
	provider, err := openaicompat.New(openaicompat.Options{
		Name: "capture", BaseURL: upstream.URL, APIKey: redact.NewSecret("sk-test-capture"), Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := newUsageStreamHandler(t, provider, usageStore)
	request := `{"model":"capture/model","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	rec := sendUsageStreamRequest(t.Context(), t, handler, request)
	if !<-includeUsage {
		t.Error("upstream request omitted stream_options.include_usage")
	}
	if strings.Contains(rec.Body.String(), `"usage"`) || !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Errorf("client stream = %s; want content without usage", rec.Body)
	}
	var prompt, completion int
	var source string
	if err := usageStoreDBUsage(t, dbPath, &prompt, &completion, &source); err != nil {
		t.Fatal(err)
	}
	if prompt != 7 || completion != 5 || source != "reported" {
		t.Errorf("recorded usage = %d/%d %q; want 7/5 reported", prompt, completion, source)
	}
}

func TestStream_MidstreamErrorRecordsEstimatedUsage(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	usageStore, err := store.Open(t.Context(), store.Options{Path: dbPath, Retention: 90 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = usageStore.Close() }()
	provider := &usageStreamProvider{stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &usageTestStream{
			chunks: []providers.ChatChunk{{Data: json.RawMessage(`{"choices":[{"delta":{"content":"1234567"}}]}`)}},
			endErr: providers.ErrUpstreamUnavailable,
		}, nil
	}}
	handler := newUsageStreamHandler(t, provider, usageStore)
	request := `{"model":"capture/model","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	rec := sendUsageStreamRequest(t.Context(), t, handler, request)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"code":"upstream_unavailable"`) {
		t.Fatalf("stream response = %d %s; want OpenAI mid-stream error", rec.Code, rec.Body)
	}
	var prompt, completion int
	var source string
	if err := usageStoreDBUsage(t, dbPath, &prompt, &completion, &source); err != nil {
		t.Fatal(err)
	}
	if prompt != 2 || completion != 3 || source != "estimated" {
		t.Errorf("recorded usage = %d/%d %q; want 2/3 estimated", prompt, completion, source)
	}
}

func TestStream_MidstreamFailurePricesRelayedCompletion(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	usageStore, err := store.Open(t.Context(), store.Options{Path: dbPath, Retention: 90 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = usageStore.Close() }()
	provider := &usageStreamProvider{stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &usageTestStream{
			chunks: []providers.ChatChunk{{Data: json.RawMessage(`{"choices":[{"delta":{"content":"1234567"}}]}`)}},
			endErr: providers.ErrUpstreamUnavailable,
		}, nil
	}}
	pricer := budget.NewPricer(config.Config{
		Providers: map[string]config.Provider{"capture": {}},
		Pricing:   map[string]config.Price{"capture/model": {InputMicroUSDPerMillion: 3_000_000, OutputMicroUSDPerMillion: 15_000_000}},
	})
	handler := newUsageStreamHandler(t, provider, usageStore, pricer)
	rec := sendUsageStreamRequest(t.Context(), t, handler, `{"model":"capture/model","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"code":"upstream_unavailable"`) {
		t.Fatalf("stream response = %d %s; want mid-stream error", rec.Code, rec.Body)
	}
	reader, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	var prompt, completion, cost int64
	var usageSource, costSource string
	if err := reader.QueryRowContext(t.Context(), "SELECT prompt_tokens, completion_tokens, usage_source, cost_micro_usd, cost_source FROM requests").Scan(&prompt, &completion, &usageSource, &cost, &costSource); err != nil {
		t.Fatal(err)
	}
	if prompt != 2 || completion != 3 || usageSource != "estimated" || cost != 51 || costSource != "declared" {
		t.Errorf("recorded stream failure = %d/%d usage=%s cost=%d/%s; want 2/3 estimated cost=51/declared", prompt, completion, usageSource, cost, costSource)
	}
}

func TestStream_ClientDisconnectRecordsEstimatedUsage(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	usageStore, err := store.Open(t.Context(), store.Options{Path: dbPath, Retention: 90 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = usageStore.Close() }()
	provider := &usageStreamProvider{stream: func(ctx context.Context, _ providers.ChatRequest) (providers.Stream, error) {
		return &usageTestStream{
			ctx: ctx, waitForCancel: true,
			chunks: []providers.ChatChunk{{Data: json.RawMessage(`{"choices":[{"delta":{"content":"1234567"}}]}`)}},
		}, nil
	}}
	recorded := make(chan struct{})
	recorder := usageRecorderFunc(func(ctx context.Context, request store.Request) error {
		if err := usageStore.Record(ctx, request); err != nil {
			return err
		}
		close(recorded)
		return nil
	})
	handler := newUsageStreamHandler(t, provider, recorder)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(t.Context())
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"capture/model","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+keyCI)
	response, err := srv.Client().Do(request)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || line != "data: {\"choices\":[{\"delta\":{\"content\":\"1234567\"}}]}\n" {
		cancel()
		_ = response.Body.Close()
		t.Fatalf("first event = %q, %v", line, err)
	}
	cancel()
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-recorded:
	case <-time.After(5 * time.Second):
		t.Fatal("usage record was not written after client disconnect")
	}
	var prompt, completion int
	var source string
	if err := usageStoreDBUsage(t, dbPath, &prompt, &completion, &source); err != nil {
		t.Fatal(err)
	}
	if prompt != 2 || completion != 3 || source != "estimated" {
		t.Errorf("recorded usage = %d/%d %q; want 2/3 estimated", prompt, completion, source)
	}
}

func TestStream_StoreOffDoesNotChangeRequestOrChunks(t *testing.T) {
	const request = `{"model":"capture/model","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	const chunk = `{"choices":[{"delta":{"content":"hello"}}]}`
	var upstreamBody string
	provider := &usageStreamProvider{stream: func(_ context.Context, req providers.ChatRequest) (providers.Stream, error) {
		upstreamBody = string(req.RawBody)
		return &usageTestStream{chunks: []providers.ChatChunk{{Data: json.RawMessage(chunk)}}}, nil
	}}
	handler := newUsageStreamHandler(t, provider, nil)
	rec := sendUsageStreamRequest(t.Context(), t, handler, request)
	if upstreamBody != request {
		t.Errorf("store-off upstream request = %s, want %s", upstreamBody, request)
	}
	if want := "data: " + chunk + "\n\n" + "data: [DONE]\n\n"; rec.Body.String() != want {
		t.Errorf("store-off stream = %q, want byte-for-byte %q", rec.Body.String(), want)
	}
}

func newUsageStreamHandler(t *testing.T, provider providers.Provider, recorder interface {
	Record(context.Context, store.Request) error
}, estimators ...*budget.Pricer) http.Handler {
	t.Helper()
	set, err := providers.NewSet(provider)
	if err != nil {
		t.Fatal(err)
	}
	var estimator *budget.Pricer
	if len(estimators) > 0 {
		estimator = estimators[0]
	}
	var costEstimator interface {
		Estimate(providers.ModelRef, int64, int64, json.RawMessage) budget.Cost
	}
	if estimator != nil {
		costEstimator = estimator
	}
	handler, err := api.NewHandler(api.Options{
		Providers: set, LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger: slog.New(slog.DiscardHandler), UsageRecorder: recorder, CostEstimator: costEstimator,
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

type usageRecorderFunc func(context.Context, store.Request) error

func (f usageRecorderFunc) Record(ctx context.Context, request store.Request) error {
	return f(ctx, request)
}

func sendUsageStreamRequest(ctx context.Context, t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+keyCI)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func usageStoreDBUsage(t *testing.T, path string, prompt, completion *int, source *string) error {
	t.Helper()
	reader, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	return reader.QueryRowContext(t.Context(), "SELECT prompt_tokens, completion_tokens, usage_source FROM requests").Scan(prompt, completion, source)
}

type usageStreamProvider struct {
	stream func(context.Context, providers.ChatRequest) (providers.Stream, error)
}

func (*usageStreamProvider) Name() string                                      { return "capture" }
func (*usageStreamProvider) Models(context.Context) ([]providers.Model, error) { return nil, nil }
func (*usageStreamProvider) Chat(context.Context, providers.ChatRequest) (providers.ChatResponse, error) {
	return providers.ChatResponse{}, errors.New("not used")
}
func (p *usageStreamProvider) ChatStream(ctx context.Context, req providers.ChatRequest) (providers.Stream, error) {
	return p.stream(ctx, req)
}

type usageTestStream struct {
	chunks        []providers.ChatChunk
	index         int
	ctx           context.Context
	waitForCancel bool
	endErr        error
}

func (s *usageTestStream) Recv() (providers.ChatChunk, error) {
	if s.index == len(s.chunks) {
		if s.waitForCancel {
			<-s.ctx.Done()
			return providers.ChatChunk{}, s.ctx.Err()
		}
		if s.endErr != nil {
			return providers.ChatChunk{}, s.endErr
		}
		return providers.ChatChunk{}, io.EOF
	}
	chunk := s.chunks[s.index]
	s.index++
	return chunk, nil
}
func (*usageTestStream) Close() error { return nil }
