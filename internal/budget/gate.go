package budget

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
)

// SpendSource seeds a gate from persisted request totals.
type SpendSource interface {
	SpendSince(context.Context, time.Time) (int64, error)
}

type instanceSpendSource interface {
	SpendSinceProvider(context.Context, time.Time, string) (int64, error)
}

// GateOptions configures a local spend gate.
type GateOptions struct {
	Limits    config.Budgets
	Providers map[string]config.Provider
	Store     SpendSource
	Now       func() time.Time
}

// Gate tracks UTC daily and monthly spend for paid models.
type Gate struct {
	mu            sync.Mutex
	limits        config.Budgets
	providers     map[string]config.Provider
	instanceSpend map[string]instancePeriods
	now           func() time.Time
	day           time.Time
	month         time.Time
	daily         int64
	monthly       int64
}

type instancePeriods struct {
	limits  config.Budgets
	day     time.Time
	month   time.Time
	daily   int64
	monthly int64
}

// PeriodStatus reports one configured UTC budget period.
type PeriodStatus struct {
	LimitMicroUSD     int64     `json:"limit_micro_usd"`     // Configured limit in micro-USD.
	SpentMicroUSD     int64     `json:"spent_micro_usd"`     // Spend recorded in the current period.
	RemainingMicroUSD int64     `json:"remaining_micro_usd"` // Non-negative amount below the limit.
	ResetAt           time.Time `json:"reset_at"`            // Start of the next UTC period.
}

// Status is a read-only snapshot of the configured UTC budget periods.
type Status struct {
	Daily   *PeriodStatus `json:"daily,omitempty"`   // Daily budget, when configured.
	Monthly *PeriodStatus `json:"monthly,omitempty"` // Monthly budget, when configured.
}

// NewGate creates a spend gate seeded from persisted usage totals.
func NewGate(ctx context.Context, o GateOptions) (*Gate, error) {
	if o.Limits.DailyMicroUSD < 0 || o.Limits.MonthlyMicroUSD < 0 {
		return nil, errors.New("budget gate limits must not be negative")
	}
	hasInstanceLimit := false
	for _, provider := range o.Providers {
		if provider.Budget != nil {
			hasInstanceLimit = true
			break
		}
	}
	if o.Limits.DailyMicroUSD <= 0 && o.Limits.MonthlyMicroUSD <= 0 && !hasInstanceLimit {
		return nil, errors.New("budget gate requires a positive limit")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	now := o.Now().UTC()
	g := &Gate{limits: o.Limits, providers: o.Providers, now: o.Now, day: utcDay(now), month: utcMonth(now), instanceSpend: make(map[string]instancePeriods)}
	if hasInstanceLimit && o.Store == nil {
		return nil, errors.New("budget gate needs a store to load per-instance spend")
	}
	if o.Store != nil {
		if o.Limits.DailyMicroUSD > 0 {
			spend, err := o.Store.SpendSince(ctx, g.day)
			if err != nil {
				return nil, err
			}
			g.daily = spend
		}
		if o.Limits.MonthlyMicroUSD > 0 {
			spend, err := o.Store.SpendSince(ctx, g.month)
			if err != nil {
				return nil, err
			}
			g.monthly = spend
		}
		providerSource, supportsInstances := o.Store.(instanceSpendSource)
		for name, provider := range o.Providers {
			if provider.Budget == nil {
				continue
			}
			if !supportsInstances {
				return nil, errors.New("budget gate store cannot load per-instance spend")
			}
			state := instancePeriods{limits: *provider.Budget, day: utcDay(now), month: utcMonth(now)}
			var err error
			if state.limits.DailyMicroUSD > 0 {
				state.daily, err = providerSource.SpendSinceProvider(ctx, state.day, name)
				if err != nil {
					return nil, err
				}
			}
			if state.limits.MonthlyMicroUSD > 0 {
				state.monthly, err = providerSource.SpendSinceProvider(ctx, state.month, name)
				if err != nil {
					return nil, err
				}
			}
			g.instanceSpend[name] = state
		}
	}
	return g, nil
}

// Allow reports whether ref can be used and when all exhausted limits reset.
func (g *Gate) Allow(ref providers.ModelRef) (bool, time.Time) {
	if config.IsZeroSpendEligible(ref, g.providers[ref.Provider]) {
		return true, time.Time{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.roll(g.now().UTC())
	g.rollInstances(g.now().UTC())
	var reset time.Time
	if (g.limits.DailyMicroUSD > 0 && g.daily >= g.limits.DailyMicroUSD) ||
		(g.limits.MonthlyMicroUSD > 0 && g.monthly >= g.limits.MonthlyMicroUSD) {
		reset = g.nextReset(g.now().UTC())
	}
	if instance, exists := g.instanceSpend[ref.Provider]; exists {
		if instance.limits.DailyMicroUSD > 0 && instance.daily >= instance.limits.DailyMicroUSD ||
			instance.limits.MonthlyMicroUSD > 0 && instance.monthly >= instance.limits.MonthlyMicroUSD {
			instanceReset := g.instanceNextReset(instance, g.now().UTC())
			if reset.IsZero() || instanceReset.After(reset) {
				reset = instanceReset
			}
		}
	}
	if !reset.IsZero() {
		return false, reset
	}
	return true, time.Time{}
}

// Charge adds the completed attempt's estimated cost to its UTC periods.
func (g *Gate) Charge(at time.Time, costMicroUSD int64) {
	if costMicroUSD <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.roll(at.UTC())
	g.daily = add(g.daily, costMicroUSD)
	g.monthly = add(g.monthly, costMicroUSD)
}

// ChargeFor adds one attempt to the process-wide ceiling and its instance budget.
func (g *Gate) ChargeFor(ref providers.ModelRef, at time.Time, costMicroUSD int64) {
	if costMicroUSD <= 0 {
		return
	}
	g.Charge(at, costMicroUSD)
	g.mu.Lock()
	defer g.mu.Unlock()
	state, exists := g.instanceSpend[ref.Provider]
	if !exists {
		return
	}
	if day := utcDay(at.UTC()); !day.Equal(state.day) {
		state.day, state.daily = day, 0
	}
	if month := utcMonth(at.UTC()); !month.Equal(state.month) {
		state.month, state.monthly = month, 0
	}
	state.daily, state.monthly = add(state.daily, costMicroUSD), add(state.monthly, costMicroUSD)
	g.instanceSpend[ref.Provider] = state
}

func (g *Gate) rollInstances(now time.Time) {
	for name, state := range g.instanceSpend {
		if day := utcDay(now); !day.Equal(state.day) {
			state.day, state.daily = day, 0
		}
		if month := utcMonth(now); !month.Equal(state.month) {
			state.month, state.monthly = month, 0
		}
		g.instanceSpend[name] = state
	}
}

func (g *Gate) instanceNextReset(state instancePeriods, now time.Time) time.Time {
	var reset time.Time
	if state.limits.DailyMicroUSD > 0 && state.daily >= state.limits.DailyMicroUSD {
		reset = state.day.AddDate(0, 0, 1)
	}
	if state.limits.MonthlyMicroUSD > 0 && state.monthly >= state.limits.MonthlyMicroUSD {
		monthly := state.month.AddDate(0, 1, 0)
		if reset.IsZero() || monthly.After(reset) {
			reset = monthly
		}
	}
	if reset.Before(now) {
		return now
	}
	return reset
}

// Status returns limits, spend, remaining amount and the next reset time.
func (g *Gate) Status() Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now().UTC()
	g.roll(now)
	var status Status
	if g.limits.DailyMicroUSD > 0 {
		status.Daily = &PeriodStatus{LimitMicroUSD: g.limits.DailyMicroUSD, SpentMicroUSD: g.daily,
			RemainingMicroUSD: max(0, g.limits.DailyMicroUSD-g.daily), ResetAt: g.day.AddDate(0, 0, 1)}
	}
	if g.limits.MonthlyMicroUSD > 0 {
		status.Monthly = &PeriodStatus{LimitMicroUSD: g.limits.MonthlyMicroUSD, SpentMicroUSD: g.monthly,
			RemainingMicroUSD: max(0, g.limits.MonthlyMicroUSD-g.monthly), ResetAt: g.month.AddDate(0, 1, 0)}
	}
	return status
}

func (g *Gate) roll(now time.Time) {
	day, month := utcDay(now), utcMonth(now)
	if !day.Equal(g.day) {
		g.day, g.daily = day, 0
	}
	if !month.Equal(g.month) {
		g.month, g.monthly = month, 0
	}
}

func (g *Gate) nextReset(now time.Time) time.Time {
	var reset time.Time
	if g.limits.DailyMicroUSD > 0 && g.daily >= g.limits.DailyMicroUSD {
		reset = g.day.AddDate(0, 0, 1)
	}
	if g.limits.MonthlyMicroUSD > 0 && g.monthly >= g.limits.MonthlyMicroUSD {
		monthlyReset := g.month.AddDate(0, 1, 0)
		if reset.IsZero() || monthlyReset.After(reset) {
			reset = monthlyReset
		}
	}
	if reset.Before(now) {
		return now
	}
	return reset
}

func utcDay(t time.Time) time.Time {
	year, month, day := t.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func utcMonth(t time.Time) time.Time {
	year, month, _ := t.UTC().Date()
	return time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
}

func add(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
