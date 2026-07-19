package broker

import (
	"testing"
	"time"

	"titan-algo/internal/strategy"
)

func TestParseCandleRows_ValidRowsAccepted(t *testing.T) {
	rows := [][]interface{}{
		{"2026-01-19T09:15:00+05:30", 100.0, 105.0, 99.0, 102.0, 1000.0},
		{"2026-01-19T09:20:00+05:30", 102.0, 106.0, 101.0, 104.0, 1500.0},
	}
	candles, skipped := parseCandleRows(rows)
	if skipped != 0 {
		t.Fatalf("expected 0 skipped, got %d", skipped)
	}
	if len(candles) != 2 {
		t.Fatalf("expected 2 candles, got %d", len(candles))
	}
	if candles[0].Close != 102.0 {
		t.Errorf("expected close 102.0, got %v", candles[0].Close)
	}
}

func TestParseCandleRows_RejectsBadNumericStrings(t *testing.T) {
	// ST-10 fix: a malformed numeric field must be SKIPPED, not silently
	// coerced to 0.0 (which used to poison every downstream indicator).
	rows := [][]interface{}{
		{"2026-01-19T09:15:00+05:30", "not-a-number", 105.0, 99.0, 102.0, 1000.0},
	}
	candles, skipped := parseCandleRows(rows)
	if len(candles) != 0 {
		t.Fatalf("expected 0 candles for malformed numeric field, got %d: %+v", len(candles), candles)
	}
	if skipped != 1 {
		t.Errorf("expected 1 skipped row, got %d", skipped)
	}
}

func TestParseCandleRows_RejectsNonPositivePrices(t *testing.T) {
	rows := [][]interface{}{
		{"2026-01-19T09:15:00+05:30", 0.0, 105.0, 99.0, 102.0, 1000.0},   // Open = 0
		{"2026-01-19T09:20:00+05:30", 100.0, -5.0, 99.0, 102.0, 1000.0},  // High negative
		{"2026-01-19T09:25:00+05:30", 100.0, 105.0, 99.0, 102.0, 1000.0}, // valid
	}
	candles, skipped := parseCandleRows(rows)
	if len(candles) != 1 {
		t.Fatalf("expected 1 valid candle, got %d", len(candles))
	}
	if skipped != 2 {
		t.Errorf("expected 2 skipped rows, got %d", skipped)
	}
}

func TestParseCandleRows_RejectsHighLessThanLow(t *testing.T) {
	rows := [][]interface{}{
		{"2026-01-19T09:15:00+05:30", 100.0, 95.0, 99.0, 97.0, 1000.0}, // High(95) < Low(99)
	}
	candles, skipped := parseCandleRows(rows)
	if len(candles) != 0 {
		t.Fatalf("expected 0 candles for High<Low, got %d", len(candles))
	}
	if skipped != 1 {
		t.Errorf("expected 1 skipped row, got %d", skipped)
	}
}

func TestParseCandleRows_RejectsOutOfOrderTimestamps(t *testing.T) {
	rows := [][]interface{}{
		{"2026-01-19T09:20:00+05:30", 100.0, 105.0, 99.0, 102.0, 1000.0}, // later ts first
		{"2026-01-19T09:15:00+05:30", 100.0, 105.0, 99.0, 102.0, 1000.0}, // earlier ts second -> out of order
	}
	candles, skipped := parseCandleRows(rows)
	if len(candles) != 1 {
		t.Fatalf("expected 1 accepted candle (the first), got %d", len(candles))
	}
	if skipped != 1 {
		t.Errorf("expected 1 skipped out-of-order row, got %d", skipped)
	}
}

func TestParseCandleRows_ShortRowSkipped(t *testing.T) {
	rows := [][]interface{}{
		{"2026-01-19T09:15:00+05:30", 100.0, 105.0}, // too few fields
	}
	candles, skipped := parseCandleRows(rows)
	if len(candles) != 0 || skipped != 1 {
		t.Errorf("expected short row to be skipped, got candles=%d skipped=%d", len(candles), skipped)
	}
}

func TestGetFloat_PropagatesParseErrors(t *testing.T) {
	if _, err := getFloat("abc"); err == nil {
		t.Errorf("expected error for non-numeric string")
	}
	if v, err := getFloat("123.5"); err != nil || v != 123.5 {
		t.Errorf("expected 123.5, nil, got %v, %v", v, err)
	}
	if v, err := getFloat(42.0); err != nil || v != 42.0 {
		t.Errorf("expected 42.0, nil, got %v, %v", v, err)
	}
	if _, err := getFloat(true); err == nil {
		t.Errorf("expected error for unsupported type")
	}
}

func TestTruncateToLastCompletedCandle_IntradayDuringSession(t *testing.T) {
	// 11:32:47 IST, FIVE_MINUTE candles anchored at 09:15 -> last completed
	// boundary is 11:30:00 (09:15 + 43*5min = 11:30), NOT the in-progress
	// 11:30-11:35 bar.
	now := time.Date(2026, 1, 19, 11, 32, 47, 0, strategy.IST)
	got := truncateToLastCompletedCandle(now, "FIVE_MINUTE")
	want := time.Date(2026, 1, 19, 11, 30, 0, 0, strategy.IST)
	if !got.Equal(want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestTruncateToLastCompletedCandle_BeforeMarketOpen(t *testing.T) {
	now := time.Date(2026, 1, 19, 8, 0, 0, 0, strategy.IST)
	got := truncateToLastCompletedCandle(now, "FIVE_MINUTE")
	want := time.Date(2026, 1, 18, 15, 30, 0, 0, strategy.IST) // prior day's close
	if !got.Equal(want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestTruncateToLastCompletedCandle_AfterMarketClose(t *testing.T) {
	now := time.Date(2026, 1, 19, 16, 0, 0, 0, strategy.IST)
	got := truncateToLastCompletedCandle(now, "FIVE_MINUTE")
	want := time.Date(2026, 1, 19, 15, 30, 0, 0, strategy.IST) // capped at close
	if !got.Equal(want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestTruncateToLastCompletedCandle_DailyBeforeClose(t *testing.T) {
	now := time.Date(2026, 1, 19, 12, 0, 0, 0, strategy.IST)
	got := truncateToLastCompletedCandle(now, "ONE_DAY")
	want := time.Date(2026, 1, 18, 15, 30, 0, 0, strategy.IST) // today's daily bar not closed yet
	if !got.Equal(want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestTruncateToLastCompletedCandle_ConvertsNonISTInput(t *testing.T) {
	// A UTC "now" must be converted to IST before truncation (ST-3): UTC
	// 06:32 == IST 12:02, so the boundary should be IST 12:00, not something
	// computed off the raw UTC clock.
	nowUTC := time.Date(2026, 1, 19, 6, 32, 0, 0, time.UTC)
	got := truncateToLastCompletedCandle(nowUTC, "FIVE_MINUTE")
	want := time.Date(2026, 1, 19, 12, 0, 0, 0, strategy.IST)
	if !got.Equal(want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}
