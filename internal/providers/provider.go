// Package providers defines the Provider interface every adapter implements,
// the OpenAI-format types they exchange, and the registry that holds them.
package providers

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Provider is one configured upstream endpoint that serves models.
type Provider interface {
	// Name is the provider part of a model reference, such as "mock".
	Name() string
	// Chat sends a non-streaming chat completion for the upstream model in req.
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
	// ChatStream starts a streaming chat completion. Errors the provider
	// reports before the first chunk are returned here, so the caller can
	// still answer with a plain HTTP error.
	ChatStream(ctx context.Context, req ChatRequest) (Stream, error)
}

// Stream is a pull-based stream of chat completion chunks. An implementation
// owns no goroutine: Recv does its work on the caller's goroutine.
type Stream interface {
	// Recv returns the next chunk, io.EOF after the last one, or any other
	// error, which ends the stream. A caller whose ctx was cancelled gets
	// context.Canceled unwrapped.
	Recv() (ChatChunk, error)
	// Close releases the stream. It is safe to call twice and after io.EOF.
	Close() error
}

var (
	// ErrModelNotFound means the provider does not serve the requested upstream model.
	ErrModelNotFound = errors.New("model not found")
	// ErrUpstreamUnavailable means the provider failed or could not be reached.
	ErrUpstreamUnavailable = errors.New("provider unavailable")
)

// ModelRef is a model reference: a provider name and an upstream model.
type ModelRef struct {
	Provider string
	Model    string
}

// ParseModelRef splits s at its first "/". The upstream model may itself
// contain "/" or ":".
func ParseModelRef(s string) (ModelRef, error) {
	provider, model, ok := strings.Cut(s, "/")
	if !ok || provider == "" || model == "" {
		return ModelRef{}, fmt.Errorf("model reference %q must be provider/model", s)
	}
	return ModelRef{Provider: provider, Model: model}, nil
}

// String returns "provider/model".
func (r ModelRef) String() string {
	return r.Provider + "/" + r.Model
}

// Set holds the configured providers by name. It is immutable after NewSet.
type Set struct {
	byName map[string]Provider
}

// NewSet builds a registry. It rejects an empty set and duplicate names.
func NewSet(ps ...Provider) (*Set, error) {
	if len(ps) == 0 {
		return nil, errors.New("providers: at least one provider is required")
	}
	byName := make(map[string]Provider, len(ps))
	for _, p := range ps {
		if _, dup := byName[p.Name()]; dup {
			return nil, fmt.Errorf("providers: duplicate provider %q", p.Name())
		}
		byName[p.Name()] = p
	}
	return &Set{byName: byName}, nil
}

// Lookup returns the provider with the given name.
func (s *Set) Lookup(name string) (Provider, bool) {
	p, ok := s.byName[name]
	return p, ok
}
