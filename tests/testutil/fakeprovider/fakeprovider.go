// Package fakeprovider provides deterministic test implementations.
package fakeprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Polqt/spillway/internal/providers"
)

// Options configures the test provider.
type Options struct {
	Name string
	// Now supplies the created timestamp of every response.
	Now func() time.Time
}

// Provider is a deterministic provider used by tests.
type Provider struct {
	name string
	now  func() time.Time
}

// New returns the test provider.
func New(o Options) (*Provider, error) {
	if o.Now == nil {
		return nil, errors.New("fake provider: Options.Now is required")
	}
	if o.Name == "" {
		o.Name = "mock"
	}
	return &Provider{name: o.Name, now: o.Now}, nil
}

// Name returns the configured provider name.
func (p *Provider) Name() string { return p.name }

// Models lists the test provider's supported models.
func (p *Provider) Models(ctx context.Context) ([]providers.Model, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []providers.Model{
		{ID: modelEcho},
		{ID: modelRateLimited},
		{ID: modelStreamError},
		{ID: modelUnauthorized},
		{ID: modelUnavailable},
	}, nil
}

// Chat answers for the upstream model "echo"; "stream-error" is
// providers.ErrUpstreamUnavailable and any other model is
// providers.ErrModelNotFound.
func (p *Provider) Chat(ctx context.Context, req providers.ChatRequest) (providers.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		// Unwrapped, like every provider, so the API layer can tell a client
		// disconnect from an upstream failure.
		return providers.ChatResponse{}, err
	}
	if err := checkModel(req.Model); err != nil {
		return providers.ChatResponse{}, err
	}
	if req.Model == modelStreamError {
		return providers.ChatResponse{}, modelError(req.Model, providers.ErrUpstreamUnavailable)
	}
	if err := simulatedError(req.Model); err != nil {
		return providers.ChatResponse{}, err
	}
	rep, err := p.reply(req)
	if err != nil {
		return providers.ChatResponse{}, err
	}
	return providers.NewChatResponse(providers.Completion{
		ID:           rep.id,
		Model:        req.Model,
		Content:      strings.Join(rep.words, " "),
		FinishReason: "stop",
		Created:      rep.created,
		Usage:        rep.usage,
	})
}

// ChatStream streams the "echo" reply one word per chunk, then a finish
// chunk with usage. "stream-error" streams the first two word chunks, then
// fails with providers.ErrUpstreamUnavailable.
func (p *Provider) ChatStream(ctx context.Context, req providers.ChatRequest) (providers.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := checkModel(req.Model); err != nil {
		return nil, err
	}
	if req.Model != modelStreamError {
		if err := simulatedError(req.Model); err != nil {
			return nil, err
		}
	}
	rep, err := p.reply(req)
	if err != nil {
		return nil, err
	}
	deltas := make([]providers.Delta, 0, len(rep.words)+1)
	for i, w := range rep.words {
		d := providers.Delta{Content: " " + w}
		if i == 0 {
			d = providers.Delta{Role: "assistant", Content: w}
		}
		deltas = append(deltas, d)
	}
	deltas = append(deltas, providers.Delta{FinishReason: "stop", Usage: &rep.usage})

	end := io.EOF
	if req.Model == modelStreamError {
		deltas = deltas[:2]
		end = fmt.Errorf("fake provider: upstream model %q failed mid-stream: %w", req.Model, providers.ErrUpstreamUnavailable)
	}
	chunks := make([]providers.ChatChunk, len(deltas))
	for i, d := range deltas {
		d.ID, d.Model, d.Created = rep.id, req.Model, rep.created
		if chunks[i], err = providers.NewChatChunk(d); err != nil {
			return nil, err
		}
	}
	return &stream{ctx: ctx, chunks: chunks, end: end}, nil
}

// stream replays precomputed chunks. It does no I/O and owns no goroutine.
type stream struct {
	ctx    context.Context
	chunks []providers.ChatChunk
	end    error // returned once the chunks are exhausted
	closed bool
}

func (s *stream) Recv() (providers.ChatChunk, error) {
	if s.closed {
		return providers.ChatChunk{}, errors.New("fake provider: stream is closed")
	}
	if err := s.ctx.Err(); err != nil {
		return providers.ChatChunk{}, err
	}
	if len(s.chunks) == 0 {
		return providers.ChatChunk{}, s.end
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return c, nil
}

func (s *stream) Close() error {
	s.closed = true
	return nil
}

const (
	modelEcho         = "echo"
	modelRateLimited  = "rate-limited"
	modelStreamError  = "stream-error"
	modelUnauthorized = "unauthorized"
	modelUnavailable  = "unavailable"
)

func checkModel(model string) error {
	switch model {
	case modelEcho, modelRateLimited, modelStreamError, modelUnauthorized, modelUnavailable:
	default:
		return modelError(model, providers.ErrModelNotFound)
	}
	return nil
}

func simulatedError(model string) error {
	switch model {
	case modelRateLimited:
		return fmt.Errorf("fake provider: upstream model %q: %w", model, providers.RetryAfterError{After: 7 * time.Second, Kind: providers.ErrRateLimited, Scope: "model"})
	case modelUnauthorized:
		return modelError(model, providers.ErrUnauthorized)
	case modelUnavailable:
		return modelError(model, providers.ErrUpstreamUnavailable)
	default:
		return nil
	}
}

func modelError(model string, kind error) error {
	return fmt.Errorf("fake provider: upstream model %q: %w", model, kind)
}

// reply creates a deterministic response before shaping it for the API.
// or as chunks.
type reply struct {
	id      string
	created int64
	words   []string
	usage   providers.Usage
}

func (p *Provider) reply(req providers.ChatRequest) (reply, error) {
	id, err := requestID(req)
	if err != nil {
		return reply{}, err
	}
	words := strings.Fields("You said: " + lastUserText(req.Messages))
	prompt := 0
	for _, m := range req.Messages {
		prompt += len(strings.Fields(m.Text()))
	}
	return reply{
		id:      id,
		created: p.now().Unix(),
		words:   words,
		usage:   providers.Usage{PromptTokens: prompt, CompletionTokens: len(words), TotalTokens: prompt + len(words)},
	}, nil
}

// requestID derives the response id from the request, so identical requests
// get identical responses.
func requestID(req providers.ChatRequest) (string, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("fake provider: encode request: %w", err)
	}
	sum := sha256.Sum256(b)
	return "chatcmpl-test-" + hex.EncodeToString(sum[:])[:12], nil
}

func lastUserText(msgs []providers.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Text()
		}
	}
	return ""
}
