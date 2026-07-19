package strategy

import (
	"testing"
	"time"
)

func TestSniperUpdateCandle_WallClockBoundaries(t *testing.T) {
	s := NewSniperStrategy()
	s.CandleMinutes = 5

	base := time.Date(2026, 1, 19, 9, 15, 0, 0, IST)

	// Three ticks inside the same 5-min bucket [09:15,09:20).
	times := []time.Time{base, base.Add(1 * time.Minute), base.Add(4*time.Minute + 59*time.Second)}
	cumVol := []float64{1000, 1200, 1500}
	for i, tm := range times {
		ctx := EvalContext{Symbol: "NIFTY", Prices: []float64{100 + float64(i)}, Volumes: []float64{cumVol[i]}, Now: tm}
		if completed := s.updateCandle(ctx); completed {
			t.Errorf("tick %d inside the same bucket should not report completion", i)
		}
	}

	// Tick crossing into the next bucket [09:20,09:25) must complete the
	// first candle.
	crossTick := base.Add(5 * time.Minute)
	ctx := EvalContext{Symbol: "NIFTY", Prices: []float64{105}, Volumes: []float64{1800}, Now: crossTick}
	if !s.updateCandle(ctx) {
		t.Fatalf("expected candle completion on bucket-crossing tick")
	}

	history := s.agg.Completed("NIFTY")

	if len(history) != 1 {
		t.Fatalf("expected exactly 1 completed candle, got %d", len(history))
	}
	got := history[0]
	if !got.Time.Equal(base) {
		t.Errorf("expected candle Time to be bucket start %v, got %v", base, got.Time)
	}
	// Volume must be the cumulative delta across the bucket's ticks
	// (1500 - 1000 = 500), NOT a tick count (3).
	if got.Volume != 500 {
		t.Errorf("expected volume delta 500, got %d", got.Volume)
	}
}

func TestSniperEvaluate_LatchesWithinCandle(t *testing.T) {
	s := NewSniperStrategy()
	s.EMAPeriod = 2
	s.RSIPeriod = 2
	s.CandleMinutes = 5

	base := time.Date(2026, 1, 19, 9, 15, 0, 0, IST)

	// Bootstrap enough completed candles (EMAPeriod+2 = 4) by sending one
	// tick per bucket across 5 buckets.
	price := 100.0
	for i := 0; i < 5; i++ {
		tickTime := base.Add(time.Duration(i) * 5 * time.Minute)
		price++
		ctx := EvalContext{Symbol: "NIFTY", Prices: []float64{price}, Volumes: []float64{1000 + float64(i)*100}, Now: tickTime}
		s.Evaluate(ctx)
	}

	// First tick of the 6th bucket completes candle index 4 and must run
	// the real strategy logic (not the latch).
	bucket6Start := base.Add(5 * 5 * time.Minute)
	sig1 := s.Evaluate(EvalContext{Symbol: "NIFTY", Prices: []float64{price + 1}, Volumes: []float64{1600}, Now: bucket6Start})
	if sig1.Reason == "Waiting for candle close" {
		t.Fatalf("expected the candle-completing tick to run strategy logic, got latch Hold")
	}

	// Subsequent ticks within the SAME bucket must be latched Hold.
	for i := 1; i <= 3; i++ {
		tick := EvalContext{
			Symbol:  "NIFTY",
			Prices:  []float64{price + 1 + float64(i)},
			Volumes: []float64{1600 + float64(i)*10},
			Now:     bucket6Start.Add(time.Duration(i) * time.Minute),
		}
		sig := s.Evaluate(tick)
		if sig.Action != Hold || sig.Reason != "Waiting for candle close" {
			t.Errorf("tick %d within same candle should be latched Hold, got Action=%v Reason=%q", i, sig.Action, sig.Reason)
		}
	}
}

func TestSniperEvaluate_CandleModeBypassesAggregation(t *testing.T) {
	s := NewSniperStrategy()
	s.EMAPeriod = 2
	s.RSIPeriod = 2

	history := make([]Candle, 0, 6)
	base := time.Date(2026, 1, 19, 9, 15, 0, 0, IST)
	for i := 0; i < 6; i++ {
		history = append(history, Candle{
			Time:  base.Add(time.Duration(i) * 5 * time.Minute),
			Open:  100 + float64(i),
			High:  101 + float64(i),
			Low:   99 + float64(i),
			Close: 100 + float64(i),
		})
	}
	sig := s.Evaluate(EvalContext{Candles: history})
	if sig.Reason == "No data" {
		t.Errorf("candle-mode evaluate incorrectly fell into the tick-mode 'No data' path")
	}
}

func TestSniperEvaluate_EmptyContextReturnsHold(t *testing.T) {
	s := NewSniperStrategy()
	sig := s.Evaluate(EvalContext{})
	if sig.Action != Hold {
		t.Errorf("expected Hold for empty context, got %v", sig.Action)
	}
}

func TestSniperAttachStops(t *testing.T) {
	s := NewSniperStrategy()
	s.StopLossPct = 1.0
	s.TargetPct = 2.0

	buy := Signal{Action: Buy}
	s.attachStops(&buy, 100)
	if !approxEqual(buy.StopLossPrice, 99, 1e-9) {
		t.Errorf("expected buy stop 99, got %v", buy.StopLossPrice)
	}
	if !approxEqual(buy.TargetPrice, 102, 1e-9) {
		t.Errorf("expected buy target 102, got %v", buy.TargetPrice)
	}

	sell := Signal{Action: Sell}
	s.attachStops(&sell, 100)
	if !approxEqual(sell.StopLossPrice, 101, 1e-9) {
		t.Errorf("expected sell stop 101, got %v", sell.StopLossPrice)
	}
	if !approxEqual(sell.TargetPrice, 98, 1e-9) {
		t.Errorf("expected sell target 98, got %v", sell.TargetPrice)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// R2-2 / G-2 regression coverage: proves the fixed sniper actually trades on
// a textbook candle pattern (it produced ZERO trades in every WP-10
// walk-forward window before this fix) and that the priority bug (Prices
// always populated -> candle-mode dead) cannot recur.
// ─────────────────────────────────────────────────────────────────────────

// buildTrendingHammerAndEngulfingSeries returns a candle series that starts
// with a clean uptrend (strictly increasing closes, so RSI reads 100 the
// whole way per the ST-1 fix), contains a textbook Hammer candle, a small
// bearish dip, then a textbook Bullish Engulfing candle — driving through
// internal/backtest/engine.go's own feeding pattern (growing prefix history
// via EvalContext.Candles, one candle at a time).
func buildTrendingHammerAndEngulfingSeries() []Candle {
	base := time.Date(2026, 1, 19, 9, 15, 0, 0, IST)
	bar := func(i int, o, h, l, c float64) Candle {
		return Candle{Time: base.Add(time.Duration(i) * 5 * time.Minute), Open: o, High: h, Low: l, Close: c}
	}
	return []Candle{
		bar(0, 100.0, 100.5, 99.8, 100.0),
		bar(1, 100.0, 101.5, 99.9, 101.0),
		bar(2, 101.0, 102.5, 100.9, 102.0),
		bar(3, 102.0, 103.5, 101.9, 103.0),
		bar(4, 103.0, 104.5, 102.9, 104.0),
		// Textbook Hammer: small body near the top, long lower wick (>=2x
		// body), negligible upper wick.
		bar(5, 104.5, 105.2, 102.0, 105.0),
		// Small bearish dip.
		bar(6, 105.0, 105.3, 104.0, 104.3),
		// Textbook Bullish Engulfing: body strictly bigger than bar 6's and
		// strictly engulfs it on at least one side.
		bar(7, 104.0, 106.6, 103.9, 106.5),
	}
}

func TestSniper_HammerThenEngulfing_CandleMode_EmitsBuy(t *testing.T) {
	s := NewSniper(SniperParams{EMAPeriod: 3, RSIPeriod: 3})
	series := buildTrendingHammerAndEngulfingSeries()

	var buys int
	for i := range series {
		history := series[:i+1]
		sig := s.Evaluate(EvalContext{Symbol: "NIFTY", Candles: history})
		if sig.Action == Buy {
			buys++
		}
	}
	if buys == 0 {
		t.Fatalf("expected at least one Buy signal from the hammer/engulfing series, got none")
	}
}

// TestSniper_CandlesTakePriorityOverPrices reproduces the exact WP-10 bug
// mechanism: the caller (runner.go / backtest engine.go) ALWAYS populates
// EvalContext.Prices in addition to Candles. Before the R2-2 fix, that made
// the candle-mode gate (`len(ctx.Prices)==0 && len(ctx.Candles)>0`)
// permanently false, so sniper always fell through to tick-aggregation and
// this exact call pattern produced zero Buy signals. Post-fix, Candles must
// win regardless of Prices.
func TestSniper_CandlesTakePriorityOverPrices(t *testing.T) {
	s := NewSniper(SniperParams{EMAPeriod: 3, RSIPeriod: 3})
	series := buildTrendingHammerAndEngulfingSeries()

	var buys int
	for i := range series {
		history := series[:i+1]
		closes := make([]float64, len(history))
		for j, c := range history {
			closes[j] = c.Close // Prices always non-empty, exactly like runner.go/engine.go
		}
		sig := s.Evaluate(EvalContext{Symbol: "NIFTY", Candles: history, Prices: closes})
		if sig.Action == Buy {
			buys++
		}
	}
	if buys == 0 {
		t.Fatalf("expected Candles to take priority over a simultaneously-populated Prices and still emit a Buy signal (this is the exact WP-10 regression)")
	}
}

// TestSniper_FlatDojiSeries_NoSignal: an all-equal-price series can never
// contain a Hammer/Engulfing/ShootingStar (every candle has zero range and
// zero body), so it must never emit Buy or Sell regardless of how much
// history accumulates.
func TestSniper_FlatDojiSeries_NoSignal(t *testing.T) {
	s := NewSniper(SniperParams{EMAPeriod: 2, RSIPeriod: 2})
	base := time.Date(2026, 1, 19, 9, 15, 0, 0, IST)
	var history []Candle
	for i := 0; i < 12; i++ {
		history = append(history, Candle{
			Time: base.Add(time.Duration(i) * 5 * time.Minute),
			Open: 100, High: 100, Low: 100, Close: 100,
		})
	}
	for i := range history {
		sig := s.Evaluate(EvalContext{Symbol: "NIFTY", Candles: history[:i+1]})
		if sig.Action == Buy || sig.Action == Sell {
			t.Fatalf("flat/doji series must never signal, got %v at candle %d (%s)", sig.Action, i, sig.Reason)
		}
	}
}

// TestCandleAggregator_TickFallbackBuildsRealRangeCandles proves the
// degraded tick-aggregation fallback (used only when the caller supplies no
// Candles) builds a genuine-range OHLC candle from multiple ticks landing
// in the same bucket — not a zero-range doji repeating the latest tick.
func TestCandleAggregator_TickFallbackBuildsRealRangeCandles(t *testing.T) {
	agg := NewCandleAggregator(5*time.Minute, 0)
	base := time.Date(2026, 1, 19, 9, 15, 0, 0, IST)

	ticks := []float64{100, 103, 98, 101} // real intra-bucket movement, not a flat repeat
	for i, p := range ticks {
		agg.Add("NIFTY", p, 1000+float64(i)*10, base.Add(time.Duration(i)*time.Minute))
	}
	// Cross into the next bucket to complete the first one.
	agg.Add("NIFTY", 101, 1050, base.Add(5*time.Minute))

	history := agg.Completed("NIFTY")
	if len(history) != 1 {
		t.Fatalf("expected exactly 1 completed candle, got %d", len(history))
	}
	c := history[0]
	if c.Open != 100 || c.Close != 101 {
		t.Errorf("expected Open=100 Close=101, got Open=%v Close=%v", c.Open, c.Close)
	}
	if c.High != 103 || c.Low != 98 {
		t.Errorf("expected High=103 Low=98, got High=%v Low=%v", c.High, c.Low)
	}
	if c.High == c.Low {
		t.Fatalf("real-range candle must not have High==Low (that would be the doji bug)")
	}
}

// TestSniper_SingleTickPerBucket_IsLegitimateDoji_NotABug documents the one
// remaining edge case: if a caller only ever provides ONE tick per interval
// (e.g. a driving loop that steps once per bar instead of streaming real
// sub-bar ticks), the aggregated candle is legitimately Open==High==Low==
// Close — there is no second data point to derive a range from. This is
// correct behavior for the aggregator, not the bug WP-10 found; the bug was
// the priority gate (fixed above), which made this degraded path run even
// when full Candles were already available.
func TestSniper_SingleTickPerBucket_IsLegitimateDoji_NotABug(t *testing.T) {
	agg := NewCandleAggregator(5*time.Minute, 0)
	base := time.Date(2026, 1, 19, 9, 15, 0, 0, IST)

	agg.Add("NIFTY", 100, 1000, base)
	agg.Add("NIFTY", 105, 1010, base.Add(5*time.Minute)) // completes bucket 1

	history := agg.Completed("NIFTY")
	if len(history) != 1 {
		t.Fatalf("expected exactly 1 completed candle, got %d", len(history))
	}
	c := history[0]
	if c.Open != 100 || c.High != 100 || c.Low != 100 || c.Close != 100 {
		t.Errorf("expected a single-tick doji (O=H=L=C=100), got %+v", c)
	}
}
