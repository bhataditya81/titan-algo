package backtest

import (
	"fmt"
	"math"
	"time"
)

// ImpliedVol inverts Black-Scholes (bs.go's Price) via bisection: given an
// observed market premium, finds the constant annualized vol that
// reproduces it under the same Spot/Strike/TimeToExpiry/Rate. Bisection
// (not Newton-Raphson) per G-4's own instruction: Price(vol) is monotonic
// increasing in vol for T > minTimeToExpiry, so bisection is simple and
// robust without needing vega (which bs.go doesn't otherwise compute).
//
// Returns an error (never a guessed/garbage value) when marketPrice is
// below intrinsic value (arbitrage violation -- not a vol-inversion
// problem) or above the price implied by the upper vol bound (implausible
// input, e.g. a bad print or wrong strike/expiry pairing).
func ImpliedVol(marketPrice, spot, strike, timeToExpiry, rate float64, kind OptionKind) (float64, error) {
	if marketPrice <= 0 {
		return 0, fmt.Errorf("backtest: implied vol: non-positive market price %.4f", marketPrice)
	}
	p := BSParams{Spot: spot, Strike: strike, TimeToExpiry: timeToExpiry, Rate: rate, Kind: kind}

	intrinsicVal := intrinsic(p)
	if marketPrice < intrinsicVal-1e-6 {
		return 0, fmt.Errorf("backtest: implied vol: market price %.4f is below intrinsic value %.4f (S=%.2f K=%.2f T=%.6f) -- arbitrage violation, not a vol problem",
			marketPrice, intrinsicVal, spot, strike, timeToExpiry)
	}
	if timeToExpiry <= minTimeToExpiry {
		// At/after expiry, price IS intrinsic for any vol -- no unique vol
		// to solve for. Since we already checked marketPrice >= intrinsic
		// above, treat an (approximately) intrinsic price as vol-agnostic
		// and report 0 rather than iterating on a degenerate bracket.
		return 0, nil
	}

	const (
		loVol    = 1e-6
		hiVol    = 5.0 // 500% annualized -- generous upper bound for index options
		maxIter  = 200
		priceTol = 1e-6
		volTol   = 1e-8
	)

	lo, hi := loVol, hiVol
	p.Vol = hi
	if Price(p) < marketPrice {
		return 0, fmt.Errorf("backtest: implied vol: market price %.4f exceeds the price at %.0f%% vol (S=%.2f K=%.2f T=%.6f) -- implausible input",
			marketPrice, hiVol*100, spot, strike, timeToExpiry)
	}

	for i := 0; i < maxIter; i++ {
		mid := (lo + hi) / 2
		p.Vol = mid
		price := Price(p)
		diff := price - marketPrice
		if math.Abs(diff) < priceTol || (hi-lo) < volTol {
			return mid, nil
		}
		if diff > 0 {
			hi = mid
		} else {
			lo = mid
		}
	}
	return (lo + hi) / 2, nil
}

// BuildIVSeries computes, per underlying candle, the implied vol backed out
// of a matching option candle (same timestamp) for one fixed contract
// (strike/expiry/kind) -- the per-bar series G-4 calls for. Bars with no
// matching option candle, or an unrecoverable ImpliedVol error (bad print),
// are simply omitted rather than interpolated/guessed.
//
// NOTE (file-ownership, see ConstantIVBanner in report.go): this is NOT
// currently consumed by Run()/priceLeg in engine.go -- that section is
// owned by R2-2 this round. This function exists, is tested, and is ready
// for R2-2/R2-INT to wire in; see docs/reports/R2-3-REPORT.md for the exact
// one-line hookup once engine.go is open again.
func BuildIVSeries(underlying, optionCandles []Candle, strike, rate float64, expiry time.Time, kind OptionKind) map[time.Time]float64 {
	byTime := make(map[time.Time]Candle, len(optionCandles))
	for _, oc := range optionCandles {
		byTime[oc.Time] = oc
	}

	out := make(map[time.Time]float64, len(underlying))
	for _, uc := range underlying {
		oc, ok := byTime[uc.Time]
		if !ok {
			continue
		}
		T := timeToExpiryYears(uc.Time, expiry)
		iv, err := ImpliedVol(oc.Close, uc.Close, strike, T, rate, kind)
		if err != nil {
			continue
		}
		out[uc.Time] = iv
	}
	return out
}

// IVSeriesStats summarizes an IV series for the informational coverage
// note cmd/backtest prints when -option-csv is supplied (see G-4's
// requirement to surface real option data even though it isn't wired into
// repricing yet this round).
type IVSeriesStats struct {
	Count    int
	Mean     float64
	Min, Max float64
}

func SummarizeIVSeries(series map[time.Time]float64) IVSeriesStats {
	var s IVSeriesStats
	if len(series) == 0 {
		return s
	}
	first := true
	var sum float64
	for _, v := range series {
		sum += v
		if first {
			s.Min, s.Max = v, v
			first = false
			continue
		}
		if v < s.Min {
			s.Min = v
		}
		if v > s.Max {
			s.Max = v
		}
	}
	s.Count = len(series)
	s.Mean = sum / float64(len(series))
	return s
}
