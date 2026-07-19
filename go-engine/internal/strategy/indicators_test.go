package strategy

import (
	"math"
	"testing"
	"time"
)

func approxEqual(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}

func TestRSIBoundaryCases(t *testing.T) {
	// All gains, no losses at all -> RSI must be exactly 100 (ST-1 fix).
	// Strictly increasing prices for period+1 points.
	allUp := []float64{100, 101, 102, 103, 104, 105}
	rsi := CalculateRSI(allUp, 5)
	if rsi == nil {
		t.Fatalf("expected non-nil RSI for all-up prices, got nil")
	}
	if !approxEqual(rsi.Value, 100, 1e-9) {
		t.Errorf("expected RSI=100 for all-gain series, got %v", rsi.Value)
	}

	// All losses, no gains at all -> RSI must be exactly 0.
	allDown := []float64{105, 104, 103, 102, 101, 100}
	rsi = CalculateRSI(allDown, 5)
	if rsi == nil {
		t.Fatalf("expected non-nil RSI for all-down prices, got nil")
	}
	if !approxEqual(rsi.Value, 0, 1e-9) {
		t.Errorf("expected RSI=0 for all-loss series, got %v", rsi.Value)
	}

	// Flat prices (no gain, no loss anywhere) -> RSI must be the neutral 50.
	flat := []float64{100, 100, 100, 100, 100, 100}
	rsi = CalculateRSI(flat, 5)
	if rsi == nil {
		t.Fatalf("expected non-nil RSI for flat prices, got nil")
	}
	if !approxEqual(rsi.Value, 50, 1e-9) {
		t.Errorf("expected RSI=50 for flat series, got %v", rsi.Value)
	}
}

func TestRSIInsufficientData(t *testing.T) {
	if rsi := CalculateRSI([]float64{100, 101}, 14); rsi != nil {
		t.Errorf("expected nil RSI for insufficient data, got %v", rsi)
	}
}

func mkCandle(dayOffset int, hh, mm int, o, h, l, c float64, vol int64) Candle {
	base := time.Date(2026, 1, 19, hh, mm, 0, 0, IST).AddDate(0, 0, dayOffset)
	return Candle{Time: base, Open: o, High: h, Low: l, Close: c, Volume: vol}
}

func TestSessionVWAPResetsAtDayBoundary(t *testing.T) {
	// Day 1 session: two candles.
	day1 := []Candle{
		mkCandle(0, 9, 15, 100, 102, 99, 101, 1000),
		mkCandle(0, 9, 20, 101, 103, 100, 102, 2000),
	}
	// Day 2 session: two different candles. Only day 2 must be used.
	day2 := []Candle{
		mkCandle(1, 9, 15, 200, 202, 199, 201, 500),
		mkCandle(1, 9, 20, 201, 205, 200, 204, 1500),
	}

	all := append(append([]Candle{}, day1...), day2...)

	vwap := CalculateSessionVWAP(all)
	if vwap == nil {
		t.Fatalf("expected non-nil VWAP")
	}

	// Hand-computed expected value using ONLY day2 candles, typical price.
	tp1 := (202.0 + 199.0 + 201.0) / 3.0 // 200.6667
	tp2 := (205.0 + 200.0 + 204.0) / 3.0 // 203.0
	expected := (tp1*500 + tp2*1500) / (500 + 1500)

	if !approxEqual(vwap.Value, expected, 1e-6) {
		t.Errorf("expected session VWAP %.6f (day2 only), got %.6f", expected, vwap.Value)
	}

	// Sanity: if it had incorrectly included day1, the value would differ
	// significantly (day1 prices are ~100, day2 ~200).
	if vwap.Value < 150 {
		t.Errorf("session VWAP %.2f looks like it leaked day1 data (expected ~200 range)", vwap.Value)
	}
}

func TestSessionVWAPEmpty(t *testing.T) {
	if v := CalculateSessionVWAP(nil); v != nil {
		t.Errorf("expected nil VWAP for empty candles, got %v", v)
	}
}

func TestMomentumNormalizationHandlesMissingVWAP(t *testing.T) {
	m := NewMomentumStrategy()
	rsi := &RSI{Value: 20}       // strongly oversold -> full 0.3 weight
	macd := &MACD{Bullish: true} // full 0.3 weight
	bb := &BollingerBands{Lower: 90, Middle: 100, Upper: 110}
	price := 85.0 // at/below lower band -> full 0.2 weight

	// With VWAP present but not favorable, weight includes the 0.2 VWAP slot.
	scoreWithVWAP := m.evaluateBuyConditions(price, rsi, macd, bb, &VWAP{Value: 200}) // price way below vwap -> full 0.2
	// With VWAP absent entirely, the 0.2 slot must not participate in the
	// denominator either — the achievable max should still normalize to 1.0
	// when all *evaluable* conditions are maximally satisfied.
	scoreNoVWAP := m.evaluateBuyConditions(price, rsi, macd, bb, nil)

	if !approxEqual(scoreWithVWAP, 1.0, 1e-9) {
		t.Errorf("expected fully-satisfied score with VWAP = 1.0, got %v", scoreWithVWAP)
	}
	if !approxEqual(scoreNoVWAP, 1.0, 1e-9) {
		t.Errorf("expected fully-satisfied score without VWAP to still normalize to 1.0, got %v", scoreNoVWAP)
	}
}
