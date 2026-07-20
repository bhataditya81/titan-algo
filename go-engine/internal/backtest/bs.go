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
// Vol per leg/bar comes from Config.IVAt: a real per-bar implied vol backed
// out of matching option candles (see iv.go's ImpliedVol/BuildIVSeries) when
// available, else the single constant Config.IV fallback (default 12% for
// NIFTY). Real options have a full volatility surface (skew across strikes,
// term structure across expiries) — this reconstructs the IV *level* per
// bar for the one contract fetched, not a full surface; strikes/expiries
// without a fetched option series still use the constant fallback.
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
