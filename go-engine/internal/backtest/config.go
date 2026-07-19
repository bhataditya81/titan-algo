package backtest

// Config holds every tunable of the simulation. All fields have sane
// NIFTY-oriented defaults via DefaultConfig().
type Config struct {
	Symbol     string
	LotSize    int // ST-10/M3: no hardcoded 50; CLI -lotsize, default 75 (NIFTY post-Apr-2025)
	MinHistory int // candles required before the strategy is evaluated

	// Black-Scholes inputs (CR-9).
	RiskFreeRate   float64 // annualized, e.g. 0.065 for 6.5%
	IV             float64 // annualized constant IV, e.g. 0.12 for 12% (NIFTY v1 default)
	DefaultDTEDays int     // fallback days-to-expiry when a leg has no/unparseable Expiry
	StrikeStep     float64 // ATM strike rounding step, e.g. 50 for NIFTY

	// Fill/cost model (ST-6, ST-7).
	OptionTickSize    float64 // minimum price increment, e.g. 0.05
	OptionSlippagePct float64 // fraction of premium; max(this, 1 tick) charged as fill slippage
	HalfSpreadPct     float64 // fraction of premium charged per leg per side as spread cost (ST-6 default 0.3%)
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
