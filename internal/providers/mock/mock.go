// Package mock is a deterministic, offline provider for tests and demos.
// It does no network or file I/O.
package mock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Polqt/spillway/internal/providers"
)

// Options configures the mock provider.
type Options struct {
	// Now supplies the created timestamp of every response.
	Now func() time.Time
}

// Provider is the mock provider. Its upstream model "echo" replies with the
// text of the last user message.
type Provider struct {
	now func() time.Time
}

// New returns the mock provider.
func New(o Options) (*Provider, error) {
	if o.Now == nil {
		return nil, errors.New("mock: Options.Now is required")
	}
	return &Provider{now: o.Now}, nil
}

// Name returns "mock".
func (p *Provider) Name() string {
	return "mock"
}

// Chat answers for the upstream model "echo"; any other model is
// providers.ErrModelNotFound.
func (p *Provider) Chat(ctx context.Context, req providers.ChatRequest) (providers.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return providers.ChatResponse{}, err
	}
	if req.Model != "echo" {
		return providers.ChatResponse{}, fmt.Errorf("mock: upstream model %q: %w", req.Model, providers.ErrModelNotFound)
	}
	id, err := requestID(req)
	if err != nil {
		return providers.ChatResponse{}, err
	}
	content := "You said: " + lastUserText(req.Messages)
	prompt := 0
	for _, m := range req.Messages {
		prompt += len(strings.Fields(m.Text()))
	}
	completion := len(strings.Fields(content))
	return providers.NewChatResponse(providers.Completion{
		ID:           id,
		Model:        req.Model,
		Content:      content,
		FinishReason: "stop",
		Created:      p.now().Unix(),
		Usage:        providers.Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion},
	})
}

// requestID derives the response id from the request, so identical requests
// get identical responses.
func requestID(req providers.ChatRequest) (string, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("mock: encode request: %w", err)
	}
	sum := sha256.Sum256(b)
	return "chatcmpl-mock-" + hex.EncodeToString(sum[:])[:12], nil
}

func lastUserText(msgs []providers.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Text()
		}
	}
	return ""
}
