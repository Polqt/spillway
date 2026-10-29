// Package quota tracks temporary provider cooldowns for configured candidates.
package quota

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Polqt/spillway/internal/providers"
)

const (
	rateLimitBase = 15 * time.Second
	maxRateLimit  = 10 * time.Minute
	circuitBase   = 30 * time.Second
	maxCircuit    = 5 * time.Minute
	providerPause = 15 * time.Minute
	modelPause    = 15 * time.Minute
)

// Options configures a Tracker. Candidates bounds all tracked state.
type Options struct {
	Now          func() time.Time
	Candidates   []providers.ModelRef
	ServiceTypes map[string]string
	QuotaDomains map[string]map[string]string
}

type cooldown struct {
	until  time.Time
	reason string
}

type candidateState struct {
	cooldown
	rateFailures     int
	upstreamFailures int
}

// BreakerState is the current state of a candidate's circuit breaker.
type BreakerState string

const (
	// BreakerClosed means requests may pass the circuit breaker.
	BreakerClosed BreakerState = "closed"
	// BreakerOpen means the circuit breaker is cooling down after upstream failures.
	BreakerOpen BreakerState = "open"
)

// CandidateSnapshot describes the cooldown and circuit state of one candidate.
type CandidateSnapshot struct {
	Provider         string       `json:"provider"`                    // Candidate provider name.
	Model            string       `json:"model"`                       // Candidate model name.
	CooldownUntil    *time.Time   `json:"cooldown_until,omitempty"`    // Resume time when cooling down.
	Reason           string       `json:"reason,omitempty"`            // Fixed cooldown reason.
	Breaker          BreakerState `json:"breaker_state"`               // Current circuit breaker state.
	UpstreamFailures int          `json:"upstream_failures,omitempty"` // Circuit-counted upstream failures, capped at seven.
}

// Tracker tracks cooldowns for the configured model references only.
type Tracker struct {
	mu           sync.Mutex
	now          func() time.Time
	configured   map[providers.ModelRef]struct{}
	candidates   map[providers.ModelRef]candidateState
	serviceTypes map[string]string
	quotaDomains map[string]map[string]string
	services     map[string]cooldown
	domains      map[string]cooldown
	models       map[string]cooldown
	knownModels  map[string]struct{}
	instances    map[string]cooldown
	breakers     map[string]candidateState
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
	knownModels := make(map[string]struct{}, len(o.Candidates))
	serviceTypes := make(map[string]string, len(o.ServiceTypes))
	for instance, service := range o.ServiceTypes {
		serviceTypes[instance] = service
	}
	domains := make(map[string]map[string]string, len(o.QuotaDomains))
	for instance, labels := range o.QuotaDomains {
		domains[instance] = make(map[string]string, len(labels))
		for scope, label := range labels {
			domains[instance][scope] = label
		}
	}
	for _, ref := range o.Candidates {
		if ref.Provider == "" || ref.Model == "" {
			return nil, fmt.Errorf("quota: candidate %q must have a provider and model", ref.String())
		}
		configured[ref] = struct{}{}
		if serviceTypes[ref.Provider] == "" {
			serviceTypes[ref.Provider] = ref.Provider
		}
		knownModels[modelCooldownKey(serviceTypes[ref.Provider], ref.Model)] = struct{}{}
	}
	return &Tracker{
		now: o.Now, configured: configured, candidates: make(map[providers.ModelRef]candidateState, len(configured)),
		serviceTypes: serviceTypes, quotaDomains: domains,
		services: make(map[string]cooldown), domains: make(map[string]cooldown), models: make(map[string]cooldown), knownModels: knownModels,
		instances: make(map[string]cooldown), breakers: make(map[string]candidateState),
	}, nil
}

// Available reports whether ref may be tried now and, if not, its resume time and reason.
func (t *Tracker) Available(ref providers.ModelRef) (ok bool, until time.Time, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	service, knownInstance := t.serviceTypes[ref.Provider]
	if !knownInstance {
		return true, time.Time{}, ""
	}
	now := t.now()
	var sharedCooldown cooldown
	sharedCooldown = t.services[service]
	if modelCooldown := t.models[modelCooldownKey(service, ref.Model)]; modelCooldown.until.After(sharedCooldown.until) {
		sharedCooldown = modelCooldown
	}
	if instanceCooldown := t.instances[ref.Provider]; instanceCooldown.until.After(sharedCooldown.until) {
		sharedCooldown = instanceCooldown
	}
	for scope, label := range t.quotaDomains[ref.Provider] {
		if domainCooldown := t.domains[quotaDomainKey(service, scope, label)]; domainCooldown.until.After(sharedCooldown.until) {
			sharedCooldown = domainCooldown
		}
	}
	if now.Before(sharedCooldown.until) {
		return false, sharedCooldown.until, sharedCooldown.reason
	}
	if _, configured := t.configured[ref]; !configured {
		return true, time.Time{}, ""
	}
	state := t.candidates[ref]
	breaker := t.breakers[ref.Provider]
	if now.Before(breaker.until) && breaker.until.After(state.until) {
		return false, breaker.until, breaker.reason
	}
	if now.Before(state.until) {
		return false, state.until, state.reason
	}
	return true, time.Time{}, ""
}

// Snapshot returns a sorted, read-only view of configured candidates.
func (t *Tracker) Snapshot() []CandidateSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make([]CandidateSnapshot, 0, len(t.configured))
	now := t.now()
	for ref := range t.configured {
		state := t.candidates[ref]
		breakerState := t.breakers[ref.Provider]
		until, reason := state.until, state.reason
		if breakerState.until.After(until) {
			until, reason = breakerState.until, breakerState.reason
		}
		breaker := BreakerClosed
		if breakerState.upstreamFailures >= 3 && now.Before(breakerState.until) && breakerState.reason == "circuit open" {
			breaker = BreakerOpen
		}
		service := t.serviceTypes[ref.Provider]
		if shared, ok := t.services[service]; ok && shared.until.After(until) {
			until, reason = shared.until, shared.reason
		}
		if shared, ok := t.models[modelCooldownKey(service, ref.Model)]; ok && shared.until.After(until) {
			until, reason = shared.until, shared.reason
		}
		if instance, ok := t.instances[ref.Provider]; ok && instance.until.After(until) {
			until, reason = instance.until, instance.reason
		}
		for scope, label := range t.quotaDomains[ref.Provider] {
			if shared, ok := t.domains[quotaDomainKey(service, scope, label)]; ok && shared.until.After(until) {
				until, reason = shared.until, shared.reason
			}
		}
		if !now.Before(until) {
			until, reason = time.Time{}, ""
		}
		var cooldownUntil *time.Time
		if !until.IsZero() {
			cooldownUntil = &until
		}
		result = append(result, CandidateSnapshot{
			Provider: ref.Provider, Model: ref.Model, CooldownUntil: cooldownUntil,
			Reason: reason, Breaker: breaker, UpstreamFailures: breakerState.upstreamFailures,
		})
	}
	slices.SortFunc(result, func(a, b CandidateSnapshot) int {
		if a.Provider != b.Provider {
			return strings.Compare(a.Provider, b.Provider)
		}
		return strings.Compare(a.Model, b.Model)
	})
	return result
}

// Observe records an outcome for a configured candidate. Cancellation, request
// errors and request-size rejections leave the cooldown state unchanged.
func (t *Tracker) Observe(ref providers.ModelRef, err error, hint providers.RateLimit) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, knownInstance := t.serviceTypes[ref.Provider]
	if !knownInstance || errors.Is(err, context.Canceled) {
		return
	}
	_, configuredCandidate := t.configured[ref]
	now := t.now()
	if err == nil {
		breaker := t.breakers[ref.Provider]
		if breaker.reason == "circuit open" {
			t.breakers[ref.Provider] = candidateState{}
		}
		if !configuredCandidate {
			if hint.Known {
				reset := max(exhaustedReset(hint.RemainingRequests, hint.ResetRequests), exhaustedReset(hint.RemainingTokens, hint.ResetTokens))
				if reset > 0 {
					t.setRateCooldown(ref, "", cooldown{until: now.Add(reset), reason: "rate limited"})
				}
			}
			return
		}
		state := t.candidates[ref]
		state.rateFailures = 0
		state.cooldown = cooldown{}
		if hint.Known {
			reset := max(
				exhaustedReset(hint.RemainingRequests, hint.ResetRequests),
				exhaustedReset(hint.RemainingTokens, hint.ResetTokens),
			)
			if reset > 0 {
				t.setRateCooldown(ref, "", cooldown{until: now.Add(reset), reason: "rate limited"})
			}
		}
		t.candidates[ref] = state
		return
	}
	var retry providers.RetryAfterError
	switch {
	case errors.Is(err, providers.ErrRateLimited):
		failures := 1
		state := t.candidates[ref]
		if configuredCandidate {
			state.rateFailures++
			failures = state.rateFailures
		}
		duration := rateLimitBackoff(failures)
		if errors.As(err, &retry) && retry.After > 0 {
			duration = providers.ClampRetryAfter(retry.After)
		}
		t.setRateCooldown(ref, retry.Scope, cooldown{until: now.Add(duration), reason: "rate limited"})
		if configuredCandidate {
			t.candidates[ref] = state
		}
	case errors.Is(err, providers.ErrPaymentRequired):
		duration := providerPause
		if errors.As(err, &retry) && retry.After > 0 {
			duration = providers.ClampRetryAfter(retry.After)
		}
		t.setServiceCooldown(ref.Provider, cooldown{until: now.Add(duration), reason: "payment required"})
	case errors.Is(err, providers.ErrUnauthorized):
		t.instances[ref.Provider] = laterCooldown(t.instances[ref.Provider], cooldown{until: now.Add(providerPause), reason: "unauthorized"})
	case errors.Is(err, providers.ErrModelNotFound):
		if !configuredCandidate {
			return
		}
		state := t.candidates[ref]
		state.cooldown = laterCooldown(state.cooldown, cooldown{until: now.Add(modelPause), reason: "model not found"})
		t.candidates[ref] = state
	case errors.Is(err, providers.ErrUpstreamUnavailable):
		state := t.breakers[ref.Provider]
		if state.upstreamFailures < 7 {
			state.upstreamFailures++
		}
		var retry providers.RetryAfterError
		if errors.As(err, &retry) && retry.After > 0 {
			state.cooldown = laterCooldown(state.cooldown, cooldown{until: now.Add(providers.ClampRetryAfter(retry.After)), reason: "circuit open"})
		} else if state.upstreamFailures >= 3 {
			state.cooldown = laterCooldown(state.cooldown, cooldown{until: now.Add(circuitBackoff(state.upstreamFailures)), reason: "circuit open"})
		}
		t.breakers[ref.Provider] = state
	}
}

func (t *Tracker) setServiceCooldown(instance string, next cooldown) {
	service := t.serviceTypes[instance]
	if service == "" {
		service = instance
	}
	t.services[service] = laterCooldown(t.services[service], next)
}

func (t *Tracker) setRateCooldown(ref providers.ModelRef, scope string, next cooldown) {
	service := t.serviceTypes[ref.Provider]
	if scope == "model" {
		key := modelCooldownKey(service, ref.Model)
		if _, known := t.knownModels[key]; known {
			t.models[key] = laterCooldown(t.models[key], next)
			return
		}
	}
	label := t.quotaDomains[ref.Provider][scope]
	if scope == "" || label == "" {
		// An unknown upstream scope must suppress every configured instance of
		// the service instead of treating another credential as capacity.
		next.reason = "rate limited; quota scope unknown, conservatively shared"
		t.setServiceCooldown(ref.Provider, next)
		return
	}
	for instance, instanceService := range t.serviceTypes {
		if instanceService == service && t.quotaDomains[instance][scope] == "" {
			// An unlabelled sibling cannot be safely separated from this scope.
			next.reason = "rate limited; quota scope unknown, conservatively shared"
			t.setServiceCooldown(ref.Provider, next)
			return
		}
	}
	key := quotaDomainKey(service, scope, label)
	t.domains[key] = laterCooldown(t.domains[key], next)
}

func modelCooldownKey(service, model string) string {
	return service + "/model/" + model
}

func quotaDomainKey(service, scope, label string) string {
	return service + "/" + scope + "/" + label
}

func exhaustedReset(remaining int, reset time.Duration) time.Duration {
	if remaining != 0 || reset <= 0 {
		return 0
	}
	return providers.ClampRetryAfter(reset)
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

func circuitBackoff(failures int) time.Duration {
	duration := circuitBase
	for range failures - 3 {
		if duration >= maxCircuit/2 {
			return maxCircuit
		}
		duration *= 2
	}
	if duration > maxCircuit {
		return maxCircuit
	}
	return duration
}
