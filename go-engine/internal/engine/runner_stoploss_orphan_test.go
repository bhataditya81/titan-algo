package engine

import (
	"path/filepath"
	"testing"

	"titan-algo/internal/broker"
	"titan-algo/internal/ledger"
	"titan-algo/internal/logger"
	"titan-algo/internal/risk"
	"titan-algo/internal/state"
	"titan-algo/internal/strategy"
)

// TestSoftStopLossCheck_CoversOrphanRiskManagerPosition reproduces the
// audit-R3 bug: an entry order that comes back ErrOrderIndeterminate is
// deliberately left in risk.Manager.OpenPositions (engine.go's
// PlaceEntryOrder does NOT roll back risk state there -- the fill may have
// happened at the broker) but the multi-leg builder returns before ever
// adding it to r.open. Without the fix, such a position got zero software
// stop-loss coverage until the next restart or an explicit flatten. This
// test skips simulating the indeterminate order itself and seeds the same
// end state directly: a risk.Manager position with no r.open leg.
func TestSoftStopLossCheck_CoversOrphanRiskManagerPosition(t *testing.T) {
	dir := t.TempDir()
	mb := broker.NewMockBrokerWithConfig(10000, broker.LegacyPaperFillConfig())
	if err := mb.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := mb.Subscribe([]string{"ORPHAN"}); err != nil {
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

	riskMgr := risk.NewManager(90, 1_000_000, risk.BrokerageConfig{},
		risk.StopLossConfig{Enabled: true, Type: "percentage", Value: 5}, 100)
	te := NewTradingEngine(riskMgr, mb, csvLog, store, ledgerDB, ledger.ModePaper)

	cfg := RunnerConfig{
		Symbols:       []string{"ORPHAN"},
		StrategyName:  "dumb-test-strategy",
		HistorySize:   10,
		MinDataPoints: 1,
		ModeLabel:     "PAPER",
	}
	strategy.Register("dumb-test-strategy", func() strategy.Strategy { return &dumbStrategy{} })
	t.Cleanup(func() { strategy.Reset("dumb-test-strategy") })

	runner, err := NewRunner(te, cfg)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	// Seed the orphan position directly -- entry price high enough that
	// MockBroker's ~1000-and-drifting synthetic price is guaranteed to sit
	// well below the 5% stop, regardless of its small random jitter.
	riskMgr.OpenPosition("ORPHAN", 2000, 10, risk.EquityIntraday, risk.Buy)
	if len(runner.open) != 0 {
		t.Fatalf("sanity check: r.open must NOT contain this position (that's the bug being reproduced)")
	}

	runner.softStopLossCheck()

	if _, stillOpen := riskMgr.GetOpenPositions()["ORPHAN"]; stillOpen {
		t.Fatalf("expected softStopLossCheck to close the orphan position via PlaceExitOrder, but it's still open")
	}
}
