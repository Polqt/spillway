package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
		return providers.ChatResponse{Body: json.RawMessage(`{"id":"chatcmpl-test","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"docs\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`)}, nil
	}
	body := `{"model":"fake/model","instructions":"Be concise.","store":false,"include":["reasoning.encrypted_content"],"client_metadata":{"app":"codex"},"prompt_cache_key":"codex-session","service_tier":"priority","parallel_tool_calls":true,"reasoning":{"effort":"medium","summary":"auto"},"text":{"verbosity":"medium"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Use a tool."}]}],"tools":[{"type":"function","name":"lookup","description":"Search docs","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}],"tool_choice":"auto"}`
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
	if err := json.Unmarshal(request["tools"], &tools); err != nil || len(tools) != 1 || tools[0].Type != "function" || tools[0].Function.Name != "lookup" {
		t.Fatalf("chat tools = %s, want chat-completions function shape", request["tools"])
	}
	var response struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal("decode Responses response:", err)
	}
	if response.Object != "response" || response.Status != "completed" || len(response.Output) != 1 || response.Output[0].Type != "function_call" || response.Output[0].Name != "lookup" || response.Output[0].Arguments != `{"query":"docs"}` {
		t.Fatalf("Responses output = %#v", response)
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
	e := newEnv(t, nil)
	rec := e.do(t, request{path: "/v1/messages", auth: "Bearer " + keyCI, body: `{"model":"fake/model","max_tokens":32,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hello"}]}`})
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode Anthropic error %q: %v", rec.Body, err)
	}
	if rec.Code != http.StatusBadRequest || body.Type != "error" || body.Error.Type != "invalid_request_error" || !strings.Contains(body.Error.Message, "thinking") {
		t.Fatalf("Anthropic error = %d %#v; want clear invalid_request_error", rec.Code, body)
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
