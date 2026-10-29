package quota_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/quota"
)

func TestTracker_RetryAfterAndBackoffExpireAtExpectedTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ref := providers.ModelRef{Provider: "groq", Model: "one"}
		tracker, err := quota.New(quota.Options{Now: time.Now, Candidates: []providers.ModelRef{ref}})
		if err != nil {
			t.Fatal(err)
		}
		tracker.Observe(ref, providers.RetryAfterError{After: 34 * time.Second, Kind: providers.ErrRateLimited}, providers.RateLimit{})
		if available, until, reason := tracker.Available(ref); available || !until.Equal(time.Now().Add(34*time.Second)) || reason != "rate limited; quota scope unknown, conservatively shared" {
			t.Fatalf("Available() = %v, %v, %q; want false, 34s resume, rate limited", available, until, reason)
		}
		<-time.After(33 * time.Second)
		if available, _, _ := tracker.Available(ref); available {
			t.Fatal("candidate available at 33s, want cooling")
		}
		<-time.After(time.Second)
		if available, _, _ := tracker.Available(ref); !available {
			t.Fatal("candidate unavailable at 34s, want available")
		}
	})

	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	ref := providers.ModelRef{Provider: "groq", Model: "one"}
	tracker, err := quota.New(quota.Options{Now: func() time.Time { return now }, Candidates: []providers.ModelRef{ref}})
	if err != nil {
		t.Fatal(err)
	}

	tracker.Observe(ref, nil, providers.RateLimit{})
	tracker.Observe(ref, providers.ErrRateLimited, providers.RateLimit{})
	if _, until, _ := tracker.Available(ref); !until.Equal(now.Add(15 * time.Second)) {
		t.Fatalf("first no-hint cooldown ends at %v, want 15s", until)
	}
	now = now.Add(15 * time.Second)
	tracker.Observe(ref, providers.ErrRateLimited, providers.RateLimit{})
	if _, until, _ := tracker.Available(ref); !until.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("second no-hint cooldown ends at %v, want 30s", until)
	}
	now = now.Add(30 * time.Second)
	tracker.Observe(ref, providers.ErrRateLimited, providers.RateLimit{})
	if _, until, _ := tracker.Available(ref); !until.Equal(now.Add(60 * time.Second)) {
		t.Fatalf("third no-hint cooldown ends at %v, want 60s", until)
	}
	now = now.Add(60 * time.Second)
	tracker.Observe(ref, nil, providers.RateLimit{})
	if available, _, _ := tracker.Available(ref); !available {
		t.Fatal("success did not reset rate-limit cooldown")
	}
	tracker.Observe(ref, providers.ErrRateLimited, providers.RateLimit{})
	if _, until, _ := tracker.Available(ref); !until.Equal(now.Add(15 * time.Second)) {
		t.Fatalf("cooldown after success ends at %v, want 15s", until)
	}
}

func TestTracker_UnknownScopeBlocksSiblingInstancesOfSameService(t *testing.T) {
	t.Parallel()
	one := providers.ModelRef{Provider: "gemini-research", Model: "flash"}
	two := providers.ModelRef{Provider: "gemini-staging", Model: "flash"}
	tracker, err := quota.New(quota.Options{
		Now:          func() time.Time { return time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC) },
		Candidates:   []providers.ModelRef{one, two},
		ServiceTypes: map[string]string{"gemini-research": "gemini", "gemini-staging": "gemini"},
		QuotaDomains: map[string]map[string]string{"gemini-research": {"project": "research"}, "gemini-staging": {"project": "staging"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tracker.Observe(one, providers.ErrRateLimited, providers.RateLimit{})
	if available, _, reason := tracker.Available(two); available || reason != "rate limited; quota scope unknown, conservatively shared" {
		t.Fatalf("sibling Available() = %v, %q; unknown scope must block service-wide", available, reason)
	}
}

func TestTracker_KnownModelScopeDoesNotBlockOtherModels(t *testing.T) {
	t.Parallel()
	one := providers.ModelRef{Provider: "mock", Model: "rate-limited"}
	two := providers.ModelRef{Provider: "mock", Model: "echo"}
	tracker, err := quota.New(quota.Options{
		Now:        func() time.Time { return time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC) },
		Candidates: []providers.ModelRef{one, two}, ServiceTypes: map[string]string{"mock": "mock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tracker.Observe(one, providers.RetryAfterError{After: time.Minute, Kind: providers.ErrRateLimited, Scope: "model"}, providers.RateLimit{})
	if available, _, reason := tracker.Available(one); available || reason != "rate limited" {
		t.Fatalf("limited model Available() = %v, %q; want shared model cooldown", available, reason)
	}
	if available, _, _ := tracker.Available(two); !available {
		t.Fatal("different model was blocked by a known model-scoped limit")
	}
}

func TestTracker_KnownQuotaScopeSharesOnlyMatchingDomain(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	one := providers.ModelRef{Provider: "gemini-research", Model: "flash"}
	same := providers.ModelRef{Provider: "gemini-research-copy", Model: "flash"}
	other := providers.ModelRef{Provider: "gemini-staging", Model: "flash"}
	tracker, err := quota.New(quota.Options{
		Now: func() time.Time { return now }, Candidates: []providers.ModelRef{one, same, other},
		ServiceTypes: map[string]string{"gemini-research": "gemini", "gemini-research-copy": "gemini", "gemini-staging": "gemini"},
		QuotaDomains: map[string]map[string]string{
			"gemini-research": {"project": "research"}, "gemini-research-copy": {"project": "research"}, "gemini-staging": {"project": "staging"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tracker.Observe(one, providers.RetryAfterError{After: time.Minute, Kind: providers.ErrRateLimited, Scope: "project"}, providers.RateLimit{})
	if available, _, reason := tracker.Available(same); available || reason != "rate limited" {
		t.Fatalf("same domain Available() = %v, %q; want shared cooldown", available, reason)
	}
	if available, _, _ := tracker.Available(other); !available {
		t.Fatal("unrelated verified project domain was blocked")
	}
}

func TestTracker_GroqQuotaHintsCoolUntilMatchingReset(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		hint     providers.RateLimit
		cooldown time.Duration
	}{
		{
			name:     "requests",
			hint:     providers.RateLimit{Known: true, RemainingRequests: 0, ResetRequests: 2*time.Minute + 59*time.Second + 560*time.Millisecond},
			cooldown: 2*time.Minute + 59*time.Second + 560*time.Millisecond,
		},
		{
			name:     "tokens",
			hint:     providers.RateLimit{Known: true, RemainingTokens: 0, ResetTokens: 7*time.Second + 660*time.Millisecond},
			cooldown: 7*time.Second + 660*time.Millisecond,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ref := providers.ModelRef{Provider: "groq", Model: "llama-3.1-8b-instant"}
				tracker, err := quota.New(quota.Options{Now: time.Now, Candidates: []providers.ModelRef{ref}})
				if err != nil {
					t.Fatal(err)
				}
				tracker.Observe(ref, nil, tt.hint)
				if available, until, _ := tracker.Available(ref); available || !until.Equal(time.Now().Add(tt.cooldown)) {
					t.Fatalf("Available() = %v, %v; want cooldown for %s", available, until, tt.cooldown)
				}
				<-time.After(tt.cooldown - time.Nanosecond)
				if available, _, _ := tracker.Available(ref); available {
					t.Fatal("candidate available before quota reset")
				}
				<-time.After(time.Nanosecond)
				if available, _, _ := tracker.Available(ref); !available {
					t.Fatal("candidate unavailable at quota reset")
				}
			})
		})
	}
}

func TestTracker_GroqQuotaHintsIgnoreUsableOrUnknownLimits(t *testing.T) {
	t.Parallel()
	ref := providers.ModelRef{Provider: "groq", Model: "llama-3.1-8b-instant"}
	tracker, err := quota.New(quota.Options{Candidates: []providers.ModelRef{ref}})
	if err != nil {
		t.Fatal(err)
	}
	for _, hint := range []providers.RateLimit{
		{},
		{Known: true, RemainingRequests: 1, ResetRequests: time.Minute},
		{Known: true, RemainingTokens: 1, ResetTokens: time.Minute},
		{Known: true, RemainingRequests: 0},
	} {
		tracker.Observe(ref, nil, hint)
		if available, _, _ := tracker.Available(ref); !available {
			t.Fatalf("hint %+v cooled a candidate without a usable exhausted reset", hint)
		}
	}
}

func TestTracker_GroqQuotaHintsClampResetTo24Hours(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	ref := providers.ModelRef{Provider: "groq", Model: "llama-3.1-8b-instant"}
	tracker, err := quota.New(quota.Options{Now: func() time.Time { return now }, Candidates: []providers.ModelRef{ref}})
	if err != nil {
		t.Fatal(err)
	}
	tracker.Observe(ref, nil, providers.RateLimit{Known: true, RemainingRequests: 0, ResetRequests: 48 * time.Hour})
	if available, until, _ := tracker.Available(ref); available || !until.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("Available() = %v, %v; want 24h clamped cooldown", available, until)
	}
}

func TestTracker_ProviderFailuresShareCooldownAcrossConfiguredCandidates(t *testing.T) {
	now := time.Now()
	groqOne := providers.ModelRef{Provider: "groq", Model: "one"}
	groqTwo := providers.ModelRef{Provider: "groq", Model: "two"}
	gemini := providers.ModelRef{Provider: "gemini", Model: "one"}
	tracker, err := quota.New(quota.Options{Now: func() time.Time { return now }, Candidates: []providers.ModelRef{groqOne, groqTwo, gemini}})
	if err != nil {
		t.Fatal(err)
	}
	tracker.Observe(groqOne, providers.RetryAfterError{After: 10 * time.Second, Kind: providers.ErrPaymentRequired}, providers.RateLimit{})
	for _, ref := range []providers.ModelRef{groqOne, groqTwo} {
		if available, until, reason := tracker.Available(ref); available || !until.Equal(now.Add(10*time.Second)) || reason != "payment required" {
			t.Errorf("Available(%s) = %v, %v, %q", ref, available, until, reason)
		}
	}
	if available, _, _ := tracker.Available(gemini); !available {
		t.Fatal("payment cooldown affected another provider")
	}

	tracker.Observe(groqOne, providers.ErrUnauthorized, providers.RateLimit{})
	if available, _, reason := tracker.Available(groqTwo); available || reason != "unauthorized" {
		t.Fatalf("provider key rejection did not cool sibling: %v, %q", available, reason)
	}
}

func TestTracker_ModelNotFoundAndIgnoredErrors(t *testing.T) {
	now := time.Now()
	ref := providers.ModelRef{Provider: "gemini", Model: "gone"}
	tracker, err := quota.New(quota.Options{Now: func() time.Time { return now }, Candidates: []providers.ModelRef{ref}})
	if err != nil {
		t.Fatal(err)
	}
	tracker.Observe(ref, providers.ErrModelNotFound, providers.RateLimit{})
	if available, until, reason := tracker.Available(ref); available || !until.Equal(now.Add(15*time.Minute)) || reason != "model not found" {
		t.Fatalf("404 cooldown = %v, %v, %q", available, until, reason)
	}
	now = now.Add(15 * time.Minute)
	if available, _, _ := tracker.Available(ref); !available {
		t.Fatal("candidate unavailable after 15m")
	}
	tracker.Observe(ref, providers.ErrBadRequest, providers.RateLimit{})
	tracker.Observe(ref, context.Canceled, providers.RateLimit{})
	if available, _, _ := tracker.Available(ref); !available {
		t.Fatal("ignored error changed candidate state")
	}
	unknown := providers.ModelRef{Provider: "client-input", Model: "unbounded"}
	tracker.Observe(unknown, providers.ErrUnauthorized, providers.RateLimit{})
	if available, _, _ := tracker.Available(unknown); !available {
		t.Fatal("unconfigured reference was tracked")
	}
}

func TestTracker_ClampsRetryTimeAndBoundsFallbackBackoff(t *testing.T) {
	now := time.Now()
	ref := providers.ModelRef{Provider: "groq", Model: "one"}
	tracker, err := quota.New(quota.Options{Now: func() time.Time { return now }, Candidates: []providers.ModelRef{ref}})
	if err != nil {
		t.Fatal(err)
	}
	tracker.Observe(ref, providers.RetryAfterError{After: 999999999 * time.Second, Kind: providers.ErrRateLimited}, providers.RateLimit{})
	if _, until, _ := tracker.Available(ref); !until.Equal(now.Add(24 * time.Hour)) {
		t.Fatalf("retry clamp = %v, want 24h", until)
	}
	now = now.Add(24 * time.Hour)
	tracker.Observe(ref, nil, providers.RateLimit{})
	for i := range 7 {
		tracker.Observe(ref, providers.ErrRateLimited, providers.RateLimit{})
		if i < 6 {
			now = now.Add(10 * time.Minute)
		}
	}
	if _, until, _ := tracker.Available(ref); !until.Equal(now.Add(10 * time.Minute)) {
		t.Fatalf("backoff cap ends at %v, want 10m", until)
	}
}

func TestTracker_CircuitBreakerThresholdBackoffAndReset(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ref := providers.ModelRef{Provider: "mock", Model: "unavailable"}
		tracker, err := quota.New(quota.Options{Now: time.Now, Candidates: []providers.ModelRef{ref}})
		if err != nil {
			t.Fatal(err)
		}

		for range 2 {
			tracker.Observe(ref, providers.ErrUpstreamUnavailable, providers.RateLimit{})
			if available, _, _ := tracker.Available(ref); !available {
				t.Fatal("breaker opened before the third failure")
			}
		}
		tracker.Observe(ref, providers.ErrUpstreamUnavailable, providers.RateLimit{})
		assertCircuit(t, tracker, ref, 30*time.Second)

		<-time.After(30 * time.Second)
		if available, _, _ := tracker.Available(ref); !available {
			t.Fatal("candidate unavailable after 30s circuit expiry")
		}
		tracker.Observe(ref, fmt.Errorf("attempt timed out: %w", providers.ErrUpstreamUnavailable), providers.RateLimit{})
		assertCircuit(t, tracker, ref, 60*time.Second)

		<-time.After(60 * time.Second)
		tracker.Observe(ref, nil, providers.RateLimit{})
		if available, _, _ := tracker.Available(ref); !available {
			t.Fatal("success did not clear the circuit")
		}
		for range 3 {
			tracker.Observe(ref, providers.ErrUpstreamUnavailable, providers.RateLimit{})
		}
		assertCircuit(t, tracker, ref, 30*time.Second)

		<-time.After(30 * time.Second)
		for range 20 {
			tracker.Observe(ref, providers.ErrUpstreamUnavailable, providers.RateLimit{})
		}
		assertCircuit(t, tracker, ref, 5*time.Minute)
	})
}

func TestTracker_SnapshotPreservesCandidateBreakerWithProviderCooldown(t *testing.T) {
	now := time.Date(2026, time.October, 15, 12, 0, 0, 0, time.UTC)
	ref := providers.ModelRef{Provider: "groq", Model: "candidate"}
	sibling := providers.ModelRef{Provider: "groq", Model: "sibling"}
	tracker, err := quota.New(quota.Options{Now: func() time.Time { return now }, Candidates: []providers.ModelRef{ref, sibling}})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		tracker.Observe(ref, providers.ErrUpstreamUnavailable, providers.RateLimit{})
	}
	for range 10 {
		tracker.Observe(ref, providers.ErrUpstreamUnavailable, providers.RateLimit{})
	}
	tracker.Observe(sibling, providers.ErrUnauthorized, providers.RateLimit{})
	for _, candidate := range tracker.Snapshot() {
		if candidate.Model == ref.Model {
			if candidate.Breaker != quota.BreakerOpen || candidate.Reason != "unauthorized" || candidate.UpstreamFailures != 7 {
				t.Fatalf("candidate snapshot = %+v, want open breaker, provider cooldown reason and capped failure count", candidate)
			}
			return
		}
	}
	t.Fatalf("candidate %q missing from snapshot", ref.Model)
}

func TestTracker_CircuitBreakerHonors503RetryAfterImmediately(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	ref := providers.ModelRef{Provider: "mock", Model: "unavailable"}
	tracker, err := quota.New(quota.Options{Now: func() time.Time { return now }, Candidates: []providers.ModelRef{ref}})
	if err != nil {
		t.Fatal(err)
	}
	tracker.Observe(ref, providers.RetryAfterError{After: 12 * time.Second, Kind: providers.ErrUpstreamUnavailable}, providers.RateLimit{})
	if available, until, reason := tracker.Available(ref); available || !until.Equal(now.Add(12*time.Second)) || reason != "circuit open" {
		t.Fatalf("first 503 with Retry-After = %v, %v, %q; want false, 12s, circuit open", available, until, reason)
	}
}

func assertCircuit(t *testing.T, tracker *quota.Tracker, ref providers.ModelRef, duration time.Duration) {
	t.Helper()
	if available, until, reason := tracker.Available(ref); available || !until.Equal(time.Now().Add(duration)) || reason != "circuit open" {
		t.Fatalf("Available() = %v, %v, %q; want false, %s circuit open", available, until, reason, duration)
	}
}

func TestTracker_ConcurrentFailuresNeverShortenCooldown(t *testing.T) {
	now := time.Now()
	ref := providers.ModelRef{Provider: "groq", Model: "one"}
	sibling := providers.ModelRef{Provider: "groq", Model: "two"}
	tracker, err := quota.New(quota.Options{Now: func() time.Time { return now }, Candidates: []providers.ModelRef{ref, sibling}})
	if err != nil {
		t.Fatal(err)
	}
	tracker.Observe(ref, providers.RetryAfterError{After: 34 * time.Second, Kind: providers.ErrRateLimited}, providers.RateLimit{})
	tracker.Observe(ref, providers.ErrRateLimited, providers.RateLimit{})
	if _, until, reason := tracker.Available(ref); !until.Equal(now.Add(34*time.Second)) || reason != "rate limited; quota scope unknown, conservatively shared" {
		t.Fatalf("shorter rate-limit failure shortened cooldown to %v, reason %q", until, reason)
	}
	tracker.Observe(ref, providers.RetryAfterError{After: 24 * time.Hour, Kind: providers.ErrPaymentRequired}, providers.RateLimit{})
	tracker.Observe(sibling, providers.ErrUnauthorized, providers.RateLimit{})
	if _, until, reason := tracker.Available(sibling); !until.Equal(now.Add(24*time.Hour)) || reason != "payment required" {
		t.Fatalf("key rejection shortened provider cooldown to %v, reason %q", until, reason)
	}
}

func TestTracker_ConcurrentObserveAndAvailable(t *testing.T) {
	refs := []providers.ModelRef{{Provider: "groq", Model: "one"}, {Provider: "gemini", Model: "two"}}
	tracker, err := quota.New(quota.Options{Candidates: refs})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				tracker.Observe(refs[0], errors.Join(providers.ErrRateLimited, errors.New("upstream")), providers.RateLimit{})
				tracker.Available(refs[1])
			}
		}()
	}
	wg.Wait()
}

// A request-size rejection is a property of the request, so it must leave every counter where it was (ADR 0034).
func TestTracker_RequestTooLargeLeavesStateUnchanged(t *testing.T) {
	tooLarge := fmt.Errorf("groq: upstream status 413: %w", providers.ErrRequestTooLarge)
	tests := []struct {
		name  string
		check func(t *testing.T, tracker *quota.Tracker, ref providers.ModelRef)
	}{
		{"fresh candidate stays available", func(t *testing.T, tracker *quota.Tracker, ref providers.ModelRef) {
			for range 5 {
				tracker.Observe(ref, tooLarge, providers.RateLimit{Known: true, RemainingTokens: 0, ResetTokens: time.Minute})
			}
			if available, _, reason := tracker.Available(ref); !available {
				t.Fatalf("candidate unavailable (%s) after request-size rejections", reason)
			}
		}},
		{"breaker failure count is neither raised nor reset", func(t *testing.T, tracker *quota.Tracker, ref providers.ModelRef) {
			for range 2 {
				tracker.Observe(ref, providers.ErrUpstreamUnavailable, providers.RateLimit{})
			}
			for range 5 {
				tracker.Observe(ref, tooLarge, providers.RateLimit{})
			}
			if available, _, _ := tracker.Available(ref); !available {
				t.Fatal("request-size rejections opened the breaker")
			}
			tracker.Observe(ref, providers.ErrUpstreamUnavailable, providers.RateLimit{})
			assertCircuit(t, tracker, ref, 30*time.Second)
		}},
		{"rate-limit backoff is not reset", func(t *testing.T, tracker *quota.Tracker, ref providers.ModelRef) {
			tracker.Observe(ref, providers.ErrRateLimited, providers.RateLimit{})
			<-time.After(15 * time.Second)
			for range 5 {
				tracker.Observe(ref, tooLarge, providers.RateLimit{})
			}
			tracker.Observe(ref, providers.ErrRateLimited, providers.RateLimit{})
			if _, until, _ := tracker.Available(ref); !until.Equal(time.Now().Add(30 * time.Second)) {
				t.Fatalf("second rate-limit cooldown ends at %v, want 30s as if no success happened", time.Until(until))
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ref := providers.ModelRef{Provider: "groq", Model: "one"}
				tracker, err := quota.New(quota.Options{Now: time.Now, Candidates: []providers.ModelRef{ref}})
				if err != nil {
					t.Fatal(err)
				}
				tt.check(t, tracker, ref)
			})
		})
	}
}
