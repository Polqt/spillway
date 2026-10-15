package gemini_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/providers/gemini"
	"github.com/Polqt/spillway/internal/redact"
)

const providerKey = "sk-test-gemini-fixture"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name)) // #nosec G304 -- callers use fixed fixture names.
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestProvider_ChatStreamAndModelsUseRecordedFixtures(t *testing.T) {
	t.Parallel()
	modelsFixture := fixture(t, "models.json")
	chatFixture := fixture(t, "chat.json")
	streamFixture := fixture(t, "stream.sse")
	var gotAuth, gotPath string
	var gotIncludeUsage bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.RequestURI()
		switch r.URL.Path {
		case "/v1beta/openai/models":
			_, _ = w.Write(modelsFixture)
		case "/v1beta/openai/chat/completions":
			if r.Header.Get("Accept") == "text/event-stream" {
				var body struct {
					StreamOptions struct {
						IncludeUsage bool `json:"include_usage"`
					} `json:"stream_options"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode stream request: %v", err)
				}
				gotIncludeUsage = body.StreamOptions.IncludeUsage
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(streamFixture)
				return
			}
			_, _ = w.Write(chatFixture)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p, err := gemini.New(gemini.Options{APIKey: redact.NewSecret(providerKey), BaseURL: srv.URL + "/v1beta/openai", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	models, err := p.Models(t.Context())
	if err != nil || len(models) != 2 || models[0].ID != "gemini-2.5-flash" || models[1].ID != "gemini-2.5-pro" {
		t.Fatalf("Models() = %+v, %v; want prefix-free Gemini model ids", models, err)
	}
	chat, err := p.Chat(t.Context(), providers.ChatRequest{Model: "gemini-2.5-flash", RawBody: []byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`)})
	if err != nil || string(chat.Body) != string(chatFixture) {
		t.Fatalf("Chat() = %s, %v; want recorded fixture verbatim", chat.Body, err)
	}
	stream, err := p.ChatStream(t.Context(), providers.ChatRequest{Model: "gemini-2.5-flash", RawBody: []byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	chunk, err := stream.Recv()
	if err != nil || !strings.Contains(string(chunk.Data), `"content":"Hello"`) {
		t.Fatalf("Recv() = %s, %v", chunk.Data, err)
	}
	usageChunk, err := stream.Recv()
	if err != nil || usageChunk.Usage == nil || usageChunk.Usage.TotalTokens != 6 {
		t.Fatalf("usage chunk = %+v, %v; want six reported tokens", usageChunk, err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("stream end = %v, want io.EOF", err)
	}
	if !gotIncludeUsage {
		t.Error("Gemini stream request omitted stream_options.include_usage")
	}
	if gotAuth != "Bearer "+providerKey || gotPath != "/v1beta/openai/chat/completions" {
		t.Errorf("last request auth/path = %q %q", gotAuth, gotPath)
	}
}

func TestProvider_ResourceExhaustedMapsToRateLimited(t *testing.T) {
	t.Parallel()
	fixture := fixture(t, "resource-exhausted.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()
	p, err := gemini.New(gemini.Options{APIKey: redact.NewSecret(providerKey), BaseURL: srv.URL + "/v1beta/openai", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Chat(t.Context(), providers.ChatRequest{Model: "gemini-2.5-flash", RawBody: []byte(`{"model":"gemini-2.5-flash","messages":[]}`)})
	if !errors.Is(err, providers.ErrRateLimited) || strings.Contains(err.Error(), providerKey) {
		t.Fatalf("Chat() error = %v; want scrubbed rate limited error", err)
	}
}

func TestProvider_ResourceExhaustedRetryInfo(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantAfter time.Duration
	}{
		{name: "RetryInfo", body: "resource-exhausted-retry.json", wantAfter: 34 * time.Second},
		{name: "subsecond RetryInfo clamps", body: "resource-exhausted-subsecond-retry.json", wantAfter: time.Second},
		{name: "missing detail", body: "resource-exhausted.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := fixture(t, tt.body)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write(body)
			}))
			defer srv.Close()
			p, err := gemini.New(gemini.Options{APIKey: redact.NewSecret(providerKey), BaseURL: srv.URL + "/v1beta/openai", Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.Chat(t.Context(), providers.ChatRequest{Model: "gemini-2.5-flash", RawBody: []byte(`{"model":"gemini-2.5-flash","messages":[]}`)})
			if !errors.Is(err, providers.ErrRateLimited) {
				t.Fatalf("Chat() error = %v, want ErrRateLimited", err)
			}
			var retry providers.RetryAfterError
			if tt.wantAfter == 0 {
				if !errors.As(err, &retry) || retry.After != 0 {
					t.Errorf("retry = %#v, want no retry time", retry)
				}
			} else if !errors.As(err, &retry) || retry.After != tt.wantAfter {
				t.Errorf("retry = %#v, want %s", retry, tt.wantAfter)
			}
			if strings.Contains(err.Error(), providerKey) {
				t.Errorf("error exposes provider key: %v", err)
			}
		})
	}
}

func TestProvider_TransportErrorDoesNotExposeKey(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := srv.URL + "/v1beta/openai"
	srv.Close()
	p, err := gemini.New(gemini.Options{APIKey: redact.NewSecret(providerKey), BaseURL: endpoint, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Models(t.Context())
	if err == nil || strings.Contains(err.Error(), providerKey) {
		t.Fatalf("Models() error = %v; want transport error without key", err)
	}
}
