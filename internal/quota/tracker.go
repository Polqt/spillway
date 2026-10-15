// Package quota tracks temporary provider cooldowns for configured candidates.
package quota

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Polqt/spillway/internal/providers"
)

const (
	rateLimitBase = 15 * time.Second
	maxRateLimit  = 10 * time.Minute
	providerPause = 15 * time.Minute
	modelPause    = 15 * time.Minute
)

// Options configures a Tracker. Candidates bounds all tracked state.
type Options struct {
	Now        func() time.Time
	Candidates []providers.ModelRef
}

type cooldown struct {
	until  time.Time
	reason string
}

type candidateState struct {
	cooldown
	rateFailures int
}

// Tracker tracks cooldowns for the configured model references only.
type Tracker struct {
	mu         sync.Mutex
	now        func() time.Time
	configured map[providers.ModelRef]struct{}
	candidates map[providers.ModelRef]candidateState
	providers  map[string]cooldown
}

// New creates a tracker bounded to the configured candidates.
func New(o Options) (*Tracker, error) {
	if len(o.Candidates) == 0 {
		return nil, errors.New("quota: at least one candidate is required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	configured := make(map[providers.ModelRef]struct{}, len(o.Candidates))
	for _, ref := range o.Candidates {
		if ref.Provider == "" || ref.Model == "" {
			return nil, fmt.Errorf("quota: candidate %q must have a provider and model", ref.String())
		}
		configured[ref] = struct{}{}
	}
	return &Tracker{
		now:        o.Now,
		configured: configured,
		candidates: make(map[providers.ModelRef]candidateState, len(configured)),
		providers:  make(map[string]cooldown),
	}, nil
}

// Available reports whether ref may be tried now and, if not, its resume time and reason.
func (t *Tracker) Available(ref providers.ModelRef) (ok bool, until time.Time, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, configured := t.configured[ref]; !configured {
		return true, time.Time{}, ""
	}
	now := t.now()
	if providerCooldown, exists := t.providers[ref.Provider]; exists && now.Before(providerCooldown.until) {
		return false, providerCooldown.until, providerCooldown.reason
	}
	state := t.candidates[ref]
	if now.Before(state.until) {
		return false, state.until, state.reason
	}
	return true, time.Time{}, ""
}

// Observe records an outcome for a configured candidate. Cancellation and
// request errors leave the cooldown state unchanged.
func (t *Tracker) Observe(ref providers.ModelRef, err error, _ providers.RateLimit) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, configured := t.configured[ref]; !configured || errors.Is(err, context.Canceled) {
		return
	}
	now := t.now()
	if err == nil {
		state := t.candidates[ref]
		state.rateFailures = 0
		state.cooldown = cooldown{}
		t.candidates[ref] = state
		return
	}
	var retry providers.RetryAfterError
	switch {
	case errors.Is(err, providers.ErrRateLimited):
		state := t.candidates[ref]
		state.rateFailures++
		duration := rateLimitBackoff(state.rateFailures)
		if errors.As(err, &retry) && retry.After > 0 {
			duration = providers.ClampRetryAfter(retry.After)
		}
		state.cooldown = laterCooldown(state.cooldown, cooldown{until: now.Add(duration), reason: "rate limited"})
		t.candidates[ref] = state
	case errors.Is(err, providers.ErrPaymentRequired):
		duration := providerPause
		if errors.As(err, &retry) && retry.After > 0 {
			duration = providers.ClampRetryAfter(retry.After)
		}
		t.providers[ref.Provider] = laterCooldown(t.providers[ref.Provider], cooldown{until: now.Add(duration), reason: "payment required"})
	case errors.Is(err, providers.ErrUnauthorized):
		t.providers[ref.Provider] = laterCooldown(t.providers[ref.Provider], cooldown{until: now.Add(providerPause), reason: "unauthorized"})
	case errors.Is(err, providers.ErrModelNotFound):
		state := t.candidates[ref]
		state.cooldown = laterCooldown(state.cooldown, cooldown{until: now.Add(modelPause), reason: "model not found"})
		t.candidates[ref] = state
	}
}

func laterCooldown(current, next cooldown) cooldown {
	if current.until.After(next.until) {
		return current
	}
	return next
}

func rateLimitBackoff(failures int) time.Duration {
	duration := rateLimitBase
	for range failures - 1 {
		if duration >= maxRateLimit/2 {
			return maxRateLimit
		}
		duration *= 2
	}
	if duration > maxRateLimit {
		return maxRateLimit
	}
	return duration
}
