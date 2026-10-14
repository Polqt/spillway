package mock_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/providers/mock"
)

var fixedNow = func() time.Time { return time.Unix(1760400000, 0) }

func newMock(t *testing.T) *mock.Provider {
	t.Helper()
	p, err := mock.New(mock.Options{Now: fixedNow})
	if err != nil {
		t.Fatalf("mock.New: %v", err)
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

func TestMock_Name(t *testing.T) {
	t.Parallel()
	if got := newMock(t).Name(); got != "mock" {
		t.Errorf("Name() = %q, want mock", got)
	}
}

func TestNew_RequiresClock(t *testing.T) {
	t.Parallel()
	if _, err := mock.New(mock.Options{}); err == nil {
		t.Error("New without Now succeeded, want error")
	}
}

func TestMock_EchoChat(t *testing.T) {
	t.Parallel()
	p := newMock(t)

	resp, err := p.Chat(t.Context(), userRequest("echo", "hello spillway"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	id, _ := body["id"].(string)
	if !regexp.MustCompile(`^chatcmpl-mock-[0-9a-f]{12}$`).MatchString(id) {
		t.Errorf("id = %q, want chatcmpl-mock-<12 hex>", id)
	}
	body["id"] = "ID"
	got, _ := json.Marshal(body)
	// Usage counts words: "be brief" + "hello spillway" in, "You said: hello spillway" out.
	want := `{"choices":[{"finish_reason":"stop","index":0,"logprobs":null,"message":{"content":"You said: hello spillway","role":"assistant"}}],"created":1760400000,"id":"ID","model":"echo","object":"chat.completion","usage":{"completion_tokens":4,"prompt_tokens":4,"total_tokens":8}}`
	if string(got) != want {
		t.Errorf("body\n got: %s\nwant: %s", got, want)
	}
}

func TestMock_EchoIDIsDeterministic(t *testing.T) {
	t.Parallel()
	p := newMock(t)
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

func TestMock_UnknownModel(t *testing.T) {
	t.Parallel()
	_, err := newMock(t).Chat(t.Context(), userRequest("nope", "hi"))
	if !errors.Is(err, providers.ErrModelNotFound) {
		t.Errorf("error = %v, want ErrModelNotFound", err)
	}
}

func TestMock_HonoursContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := newMock(t).Chat(ctx, userRequest("echo", "hi"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}
