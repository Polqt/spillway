// Package gemini adapts Gemini's OpenAI-compatible endpoint.
package gemini

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/providers/openaicompat"
	"github.com/Polqt/spillway/internal/redact"
)

// Options configures a Gemini provider.
type Options struct {
	Name    string
	APIKey  redact.Secret
	BaseURL string
	Timeout time.Duration
}

// Client implements providers.Provider for Gemini's OpenAI-compatible API.
type Client struct {
	compat *openaicompat.Client
}

// New validates options and builds a Gemini client.
func New(o Options) (*Client, error) {
	if o.BaseURL == "" {
		preset, _ := providers.LookupPreset("gemini")
		o.BaseURL = preset.DefaultBaseURL
	}
	if o.Name == "" {
		o.Name = "gemini"
	}
	compat, err := openaicompat.New(openaicompat.Options{
		Name: o.Name, BaseURL: o.BaseURL, APIKey: o.APIKey, Timeout: o.Timeout,
		ClassifyError: classifyError,
	})
	if err != nil {
		return nil, err
	}
	return &Client{compat: compat}, nil
}

// Name returns the provider name used in model references.
func (c *Client) Name() string { return c.compat.Name() }

// Models lists Gemini models without the API's models/ resource prefix.
func (c *Client) Models(ctx context.Context) ([]providers.Model, error) {
	models, err := c.compat.Models(ctx)
	if err != nil {
		return nil, err
	}
	for i := range models {
		models[i].ID = strings.TrimPrefix(models[i].ID, "models/")
	}
	return models, nil
}

// Chat sends a non-streaming chat completion.
func (c *Client) Chat(ctx context.Context, req providers.ChatRequest) (providers.ChatResponse, error) {
	return c.compat.Chat(ctx, req)
}

// ChatStream starts a streaming chat completion.
func (c *Client) ChatStream(ctx context.Context, req providers.ChatRequest) (providers.Stream, error) {
	return c.compat.ChatStream(ctx, req)
}

func classifyError(_ int, body []byte) (error, bool) {
	var payload []struct {
		Error struct {
			Status  string `json:"status"`
			Details []struct {
				Type       string `json:"@type"`
				RetryDelay string `json:"retryDelay"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload) != 1 || payload[0].Error.Status != "RESOURCE_EXHAUSTED" {
		return nil, false
	}
	var after time.Duration
	for _, detail := range payload[0].Error.Details {
		if detail.Type != "type.googleapis.com/google.rpc.RetryInfo" {
			continue
		}
		parsed, err := time.ParseDuration(detail.RetryDelay)
		if err == nil {
			after = providers.ClampRetryAfter(parsed)
		}
		break
	}
	return providers.RetryAfterError{After: after, Kind: providers.ErrRateLimited}, true
}
