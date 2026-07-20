package backtest

import (
	"testing"
	"time"
)

// TestIVAt_FallsBackToConstantWhenNoRealSeries proves the default (no
// -option-csv) behavior is unchanged: every leg uses the constant IV.
func TestIVAt_FallsBackToConstantWhenNoRealSeries(t *testing.T) {
	cfg := DefaultConfig()
	asOf := time.Date(2026, 1, 5, 9, 15, 0, 0, IST)
	got := cfg.IVAt("CE", 25000, asOf, asOf)
	if got != cfg.IV {
		t.Fatalf("want constant IV %.4f when RealIVSeries is nil, got %.4f", cfg.IV, got)
	}
}

// TestIVAt_UsesRealSeriesForMatchingLeg proves the actual wiring: a leg
// whose optionType/strike/expiry exactly match RealIVSeries gets the real
// per-bar value, not the constant fallback.
func TestIVAt_UsesRealSeriesForMatchingLeg(t *testing.T) {
	cfg := DefaultConfig()
	expiry := time.Date(2026, 1, 29, 15, 30, 0, 0, IST)
	bar := time.Date(2026, 1, 5, 9, 15, 0, 0, IST)
	const realIV = 0.35 // deliberately far from cfg.IV's 0.12 default -- proves it's not coincidental
	cfg.RealIVSeries = map[time.Time]float64{bar: realIV}
	cfg.RealIVOptionType = "CE"
	cfg.RealIVStrike = 25000
	cfg.RealIVExpiry = expiry

	got := cfg.IVAt("CE", 25000, expiry, bar)
	if got != realIV {
		t.Fatalf("want real IV %.4f for the matching leg/bar, got %.4f", realIV, got)
	}
}

// TestIVAt_FallsBackForNonMatchingLeg proves that a DIFFERENT leg (wrong
// option type, strike, or expiry) never borrows another contract's IV --
// each dimension is checked independently.
func TestIVAt_FallsBackForNonMatchingLeg(t *testing.T) {
	cfg := DefaultConfig()
	expiry := time.Date(2026, 1, 29, 15, 30, 0, 0, IST)
	otherExpiry := time.Date(2026, 2, 26, 15, 30, 0, 0, IST)
	bar := time.Date(2026, 1, 5, 9, 15, 0, 0, IST)
	cfg.RealIVSeries = map[time.Time]float64{bar: 0.35}
	cfg.RealIVOptionType = "CE"
	cfg.RealIVStrike = 25000
	cfg.RealIVExpiry = expiry

	cases := []struct {
		name             string
		optType          string
		strike           float64
		expiry           time.Time
		asOf             time.Time
	}{
		{"wrong option type (PE vs CE)", "PE", 25000, expiry, bar},
		{"wrong strike", "CE", 25100, expiry, bar},
		{"wrong expiry", "CE", 25000, otherExpiry, bar},
		{"wrong bar (no matching timestamp)", "CE", 25000, expiry, bar.Add(5 * time.Minute)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cfg.IVAt(tc.optType, tc.strike, tc.expiry, tc.asOf)
			if got != cfg.IV {
				t.Fatalf("want constant-IV fallback %.4f for %s, got %.4f (leaked real IV to a non-matching leg/bar)", cfg.IV, tc.name, got)
			}
		})
	}
}

// TestPriceLeg_UsesRealIVWhenConfigured is the end-to-end proof: priceLeg
// (the actual Black-Scholes call site) produces a DIFFERENT price for the
// matching leg when real IV is wired in, versus the constant-IV price --
// proving Config.IVAt is actually consulted in the pricing path, not just
// correct in isolation.
func TestPriceLeg_UsesRealIVWhenConfigured(t *testing.T) {
	expiry := time.Date(2026, 1, 29, 15, 30, 0, 0, IST)
	bar := time.Date(2026, 1, 5, 9, 15, 0, 0, IST)
	spot := 25000.0
	strike := 25000.0

	constCfg := DefaultConfig() // IV = 0.12
	constPrice := priceLeg("CE", strike, expiry, bar, spot, constCfg)

	realCfg := DefaultConfig()
	realCfg.RealIVSeries = map[time.Time]float64{bar: 0.35}
	realCfg.RealIVOptionType = "CE"
	realCfg.RealIVStrike = strike
	realCfg.RealIVExpiry = expiry
	realPrice := priceLeg("CE", strike, expiry, bar, spot, realCfg)

	if realPrice == constPrice {
		t.Fatalf("expected real-IV price (0.35 vol) to differ from constant-IV price (0.12 vol), both came out %.4f -- IVAt isn't being consulted", constPrice)
	}
	if realPrice <= constPrice {
		t.Fatalf("higher IV (0.35 vs 0.12) must price the option higher (more time value), got real=%.4f <= const=%.4f", realPrice, constPrice)
	}

	// The OTHER leg of a real multi-leg strategy (different strike) must be
	// completely unaffected -- still priced at the constant IV.
	otherLegConst := priceLeg("CE", 25200, expiry, bar, spot, constCfg)
	otherLegReal := priceLeg("CE", 25200, expiry, bar, spot, realCfg)
	if otherLegConst != otherLegReal {
		t.Fatalf("a non-matching leg (different strike) must price identically with or without RealIVSeries set; got const=%.4f real=%.4f", otherLegConst, otherLegReal)
	}
}
