// Package openaicompat implements providers that expose the OpenAI API.
package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/redact"
)

const maxResponseBytes = 8 << 20
const maxErrorBytes = 64 << 10
const maxSSELineBytes = 4 << 20

// Options configures one OpenAI-compatible provider endpoint.
type Options struct {
	Name          string
	BaseURL       string
	APIKey        redact.Secret
	Timeout       time.Duration
	HTTPClient    *http.Client
	ClassifyError func(status int, body []byte) (error, bool)
	// KeepToolCallExtraContent forwards extra_content on assistant tool calls; other OpenAI-compatible APIs may reject it.
	KeepToolCallExtraContent bool
}

// Client implements providers.Provider using the OpenAI-compatible API.
type Client struct {
	name                     string
	baseURL                  string
	key                      redact.Secret
	timeout                  time.Duration
	http                     *http.Client
	clean                    *redact.Scrubber
	classifyError            func(status int, body []byte) (error, bool)
	keepToolCallExtraContent bool
}

// New validates options and builds a client that never follows redirects.
func New(o Options) (*Client, error) {
	if o.Name == "" {
		return nil, errors.New("openaicompat: provider name is required")
	}
	u, err := url.Parse(o.BaseURL)
	if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("openaicompat: base URL must be an absolute URL without userinfo, query or fragment")
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname())) {
		return nil, errors.New("openaicompat: base URL must use https unless the host is loopback")
	}
	if o.APIKey.IsZero() && o.Name != "ollama" {
		return nil, errors.New("openaicompat: API key is required")
	}
	if o.Timeout <= 0 {
		return nil, errors.New("openaicompat: timeout must be positive")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	httpClient := &http.Client{Transport: transport}
	if o.HTTPClient != nil {
		clientCopy := *o.HTTPClient
		httpClient = &clientCopy
	}
	httpClient.Timeout = 0
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		name: o.Name, baseURL: strings.TrimRight(o.BaseURL, "/"), key: o.APIKey, timeout: o.Timeout,
		http:  httpClient,
		clean: redact.NewScrubber(o.APIKey), classifyError: o.ClassifyError,
		keepToolCallExtraContent: o.KeepToolCallExtraContent,
	}, nil
}

func loopback(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Name returns the configured provider name.
func (c *Client) Name() string { return c.name }

// Models lists upstream models from the configured endpoint.
func (c *Client) Models(ctx context.Context) ([]providers.Model, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := c.request(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, fmt.Errorf("%s: build models request: %w", c.name, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.transportError("list models", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.responseError(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, c.clean.Error(fmt.Errorf("%s: read model list: %w", c.name, err))
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("%s: model list response exceeds limit", c.name)
	}
	var result struct {
		Data []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("%s: decode model list: %w", c.name, err)
	}
	models := make([]providers.Model, 0, len(result.Data))
	for _, item := range result.Data {
		if item.ID == "" {
			continue
		}
		models = append(models, providers.Model{ID: item.ID, Created: item.Created, OwnedBy: item.OwnedBy})
	}
	return models, nil
}

// Chat sends a chat completion and returns its response body without reshaping it.
func (c *Client) Chat(ctx context.Context, chat providers.ChatRequest) (providers.ChatResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	body, err := requestBody(chat, c.keepToolCallExtraContent)
	if err != nil {
		return providers.ChatResponse{}, fmt.Errorf("%s: encode chat request: %w", c.name, err)
	}
	req, err := c.request(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return providers.ChatResponse{}, fmt.Errorf("%s: build chat request: %w", c.name, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return providers.ChatResponse{}, c.transportError("chat completion", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, err := c.responseErrorBody(resp)
		return providers.ChatResponse{Body: responseUsageOnly(data)}, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return providers.ChatResponse{}, c.clean.Error(fmt.Errorf("%s: read chat response: %w", c.name, err))
	}
	if len(data) > maxResponseBytes {
		return providers.ChatResponse{}, fmt.Errorf("%s: chat response exceeds limit", c.name)
	}
	return providers.ChatResponse{Body: data, RateLimit: rateLimit(resp.Header)}, nil
}

// ChatStream starts an SSE chat completion stream.
func (c *Client) ChatStream(ctx context.Context, chat providers.ChatRequest) (providers.Stream, error) {
	ctx, cancel := context.WithCancel(ctx)
	timeout := time.AfterFunc(c.timeout, cancel)
	body, err := requestBody(chat, c.keepToolCallExtraContent)
	if err != nil {
		timeout.Stop()
		cancel()
		return nil, fmt.Errorf("%s: encode chat request: %w", c.name, err)
	}
	req, err := c.request(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		timeout.Stop()
		cancel()
		return nil, fmt.Errorf("%s: build chat request: %w", c.name, err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		timeout.Stop()
		cancel()
		return nil, c.transportError("stream chat completion", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() {
			timeout.Stop()
			cancel()
		}()
		defer func() { _ = resp.Body.Close() }()
		return nil, c.responseError(resp)
	}
	timeout.Stop()
	return &sseStream{
		body: resp.Body, cancel: cancel, scanner: newScanner(resp.Body), clean: c.clean,
		name: c.name, idleTimeout: c.timeout, rateLimit: rateLimit(resp.Header),
	}, nil
}

func rateLimit(header http.Header) providers.RateLimit {
	requests, resetRequests, requestsKnown := rateLimitPair(
		header.Get("x-ratelimit-remaining-requests"), header.Get("x-ratelimit-reset-requests"),
	)
	tokens, resetTokens, tokensKnown := rateLimitPair(
		header.Get("x-ratelimit-remaining-tokens"), header.Get("x-ratelimit-reset-tokens"),
	)
	return providers.RateLimit{
		RemainingRequests: requests, RemainingTokens: tokens,
		ResetRequests: resetRequests, ResetTokens: resetTokens,
		Known: requestsKnown || tokensKnown,
	}
}

func rateLimitPair(remainingValue, resetValue string) (int, time.Duration, bool) {
	remaining, err := strconv.Atoi(strings.TrimSpace(remainingValue))
	if err != nil || remaining < 0 {
		return 0, 0, false
	}
	reset, err := time.ParseDuration(strings.TrimSpace(resetValue))
	if err != nil || reset <= 0 {
		return 0, 0, false
	}
	return remaining, providers.ClampRetryAfter(reset), true
}

func (c *Client) request(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	if !c.key.IsZero() {
		req.Header.Set("Authorization", "Bearer "+c.key.Reveal())
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *Client) transportError(action string, err error) error {
	return c.clean.Error(fmt.Errorf("%s: %s: %w: %w", c.name, action, providers.ErrUpstreamUnavailable, err))
}

func (c *Client) responseError(resp *http.Response) error {
	_, err := c.responseErrorBody(resp)
	return err
}

func (c *Client) responseErrorBody(resp *http.Response) ([]byte, error) {
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes+int64(len(c.key.Reveal()))))
	// The status decides before any body code or preset classifier: Groq's per-request TPM 413 says rate_limit_exceeded, yet waiting never helps it (ADR 0034).
	tooLarge := resp.StatusCode == http.StatusRequestEntityTooLarge || resp.StatusCode == http.StatusBadRequest && isContextLengthCode(errorCode(body))
	if c.classifyError != nil && !tooLarge {
		if kind, ok := c.classifyError(resp.StatusCode, body); ok {
			message := fmt.Errorf("%s: upstream status %d: %s", c.name, resp.StatusCode, errorMessage(body, c.clean))
			if readErr != nil {
				message = fmt.Errorf("%w: read upstream error response: %w", message, c.clean.Error(readErr))
			}
			return body, fmt.Errorf("%w: %w", kind, message)
		}
	}
	detail := errorMessage(body, c.clean)
	if detail == "" {
		detail = http.StatusText(resp.StatusCode)
	}
	message := fmt.Errorf("%s: upstream status %d: %s", c.name, resp.StatusCode, detail)
	if readErr != nil {
		message = fmt.Errorf("%w: read upstream error response: %w", message, c.clean.Error(readErr))
	}
	switch {
	case tooLarge:
		return body, fmt.Errorf("%w: %w", providers.ErrRequestTooLarge, message)
	case resp.StatusCode == http.StatusPaymentRequired:
		after := retryAfter(resp.Header.Get("Retry-After"), time.Now())
		if after > 0 {
			return body, fmt.Errorf("%w: %w", providers.RetryAfterError{After: after, Kind: providers.ErrPaymentRequired}, message)
		}
		return body, fmt.Errorf("%w: %w", providers.ErrPaymentRequired, message)
	case resp.StatusCode == http.StatusTooManyRequests:
		after := retryAfter(resp.Header.Get("Retry-After"), time.Now())
		return body, fmt.Errorf("%w: %w", providers.RetryAfterError{After: after, Kind: providers.ErrRateLimited}, message)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return body, fmt.Errorf("%w: %w", providers.ErrUnauthorized, message)
	case resp.StatusCode == http.StatusBadRequest:
		return body, fmt.Errorf("%w: %w", providers.ErrBadRequest, message)
	case resp.StatusCode == http.StatusNotFound:
		return body, fmt.Errorf("%w: %w", providers.ErrModelNotFound, message)
	case resp.StatusCode >= 500:
		after := retryAfter(resp.Header.Get("Retry-After"), time.Now())
		if after > 0 {
			return body, fmt.Errorf("%w: %w", providers.RetryAfterError{After: after, Kind: providers.ErrUpstreamUnavailable}, message)
		}
		return body, fmt.Errorf("%w: %w", providers.ErrUpstreamUnavailable, message)
	default:
		return body, fmt.Errorf("%w: %w", providers.ErrBadRequest, message)
	}
}

func responseUsageOnly(body []byte) []byte {
	var response struct {
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(body, &response) != nil || len(response.Usage) == 0 || string(response.Usage) == "null" {
		return nil
	}
	encoded, err := json.Marshal(struct {
		Usage json.RawMessage `json:"usage"`
	}{Usage: response.Usage})
	if err != nil {
		return nil
	}
	return encoded
}

// errorCode returns error.code from an OpenAI-shaped error body, or nil.
func errorCode(body []byte) json.RawMessage {
	var payload struct {
		Error struct {
			Code json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	return payload.Error.Code
}

// isContextLengthCode reports whether code is OpenAI's structured code for input beyond a model's limits.
func isContextLengthCode(code json.RawMessage) bool {
	var text string
	return json.Unmarshal(code, &text) == nil && text == "context_length_exceeded"
}

// errorMessage scrubs before truncating, because a cut through a key would hide it from the scrubber.
func errorMessage(body []byte, clean *redact.Scrubber) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &payload) == nil {
		message := payload.Error.Message
		if message == "" {
			message = payload.Message
		}
		if message != "" {
			message = strings.TrimSpace(clean.Scrub(message))
			if len(message) > 512 {
				message = message[:512]
				for !utf8.ValidString(message) {
					message = message[:len(message)-1]
				}
			}
			return message
		}
	}
	return "upstream returned an error response"
}

func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds > 0 {
		return providers.ClampRetryAfter(time.Duration(min(seconds, int64((24*time.Hour)/time.Second))) * time.Second)
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return providers.ClampRetryAfter(at.Sub(now))
	}
	return 0
}

func requestBody(chat providers.ChatRequest, keepToolCallExtraContent bool) ([]byte, error) {
	data := chat.RawBody
	if len(chat.RawBody) == 0 {
		var err error
		data, err = json.Marshal(chat)
		if err != nil {
			return nil, err
		}
	}
	if !keepToolCallExtraContent {
		var err error
		if data, err = EditMessages(data, messageWithoutToolCallExtraContent); err != nil {
			return nil, fmt.Errorf("%w: chat request: %w", providers.ErrBadRequest, err)
		}
	}
	model, err := json.Marshal(chat.Model)
	if err != nil {
		return nil, err
	}
	members, err := objectMembers(data)
	if err != nil {
		return nil, fmt.Errorf("chat request: %w", err)
	}
	if !slices.ContainsFunc(members, func(member objectMember) bool { return member.key == "model" }) {
		return nil, errors.New("chat request is missing model")
	}
	var edits []valueEdit
	for _, member := range members {
		if member.key == "model" {
			edits = append(edits, valueEdit{objectMember: member, value: model})
		}
	}
	return applyEdits(data, edits), nil
}

// EditMessages applies edit to each item of a chat request body's messages array.
// Every byte outside the edited messages is kept, and body itself is returned when no message changes.
func EditMessages(body []byte, edit func(index int, message json.RawMessage) (edited json.RawMessage, changed bool, err error)) ([]byte, error) {
	members, err := objectMembers(body)
	if err != nil {
		return nil, err
	}
	var edits []valueEdit
	for _, member := range members {
		if member.key != "messages" {
			continue
		}
		messages, changed, err := editArrayItems(body[member.start:member.end], "messages", edit)
		if err != nil {
			return nil, err
		}
		if changed {
			edits = append(edits, valueEdit{objectMember: member, value: messages})
		}
	}
	return applyEdits(body, edits), nil
}

// messageWithoutToolCallExtraContent removes Google's extra_content from a message's tool calls.
func messageWithoutToolCallExtraContent(index int, message json.RawMessage) (json.RawMessage, bool, error) {
	members, err := objectMembers(message)
	if err != nil {
		return nil, false, fmt.Errorf("message %d: %w", index, err)
	}
	var edits []valueEdit
	for _, member := range members {
		if member.key != "tool_calls" {
			continue
		}
		calls, changed, err := editArrayItems(message[member.start:member.end], "tool_calls", toolCallWithoutExtraContent)
		if err != nil {
			return nil, false, fmt.Errorf("message %d: %w", index, err)
		}
		if changed {
			edits = append(edits, valueEdit{objectMember: member, value: calls})
		}
	}
	return applyEdits(message, edits), len(edits) > 0, nil
}

// toolCallWithoutExtraContent re-encodes only calls that carry extra_content, so other calls stay byte-identical.
func toolCallWithoutExtraContent(index int, raw json.RawMessage) (json.RawMessage, bool, error) {
	var call map[string]json.RawMessage
	if err := json.Unmarshal(raw, &call); err != nil || call == nil {
		return nil, false, fmt.Errorf("tool call %d must be a JSON object", index)
	}
	if _, ok := call["extra_content"]; !ok {
		return raw, false, nil
	}
	delete(call, "extra_content")
	var encoded bytes.Buffer
	enc := json.NewEncoder(&encoded)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(call); err != nil {
		return nil, false, fmt.Errorf("encode tool call %d: %w", index, err)
	}
	return bytes.TrimSuffix(encoded.Bytes(), []byte("\n")), true, nil
}

// editArrayItems applies edit to each item of a JSON array and returns raw unchanged when no item changes.
func editArrayItems(raw []byte, name string, edit func(int, json.RawMessage) (json.RawMessage, bool, error)) ([]byte, bool, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, false, fmt.Errorf("%s must be an array", name)
	}
	changed := false
	for i, item := range items {
		edited, itemChanged, err := edit(i, item)
		if err != nil {
			return nil, false, err
		}
		if itemChanged {
			items[i], changed = edited, true
		}
	}
	if !changed {
		return raw, false, nil
	}
	var out bytes.Buffer
	out.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			out.WriteByte(',')
		}
		out.Write(item)
	}
	out.WriteByte(']')
	return out.Bytes(), true, nil
}

// objectMember locates one member's value inside a JSON object so it can be replaced without re-encoding its neighbours.
type objectMember struct {
	key        string
	start, end int
}

type valueEdit struct {
	objectMember
	value []byte
}

func objectMembers(data []byte) ([]objectMember, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("must be a JSON object")
	}
	var members []objectMember
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("contains an invalid field")
		}
		start := int(dec.InputOffset())
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		end := int(dec.InputOffset())
		for start < end && (data[start] == ' ' || data[start] == '\n' || data[start] == '\r' || data[start] == '\t' || data[start] == ':') {
			start++
		}
		members = append(members, objectMember{key: key, start: start, end: end})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return members, nil
}

func applyEdits(data []byte, edits []valueEdit) []byte {
	if len(edits) == 0 {
		return data
	}
	var out bytes.Buffer
	last := 0
	for _, edit := range edits {
		out.Write(data[last:edit.start])
		out.Write(edit.value)
		last = edit.end
	}
	out.Write(data[last:])
	return out.Bytes()
}

func newScanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), maxSSELineBytes)
	return s
}

type sseStream struct {
	body         io.ReadCloser
	cancel       context.CancelFunc
	scanner      *bufio.Scanner
	clean        *redact.Scrubber
	name         string
	idleTimeout  time.Duration
	rateLimit    providers.RateLimit
	idleTimedOut atomic.Bool
	closed       bool
	done         bool
}

// RateLimit reports quota hints from the stream's response headers.
func (s *sseStream) RateLimit() providers.RateLimit { return s.rateLimit }

func (s *sseStream) Recv() (providers.ChatChunk, error) {
	if s.closed {
		return providers.ChatChunk{}, errors.New("openaicompat: stream is closed")
	}
	idleTimer := time.AfterFunc(s.idleTimeout, func() {
		s.idleTimedOut.Store(true)
		s.cancel()
	})
	defer idleTimer.Stop()
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := bytes.TrimSpace([]byte(strings.TrimPrefix(line, "data:")))
		if bytes.Equal(data, []byte("[DONE]")) {
			s.done = true
			return providers.ChatChunk{}, io.EOF
		}
		var payload struct {
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(data, &payload); err == nil && len(payload.Error) > 0 && !bytes.Equal(payload.Error, []byte("null")) {
			var upstreamError struct {
				Code json.RawMessage `json:"code"`
			}
			_ = json.Unmarshal(payload.Error, &upstreamError)
			return providers.ChatChunk{}, s.streamError(streamErrorKind(upstreamError.Code), errorMessage(data, s.clean))
		}
		var chunk struct {
			Usage *struct {
				PromptTokens     int             `json:"prompt_tokens"`
				CompletionTokens int             `json:"completion_tokens"`
				TotalTokens      int             `json:"total_tokens"`
				Cost             json.RawMessage `json:"cost"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return providers.ChatChunk{}, fmt.Errorf("%s: decode stream chunk: %w", s.name, err)
		}
		result := providers.ChatChunk{Data: append(json.RawMessage(nil), data...)}
		if chunk.Usage != nil {
			result.Usage = &providers.Usage{PromptTokens: chunk.Usage.PromptTokens, CompletionTokens: chunk.Usage.CompletionTokens, TotalTokens: chunk.Usage.TotalTokens}
			result.ReportedCost = chunk.Usage.Cost
		}
		return result, nil
	}
	if err := s.scanner.Err(); err != nil {
		if s.idleTimedOut.Load() {
			return providers.ChatChunk{}, fmt.Errorf("%w: no stream data for %s", providers.ErrUpstreamUnavailable, s.idleTimeout)
		}
		return providers.ChatChunk{}, s.clean.Error(fmt.Errorf("%s: read stream: %w", s.name, err))
	}
	if s.done {
		return providers.ChatChunk{}, io.EOF
	}
	return providers.ChatChunk{}, s.streamError(providers.ErrUpstreamUnavailable, "stream ended without [DONE]")
}

func streamErrorKind(code json.RawMessage) error {
	if isContextLengthCode(code) {
		return providers.ErrRequestTooLarge
	}
	var status int
	if err := json.Unmarshal(code, &status); err != nil {
		var text string
		if json.Unmarshal(code, &text) != nil {
			return providers.ErrUpstreamUnavailable
		}
		parsed, err := strconv.Atoi(text)
		if err != nil {
			return providers.ErrUpstreamUnavailable
		}
		status = parsed
	}
	switch status {
	case http.StatusRequestEntityTooLarge:
		return providers.ErrRequestTooLarge
	case http.StatusTooManyRequests:
		return providers.ErrRateLimited
	case http.StatusPaymentRequired:
		return providers.ErrPaymentRequired
	default:
		return providers.ErrUpstreamUnavailable
	}
}

func (s *sseStream) streamError(kind error, message string) error {
	text := s.clean.Scrub(fmt.Sprintf("%s: upstream stream error: %s", s.name, message))
	limit := 512 - len(kind.Error()) - 2
	if len(text) > limit {
		text = text[:limit]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	return fmt.Errorf("%w: %s", kind, text)
}

func (s *sseStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.cancel()
	return s.body.Close()
}
