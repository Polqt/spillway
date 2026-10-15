// Package router turns a virtual model and a request body into an ordered
// list of eligible candidates, with a reason for every skip. It is pure: no
// I/O and no clock.
package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// Capability is something a request can need and a model can declare (ADR 0007).
type Capability string

// The closed set of capabilities, in the order reasons list them.
const (
	Tools             Capability = "tools"
	Vision            Capability = "vision"
	JSONMode          Capability = "json_mode"
	StructuredOutputs Capability = "structured_outputs"
)

var capabilityOrder = []Capability{Tools, Vision, JSONMode, StructuredOutputs}

// ErrInvalidField means a request field Spillway inspects has the wrong JSON type.
var ErrInvalidField = errors.New("invalid request field")

type fieldError struct {
	field, want string
}

func (e fieldError) Error() string { return fmt.Sprintf("%q must be %s", e.field, e.want) }

func (e fieldError) Unwrap() error { return ErrInvalidField }

// Requirements is what one request needs from a model.
type Requirements struct {
	// Needs lists the required capabilities in capabilityOrder.
	Needs []Capability
	// EstimatedTokens undercounts on purpose: text bytes / 6, rounded up,
	// plus the requested output tokens. Images are not counted.
	EstimatedTokens       int
	EstimatedInputTokens  int
	RequestedOutputTokens int
	ToolsPresent          bool
}

// bytesPerToken is above the 4 to 5 bytes per token of English text, so the
// estimate errs toward missing an oversized request rather than wrongly
// skipping a candidate.
const bytesPerToken = 6

type detectBody struct {
	Messages            []detectMessage `json:"messages"`
	Tools               json.RawMessage `json:"tools"`
	Functions           json.RawMessage `json:"functions"`
	ResponseFormat      json.RawMessage `json:"response_format"`
	MaxTokens           json.RawMessage `json:"max_tokens"`
	MaxCompletionTokens json.RawMessage `json:"max_completion_tokens"`
}

type detectMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	ToolCalls    json.RawMessage `json:"tool_calls"`
	FunctionCall json.RawMessage `json:"function_call"`
}

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Detect reads a chat request body and reports what it needs. The body is
// assumed to be one JSON object with a messages array, as the API checks
// before routing.
func Detect(body []byte) (Requirements, error) {
	var b detectBody
	if err := json.Unmarshal(body, &b); err != nil {
		return Requirements{}, fieldError{field: "messages", want: "an array of message objects"}
	}
	needs := make(map[Capability]bool)
	textBytes := 0

	toolsPresent := false
	for _, field := range []struct {
		name string
		raw  json.RawMessage
	}{{"tools", b.Tools}, {"functions", b.Functions}} {
		n, err := arrayLen(field.raw)
		if err != nil {
			return Requirements{}, fieldError{field: field.name, want: "an array"}
		}
		if n > 0 {
			needs[Tools] = true
			toolsPresent = true
			textBytes += len(field.raw)
		}
	}

	if format, err := responseFormat(b.ResponseFormat); err != nil {
		return Requirements{}, err
	} else if format == "json_object" {
		needs[JSONMode] = true
	} else if format == "json_schema" {
		needs[StructuredOutputs] = true
	}

	for i, m := range b.Messages {
		if m.Role == "tool" || m.Role == "function" || !isNull(m.FunctionCall) {
			needs[Tools] = true
			toolsPresent = true
		}
		if n, err := arrayLen(m.ToolCalls); err == nil && n > 0 {
			needs[Tools] = true
			toolsPresent = true
			textBytes += len(m.ToolCalls)
		}
		n, vision, err := contentText(m.Content)
		if err != nil {
			return Requirements{}, fieldError{field: fmt.Sprintf("messages[%d].content", i), want: "a string, null or an array of content parts"}
		}
		textBytes += n
		if vision {
			needs[Vision] = true
		}
	}

	output, err := outputTokens(b)
	if err != nil {
		return Requirements{}, err
	}
	input := (textBytes + bytesPerToken - 1) / bytesPerToken
	r := Requirements{EstimatedTokens: input + output, EstimatedInputTokens: input, RequestedOutputTokens: output, ToolsPresent: toolsPresent}
	for _, c := range capabilityOrder {
		if needs[c] {
			r.Needs = append(r.Needs, c)
		}
	}
	return r, nil
}

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(raw, []byte("null"))
}

// arrayLen returns the length of a JSON array; absent and null are empty.
func arrayLen(raw json.RawMessage) (int, error) {
	if isNull(raw) {
		return 0, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return 0, err
	}
	return len(items), nil
}

func responseFormat(raw json.RawMessage) (string, error) {
	if isNull(raw) {
		return "", nil
	}
	var format struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &format); err != nil {
		return "", fieldError{field: "response_format", want: `an object with a string "type"`}
	}
	return format.Type, nil
}

// contentText returns the text bytes of one message's content and whether it
// holds an image part.
func contentText(raw json.RawMessage) (int, bool, error) {
	if isNull(raw) {
		return 0, false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return len(s), false, nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return 0, false, err
	}
	n, vision := 0, false
	for _, p := range parts {
		switch p.Type {
		case "text":
			n += len(p.Text)
		case "image_url":
			vision = true
		}
	}
	return n, vision, nil
}

// outputTokens returns the requested output size, max_completion_tokens
// first, saturated so the sum cannot overflow.
func outputTokens(b detectBody) (int, error) {
	completion, err := tokenCount("max_completion_tokens", b.MaxCompletionTokens)
	if err != nil {
		return 0, err
	}
	legacy, err := tokenCount("max_tokens", b.MaxTokens)
	if err != nil {
		return 0, err
	}
	if completion > 0 {
		return completion, nil
	}
	return legacy, nil
}

func tokenCount(field string, raw json.RawMessage) (int, error) {
	if isNull(raw) {
		return 0, nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fieldError{field: field, want: "a whole number"}
	}
	if n <= 0 {
		return 0, nil
	}
	return int(min(n, math.MaxInt32)), nil
}
