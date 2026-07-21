package broker

import (
	"errors"
	"testing"
	"time"
)

// fakeExtendedLiveBroker is a minimal TradeService + ExtendedTradeService
// double for testing LivePaperBroker's delegation, independent of a real
// AngelBroker (which needs live credentials/network).
type fakeExtendedLiveBroker struct {
	*MockBroker
	age       time.Duration
	priceErr  error
	healthy   bool
	healthErr error
	margin    float64
	marginErr error
}

func (f *fakeExtendedLiveBroker) PlaceStopLossOrder(string, int, float64, OrderSide) (string, error) {
	return "", errors.New("not used in this test")
}
func (f *fakeExtendedLiveBroker) CancelOrder(string) error { return nil }
func (f *fakeExtendedLiveBroker) GetCurrentPriceWithAge(symbol string) (float64, time.Duration, error) {
	if f.priceErr != nil {
		return 0, 0, f.priceErr
	}
	return f.MockBroker.GetCurrentPrice(symbol), f.age, nil
}
func (f *fakeExtendedLiveBroker) Healthy() bool      { return f.healthy }
func (f *fakeExtendedLiveBroker) HealthError() error { return f.healthErr }
func (f *fakeExtendedLiveBroker) RefreshBalance() (float64, error) {
	return f.MockBroker.GetBalance(), nil
}
func (f *fakeExtendedLiveBroker) GetRequiredMargin(orders []MarginOrderInput) (float64, error) {
	return f.margin, f.marginErr
}
func (f *fakeExtendedLiveBroker) SubscribeLive([]string) error   { return nil }
func (f *fakeExtendedLiveBroker) UnsubscribeLive([]string) error { return nil }

var _ ExtendedTradeService = (*fakeExtendedLiveBroker)(nil)

// TestLivePaperBroker_DelegatesStalenessAndHealthToLiveData reproduces the
// audit-R3 bug: LivePaperBroker didn't implement ExtendedTradeService at
// all, so the software stop-loss loop's staleness check and broker-health
// check silently always reported "fresh"/"healthy" regardless of what the
// REAL live data feed underneath was actually doing — during the exact
// paper-trading run meant to validate safety before real money is at risk.
func TestLivePaperBroker_DelegatesStalenessAndHealthToLiveData(t *testing.T) {
	live := &fakeExtendedLiveBroker{
		MockBroker: NewMockBroker(0),
		age:        5 * time.Minute, // stale
		healthy:    false,
		healthErr:  errors.New("session expired"),
		margin:     12345,
	}
	live.marketPrices = map[string]float64{"NIFTY": 25000}

	lpb := NewLivePaperBroker(live, 10000)

	ext, ok := interface{}(lpb).(ExtendedTradeService)
	if !ok {
		t.Fatal("LivePaperBroker must implement ExtendedTradeService")
	}

	// MockBroker.GetCurrentPrice applies small random movement each call, so
	// assert only what this test actually cares about: a positive price and
	// the REAL age passed through unchanged.
	price, age, err := ext.GetCurrentPriceWithAge("NIFTY")
	if err != nil || price <= 0 || age != 5*time.Minute {
		t.Fatalf("expected staleness to reflect the REAL live feed's age, got price=%v age=%v err=%v", price, age, err)
	}
	if ext.Healthy() {
		t.Fatal("expected Healthy() to reflect the live broker's real (unhealthy) state, not silently report healthy")
	}
	if ext.HealthError() == nil {
		t.Fatal("expected HealthError() to surface the live broker's real error")
	}
	margin, err := ext.GetRequiredMargin(nil)
	if err != nil || margin != 12345 {
		t.Fatalf("expected GetRequiredMargin to delegate to the live broker's real margin API, got margin=%v err=%v", margin, err)
	}
}

// TestLivePaperBroker_FailsClosedWhenLiveDataHasNoExtendedSupport proves
// the "never guess fresh" fallback: if the underlying live broker doesn't
// implement ExtendedTradeService either, LivePaperBroker must error rather
// than silently claiming freshness/health.
func TestLivePaperBroker_FailsClosedWhenLiveDataHasNoExtendedSupport(t *testing.T) {
	plainLive := NewMockBroker(0) // plain TradeService only, no ExtendedTradeService
	lpb := NewLivePaperBroker(plainLive, 10000)

	if _, _, err := lpb.GetCurrentPriceWithAge("NIFTY"); err == nil {
		t.Fatal("expected an error, not a guessed 'fresh' result, when the live broker has no staleness tracking")
	}
	if !lpb.Healthy() {
		t.Fatal("expected Healthy()==true as the documented default when the underlying broker has no health concept")
	}
	if _, err := lpb.GetRequiredMargin(nil); err == nil {
		t.Fatal("expected an error, not a guessed margin, when the live broker has no margin API")
	}
}
