package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"unicode/utf8"
)

type anthropicCreateRequest struct {
	Model       string            `json:"model"`
	MaxTokens   int               `json:"max_tokens"`
	Metadata    json.RawMessage   `json:"metadata"`
	Thinking    json.RawMessage   `json:"thinking"`
	Output      json.RawMessage   `json:"output_config"`
	Context     json.RawMessage   `json:"context_management"`
	Cache       json.RawMessage   `json:"cache_control"`
	System      json.RawMessage   `json:"system"`
	Messages    []json.RawMessage `json:"messages"`
	Tools       []json.RawMessage `json:"tools"`
	ToolChoice  json.RawMessage   `json:"tool_choice"`
	Temperature json.RawMessage   `json:"temperature"`
	TopP        json.RawMessage   `json:"top_p"`
	TopK        json.RawMessage   `json:"top_k"`
	Stop        json.RawMessage   `json:"stop_sequences"`
	Stream      bool              `json:"stream"`
}

func anthropicRequest(raw []byte) ([]byte, bool, error) {
	var request anthropicCreateRequest
	if err := decodeClientJSON(raw, &request); err != nil {
		return nil, false, fmt.Errorf("invalid Anthropic Messages request: %w", err)
	}
	if request.Model == "" || request.MaxTokens <= 0 {
		return nil, false, errors.New("model and positive max_tokens are required")
	}
	if err := validateAnthropicHints(request); err != nil {
		return nil, false, err
	}
	messages := make([]map[string]any, 0, len(request.Messages)+1)
	if len(request.System) > 0 {
		content, err := anthropicContent(request.System)
		if err != nil {
			return nil, false, err
		}
		messages = append(messages, map[string]any{"role": "system", "content": content})
	}
	// Chat Completions requires tool results directly after their tool calls, so a system message sent in between waits for them.
	var heldSystem []map[string]any
	for _, rawMessage := range request.Messages {
		var message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := decodeClientJSON(rawMessage, &message); err != nil {
			return nil, false, fmt.Errorf("message is invalid: %w", err)
		}
		if message.Role == "system" {
			// Claude Code sends mid-conversation system messages; Chat Completions keeps them in place.
			content, err := anthropicContent(message.Content)
			if err != nil {
				return nil, false, err
			}
			if content == "" {
				continue
			}
			system := map[string]any{"role": "system", "content": content}
			if len(heldSystem) > 0 || awaitsToolResults(messages) {
				heldSystem = append(heldSystem, system)
			} else {
				messages = append(messages, system)
			}
			continue
		}
		if message.Role != "user" && message.Role != "assistant" {
			return nil, false, fmt.Errorf("message role %q is not supported", message.Role)
		}
		converted, err := anthropicMessages(message.Role, message.Content)
		if err != nil {
			return nil, false, err
		}
		results := 0
		for results < len(converted) && converted[results]["role"] == "tool" {
			results++
		}
		messages = append(messages, converted[:results]...)
		messages = append(messages, heldSystem...)
		messages = append(messages, converted[results:]...)
		heldSystem = nil
	}
	messages = append(messages, heldSystem...)
	if len(messages) == 0 {
		return nil, false, errors.New("messages must be a non-empty array")
	}
	chat := map[string]any{"model": request.Model, "messages": messages, "max_tokens": request.MaxTokens, "stream": request.Stream}
	copyRawField(chat, "temperature", request.Temperature)
	copyRawField(chat, "top_p", request.TopP)
	copyRawField(chat, "top_k", request.TopK)
	copyRawField(chat, "stop", request.Stop)
	if len(request.Tools) > 0 {
		tools, err := anthropicTools(request.Tools)
		if err != nil {
			return nil, false, err
		}
		chat["tools"] = tools
	}
	if len(request.ToolChoice) > 0 {
		choice, err := anthropicToolChoice(request.ToolChoice)
		if err != nil {
			return nil, false, err
		}
		chat["tool_choice"] = choice
	}
	data, err := json.Marshal(chat)
	return data, request.Stream, err
}

func anthropicMessages(role string, raw json.RawMessage) ([]map[string]any, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []map[string]any{{"role": role, "content": text}}, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, errors.New("message content must be text or an array of content blocks")
	}
	content := make([]map[string]any, 0, len(blocks))
	toolCalls := make([]map[string]any, 0)
	toolResults := make([]map[string]any, 0)
	for _, rawBlock := range blocks {
		var block struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			Cache     json.RawMessage `json:"cache_control"`
			Thinking  string          `json:"thinking"`
			Signature string          `json:"signature"`
			Data      string          `json:"data"`
			IsError   bool            `json:"is_error"`
			Source    struct {
				Type      string `json:"type"`
				MediaType string `json:"media_type"`
				Data      string `json:"data"`
				URL       string `json:"url"`
			} `json:"source"`
		}
		if err := decodeClientJSON(rawBlock, &block); err != nil {
			return nil, fmt.Errorf("content block is invalid: %w", err)
		}
		if err := validateAnthropicCacheControl(block.Cache); err != nil {
			return nil, err
		}
		switch block.Type {
		case "text":
			content = append(content, map[string]any{"type": "text", "text": block.Text})
		case "image":
			url := block.Source.URL
			if block.Source.Type == "base64" && block.Source.Data != "" {
				mediaType := block.Source.MediaType
				if mediaType == "" {
					mediaType = "application/octet-stream"
				}
				url = "data:" + mediaType + ";base64," + block.Source.Data
			}
			if url == "" {
				return nil, errors.New("image source must be a URL or base64 data")
			}
			content = append(content, map[string]any{"type": "image_url", "image_url": map[string]string{"url": url}})
		case "tool_use":
			if role != "assistant" {
				return nil, errors.New("tool_use blocks are supported only in assistant messages")
			}
			ref, err := parseClientToolCallID(block.ID)
			if err != nil {
				return nil, errors.New("tool_use.id is invalid")
			}
			toolCalls = append(toolCalls, chatToolCall(ref, block.Name, string(block.Input)))
		case "tool_result":
			if role != "user" {
				return nil, errors.New("tool_result blocks are supported only in user messages")
			}
			if anthropicToolResultHasImage(block.Content) {
				return nil, errors.New("image tool results are not supported")
			}
			ref, err := parseClientToolCallID(block.ToolUseID)
			if err != nil {
				return nil, errors.New("tool_result.tool_use_id is invalid")
			}
			output, err := responseOutputText(block.Content)
			if err != nil {
				return nil, err
			}
			toolResults = append(toolResults, map[string]any{"role": "tool", "tool_call_id": ref.ID, "content": output})
		case "thinking", "redacted_thinking":
			// Client thinking blocks are not forwarded to providers.
		default:
			return nil, fmt.Errorf("content block type %q is not supported", block.Type)
		}
	}
	messages := make([]map[string]any, 0, 1+len(toolResults))
	if role == "user" {
		messages = append(messages, toolResults...)
	}
	if len(content) > 0 || len(toolCalls) > 0 {
		var messageContent any = content
		if role == "assistant" && len(toolCalls) > 0 && len(content) == 0 {
			messageContent = nil
		}
		message := map[string]any{"role": role, "content": messageContent}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}
		messages = append(messages, message)
	}
	if role != "user" {
		messages = append(messages, toolResults...)
	}
	return messages, nil
}

func anthropicToolResultHasImage(raw json.RawMessage) bool {
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return false
	}
	for _, block := range blocks {
		if block.Type == "image" {
			return true
		}
	}
	return false
}

func anthropicContent(raw json.RawMessage) (any, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, errors.New("system must be text or an array of text blocks")
	}
	var result strings.Builder
	first := true
	for _, rawBlock := range blocks {
		var block struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Cache json.RawMessage `json:"cache_control"`
		}
		if err := decodeClientJSON(rawBlock, &block); err != nil || block.Type != "text" {
			return nil, errors.New("system supports text blocks only")
		}
		if err := validateAnthropicCacheControl(block.Cache); err != nil {
			return nil, err
		}
		if !first {
			result.WriteString("\n\n")
		}
		result.WriteString(block.Text)
		first = false
	}
	return result.String(), nil
}

func anthropicTools(raw []json.RawMessage) ([]map[string]any, error) {
	tools := make([]map[string]any, 0, len(raw))
	for _, rawTool := range raw {
		var tool struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Schema      json.RawMessage `json:"input_schema"`
			Cache       json.RawMessage `json:"cache_control"`
			// Chat Completions has no deferred tool loading, so deferred tools are sent up front.
			DeferLoading bool `json:"defer_loading"`
		}
		if err := decodeClientJSON(rawTool, &tool); err != nil {
			return nil, errors.New("tool requires a name and input_schema")
		}
		if tool.Type != "" && tool.Type != "custom" {
			return nil, fmt.Errorf("tool type %q is not supported", tool.Type)
		}
		if err := validateAnthropicCacheControl(tool.Cache); err != nil {
			return nil, err
		}
		if tool.Name == "" || len(tool.Schema) == 0 {
			return nil, errors.New("tool requires a name and input_schema")
		}
		function := map[string]any{"name": tool.Name, "parameters": tool.Schema}
		if tool.Description != "" {
			function["description"] = tool.Description
		}
		tools = append(tools, map[string]any{"type": "function", "function": function})
	}
	return tools, nil
}

func validateAnthropicHints(request anthropicCreateRequest) error {
	if len(request.Metadata) > 0 && string(request.Metadata) != "null" {
		var metadata struct {
			UserID *string `json:"user_id"`
		}
		if err := decodeClientJSON(request.Metadata, &metadata); err != nil {
			return fmt.Errorf("metadata is invalid: %w", err)
		}
		if metadata.UserID != nil && utf8.RuneCountInString(*metadata.UserID) > 512 {
			return errors.New("metadata.user_id must be at most 512 characters")
		}
	}
	if len(request.Thinking) > 0 && string(request.Thinking) != "null" {
		var thinking struct {
			Type         string `json:"type"`
			BudgetTokens *int   `json:"budget_tokens"`
			Display      string `json:"display"`
		}
		if err := decodeClientJSON(request.Thinking, &thinking); err != nil {
			return fmt.Errorf("thinking is invalid: %w", err)
		}
		switch thinking.Type {
		case "adaptive", "disabled", "between_tools", "enabled":
		default:
			return errors.New("thinking.type is not supported")
		}
		if thinking.Type == "enabled" && thinking.BudgetTokens == nil {
			return errors.New("thinking.budget_tokens is required when thinking is enabled")
		}
		if thinking.BudgetTokens != nil && (*thinking.BudgetTokens < 1024 || *thinking.BudgetTokens >= request.MaxTokens) {
			return errors.New("thinking.budget_tokens must be at least 1024 and below max_tokens")
		}
		if thinking.Display != "" && (thinking.Display != "summarized" && thinking.Display != "omitted" || thinking.Type != "adaptive" && thinking.Type != "enabled") {
			return errors.New("thinking.display is not supported")
		}
	}
	if len(request.Output) > 0 && string(request.Output) != "null" {
		var output struct {
			Effort string `json:"effort"`
		}
		if err := decodeClientJSON(request.Output, &output); err != nil {
			return fmt.Errorf("output_config is invalid: %w", err)
		}
		switch output.Effort {
		case "", "low", "medium", "high", "xhigh", "max":
		default:
			return errors.New("output_config.effort is not supported")
		}
	}
	if len(request.Context) > 0 && string(request.Context) != "null" {
		// Edit parameters vary by edit type and are dropped, so only the type is checked.
		var management struct {
			Edits []map[string]json.RawMessage `json:"edits"`
		}
		if err := decodeClientJSON(request.Context, &management); err != nil {
			return fmt.Errorf("context_management is invalid: %w", err)
		}
		for _, edit := range management.Edits {
			if kind := edit["type"]; len(kind) == 0 || kind[0] != '"' {
				return errors.New("context_management.edits[].type must be a string")
			}
		}
	}
	return validateAnthropicCacheControl(request.Cache)
}

func validateAnthropicCacheControl(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var cache struct {
		Type string `json:"type"`
		TTL  string `json:"ttl"`
	}
	if err := decodeClientJSON(raw, &cache); err != nil {
		return fmt.Errorf("cache_control is invalid: %w", err)
	}
	if cache.Type != "ephemeral" {
		return errors.New("cache_control.type must be ephemeral")
	}
	if cache.TTL != "" && cache.TTL != "5m" && cache.TTL != "1h" {
		return errors.New("cache_control.ttl must be 5m or 1h")
	}
	return nil
}

func anthropicToolChoice(raw json.RawMessage) (any, error) {
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := decodeClientJSON(raw, &choice); err != nil {
		return nil, fmt.Errorf("tool_choice is invalid: %w", err)
	}
	switch choice.Type {
	case "auto":
		return "auto", nil
	case "none":
		return "none", nil
	case "any":
		return "required", nil
	case "tool":
		if choice.Name == "" {
			return nil, errors.New("tool choice requires a name")
		}
		return map[string]any{"type": "function", "function": map[string]string{"name": choice.Name}}, nil
	default:
		return nil, fmt.Errorf("tool choice type %q is not supported", choice.Type)
	}
}

func anthropicResponse(raw []byte, logger *slog.Logger, provider string) ([]byte, error) {
	chat, err := decodeChatCompletion(raw)
	if err != nil {
		return nil, err
	}
	choice := chat.Choices[0]
	content := make([]map[string]any, 0, 1+len(choice.Message.ToolCalls))
	if choice.Message.Content != nil && *choice.Message.Content != "" {
		content = append(content, map[string]any{"type": "text", "text": *choice.Message.Content})
	}
	for _, call := range choice.Message.ToolCalls {
		ref := toolCallRef{ID: call.ID, ThoughtSignature: string(call.ExtraContent.Google.ThoughtSignature)}
		if reason := signatureIssue(ref); reason != "" && logger != nil {
			logger.Warn("thought signature not forwarded", "provider", provider, "reason", reason, "bytes", len(ref.ThoughtSignature))
		}
		var input any
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil || input == nil {
			input = map[string]any{}
		}
		callID, _ := clientToolCallID(ref)
		content = append(content, map[string]any{"type": "tool_use", "id": callID, "name": call.Function.Name, "input": input})
	}
	stop := "end_turn"
	switch choice.FinishReason {
	case "tool_calls":
		stop = "tool_use"
	case "length":
		stop = "max_tokens"
	case "content_filter":
		stop = "refusal"
	}
	return json.Marshal(map[string]any{
		"id": "msg_" + chat.ID, "type": "message", "role": "assistant", "model": chat.Model,
		"content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": chat.Usage.PromptTokens, "output_tokens": chat.Usage.CompletionTokens},
	})
}

type anthropicStreamTool struct {
	ID               string
	Name             string
	ThoughtSignature string
	Arguments        strings.Builder
	Block            int
	Emitted          bool
}

type anthropicStreamState struct {
	started   bool
	finished  bool
	textBlock int
	hasText   bool
	id        string
	model     string
	text      strings.Builder
	tools     map[int]*anthropicStreamTool
	usageIn   int
	usageOut  int
}

func (s *anthropicStreamState) writeChunk(w *clientCompatibilityWriter, chunk map[string]json.RawMessage) {
	if !s.started {
		s.started = true
		s.id = rawString(chunk["id"])
		if s.id == "" {
			s.id = "msg_compat"
		} else {
			s.id = "msg_" + strings.TrimPrefix(s.id, "chatcmpl-")
		}
		s.model = rawString(chunk["model"])
		w.writeEvent("message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": s.id, "type": "message", "role": "assistant", "model": s.model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
		}})
	}
	var usage struct {
		Input  int `json:"prompt_tokens"`
		Output int `json:"completion_tokens"`
	}
	if json.Unmarshal(chunk["usage"], &usage) == nil && len(chunk["usage"]) > 0 {
		s.usageIn, s.usageOut = usage.Input, usage.Output
	}
	var choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
				ExtraContent googleExtraContent `json:"extra_content"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	}
	if json.Unmarshal(chunk["choices"], &choices) != nil || len(choices) == 0 {
		return
	}
	choice := choices[0]
	if choice.Delta.Content != "" {
		if !s.hasText {
			s.textBlock = s.nextBlock()
			s.hasText = true
			w.writeEvent("content_block_start", map[string]any{"type": "content_block_start", "index": s.textBlock, "content_block": map[string]string{"type": "text", "text": ""}})
		}
		s.text.WriteString(choice.Delta.Content)
		w.writeEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.textBlock, "delta": map[string]string{"type": "text_delta", "text": choice.Delta.Content}})
	}
	if s.tools == nil {
		s.tools = make(map[int]*anthropicStreamTool)
	}
	for _, delta := range choice.Delta.ToolCalls {
		tool := s.tools[delta.Index]
		if tool == nil {
			tool = &anthropicStreamTool{ID: delta.ID, Name: delta.Function.Name}
			s.tools[delta.Index] = tool
		}
		if delta.ID != "" {
			tool.ID = delta.ID
		}
		if delta.Function.Name != "" {
			tool.Name = delta.Function.Name
		}
		if delta.Function.Arguments != "" {
			tool.Arguments.WriteString(delta.Function.Arguments)
		}
		if signature := string(delta.ExtraContent.Google.ThoughtSignature); signature != "" {
			tool.ThoughtSignature = signature
		}
	}
	if choice.FinishReason != nil {
		s.finish(w, anthropicStop(*choice.FinishReason))
	}
}

func (s *anthropicStreamState) nextBlock() int {
	max := 0
	if s.hasText {
		max = s.textBlock + 1
	}
	for _, tool := range s.tools {
		if tool.Emitted && tool.Block >= max {
			max = tool.Block + 1
		}
	}
	return max
}

func (s *anthropicStreamState) finish(w *clientCompatibilityWriter, stop string) {
	if s.finished || !s.started {
		return
	}
	s.finished = true
	if s.hasText {
		w.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.textBlock})
	}
	indexes := make([]int, 0, len(s.tools))
	for index := range s.tools {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		tool := s.tools[index]
		ref := toolCallRef{ID: tool.ID, ThoughtSignature: tool.ThoughtSignature}
		if reason := signatureIssue(ref); reason != "" && w.logger != nil {
			w.logger.Warn("thought signature not forwarded", "provider", w.Header().Get("X-Spillway-Provider"), "reason", reason, "bytes", len(ref.ThoughtSignature))
		}
		callID, _ := clientToolCallID(ref)
		tool.Block = s.nextBlock()
		tool.Emitted = true
		w.writeEvent("content_block_start", map[string]any{"type": "content_block_start", "index": tool.Block, "content_block": map[string]any{"type": "tool_use", "id": callID, "name": tool.Name, "input": map[string]any{}}})
		w.writeEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": tool.Block, "delta": map[string]string{"type": "input_json_delta", "partial_json": tool.Arguments.String()}})
		w.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": tool.Block})
	}
	w.writeEvent("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": s.usageOut}})
	w.writeEvent("message_stop", map[string]string{"type": "message_stop"})
}

func anthropicStop(reason string) string {
	switch reason {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

func awaitsToolResults(messages []map[string]any) bool {
	if len(messages) == 0 {
		return false
	}
	_, ok := messages[len(messages)-1]["tool_calls"]
	return ok
}
