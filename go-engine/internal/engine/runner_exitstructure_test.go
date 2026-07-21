package engine

import (
	"fmt"
	"path/filepath"
	"testing"

	"titan-algo/internal/broker"
	"titan-algo/internal/ledger"
	"titan-algo/internal/logger"
	"titan-algo/internal/risk"
	"titan-algo/internal/state"
	"titan-algo/internal/strategy"
)

// exitRejectingBroker rejects reduce-only (exit) orders for one symbol,
// deterministically forcing exitStructure's allOK=false path -- no reliance
// on MockBroker's nondeterministic realistic-fill-rejection model.
type exitRejectingBroker struct {
	*broker.MockBroker
	rejectExitFor string
}

func (b *exitRejectingBroker) PlaceOrder(order broker.Order) (*broker.FilledOrder, error) {
	if order.Intent == broker.IntentReduceOnly && order.Symbol == b.rejectExitFor {
		return nil, fmt.Errorf("simulated exchange rejection: margin insufficient")
	}
	return b.MockBroker.PlaceOrder(order)
}

// TestExitStructure_FailedCloseKeepsPositionTracked reproduces a bug found
// running live-data paper trading: exitStructure used to delete r.open (and
// wipe the durable "legs:" record) UNCONDITIONALLY, even when a leg failed
// to close. The very next tick then saw hasPos=false for that symbol, so
// the strategy was free to open a BRAND NEW position on top of the one that
// never actually closed -- while risk.Manager's OpenPositions map silently
// overwrote the original entry with the new one, losing all trace of it.
// A failed/partial close must keep the structure tracked instead.
func TestExitStructure_FailedCloseKeepsPositionTracked(t *testing.T) {
	const symbol = "TESTOPT"
	dir := t.TempDir()
	fake := &exitRejectingBroker{
		MockBroker:    broker.NewMockBrokerWithConfig(10000, broker.LegacyPaperFillConfig()),
		rejectExitFor: symbol,
	}
	if err := fake.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := fake.Subscribe([]string{symbol}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	store, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer store.Close()
	ledgerDB, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer ledgerDB.Close()
	csvLog, err := logger.NewCSVLogger(dir)
	if err != nil {
		t.Fatalf("csv logger: %v", err)
	}
	defer csvLog.Close()

	riskMgr := risk.NewManager(90, 100000, risk.BrokerageConfig{}, risk.StopLossConfig{Enabled: false}, 100)
	te := NewTradingEngine(riskMgr, fake, csvLog, store, ledgerDB, ledger.ModePaper)
	strategy.Register("dumb-test-strategy", func() strategy.Strategy { return &dumbStrategy{} })
	t.Cleanup(func() { strategy.Reset("dumb-test-strategy") })
	runner, err := NewRunner(te, RunnerConfig{
		Symbols: []string{symbol}, StrategyName: "dumb-test-strategy",
		HistorySize: 10, MinDataPoints: 1, ModeLabel: "PAPER",
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	// Open a position via the entry path, exactly like the runner would.
	res, err := te.PlaceEntryOrder(symbol, 1, broker.Buy, risk.EquityIntraday, "test", 0)
	if err != nil {
		t.Fatalf("entry: %v", err)
	}
	runner.mu.Lock()
	runner.open[symbol] = &openStructure{
		Legs: []legRecord{{Symbol: symbol, Side: broker.Buy, PositionID: res.PositionID}},
	}
	runner.mu.Unlock()

	// Exit attempt fails (simulated rejection) -- this is the exact path
	// that used to silently forget the position.
	runner.exitStructure(symbol, "test exit")

	runner.mu.Lock()
	_, stillTracked := runner.open[symbol]
	runner.mu.Unlock()
	if !stillTracked {
		t.Fatal("expected the structure to remain tracked in r.open after a failed close, got it deleted -- the position would be silently re-enterable next tick")
	}
	if _, exists := riskMgr.GetOpenPositions()[symbol]; !exists {
		t.Fatal("expected the risk-manager position to still exist (the exit never actually succeeded)")
	}
}
