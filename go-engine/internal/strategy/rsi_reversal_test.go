package strategy

import (
	"strings"
	"testing"
)

// TestRSIReversal_MultiSymbol_OneSymbolGoingFlatDoesNotResetAnother
// reproduces the audit-R3 bug: registry.Get caches one shared singleton per
// strategy name, and runner.go's tick loop evaluates every configured
// symbol against that same instance (discovery-driven multi-symbol
// trading). An un-keyed `lastDirection` field let one symbol's flat state
// (ctx.HasPosition == false) silently reset another symbol's still-open
// position direction, causing the wrong-side mean-reversion exit rule to
// apply.
func TestRSIReversal_MultiSymbol_OneSymbolGoingFlatDoesNotResetAnother(t *testing.T) {
	s := NewRSIReversalStrategy()

	// NIFTY: strongly rising prices push RSI(2) to 100 (overbought) ->
	// Sell entry signal, latching lastDirection["NIFTY"] = Sell.
	sigNifty := s.Evaluate(EvalContext{Symbol: "NIFTY", Prices: []float64{100, 101, 102}})
	if sigNifty.Action != Sell {
		t.Fatalf("NIFTY: expected Sell (overbought) entry signal, got %v", sigNifty.Action)
	}

	// BANKNIFTY evaluated flat (no position) on the SAME shared instance --
	// before the fix this reset the single shared lastDirection field,
	// corrupting NIFTY's tracked direction.
	_ = s.Evaluate(EvalContext{Symbol: "BANKNIFTY", HasPosition: false, Prices: []float64{500, 499, 498}})

	// NIFTY is now reported as holding its short position. The mean-reversion
	// exit must still use the SHORT-side rule (exit when price/RSI reverts
	// downward), not silently fall back to the long-side rule.
	sigNiftyHeld := s.Evaluate(EvalContext{
		Symbol:      "NIFTY",
		HasPosition: true,
		// Falling price after the short entry: RSI < 50 -> should trigger the
		// SHORT-side exit condition (dir == Sell branch). If lastDirection was
		// wrongly reset/corrupted, this falls into the `default` (long) branch
		// instead, which happens to also fire here on falling RSI -- so the
		// distinguishing assertion is on Reason mentioning "short", not just Action.
		Prices: []float64{102, 101, 100, 99, 98},
	})
	if sigNiftyHeld.Action != Exit {
		t.Fatalf("NIFTY: expected an exit signal, got %v (%s)", sigNiftyHeld.Action, sigNiftyHeld.Reason)
	}
	if !strings.Contains(sigNiftyHeld.Reason, "short") {
		t.Errorf("NIFTY: expected the SHORT-side exit reason (lastDirection=Sell preserved), got %q -- BANKNIFTY going flat may have corrupted NIFTY's tracked direction", sigNiftyHeld.Reason)
	}
}
