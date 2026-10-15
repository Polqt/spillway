package budget_test

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Polqt/spillway/internal/budget"
	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/store"
)

func TestGate_UTCPeriodsAndFreeModels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Date(2026, time.January, 31, 23, 59, 59, 0, time.UTC)
		gate, err := budget.NewGate(t.Context(), budget.GateOptions{Limits: config.Budgets{DailyMicroUSD: 10}, Providers: map[string]config.Provider{"groq": {}}, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		paid := providers.ModelRef{Provider: "groq", Model: "paid"}
		free := providers.ModelRef{Provider: "ollama", Model: "local"}
		gate.Charge(now, 10)
		if allowed, reset := gate.Allow(paid); allowed || !reset.Equal(time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("daily limit = %v, %v; want blocked until UTC midnight", allowed, reset)
		}
		if allowed, _ := gate.Allow(free); !allowed {
			t.Fatal("free model was blocked at budget cap")
		}
		now = now.Add(time.Second)
		if allowed, _ := gate.Allow(paid); !allowed {
			t.Fatal("paid model remained blocked after daily rollover")
		}
		monthGate, err := budget.NewGate(t.Context(), budget.GateOptions{Limits: config.Budgets{MonthlyMicroUSD: 10}, Providers: map[string]config.Provider{"groq": {}}, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		monthGate.Charge(now, 10)
		if allowed, reset := monthGate.Allow(paid); allowed || !reset.Equal(time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("monthly limit = %v, %v; want blocked until UTC month start", allowed, reset)
		}
		bothGate, err := budget.NewGate(t.Context(), budget.GateOptions{Limits: config.Budgets{DailyMicroUSD: 10, MonthlyMicroUSD: 10}, Providers: map[string]config.Provider{"groq": {}}, Now: func() time.Time { return time.Date(2026, time.February, 1, 8, 0, 0, 0, time.UTC) }})
		if err != nil {
			t.Fatal(err)
		}
		bothGate.Charge(time.Date(2026, time.February, 1, 8, 0, 0, 0, time.UTC), 10)
		if allowed, reset := bothGate.Allow(paid); allowed || !reset.Equal(time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("simultaneous daily and monthly limits = %v, %v; want latest reset", allowed, reset)
		}
	})
}

func TestGate_ConcurrentCapEdgeBlocksNextRequest(t *testing.T) {
	gate, err := budget.NewGate(t.Context(), budget.GateOptions{Limits: config.Budgets{DailyMicroUSD: 100}, Providers: map[string]config.Provider{"groq": {}}})
	if err != nil {
		t.Fatal(err)
	}
	gate.Charge(time.Now(), 99)
	ref := providers.ModelRef{Provider: "groq", Model: "paid"}
	const requests = 50
	start := make(chan struct{})
	charge := make(chan struct{})
	var wg sync.WaitGroup
	var allowed sync.WaitGroup
	var chargeAllowed sync.WaitGroup
	var accepted atomic.Int32
	allowed.Add(requests)
	chargeAllowed.Add(requests)
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, _ := gate.Allow(ref)
			if ok {
				accepted.Add(1)
			}
			allowed.Done()
			<-charge
			if ok {
				gate.Charge(time.Now(), 1)
			}
			chargeAllowed.Done()
		}()
	}
	close(start)
	allowed.Wait()
	if accepted.Load() != requests {
		t.Fatalf("accepted at cap edge = %d, want %d in-flight requests", accepted.Load(), requests)
	}
	close(charge)
	chargeAllowed.Wait()
	wg.Wait()
	if ok, _ := gate.Allow(ref); ok {
		t.Fatal("request after concurrent cap edge was allowed")
	}
}

type seededSpend int64

func (s seededSpend) SpendSince(context.Context, time.Time) (int64, error) { return int64(s), nil }

func TestGate_SeedsSpendFromStore(t *testing.T) {
	now := time.Date(2026, time.June, 12, 8, 0, 0, 0, time.FixedZone("local", 8*60*60))
	gate, err := budget.NewGate(t.Context(), budget.GateOptions{
		Limits: config.Budgets{DailyMicroUSD: 50}, Providers: map[string]config.Provider{"groq": {}},
		Store: seededSpend(50), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := gate.Allow(providers.ModelRef{Provider: "groq", Model: "paid"}); ok {
		t.Fatal("persisted spend at limit was not applied")
	}
}

func TestGate_RejectsNegativeLimit(t *testing.T) {
	_, err := budget.NewGate(t.Context(), budget.GateOptions{Limits: config.Budgets{DailyMicroUSD: -1, MonthlyMicroUSD: 10}})
	if err == nil {
		t.Fatal("NewGate accepted a negative daily limit alongside a positive monthly limit")
	}
}

func TestGate_SeedsSpendFromPersistedStoreAfterRestart(t *testing.T) {
	now := time.Date(2026, time.June, 12, 8, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "usage.db")
	usageStore, err := store.Open(t.Context(), store.Options{Path: path, Retention: 90 * 24 * time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := usageStore.Record(t.Context(), store.Request{
		At: now.Add(-time.Hour), CostMicroUSD: 50,
		Attempts: []store.Attempt{{At: now.Add(-time.Hour), CostMicroUSD: 50}},
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := usageStore.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(t.Context(), store.Options{Path: path, Retention: 90 * 24 * time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	gate, err := budget.NewGate(t.Context(), budget.GateOptions{
		Limits: config.Budgets{DailyMicroUSD: 50}, Providers: map[string]config.Provider{"groq": {}}, Store: reopened,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := gate.Allow(providers.ModelRef{Provider: "groq", Model: "paid"}); ok {
		t.Fatal("reopened store spend at limit was not applied")
	}
}
