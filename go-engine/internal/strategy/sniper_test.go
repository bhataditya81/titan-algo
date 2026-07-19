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

	s.mu.Lock()
	history := append([]Candle(nil), s.candles["NIFTY"]...)
	s.mu.Unlock()

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
