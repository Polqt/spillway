package api_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/providers"
)

func TestResponses_UsesChatExecutionAndReturnsResponsesShape(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	var got providers.ChatRequest
	e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
		got = req
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-test","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"spillway_tool_0","arguments":"{\"query\":\"docs\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`)}, nil
	}
	body := `{"model":"fake/model","instructions":"Be concise.","store":false,"include":["reasoning.encrypted_content"],"client_metadata":{"app":"codex"},"prompt_cache_key":"codex-session","service_tier":"priority","parallel_tool_calls":true,"reasoning":{"effort":"medium","summary":"auto"},"text":{"verbosity":"medium"},"input":[{"type":"message","id":"msg_codex_1","internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]},"role":"user","content":[{"type":"input_text","text":"Use a tool."}]}],"tools":[{"type":"namespace","name":"functions","description":"Local functions","external_web_access":true,"tools":[{"type":"function","name":"lookup","description":"Search docs","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}]},{"type":"web_search"}],"tool_choice":"auto"}`
	rec := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: body})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	if got.Model != "model" || len(got.Messages) != 2 || got.Messages[0].Role != "system" || got.Messages[1].Text() != "Use a tool." {
		t.Fatalf("translated request = %#v, want system and user messages", got)
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(got.RawBody, &request); err != nil {
		t.Fatal("decode translated chat request:", err)
	}
	var tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(request["tools"], &tools); err != nil || len(tools) != 1 || tools[0].Type != "function" || tools[0].Function.Name != "spillway_tool_0" {
		t.Fatalf("chat tools = %s, want chat-completions function shape", request["tools"])
	}
	var response struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type      string `json:"type"`
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal("decode Responses response:", err)
	}
	if response.Object != "response" || response.Status != "completed" || len(response.Output) != 1 || response.Output[0].Type != "function_call" || response.Output[0].Namespace != "functions" || response.Output[0].Name != "lookup" || response.Output[0].Arguments != `{"query":"docs"}` {
		t.Fatalf("Responses output = %#v", response)
	}
}

func TestResponses_RestoresNamespacedToolHistory(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	var got providers.ChatRequest
	e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
		got = req
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-test","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)}, nil
	}
	body := `{"model":"fake/model","input":[{"type":"function_call","call_id":"call-9","namespace":"functions","name":"lookup","arguments":"{\"query\":\"docs\"}"},{"type":"function_call_output","call_id":"call-9","output":"found"},{"type":"message","role":"user","content":"Continue."}],"tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}]}`
	rec := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: body})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var translated struct {
		Messages []struct {
			Role      string `json:"role"`
			ToolCalls []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
			Content    string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got.RawBody, &translated); err != nil {
		t.Fatal("decode translated tool history:", err)
	}
	if len(translated.Messages) != 3 || translated.Messages[0].ToolCalls[0].Function.Name != "spillway_tool_0" || translated.Messages[1].ToolCallID != "call-9" || translated.Messages[2].Content != "Continue." {
		t.Fatalf("translated tool history = %#v", translated.Messages)
	}
}

func TestResponses_ForwardsFunctionCallArgumentsUnchanged(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, nil)
			var got providers.ChatRequest
			e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
				got = req
				return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-test","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)}, nil
			}
			e.fake.stream = func(_ context.Context, req providers.ChatRequest) (providers.Stream, error) {
				got = req
				return &compatTestStream{chunks: []providers.ChatChunk{
					{Data: json.RawMessage(`{"id":"chatcmpl-stream","object":"chat.completion.chunk","created":1760000000,"model":"model","choices":[{"index":0,"delta":{"content":"Done."},"finish_reason":"stop"}]}`)},
				}}, nil
			}
			body := fmt.Sprintf(`{"model":"fake/model","stream":%t,"input":[{"type":"message","role":"user","content":"Look it up."},{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{\"q\":\"a\"}"},{"type":"function_call_output","call_id":"call-1","output":"found"}]}`, stream)
			rec := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: body})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
			}
			var translated struct {
				Messages []struct {
					ToolCalls []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(got.RawBody, &translated); err != nil {
				t.Fatal("decode translated request:", err)
			}
			if len(translated.Messages) != 3 || len(translated.Messages[1].ToolCalls) != 1 || translated.Messages[1].ToolCalls[0].Function.Arguments != `{"q":"a"}` {
				t.Fatalf("forwarded tool call = %s, want arguments {\"q\":\"a\"}", got.RawBody)
			}
		})
	}
}

func TestResponses_RejectsNonStringFunctionCallArguments(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	body := `{"model":"fake/model","input":[{"type":"function_call","call_id":"call-1","name":"lookup","arguments":{"q":"secret-argument-value"}},{"type":"function_call_output","call_id":"call-1","output":"found"}]}`
	rec := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: body})
	if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "secret-argument-value") {
		t.Fatalf("status = %d body %s, want 400 without the argument content", rec.Code, rec.Body)
	}
}

func TestResponses_GroupsParallelFunctionCallsIntoOneAssistantMessage(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	var got providers.ChatRequest
	e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
		got = req
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-test","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)}, nil
	}
	body := `{"model":"fake/model","input":[{"type":"message","role":"user","content":"Run both."},{"type":"function_call","call_id":"call-1","name":"first","arguments":"{}"},{"type":"function_call","call_id":"call-2","name":"second","arguments":"{}"},{"type":"function_call_output","call_id":"call-1","output":"one"},{"type":"function_call_output","call_id":"call-2","output":"two"}]}`
	rec := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: body})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var translated struct {
		Messages []struct {
			Role      string            `json:"role"`
			ToolCalls []json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got.RawBody, &translated); err != nil {
		t.Fatal("decode translated request:", err)
	}
	if len(translated.Messages) != 4 || translated.Messages[0].Role != "user" || translated.Messages[1].Role != "assistant" || len(translated.Messages[1].ToolCalls) != 2 || translated.Messages[2].Role != "tool" || translated.Messages[3].Role != "tool" {
		t.Fatalf("translated messages = %#v, want user, one assistant with two calls, and two tool results", translated.Messages)
	}
}

func TestResponses_FoldsFunctionCallsIntoPrecedingAssistantText(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	var got providers.ChatRequest
	e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
		got = req
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-test","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)}, nil
	}
	body := `{"model":"fake/model","input":[{"type":"message","role":"user","content":"Check this."},{"type":"message","role":"assistant","content":"Checking."},{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call-1","output":"found"}]}`
	rec := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: body})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var translated struct {
		Messages []struct {
			Role      string            `json:"role"`
			Content   string            `json:"content"`
			ToolCalls []json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got.RawBody, &translated); err != nil {
		t.Fatal("decode translated request:", err)
	}
	if len(translated.Messages) != 3 || translated.Messages[1].Role != "assistant" || translated.Messages[1].Content != "Checking." || len(translated.Messages[1].ToolCalls) != 1 || translated.Messages[2].Role != "tool" {
		t.Fatalf("translated messages = %#v, want assistant text and call grouped before tool result", translated.Messages)
	}
}

func TestResponses_KeepsSequentialStepsSeparate(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	var got providers.ChatRequest
	e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
		got = req
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-test","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)}, nil
	}
	body := `{"model":"fake/model","input":[{"type":"message","role":"user","content":"Do two steps."},{"type":"function_call","call_id":"call-1","name":"first","arguments":"{}"},{"type":"function_call_output","call_id":"call-1","output":"one"},{"type":"function_call","call_id":"call-2","name":"second","arguments":"{}"},{"type":"function_call_output","call_id":"call-2","output":"two"}]}`
	rec := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: body})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var translated struct {
		Messages []struct {
			Role      string            `json:"role"`
			ToolCalls []json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got.RawBody, &translated); err != nil {
		t.Fatal("decode translated request:", err)
	}
	if len(translated.Messages) != 5 || len(translated.Messages[1].ToolCalls) != 1 || translated.Messages[2].Role != "tool" || len(translated.Messages[3].ToolCalls) != 1 || translated.Messages[4].Role != "tool" {
		t.Fatalf("translated messages = %#v, want two assistant steps separated by tool results", translated.Messages)
	}
}

func TestResponses_ToolOnlyAssistantHasNullContent(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	var got providers.ChatRequest
	e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
		got = req
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-test","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)}, nil
	}
	body := `{"model":"fake/model","input":[{"type":"message","role":"user","content":"Run a tool."},{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call-1","output":"found"}]}`
	rec := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: body})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var translated struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got.RawBody, &translated); err != nil {
		t.Fatal("decode translated request:", err)
	}
	if len(translated.Messages) != 3 || string(translated.Messages[1].Content) != "null" {
		t.Fatalf("assistant content = %s, want null", translated.Messages[1].Content)
	}
}

func TestResponses_SignedToolCallRoundTripsThoughtSignature(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	fixture, err := os.ReadFile(filepath.Join("testdata", "gemini-signed-tool-call.json"))
	if err != nil {
		t.Fatal("read signed tool-call fixture:", err)
	}
	var got providers.ChatRequest
	calls := 0
	e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
		calls++
		got = req
		if calls == 1 {
			return providers.ChatResponse{Body: fixture}, nil
		}
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-done","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)}, nil
	}
	first := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: `{"model":"fake/model","input":"Use a tool."}`})
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200; body %s", first.Code, first.Body)
	}
	var output struct {
		Output []struct {
			ID     string `json:"id"`
			CallID string `json:"call_id"`
		} `json:"output"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &output); err != nil {
		t.Fatal("decode first response:", err)
	}
	if len(output.Output) != 1 || output.Output[0].ID != "function-call-1" || !strings.HasPrefix(output.Output[0].CallID, "spwts1_") {
		t.Fatalf("function call output = %#v, want provider item ID and signed call ID", output.Output)
	}
	secondBody, err := json.Marshal(map[string]any{"model": "fake/model", "input": []any{
		map[string]any{"type": "function_call", "call_id": output.Output[0].CallID, "name": "lookup", "arguments": `{"query":"docs"}`},
		map[string]any{"type": "function_call_output", "call_id": output.Output[0].CallID, "output": "found"},
	}})
	if err != nil {
		t.Fatal("encode replay request:", err)
	}
	second := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: string(secondBody)})
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200; body %s", second.Code, second.Body)
	}
	var translated struct {
		Messages []struct {
			ToolCalls []struct {
				ID           string `json:"id"`
				ExtraContent struct {
					Google struct {
						ThoughtSignature string `json:"thought_signature"`
					} `json:"google"`
				} `json:"extra_content"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got.RawBody, &translated); err != nil {
		t.Fatal("decode translated request:", err)
	}
	if len(translated.Messages) != 2 || len(translated.Messages[0].ToolCalls) != 1 || translated.Messages[0].ToolCalls[0].ID != "function-call-1" || translated.Messages[0].ToolCalls[0].ExtraContent.Google.ThoughtSignature != "c2lnbmF0dXJlLWZvci10ZXN0cw==" || translated.Messages[1].ToolCallID != "function-call-1" {
		t.Fatalf("translated history = %#v, want restored provider ID and signature", translated.Messages)
	}
}

func TestResponses_UnsignedToolCallKeepsProviderID(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.fake.chat = func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-plain","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-plain","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)}, nil
	}
	rec := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: `{"model":"fake/model","input":"Use a tool."}`})
	var output struct {
		Output []struct {
			ID     string `json:"id"`
			CallID string `json:"call_id"`
		} `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &output); err != nil {
		t.Fatal("decode response:", err)
	}
	if rec.Code != http.StatusOK || len(output.Output) != 1 || output.Output[0].ID != "call-plain" || output.Output[0].CallID != "call-plain" {
		t.Fatalf("response = %d %#v, want unchanged provider ID", rec.Code, output.Output)
	}
}

func TestResponses_ParallelCallsSignOnlyTheFirst(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	fixture := []byte(`{"id":"chatcmpl-parallel","object":"chat.completion","created":1761609600,"model":"gemini","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"function-call-1","type":"function","function":{"name":"first","arguments":"{}"},"extra_content":{"google":{"thought_signature":"c2lnbmF0dXJlLWZvci10ZXN0cw=="}}},{"id":"function-call-2","type":"function","function":{"name":"second","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)
	var got providers.ChatRequest
	calls := 0
	e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
		calls++
		got = req
		if calls == 1 {
			return providers.ChatResponse{Body: fixture}, nil
		}
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-done","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)}, nil
	}
	first := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: `{"model":"fake/model","input":"Use two tools."}`})
	var output struct {
		Output []struct {
			CallID string `json:"call_id"`
		} `json:"output"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &output); err != nil {
		t.Fatal("decode first response:", err)
	}
	if first.Code != http.StatusOK || len(output.Output) != 2 || !strings.HasPrefix(output.Output[0].CallID, "spwts1_") || output.Output[1].CallID != "function-call-2" {
		t.Fatalf("call IDs = %#v, want only first signed", output.Output)
	}
	secondBody, _ := json.Marshal(map[string]any{"model": "fake/model", "input": []any{
		map[string]any{"type": "function_call", "call_id": output.Output[0].CallID, "name": "first", "arguments": "{}"},
		map[string]any{"type": "function_call", "call_id": output.Output[1].CallID, "name": "second", "arguments": "{}"},
	}})
	second := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: string(secondBody)})
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200; body %s", second.Code, second.Body)
	}
	var translated struct {
		Messages []struct {
			ToolCalls []struct {
				ID           string `json:"id"`
				ExtraContent struct {
					Google struct {
						ThoughtSignature string `json:"thought_signature"`
					} `json:"google"`
				} `json:"extra_content"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got.RawBody, &translated); err != nil {
		t.Fatal("decode replay request:", err)
	}
	if len(translated.Messages) != 1 || len(translated.Messages[0].ToolCalls) != 2 || translated.Messages[0].ToolCalls[0].ID != "function-call-1" || translated.Messages[0].ToolCalls[0].ExtraContent.Google.ThoughtSignature == "" || translated.Messages[0].ToolCalls[1].ID != "function-call-2" || translated.Messages[0].ToolCalls[1].ExtraContent.Google.ThoughtSignature != "" {
		t.Fatalf("replayed calls = %#v, want signature only on first", translated.Messages)
	}
}

func TestResponses_MalformedSignedCallIDIsRejected(t *testing.T) {
	t.Parallel()
	unknown := base64.RawURLEncoding.EncodeToString([]byte(`{"id":"call-1","ts":"secret-fragment","unknown":true}`))
	valid := base64.RawURLEncoding.EncodeToString([]byte(`{"id":"call-1","ts":"sig"}`))
	emptySignature := base64.RawURLEncoding.EncodeToString([]byte(`{"id":"call-1","ts":""}`))
	invalidCharacters := base64.RawURLEncoding.EncodeToString([]byte(`{"id":"call-1","ts":"bad*signature"}`))
	longID := base64.RawURLEncoding.EncodeToString([]byte(`{"id":"` + strings.Repeat("a", 257) + `","ts":"signature"}`))
	tests := []struct{ name, callID, noLeak string }{
		{"bad base64", "spwts1_***", "***"},
		{"padding", "spwts1_" + valid + "=", "call-1"},
		{"line feed in base64", "spwts1_" + valid[:5] + "\n" + valid[5:], "call-1"},
		{"carriage return in base64", "spwts1_" + valid[:5] + "\r" + valid[5:], "call-1"},
		{"unknown JSON field", "spwts1_" + unknown, "secret-fragment"},
		{"empty signature", "spwts1_" + emptySignature, "call-1"},
		{"invalid signature characters", "spwts1_" + invalidCharacters, "bad*signature"},
		{"oversized provider ID", "spwts1_" + longID, strings.Repeat("a", 20)},
		{"oversized encoded length", "spwts1_" + strings.Repeat("A", 100_000), strings.Repeat("A", 20)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, nil)
			e.fake.chat = func(providers.ChatRequest) (providers.ChatResponse, error) {
				t.Fatal("malformed signed ID reached provider")
				return providers.ChatResponse{}, nil
			}
			body, err := json.Marshal(map[string]any{"model": "fake/model", "input": []any{map[string]any{"type": "function_call", "call_id": tt.callID, "name": "lookup", "arguments": "{}"}}})
			if err != nil {
				t.Fatal("encode malformed call ID request:", err)
			}
			rec := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: string(body)})
			var response struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal("decode error response:", err)
			}
			if rec.Code != http.StatusBadRequest || response.Error.Type != "invalid_request_error" || strings.Contains(response.Error.Message, tt.callID) || tt.noLeak != "" && strings.Contains(response.Error.Message, tt.noLeak) {
				t.Fatalf("error = %d %+v; must not disclose client ID or payload", rec.Code, response.Error)
			}
		})
	}
}

func TestResponses_OversizedUpstreamSignatureIsNotEmbedded(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	signature := strings.Repeat("s", 64<<10+1)
	body, err := json.Marshal(map[string]any{"id": "chatcmpl-large", "object": "chat.completion", "created": 1761609600, "model": "gemini", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "function-call-large", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{}"}, "extra_content": map[string]any{"google": map[string]any{"thought_signature": signature}}}}}, "finish_reason": "tool_calls"}}})
	if err != nil {
		t.Fatal("encode provider response:", err)
	}
	e.fake.chat = func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{Body: body}, nil
	}
	rec := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: `{"model":"fake/model","input":"Use a tool."}`})
	var response struct {
		Output []struct {
			CallID string `json:"call_id"`
		} `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal("decode response:", err)
	}
	if rec.Code != http.StatusOK || len(response.Output) != 1 || response.Output[0].CallID != "function-call-large" {
		t.Fatalf("response = %d %#v, want the plain provider ID", rec.Code, response.Output)
	}
	if strings.Contains(e.logs.String(), signature) || !strings.Contains(e.logs.String(), `reason=too_large`) {
		t.Fatalf("logs = %s, want size warning without signature", e.logs)
	}
}

func TestClientCompatibility_ThoughtSignatureAbsentFromLogs(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	fixture, err := os.ReadFile(filepath.Join("testdata", "gemini-signed-tool-call.json"))
	if err != nil {
		t.Fatal("read signed tool-call fixture:", err)
	}
	e.fake.chat = func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{Body: fixture}, nil
	}
	first := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: `{"model":"fake/model","input":"Use a tool."}`})
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200; body %s", first.Code, first.Body)
	}
	var response struct {
		Output []struct {
			CallID string `json:"call_id"`
		} `json:"output"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil || len(response.Output) != 1 {
		t.Fatalf("decode first response: calls=%d, err=%v", len(response.Output), err)
	}
	secondBody, err := json.Marshal(map[string]any{"model": "fake/model", "input": []any{
		map[string]any{"type": "function_call", "call_id": response.Output[0].CallID, "name": "lookup", "arguments": `{"query":"docs"}`},
		map[string]any{"type": "function_call_output", "call_id": response.Output[0].CallID, "output": "found"},
	}})
	if err != nil {
		t.Fatal("encode replay request:", err)
	}
	if rec := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: string(secondBody)}); rec.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	if strings.Contains(e.logs.String(), "c2lnbmF0dXJlLWZvci10ZXN0cw==") || strings.Contains(e.logs.String(), "spwts1_") {
		t.Fatalf("tool signature or signed ID leaked to logs: %s", e.logs)
	}
}

func TestResponses_StreamEmbedsThoughtSignature(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	chunks := []providers.ChatChunk{
		{Data: json.RawMessage(`{"id":"chatcmpl-stream","object":"chat.completion.chunk","created":1760000000,"model":"gemini","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"function-call-1","type":"function","function":{"name":"lookup","arguments":"{}"},"extra_content":{"google":{"thought_signature":"c2lnbmF0dXJlLWZvci10ZXN0cw=="}}}]},"finish_reason":null}]}`)},
		{Data: json.RawMessage(`{"id":"chatcmpl-stream","object":"chat.completion.chunk","created":1760000000,"model":"gemini","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)},
	}
	e.fake.stream = func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &compatTestStream{chunks: chunks}, nil
	}
	rec := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: `{"model":"fake/model","input":"Use a tool.","stream":true}`})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	added, done := responseFunctionCallEvents(t, rec.Body.String())
	if len(added) != 1 || len(done) != 1 || added[0].CallID != done[0].CallID || !strings.HasPrefix(added[0].CallID, "spwts1_") {
		t.Fatalf("added=%#v done=%#v, want one matching signed call ID", added, done)
	}
}

func TestResponses_StreamSignatureAfterArgumentsChunk(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	chunks := []providers.ChatChunk{
		{Data: json.RawMessage(`{"id":"chatcmpl-stream","object":"chat.completion.chunk","created":1760000000,"model":"gemini","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"function-call-1","type":"function","function":{"name":"lookup","arguments":""}}]},"finish_reason":null}]}`)},
		{Data: json.RawMessage(`{"id":"chatcmpl-stream","object":"chat.completion.chunk","created":1760000000,"model":"gemini","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\":"}}]},"finish_reason":null}]}`)},
		{Data: json.RawMessage(`{"id":"chatcmpl-stream","object":"chat.completion.chunk","created":1760000000,"model":"gemini","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"docs\"}"},"extra_content":{"google":{"thought_signature":"c2lnbmF0dXJlLWZvci10ZXN0cw=="}}}]},"finish_reason":null}]}`)},
		{Data: json.RawMessage(`{"id":"chatcmpl-stream","object":"chat.completion.chunk","created":1760000000,"model":"gemini","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)},
	}
	e.fake.stream = func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &compatTestStream{chunks: chunks}, nil
	}
	rec := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: `{"model":"fake/model","input":"Use a tool.","stream":true}`})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	added, done := responseFunctionCallEvents(t, rec.Body.String())
	if len(added) != 1 || len(done) != 1 || added[0].CallID != done[0].CallID || !strings.HasPrefix(done[0].CallID, "spwts1_") || !strings.Contains(rec.Body.String(), `"arguments":"{\"query\":\"docs\"}"`) {
		t.Fatalf("stream = %s, want one signed added/done pair and joined arguments", rec.Body)
	}
}

func TestResponses_StreamTextStillStreamsBeforeToolCalls(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	chunks := []providers.ChatChunk{
		{Data: json.RawMessage(`{"id":"chatcmpl-stream","object":"chat.completion.chunk","created":1760000000,"model":"model","choices":[{"index":0,"delta":{"content":"Checking."},"finish_reason":null}]}`)},
		{Data: json.RawMessage(`{"id":"chatcmpl-stream","object":"chat.completion.chunk","created":1760000000,"model":"model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":null}]}`)},
		{Data: json.RawMessage(`{"id":"chatcmpl-stream","object":"chat.completion.chunk","created":1760000000,"model":"model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)},
	}
	e.fake.stream = func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &compatTestStream{chunks: chunks}, nil
	}
	rec := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: `{"model":"fake/model","input":"Use a tool.","stream":true}`})
	textDelta := strings.Index(rec.Body.String(), `event: response.output_text.delta`)
	toolAdded := strings.Index(rec.Body.String(), `"type":"function_call"`)
	if rec.Code != http.StatusOK || textDelta < 0 || toolAdded <= textDelta {
		t.Fatalf("stream = %d %s, want text emitted before completed tool call", rec.Code, rec.Body)
	}
}

func TestResponses_StreamUpstreamErrorDuringToolCallEmitsNoFunctionCall(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.fake.stream = func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &compatTestStream{chunks: []providers.ChatChunk{
			{Data: json.RawMessage(`{"id":"chatcmpl-stream","object":"chat.completion.chunk","created":1760000000,"model":"model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]},"finish_reason":null}]}`)},
			{Data: json.RawMessage(`{"error":{"message":"upstream failed"}}`)},
		}}, nil
	}
	rec := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: `{"model":"fake/model","input":"Use a tool.","stream":true}`})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: error\n") || strings.Contains(rec.Body.String(), `"type":"function_call"`) || strings.Contains(rec.Body.String(), "response.completed") {
		t.Fatalf("stream = %d %s, want error without any partial function call", rec.Code, rec.Body)
	}
}

func TestResponses_StreamClientDisconnectClosesUpstream(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	streams := make(chan *gatedStream, 1)
	e.fake.stream = func(ctx context.Context, _ providers.ChatRequest) (providers.Stream, error) {
		stream := newGatedStream(ctx, `{"id":"chatcmpl-disconnect","object":"chat.completion.chunk","created":1760000000,"model":"model","choices":[{"index":0,"delta":{},"finish_reason":null}]}`, `{"id":"chatcmpl-disconnect","object":"chat.completion.chunk","created":1760000000,"model":"model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":null}]}`)
		streams <- stream
		return stream, nil
	}
	srv := httptest.NewServer(e.handler)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/responses", strings.NewReader(`{"model":"fake/model","input":"Use a tool.","stream":true}`))
	if err != nil {
		t.Fatal("create streaming request:", err)
	}
	req.Header.Set("Authorization", "Bearer "+keyCI)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal("send streaming request:", err)
	}
	stream := <-streams
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal("read response.created:", err)
		}
		if strings.Contains(line, `"type":"response.created"`) {
			break
		}
	}
	cancel()
	_ = resp.Body.Close()
	select {
	case err := <-stream.ctxErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("upstream Recv error = %v, want context canceled", err)
		}
	case <-t.Context().Done():
		t.Fatal("upstream stream did not observe client disconnect")
	}
	select {
	case <-stream.closed:
	case <-t.Context().Done():
		t.Fatal("upstream stream was not closed after client disconnect")
	}
}

func responseFunctionCallEvents(t *testing.T, body string) ([]struct{ CallID string }, []struct{ CallID string }) {
	t.Helper()
	var added, done []struct{ CallID string }
	lastSequence := 0
	for _, frame := range strings.Split(body, "\n\n") {
		var event struct {
			Type           string `json:"type"`
			SequenceNumber int    `json:"sequence_number"`
			Item           struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
			} `json:"item"`
		}
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data: ") {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal("decode Responses event:", err)
				}
			}
		}
		if event.SequenceNumber != 0 {
			if event.SequenceNumber <= lastSequence {
				t.Fatalf("sequence_number = %d after %d", event.SequenceNumber, lastSequence)
			}
			lastSequence = event.SequenceNumber
		}
		if event.Item.Type != "function_call" {
			continue
		}
		value := struct{ CallID string }{event.Item.CallID}
		switch event.Type {
		case "response.output_item.added":
			added = append(added, value)
		case "response.output_item.done":
			done = append(done, value)
		}
	}
	return added, done
}

func TestResponses_AcceptsReplayedAssistantOutputText(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	var got providers.ChatRequest
	e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
		got = req
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-test","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"Again."},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)}, nil
	}
	body := `{"model":"fake/model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Hi."}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello.","annotations":[],"logprobs":[]}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"Again."}]}]}`
	rec := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: body})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	if len(got.Messages) != 3 || got.Messages[1].Role != "assistant" || got.Messages[1].Text() != "Hello." {
		t.Fatalf("translated messages = %#v, want replayed assistant text", got.Messages)
	}
}

func TestAnthropicMessages_UsesChatExecutionAndReturnsMessagesShape(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	var got providers.ChatRequest
	e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
		got = req
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-test","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"docs\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`)}, nil
	}
	body := `{"model":"fake/model","max_tokens":256,"system":"Be concise.","messages":[{"role":"user","content":[{"type":"text","text":"Use a tool."}]}],"tools":[{"name":"lookup","description":"Search docs","input_schema":{"type":"object","properties":{"query":{"type":"string"}}}}]}`
	rec := e.do(t, request{path: "/v1/messages", auth: "Bearer " + keyCI, body: body})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	if got.Model != "model" || len(got.Messages) != 2 || got.Messages[0].Role != "system" || got.Messages[1].Text() != "Use a tool." {
		t.Fatalf("translated request = %#v, want system and user messages", got)
	}
	var response struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal("decode Anthropic Messages response:", err)
	}
	if response.Type != "message" || response.Role != "assistant" || response.StopReason != "tool_use" || len(response.Content) != 1 || response.Content[0].Type != "tool_use" || response.Content[0].Name != "lookup" || string(response.Content[0].Input) != `{"query":"docs"}` || response.Usage.InputTokens != 4 || response.Usage.OutputTokens != 3 {
		t.Fatalf("Anthropic Messages response = %#v", response)
	}
}

func TestResponses_RejectsUnsupportedFieldsWithResponsesError(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	rec := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: `{"model":"fake/model","input":"hello","previous_response_id":"resp_previous"}`})
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode Responses error %q: %v", rec.Body, err)
	}
	if rec.Code != http.StatusBadRequest || body.Error.Type != "invalid_request_error" || !strings.Contains(body.Error.Message, "previous_response_id") {
		t.Fatalf("Responses error = %d %#v; want clear invalid_request_error", rec.Code, body)
	}
}

func TestAnthropicMessages_RejectsUnsupportedFieldsWithAnthropicError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, body, field string
	}{
		{"unknown thinking type", `"thinking":{"type":"always"}`, "thinking"},
		{"missing enabled budget", `"thinking":{"type":"enabled"}`, "budget_tokens"},
		{"disabled display", `"thinking":{"type":"disabled","display":"omitted"}`, "display"},
		{"between tools display", `"thinking":{"type":"between_tools","display":"summarized"}`, "display"},
		{"small budget", `"thinking":{"type":"enabled","budget_tokens":512}`, "budget_tokens"},
		{"budget reaches max tokens", `"thinking":{"type":"enabled","budget_tokens":32}`, "budget_tokens"},
		{"unsupported output format", `"output_config":{"format":{"type":"json_schema"}}`, "format"},
		{"unknown effort", `"output_config":{"effort":"extreme"}`, "effort"},
		{"edit without type", `"context_management":{"edits":[{"keep":"all"}]}`, "type"},
		{"edit with numeric type", `"context_management":{"edits":[{"type":1}]}`, "type"},
		{"edit that is not an object", `"context_management":{"edits":["clear"]}`, "context_management"},
		{"invalid cache control", `"cache_control":{"type":"persistent"}`, "cache_control"},
		{"container", `"container":{}`, "container"},
		{"mcp servers", `"mcp_servers":[]`, "mcp_servers"},
		{"non-string user id", `"metadata":{"user_id":7}`, "user_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, nil)
			body := `{"model":"fake/model","max_tokens":32,` + tt.body + `,"messages":[{"role":"user","content":"hello"}]}`
			rec := e.do(t, request{path: "/v1/messages", auth: "Bearer " + keyCI, body: body})
			var response struct {
				Type  string `json:"type"`
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode Anthropic error %q: %v", rec.Body, err)
			}
			if rec.Code != http.StatusBadRequest || response.Type != "error" || response.Error.Type != "invalid_request_error" || !strings.Contains(response.Error.Message, tt.field) {
				t.Fatalf("Anthropic error = %d %#v; want invalid_request_error naming %s", rec.Code, response, tt.field)
			}
		})
	}
}

func TestAnthropicMessages_AcceptsClaudeCodeFirstTurn(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	var got providers.ChatRequest
	e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
		got = req
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-test","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"Ready."},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)}, nil
	}
	fixture, err := os.ReadFile(filepath.Join("testdata", "claude-code-first-turn.json"))
	if err != nil {
		t.Fatal("read Claude Code fixture:", err)
	}
	rec := e.do(t, request{path: "/v1/messages", auth: "Bearer " + keyCI, body: string(fixture)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	for _, field := range []string{`"metadata"`, "user_test_0000", `"thinking"`, `"output_config"`, `"context_management"`, `"cache_control"`} {
		if strings.Contains(string(got.RawBody), field) {
			t.Errorf("upstream body contains ignored field %q: %s", field, got.RawBody)
		}
	}
	var upstream struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got.RawBody, &upstream); err != nil {
		t.Fatal("decode upstream body:", err)
	}
	var system string
	if len(upstream.Messages) > 0 {
		_ = json.Unmarshal(upstream.Messages[0].Content, &system)
	}
	if len(upstream.Messages) < 2 || system != "System block one.\n\nSystem block two." {
		t.Fatalf("system content = %#v, want blocks joined with blank line", upstream.Messages)
	}
}

func TestAnthropicMessages_AcceptsHintFields(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, fields string }{
		{"thinking adaptive", `"thinking":{"type":"adaptive"}`},
		{"thinking adaptive display", `"thinking":{"type":"adaptive","display":"summarized"}`},
		{"thinking adaptive omitted display", `"thinking":{"type":"adaptive","display":"omitted"}`},
		{"thinking disabled", `"thinking":{"type":"disabled"}`},
		{"thinking between tools", `"thinking":{"type":"between_tools"}`},
		{"thinking enabled", `"thinking":{"type":"enabled","budget_tokens":1024}`},
		{"thinking enabled display", `"thinking":{"type":"enabled","budget_tokens":1024,"display":"summarized"}`},
		{"thinking enabled omitted display", `"thinking":{"type":"enabled","budget_tokens":1024,"display":"omitted"}`},
		{"effort low", `"output_config":{"effort":"low"}`},
		{"effort medium", `"output_config":{"effort":"medium"}`},
		{"effort high", `"output_config":{"effort":"high"}`},
		{"effort xhigh", `"output_config":{"effort":"xhigh"}`},
		{"effort max", `"output_config":{"effort":"max"}`},
		{"clear tool uses edit", `"context_management":{"edits":[{"type":"clear_tool_uses_20250919"}]}`},
		{"clear thinking edit", `"context_management":{"edits":[{"type":"clear_thinking_20251015"}]}`},
		{"empty output config", `"output_config":{}`},
		{"clear thinking edit keep", `"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`},
		{"clear tool uses edit parameters", `"context_management":{"edits":[{"type":"clear_tool_uses_20250919","trigger":{"type":"input_tokens","value":30000},"keep":{"type":"tool_uses","value":3},"clear_at_least":{"type":"input_tokens","value":5000},"exclude_tools":["web_search"],"clear_tool_inputs":true}]}`},
		{"top-level cache control", `"cache_control":{"type":"ephemeral"}`},
		{"cache control one hour", `"cache_control":{"type":"ephemeral","ttl":"1h"}`},
		{"tool cache control custom tool", `"tools":[{"type":"custom","name":"lookup","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral","ttl":"1h"}}]`},
		{"image cache control", `"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.test/image.png"},"cache_control":{"type":"ephemeral"}}]}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, nil)
			e.fake.chat = func(providers.ChatRequest) (providers.ChatResponse, error) {
				return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-test","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"Ready."},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)}, nil
			}
			maxTokens := 32
			if strings.HasPrefix(tt.name, "thinking enabled") {
				maxTokens = 2048
			}
			base := fmt.Sprintf(`"model":"fake/model","max_tokens":%d,"messages":[{"role":"user","content":"hello"}]`, maxTokens)
			fields := strings.TrimPrefix(tt.fields, `"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.test/image.png"},"cache_control":{"type":"ephemeral"}}]}]`)
			if tt.name == "image cache control" {
				base = `"model":"fake/model","max_tokens":32`
			}
			body := "{" + base
			if fields != "" {
				body += "," + fields
			}
			if tt.name == "image cache control" {
				body += "," + tt.fields
			}
			body += "}"
			rec := e.do(t, request{path: "/v1/messages", auth: "Bearer " + keyCI, body: body})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestAnthropicMessages_DoesNotForwardOrLogMetadataUserID(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	var got providers.ChatRequest
	e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
		got = req
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-test","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"Ready."},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)}, nil
	}
	rec := e.do(t, request{path: "/v1/messages", auth: "Bearer " + keyCI, body: `{"model":"fake/model","max_tokens":32,"metadata":{"user_id":"user_test_private_987"},"messages":[{"role":"user","content":"hello"}]}`})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	if strings.Contains(string(got.RawBody)+e.logs.String(), "user_test_private_987") {
		t.Fatalf("user identifier leaked: upstream=%s logs=%s", got.RawBody, e.logs)
	}
}

func TestAnthropicMessages_AcceptsClaudeCodeToolTurn(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	var got providers.ChatRequest
	e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
		got = req
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-tool-turn","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)}, nil
	}
	fixture, err := os.ReadFile(filepath.Join("testdata", "claude-code-tool-turn.json"))
	if err != nil {
		t.Fatal("read Claude Code tool-turn fixture:", err)
	}
	rec := e.do(t, request{path: "/v1/messages", auth: "Bearer " + keyCI, body: string(fixture)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	if strings.Contains(string(got.RawBody), "private thought text") || strings.Contains(string(got.RawBody), "private-thinking-signature") || strings.Contains(string(got.RawBody), "private-redacted-data") {
		t.Fatalf("thinking data reached provider: %s", got.RawBody)
	}
	var upstream struct {
		Messages []struct {
			Role      string            `json:"role"`
			Content   json.RawMessage   `json:"content"`
			ToolCalls []json.RawMessage `json:"tool_calls"`
			ToolID    string            `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got.RawBody, &upstream); err != nil {
		t.Fatal("decode upstream request:", err)
	}
	if len(upstream.Messages) != 3 || upstream.Messages[0].Role != "assistant" || len(upstream.Messages[0].ToolCalls) != 1 || !strings.Contains(string(upstream.Messages[0].Content), "Checking the file.") || upstream.Messages[1].Role != "tool" || upstream.Messages[1].ToolID != "call-1" || upstream.Messages[1].Content == nil || upstream.Messages[2].Role != "user" {
		t.Fatalf("translated messages = %#v, want assistant call, tool result, then user text", upstream.Messages)
	}
}

func TestAnthropicMessages_OrdersToolResultsBeforeUserText(t *testing.T) {
	t.Parallel()
	for _, content := range []string{
		`[{"type":"tool_result","tool_use_id":"call-1","content":"done"},{"type":"text","text":"after"}]`,
		`[{"type":"text","text":"before"},{"type":"tool_result","tool_use_id":"call-1","content":"done"}]`,
	} {
		t.Run(content, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, nil)
			var got providers.ChatRequest
			e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
				got = req
				return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-order","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)}, nil
			}
			body := `{"model":"fake/model","max_tokens":32,"messages":[{"role":"user","content":` + content + `}]}`
			rec := e.do(t, request{path: "/v1/messages", auth: "Bearer " + keyCI, body: body})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
			}
			var translated struct {
				Messages []struct {
					Role string `json:"role"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(got.RawBody, &translated); err != nil {
				t.Fatal("decode upstream request:", err)
			}
			if len(translated.Messages) != 2 || translated.Messages[0].Role != "tool" || translated.Messages[1].Role != "user" {
				t.Fatalf("translated messages = %#v, want tool before user", translated.Messages)
			}
		})
	}
}

func TestAnthropicMessages_DropsThinkingBlocksFromHistory(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	var got providers.ChatRequest
	e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
		got = req
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-thinking","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)}, nil
	}
	body := `{"model":"fake/model","max_tokens":32,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"private thought","signature":"secret-sig"},{"type":"redacted_thinking","data":"secret-data"},{"type":"text","text":"visible"}]},{"role":"user","content":"continue"}]}`
	rec := e.do(t, request{path: "/v1/messages", auth: "Bearer " + keyCI, body: body})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	for _, forbidden := range []string{"private thought", "secret-sig", "secret-data"} {
		if strings.Contains(string(got.RawBody), forbidden) {
			t.Errorf("upstream body contains %q: %s", forbidden, got.RawBody)
		}
	}
}

func TestAnthropicMessages_ToolOnlyAssistantHasNullContent(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	var got providers.ChatRequest
	e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
		got = req
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-tool-only","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)}, nil
	}
	body := `{"model":"fake/model","max_tokens":32,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"found"}]}]}`
	rec := e.do(t, request{path: "/v1/messages", auth: "Bearer " + keyCI, body: body})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var translated struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got.RawBody, &translated); err != nil {
		t.Fatal("decode upstream request:", err)
	}
	if len(translated.Messages) != 2 || string(translated.Messages[0].Content) != "null" {
		t.Fatalf("assistant content = %s, want null", translated.Messages[0].Content)
	}
}

func TestAnthropicMessages_ToolResultIsErrorKeepsContent(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	var got providers.ChatRequest
	e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
		got = req
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-tool-error","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)}, nil
	}
	body := `{"model":"fake/model","max_tokens":32,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","is_error":true,"content":"file missing"}]}]}`
	rec := e.do(t, request{path: "/v1/messages", auth: "Bearer " + keyCI, body: body})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var translated struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got.RawBody, &translated); err != nil {
		t.Fatal("decode upstream request:", err)
	}
	if len(translated.Messages) != 1 || translated.Messages[0].Content != "file missing" {
		t.Fatalf("tool result = %#v, want preserved error content", translated.Messages)
	}
}

func TestAnthropicMessages_ToolResultImageIsRejectedClearly(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	body := `{"model":"fake/model","max_tokens":32,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":[{"type":"image","source":{"type":"url","url":"https://example.test/image.png"}}]}]}]}`
	rec := e.do(t, request{path: "/v1/messages", auth: "Bearer " + keyCI, body: body})
	var response struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal("decode error response:", err)
	}
	if rec.Code != http.StatusBadRequest || response.Error.Type != "invalid_request_error" || !strings.Contains(response.Error.Message, "image tool results") {
		t.Fatalf("error = %d %+v, want clear image tool result rejection", rec.Code, response.Error)
	}
}

func TestAnthropicMessages_SignedToolUseRoundTripsThoughtSignature(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	fixture, err := os.ReadFile(filepath.Join("testdata", "gemini-signed-tool-call.json"))
	if err != nil {
		t.Fatal("read signed tool-call fixture:", err)
	}
	var got providers.ChatRequest
	calls := 0
	e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
		calls++
		got = req
		if calls == 1 {
			return providers.ChatResponse{Body: fixture}, nil
		}
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-done","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)}, nil
	}
	first := e.do(t, request{path: "/v1/messages", auth: "Bearer " + keyCI, body: `{"model":"fake/model","max_tokens":128,"messages":[{"role":"user","content":"Use a tool."}]}`})
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200; body %s", first.Code, first.Body)
	}
	var response struct {
		Content []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"content"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil {
		t.Fatal("decode first response:", err)
	}
	if len(response.Content) != 1 || response.Content[0].Type != "tool_use" || !regexp.MustCompile(`^spwts1_[A-Za-z0-9_-]+$`).MatchString(response.Content[0].ID) {
		t.Fatalf("tool_use = %#v, want alphabet-safe signed ID", response.Content)
	}
	secondBody, err := json.Marshal(map[string]any{"model": "fake/model", "max_tokens": 128, "messages": []any{
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": response.Content[0].ID, "name": "lookup", "input": map[string]any{"query": "docs"}}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": response.Content[0].ID, "content": "found"}}},
	}})
	if err != nil {
		t.Fatal("encode replay request:", err)
	}
	second := e.do(t, request{path: "/v1/messages", auth: "Bearer " + keyCI, body: string(secondBody)})
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200; body %s", second.Code, second.Body)
	}
	var translated struct {
		Messages []struct {
			ToolCalls []struct {
				ID           string `json:"id"`
				ExtraContent struct {
					Google struct {
						ThoughtSignature string `json:"thought_signature"`
					} `json:"google"`
				} `json:"extra_content"`
			} `json:"tool_calls"`
			ToolID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got.RawBody, &translated); err != nil {
		t.Fatal("decode translated request:", err)
	}
	if len(translated.Messages) != 2 || len(translated.Messages[0].ToolCalls) != 1 || translated.Messages[0].ToolCalls[0].ID != "function-call-1" || translated.Messages[0].ToolCalls[0].ExtraContent.Google.ThoughtSignature != "c2lnbmF0dXJlLWZvci10ZXN0cw==" || translated.Messages[1].ToolID != "function-call-1" {
		t.Fatalf("translated history = %#v, want restored ID and signature", translated.Messages)
	}
}

func TestAnthropicMessages_MalformedSignedToolUseIDIsRejected(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, message string }{
		{"tool_use ID", `{"role":"assistant","content":[{"type":"tool_use","id":"spwts1_***","name":"lookup","input":{}}]}`},
		{"tool_result ID", `{"role":"user","content":[{"type":"tool_result","tool_use_id":"spwts1_***","content":"found"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, nil)
			e.fake.chat = func(providers.ChatRequest) (providers.ChatResponse, error) {
				t.Fatal("malformed signed ID reached provider")
				return providers.ChatResponse{}, nil
			}
			body := `{"model":"fake/model","max_tokens":32,"messages":[` + tt.message + `]}`
			rec := e.do(t, request{path: "/v1/messages", auth: "Bearer " + keyCI, body: body})
			var response struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal("decode error response:", err)
			}
			if rec.Code != http.StatusBadRequest || response.Error.Type != "invalid_request_error" || strings.Contains(response.Error.Message, "spwts1_***") {
				t.Fatalf("error = %d %+v, want sanitized invalid request", rec.Code, response.Error)
			}
		})
	}
}

func TestAnthropicMessages_StreamSignatureAfterArgumentsChunk(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.fake.stream = func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &compatTestStream{chunks: []providers.ChatChunk{
			{Data: json.RawMessage(`{"id":"chatcmpl-anthropic-stream","object":"chat.completion.chunk","created":1760000000,"model":"gemini","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"function-call-1","type":"function","function":{"name":"lookup","arguments":""}}]},"finish_reason":null}]}`)},
			{Data: json.RawMessage(`{"id":"chatcmpl-anthropic-stream","object":"chat.completion.chunk","created":1760000000,"model":"gemini","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\":"}}]},"finish_reason":null}]}`)},
			{Data: json.RawMessage(`{"id":"chatcmpl-anthropic-stream","object":"chat.completion.chunk","created":1760000000,"model":"gemini","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"docs\"}"},"extra_content":{"google":{"thought_signature":"c2lnbmF0dXJlLWZvci10ZXN0cw=="}}}]},"finish_reason":null}]}`)},
			{Data: json.RawMessage(`{"id":"chatcmpl-anthropic-stream","object":"chat.completion.chunk","created":1760000000,"model":"gemini","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)},
		}}, nil
	}
	rec := e.do(t, request{path: "/v1/messages", auth: "Bearer " + keyCI, body: `{"model":"fake/model","max_tokens":128,"messages":[{"role":"user","content":"Use a tool."}],"stream":true}`})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var id string
	var jsonDelta string
	for _, frame := range strings.Split(rec.Body.String(), "\n\n") {
		if !strings.Contains(frame, "data: ") {
			continue
		}
		var event struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data: ") {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal("decode Messages event:", err)
				}
			}
		}
		if event.Type == "content_block_start" && event.ContentBlock.Type == "tool_use" {
			id = event.ContentBlock.ID
		}
		if event.Type == "content_block_delta" && event.Delta.Type == "input_json_delta" {
			jsonDelta = event.Delta.PartialJSON
		}
	}
	if !regexp.MustCompile(`^spwts1_[A-Za-z0-9_-]+$`).MatchString(id) || jsonDelta != `{"query":"docs"}` || strings.Count(rec.Body.String(), "event: content_block_start\n") != 1 {
		t.Fatalf("stream = %s, want one signed tool block with full arguments", rec.Body)
	}
}

func TestAnthropicMessages_StreamTextStillStreamsBeforeToolCalls(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.fake.stream = func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &compatTestStream{chunks: []providers.ChatChunk{
			{Data: json.RawMessage(`{"id":"chatcmpl-anthropic-stream","object":"chat.completion.chunk","created":1760000000,"model":"model","choices":[{"index":0,"delta":{"content":"Checking."},"finish_reason":null}]}`)},
			{Data: json.RawMessage(`{"id":"chatcmpl-anthropic-stream","object":"chat.completion.chunk","created":1760000000,"model":"model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":null}]}`)},
			{Data: json.RawMessage(`{"id":"chatcmpl-anthropic-stream","object":"chat.completion.chunk","created":1760000000,"model":"model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)},
		}}, nil
	}
	rec := e.do(t, request{path: "/v1/messages", auth: "Bearer " + keyCI, body: `{"model":"fake/model","max_tokens":128,"messages":[{"role":"user","content":"Use a tool."}],"stream":true}`})
	textDelta := strings.Index(rec.Body.String(), "event: content_block_delta\n")
	toolStart := strings.Index(rec.Body.String(), `"type":"tool_use"`)
	var indexes []int
	for _, frame := range strings.Split(rec.Body.String(), "\n\n") {
		if !strings.Contains(frame, "event: content_block_start\n") {
			continue
		}
		var event struct {
			Index int `json:"index"`
		}
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data: ") {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal("decode block start:", err)
				}
			}
		}
		indexes = append(indexes, event.Index)
	}
	if rec.Code != http.StatusOK || textDelta < 0 || toolStart <= textDelta || len(indexes) != 2 || indexes[0] != 0 || indexes[1] != 1 {
		t.Fatalf("stream = %d %s, want text before complete tool call", rec.Code, rec.Body)
	}
}

func TestAnthropicMessages_StreamUpstreamErrorDuringToolCallEmitsNoToolUse(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.fake.stream = func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &compatTestStream{chunks: []providers.ChatChunk{
			{Data: json.RawMessage(`{"id":"chatcmpl-anthropic-stream","object":"chat.completion.chunk","created":1760000000,"model":"model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]},"finish_reason":null}]}`)},
			{Data: json.RawMessage(`{"error":{"message":"upstream failed"}}`)},
		}}, nil
	}
	rec := e.do(t, request{path: "/v1/messages", auth: "Bearer " + keyCI, body: `{"model":"fake/model","max_tokens":128,"messages":[{"role":"user","content":"Use a tool."}],"stream":true}`})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: error\n") || strings.Contains(rec.Body.String(), `"type":"tool_use"`) || strings.Contains(rec.Body.String(), "message_stop") {
		t.Fatalf("stream = %d %s, want error without partial tool call", rec.Code, rec.Body)
	}
}

func TestAnthropicMessages_StreamClientDisconnectClosesUpstream(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	streams := make(chan *gatedStream, 1)
	e.fake.stream = func(ctx context.Context, _ providers.ChatRequest) (providers.Stream, error) {
		stream := newGatedStream(ctx, `{"id":"chatcmpl-disconnect","object":"chat.completion.chunk","created":1760000000,"model":"model","choices":[{"index":0,"delta":{},"finish_reason":null}]}`, `{"id":"chatcmpl-disconnect","object":"chat.completion.chunk","created":1760000000,"model":"model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":null}]}`)
		streams <- stream
		return stream, nil
	}
	srv := httptest.NewServer(e.handler)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{"model":"fake/model","max_tokens":128,"messages":[{"role":"user","content":"Use a tool."}],"stream":true}`))
	if err != nil {
		t.Fatal("create streaming request:", err)
	}
	req.Header.Set("Authorization", "Bearer "+keyCI)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal("send streaming request:", err)
	}
	stream := <-streams
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal("read message_start:", err)
		}
		if strings.Contains(line, `"type":"message_start"`) {
			break
		}
	}
	cancel()
	_ = resp.Body.Close()
	select {
	case err := <-stream.ctxErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("upstream Recv error = %v, want context canceled", err)
		}
	case <-t.Context().Done():
		t.Fatal("upstream stream did not observe client disconnect")
	}
	select {
	case <-stream.closed:
	case <-t.Context().Done():
		t.Fatal("upstream stream was not closed after client disconnect")
	}
}

func TestAnthropicMessages_UsesXAPIKeyAuthentication(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.fake.chat = func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-auth","object":"chat.completion","created":1,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)}, nil
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"fake/model","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("x-api-key", keyCI)
	request.Header.Set("anthropic-version", "2023-06-01")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	e.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("x-api-key response = %d %s, want 200", response.Code, response.Body)
	}
}

func TestAnthropicMessages_AuthenticationFailureUsesAnthropicErrorShape(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	rec := e.do(t, request{path: "/v1/messages", body: `{"model":"fake/model","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`})
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode auth error %q: %v", rec.Body, err)
	}
	if rec.Code != http.StatusUnauthorized || body.Type != "error" || body.Error.Type != "authentication_error" {
		t.Fatalf("Anthropic auth error = %d %#v; want authentication_error", rec.Code, body)
	}
}

func TestAnthropicMessages_PreservesBase64ImageData(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	var rawRequest json.RawMessage
	e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
		rawRequest = req.RawBody
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-image","object":"chat.completion","created":1,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"seen"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)}, nil
	}
	rec := e.do(t, request{path: "/v1/messages", auth: "Bearer " + keyCI, body: `{"model":"fake/model","max_tokens":16,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}]}]}`})
	if rec.Code != http.StatusOK || !strings.Contains(string(rawRequest), `data:image/png;base64,YWJj`) {
		t.Fatalf("translated request = %s, status = %d", rawRequest, rec.Code)
	}
}

func TestClientCompatibility_RequiresAuthentication(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	for _, path := range []string{"/v1/messages", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			rec := e.do(t, request{path: path, body: `{"model":"fake/model","messages":[{"role":"user","content":"hi"}]}`})
			if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), keyCI) {
				t.Fatalf("unauthorized response = %d %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestResponses_StreamsResponsesEvents(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	rec := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: `{"model":"mock/echo","input":"hello stream","stream":true}`})
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("response = %d %q %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	for _, event := range []string{"response.created", "response.output_text.delta", "response.completed"} {
		if !strings.Contains(rec.Body.String(), "event: "+event+"\n") {
			t.Errorf("stream missing %s event: %s", event, rec.Body)
		}
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("Responses stream contains Chat Completions sentinel: %s", rec.Body)
	}
}

func TestResponses_StreamsNamespacedToolCalls(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.fake.stream = func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return &compatTestStream{chunks: []providers.ChatChunk{
			{Data: json.RawMessage(`{"id":"chatcmpl-tool","object":"chat.completion.chunk","created":1760000000,"model":"model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"spillway_tool_0","arguments":"{}"}}]},"finish_reason":null}]}`)},
			{Data: json.RawMessage(`{"id":"chatcmpl-tool","object":"chat.completion.chunk","created":1760000000,"model":"model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)},
		}}, nil
	}
	body := `{"model":"fake/model","input":"call lookup","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}],"stream":true}`
	rec := e.do(t, request{path: "/v1/responses", auth: "Bearer " + keyCI, body: body})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"namespace":"functions"`) || !strings.Contains(rec.Body.String(), `"name":"lookup"`) {
		t.Fatalf("namespaced Responses stream = %d %s", rec.Code, rec.Body)
	}
}

func TestAnthropicMessages_StreamsMessagesEvents(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	rec := e.do(t, request{path: "/v1/messages", auth: "Bearer " + keyCI, body: `{"model":"mock/echo","max_tokens":64,"messages":[{"role":"user","content":"hello stream"}],"stream":true}`})
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("response = %d %q %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	for _, event := range []string{"message_start", "content_block_delta", "message_delta", "message_stop"} {
		if !strings.Contains(rec.Body.String(), "event: "+event+"\n") {
			t.Errorf("stream missing %s event: %s", event, rec.Body)
		}
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("Anthropic stream contains Chat Completions sentinel: %s", rec.Body)
	}
}

func TestClientCompatibility_StreamsToolCalls(t *testing.T) {
	t.Parallel()
	chunks := []providers.ChatChunk{
		{Data: json.RawMessage(`{"id":"chatcmpl-tool","object":"chat.completion.chunk","created":1760000000,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"query\":"}}]},"finish_reason":null}]}`)},
		{Data: json.RawMessage(`{"id":"chatcmpl-tool","object":"chat.completion.chunk","created":1760000000,"model":"model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"docs\"}"}}]},"finish_reason":null}]}`)},
		{Data: json.RawMessage(`{"id":"chatcmpl-tool","object":"chat.completion.chunk","created":1760000000,"model":"model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)},
	}
	for _, test := range []struct {
		name string
		path string
		body string
		want []string
	}{
		{name: "responses", path: "/v1/responses", body: `{"model":"fake/model","input":"call lookup","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"stream":true}`, want: []string{"response.function_call_arguments.delta", "response.function_call_arguments.done", `"type":"function_call"`}},
		{name: "anthropic", path: "/v1/messages", body: `{"model":"fake/model","max_tokens":128,"messages":[{"role":"user","content":"call lookup"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"stream":true}`, want: []string{"content_block_start", "tool_use", "input_json_delta", "message_stop"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.fake.stream = func(context.Context, providers.ChatRequest) (providers.Stream, error) {
				return &compatTestStream{chunks: append([]providers.ChatChunk(nil), chunks...)}, nil
			}
			rec := e.do(t, request{path: test.path, auth: "Bearer " + keyCI, body: test.body})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
			}
			for _, want := range test.want {
				if !strings.Contains(rec.Body.String(), want) {
					t.Errorf("stream missing %q: %s", want, rec.Body)
				}
			}
		})
	}
}

func TestClientCompatibility_StreamErrorDoesNotEmitSuccessTerminalEvent(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		path string
		body string
		bad  string
	}{
		{path: "/v1/responses", body: `{"model":"fake/model","input":"hello","stream":true}`, bad: "response.completed"},
		{path: "/v1/messages", body: `{"model":"fake/model","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":true}`, bad: "message_stop"},
	} {
		t.Run(test.path, func(t *testing.T) {
			e := newEnv(t, nil)
			e.fake.stream = func(context.Context, providers.ChatRequest) (providers.Stream, error) {
				return &compatTestStream{chunks: []providers.ChatChunk{{Data: json.RawMessage(`{"error":{"message":"upstream failed"}}`)}}}, nil
			}
			rec := e.do(t, request{path: test.path, auth: "Bearer " + keyCI, body: test.body})
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: error\n") || strings.Contains(rec.Body.String(), test.bad) {
				t.Fatalf("stream error response = %d %s", rec.Code, rec.Body)
			}
		})
	}
}

type compatTestStream struct {
	chunks []providers.ChatChunk
}

func (s *compatTestStream) Recv() (providers.ChatChunk, error) {
	if len(s.chunks) == 0 {
		return providers.ChatChunk{}, io.EOF
	}
	chunk := s.chunks[0]
	s.chunks = s.chunks[1:]
	return chunk, nil
}

func (*compatTestStream) Close() error { return nil }
