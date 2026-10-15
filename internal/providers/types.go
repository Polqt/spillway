package providers

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ChatRequest is an OpenAI chat completion request.
type ChatRequest struct {
	// Model is the upstream model, with the provider prefix already stripped.
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	// RawBody preserves request fields the gateway does not interpret.
	RawBody json.RawMessage `json:"-"`
}

// Message is one chat message.
type Message struct {
	Role string `json:"role"`
	// Content is a string or an array of content parts, kept verbatim.
	Content json.RawMessage `json:"content,omitempty"`
}

// Text returns the message's text: the string content, or the text parts
// joined by spaces. Non-text parts and null content contribute nothing.
func (m Message) Text() string {
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.Type == "text" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, " ")
}

// ChatResponse carries a complete OpenAI chat.completion object verbatim.
type ChatResponse struct {
	// Body is the complete OpenAI-format response.
	Body json.RawMessage
	// RateLimit contains quota hints from the successful response, when present.
	RateLimit RateLimit
}

// RateLimit carries provider quota hints for a successful response.
type RateLimit struct {
	// RemainingRequests is the reported number of remaining requests.
	RemainingRequests int
	// RemainingTokens is the reported number of remaining tokens.
	RemainingTokens int
	// ResetRequests is the duration until the request quota resets.
	ResetRequests time.Duration
	// ResetTokens is the duration until the token quota resets.
	ResetTokens time.Duration
	// Known reports whether the provider supplied quota hints.
	Known bool
}

// Usage is the token accounting of one completion.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Completion is the input for NewChatResponse, for providers that synthesise
// responses instead of relaying them.
type Completion struct {
	ID, Model, Content, FinishReason string
	Created                          int64
	Usage                            Usage
}

type chatCompletion struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   Usage        `json:"usage"`
}

type chatChoice struct {
	Index        int             `json:"index"`
	Message      assistantReply  `json:"message"`
	Logprobs     json.RawMessage `json:"logprobs"`
	FinishReason string          `json:"finish_reason"`
}

type assistantReply struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// NewChatResponse builds an OpenAI chat.completion object with one choice.
func NewChatResponse(c Completion) (ChatResponse, error) {
	body, err := json.Marshal(chatCompletion{
		ID:      c.ID,
		Object:  "chat.completion",
		Created: c.Created,
		Model:   c.Model,
		Choices: []chatChoice{{
			Message:      assistantReply{Role: "assistant", Content: c.Content},
			Logprobs:     json.RawMessage("null"),
			FinishReason: c.FinishReason,
		}},
		Usage: c.Usage,
	})
	if err != nil {
		return ChatResponse{}, fmt.Errorf("encode chat completion: %w", err)
	}
	return ChatResponse{Body: body}, nil
}

// ChatChunk carries one OpenAI chat.completion.chunk object verbatim.
type ChatChunk struct {
	Data json.RawMessage
	// Usage is set when the chunk carries token accounting.
	Usage *Usage
	// ReportedCost is the provider-reported USD amount from usage.cost.
	ReportedCost json.RawMessage
}

// Delta is the input for NewChatChunk: one step of a synthesised stream.
// Role is set on the first chunk only, and FinishReason on the last.
type Delta struct {
	ID, Model, Role, Content, FinishReason string
	Created                                int64
	Usage                                  *Usage
}

type chatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index        int             `json:"index"`
	Delta        chunkDelta      `json:"delta"`
	Logprobs     json.RawMessage `json:"logprobs"`
	FinishReason *string         `json:"finish_reason"`
}

type chunkDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// NewChatChunk builds an OpenAI chat.completion.chunk object with one choice.
func NewChatChunk(d Delta) (ChatChunk, error) {
	var finish *string
	if d.FinishReason != "" {
		finish = &d.FinishReason
	}
	data, err := json.Marshal(chatCompletionChunk{
		ID:      d.ID,
		Object:  "chat.completion.chunk",
		Created: d.Created,
		Model:   d.Model,
		Choices: []chunkChoice{{
			Delta:        chunkDelta{Role: d.Role, Content: d.Content},
			Logprobs:     json.RawMessage("null"),
			FinishReason: finish,
		}},
		Usage: d.Usage,
	})
	if err != nil {
		return ChatChunk{}, fmt.Errorf("encode chat completion chunk: %w", err)
	}
	return ChatChunk{Data: data, Usage: d.Usage}, nil
}
