package engine

import (
	"path/filepath"
	"testing"
	"time"

	"titan-algo/internal/broker"
	"titan-algo/internal/ledger"
	"titan-algo/internal/logger"
	"titan-algo/internal/risk"
	"titan-algo/internal/state"
	"titan-algo/internal/strategy"
)

// fakeMarginBroker wraps MockBroker to also implement ExtendedTradeService
// with a controllable GetRequiredMargin — MockBroker itself deliberately
// does NOT implement ExtendedTradeService (R2-1/R2-4's reports), so this is
// the minimal fake needed to exercise the runner's margin-aware SELL-entry
// wiring (R2-INT wiring task 2) against a fail-closed and a happy path.
type fakeMarginBroker struct {
	*broker.MockBroker
	marginFn    func(orders []broker.MarginOrderInput) (float64, error)
	marginCalls [][]broker.MarginOrderInput
}

func (f *fakeMarginBroker) PlaceStopLossOrder(string, int, float64, broker.OrderSide) (string, error) {
	return "", nil
}
func (f *fakeMarginBroker) CancelOrder(string) error { return nil }
func (f *fakeMarginBroker) GetCurrentPriceWithAge(symbol string) (float64, time.Duration, error) {
	p := f.MockBroker.GetCurrentPrice(symbol)
	if p <= 0 {
		return 0, 0, broker.ErrNoPrice
	}
	return p, 0, nil
}
func (f *fakeMarginBroker) Healthy() bool          { return true }
func (f *fakeMarginBroker) HealthError() error     { return nil }
func (f *fakeMarginBroker) RefreshBalance() (float64, error) {
	return f.MockBroker.GetBalance(), nil
}
func (f *fakeMarginBroker) GetRequiredMargin(orders []broker.MarginOrderInput) (float64, error) {
	f.marginCalls = append(f.marginCalls, orders)
	return f.marginFn(orders)
}
func (f *fakeMarginBroker) SubscribeLive([]string) error   { return nil }
func (f *fakeMarginBroker) UnsubscribeLive([]string) error { return nil }

var _ broker.ExtendedTradeService = (*fakeMarginBroker)(nil)

func newTestRunner(t *testing.T, tradeService broker.TradeService) *Runner {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	ledgerDB, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() { ledgerDB.Close() })
	csvLog, err := logger.NewCSVLogger(dir)
	if err != nil {
		t.Fatalf("csv logger: %v", err)
	}
	t.Cleanup(func() { csvLog.Close() })

	riskMgr := risk.NewManager(90, 1000000, risk.BrokerageConfig{}, risk.StopLossConfig{Enabled: false}, 100)
	te := NewTradingEngine(riskMgr, tradeService, csvLog, store, ledgerDB, ledger.ModePaper)

	cfg := RunnerConfig{
		Symbols:              []string{"NIFTY"},
		StrategyName:         "dumb-test-strategy",
		HistorySize:          10,
		MinDataPoints:        1,
		ModeLabel:            "PAPER",
		OptionExpiryOverride: "29JAN26",
		StrikeStep:           50,
		// Explicit, not a guess: lot size must always come from the
		// instrument master or an operator-set override (never an implicit
		// default), so test fixtures must declare it too.
		LotSizes: map[string]int{"NIFTY": 75},
	}
	strategy.Register("dumb-test-strategy", func() strategy.Strategy { return &dumbStrategy{} })
	t.Cleanup(func() { strategy.Reset("dumb-test-strategy") })

	runner, err := NewRunner(te, cfg)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return runner
}

// TestPlaceSingleLeg_SellDerivative_FailsClosedWithoutExtendedTradeService is
// the R2-INT acceptance test for wiring task 2 (R2-1-REPORT.md §5): a SELL
// derivative entry against a broker that does NOT implement
// ExtendedTradeService (plain MockBroker, exactly as R2-1/R2-4's reports
// document) must be rejected fail-closed — no position opened, no fallback
// to premium-based sizing.
func TestPlaceSingleLeg_SellDerivative_FailsClosedWithoutExtendedTradeService(t *testing.T) {
	mb := broker.NewMockBrokerWithConfig(10000, broker.LegacyPaperFillConfig())
	if err := mb.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := mb.Subscribe([]string{"NIFTY29JAN2622000CE"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	runner := newTestRunner(t, mb)

	runner.placeSingleLeg("NIFTY", "NIFTY29JAN2622000CE", broker.Sell, 75, risk.OptCarry)

	runner.mu.Lock()
	_, opened := runner.open["NIFTY"]
	runner.mu.Unlock()
	if opened {
		t.Fatalf("expected SELL derivative entry to be REJECTED (no ExtendedTradeService) — a position was opened instead")
	}
	if len(runner.te.riskManager.GetOpenPositions()) != 0 {
		t.Fatalf("expected no risk position recorded for a fail-closed rejection")
	}
}

// TestPlaceSingleLeg_SellDerivative_UsesRealMargin proves the happy path:
// when the broker implements ExtendedTradeService, the runner calls
// GetRequiredMargin with the correct single-leg order and, given a valid
// margin figure, the entry proceeds (risk.ValidateOrderWithMargin accepts
// it and a position opens).
func TestPlaceSingleLeg_SellDerivative_UsesRealMargin(t *testing.T) {
	mb := broker.NewMockBrokerWithConfig(10000000, broker.LegacyPaperFillConfig())
	if err := mb.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := mb.Subscribe([]string{"NIFTY29JAN2622000CE"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	fake := &fakeMarginBroker{
		MockBroker: mb,
		marginFn: func(orders []broker.MarginOrderInput) (float64, error) {
			return 50000, nil
		},
	}
	runner := newTestRunner(t, fake)

	runner.placeSingleLeg("NIFTY", "NIFTY29JAN2622000CE", broker.Sell, 75, risk.OptCarry)

	if len(fake.marginCalls) != 1 {
		t.Fatalf("expected exactly 1 GetRequiredMargin call, got %d", len(fake.marginCalls))
	}
	got := fake.marginCalls[0]
	if len(got) != 1 || got[0].Symbol != "NIFTY29JAN2622000CE" || got[0].TransactionType != broker.Sell || got[0].Quantity != 75 {
		t.Fatalf("unexpected margin order sent: %+v", got)
	}

	runner.mu.Lock()
	_, opened := runner.open["NIFTY"]
	runner.mu.Unlock()
	if !opened {
		t.Fatalf("expected the entry to succeed once real margin was supplied")
	}
}

// TestPlaceSingleLeg_SellDerivative_MarginErrorFailsClosed proves the broker
// margin call itself erroring (e.g. a transport failure) also rejects the
// entry — never falls back to premium-based sizing.
func TestPlaceSingleLeg_SellDerivative_MarginErrorFailsClosed(t *testing.T) {
	mb := broker.NewMockBrokerWithConfig(10000, broker.LegacyPaperFillConfig())
	if err := mb.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := mb.Subscribe([]string{"NIFTY29JAN2622000CE"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	fake := &fakeMarginBroker{
		MockBroker: mb,
		marginFn: func(orders []broker.MarginOrderInput) (float64, error) {
			return 0, broker.ErrNoPrice // stand-in for any margin-API transport error
		},
	}
	runner := newTestRunner(t, fake)

	runner.placeSingleLeg("NIFTY", "NIFTY29JAN2622000CE", broker.Sell, 75, risk.OptCarry)

	runner.mu.Lock()
	_, opened := runner.open["NIFTY"]
	runner.mu.Unlock()
	if opened {
		t.Fatalf("expected the entry to be REJECTED when GetRequiredMargin errors")
	}
}

// TestEnterMultiLeg_PricesWholeBasketInOneCall proves R2-1's hedged-margin
// design (R2-1-REPORT.md §5: "Support multi-leg baskets... send all legs
// together for hedged-margin benefit"): a 2-leg SELL/SELL basket (short
// straddle shape) results in exactly ONE GetRequiredMargin call carrying
// BOTH legs, not one call per leg.
func TestEnterMultiLeg_PricesWholeBasketInOneCall(t *testing.T) {
	mb := broker.NewMockBrokerWithConfig(10000000, broker.LegacyPaperFillConfig())
	if err := mb.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	symbols := []string{"NIFTY"}
	if err := mb.Subscribe(symbols); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	fake := &fakeMarginBroker{
		MockBroker: mb,
		marginFn: func(orders []broker.MarginOrderInput) (float64, error) {
			return 80000, nil // combined, hedged-benefit basket margin
		},
	}
	runner := newTestRunner(t, fake)

	sig := strategy.Signal{
		Action: strategy.Sell,
		Legs: []strategy.OrderLeg{
			{Direction: strategy.LegSell, StrikeOffset: 0, OptionType: "CE", Quantity: 1},
			{Direction: strategy.LegSell, StrikeOffset: 0, OptionType: "PE", Quantity: 1},
		},
	}
	runner.enterMultiLeg("NIFTY", sig)

	if len(fake.marginCalls) != 1 {
		t.Fatalf("expected exactly 1 basket-level GetRequiredMargin call, got %d", len(fake.marginCalls))
	}
	if len(fake.marginCalls[0]) != 2 {
		t.Fatalf("expected the single call to carry both legs, got %d leg(s)", len(fake.marginCalls[0]))
	}
	for _, o := range fake.marginCalls[0] {
		if o.TransactionType != broker.Sell {
			t.Fatalf("expected both legs to be SELL in the basket, got %+v", o)
		}
	}

	runner.mu.Lock()
	st, opened := runner.open["NIFTY"]
	runner.mu.Unlock()
	if !opened {
		t.Fatalf("expected the multi-leg entry to succeed once basket margin was supplied")
	}
	if len(st.Legs) != 2 {
		t.Fatalf("expected 2 legs recorded, got %d", len(st.Legs))
	}
}

// TestEnterMultiLeg_BasketMarginError_RejectsBeforePlacingAnyLeg proves a
// basket-level margin failure rejects the WHOLE multi-leg entry up front —
// no leg is placed, so there is nothing to unwind.
func TestEnterMultiLeg_BasketMarginError_RejectsBeforePlacingAnyLeg(t *testing.T) {
	mb := broker.NewMockBrokerWithConfig(10000000, broker.LegacyPaperFillConfig())
	if err := mb.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := mb.Subscribe([]string{"NIFTY"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	fake := &fakeMarginBroker{
		MockBroker: mb,
		marginFn: func(orders []broker.MarginOrderInput) (float64, error) {
			return 0, broker.ErrNoPrice
		},
	}
	runner := newTestRunner(t, fake)

	sig := strategy.Signal{
		Action: strategy.Sell,
		Legs: []strategy.OrderLeg{
			{Direction: strategy.LegSell, StrikeOffset: 0, OptionType: "CE", Quantity: 1},
			{Direction: strategy.LegSell, StrikeOffset: 0, OptionType: "PE", Quantity: 1},
		},
	}
	runner.enterMultiLeg("NIFTY", sig)

	runner.mu.Lock()
	_, opened := runner.open["NIFTY"]
	runner.mu.Unlock()
	if opened {
		t.Fatalf("expected the whole multi-leg entry to be rejected before any leg was placed")
	}
	if len(runner.te.riskManager.GetOpenPositions()) != 0 {
		t.Fatalf("expected no risk positions recorded at all")
	}
}
