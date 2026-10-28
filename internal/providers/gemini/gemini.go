// Package gemini adapts Gemini's OpenAI-compatible endpoint.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
		ClassifyError: classifyError, KeepToolCallExtraContent: true,
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
	req, err := withPlaceholderSignatures(req)
	if err != nil {
		return providers.ChatResponse{}, err
	}
	return c.compat.Chat(ctx, req)
}

// ChatStream starts a streaming chat completion.
func (c *Client) ChatStream(ctx context.Context, req providers.ChatRequest) (providers.Stream, error) {
	req, err := withPlaceholderSignatures(req)
	if err != nil {
		return nil, err
	}
	return c.compat.ChatStream(ctx, req)
}

// placeholderSignature is Google's documented stand-in for tool calls Gemini did not generate, such as history from another provider.
const placeholderSignature = `"skip_thought_signature_validator"`

// withPlaceholderSignatures fills the placeholder on the first call of each unsigned assistant tool-call message,
// because Gemini rejects a current-turn step whose first call has no signature.
func withPlaceholderSignatures(req providers.ChatRequest) (providers.ChatRequest, error) {
	if len(req.RawBody) == 0 {
		return req, nil
	}
	body, err := openaicompat.EditMessages(req.RawBody, messageWithPlaceholderSignature)
	if err != nil {
		return req, fmt.Errorf("%w: gemini request: %w", providers.ErrBadRequest, err)
	}
	req.RawBody = body
	return req, nil
}

func messageWithPlaceholderSignature(index int, raw json.RawMessage) (json.RawMessage, bool, error) {
	var message struct {
		Role      string            `json:"role"`
		ToolCalls []json.RawMessage `json:"tool_calls"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		return nil, false, fmt.Errorf("message %d is malformed", index)
	}
	if message.Role != "assistant" || len(message.ToolCalls) == 0 {
		return raw, false, nil
	}
	for _, call := range message.ToolCalls {
		var signed struct {
			ExtraContent struct {
				Google struct {
					ThoughtSignature string `json:"thought_signature"`
				} `json:"google"`
			} `json:"extra_content"`
		}
		if err := json.Unmarshal(call, &signed); err != nil {
			return nil, false, fmt.Errorf("message %d has a malformed tool call", index)
		}
		if signed.ExtraContent.Google.ThoughtSignature != "" {
			return raw, false, nil
		}
	}
	first, err := withMember(message.ToolCalls[0], json.RawMessage(placeholderSignature), "extra_content", "google", "thought_signature")
	if err != nil {
		return nil, false, fmt.Errorf("message %d has a malformed tool call", index)
	}
	message.ToolCalls[0] = first
	calls, err := encodeUnescaped(message.ToolCalls)
	if err != nil {
		return nil, false, fmt.Errorf("encode message %d tool calls: %w", index, err)
	}
	edited, err := withMember(raw, calls, "tool_calls")
	if err != nil {
		return nil, false, fmt.Errorf("message %d must be a JSON object", index)
	}
	return edited, true, nil
}

// withMember sets the member at path inside a JSON object, creating missing objects and keeping sibling members.
func withMember(object, value json.RawMessage, path ...string) (json.RawMessage, error) {
	if len(path) == 0 {
		return value, nil
	}
	var members map[string]json.RawMessage
	if len(object) > 0 {
		if err := json.Unmarshal(object, &members); err != nil {
			return nil, err
		}
	}
	if members == nil {
		members = map[string]json.RawMessage{}
	}
	child, err := withMember(members[path[0]], value, path[1:]...)
	if err != nil {
		return nil, err
	}
	members[path[0]] = child
	return encodeUnescaped(members)
}

// encodeUnescaped marshals without HTML escaping so raw member values keep their bytes.
func encodeUnescaped(value any) (json.RawMessage, error) {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
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
