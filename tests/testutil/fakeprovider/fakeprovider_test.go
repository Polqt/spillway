package fakeprovider_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/tests/testutil/fakeprovider"
)

var fixedNow = func() time.Time { return time.Unix(1760400000, 0) }

func newFakeProvider(t *testing.T) *fakeprovider.Provider {
	t.Helper()
	p, err := fakeprovider.New(fakeprovider.Options{Now: fixedNow})
	if err != nil {
		t.Fatalf("fakeprovider.New: %v", err)
	}
	return p
}

func userRequest(model, text string) providers.ChatRequest {
	content, _ := json.Marshal(text)
	return providers.ChatRequest{
		Model: model,
		Messages: []providers.Message{
			{Role: "system", Content: json.RawMessage(`"be brief"`)},
			{Role: "user", Content: content},
		},
	}
}

func TestFakeProvider_Name(t *testing.T) {
	t.Parallel()
	if got := newFakeProvider(t).Name(); got != "mock" {
		t.Errorf("Name() = %q, want mock", got)
	}
}

func TestNew_RequiresClock(t *testing.T) {
	t.Parallel()
	if _, err := fakeprovider.New(fakeprovider.Options{}); err == nil {
		t.Error("New without Now succeeded, want error")
	}
}

func TestFakeProvider_Models(t *testing.T) {
	t.Parallel()
	models, err := newFakeProvider(t).Models(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, model := range models {
		got = append(got, model.ID)
	}
	want := []string{"echo", "rate-limited", "stream-error", "unauthorized", "unavailable"}
	if !slices.Equal(got, want) {
		t.Errorf("Models() ids = %q, want %q", got, want)
	}
}

func TestFakeProvider_ErrorModels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		model string
		kind  error
	}{
		{model: "rate-limited", kind: providers.ErrRateLimited},
		{model: "unavailable", kind: providers.ErrUpstreamUnavailable},
		{model: "unauthorized", kind: providers.ErrUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			t.Parallel()
			p := newFakeProvider(t)
			_, err := p.Chat(t.Context(), userRequest(tt.model, "hi"))
			if !errors.Is(err, tt.kind) {
				t.Errorf("Chat() error = %v, want %v", err, tt.kind)
			}
			if tt.model == "rate-limited" {
				var retry providers.RetryAfterError
				if !errors.As(err, &retry) || retry.After != 7*time.Second {
					t.Errorf("Chat() error = %v, want RetryAfter 7s", err)
				}
			}
			_, err = p.ChatStream(t.Context(), userRequest(tt.model, "hi"))
			if !errors.Is(err, tt.kind) {
				t.Errorf("ChatStream() error = %v, want %v", err, tt.kind)
			}
			if tt.model == "rate-limited" {
				var retry providers.RetryAfterError
				if !errors.As(err, &retry) || retry.After != 7*time.Second {
					t.Errorf("ChatStream() error = %v, want RetryAfter 7s", err)
				}
			}
		})
	}
}

func TestFakeProvider_EchoChat(t *testing.T) {
	t.Parallel()
	p := newFakeProvider(t)

	resp, err := p.Chat(t.Context(), userRequest("echo", "hello spillway"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	id, _ := body["id"].(string)
	if !regexp.MustCompile(`^chatcmpl-test-[0-9a-f]{12}$`).MatchString(id) {
		t.Errorf("id = %q, want chatcmpl-test-<12 hex>", id)
	}
	body["id"] = "ID"
	got, _ := json.Marshal(body)
	// Usage counts words: "be brief" + "hello spillway" in, "You said: hello spillway" out.
	want := `{"choices":[{"finish_reason":"stop","index":0,"logprobs":null,"message":{"content":"You said: hello spillway","role":"assistant"}}],"created":1760400000,"id":"ID","model":"echo","object":"chat.completion","usage":{"completion_tokens":4,"prompt_tokens":4,"total_tokens":8}}`
	if string(got) != want {
		t.Errorf("body\n got: %s\nwant: %s", got, want)
	}
}

func TestFakeProvider_EchoIDIsDeterministic(t *testing.T) {
	t.Parallel()
	p := newFakeProvider(t)
	id := func(text string) string {
		resp, err := p.Chat(t.Context(), userRequest("echo", text))
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		var body struct{ ID string }
		if err := json.Unmarshal(resp.Body, &body); err != nil {
			t.Fatal(err)
		}
		return body.ID
	}
	first, second := id("a"), id("a")
	if first != second {
		t.Error("identical requests got different ids")
	}
	if id("a") == id("b") {
		t.Error("different requests got the same id")
	}
}

func TestFakeProvider_UnknownModel(t *testing.T) {
	t.Parallel()
	_, err := newFakeProvider(t).Chat(t.Context(), userRequest("nope", "hi"))
	if !errors.Is(err, providers.ErrModelNotFound) {
		t.Errorf("error = %v, want ErrModelNotFound", err)
	}
}

func TestFakeProvider_HonoursContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := newFakeProvider(t).Chat(ctx, userRequest("echo", "hi"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func closeStream(t *testing.T, s providers.Stream) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// drain reads a stream to its end and returns the chunk payloads and the
// error that ended it.
func drain(t *testing.T, s providers.Stream) ([]string, error) {
	t.Helper()
	var chunks []string
	for {
		c, err := s.Recv()
		if err != nil {
			return chunks, err
		}
		chunks = append(chunks, string(c.Data))
	}
}

func TestFakeProvider_EchoStream(t *testing.T) {
	t.Parallel()
	s, err := newFakeProvider(t).ChatStream(t.Context(), userRequest("echo", "hello spillway"))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer closeStream(t, s)

	chunks, err := drain(t, s)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("stream ended with %v, want io.EOF", err)
	}
	id := regexp.MustCompile(`chatcmpl-test-[0-9a-f]{12}`)
	const head = `{"id":"ID","object":"chat.completion.chunk","created":1760400000,"model":"echo","choices":[{"index":0,"delta":`
	want := []string{
		head + `{"role":"assistant","content":"You"},"logprobs":null,"finish_reason":null}]}`,
		head + `{"content":" said:"},"logprobs":null,"finish_reason":null}]}`,
		head + `{"content":" hello"},"logprobs":null,"finish_reason":null}]}`,
		head + `{"content":" spillway"},"logprobs":null,"finish_reason":null}]}`,
		head + `{},"logprobs":null,"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":4,"total_tokens":8}}`,
	}
	if len(chunks) != len(want) {
		t.Fatalf("got %d chunks, want %d:\n%s", len(chunks), len(want), strings.Join(chunks, "\n"))
	}
	for i, c := range chunks {
		if got := id.ReplaceAllString(c, "ID"); got != want[i] {
			t.Errorf("chunk %d\n got: %s\nwant: %s", i, got, want[i])
		}
	}
}

func TestFakeProvider_EchoStreamReportsUsage(t *testing.T) {
	t.Parallel()
	s, err := newFakeProvider(t).ChatStream(t.Context(), userRequest("echo", "hello spillway"))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer closeStream(t, s)
	var last providers.ChatChunk
	for {
		c, err := s.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if last.Usage != nil {
			t.Error("a chunk before the last carries usage")
		}
		last = c
	}
	want := providers.Usage{PromptTokens: 4, CompletionTokens: 4, TotalTokens: 8}
	if last.Usage == nil || *last.Usage != want {
		t.Errorf("last chunk usage = %+v, want %+v", last.Usage, want)
	}
}

func TestFakeProvider_StreamErrorMidStream(t *testing.T) {
	t.Parallel()
	s, err := newFakeProvider(t).ChatStream(t.Context(), userRequest("stream-error", "hello spillway"))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer closeStream(t, s)

	chunks, err := drain(t, s)
	if len(chunks) != 2 {
		t.Errorf("got %d chunks before the error, want 2", len(chunks))
	}
	if !errors.Is(err, providers.ErrUpstreamUnavailable) {
		t.Errorf("stream ended with %v, want ErrUpstreamUnavailable", err)
	}
}

func TestFakeProvider_StreamErrorModelFailsChat(t *testing.T) {
	t.Parallel()
	_, err := newFakeProvider(t).Chat(t.Context(), userRequest("stream-error", "hi"))
	if !errors.Is(err, providers.ErrUpstreamUnavailable) {
		t.Errorf("error = %v, want ErrUpstreamUnavailable", err)
	}
}

func TestFakeProvider_StreamUnknownModel(t *testing.T) {
	t.Parallel()
	_, err := newFakeProvider(t).ChatStream(t.Context(), userRequest("nope", "hi"))
	if !errors.Is(err, providers.ErrModelNotFound) {
		t.Errorf("error = %v, want ErrModelNotFound", err)
	}
}

func TestFakeProvider_StreamHonoursContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	s, err := newFakeProvider(t).ChatStream(ctx, userRequest("echo", "hello spillway"))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer closeStream(t, s)
	if _, err := s.Recv(); err != nil {
		t.Fatalf("first Recv: %v", err)
	}
	cancel()
	if _, err := s.Recv(); err != context.Canceled { //nolint:errorlint // the contract is context.Canceled unwrapped
		t.Errorf("Recv after cancel = %v, want context.Canceled unwrapped", err)
	}
}

func TestFakeProvider_StreamClose(t *testing.T) {
	t.Parallel()
	s, err := newFakeProvider(t).ChatStream(t.Context(), userRequest("echo", "hi"))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := s.Recv(); err == nil || errors.Is(err, io.EOF) {
		t.Errorf("Recv after Close = %v, want a non-EOF error", err)
	}
}
