package openaicompat_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/providers/openaicompat"
	"github.com/Polqt/spillway/internal/redact"
)

const fuzzProviderKey = "sk-test-fuzz-provider-key"

func FuzzStreamRecv(f *testing.F) {
	f.Add([]byte("data: {\"id\":\"chunk\"}\n\ndata: [DONE]\n\n"))
	f.Add([]byte("data: {\"error\":{\"message\":\"" + fuzzProviderKey + "\"}}\n\n"))
	f.Add([]byte("data: [DONE]\n\n"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Payload []byte `json:"fuzz_payload"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid fuzz request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(request.Payload)
		_, _ = w.Write([]byte("\n\ndata: [DONE]\n\n"))
	}))
	f.Cleanup(server.Close)
	provider, err := openaicompat.New(openaicompat.Options{
		Name: "groq", BaseURL: server.URL + "/v1", APIKey: redact.NewSecret(fuzzProviderKey), Timeout: time.Second,
	})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		requestBody, err := json.Marshal(struct {
			Model       string `json:"model"`
			Messages    []any  `json:"messages"`
			Stream      bool   `json:"stream"`
			FuzzPayload []byte `json:"fuzz_payload"`
		}{Model: "model", Messages: []any{map[string]string{"role": "user", "content": "test"}}, Stream: true, FuzzPayload: body})
		if err != nil {
			t.Fatal(err)
		}
		stream, err := provider.ChatStream(t.Context(), providers.ChatRequest{
			Model:   "model",
			RawBody: requestBody,
		})
		if err != nil {
			t.Fatalf("ChatStream: %v", err)
		}
		defer func() { _ = stream.Close() }()

		for {
			_, err := stream.Recv()
			if err == nil {
				continue
			}
			if strings.Contains(err.Error(), fuzzProviderKey) {
				t.Fatalf("Recv error exposes provider key: %v", err)
			}
			if errors.Is(err, io.EOF) || !strings.Contains(err.Error(), "upstream stream error") {
				return
			}
		}
	})
}
