package providers_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Polqt/spillway/internal/providers"
)

func TestPresets_CoverEverySupportedProvider(t *testing.T) {
	t.Parallel()
	want := []providers.Preset{
		{Name: "gemini", DefaultBaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", KeyEnv: "GEMINI_API_KEY", RequiresKey: true, AcceptsKey: true, FreeTierAttestable: true, SuggestedModel: "gemini-2.5-flash"},
		{Name: "groq", DefaultBaseURL: "https://api.groq.com/openai/v1", KeyEnv: "GROQ_API_KEY", RequiresKey: true, AcceptsKey: true, FreeTierAttestable: true, SuggestedModel: "llama-3.3-70b-versatile"},
		{Name: "mock", SuggestedModel: "echo"},
		{Name: "ollama", DefaultBaseURL: "http://localhost:11434", SuggestedModel: "llama3.2:3b"},
		{Name: "openrouter", DefaultBaseURL: "https://openrouter.ai/api/v1", KeyEnv: "OPENROUTER_API_KEY", RequiresKey: true, AcceptsKey: true, SuggestedModel: "meta-llama/llama-3.3-70b-instruct:free"},
	}
	got := providers.Presets()
	if len(got) != len(want) {
		t.Fatalf("Presets() returned %d providers, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Presets()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestLookupPreset_UnknownProvider(t *testing.T) {
	t.Parallel()
	if _, ok := providers.LookupPreset("unknown"); ok {
		t.Fatal("LookupPreset(unknown) found a preset, want none")
	}
}

func TestParseModelRef_SplitsAtFirstSlash(t *testing.T) {
	t.Parallel()
	valid := []struct {
		in             string
		provider, upst string
	}{
		{"mock/echo", "mock", "echo"},
		{"groq/llama-3.1-8b-instant", "groq", "llama-3.1-8b-instant"},
		{"openrouter/meta-llama/llama-3.1-8b-instruct:free", "openrouter", "meta-llama/llama-3.1-8b-instruct:free"},
		{"ollama/llama3.2:3b", "ollama", "llama3.2:3b"},
	}
	for _, tt := range valid {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			ref, err := providers.ParseModelRef(tt.in)
			if err != nil {
				t.Fatalf("ParseModelRef: %v", err)
			}
			if ref.Provider != tt.provider || ref.Model != tt.upst {
				t.Errorf("got %+v, want {%s %s}", ref, tt.provider, tt.upst)
			}
			if ref.String() != tt.in {
				t.Errorf("String() = %q, want %q", ref.String(), tt.in)
			}
		})
	}
	for _, in := range []string{"llama3", "/x", "groq/", ""} {
		t.Run("invalid "+in, func(t *testing.T) {
			t.Parallel()
			if _, err := providers.ParseModelRef(in); err == nil {
				t.Errorf("ParseModelRef(%q) succeeded, want error", in)
			}
		})
	}
}

type named string

func (n named) Name() string                                      { return string(n) }
func (n named) Models(context.Context) ([]providers.Model, error) { return nil, nil }
func (n named) Chat(context.Context, providers.ChatRequest) (providers.ChatResponse, error) {
	return providers.ChatResponse{}, nil
}
func (n named) ChatStream(context.Context, providers.ChatRequest) (providers.Stream, error) {
	return nil, errors.New("named: no stream")
}

func TestSet_Lookup(t *testing.T) {
	t.Parallel()
	set, err := providers.NewSet(named("mock"), named("groq"))
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	if p, ok := set.Lookup("groq"); !ok || p.Name() != "groq" {
		t.Errorf("Lookup(groq) = %v, %v", p, ok)
	}
	if _, ok := set.Lookup("gemini"); ok {
		t.Error("Lookup(gemini) found a provider, want none")
	}
}

func TestSet_ProvidersAreNameSorted(t *testing.T) {
	t.Parallel()
	set, err := providers.NewSet(named("zeta"), named("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	got := set.Providers()
	if len(got) != 2 || got[0].Name() != "alpha" || got[1].Name() != "zeta" {
		t.Errorf("Providers() = %v, want [alpha zeta]", got)
	}
}

func TestNewSet_Rejects(t *testing.T) {
	t.Parallel()
	if _, err := providers.NewSet(); err == nil {
		t.Error("NewSet() succeeded, want error for an empty set")
	}
	if _, err := providers.NewSet(named("mock"), named("mock")); err == nil {
		t.Error("NewSet(mock, mock) succeeded, want error for duplicate names")
	}
}

func TestMessage_Text(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, content, want string
	}{
		{"string", `"hello spillway"`, "hello spillway"},
		{"text parts", `[{"type":"text","text":"hello"},{"type":"text","text":"spillway"}]`, "hello spillway"},
		{"mixed parts", `[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:x"}}]`, "look"},
		{"null", `null`, ""},
		{"absent", ``, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := providers.Message{Role: "user", Content: json.RawMessage(tt.content)}
			if got := m.Text(); got != tt.want {
				t.Errorf("Text() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewChatResponse_OpenAIShape(t *testing.T) {
	t.Parallel()
	resp, err := providers.NewChatResponse(providers.Completion{
		ID:           "chatcmpl-1",
		Model:        "echo",
		Content:      "You said: hi",
		FinishReason: "stop",
		Created:      1760400000,
		Usage:        providers.Usage{PromptTokens: 1, CompletionTokens: 3, TotalTokens: 4},
	})
	if err != nil {
		t.Fatalf("NewChatResponse: %v", err)
	}
	want := `{
		"id": "chatcmpl-1",
		"object": "chat.completion",
		"created": 1760400000,
		"model": "echo",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": "You said: hi"},
			"logprobs": null,
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 1, "completion_tokens": 3, "total_tokens": 4}
	}`
	assertJSONEqual(t, resp.Body, []byte(want))
}

func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("want is not JSON: %v", err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Errorf("JSON mismatch\n got: %s\nwant: %s", gb, wb)
	}
}
