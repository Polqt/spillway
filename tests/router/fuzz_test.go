package router_test

import (
	"testing"

	"github.com/Polqt/spillway/internal/router"
)

func FuzzDetect(f *testing.F) {
	f.Add([]byte(`{"messages":[{"role":"user","content":"hello"}]}`))
	f.Add([]byte(`{"messages":[{"role":"user","content":"hello"}],"max_tokens":100}`))
	f.Add([]byte("not JSON"))
	f.Fuzz(func(t *testing.T, body []byte) {
		requirements, err := router.Detect(body)
		if err != nil {
			return
		}
		// Output tokens can exceed the body length; the input estimate cannot.
		if requirements.EstimatedInputTokens < 0 || requirements.EstimatedInputTokens > len(body) {
			t.Fatalf("estimated input tokens = %d for body length %d", requirements.EstimatedInputTokens, len(body))
		}
		if requirements.EstimatedTokens < requirements.EstimatedInputTokens {
			t.Fatalf("estimated tokens = %d, below estimated input tokens %d", requirements.EstimatedTokens, requirements.EstimatedInputTokens)
		}
	})
}
