package backtest

import "math"

// OptionKind distinguishes calls from puts for Black-Scholes.
type OptionKind string

const (
	CallOption OptionKind = "CE"
	PutOption  OptionKind = "PE"
)

// BSParams are the inputs to the closed-form European Black-Scholes model.
//
// v1 LIMITATION (CR-9, documented deliberately): Vol is a single constant
// supplied per backtest run (Config.IV, default 12% for NIFTY). Real
// options have a volatility surface (skew across strikes, term structure
// across expiries, and IV that itself moves with realized vol day to day).
// Reconstructing a historical per-strike IV series (e.g. by inverting
// Black-Scholes against real option candle closes) is NOT implemented here
// — it is future work (see docs/reports/WP-7-REPORT.md). This v1 still
// fixes the CR-9 defect (constant delta 0.5, zero gamma) because full
// repricing on spot + time decay under ANY fixed vol correctly reintroduces
// delta, gamma and theta; only the vol *level itself* is a simplification.
type BSParams struct {
	Spot         float64
	Strike       float64
	TimeToExpiry float64 // years
	Rate         float64 // annualized, e.g. 0.065
	Vol          float64 // annualized, e.g. 0.12
	Kind         OptionKind
}

// minTimeToExpiry: below this we price at intrinsic value instead of
// dividing by a ~zero sqrt(T) (avoids NaN as options approach expiry).
const minTimeToExpiry = 1.0 / (365.0 * 24.0 * 60.0) // ~1 minute, in years

// Price returns the theoretical European premium under Black-Scholes.
func Price(p BSParams) float64 {
	if p.TimeToExpiry <= minTimeToExpiry || p.Vol <= 0 {
		return intrinsic(p)
	}
	d1, d2 := d1d2(p)
	discStrike := p.Strike * math.Exp(-p.Rate*p.TimeToExpiry)
	if p.Kind == PutOption {
		return discStrike*normCDF(-d2) - p.Spot*normCDF(-d1)
	}
	return p.Spot*normCDF(d1) - discStrike*normCDF(d2)
}

// Delta returns dPremium/dSpot.
func Delta(p BSParams) float64 {
	if p.TimeToExpiry <= minTimeToExpiry || p.Vol <= 0 {
		return intrinsicDelta(p)
	}
	d1, _ := d1d2(p)
	if p.Kind == PutOption {
		return normCDF(d1) - 1
	}
	return normCDF(d1)
}

func d1d2(p BSParams) (float64, float64) {
	sqrtT := math.Sqrt(p.TimeToExpiry)
	d1 := (math.Log(p.Spot/p.Strike) + (p.Rate+0.5*p.Vol*p.Vol)*p.TimeToExpiry) / (p.Vol * sqrtT)
	return d1, d1 - p.Vol*sqrtT
}

// normCDF is the standard normal CDF, via erfc for numerical stability.
func normCDF(x float64) float64 {
	return 0.5 * math.Erfc(-x/math.Sqrt2)
}

func intrinsic(p BSParams) float64 {
	if p.Kind == PutOption {
		return math.Max(p.Strike-p.Spot, 0)
	}
	return math.Max(p.Spot-p.Strike, 0)
}

func intrinsicDelta(p BSParams) float64 {
	if p.Kind == PutOption {
		if p.Spot < p.Strike {
			return -1
		}
		return 0
	}
	if p.Spot > p.Strike {
		return 1
	}
	return 0
}
