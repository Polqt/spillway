package e2e_test

import (
	"encoding/json"
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
	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/providers/openaicompat"
	"github.com/Polqt/spillway/internal/redact"
)

const (
	groqProviderKey = "sk-test-groq-e2e"
	groqLocalKey    = "sk-test-local-e2e"
)

func TestGroq_EndToEndChatStreamAndModels(t *testing.T) {
	chatFixture := groqFixture(t, "providers/openaicompat/testdata/chat.json")
	modelsFixture := groqFixture(t, "providers/openaicompat/testdata/models.json")
	streamFixture := groqFixture(t, "providers/openaicompat/testdata/stream.sse")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+groqProviderKey {
			t.Errorf("upstream Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Client-Only") != "" {
			t.Errorf("client-only header reached provider")
		}
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write(modelsFixture)
		case "/v1/chat/completions":
			body, _ := io.ReadAll(r.Body)
			var request struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal(body, &request); err != nil || request.Model != "llama-3.1-8b-instant" {
				t.Errorf("forwarded request model = %q, %v", request.Model, err)
			}
			if r.Header.Get("Accept") == "text/event-stream" {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(streamFixture)
				return
			}
			_, _ = w.Write(chatFixture)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	provider, err := openaicompat.New(openaicompat.Options{
		Name: "groq", BaseURL: upstream.URL + "/v1", APIKey: redact.NewSecret(groqProviderKey), Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	set, err := providers.NewSet(provider)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.Options{
		Providers: set, LocalKeys: []api.LocalKey{{Name: "e2e", Key: redact.NewSecret(groqLocalKey)}}, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	chat := groqRequest(t, server, http.MethodPost, "/v1/chat/completions", `{"model":"groq/llama-3.1-8b-instant","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"x"}}]}`)
	if chat.Status != http.StatusOK || !strings.Contains(chat.Body, "Hello from Groq.") || chat.Header.Get("X-Spillway-Provider") != "groq" {
		t.Errorf("chat = %d %s, headers %v", chat.Status, chat.Body, chat.Header)
	}
	if got := groqRequest(t, server, http.MethodPost, "/v1/chat/completions", `{"model":"groq/llama-3.1-8b-instant","messages":[{"role":"user","content":"hello"}],"stream":true}`); got.Status != http.StatusOK || got.Header.Get("Content-Type") != "text/event-stream" || !strings.Contains(got.Body, "[DONE]") {
		t.Errorf("stream = %d %s, headers %v", got.Status, got.Body, got.Header)
	}
	models := groqRequest(t, server, http.MethodGet, "/v1/models", "")
	if models.Status != http.StatusOK || !strings.Contains(models.Body, `"id":"groq/llama-3.1-8b-instant"`) {
		t.Errorf("models = %d %s", models.Status, models.Body)
	}
}

func TestGroq_UpstreamErrorsBecomeOpenAIErrors(t *testing.T) {
	tests := []struct {
		status     int
		wantStatus int
		want       string
	}{
		{http.StatusTooManyRequests, http.StatusTooManyRequests, "rate_limit_exceeded"},
		{http.StatusUnauthorized, http.StatusBadGateway, "upstream_unauthorized"},
		{http.StatusForbidden, http.StatusBadGateway, "upstream_unauthorized"},
		{http.StatusBadRequest, http.StatusBadRequest, "provider_bad_request"},
		{http.StatusRequestEntityTooLarge, http.StatusBadRequest, "context_length_exceeded"},
		{http.StatusNotFound, http.StatusNotFound, "model_not_found"},
		{http.StatusServiceUnavailable, http.StatusBadGateway, "upstream_unavailable"},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "9")
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, `{"error":{"message":"upstream said `+groqProviderKey+`"}}`)
			}))
			defer upstream.Close()
			p, err := openaicompat.New(openaicompat.Options{Name: "groq", BaseURL: upstream.URL, APIKey: redact.NewSecret(groqProviderKey), Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			set, _ := providers.NewSet(p)
			h, err := api.NewHandler(api.Options{Providers: set, LocalKeys: []api.LocalKey{{Name: "e2e", Key: redact.NewSecret(groqLocalKey)}}, Logger: slog.New(slog.DiscardHandler)})
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(h)
			defer srv.Close()
			got := groqRequest(t, srv, http.MethodPost, "/v1/chat/completions", `{"model":"groq/model","messages":[{"role":"user","content":"hi"}]}`)
			var body struct {
				Error struct {
					Type string `json:"type"`
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(got.Body), &body); err != nil || got.Status != tt.wantStatus || body.Error.Code != tt.want || strings.Contains(got.Body, groqProviderKey) {
				t.Errorf("response = %d %s; decode error %v", got.Status, got.Body, err)
			}
			if tt.status == http.StatusTooManyRequests && got.Header.Get("Retry-After") != "9" {
				t.Errorf("Retry-After = %q, want 9", got.Header.Get("Retry-After"))
			}
		})
	}
}

type groqReply struct {
	Status int
	Header http.Header
	Body   string
}

func groqRequest(t *testing.T, server *httptest.Server, method, path, body string) groqReply {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+groqLocalKey)
	req.Header.Set("X-Client-Only", "do-not-forward")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return groqReply{Status: resp.StatusCode, Header: resp.Header, Body: string(data)}
}

func groqFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", name)
	data, err := os.ReadFile(path) // #nosec G304 -- callers use fixed fixture paths.
	if err != nil {
		t.Fatal(err)
	}
	return data
}
