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

// candleCapturingStrategy is a test double that records the EvalContext of
// every Evaluate() call so the test can assert what the runner actually fed
// it (as opposed to what the runner is documented/intended to feed it).
type candleCapturingStrategy struct {
	lastCtx strategy.EvalContext
	calls   int
}

func (c *candleCapturingStrategy) Name() string { return "candle-capture-test-strategy" }
func (c *candleCapturingStrategy) Evaluate(ctx strategy.EvalContext) strategy.Signal {
	c.calls++
	c.lastCtx = ctx
	return strategy.Signal{Action: strategy.Hold, Reason: "test: capture only"}
}

// TestRunner_EvaluateSymbol_FeedsNonEmptyCandles is the R2-INT acceptance
// test for wiring task 1 (R2-2-REPORT.md §3): the runner's real
// evaluateSymbol call path must populate ctx.Candles from a live
// CandleAggregator, alongside the existing ctx.Prices/Volumes tick series —
// not leave it perpetually empty (which was the root cause of sniper's "zero
// trades ever" bug, G-2).
func TestRunner_EvaluateSymbol_FeedsNonEmptyCandles(t *testing.T) {
	strategy.Register("candle-capture-test-strategy", func() strategy.Strategy { return &candleCapturingStrategy{} })
	defer strategy.Reset("candle-capture-test-strategy")

	dir := t.TempDir()
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

	mb := broker.NewMockBrokerWithConfig(10000, broker.LegacyPaperFillConfig())
	if err := mb.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := mb.Subscribe([]string{"TESTSYM"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	riskMgr := risk.NewManager(90, 10000, risk.BrokerageConfig{}, risk.StopLossConfig{Enabled: false}, 100)
	te := NewTradingEngine(riskMgr, mb, csvLog, store, ledgerDB, ledger.ModePaper)

	cfg := RunnerConfig{
		Symbols:       []string{"TESTSYM"},
		StrategyName:  "candle-capture-test-strategy",
		HistorySize:   50,
		MinDataPoints: 1,
		ModeLabel:     "PAPER",
	}
	runner, err := NewRunner(te, cfg)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	strat := runner.strat.(*candleCapturingStrategy)

	// Drive evaluateSymbol across several distinct 5-minute wall-clock
	// buckets, exactly as the real tick loop would over time (MockBroker's
	// GetCurrentPrice does its own random walk on every call, so prices
	// naturally vary) — crossing a bucket boundary is what completes a
	// candle in the CandleAggregator.
	base := time.Date(2026, 7, 20, 9, 20, 0, 0, strategy.IST)
	const numTicks = 8
	for i := 0; i < numTicks; i++ {
		now := base.Add(time.Duration(i) * 6 * time.Minute)
		runner.evaluateSymbol("TESTSYM", false, now)
	}

	if strat.calls == 0 {
		t.Fatalf("strategy was never evaluated")
	}
	if len(strat.lastCtx.Candles) == 0 {
		t.Fatalf("expected ctx.Candles to be non-empty after %d ticks across distinct 5-minute buckets, got 0 (the R2-2/G-2 wiring is missing or broken)", numTicks)
	}
	t.Logf("ctx.Candles has %d completed candle(s) after %d ticks; ctx.Prices has %d point(s)",
		len(strat.lastCtx.Candles), numTicks, len(strat.lastCtx.Prices))
	if len(strat.lastCtx.Prices) == 0 {
		t.Fatalf("ctx.Prices must remain populated too (additive change, not a replacement) — nine_twenty/short_straddle depend on it")
	}
}
