package gemini_test

import (
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
	modelsFixture := fixture(t, "models.json")
	chatFixture := fixture(t, "chat.json")
	streamFixture := fixture(t, "stream.sse")
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.RequestURI()
		switch r.URL.Path {
		case "/v1beta/openai/models":
			_, _ = w.Write(modelsFixture)
		case "/v1beta/openai/chat/completions":
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
	stream, err := p.ChatStream(t.Context(), providers.ChatRequest{Model: "gemini-2.5-flash", RawBody: []byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	chunk, err := stream.Recv()
	if err != nil || !strings.Contains(string(chunk.Data), `"content":"Hello"`) {
		t.Fatalf("Recv() = %s, %v", chunk.Data, err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("stream end = %v, want io.EOF", err)
	}
	if gotAuth != "Bearer "+providerKey || gotPath != "/v1beta/openai/chat/completions" {
		t.Errorf("last request auth/path = %q %q", gotAuth, gotPath)
	}
}

func TestProvider_ResourceExhaustedMapsToRateLimited(t *testing.T) {
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

func TestProvider_TransportErrorDoesNotExposeKey(t *testing.T) {
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
