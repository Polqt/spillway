package providers

import (
	"encoding/json"
	"strings"
)

// ChatRequest is an OpenAI chat completion request.
type ChatRequest struct {
	// Model is the upstream model, with the provider prefix already stripped.
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
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
	Body json.RawMessage
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
		return ChatResponse{}, err
	}
	return ChatResponse{Body: body}, nil
}
