package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Polqt/spillway/internal/api"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/providers/mock"
	"github.com/Polqt/spillway/internal/redact"
	"github.com/Polqt/spillway/internal/router"
	"github.com/Polqt/spillway/internal/store"
)

func TestChat_RecordsUsageAndRoutingMetadata(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	usageStore, err := store.Open(t.Context(), store.Options{Path: dbPath, Retention: 90 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = usageStore.Close() }()

	mockProvider, err := mock.New(mock.Options{Now: func() time.Time { return time.Unix(1760400000, 0) }})
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
		{body: `{"model":"spillway/demo","messages":[{"role":"user","content":"hello"}]}`, status: http.StatusOK, route: "tier demo; candidate 2/2", model: "spillway/demo", provider: "mock", attempts: 2, usage: "reported", prompt: 1, completion: 3},
		{body: `{"model":"auto","messages":[{"role":"user","content":"hello"}]}`, status: http.StatusOK, route: "auto; tier auto-tier; candidate 1/1", model: "auto", provider: "mock", attempts: 1, usage: "reported", prompt: 1, completion: 3},
		{body: `{"model":"auto","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"x"}}]}`, status: http.StatusBadRequest, route: "auto; needs tools; no capable model", model: "auto", usage: "none", errorKind: "no_capable_model"},
		{body: `{"model":"spillway/exhausted","messages":[{"role":"user","content":"hello"}]}`, status: http.StatusServiceUnavailable, route: "tier exhausted; exhausted (1 eligible)", model: "spillway/exhausted", attempts: 1, usage: "none", errorKind: "no_candidate_available"},
		{body: `{"model":"bad/request","messages":[{"role":"user","content":"hello"}]}`, status: http.StatusBadRequest, route: "direct", model: "bad/request", provider: "bad", attempts: 1, usage: "none", errorKind: "bad_request"},
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
