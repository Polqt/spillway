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

// GateOptions configures a local spend gate.
type GateOptions struct {
	Limits    config.Budgets
	Providers map[string]config.Provider
	Store     SpendSource
	Now       func() time.Time
}

// Gate tracks UTC daily and monthly spend for paid models.
type Gate struct {
	mu        sync.Mutex
	limits    config.Budgets
	providers map[string]config.Provider
	now       func() time.Time
	day       time.Time
	month     time.Time
	daily     int64
	monthly   int64
}

// NewGate creates a spend gate seeded from persisted usage totals.
func NewGate(ctx context.Context, o GateOptions) (*Gate, error) {
	if o.Limits.DailyMicroUSD <= 0 && o.Limits.MonthlyMicroUSD <= 0 {
		return nil, errors.New("budget gate requires a positive limit")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	now := o.Now().UTC()
	g := &Gate{limits: o.Limits, providers: o.Providers, now: o.Now, day: utcDay(now), month: utcMonth(now)}
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
	}
	return g, nil
}

// Allow reports whether ref can be used and the earliest applicable reset.
func (g *Gate) Allow(ref providers.ModelRef) (bool, time.Time) {
	if config.IsZeroSpendEligible(ref, g.providers[ref.Provider]) {
		return true, time.Time{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.roll(g.now().UTC())
	if (g.limits.DailyMicroUSD > 0 && g.daily >= g.limits.DailyMicroUSD) ||
		(g.limits.MonthlyMicroUSD > 0 && g.monthly >= g.limits.MonthlyMicroUSD) {
		return false, g.nextReset(g.now().UTC())
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
		if reset.IsZero() || monthlyReset.Before(reset) {
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
