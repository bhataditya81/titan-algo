package engine

import (
	"path/filepath"
	"sync"
	"testing"

	"titan-algo/internal/broker"
	"titan-algo/internal/ledger"
	"titan-algo/internal/logger"
	"titan-algo/internal/risk"
	"titan-algo/internal/state"
)

// TestPlaceExitOrder_ConcurrentCallsDoNotBothReachTheBroker reproduces the
// audit-R3 race: the software stop-loss loop and a manual /api/kill run on
// different goroutines and can both call PlaceExitOrder for the same
// symbol. Between the "position still exists" check and the risk manager
// actually closing it lies a real broker call — without a guard, both
// callers could pass the exists-check before either closes the position,
// placing two live exit orders for the same position.
func TestPlaceExitOrder_ConcurrentCallsDoNotBothReachTheBroker(t *testing.T) {
	dir := t.TempDir()
	mb := broker.NewMockBrokerWithConfig(10000, broker.LegacyPaperFillConfig())
	if err := mb.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := mb.Subscribe([]string{"TESTSYM"}); err != nil {
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

	riskMgr := risk.NewManager(90, 10000, risk.BrokerageConfig{}, risk.StopLossConfig{Enabled: false}, 100)
	csvLog, err := logger.NewCSVLogger(dir)
	if err != nil {
		t.Fatalf("csv logger: %v", err)
	}
	defer csvLog.Close()

	e := NewTradingEngine(riskMgr, mb, csvLog, store, ledgerDB, ledger.ModePaper)

	if _, err := e.PlaceEntryOrder("TESTSYM", 10, broker.Buy, risk.EquityIntraday, "test", 0); err != nil {
		t.Fatalf("entry: %v", err)
	}

	const n = 10
	var wg sync.WaitGroup
	results := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, _, err := e.PlaceExitOrder("TESTSYM", "test", "")
			results[i] = err
		}(i)
	}
	wg.Wait()

	successes := 0
	for _, err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly 1 of %d concurrent PlaceExitOrder calls to succeed (the rest refused as duplicates), got %d successes", n, successes)
	}
	if len(riskMgr.GetOpenPositions()) != 0 {
		t.Fatalf("position should be closed exactly once, but risk manager still shows %d open", len(riskMgr.GetOpenPositions()))
	}
}
