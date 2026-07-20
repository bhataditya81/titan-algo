package strategy

import (
	"testing"
	"time"
)

func TestNineTwentyEntryWindowGating(t *testing.T) {
	s := NewNineTwentyStrategy()

	beforeWindow := time.Date(2026, 1, 19, 9, 19, 0, 0, IST)
	sig := s.Evaluate(EvalContext{Symbol: "NIFTY", Now: beforeWindow})
	if sig.Action != Hold {
		t.Fatalf("expected Hold before entry window, got %v", sig.Action)
	}

	inWindow := time.Date(2026, 1, 19, 9, 21, 0, 0, IST)
	sig = s.Evaluate(EvalContext{Symbol: "NIFTY", Now: inWindow})
	if sig.Action != Sell {
		t.Fatalf("expected Sell entry signal inside the window, got %v (%s)", sig.Action, sig.Reason)
	}
	if len(sig.Legs) != 2 {
		t.Fatalf("expected 2 legs (CE+PE), got %d", len(sig.Legs))
	}

	// entered must NOT flip true from signal generation alone (ST-4 fix).
	snap := s.Snapshot()
	if snap["NIFTY|entered"] != "false" {
		t.Errorf("expected entered=false until ConfirmEntry() is called, got %v", snap["NIFTY|entered"])
	}

	// Repeated evaluation within the window before confirmation must NOT
	// re-signal (latched via signaledEntry).
	sig2 := s.Evaluate(EvalContext{Symbol: "NIFTY", Now: inWindow.Add(time.Minute)})
	if sig2.Action != Hold {
		t.Errorf("expected no duplicate entry signal before confirmation, got %v", sig2.Action)
	}
}

func TestNineTwentyOutsideWindowNoEntry(t *testing.T) {
	s := NewNineTwentyStrategy()
	afterWindow := time.Date(2026, 1, 19, 9, 30, 0, 0, IST)
	sig := s.Evaluate(EvalContext{Symbol: "NIFTY", Now: afterWindow})
	if sig.Action != Hold {
		t.Errorf("expected Hold after the entry window has closed for the day, got %v", sig.Action)
	}
}

func TestNineTwentyPremiumStopLossTriggers(t *testing.T) {
	s := NewNineTwentyStrategy()
	s.ConfirmEntry("NIFTY")

	mid := time.Date(2026, 1, 19, 11, 0, 0, 0, IST)
	// Combined premium 145 >= entry 100 * 1.4 (140) -> must trigger.
	ctx := EvalContext{
		Symbol:       "NIFTY",
		Prices:       []float64{145},
		Now:          mid,
		HasPosition:  true,
		EntryPremium: 100,
	}
	sig := s.Evaluate(ctx)
	if sig.Action != Exit {
		t.Fatalf("expected Exit on premium stop breach, got %v (%s)", sig.Action, sig.Reason)
	}
}

func TestNineTwentyPremiumStopLossNotTriggeredBelowThreshold(t *testing.T) {
	s := NewNineTwentyStrategy()
	s.ConfirmEntry("NIFTY")

	mid := time.Date(2026, 1, 19, 11, 0, 0, 0, IST)
	ctx := EvalContext{
		Symbol:       "NIFTY",
		Prices:       []float64{110}, // below 100*1.4=140
		Now:          mid,
		HasPosition:  true,
		EntryPremium: 100,
	}
	sig := s.Evaluate(ctx)
	if sig.Action != Hold {
		t.Fatalf("expected Hold when premium is below the stop level, got %v (%s)", sig.Action, sig.Reason)
	}
}

func TestNineTwentySquareOffTime(t *testing.T) {
	s := NewNineTwentyStrategy()
	s.ConfirmEntry("NIFTY")

	squareOff := time.Date(2026, 1, 19, 15, 15, 0, 0, IST)
	sig := s.Evaluate(EvalContext{Symbol: "NIFTY", Now: squareOff, HasPosition: true})
	if sig.Action != Exit {
		t.Fatalf("expected Exit at square-off time, got %v", sig.Action)
	}
}

func TestNineTwentySnapshotRestoreRoundTrip(t *testing.T) {
	a := NewNineTwentyStrategy()
	a.ConfirmEntry("NIFTY")

	inSession := time.Date(2026, 1, 19, 11, 0, 0, 0, IST)
	a.Evaluate(EvalContext{Symbol: "NIFTY", Now: inSession, HasPosition: true, EntryPremium: 123.45})

	snap := a.Snapshot()

	b := NewNineTwentyStrategy()
	b.Restore(snap)

	snapB := b.Snapshot()
	if snapB["NIFTY|entered"] != snap["NIFTY|entered"] {
		t.Errorf("entered mismatch after restore: got %v want %v", snapB["NIFTY|entered"], snap["NIFTY|entered"])
	}
	if snapB["NIFTY|entry_premium"] != snap["NIFTY|entry_premium"] {
		t.Errorf("entry_premium mismatch after restore: got %v want %v", snapB["NIFTY|entry_premium"], snap["NIFTY|entry_premium"])
	}

	// Restored instance must behave as "in position": at square-off time it
	// should Exit, not try to re-enter.
	squareOff := time.Date(2026, 1, 19, 15, 15, 0, 0, IST)
	sig := b.Evaluate(EvalContext{Symbol: "NIFTY", Now: squareOff, HasPosition: true})
	if sig.Action != Exit {
		t.Errorf("expected restored strategy to Exit at square-off, got %v", sig.Action)
	}
}

// TestNineTwentyMultiSymbol_OneSymbolsEntryDoesNotBlockAnother reproduces the
// audit-R3 bug: registry.Get caches one shared singleton per strategy name,
// and runner.go's tick loop evaluates every configured symbol against that
// same instance (discovery-driven multi-symbol trading). An un-keyed
// `entered` field let confirming NIFTY's entry silently make BANKNIFTY look
// "in position" too, blocking it from ever entering.
func TestNineTwentyMultiSymbol_OneSymbolsEntryDoesNotBlockAnother(t *testing.T) {
	s := NewNineTwentyStrategy()

	inWindow := time.Date(2026, 1, 19, 9, 21, 0, 0, IST)
	sigNifty := s.Evaluate(EvalContext{Symbol: "NIFTY", Now: inWindow})
	if sigNifty.Action != Sell {
		t.Fatalf("NIFTY: expected entry signal, got %v", sigNifty.Action)
	}
	s.ConfirmEntry("NIFTY")

	// BANKNIFTY, evaluated against the SAME shared instance, must still see
	// its own entry window as open -- NIFTY's confirmed entry must not leak.
	sigBankNifty := s.Evaluate(EvalContext{Symbol: "BANKNIFTY", Now: inWindow})
	if sigBankNifty.Action != Sell {
		t.Fatalf("BANKNIFTY: expected its own entry signal unaffected by NIFTY's confirmed entry, got %v (%s)",
			sigBankNifty.Action, sigBankNifty.Reason)
	}
}

func TestNineTwentyFullDateReset(t *testing.T) {
	s := NewNineTwentyStrategy()

	// Enter on the 15th of one month.
	day1 := time.Date(2026, 1, 15, 9, 21, 0, 0, IST)
	sig := s.Evaluate(EvalContext{Symbol: "NIFTY", Now: day1})
	if sig.Action != Sell {
		t.Fatalf("expected entry signal on day1, got %v", sig.Action)
	}
	s.ConfirmEntry("NIFTY")

	// Old bug: comparing only Day() (15) would treat the 15th of ANY month
	// as "the same day" and never reset. Use the 15th of the NEXT month.
	day2 := time.Date(2026, 2, 15, 9, 21, 0, 0, IST)
	sig2 := s.Evaluate(EvalContext{Symbol: "NIFTY", Now: day2})

	if sig2.Action != Sell {
		t.Fatalf("expected full-date-aware reset to allow a fresh entry signal on day2, got %v (%s)", sig2.Action, sig2.Reason)
	}
}
