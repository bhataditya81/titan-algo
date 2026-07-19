package backtest

import (
	"strings"
	"testing"
	"time"
)

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.MinHistory = 5
	return cfg
}

// TestFillAtNextOpen proves ST-7: a signal computed from candle i's history
// fills at candle i+1's OPEN, never at candle i's close, even when the two
// prices differ wildly (a large gap).
func TestFillAtNextOpen(t *testing.T) {
	start := time.Date(2026, 1, 5, 9, 15, 0, 0, time.UTC)
	cfg := testConfig()
	candles := genFlatCandles(30, 20000, start)

	// The eval candle (index MinHistory) closes at 20000; the very next
	// candle gaps up hard at its open. If the engine ever filled at the
	// eval candle's close, EntrySpot would come out 20000, not 20500.
	candles[cfg.MinHistory].Close = 20000
	candles[cfg.MinHistory+1].Open = 20500

	fired := false
	strat := &fakeStrategy{name: "gap-fill-probe", fn: func(ctx EvalContext) Signal {
		if !ctx.HasPosition && !fired {
			fired = true
			return Signal{Action: Buy, Reason: "one-shot entry"}
		}
		return Signal{Action: Hold}
	}}

	report, err := Run(candles, strat, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Trades) != 1 {
		t.Fatalf("expected exactly 1 (force-closed) trade, got %d", len(report.Trades))
	}
	got := report.Trades[0].EntrySpot
	if got != 20500 {
		t.Errorf("EntrySpot = %.2f, want 20500 (next candle's open) -- got the signal candle's close instead (look-ahead bias)", got)
	}
}

// TestDirectionalEntryAndExit proves CR-10: a leg-less Buy signal actually
// opens a position when flat, and an Exit signal actually closes it. The
// old code's single-leg entry branch was unreachable dead code.
func TestDirectionalEntryAndExit(t *testing.T) {
	start := time.Date(2026, 1, 5, 9, 15, 0, 0, time.UTC)
	cfg := testConfig()
	candles := genFlatCandles(30, 20000, start)

	step := 0
	strat := &fakeStrategy{name: "buy-then-exit", fn: func(ctx EvalContext) Signal {
		step++
		switch step {
		case 1:
			return Signal{Action: Buy, Reason: "enter"}
		case 2:
			return Signal{Action: Exit, Reason: "exit"}
		default:
			return Signal{Action: Hold}
		}
	}}

	report, err := Run(candles, strat, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Trades) != 1 {
		t.Fatalf("expected exactly 1 trade (entered then exited), got %d", len(report.Trades))
	}
	tr := report.Trades[0]
	if tr.Kind != "directional" {
		t.Errorf("Kind = %q, want \"directional\"", tr.Kind)
	}
	if !strings.Contains(tr.Reason, "exit") {
		t.Errorf("expected exit-driven close, got reason %q", tr.Reason)
	}
}

// TestDirectionalOppositeSignalCloses proves the "opposite-direction
// signal" half of CR-10's fix: while long (from a Buy), a Sell signal
// closes the position instead of being silently ignored or auto-reversing.
func TestDirectionalOppositeSignalCloses(t *testing.T) {
	start := time.Date(2026, 1, 5, 9, 15, 0, 0, time.UTC)
	cfg := testConfig()
	candles := genFlatCandles(30, 20000, start)

	step := 0
	strat := &fakeStrategy{name: "buy-then-sell", fn: func(ctx EvalContext) Signal {
		step++
		switch step {
		case 1:
			return Signal{Action: Buy, Reason: "enter long"}
		case 2:
			return Signal{Action: Sell, Reason: "flip signal"}
		default:
			return Signal{Action: Hold}
		}
	}}

	report, err := Run(candles, strat, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Trades) != 1 {
		t.Fatalf("expected exactly 1 trade (closed on opposite signal), got %d", len(report.Trades))
	}
	if !strings.Contains(report.Trades[0].Reason, "opposite-direction") {
		t.Errorf("reason = %q, want it to mention opposite-direction close", report.Trades[0].Reason)
	}
}

// TestNoEntryWhileAlreadyInPosition proves the ST-5 side-effect of the
// engine's flat-check: a strategy re-signaling Buy every candle (stateless,
// pre-WP-6 style) does not churn open/close every bar.
func TestNoEntryWhileAlreadyInPosition(t *testing.T) {
	start := time.Date(2026, 1, 5, 9, 15, 0, 0, time.UTC)
	cfg := testConfig()
	candles := genFlatCandles(60, 20000, start)

	strat := &fakeStrategy{name: "always-buy", fn: func(ctx EvalContext) Signal {
		return Signal{Action: Buy, Reason: "always fires"}
	}}

	report, err := Run(candles, strat, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Only the end-of-data force close should appear -- one trade, not
	// dozens from re-entering every candle.
	if len(report.Trades) != 1 {
		t.Fatalf("expected 1 trade (no churn), got %d", len(report.Trades))
	}
}

func TestRun_InsufficientCandles(t *testing.T) {
	cfg := testConfig()
	candles := genFlatCandles(cfg.MinHistory, 20000, time.Now())
	strat := &fakeStrategy{name: "noop", fn: func(ctx EvalContext) Signal { return Signal{Action: Hold} }}
	if _, err := Run(candles, strat, cfg); err == nil {
		t.Fatal("expected error for insufficient candles, got nil")
	}
}
