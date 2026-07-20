package backtest

import "time"

// Config holds every tunable of the simulation. All fields have sane
// NIFTY-oriented defaults via DefaultConfig().
type Config struct {
	Symbol     string
	LotSize    int // ST-10/M3: no hardcoded 50; CLI -lotsize, default 75 (NIFTY post-Apr-2025)
	MinHistory int // candles required before the strategy is evaluated

	// Black-Scholes inputs (CR-9).
	RiskFreeRate   float64 // annualized, e.g. 0.065 for 6.5%
	IV             float64 // annualized constant IV, e.g. 0.12 for 12% (NIFTY v1 default) -- fallback for any leg/bar RealIVSeries doesn't cover
	DefaultDTEDays int     // fallback days-to-expiry when a leg has no/unparseable Expiry
	StrikeStep     float64 // ATM strike rounding step, e.g. 50 for NIFTY

	// Real per-bar implied vol for ONE specific option contract (G-4), built
	// via BuildIVSeries from actual fetched option-candle data. Optional --
	// nil means every leg uses the constant IV above, exactly as before.
	// When set, ONLY the leg matching RealIVOptionType/RealIVStrike/
	// RealIVExpiry gets real per-bar IV; every other leg (e.g. iron_fly's
	// other three strikes) still uses the constant IV, because we only ever
	// have genuine historical option data for the one contract an operator
	// fetched -- see IVAt.
	RealIVSeries     map[time.Time]float64
	RealIVOptionType string // "CE" or "PE", matching priceLeg's kind param
	RealIVStrike     float64
	RealIVExpiry     time.Time

	// Fill/cost model (ST-6, ST-7).
	OptionTickSize    float64 // minimum price increment, e.g. 0.05
	OptionSlippagePct float64 // fraction of premium; max(this, 1 tick) charged as fill slippage
	HalfSpreadPct     float64 // fraction of premium charged per leg per side as spread cost (ST-6 default 0.3%)
}

// IVAt returns the volatility priceLeg should use to price one leg at one
// bar: the real per-bar implied vol from RealIVSeries when this leg's
// optionType/strike/expiry exactly match the contract RealIVSeries was
// built for AND a bar exists at asOf, otherwise the constant IV fallback.
// This never guesses IV for a contract we have no real data for -- it only
// ever upgrades the ONE leg we actually fetched real option prints for.
func (c Config) IVAt(optionType string, strike float64, expiry, asOf time.Time) float64 {
	if c.RealIVSeries == nil {
		return c.IV
	}
	if optionType != c.RealIVOptionType || strike != c.RealIVStrike || !expiry.Equal(c.RealIVExpiry) {
		return c.IV
	}
	if iv, ok := c.RealIVSeries[asOf]; ok {
		return iv
	}
	return c.IV
}

// DefaultConfig returns sane NIFTY defaults per the remediation plan.
func DefaultConfig() Config {
	return Config{
		Symbol:            "NIFTY",
		LotSize:           75,
		MinHistory:        52,
		RiskFreeRate:      0.065,
		IV:                0.12,
		DefaultDTEDays:    7,
		StrikeStep:        50,
		OptionTickSize:    0.05,
		OptionSlippagePct: 0.0005,
		HalfSpreadPct:     0.003,
	}
}
