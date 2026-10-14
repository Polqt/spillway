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
		return providers.ChatResponse{}, c.responseError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return providers.ChatResponse{}, c.clean.Error(fmt.Errorf("%s: read chat response: %w", c.name, err))
	}
	if len(data) > maxResponseBytes {
		return providers.ChatResponse{}, fmt.Errorf("%s: chat response exceeds limit", c.name)
	}
	return providers.ChatResponse{Body: data}, nil
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
	return &sseStream{body: resp.Body, cancel: cancel, scanner: newScanner(resp.Body), clean: c.clean, name: c.name, idleTimeout: c.timeout}, nil
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
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes+int64(len(c.key.Reveal()))))
	if c.classifyError != nil {
		if kind, ok := c.classifyError(resp.StatusCode, body); ok {
			message := fmt.Errorf("%s: upstream status %d: %s", c.name, resp.StatusCode, c.clean.Scrub(errorMessage(body)))
			if readErr != nil {
				message = fmt.Errorf("%w: read upstream error response: %w", message, c.clean.Error(readErr))
			}
			return fmt.Errorf("%w: %w", kind, message)
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
	case resp.StatusCode == http.StatusTooManyRequests:
		after := retryAfter(resp.Header.Get("Retry-After"), time.Now())
		return fmt.Errorf("%w: %w", providers.RetryAfterError{After: after, Kind: providers.ErrRateLimited}, message)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %w", providers.ErrUnauthorized, message)
	case resp.StatusCode == http.StatusBadRequest:
		return fmt.Errorf("%w: %w", providers.ErrBadRequest, message)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %w", providers.ErrModelNotFound, message)
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: %w", providers.ErrUpstreamUnavailable, message)
	default:
		return fmt.Errorf("%w: %w", providers.ErrBadRequest, message)
	}
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
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

func requestBody(chat providers.ChatRequest) ([]byte, error) {
	data := chat.RawBody
	if len(chat.RawBody) != 0 {
	} else {
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
	idleTimedOut atomic.Bool
	closed       bool
}

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
			return providers.ChatChunk{}, io.EOF
		}
		var chunk struct {
			Usage *providers.Usage `json:"usage"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return providers.ChatChunk{}, fmt.Errorf("%s: decode stream chunk: %w", s.name, err)
		}
		return providers.ChatChunk{Data: append(json.RawMessage(nil), data...), Usage: chunk.Usage}, nil
	}
	if err := s.scanner.Err(); err != nil {
		if s.idleTimedOut.Load() {
			return providers.ChatChunk{}, fmt.Errorf("%w: no stream data for %s", providers.ErrUpstreamUnavailable, s.idleTimeout)
		}
		return providers.ChatChunk{}, s.clean.Error(fmt.Errorf("%s: read stream: %w", s.name, err))
	}
	return providers.ChatChunk{}, io.EOF
}

func (s *sseStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.cancel()
	return s.body.Close()
}
