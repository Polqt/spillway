package openaicompat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Polqt/spillway/internal/fakenet"
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

func TestNew_RequiresKeyExceptForOllama(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"groq", "openrouter"} {
		t.Run(name, func(t *testing.T) {
			_, err := openaicompat.New(openaicompat.Options{
				Name: name, BaseURL: "http://127.0.0.1:11434/v1", Timeout: time.Second,
			})
			if err == nil || !strings.Contains(err.Error(), "API key is required") {
				t.Errorf("New() error = %v, want API key requirement", err)
			}
		})
	}
	if _, err := openaicompat.New(openaicompat.Options{
		Name: "ollama", BaseURL: "http://127.0.0.1:11434/v1", Timeout: time.Second,
	}); err != nil {
		t.Errorf("New() without Ollama key = %v, want success", err)
	}
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

func TestClient_ChatTimeoutOnStalledResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		listener := fakenet.Listen()
		started := make(chan struct{})
		release := make(chan struct{})
		server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(started)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		})}
		serveDone := make(chan error, 1)
		go func() { serveDone <- server.Serve(listener) }()
		t.Cleanup(func() { close(release); _ = server.Close(); _ = listener.Close() })
		client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return listener.DialContext(ctx, "", "")
		}}}
		provider, err := openaicompat.New(openaicompat.Options{
			Name: "groq", BaseURL: "http://127.0.0.1/v1", APIKey: redact.NewSecret(providerKey),
			Timeout: 5 * time.Second, HTTPClient: client,
		})
		if err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := provider.Chat(t.Context(), providers.ChatRequest{Model: "model", Messages: []providers.Message{{Role: "user", Content: json.RawMessage(`"hello"`)}}})
			result <- err
		}()
		<-started
		<-time.After(6 * time.Second)
		if err := <-result; !errors.Is(err, providers.ErrUpstreamUnavailable) {
			t.Fatalf("Chat() error = %v, want upstream unavailable", err)
		}
		_ = server.Close()
		_ = listener.Close()
		if err := <-serveDone; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve() = %v, want http.ErrServerClosed", err)
		}
	})
}

func TestClient_ChatStreamAllowsLongActiveStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		listener := fakenet.Listen()
		server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for i := 1; i <= 3; i++ {
				_, _ = fmt.Fprintf(w, "data: {\"n\":%d}\n\n", i)
				w.(http.Flusher).Flush()
				if i < 3 {
					<-time.After(4 * time.Second)
				}
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		})}
		serveDone := make(chan error, 1)
		go func() { serveDone <- server.Serve(listener) }()
		t.Cleanup(func() { _ = server.Close(); _ = listener.Close() })
		client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return listener.DialContext(ctx, "", "")
		}}}
		provider, err := openaicompat.New(openaicompat.Options{
			Name: "groq", BaseURL: "http://127.0.0.1/v1", APIKey: redact.NewSecret(providerKey),
			Timeout: 5 * time.Second, HTTPClient: client,
		})
		if err != nil {
			t.Fatal(err)
		}
		stream, err := provider.ChatStream(t.Context(), providers.ChatRequest{Model: "model", Messages: []providers.Message{{Role: "user", Content: json.RawMessage(`"hello"`)}}})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = stream.Close() }()
		for i := 1; i <= 3; i++ {
			chunk, err := stream.Recv()
			if err != nil || string(chunk.Data) != fmt.Sprintf("{\"n\":%d}", i) {
				t.Fatalf("Recv() = %q, %v; want chunk %d", chunk.Data, err, i)
			}
		}
		if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
			t.Fatalf("Recv() after chunks = %v, want EOF", err)
		}
		_ = server.Close()
		_ = listener.Close()
		if err := <-serveDone; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve() = %v, want http.ErrServerClosed", err)
		}
	})
}

func TestClient_ChatStreamTimesOutWhenUpstreamStalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		listener := fakenet.Listen()
		release := make(chan struct{})
		server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"n\":1}\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-release:
			}
		})}
		serveDone := make(chan error, 1)
		go func() { serveDone <- server.Serve(listener) }()
		t.Cleanup(func() { close(release); _ = server.Close(); _ = listener.Close() })
		client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return listener.DialContext(ctx, "", "")
		}}}
		provider, err := openaicompat.New(openaicompat.Options{
			Name: "groq", BaseURL: "http://127.0.0.1/v1", APIKey: redact.NewSecret(providerKey),
			Timeout: 5 * time.Second, HTTPClient: client,
		})
		if err != nil {
			t.Fatal(err)
		}
		stream, err := provider.ChatStream(t.Context(), providers.ChatRequest{Model: "model", Messages: []providers.Message{{Role: "user", Content: json.RawMessage(`"hello"`)}}})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = stream.Close() }()
		if chunk, err := stream.Recv(); err != nil || string(chunk.Data) != `{"n":1}` {
			t.Fatalf("first Recv() = %q, %v; want initial event", chunk.Data, err)
		}
		result := make(chan error, 1)
		go func() { _, err := stream.Recv(); result <- err }()
		<-time.After(6 * time.Second)
		if err := <-result; !errors.Is(err, providers.ErrUpstreamUnavailable) {
			t.Fatalf("stalled Recv() error = %v, want upstream unavailable", err)
		}
		_ = server.Close()
		_ = listener.Close()
		if err := <-serveDone; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve() = %v, want http.ErrServerClosed", err)
		}
	})
}

func TestClient_OpenRouterAndOllamaUseRecordedFixtures(t *testing.T) {
	tests := []struct {
		name       string
		model      string
		apiKey     redact.Secret
		wantAuth   string
		modelsFile string
		chatFile   string
		streamFile string
	}{
		{
			name: "openrouter", model: "meta-llama/llama-3.1-8b-instruct:free",
			apiKey: redact.NewSecret("sk-test-openrouter-fixture"), wantAuth: "Bearer sk-test-openrouter-fixture",
			modelsFile: "openrouter-models.json", chatFile: "openrouter-chat.json", streamFile: "openrouter-stream.sse",
		},
		{
			name: "ollama", model: "llama3.2:3b",
			modelsFile: "ollama-models.json", chatFile: "ollama-chat.json", streamFile: "ollama-stream.sse",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			modelsFixture := fixture(t, tt.modelsFile)
			chatFixture := fixture(t, tt.chatFile)
			streamFixture := fixture(t, tt.streamFile)
			var gotModel string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got, want := r.Header.Get("Authorization"), tt.wantAuth; got != want {
					t.Errorf("Authorization = %q, want %q", got, want)
				}
				switch r.URL.Path {
				case "/v1/models":
					_, _ = w.Write(modelsFixture)
				case "/v1/chat/completions":
					body, _ := io.ReadAll(r.Body)
					var request struct {
						Model string `json:"model"`
					}
					if err := json.Unmarshal(body, &request); err != nil {
						t.Errorf("decode model from request: %v", err)
					}
					gotModel = request.Model
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

			c, err := openaicompat.New(openaicompat.Options{
				Name: tt.name, BaseURL: srv.URL + "/v1", APIKey: tt.apiKey, Timeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			models, err := c.Models(t.Context())
			if err != nil || len(models) != 1 || models[0].ID != tt.model {
				t.Fatalf("Models() = %+v, %v; want model %q", models, err, tt.model)
			}
			chat, err := c.Chat(t.Context(), providers.ChatRequest{
				Model: tt.model, RawBody: []byte(`{"model":"` + tt.name + `/` + tt.model + `","messages":[{"role":"user","content":"hi"}]}`),
			})
			if err != nil || string(chat.Body) != string(chatFixture) {
				t.Fatalf("Chat() = %s, %v; want fixture verbatim", chat.Body, err)
			}
			if gotModel != tt.model {
				t.Errorf("upstream model = %q, want %q", gotModel, tt.model)
			}
			stream, err := c.ChatStream(t.Context(), providers.ChatRequest{
				Model: tt.model, RawBody: []byte(`{"model":"` + tt.name + `/` + tt.model + `","messages":[{"role":"user","content":"hi"}],"stream":true}`),
			})
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
					t.Fatalf("Recv() error = %v", err)
				}
				chunks = append(chunks, string(chunk.Data))
			}
			if len(chunks) != 1 || !strings.Contains(chunks[0], `"content":"ok"`) {
				t.Errorf("stream chunks = %q; want recorded fixture chunk", chunks)
			}
		})
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

func TestClient_MapsPaymentRequiredAndRetryAfter(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		retry     string
		body      string
		wantKind  error
		wantAfter time.Duration
	}{
		{name: "OpenRouter credits", status: http.StatusPaymentRequired, retry: "12", body: "openrouter-payment-credits.json", wantKind: providers.ErrPaymentRequired, wantAfter: 12 * time.Second},
		{name: "OpenRouter in-flight budget", status: http.StatusPaymentRequired, retry: "12", body: "openrouter-payment-budget.json", wantKind: providers.ErrPaymentRequired, wantAfter: 12 * time.Second},
		{name: "503 with retry", status: http.StatusServiceUnavailable, retry: "12", wantKind: providers.ErrUpstreamUnavailable, wantAfter: 12 * time.Second},
		{name: "503 without retry", status: http.StatusServiceUnavailable, wantKind: providers.ErrUpstreamUnavailable},
		{name: "zero", status: http.StatusPaymentRequired, retry: "0", wantKind: providers.ErrPaymentRequired},
		{name: "negative", status: http.StatusPaymentRequired, retry: "-2", wantKind: providers.ErrPaymentRequired},
		{name: "garbage", status: http.StatusPaymentRequired, retry: "later", wantKind: providers.ErrPaymentRequired},
		{name: "clamped", status: http.StatusPaymentRequired, retry: "999999999", wantKind: providers.ErrPaymentRequired, wantAfter: 24 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"error":{"message":"payment required"}}`)
			if tt.body != "" {
				body = fixture(t, tt.body)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.retry != "" {
					w.Header().Set("Retry-After", tt.retry)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write(body)
			}))
			defer srv.Close()

			_, err := client(t, srv.URL).Chat(t.Context(), providers.ChatRequest{Model: "upstream", RawBody: []byte(`{"model":"upstream","messages":[]}`)})
			if !errors.Is(err, tt.wantKind) {
				t.Fatalf("Chat() error = %v, want %v", err, tt.wantKind)
			}
			var retry providers.RetryAfterError
			if tt.wantAfter == 0 {
				if errors.As(err, &retry) && retry.After != 0 {
					t.Errorf("retry = %s, want none", retry.After)
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

func TestClient_ChatStreamErrorFidelity(t *testing.T) {
	tests := []struct {
		name          string
		fixture       string
		wantKind      error
		wantError     string
		wantChunks    int
		maxErrorBytes int
	}{
		{name: "OpenRouter first event", fixture: "stream-error-first.sse", wantKind: providers.ErrRateLimited, wantError: "OpenRouter quota exhausted", wantChunks: 0},
		{name: "payment required", fixture: "stream-error-payment.sse", wantKind: providers.ErrPaymentRequired, wantError: "payment required", wantChunks: 0},
		{name: "after two chunks", fixture: "stream-error-after-chunks.sse", wantKind: providers.ErrUpstreamUnavailable, wantError: "upstream unavailable", wantChunks: 2},
		{name: "non-status code", fixture: "stream-error-other-code.sse", wantKind: providers.ErrUpstreamUnavailable, wantError: "provider unavailable", wantChunks: 0},
		{name: "without done", fixture: "stream-truncated.sse", wantKind: providers.ErrUpstreamUnavailable, wantError: "stream ended without [DONE]", wantChunks: 2},
		{name: "long error message", fixture: "stream-error-long-message.sse", wantKind: providers.ErrUpstreamUnavailable, wantError: "upstream stream error", wantChunks: 0, maxErrorBytes: 512},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixtureBody := fixture(t, tt.fixture)
			var results []string
			for _, partial := range []bool{false, true} {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					if !partial {
						_, _ = w.Write(fixtureBody)
						return
					}
					flusher := w.(http.Flusher)
					for start := 0; start < len(fixtureBody); start += 3 {
						end := min(start+3, len(fixtureBody))
						_, _ = w.Write(fixtureBody[start:end])
						flusher.Flush()
					}
				}))
				stream, err := client(t, srv.URL).ChatStream(t.Context(), providers.ChatRequest{Model: "upstream", RawBody: []byte(`{"model":"upstream","messages":[],"stream":true}`)})
				if err != nil {
					srv.Close()
					t.Fatal(err)
				}
				var chunks []string
				var recvErr error
				for {
					chunk, err := stream.Recv()
					if err != nil {
						recvErr = err
						break
					}
					chunks = append(chunks, string(chunk.Data))
				}
				_ = stream.Close()
				srv.Close()
				if len(chunks) != tt.wantChunks {
					t.Fatalf("chunks = %q, want %d", chunks, tt.wantChunks)
				}
				if !errors.Is(recvErr, tt.wantKind) || !strings.Contains(recvErr.Error(), tt.wantError) {
					t.Fatalf("Recv() error = %v, want %v containing %q", recvErr, tt.wantKind, tt.wantError)
				}
				if strings.Contains(recvErr.Error(), providerKey) {
					t.Fatalf("Recv() error exposes provider key: %v", recvErr)
				}
				if tt.maxErrorBytes > 0 && len(recvErr.Error()) > tt.maxErrorBytes {
					t.Fatalf("Recv() error length = %d, want at most %d bytes", len(recvErr.Error()), tt.maxErrorBytes)
				}
				results = append(results, fmt.Sprintf("%q|%v", chunks, recvErr))
			}
			if !bytes.Equal([]byte(results[0]), []byte(results[1])) {
				t.Errorf("3-byte writes result = %q, want %q", results[1], results[0])
			}
		})
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
