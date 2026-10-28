package gemini_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
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

func TestProvider_ForwardsThoughtSignaturesToGemini(t *testing.T) {
	t.Parallel()
	const extra = `{ "google" : { "thought_signature" : "c2lnbmF0dXJlLWZvci10ZXN0cw==" } }`
	body := []byte(`{"model":"gemini/gemini-3-flash-preview","messages":[{"role":"user","content":"read a.go"},{"role":"assistant","content":null,"tool_calls":[{"id":"function-call-1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"},"extra_content":` + extra + `}]},{"role":"tool","tool_call_id":"function-call-1","content":"package a"}]}`)
	chatFixture := fixture(t, "chat.json")
	got := map[string]string{}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				ToolCalls []struct {
					ExtraContent json.RawMessage `json:"extra_content"`
				} `json:"tool_calls"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) != 3 || len(request.Messages[1].ToolCalls) != 1 {
			t.Errorf("upstream request = %+v, %v; want the three-message tool history", request, err)
			return
		}
		method := "Chat"
		if r.Header.Get("Accept") == "text/event-stream" {
			method = "ChatStream"
			_, _ = io.WriteString(w, "data: {\"object\":\"chat.completion.chunk\"}\n\ndata: [DONE]\n\n")
		} else {
			_, _ = w.Write(chatFixture)
		}
		mu.Lock()
		got[method] = string(request.Messages[1].ToolCalls[0].ExtraContent)
		mu.Unlock()
	}))
	defer srv.Close()
	p, err := gemini.New(gemini.Options{APIKey: redact.NewSecret(providerKey), BaseURL: srv.URL + "/v1beta/openai", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(t.Context(), providers.ChatRequest{Model: "gemini-3-flash-preview", RawBody: body}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	stream, err := p.ChatStream(t.Context(), providers.ChatRequest{Model: "gemini-3-flash-preview", RawBody: body})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("stream Recv: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, method := range []string{"Chat", "ChatStream"} {
		if got[method] != extra {
			t.Errorf("%s upstream extra_content = %s, want %s", method, got[method], extra)
		}
	}
}

func TestProvider_ChatReturnsToolCallSignatureVerbatim(t *testing.T) {
	t.Parallel()
	chatFixture := fixture(t, "chat-tool-signature.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(chatFixture)
	}))
	defer srv.Close()
	p, err := gemini.New(gemini.Options{APIKey: redact.NewSecret(providerKey), BaseURL: srv.URL + "/v1beta/openai", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	chat, err := p.Chat(t.Context(), providers.ChatRequest{Model: "gemini-3-flash-preview", RawBody: []byte(`{"model":"gemini-3-flash-preview","messages":[{"role":"user","content":"read both"}]}`)})
	if err != nil || !bytes.Equal(chat.Body, chatFixture) {
		t.Fatalf("Chat() = %s, %v; want recorded fixture verbatim", chat.Body, err)
	}
	if !bytes.Contains(chat.Body, []byte(`"extra_content":{"google":{"thought_signature":"c2lnbmF0dXJlLWZvci10ZXN0cw=="}}`)) {
		t.Errorf("Chat() body lost the thought signature: %s", chat.Body)
	}
}

func TestProvider_ChatStreamDeliversSignatureChunksVerbatim(t *testing.T) {
	t.Parallel()
	streamFixture := fixture(t, "stream-tool-signature.sse")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(streamFixture)
	}))
	defer srv.Close()
	p, err := gemini.New(gemini.Options{APIKey: redact.NewSecret(providerKey), BaseURL: srv.URL + "/v1beta/openai", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := p.ChatStream(t.Context(), providers.ChatRequest{Model: "gemini-3-flash-preview", RawBody: []byte(`{"model":"gemini-3-flash-preview","messages":[{"role":"user","content":"read a.go"}],"stream":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	// Each chunk must be the next fixture event byte for byte: ID and name, arguments, signature only, finish.
	rest := string(streamFixture)
	for i := range 4 {
		got, err := stream.Recv()
		event := "data: " + string(got.Data) + "\n\n"
		if err != nil || !strings.HasPrefix(rest, event) {
			t.Fatalf("Recv() %d = %s, %v; want the next fixture event verbatim", i, got.Data, err)
		}
		rest = rest[len(event):]
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) || rest != "data: [DONE]\n\n" {
		t.Fatalf("stream end = %v with %q left; want io.EOF after [DONE]", err, rest)
	}
}

const placeholderExtra = `{"google":{"thought_signature":"skip_thought_signature_validator"}}`

// upstreamBodies sends body through Chat and ChatStream and returns the raw request body Gemini received for each.
func upstreamBodies(t *testing.T, body string) map[string][]byte {
	t.Helper()
	chatFixture := fixture(t, "chat.json")
	got := map[string][]byte{}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		method := "Chat"
		if r.Header.Get("Accept") == "text/event-stream" {
			method = "ChatStream"
			_, _ = io.WriteString(w, "data: {\"object\":\"chat.completion.chunk\"}\n\ndata: [DONE]\n\n")
		} else {
			_, _ = w.Write(chatFixture)
		}
		mu.Lock()
		got[method] = data
		mu.Unlock()
	}))
	defer srv.Close()
	p, err := gemini.New(gemini.Options{APIKey: redact.NewSecret(providerKey), BaseURL: srv.URL + "/v1beta/openai", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(t.Context(), providers.ChatRequest{Model: "gemini-3-flash-preview", RawBody: []byte(body)}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	stream, err := p.ChatStream(t.Context(), providers.ChatRequest{Model: "gemini-3-flash-preview", RawBody: []byte(body)})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("stream Recv: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("upstream saw %d request kinds, want Chat and ChatStream", len(got))
	}
	return got
}

func messagesOf(t *testing.T, body []byte) []json.RawMessage {
	t.Helper()
	var request struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode messages of %s: %v", body, err)
	}
	return request.Messages
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("decode %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return reflect.DeepEqual(x, y)
}

func TestProvider_AddsPlaceholderSignatureToUnsignedToolCalls(t *testing.T) {
	t.Parallel()
	const (
		user        = `{"role":"user","content":"read a.go and b.go"}`
		toolA       = `{"role":"tool","tool_call_id":"call_a","content":"package a"}`
		toolB       = `{"role":"tool","tool_call_id":"call_b","content":"package b"}`
		callA       = `{"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}`
		callB       = `{"id":"call_b","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"b.go\"}"}}`
		signedCallA = `{"id":"function-call-1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"},"extra_content":{"google":{"thought_signature":"c2lnbmF0dXJlLWZvci10ZXN0cw=="}}}`
		signedTool  = `{"role":"tool","tool_call_id":"function-call-1","content":"package a"}`
		placedCallA = `{"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"},"extra_content":` + placeholderExtra + `}`
		placedCallB = `{"id":"call_b","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"b.go\"}"},"extra_content":` + placeholderExtra + `}`
	)
	tests := []struct {
		name     string
		messages []string
		// changed maps a message index to its expected upstream JSON; every other message must arrive byte-identical.
		changed map[int]string
	}{
		{
			name:     "one unsigned call",
			messages: []string{user, `{"role":"assistant","content":null,"tool_calls":[` + callA + `]}`, toolA},
			changed:  map[int]string{1: `{"role":"assistant","content":null,"tool_calls":[` + placedCallA + `]}`},
		},
		{
			name:     "unsigned parallel calls get it on the first call only",
			messages: []string{user, `{"role":"assistant","content":null,"tool_calls":[` + callA + `,` + callB + `]}`, toolA, toolB},
			changed:  map[int]string{1: `{"role":"assistant","content":null,"tool_calls":[` + placedCallA + `,` + callB + `]}`},
		},
		{
			name: "mixed history changes only the unsigned assistant message",
			messages: []string{
				user,
				`{"role":"assistant","content":null,"tool_calls":[` + signedCallA + `]}`,
				signedTool,
				`{"role":"assistant","content":"now b.go","tool_calls":[` + callB + `]}`,
				toolB,
			},
			changed: map[int]string{3: `{"role":"assistant","content":"now b.go","tool_calls":[` + placedCallB + `]}`},
		},
		{
			name:     "an empty signature counts as unsigned",
			messages: []string{user, `{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{}"},"extra_content":{"google":{"thought_signature":""}}}]}`, toolA},
			changed:  map[int]string{1: `{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{}"},"extra_content":` + placeholderExtra + `}]}`},
		},
		{
			name:     "tool calls outside assistant messages are untouched",
			messages: []string{`{"role":"user","content":"read a.go","tool_calls":[` + callA + `]}`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := `{"model":"gemini-3-flash-preview","messages":[` + strings.Join(tt.messages, ",") + `]}`
			for method, upstream := range upstreamBodies(t, body) {
				got := messagesOf(t, upstream)
				if len(got) != len(tt.messages) {
					t.Fatalf("%s upstream has %d messages, want %d: %s", method, len(got), len(tt.messages), upstream)
				}
				for i, message := range tt.messages {
					if want, ok := tt.changed[i]; ok {
						if !jsonEqual(t, got[i], []byte(want)) {
							t.Errorf("%s upstream message %d = %s, want %s", method, i, got[i], want)
						}
					} else if string(got[i]) != message {
						t.Errorf("%s upstream message %d = %s, want it byte-identical: %s", method, i, got[i], message)
					}
				}
			}
		})
	}
}

func TestProvider_PlaceholderNeverReplacesRealSignature(t *testing.T) {
	t.Parallel()
	const signed = `{"google" : {"thought_signature":"c2lnbmF0dXJlLWZvci10ZXN0cw=="}}`
	// The second message is a Gemini parallel step: only its first call carries a signature.
	body := `{"model":"gemini-3-flash-preview","messages":[` +
		`{"role":"user","content":"read a.go and b.go"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"function-call-1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"},"extra_content":` + signed + `},{"id":"function-call-2","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"b.go\"}"}}]},` +
		`{"role":"tool","tool_call_id":"function-call-1","content":"package a"},` +
		`{"role":"tool","tool_call_id":"function-call-2","content":"package b"}]}`
	for method, upstream := range upstreamBodies(t, body) {
		if string(upstream) != body {
			t.Errorf("%s upstream body = %s, want it byte-identical: %s", method, upstream, body)
		}
	}
}

func TestProvider_BodyWithoutToolCallsIsForwardedUnchanged(t *testing.T) {
	t.Parallel()
	body := `{ "model" : "gemini/gemini-3-flash-preview", "messages" : [ {"role":"system","content":"be brief"}, {"role":"user","content":"hi <b>&</b>"} ], "tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}], "temperature" : 0.2 }`
	want := strings.Replace(body, `"gemini/gemini-3-flash-preview"`, `"gemini-3-flash-preview"`, 1)
	for method, upstream := range upstreamBodies(t, body) {
		if string(upstream) != want {
			t.Errorf("%s upstream body = %s, want %s", method, upstream, want)
		}
	}
}

func TestProvider_MalformedToolCallsErrorOmitsBody(t *testing.T) {
	t.Parallel()
	const sentinel = "sentinel-tool-history-c2lnbmF0dXJl"
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Store(true) }))
	defer srv.Close()
	p, err := gemini.New(gemini.Options{APIKey: redact.NewSecret(providerKey), BaseURL: srv.URL + "/v1beta/openai", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, toolCalls string }{
		{"tool_calls is not an array", `"` + sentinel + `"`},
		{"tool call is not an object", `["` + sentinel + `"]`},
		{"extra_content is not an object", `[{"id":"call_a","extra_content":"` + sentinel + `"}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := providers.ChatRequest{Model: "gemini-3-flash-preview", RawBody: []byte(`{"model":"gemini-3-flash-preview","messages":[{"role":"assistant","tool_calls":` + tt.toolCalls + `}]}`)}
			_, chatErr := p.Chat(t.Context(), req)
			_, streamErr := p.ChatStream(t.Context(), req)
			for method, err := range map[string]error{"Chat": chatErr, "ChatStream": streamErr} {
				if !errors.Is(err, providers.ErrBadRequest) || strings.Contains(err.Error(), sentinel) || strings.Contains(err.Error(), providerKey) {
					t.Errorf("%s error = %v; want ErrBadRequest without body content or key", method, err)
				}
			}
		})
	}
	t.Cleanup(func() {
		if called.Load() {
			t.Error("malformed tool history reached Gemini")
		}
	})
}
