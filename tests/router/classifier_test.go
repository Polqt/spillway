package router_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Polqt/spillway/internal/router"
)

func TestOllamaClassifier_Classify(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" || r.Method != http.MethodPost {
			t.Errorf("request = %s %s, want POST /api/generate", r.Method, r.URL.Path)
		}
		var request struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
			Stream bool   `json:"stream"`
			Format struct {
				Properties map[string]struct {
					Enum []string `json:"enum"`
				} `json:"properties"`
				AdditionalProperties bool `json:"additionalProperties"`
			} `json:"format"`
			Options struct {
				NumPredict int `json:"num_predict"`
			} `json:"options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Model != "llama3.2:3b" || request.Stream || request.Options.NumPredict != 32 || request.Format.AdditionalProperties {
			t.Errorf("classifier request = %+v, want pinned model, non-streaming, bounded output and closed schema", request)
		}
		if len(request.Prompt) > 4300 || !strings.Contains(request.Prompt, "hello") {
			t.Errorf("prompt length/content unexpected: %d", len(request.Prompt))
		}
		_, _ = fmt.Fprint(w, ollamaEnvelope(`{"difficulty":"hard","reason":"classified"}`))
	}))
	defer server.Close()

	classifier, err := router.NewOllamaClassifier(server.URL+"/v1", "llama3.2:3b")
	if err != nil {
		t.Fatal(err)
	}
	got, err := classifier.Classify(t.Context(), []byte(`{"messages":[{"role":"user","content":"`+strings.Repeat("hello", 2000)+`"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != "hard" {
		t.Fatalf("Classify = %q, want hard", got)
	}
}

func TestOllamaClassifier_RejectsInvalidOutput(t *testing.T) {
	t.Parallel()
	for _, output := range []struct {
		name, envelope string
	}{
		{name: "malformed response", envelope: "not json"},
		{name: "free-form response", envelope: ollamaEnvelope("hard")},
		{name: "unknown label", envelope: ollamaEnvelope(`{"difficulty":"ollama/model","reason":"classified"}`)},
		{name: "extra field", envelope: ollamaEnvelope(`{"difficulty":"hard","reason":"classified","model":"ollama/x"}`)},
		{name: "duplicate difficulty", envelope: ollamaEnvelope(`{"difficulty":"simple","difficulty":"hard","reason":"classified"}`)},
		{name: "trailing output", envelope: ollamaEnvelope(`{"difficulty":"hard","reason":"classified"} extra`)},
		{name: "uncertain", envelope: ollamaEnvelope(`{"difficulty":"moderate","reason":"uncertain"}`)},
		{name: "oversized response", envelope: ollamaEnvelope(strings.Repeat("x", 2048))},
	} {
		t.Run(output.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, output.envelope)
			}))
			defer server.Close()
			classifier, err := router.NewOllamaClassifier(server.URL, "llama3.2:3b")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := classifier.Classify(t.Context(), []byte(`{"messages":[{"role":"user","content":"hello"}]}`)); err == nil {
				t.Fatal("Classify succeeded with invalid output")
			}
		})
	}
}

func ollamaEnvelope(output string) string {
	data, _ := json.Marshal(map[string]string{"response": output})
	return string(data)
}

func TestOllamaClassifier_CancellationAndConnectionFailure(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	stop := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	stopServer := func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
		server.Close()
	}
	t.Cleanup(stopServer)
	classifier, err := router.NewOllamaClassifier(server.URL, "llama3.2:3b")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := classifier.Classify(ctx, []byte("{\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}"))
		result <- err
	}()
	<-entered
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Classify error = %v, want context cancellation", err)
	}
	stopServer()
	offline, err := router.NewOllamaClassifier(server.URL, "llama3.2:3b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := offline.Classify(t.Context(), []byte(`{"messages":[{"role":"user","content":"hello"}]}`)); err == nil {
		t.Fatal("Classify succeeded with a closed local endpoint")
	}
}

func TestOllamaClassifier_DoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalls.Add(1)
		_, _ = fmt.Fprint(w, ollamaEnvelope(`{"difficulty":"simple","reason":"classified"}`))
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	classifier, err := router.NewOllamaClassifier(server.URL, "llama3.2:3b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := classifier.Classify(t.Context(), []byte(`{"messages":[{"role":"user","content":"hello"}]}`)); err == nil {
		t.Fatal("Classify succeeded after receiving a redirect")
	}
	if got := targetCalls.Load(); got != 0 {
		t.Errorf("redirect target calls = %d, want 0", got)
	}
}

func TestNewOllamaClassifier_RequiresPinnedLoopbackModel(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ baseURL, model string }{
		{baseURL: "https://classifier.example", model: "llama3.2:3b"},
		{baseURL: "http://localhost:11434", model: "llama3.2"},
		{baseURL: "http://localhost:11434", model: "llama3.2:latest"},
	} {
		if _, err := router.NewOllamaClassifier(tt.baseURL, tt.model); err == nil {
			t.Errorf("NewOllamaClassifier(%q, %q) succeeded", tt.baseURL, tt.model)
		}
	}
}
