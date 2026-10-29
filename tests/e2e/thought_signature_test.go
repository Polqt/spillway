package e2e_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	thoughtSignatureGeminiKey = "sk-test-gemini-thought-signature"
	thoughtSignatureGroqKey   = "sk-test-groq-thought-signature"
	thoughtSignatureLocalKey  = "sk-test-local-thought-signature"
	thoughtSignatureValue     = "sig-from-gemini-e2e"
)

func TestBinary_GeminiSignedToolLoopThroughResponses(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertThoughtSignatureProviderAuth(t, r, thoughtSignatureGeminiKey)
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode Gemini request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL.Path != "/v1beta/openai/chat/completions" {
			t.Errorf("Gemini path = %q", r.URL.Path)
		}
		if requests.Add(1) == 1 {
			writeThoughtSignatureToolStream(w, "function-call-responses")
			return
		}
		assertRealThoughtSignature(t, body, "function-call-responses")
		writeThoughtSignatureTextStream(w, "Responses loop complete.")
	}))
	t.Cleanup(upstream.Close)
	baseURL, output := startThoughtSignatureBinary(t, upstream.URL+"/v1beta/openai", upstream.URL+"/v1", "gemini/gemini-3-flash-preview, groq/test-model")
	first := requestBinaryAPI(t, baseURL, http.MethodPost, "/v1/responses", `{"model":"gemini/gemini-3-flash-preview","input":"Use a tool.","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"stream":true}`, thoughtSignatureLocalKey, http.StatusOK, "event: response.completed")
	callID := responsesStreamCallID(t, first)
	if !strings.HasPrefix(callID, "spwts1_") {
		t.Fatalf("Responses call ID = %q, want signed ID", callID)
	}
	secondBody, err := json.Marshal(map[string]any{"model": "gemini/gemini-3-flash-preview", "stream": true, "input": []any{
		map[string]any{"type": "message", "role": "user", "content": "Use a tool."},
		map[string]any{"type": "function_call", "call_id": callID, "name": "lookup", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": callID, "output": "done"},
	}})
	if err != nil {
		t.Fatal("encode Responses replay:", err)
	}
	second := requestBinaryAPI(t, baseURL, http.MethodPost, "/v1/responses", string(secondBody), thoughtSignatureLocalKey, http.StatusOK, "Responses loop complete.")
	assertNoThoughtSignatureLeak(t, first+second, output.String())
	if got := requests.Load(); got != 2 {
		t.Fatalf("Gemini requests = %d, want 2", got)
	}
}

func TestBinary_GeminiSignedToolLoopThroughMessages(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertThoughtSignatureProviderAuth(t, r, thoughtSignatureGeminiKey)
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode Gemini request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL.Path != "/v1beta/openai/chat/completions" {
			t.Errorf("Gemini path = %q", r.URL.Path)
		}
		if requests.Add(1) == 1 {
			writeThoughtSignatureToolStream(w, "function-call-messages")
			return
		}
		assertRealThoughtSignature(t, body, "function-call-messages")
		writeThoughtSignatureTextStream(w, "Messages loop complete.")
	}))
	t.Cleanup(upstream.Close)
	baseURL, output := startThoughtSignatureBinary(t, upstream.URL+"/v1beta/openai", upstream.URL+"/v1", "gemini/gemini-3-flash-preview, groq/test-model")
	first := requestBinaryAPI(t, baseURL, http.MethodPost, "/v1/messages", `{"model":"gemini/gemini-3-flash-preview","max_tokens":128,"messages":[{"role":"user","content":"Use a tool."}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"stream":true}`, thoughtSignatureLocalKey, http.StatusOK, "event: message_stop")
	callID := anthropicStreamToolUseID(t, first)
	if !strings.HasPrefix(callID, "spwts1_") {
		t.Fatalf("Messages tool_use ID = %q, want signed ID", callID)
	}
	secondBody, err := json.Marshal(map[string]any{"model": "gemini/gemini-3-flash-preview", "max_tokens": 128, "stream": true, "messages": []any{
		map[string]any{"role": "user", "content": "Use a tool."},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": callID, "name": "lookup", "input": map[string]any{}}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": callID, "content": "done"}}},
	}})
	if err != nil {
		t.Fatal("encode Messages replay:", err)
	}
	second := requestBinaryAPI(t, baseURL, http.MethodPost, "/v1/messages", string(secondBody), thoughtSignatureLocalKey, http.StatusOK, "Messages loop complete.")
	assertNoThoughtSignatureLeak(t, first+second, output.String())
	if got := requests.Load(); got != 2 {
		t.Fatalf("Gemini requests = %d, want 2", got)
	}
}

func TestBinary_SpilloverGeminiHistoryToGroqStripsSignature(t *testing.T) {
	var geminiRequests, groqRequests atomic.Int32
	gemini := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertThoughtSignatureProviderAuth(t, r, thoughtSignatureGeminiKey)
		if geminiRequests.Add(1) == 1 {
			writeThoughtSignatureChatResponse(w, "function-call-spill")
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"quota reached"}}`)
	}))
	t.Cleanup(gemini.Close)
	groq := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertThoughtSignatureProviderAuth(t, r, thoughtSignatureGroqKey)
		groqRequests.Add(1)
		var body struct {
			Messages []struct {
				Role      string `json:"role"`
				ToolCalls []struct {
					ID           string          `json:"id"`
					ExtraContent json.RawMessage `json:"extra_content"`
				} `json:"tool_calls"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode Groq request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, message := range body.Messages {
			if message.Role == "assistant" && len(message.ToolCalls) > 0 {
				if message.ToolCalls[0].ID != "function-call-spill" || len(message.ToolCalls[0].ExtraContent) != 0 {
					t.Errorf("Groq tool call = %+v, want provider ID without extra_content", message.ToolCalls[0])
				}
			}
		}
		writeThoughtSignatureChatJSON(w, "Groq spillover complete.")
	}))
	t.Cleanup(groq.Close)
	baseURL, output := startThoughtSignatureBinary(t, gemini.URL+"/v1beta/openai", groq.URL+"/v1", "gemini/gemini-3-flash-preview, groq/test-model")
	first := requestBinaryAPI(t, baseURL, http.MethodPost, "/v1/responses", `{"model":"spillway/demo","input":"Use a tool."}`, thoughtSignatureLocalKey, http.StatusOK, "function-call-spill")
	callID := responsesOutputCallID(t, first)
	secondBody := fmt.Sprintf(`{"model":"spillway/demo","input":[{"type":"message","role":"user","content":"Use a tool."},{"type":"function_call","call_id":%q,"name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":%q,"output":"done"}]}`, callID, callID)
	second := requestBinaryAPIDetails(t, baseURL, "/v1/responses", secondBody, http.StatusOK)
	if !strings.Contains(second.Header.Get("x-spillway-route"), "candidate 2/2") || !strings.Contains(second.Body, "Groq spillover complete.") {
		t.Fatalf("spillover response = %+v, want candidate 2/2 and Groq answer", second)
	}
	assertNoThoughtSignatureLeak(t, first+second.Body, output.String())
	if geminiRequests.Load() != 2 || groqRequests.Load() != 1 {
		t.Fatalf("upstream requests Gemini=%d Groq=%d, want 2 and 1", geminiRequests.Load(), groqRequests.Load())
	}
}

func TestBinary_SpilloverGroqHistoryToGeminiAddsPlaceholder(t *testing.T) {
	var groqRequests, geminiRequests atomic.Int32
	groq := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertThoughtSignatureProviderAuth(t, r, thoughtSignatureGroqKey)
		if groqRequests.Add(1) == 1 {
			writeUnsignedThoughtSignatureChatResponse(w, "function-call-groq")
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"quota reached"}}`)
	}))
	t.Cleanup(groq.Close)
	gemini := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertThoughtSignatureProviderAuth(t, r, thoughtSignatureGeminiKey)
		geminiRequests.Add(1)
		var body struct {
			Messages []struct {
				Role      string `json:"role"`
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
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode Gemini request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, message := range body.Messages {
			if message.Role == "assistant" && len(message.ToolCalls) > 0 {
				if message.ToolCalls[0].ID != "function-call-groq" || message.ToolCalls[0].ExtraContent.Google.ThoughtSignature != "skip_thought_signature_validator" {
					t.Errorf("Gemini tool call = %+v, want placeholder signature", message.ToolCalls[0])
				}
			}
		}
		writeThoughtSignatureChatJSON(w, "Gemini placeholder complete.")
	}))
	t.Cleanup(gemini.Close)
	baseURL, output := startThoughtSignatureBinary(t, gemini.URL+"/v1beta/openai", groq.URL+"/v1", "groq/test-model, gemini/gemini-3-flash-preview")
	first := requestBinaryAPI(t, baseURL, http.MethodPost, "/v1/responses", `{"model":"spillway/demo","input":"Use a tool."}`, thoughtSignatureLocalKey, http.StatusOK, "function-call-groq")
	callID := responsesOutputCallID(t, first)
	secondBody := fmt.Sprintf(`{"model":"spillway/demo","input":[{"type":"message","role":"user","content":"Use a tool."},{"type":"function_call","call_id":%q,"name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":%q,"output":"done"}]}`, callID, callID)
	second := requestBinaryAPIDetails(t, baseURL, "/v1/responses", secondBody, http.StatusOK)
	if !strings.Contains(second.Header.Get("x-spillway-route"), "candidate 2/2") || !strings.Contains(second.Body, "Gemini placeholder complete.") {
		t.Fatalf("spillover response = %+v, want candidate 2/2 and Gemini answer", second)
	}
	assertNoThoughtSignatureLeak(t, first+second.Body, output.String())
	if groqRequests.Load() != 2 || geminiRequests.Load() != 1 {
		t.Fatalf("upstream requests Groq=%d Gemini=%d, want 2 and 1", groqRequests.Load(), geminiRequests.Load())
	}
}

func TestBinary_ChatCompletionsForwardsThoughtSignatureVerbatim(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertThoughtSignatureProviderAuth(t, r, thoughtSignatureGeminiKey)
		var body struct {
			Messages []struct {
				ToolCalls []struct {
					ExtraContent struct {
						Google struct {
							ThoughtSignature string `json:"thought_signature"`
						} `json:"google"`
					} `json:"extra_content"`
				} `json:"tool_calls"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 1 || len(body.Messages[0].ToolCalls) != 1 || body.Messages[0].ToolCalls[0].ExtraContent.Google.ThoughtSignature != thoughtSignatureValue {
			t.Errorf("chat request lost signature: %+v err=%v", body, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		writeThoughtSignatureChatResponse(w, "function-call-chat")
	}))
	t.Cleanup(upstream.Close)
	baseURL, output := startThoughtSignatureBinary(t, upstream.URL+"/v1beta/openai", upstream.URL+"/v1", "gemini/gemini-3-flash-preview, groq/test-model")
	body := `{"model":"gemini/gemini-3-flash-preview","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"function-call-chat","type":"function","function":{"name":"lookup","arguments":"{}"},"extra_content":{"google":{"thought_signature":"` + thoughtSignatureValue + `"}}}]}]}`
	response := requestBinaryAPI(t, baseURL, http.MethodPost, "/v1/chat/completions", body, thoughtSignatureLocalKey, http.StatusOK, thoughtSignatureValue)
	assertNoThoughtSignatureLeak(t, "", output.String())
	if !strings.Contains(response, thoughtSignatureValue) {
		t.Fatalf("chat response lost signature: %s", response)
	}
}

func startThoughtSignatureBinary(t *testing.T, geminiURL, groqURL, tierModels string) (string, *synchronizedBuffer) {
	t.Helper()
	t.Setenv("SPILLWAY_E2E_LOCAL_KEY", thoughtSignatureLocalKey)
	t.Setenv("SPILLWAY_E2E_GEMINI_KEY", thoughtSignatureGeminiKey)
	t.Setenv("SPILLWAY_E2E_GROQ_KEY", thoughtSignatureGroqKey)
	return startConfiguredBinary(t, func(addr string) string {
		return fmt.Sprintf(`server:
  listen: %q
  api_keys:
    - name: default
      key: ${SPILLWAY_E2E_LOCAL_KEY}
providers:
  gemini:
    api_key: ${SPILLWAY_E2E_GEMINI_KEY}
    base_url: %q
  groq:
    api_key: ${SPILLWAY_E2E_GROQ_KEY}
    base_url: %q
tiers:
  demo:
    models: [%s]
    allow_spend: true
models:
  gemini/gemini-3-flash-preview: {capabilities: [tools]}
  groq/test-model: {capabilities: [tools]}
router:
  auto: [demo]
`, addr, geminiURL, groqURL, tierModels)
	})
}

// startConfiguredBinary serves the config that config returns for a free loopback address and stops it at cleanup.
func startConfiguredBinary(t *testing.T, config func(addr string) string) (string, *synchronizedBuffer) {
	t.Helper()
	workDir := t.TempDir()
	addr := freeLoopbackAddr(t)
	configPath := filepath.Join(workDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(config(addr)), 0o600); err != nil { // #nosec G703 -- configPath is inside t.TempDir.
		t.Fatal("write binary config:", err)
	}
	command := exec.CommandContext(t.Context(), spillwayBinary, "serve", "-config", configPath) //nolint:gosec // TestMain built this executable and configPath is inside t.TempDir.
	command.Dir = workDir
	output := &synchronizedBuffer{}
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		t.Fatal("start binary:", err)
	}
	wait := make(chan error, 1)
	waitDone := make(chan struct{})
	go func() {
		wait <- command.Wait()
		close(waitDone)
	}()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		<-waitDone
	})
	baseURL := "http://" + addr
	waitForThoughtSignatureBinary(t, baseURL, wait, output)
	return baseURL, output
}

func waitForThoughtSignatureBinary(t *testing.T, baseURL string, wait <-chan error, output *synchronizedBuffer) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	client := &http.Client{Timeout: time.Second}
	for {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case err := <-wait:
			t.Fatalf("spillway exited before listening: %v\n%s", err, output.String())
		case <-deadline.C:
			t.Fatalf("spillway did not listen within 10s\n%s", output.String())
		case <-ticker.C:
		}
	}
}

func assertThoughtSignatureProviderAuth(t *testing.T, r *http.Request, providerKey string) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer "+providerKey || strings.Contains(got, thoughtSignatureLocalKey) {
		t.Errorf("upstream Authorization = %q, want only configured provider key", got)
	}
}

func assertRealThoughtSignature(t *testing.T, raw json.RawMessage, callID string) {
	t.Helper()
	var body struct {
		Messages []struct {
			Role      string `json:"role"`
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
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode replay request: %v", err)
	}
	for _, message := range body.Messages {
		if message.Role == "assistant" && len(message.ToolCalls) > 0 {
			call := message.ToolCalls[0]
			if call.ID != callID || call.ExtraContent.Google.ThoughtSignature != thoughtSignatureValue {
				t.Errorf("replayed tool call = %+v, want original ID and real signature", call)
			}
			return
		}
	}
	t.Error("replay contains no assistant tool call")
}

func writeThoughtSignatureToolStream(w http.ResponseWriter, callID string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-signature\",\"object\":\"chat.completion.chunk\",\"created\":1760000000,\"model\":\"gemini-3-flash-preview\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":%q,\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\n", callID)
	_, _ = io.WriteString(w, `data: {"id":"chatcmpl-signature","object":"chat.completion.chunk","created":1760000000,"model":"gemini-3-flash-preview","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"extra_content":{"google":{"thought_signature":"`+thoughtSignatureValue+`"}}}]},"finish_reason":null}]}`+"\n\n")
	_, _ = io.WriteString(w, `data: {"id":"chatcmpl-signature","object":"chat.completion.chunk","created":1760000000,"model":"gemini-3-flash-preview","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func writeThoughtSignatureTextStream(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-signature\",\"object\":\"chat.completion.chunk\",\"created\":1760000000,\"model\":\"gemini-3-flash-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", text)
	_, _ = io.WriteString(w, `data: {"id":"chatcmpl-signature","object":"chat.completion.chunk","created":1760000000,"model":"gemini-3-flash-preview","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func writeThoughtSignatureChatJSON(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-signature","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`, text)
}

func writeThoughtSignatureChatResponse(w http.ResponseWriter, callID string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-signature","object":"chat.completion","created":1760000000,"model":"gemini-3-flash-preview","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":%q,"type":"function","function":{"name":"lookup","arguments":"{}"},"extra_content":{"google":{"thought_signature":"`+thoughtSignatureValue+`"}}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`, callID)
}

func writeUnsignedThoughtSignatureChatResponse(w http.ResponseWriter, callID string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-signature","object":"chat.completion","created":1760000000,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":%q,"type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`, callID)
}

func responsesStreamCallID(t *testing.T, stream string) string {
	t.Helper()
	for _, frame := range strings.Split(stream, "\n\n") {
		if !strings.Contains(frame, `"type":"response.completed"`) {
			continue
		}
		var event struct {
			Response struct {
				Output []struct {
					Type   string `json:"type"`
					CallID string `json:"call_id"`
				} `json:"output"`
			} `json:"response"`
		}
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data: ") {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal("decode Responses completed event:", err)
				}
			}
		}
		for _, item := range event.Response.Output {
			if item.Type == "function_call" {
				return item.CallID
			}
		}
	}
	t.Fatal("Responses stream did not include a function call")
	return ""
}

func anthropicStreamToolUseID(t *testing.T, stream string) string {
	t.Helper()
	for _, frame := range strings.Split(stream, "\n\n") {
		if !strings.Contains(frame, "event: content_block_start\n") {
			continue
		}
		var event struct {
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"content_block"`
		}
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data: ") {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal("decode Messages block event:", err)
				}
			}
		}
		if event.ContentBlock.Type == "tool_use" {
			return event.ContentBlock.ID
		}
	}
	t.Fatal("Messages stream did not include a tool_use")
	return ""
}

type binaryRequestResult struct {
	Status int
	Header http.Header
	Body   string
}

func requestBinaryAPIDetails(t *testing.T, baseURL, path, body string, wantStatus int) binaryRequestResult {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal("create binary API request:", err)
	}
	request.Header.Set("Authorization", "Bearer "+thoughtSignatureLocalKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatal("send binary API request:", err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal("read binary API response:", err)
	}
	result := binaryRequestResult{Status: response.StatusCode, Header: response.Header, Body: string(data)}
	if result.Status != wantStatus {
		t.Fatalf("binary API response = %d %s, want %d", result.Status, result.Body, wantStatus)
	}
	return result
}

func responsesOutputCallID(t *testing.T, response string) string {
	t.Helper()
	var result struct {
		Output []struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		} `json:"output"`
	}
	if err := json.Unmarshal([]byte(response), &result); err != nil {
		t.Fatal("decode Responses output:", err)
	}
	for _, item := range result.Output {
		if item.Type == "function_call" {
			return item.CallID
		}
	}
	t.Fatal("Responses output did not include a function call")
	return ""
}

func assertNoThoughtSignatureLeak(t *testing.T, responses, logs string) {
	t.Helper()
	for _, source := range []struct{ name, value string }{{"response", responses}, {"server logs", logs}} {
		if strings.Contains(source.value, thoughtSignatureValue) || strings.Contains(source.value, `"thought_signature"`) {
			t.Errorf("thought signature leaked to %s: %s", source.name, source.value)
		}
	}
}
