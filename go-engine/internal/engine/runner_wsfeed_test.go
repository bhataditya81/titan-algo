package engine

import (
	"testing"
	"time"

	"titan-algo/internal/broker"
)

// agingMarginBroker is fakeMarginBroker plus a controllable price age, so
// this test can simulate "a tick just arrived over WS" (age ~0) vs "REST
// polling hasn't refreshed in a while" (age > staleness threshold) without
// needing a real WebSocket connection (blocked this round — see the R2-INT
// report's real-endpoint section).
type agingMarginBroker struct {
	*fakeMarginBroker
	age time.Duration
	err error
}

func (a *agingMarginBroker) GetCurrentPriceWithAge(symbol string) (float64, time.Duration, error) {
	if a.err != nil {
		return 0, 0, a.err
	}
	return a.MockBroker.GetCurrentPrice(symbol), a.age, nil
}

// TestPriceWithAge_SameLoopLogicRegardlessOfSource is the R2-INT acceptance
// test for wiring task 3 (R2-1-REPORT.md §5): the software stop-loss loop's
// staleness decision (priceWithAge / GetCurrentPriceWithAge) must behave
// identically whether the underlying price cache was last written by a WS
// tick or a REST poll — R2-1's design keeps both paths writing into the same
// cache, so the runner needs no source-aware branching, only a single
// consistent staleness check. This test proves that check reacts correctly
// to a fresh tick and to a stale one, and that a broker with no
// ExtendedTradeService (no staleness concept at all) is always treated as
// fresh, exactly as documented in priceWithAge's own comment.
func TestPriceWithAge_SameLoopLogicRegardlessOfSource(t *testing.T) {
	mb := broker.NewMockBrokerWithConfig(10000, broker.LegacyPaperFillConfig())
	if err := mb.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := mb.Subscribe([]string{"NIFTY"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	t.Run("fresh tick (as if just delivered over WS)", func(t *testing.T) {
		fake := &agingMarginBroker{fakeMarginBroker: &fakeMarginBroker{MockBroker: mb}, age: 200 * time.Millisecond}
		runner := newTestRunner(t, fake)
		price, age, fresh := runner.priceWithAge("NIFTY")
		if !fresh || price <= 0 {
			t.Fatalf("expected a fresh, positive price, got price=%v age=%v fresh=%v", price, age, fresh)
		}
		if age >= runner.cfg.StaleAge {
			t.Fatalf("expected age (%v) below the staleness threshold (%v)", age, runner.cfg.StaleAge)
		}
	})

	t.Run("stale price (as if REST polling fell behind)", func(t *testing.T) {
		fake := &agingMarginBroker{fakeMarginBroker: &fakeMarginBroker{MockBroker: mb}, age: 5 * time.Minute}
		runner := newTestRunner(t, fake)
		price, age, fresh := runner.priceWithAge("NIFTY")
		if !fresh || price <= 0 {
			t.Fatalf("priceWithAge itself should still report the (stale) price, not fail: price=%v age=%v fresh=%v", price, age, fresh)
		}
		if age < runner.cfg.StaleAge {
			t.Fatalf("expected age (%v) to exceed the staleness threshold (%v) so softStopLossCheck skips it", age, runner.cfg.StaleAge)
		}
	})

	t.Run("plain MockBroker (no ExtendedTradeService, no source-of-truth distinction)", func(t *testing.T) {
		runner := newTestRunner(t, mb)
		price, age, fresh := runner.priceWithAge("NIFTY")
		if !fresh || price <= 0 {
			t.Fatalf("expected paper/mock broker prices to always be treated as fresh, got price=%v age=%v fresh=%v", price, age, fresh)
		}
		if age != 0 {
			t.Fatalf("expected age=0 (no staleness concept) for a broker without ExtendedTradeService, got %v", age)
		}
	})
}
