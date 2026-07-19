package backtest

import (
	"fmt"
	"math"
	"testing"
	"time"
)

// TestSampleBacktestRun_PrintsReport exercises the full engine end-to-end
// on a synthetic multi-month dataset (cyclic + drifting, so it has both
// range-bound and trending stretches) and prints the report. This is the
// sample run captured in docs/reports/WP-7-REPORT.md ("go test -v -run
// TestSampleBacktestRun_PrintsReport ./internal/backtest/").
func TestSampleBacktestRun_PrintsReport(t *testing.T) {
	start := time.Date(2026, 1, 1, 9, 15, 0, 0, time.UTC)
	const n = 140
	candles := make([]Candle, n)
	price := 20000.0
	for i := 0; i < n; i++ {
		move := 700*math.Sin(float64(i)/9.0) + float64(i)*1.5
		c := 20000 + move
		candles[i] = Candle{
			Time:   start.AddDate(0, 0, i),
			Open:   price,
			High:   math.Max(price, c) + 20,
			Low:    math.Min(price, c) - 20,
			Close:  c,
			Volume: 100000,
		}
		price = c
	}

	cfg := DefaultConfig()
	cfg.MinHistory = 10
	cfg.DefaultDTEDays = 7

	holdLeft := 0
	strat := &fakeStrategy{name: "weekly-short-straddle-demo", fn: func(ctx EvalContext) Signal {
		if ctx.HasPosition {
			holdLeft--
			if holdLeft <= 0 {
				return Signal{Action: Exit, Reason: "scheduled exit after holding period"}
			}
			return Signal{Action: Hold}
		}
		idx := len(ctx.Candles)
		if idx%8 == 0 {
			holdLeft = 6
			return Signal{
				Reason: "weekly ATM short straddle entry",
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
	if len(report.Trades) == 0 {
		t.Fatal("expected at least one trade in the sample run")
	}
	fmt.Println(report.String())
}
