package backtest

import (
	"testing"
	"time"
)

// TestShortStraddleLosesOnTrendingMarket is the key proof for CR-9: the old
// constant-delta-0.5 model made short CE and short PE deltas cancel
// exactly, so a short straddle was mathematically guaranteed to profit
// (free theta, zero gamma risk) no matter what the market did. Under real
// Black-Scholes repricing, a short ATM straddle held through a large,
// sustained directional move must show a net LOSS: the losing leg's
// intrinsic-value growth (delta+gamma) overwhelms the combined theta
// collected on both legs plus the winning leg's decay.
func TestShortStraddleLosesOnTrendingMarket(t *testing.T) {
	start := time.Date(2026, 1, 5, 9, 15, 0, 0, time.UTC)
	cfg := testConfig()
	cfg.DefaultDTEDays = 7 // weekly-style expiry

	// +12.5% sustained trend (20000 -> 22500) over ~1000 minutes -- roughly
	// 12 standard deviations against a 12% annualized IV weekly straddle.
	// Barely any calendar time elapses (well under the week to expiry), so
	// theta collected is small; this isolates gamma/delta loss.
	candles := genTrendCandles(200, 20000, 22500, start)

	fired := false
	strat := &fakeStrategy{name: "short-straddle-probe", fn: func(ctx EvalContext) Signal {
		if !ctx.HasPosition && !fired {
			fired = true
			return Signal{
				Action: Hold, // action ignored when Legs is set; engine keys off len(Legs)>0
				Reason: "short ATM straddle",
				Legs: []OrderLeg{
					{Direction: LegSell, StrikeOffset: 0, OptionType: "CE", Quantity: 1},
					{Direction: LegSell, StrikeOffset: 0, OptionType: "PE", Quantity: 1},
				},
			}
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

	trade := report.Trades[0]
	if trade.Kind != "multi-leg" {
		t.Fatalf("Kind = %q, want multi-leg", trade.Kind)
	}
	if trade.Legs != 2 {
		t.Fatalf("Legs = %d, want 2", trade.Legs)
	}

	if trade.GrossPnL >= 0 {
		t.Errorf("short straddle GrossPnL = %.2f, want < 0 on a strongly trending market -- gamma risk is not being modeled (still looks like the old delta=0.5 free-theta bug)", trade.GrossPnL)
	}
	if report.NetPnL >= 0 {
		t.Errorf("short straddle NetPnL = %.2f, want < 0", report.NetPnL)
	}
}

// TestShortStraddleProfitsInFlatMarket is the mirror sanity check: the same
// short straddle in a genuinely flat/range-bound market should still
// collect positive theta, proving the fix isn't just "always lose" -- it's
// gamma risk that is now priced, not a broken model in the other direction.
func TestShortStraddleProfitsInFlatMarket(t *testing.T) {
	start := time.Date(2026, 1, 5, 9, 15, 0, 0, time.UTC)
	cfg := testConfig()
	cfg.DefaultDTEDays = 7

	candles := genFlatCandles(200, 20000, start)

	fired := false
	strat := &fakeStrategy{name: "short-straddle-flat", fn: func(ctx EvalContext) Signal {
		if !ctx.HasPosition && !fired {
			fired = true
			return Signal{
				Reason: "short ATM straddle, flat market",
				Legs: []OrderLeg{
					{Direction: LegSell, StrikeOffset: 0, OptionType: "CE", Quantity: 1},
					{Direction: LegSell, StrikeOffset: 0, OptionType: "PE", Quantity: 1},
				},
			}
		}
		return Signal{Action: Hold}
	}}

	report, err := Run(candles, strat, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Trades) != 1 {
		t.Fatalf("expected exactly 1 trade, got %d", len(report.Trades))
	}
	if report.Trades[0].GrossPnL <= 0 {
		t.Errorf("short straddle GrossPnL in a flat market = %.2f, want > 0 (theta collected, no adverse spot move)", report.Trades[0].GrossPnL)
	}
}
