package engine

import (
	"path/filepath"
	"testing"

	"titan-algo/internal/broker"
	"titan-algo/internal/ledger"
	"titan-algo/internal/logger"
	"titan-algo/internal/risk"
	"titan-algo/internal/state"
)

// countingBroker wraps MockBroker to count PlaceOrder calls, so this test
// can prove the broker was NEVER reached when the durable write fails --
// not just that PlaceEntryOrder returned an error.
type countingBroker struct {
	*broker.MockBroker
	placeOrderCalls int
}

func (c *countingBroker) PlaceOrder(order broker.Order) (*broker.FilledOrder, error) {
	c.placeOrderCalls++
	return c.MockBroker.PlaceOrder(order)
}

// TestPlaceEntryOrder_FailsClosedWhenIntentCannotBePersisted reproduces the
// audit-R3 bug: the entry-order intent record MUST exist before the broker
// call (state.Store's own doc comment), but a failure to write it was only
// logged -- a real order still went out with zero durable trace of it, and
// a crash right after would leave nothing to reconcile against. This proves
// the fix: a durable-write failure now refuses the entry BEFORE any broker
// call happens.
func TestPlaceEntryOrder_FailsClosedWhenIntentCannotBePersisted(t *testing.T) {
	dir := t.TempDir()
	mb := &countingBroker{MockBroker: broker.NewMockBrokerWithConfig(10000, broker.LegacyPaperFillConfig())}
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

	// Force every subsequent durable write to fail deterministically.
	store.Close()

	_, err = e.PlaceEntryOrder("TESTSYM", 10, broker.Buy, risk.EquityIntraday, "test", 0)
	if err == nil {
		t.Fatal("expected PlaceEntryOrder to refuse the entry when the intent record can't be durably persisted")
	}
	if mb.placeOrderCalls != 0 {
		t.Fatalf("expected the broker to NEVER be called when the durable write failed, but PlaceOrder was called %d time(s)", mb.placeOrderCalls)
	}
	if len(riskMgr.GetOpenPositions()) != 0 {
		t.Fatalf("expected no risk-manager position to be opened, got %d", len(riskMgr.GetOpenPositions()))
	}
}
