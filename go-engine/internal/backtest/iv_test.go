package backtest

import (
	"math"
	"testing"
	"time"
)

// TestImpliedVol_GoldenRoundTrip is the G-4 golden test: price a known
// option at a known IV via bs.go's Price, then invert it back with
// ImpliedVol and confirm we recover the original IV within tolerance.
func TestImpliedVol_GoldenRoundTrip(t *testing.T) {
	cases := []struct {
		name                   string
		spot, strike, t, r, iv float64
		kind                   OptionKind
	}{
		{"ATM call, 1yr, 20%vol", 100, 100, 1.0, 0.05, 0.20, CallOption},
		{"ATM put, 1yr, 20%vol", 100, 100, 1.0, 0.05, 0.20, PutOption},
		{"OTM call, 30d, NIFTY-ish", 20000, 20500, 30.0 / 365, 0.065, 0.12, CallOption},
		{"ITM put, 7d", 20000, 20500, 7.0 / 365, 0.065, 0.15, PutOption},
		{"deep OTM call, high vol", 20000, 23000, 45.0 / 365, 0.065, 0.35, CallOption},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			price := Price(BSParams{Spot: c.spot, Strike: c.strike, TimeToExpiry: c.t, Rate: c.r, Vol: c.iv, Kind: c.kind})

			got, err := ImpliedVol(price, c.spot, c.strike, c.t, c.r, c.kind)
			if err != nil {
				t.Fatalf("ImpliedVol returned error: %v", err)
			}
			if math.Abs(got-c.iv) > 1e-4 {
				t.Errorf("round trip: priced at iv=%.4f, recovered iv=%.4f (diff %.6f)", c.iv, got, got-c.iv)
			}

			// Reprice at the recovered IV and confirm it reproduces the
			// same market price (the actual round-trip proof, not just
			// "close enough on vol").
			reprice := Price(BSParams{Spot: c.spot, Strike: c.strike, TimeToExpiry: c.t, Rate: c.r, Vol: got, Kind: c.kind})
			if math.Abs(reprice-price) > 1e-3 {
				t.Errorf("reprice at recovered iv = %.6f, want original price %.6f", reprice, price)
			}
		})
	}
}

func TestImpliedVol_BelowIntrinsicIsError(t *testing.T) {
	// Deep ITM call: intrinsic = 21000-20000 = 1000. A quoted price of 500
	// is below intrinsic -- an arbitrage violation, not a vol problem.
	_, err := ImpliedVol(500, 21000, 20000, 0.1, 0.065, CallOption)
	if err == nil {
		t.Fatal("expected an error for a below-intrinsic market price, got nil")
	}
}

func TestImpliedVol_ImplausiblyHighPriceIsError(t *testing.T) {
	// A price far above what even 500% vol could produce.
	_, err := ImpliedVol(1e9, 100, 100, 0.1, 0.065, CallOption)
	if err == nil {
		t.Fatal("expected an error for an implausibly high market price, got nil")
	}
}

func TestImpliedVol_NonPositivePriceIsError(t *testing.T) {
	if _, err := ImpliedVol(0, 100, 100, 0.1, 0.065, CallOption); err == nil {
		t.Fatal("expected an error for a zero market price")
	}
	if _, err := ImpliedVol(-5, 100, 100, 0.1, 0.065, CallOption); err == nil {
		t.Fatal("expected an error for a negative market price")
	}
}

// TestBuildIVSeries_MatchesByTimeAndSkipsGaps proves the per-bar wiring
// point (G-4 task 2) works: it aligns option candles to underlying candles
// by timestamp and silently skips bars with no matching option print.
func TestBuildIVSeries_MatchesByTimeAndSkipsGaps(t *testing.T) {
	start := time.Date(2026, 1, 5, 9, 15, 0, 0, IST)
	expiry := time.Date(2026, 1, 29, 15, 30, 0, 0, IST)
	strike := 20000.0
	rate := 0.065
	iv := 0.15

	underlying := genFlatCandles(5, 20000, start)

	// Build option candles priced at the true iv for bars 0,1,3 (skip bar 2
	// entirely -- simulates a missing print/gap).
	var optionCandles []Candle
	for i, uc := range underlying {
		if i == 2 {
			continue
		}
		T := timeToExpiryYears(uc.Time, expiry)
		price := Price(BSParams{Spot: uc.Close, Strike: strike, TimeToExpiry: T, Rate: rate, Vol: iv, Kind: CallOption})
		optionCandles = append(optionCandles, Candle{Time: uc.Time, Open: price, High: price, Low: price, Close: price})
	}

	series := BuildIVSeries(underlying, optionCandles, strike, rate, expiry, CallOption)

	if len(series) != 4 {
		t.Fatalf("expected 4 matched bars (5 underlying - 1 gap), got %d", len(series))
	}
	if _, ok := series[underlying[2].Time]; ok {
		t.Errorf("bar 2 had no option print and should have been skipped, but was present")
	}
	for i, uc := range underlying {
		if i == 2 {
			continue
		}
		got, ok := series[uc.Time]
		if !ok {
			t.Fatalf("bar %d missing from series", i)
		}
		if math.Abs(got-iv) > 1e-4 {
			t.Errorf("bar %d: implied iv = %.4f, want ~%.4f", i, got, iv)
		}
	}

	stats := SummarizeIVSeries(series)
	if stats.Count != 4 {
		t.Errorf("stats.Count = %d, want 4", stats.Count)
	}
	if math.Abs(stats.Mean-iv) > 1e-4 {
		t.Errorf("stats.Mean = %.4f, want ~%.4f", stats.Mean, iv)
	}
}

func TestSummarizeIVSeries_Empty(t *testing.T) {
	stats := SummarizeIVSeries(nil)
	if stats.Count != 0 || stats.Mean != 0 || stats.Min != 0 || stats.Max != 0 {
		t.Errorf("expected zero-value stats for empty series, got %+v", stats)
	}
}
