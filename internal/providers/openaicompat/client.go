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
}

// Client implements providers.Provider using the OpenAI-compatible API.
type Client struct {
	name          string
	baseURL       string
	key           redact.Secret
	timeout       time.Duration
	http          *http.Client
	clean         *redact.Scrubber
	classifyError func(status int, body []byte) (error, bool)
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
	body, err := requestBody(chat)
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
	body, err := requestBody(chat)
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
	if c.classifyError != nil {
		if kind, ok := c.classifyError(resp.StatusCode, body); ok {
			message := fmt.Errorf("%s: upstream status %d: %s", c.name, resp.StatusCode, c.clean.Scrub(errorMessage(body)))
			if readErr != nil {
				message = fmt.Errorf("%w: read upstream error response: %w", message, c.clean.Error(readErr))
			}
			return body, fmt.Errorf("%w: %w", kind, message)
		}
	}
	detail := c.clean.Scrub(errorMessage(body))
	if detail == "" {
		detail = http.StatusText(resp.StatusCode)
	}
	message := fmt.Errorf("%s: upstream status %d: %s", c.name, resp.StatusCode, detail)
	if readErr != nil {
		message = fmt.Errorf("%w: read upstream error response: %w", message, c.clean.Error(readErr))
	}
	switch {
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

func errorMessage(body []byte) string {
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
			message = strings.TrimSpace(message)
			if len(message) > 512 {
				message = message[:512]
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

func requestBody(chat providers.ChatRequest) ([]byte, error) {
	data := chat.RawBody
	if len(chat.RawBody) == 0 {
		var err error
		data, err = json.Marshal(chat)
		if err != nil {
			return nil, err
		}
	}
	model, err := json.Marshal(chat.Model)
	if err != nil {
		return nil, err
	}
	return replaceModel(data, model)
}

func replaceModel(data, model []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("chat request must be a JSON object")
	}
	type span struct{ start, end int }
	var spans []span
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("chat request contains an invalid field")
		}
		keyEnd := int(dec.InputOffset())
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		valueEnd := int(dec.InputOffset())
		if key == "model" {
			start := keyEnd
			for start < valueEnd && (data[start] == ' ' || data[start] == '\n' || data[start] == '\r' || data[start] == '\t' || data[start] == ':') {
				start++
			}
			spans = append(spans, span{start: start, end: valueEnd})
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if len(spans) == 0 {
		return nil, errors.New("chat request is missing model")
	}
	var out bytes.Buffer
	last := 0
	for _, part := range spans {
		out.Write(data[last:part.start])
		out.Write(model)
		last = part.end
	}
	out.Write(data[last:])
	return out.Bytes(), nil
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
			return providers.ChatChunk{}, s.streamError(streamErrorKind(upstreamError.Code), errorMessage(data))
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
