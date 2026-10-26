package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

type responseCreateRequest struct {
	Model           string            `json:"model"`
	Input           json.RawMessage   `json:"input"`
	Instructions    string            `json:"instructions"`
	Tools           []json.RawMessage `json:"tools"`
	ToolChoice      json.RawMessage   `json:"tool_choice"`
	MaxOutputTokens json.RawMessage   `json:"max_output_tokens"`
	Temperature     json.RawMessage   `json:"temperature"`
	TopP            json.RawMessage   `json:"top_p"`
	Stream          bool              `json:"stream"`
	ParallelTools   json.RawMessage   `json:"parallel_tool_calls"`
	Reasoning       json.RawMessage   `json:"reasoning"`
	Text            json.RawMessage   `json:"text"`
	Metadata        json.RawMessage   `json:"metadata"`
	Include         json.RawMessage   `json:"include"` // Accepted for client compatibility; unsupported extra fields are omitted.
	Background      bool              `json:"background"`
	Store           *bool             `json:"store"` // Accepted for client compatibility; Spillway does not retain Responses objects.
}

func responsesRequest(raw []byte) ([]byte, bool, error) {
	var request responseCreateRequest
	if err := decodeClientJSON(raw, &request); err != nil {
		return nil, false, fmt.Errorf("invalid Responses API request: %w", err)
	}
	if request.Model == "" {
		return nil, false, errors.New("model is required")
	}
	if request.Background {
		return nil, false, errors.New("background Responses API requests are not supported")
	}
	messages, err := responseInputMessages(request.Input)
	if err != nil {
		return nil, false, err
	}
	if request.Instructions != "" {
		messages = append([]map[string]any{{"role": "system", "content": request.Instructions}}, messages...)
	}
	if len(messages) == 0 {
		return nil, false, errors.New("input must contain at least one message or instruction")
	}
	chat := map[string]any{"model": request.Model, "messages": messages, "stream": request.Stream}
	if len(request.Tools) > 0 {
		tools, err := responseTools(request.Tools)
		if err != nil {
			return nil, false, err
		}
		chat["tools"] = tools
	}
	if len(request.ToolChoice) > 0 {
		choice, err := responseToolChoice(request.ToolChoice)
		if err != nil {
			return nil, false, err
		}
		chat["tool_choice"] = choice
	}
	copyRawField(chat, "max_tokens", request.MaxOutputTokens)
	copyRawField(chat, "temperature", request.Temperature)
	copyRawField(chat, "top_p", request.TopP)
	copyRawField(chat, "parallel_tool_calls", request.ParallelTools)
	copyRawField(chat, "metadata", request.Metadata)
	if len(request.Reasoning) > 0 {
		var reasoning struct {
			Effort string `json:"effort"`
		}
		if err := decodeClientJSON(request.Reasoning, &reasoning); err != nil {
			return nil, false, fmt.Errorf("reasoning is invalid: %w", err)
		}
		if reasoning.Effort != "" {
			chat["reasoning_effort"] = reasoning.Effort
		}
	}
	if len(request.Text) > 0 {
		format, err := responseTextFormat(request.Text)
		if err != nil {
			return nil, false, err
		}
		if format != nil {
			chat["response_format"] = format
		}
	}
	data, err := json.Marshal(chat)
	return data, request.Stream, err
}

func responseInputMessages(raw json.RawMessage) ([]map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []map[string]any{{"role": "user", "content": text}}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errors.New("input must be text or an array of input items")
	}
	messages := make([]map[string]any, 0, len(items))
	for _, item := range items {
		var entry struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Output    json.RawMessage `json:"output"`
		}
		if err := decodeClientJSON(item, &entry); err != nil {
			return nil, fmt.Errorf("input item is invalid: %w", err)
		}
		switch entry.Type {
		case "message", "":
			if entry.Role != "user" && entry.Role != "assistant" && entry.Role != "system" && entry.Role != "developer" {
				return nil, fmt.Errorf("input message role %q is not supported", entry.Role)
			}
			content, err := responseContent(entry.Content)
			if err != nil {
				return nil, err
			}
			messages = append(messages, map[string]any{"role": entry.Role, "content": content})
		case "function_call":
			messages = append(messages, map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []map[string]any{{"id": entry.CallID, "type": "function", "function": map[string]any{
					"name": entry.Name, "arguments": string(entry.Arguments),
				}}},
			})
		case "function_call_output":
			if entry.CallID == "" {
				return nil, errors.New("function_call_output requires call_id")
			}
			output, err := responseOutputText(entry.Output)
			if err != nil {
				return nil, err
			}
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": entry.CallID, "content": output})
		default:
			return nil, fmt.Errorf("input item type %q is not supported", entry.Type)
		}
	}
	return messages, nil
}

func responseContent(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL string `json:"image_url"`
	}
	if err := decodeClientJSON(raw, &parts); err != nil {
		return nil, errors.New("message content must be text or an array of content parts")
	}
	converted := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "input_text", "output_text":
			converted = append(converted, map[string]any{"type": "text", "text": part.Text})
		case "input_image":
			if part.ImageURL == "" {
				return nil, errors.New("input image must use an image_url URL")
			}
			converted = append(converted, map[string]any{"type": "image_url", "image_url": map[string]string{"url": part.ImageURL}})
		default:
			return nil, fmt.Errorf("input content type %q is not supported", part.Type)
		}
	}
	return converted, nil
}

func responseOutputText(raw json.RawMessage) (string, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := decodeClientJSON(raw, &parts); err != nil {
		return "", errors.New("tool output must be text or an array of text parts")
	}
	var result strings.Builder
	for _, part := range parts {
		switch part.Type {
		case "", "text", "input_text", "output_text":
			result.WriteString(part.Text)
		default:
			return "", fmt.Errorf("tool output type %q is not supported", part.Type)
		}
	}
	return result.String(), nil
}

func responseTools(raw []json.RawMessage) ([]map[string]any, error) {
	tools := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		var tool struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
			Strict      *bool           `json:"strict"`
		}
		if err := decodeClientJSON(item, &tool); err != nil {
			return nil, fmt.Errorf("tool definition is invalid: %w", err)
		}
		if tool.Type != "function" || tool.Name == "" || len(tool.Parameters) == 0 {
			return nil, fmt.Errorf("tool type %q requires a function name and parameters", tool.Type)
		}
		function := map[string]any{"name": tool.Name, "parameters": tool.Parameters}
		if tool.Description != "" {
			function["description"] = tool.Description
		}
		if tool.Strict != nil {
			function["strict"] = *tool.Strict
		}
		tools = append(tools, map[string]any{"type": "function", "function": function})
	}
	return tools, nil
}

func responseToolChoice(raw json.RawMessage) (any, error) {
	var simple string
	if json.Unmarshal(raw, &simple) == nil {
		switch simple {
		case "auto", "none", "required":
			return simple, nil
		default:
			return nil, fmt.Errorf("tool choice %q is not supported", simple)
		}
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := decodeClientJSON(raw, &choice); err != nil || choice.Type != "function" || choice.Name == "" {
		return nil, errors.New("tool_choice must be auto, none, required, or a function choice")
	}
	return map[string]any{"type": "function", "function": map[string]string{"name": choice.Name}}, nil
}

func responseTextFormat(raw json.RawMessage) (map[string]any, error) {
	var text struct {
		Format json.RawMessage `json:"format"`
	}
	if err := decodeClientJSON(raw, &text); err != nil {
		return nil, fmt.Errorf("text is invalid: %w", err)
	}
	if len(text.Format) == 0 {
		return nil, nil
	}
	var format struct {
		Type   string          `json:"type"`
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
		Strict *bool           `json:"strict"`
	}
	if err := decodeClientJSON(text.Format, &format); err != nil {
		return nil, fmt.Errorf("text.format is invalid: %w", err)
	}
	switch format.Type {
	case "text":
		return nil, nil
	case "json_object":
		return map[string]any{"type": "json_object"}, nil
	case "json_schema":
		if format.Name == "" || len(format.Schema) == 0 {
			return nil, errors.New("json_schema format requires name and schema")
		}
		schema := map[string]any{"name": format.Name, "schema": format.Schema}
		if format.Strict != nil {
			schema["strict"] = *format.Strict
		}
		return map[string]any{"type": "json_schema", "json_schema": schema}, nil
	default:
		return nil, fmt.Errorf("text format %q is not supported", format.Type)
	}
}

func copyRawField(dst map[string]any, target string, raw json.RawMessage) {
	if len(raw) > 0 && string(raw) != "null" {
		dst[target] = raw
	}
}

func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

type chatCompletionResponse struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   *string `json:"content"`
			Refusal   string  `json:"refusal"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func decodeChatCompletion(raw []byte) (chatCompletionResponse, error) {
	var response chatCompletionResponse
	if err := json.Unmarshal(raw, &response); err != nil || len(response.Choices) == 0 {
		return chatCompletionResponse{}, errors.New("provider returned an invalid chat completion")
	}
	return response, nil
}

func responsesResponse(raw []byte) ([]byte, error) {
	chat, err := decodeChatCompletion(raw)
	if err != nil {
		return nil, err
	}
	choice := chat.Choices[0]
	output := make([]map[string]any, 0, 1+len(choice.Message.ToolCalls))
	if choice.Message.Content != nil || choice.Message.Refusal != "" {
		text := ""
		if choice.Message.Content != nil {
			text = *choice.Message.Content
		}
		contentType := "output_text"
		if choice.Message.Refusal != "" {
			contentType = "refusal"
			text = choice.Message.Refusal
		}
		output = append(output, map[string]any{"id": "msg_" + chat.ID, "type": "message", "status": "completed", "role": "assistant", "content": []map[string]any{{"type": contentType, "text": text, "annotations": []any{}}}})
	}
	for _, call := range choice.Message.ToolCalls {
		output = append(output, map[string]any{"id": call.ID, "type": "function_call", "status": "completed", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments})
	}
	status := "completed"
	var incomplete any
	if choice.FinishReason == "length" {
		status = "incomplete"
		incomplete = map[string]string{"reason": "max_output_tokens"}
	}
	return json.Marshal(map[string]any{
		"id": "resp_" + strings.TrimPrefix(chat.ID, "chatcmpl-"), "object": "response", "created_at": chat.Created,
		"status": status, "error": nil, "incomplete_details": incomplete, "model": chat.Model,
		"output": output, "usage": map[string]int{"input_tokens": chat.Usage.PromptTokens, "output_tokens": chat.Usage.CompletionTokens, "total_tokens": chat.Usage.TotalTokens},
	})
}

type responseToolStream struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

type responsesStreamState struct {
	started  bool
	finished bool
	id       string
	model    string
	created  int64
	text     strings.Builder
	tools    map[int]*responseToolStream
	usage    map[string]int
	sequence int
}

func (s *responsesStreamState) writeChunk(w *clientCompatibilityWriter, chunk map[string]json.RawMessage) {
	if !s.started {
		s.started = true
		s.id = "resp_compat"
		s.model = rawString(chunk["model"])
		_ = json.Unmarshal(chunk["created"], &s.created)
		id := rawString(chunk["id"])
		if id != "" {
			s.id = "resp_" + strings.TrimPrefix(id, "chatcmpl-")
		}
		w.writeEvent("response.created", s.event(map[string]any{"type": "response.created", "response": map[string]any{
			"id": s.id, "object": "response", "created_at": s.created, "status": "in_progress", "model": s.model, "output": []any{}, "usage": nil,
		}}))
		w.writeEvent("response.output_item.added", s.event(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "msg_" + s.id, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}}))
		w.writeEvent("response.content_part.added", s.event(map[string]any{"type": "response.content_part.added", "item_id": "msg_" + s.id, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}))
	}
	var usage struct {
		Prompt     int `json:"prompt_tokens"`
		Completion int `json:"completion_tokens"`
		Total      int `json:"total_tokens"`
	}
	if len(chunk["usage"]) > 0 && json.Unmarshal(chunk["usage"], &usage) == nil {
		s.usage = map[string]int{"input_tokens": usage.Prompt, "output_tokens": usage.Completion, "total_tokens": usage.Total}
	}
	var choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	}
	if json.Unmarshal(chunk["choices"], &choices) != nil {
		return
	}
	if len(choices) == 0 {
		return
	}
	choice := choices[0]
	if choice.Delta.Content != "" {
		s.text.WriteString(choice.Delta.Content)
		w.writeEvent("response.output_text.delta", s.event(map[string]any{"type": "response.output_text.delta", "item_id": "msg_" + s.id, "output_index": 0, "content_index": 0, "delta": choice.Delta.Content}))
	}
	if s.tools == nil {
		s.tools = make(map[int]*responseToolStream)
	}
	for _, delta := range choice.Delta.ToolCalls {
		tool := s.tools[delta.Index]
		if tool == nil {
			tool = &responseToolStream{ID: delta.ID}
			s.tools[delta.Index] = tool
			w.writeEvent("response.output_item.added", s.event(map[string]any{"type": "response.output_item.added", "output_index": delta.Index + 1, "item": map[string]any{"id": delta.ID, "type": "function_call", "status": "in_progress", "call_id": delta.ID, "name": delta.Function.Name, "arguments": ""}}))
		}
		if delta.ID != "" {
			tool.ID = delta.ID
		}
		if delta.Function.Name != "" {
			tool.Name = delta.Function.Name
		}
		if delta.Function.Arguments != "" {
			tool.Arguments.WriteString(delta.Function.Arguments)
			w.writeEvent("response.function_call_arguments.delta", s.event(map[string]any{"type": "response.function_call_arguments.delta", "item_id": tool.ID, "output_index": delta.Index + 1, "delta": delta.Function.Arguments}))
		}
	}
	if choice.FinishReason != nil {
		s.finish(w)
	}
}

func (s *responsesStreamState) finish(w *clientCompatibilityWriter) {
	if s.finished || !s.started {
		return
	}
	s.finished = true
	w.writeEvent("response.output_text.done", s.event(map[string]any{"type": "response.output_text.done", "item_id": "msg_" + s.id, "output_index": 0, "content_index": 0, "text": s.text.String()}))
	w.writeEvent("response.content_part.done", s.event(map[string]any{"type": "response.content_part.done", "item_id": "msg_" + s.id, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": s.text.String(), "annotations": []any{}}}))
	w.writeEvent("response.output_item.done", s.event(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"id": "msg_" + s.id, "type": "message", "status": "completed", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": s.text.String(), "annotations": []any{}}}}}))
	output := []any{map[string]any{"id": "msg_" + s.id, "type": "message", "status": "completed", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": s.text.String(), "annotations": []any{}}}}}
	indexes := make([]int, 0, len(s.tools))
	for index := range s.tools {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		tool := s.tools[index]
		w.writeEvent("response.function_call_arguments.done", s.event(map[string]any{"type": "response.function_call_arguments.done", "item_id": tool.ID, "output_index": index + 1, "arguments": tool.Arguments.String()}))
		item := map[string]any{"id": tool.ID, "type": "function_call", "status": "completed", "call_id": tool.ID, "name": tool.Name, "arguments": tool.Arguments.String()}
		output = append(output, item)
		w.writeEvent("response.output_item.done", s.event(map[string]any{"type": "response.output_item.done", "output_index": index + 1, "item": item}))
	}
	if s.usage == nil {
		s.usage = map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	}
	response := map[string]any{"id": s.id, "object": "response", "created_at": s.created, "status": "completed", "error": nil, "incomplete_details": nil, "model": s.model, "output": output, "usage": s.usage}
	w.writeEvent("response.completed", s.event(map[string]any{"type": "response.completed", "response": response}))
}

func (s *responsesStreamState) event(value map[string]any) map[string]any {
	s.sequence++
	value["sequence_number"] = s.sequence
	return value
}
