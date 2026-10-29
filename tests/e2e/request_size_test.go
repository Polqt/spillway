package e2e_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	sizeGeminiKey      = "sk-test-size-gemini"
	sizeGroqKey        = "sk-test-size-groq"
	sizeProviderMarker = "org_test_marker"
	sizePromptMarker   = "PROMPT-MARKER-7f3a"
	groqTPM413Body     = `{"error":{"message":"Request too large for model openai/gpt-oss-120b in organization org_test_marker service tier on_demand on tokens per minute (TPM): Limit 8000, Requested 35112, please reduce your message size and try again.","type":"tokens","code":"rate_limit_exceeded"}}`
)

type sizeUpstream struct {
	server *httptest.Server
	calls  atomic.Int32
}

// newSizeUpstream answers every request with reply after checking that only key reached it.
func newSizeUpstream(t *testing.T, key string, reply func(w http.ResponseWriter)) *sizeUpstream {
	t.Helper()
	upstream := &sizeUpstream{}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.calls.Add(1)
		want := ""
		if key != "" {
			want = "Bearer " + key
		}
		if got := r.Header.Get("Authorization"); got != want {
			t.Errorf("upstream Authorization = %q, want only its own provider key", got)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		reply(w)
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func sizeStatus(status int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func ollamaStreamReply(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, chunk := range []string{
		`{"id":"chatcmpl-ollama","object":"chat.completion.chunk","created":1760000000,"model":"llama3.2:3b","choices":[{"index":0,"delta":{"content":"Local "},"finish_reason":null}]}`,
		`{"id":"chatcmpl-ollama","object":"chat.completion.chunk","created":1760000000,"model":"llama3.2:3b","choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}]}`,
	} {
		_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func startSizeBinary(t *testing.T, gemini, groq, ollama *sizeUpstream) (string, *synchronizedBuffer) {
	t.Helper()
	t.Setenv("SPILLWAY_E2E_LOCAL_KEY", thoughtSignatureLocalKey)
	t.Setenv("SPILLWAY_E2E_SIZE_GEMINI_KEY", sizeGeminiKey)
	t.Setenv("SPILLWAY_E2E_SIZE_GROQ_KEY", sizeGroqKey)
	return startConfiguredBinary(t, func(addr string) string {
		return fmt.Sprintf(`server:
  listen: %q
  api_keys:
    - name: default
      key: ${SPILLWAY_E2E_LOCAL_KEY}
providers:
  gemini:
    api_key: ${SPILLWAY_E2E_SIZE_GEMINI_KEY}
    base_url: %q
    free_tier: true
  groq:
    api_key: ${SPILLWAY_E2E_SIZE_GROQ_KEY}
    base_url: %q
    free_tier: true
  ollama:
    base_url: %q
tiers:
  free:
    models: [gemini/gemini-3.5-flash, groq/openai/gpt-oss-120b, ollama/llama3.2:3b]
router:
  auto: [free]
`, addr, gemini.server.URL+"/v1beta/openai", groq.server.URL+"/v1", ollama.server.URL+"/v1")
	})
}

func assertNoSizeBinaryLeak(t *testing.T, label, text string) {
	t.Helper()
	for _, secret := range []string{sizeGeminiKey, sizeGroqKey, thoughtSignatureLocalKey, sizeProviderMarker, sizePromptMarker} {
		if strings.Contains(text, secret) {
			t.Errorf("%s contains %q", label, secret)
		}
	}
}

// Replays the 2026-10-29 OpenCode run: Gemini 503, Groq's free-tier TPM 413, then Ollama serves (ADR 0034).
func TestBinary_RequestTooLargeSpillsLikeLiveOpenCodeRun(t *testing.T) {
	gemini := newSizeUpstream(t, sizeGeminiKey, sizeStatus(http.StatusServiceUnavailable, `{"error":{"message":"overloaded"}}`))
	groq := newSizeUpstream(t, sizeGroqKey, sizeStatus(http.StatusRequestEntityTooLarge, groqTPM413Body))
	ollama := newSizeUpstream(t, "", ollamaStreamReply)
	baseURL, output := startSizeBinary(t, gemini, groq, ollama)
	body := fmt.Sprintf(`{"model":"spillway/free","stream":true,"messages":[{"role":"user","content":"%s %s"}]}`, sizePromptMarker, strings.Repeat("context ", 5000))
	for request := 1; request <= 2; request++ {
		reply := requestBinaryAPIDetails(t, baseURL, "/v1/chat/completions", body, http.StatusOK)
		if reply.Header.Get("X-Spillway-Route") != "tier free; candidate 3/3" || !strings.Contains(reply.Body, "Local ") || !strings.Contains(reply.Body, "data: [DONE]") {
			t.Fatalf("request %d = %v %s, want Ollama's stream as candidate 3/3", request, reply.Header, reply.Body)
		}
		assertNoSizeBinaryLeak(t, "response", reply.Body)
	}
	if gemini.calls.Load() != 2 || groq.calls.Load() != 2 || ollama.calls.Load() != 2 {
		t.Fatalf("upstream calls Gemini=%d Groq=%d Ollama=%d, want 2 each because a size rejection does not cool Groq down", gemini.calls.Load(), groq.calls.Load(), ollama.calls.Load())
	}
	logs := output.String()
	if !strings.Contains(logs, "error_kind=upstream_unavailable") || !strings.Contains(logs, "error_kind=request_too_large") {
		t.Fatalf("logs lack the per-attempt reasons:\n%s", logs)
	}
	assertNoSizeBinaryLeak(t, "logs", logs)
}

func TestBinary_RequestTooLargeEverywhereIsContextLengthExceeded(t *testing.T) {
	gemini := newSizeUpstream(t, sizeGeminiKey, sizeStatus(http.StatusRequestEntityTooLarge, `{"error":{"message":"too large org_test_marker"}}`))
	groq := newSizeUpstream(t, sizeGroqKey, sizeStatus(http.StatusRequestEntityTooLarge, groqTPM413Body))
	ollama := newSizeUpstream(t, "", sizeStatus(http.StatusBadRequest, `{"error":{"message":"too long org_test_marker","type":"invalid_request_error","code":"context_length_exceeded"}}`))
	baseURL, output := startSizeBinary(t, gemini, groq, ollama)
	tests := []struct{ path, body, want string }{
		{"/v1/chat/completions", `{"model":"spillway/free","messages":[{"role":"user","content":"` + sizePromptMarker + `"}]}`, `"code":"context_length_exceeded"`},
		{"/v1/responses", `{"model":"spillway/free","input":"` + sizePromptMarker + `"}`, `"code":"context_length_exceeded"`},
		{"/v1/messages", `{"model":"spillway/free","max_tokens":16,"messages":[{"role":"user","content":"` + sizePromptMarker + `"}]}`, `"type":"invalid_request_error"`},
	}
	for _, tt := range tests {
		reply := requestBinaryAPIDetails(t, baseURL, tt.path, tt.body, http.StatusBadRequest)
		if !strings.Contains(reply.Body, tt.want) || !strings.Contains(reply.Body, "request too large") {
			t.Errorf("%s = %s, want %s with per-candidate reasons", tt.path, reply.Body, tt.want)
		}
		assertNoSizeBinaryLeak(t, tt.path, reply.Body)
	}
	assertNoSizeBinaryLeak(t, "logs", output.String())
}

func TestBinary_DirectRequestTooLargeMakesOneCall(t *testing.T) {
	gemini := newSizeUpstream(t, sizeGeminiKey, sizeStatus(http.StatusOK, `{}`))
	groq := newSizeUpstream(t, sizeGroqKey, sizeStatus(http.StatusRequestEntityTooLarge, groqTPM413Body))
	ollama := newSizeUpstream(t, "", ollamaStreamReply)
	baseURL, _ := startSizeBinary(t, gemini, groq, ollama)
	reply := requestBinaryAPIDetails(t, baseURL, "/v1/chat/completions", `{"model":"groq/openai/gpt-oss-120b","messages":[{"role":"user","content":"hi"}]}`, http.StatusBadRequest)
	if !strings.Contains(reply.Body, `"code":"context_length_exceeded"`) || reply.Header.Get("X-Spillway-Route") != "direct" {
		t.Fatalf("direct reply = %v %s, want 400 context_length_exceeded on route direct", reply.Header, reply.Body)
	}
	if groq.calls.Load() != 1 || gemini.calls.Load() != 0 || ollama.calls.Load() != 0 {
		t.Fatalf("upstream calls Gemini=%d Groq=%d Ollama=%d, want only one Groq call", gemini.calls.Load(), groq.calls.Load(), ollama.calls.Load())
	}
	assertNoSizeBinaryLeak(t, "direct reply", reply.Body)
}
