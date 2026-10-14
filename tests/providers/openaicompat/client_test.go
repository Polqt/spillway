package openaicompat_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/providers/openaicompat"
	"github.com/Polqt/spillway/internal/redact"
)

const providerKey = "sk-test-groq-fixture"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name)) // #nosec G304 -- callers use fixed fixture names.
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func client(t *testing.T, endpoint string) *openaicompat.Client {
	t.Helper()
	c, err := openaicompat.New(openaicompat.Options{
		Name: "groq", BaseURL: endpoint, APIKey: redact.NewSecret(providerKey), Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClient_ChatModelsAndStreamUseRecordedFixtures(t *testing.T) {
	modelFixture, chatFixture, streamFixture := fixture(t, "models.json"), fixture(t, "chat.json"), fixture(t, "stream.sse")
	var authValues []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authValues = append(authValues, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write(modelFixture)
		case "/v1/chat/completions":
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
	c := client(t, srv.URL+"/v1")

	models, err := c.Models(t.Context())
	if err != nil || len(models) != 2 || models[0].ID != "llama-3.1-8b-instant" || models[0].OwnedBy != "groq" {
		t.Fatalf("Models() = %+v, %v", models, err)
	}

	chat, err := c.Chat(t.Context(), providers.ChatRequest{Model: "llama-3.1-8b-instant", RawBody: []byte(`{"model":"groq/llama-3.1-8b-instant","messages":[{"role":"user","content":"hi"}]}`)})
	if err != nil || string(chat.Body) != string(chatFixture) {
		t.Fatalf("Chat() = %s, %v; want fixture verbatim", chat.Body, err)
	}

	stream, err := c.ChatStream(t.Context(), providers.ChatRequest{Model: "llama-3.1-8b-instant", RawBody: []byte(`{"model":"groq/llama-3.1-8b-instant","messages":[{"role":"user","content":"hi"}],"stream":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	var chunks []string
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, string(chunk.Data))
	}
	if len(chunks) != 3 || !strings.Contains(chunks[0], `"content":"Hello"`) || !strings.Contains(chunks[2], `"total_tokens":8`) {
		t.Errorf("stream chunks = %q", chunks)
	}
	for _, auth := range authValues {
		if auth != "Bearer "+providerKey {
			t.Errorf("upstream Authorization = %q, want configured provider key", auth)
		}
	}
}

func TestClient_UnknownFieldsPreserveRawValuesAndNeverForwardClientAuth(t *testing.T) {
	const tools = `[{ "type" : "function", "function" : { "name":"f", "parameters" : {"type":"object"} } }]`
	const format = `{ "type" : "json_schema", "json_schema" : { "name":"x" } }`
	const body = `{"model":"groq/upstream","messages":[{"role":"user","content":"hi"}],"tools":` + tools + `,"response_format":` + format + `,"future_field": { "raw" : [1, 2] }}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+providerKey {
			t.Errorf("upstream Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Client-Secret") != "" {
			t.Errorf("unallowlisted client header reached upstream: %v", r.Header)
		}
		data, _ := io.ReadAll(r.Body)
		for key, want := range map[string]string{"tools": tools, "response_format": format, "future_field": `{ "raw" : [1, 2] }`} {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields[key]) != want {
				t.Errorf("%s = %s, want raw value %s", key, fields[key], want)
			}
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		var model string
		if err := json.Unmarshal(fields["model"], &model); err != nil || model != "upstream" {
			t.Errorf("forwarded model = %q, %v; want upstream", model, err)
		}
		if r.Header.Get("Accept") == "text/event-stream" {
			_, _ = io.WriteString(w, "data: {\"object\":\"chat.completion.chunk\"}\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = w.Write([]byte(`{"object":"chat.completion"}`))
	}))
	defer srv.Close()
	c := client(t, srv.URL)
	if _, err := c.Chat(t.Context(), providers.ChatRequest{Model: "upstream", RawBody: []byte(body)}); err != nil {
		t.Fatal(err)
	}
	streamBody := strings.TrimSuffix(body, "}") + `,"stream":true}`
	stream, err := c.ChatStream(t.Context(), providers.ChatRequest{Model: "upstream", RawBody: []byte(streamBody)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("stream Recv: %v", err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("stream end = %v, want io.EOF", err)
	}
}

func TestClient_MapsStatusAndScrubsProviderKey(t *testing.T) {
	tests := []struct {
		status int
		kind   error
	}{
		{429, providers.ErrRateLimited}, {401, providers.ErrUnauthorized}, {403, providers.ErrUnauthorized},
		{400, providers.ErrBadRequest}, {404, providers.ErrModelNotFound}, {503, providers.ErrUpstreamUnavailable},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "7")
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, `{"error":{"message":"bad `+providerKey+`"},"extra":"full-body-marker"}`)
			}))
			defer srv.Close()
			_, err := client(t, srv.URL).Chat(t.Context(), providers.ChatRequest{Model: "upstream", RawBody: []byte(`{"model":"upstream","messages":[]}`)})
			if !errors.Is(err, tt.kind) {
				t.Errorf("error = %v, want %v", err, tt.kind)
			}
			if strings.Contains(err.Error(), providerKey) {
				t.Errorf("error exposes provider key: %v", err)
			}
			if strings.Contains(err.Error(), "full-body-marker") {
				t.Errorf("error contains the full upstream body: %v", err)
			}
			if tt.status == http.StatusTooManyRequests {
				var retry providers.RetryAfterError
				if !errors.As(err, &retry) || retry.After != 7*time.Second {
					t.Errorf("retry error = %#v, want 7s", retry)
				}
			}
		})
	}
}

func TestClient_ChatStreamSurfacesUpstreamFailureAfterChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprint(rw, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		payload := []byte("data: {\"id\":\"partial\"}\n\n")
		_, _ = fmt.Fprintf(rw, "%x\r\n%s\r\n", len(payload), payload)
		_ = rw.Flush()
	}))
	defer srv.Close()
	stream, err := client(t, srv.URL).ChatStream(t.Context(), providers.ChatRequest{Model: "upstream", RawBody: []byte(`{"model":"upstream","messages":[],"stream":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if chunk, err := stream.Recv(); err != nil || string(chunk.Data) != `{"id":"partial"}` {
		t.Fatalf("first Recv() = %s, %v", chunk.Data, err)
	}
	if _, err := stream.Recv(); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("second Recv() = %v, want an upstream read failure", err)
	}
}

func TestClient_ClosingChatStreamCancelsUpstream(t *testing.T) {
	entered, cancelled := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"first\"}\n\n")
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream, err := client(t, srv.URL).ChatStream(ctx, providers.ChatRequest{Model: "upstream", RawBody: []byte(`{"model":"upstream","messages":[],"stream":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	<-entered
	<-cancelled
}

func TestClient_DoesNotFollowRedirect(t *testing.T) {
	var redirectReached bool
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { redirectReached = true; w.WriteHeader(http.StatusOK) }))
	defer redirect.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", redirect.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer first.Close()
	_, err := client(t, first.URL).Models(t.Context())
	if err == nil || redirectReached {
		t.Errorf("Models() error = %v, redirect reached = %v", err, redirectReached)
	}
}
