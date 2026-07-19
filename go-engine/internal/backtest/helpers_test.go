package backtest

import (
	"math"
	"time"
)

// fakeStrategy lets tests script exact signal sequences without depending
// on internal/strategy (which may not have landed WP-6's contract yet).
type fakeStrategy struct {
	name string
	fn   func(ctx EvalContext) Signal
}

func (f *fakeStrategy) Name() string                    { return f.name }
func (f *fakeStrategy) Evaluate(ctx EvalContext) Signal { return f.fn(ctx) }

// genFlatCandles builds n 5-min candles all at the same price -- a neutral
// baseline for tests that only care about signal/fill timing, not P&L.
func genFlatCandles(n int, price float64, start time.Time) []Candle {
	cs := make([]Candle, n)
	for i := 0; i < n; i++ {
		cs[i] = Candle{
			Time:  start.Add(time.Duration(i) * 5 * time.Minute),
			Open:  price,
			High:  price,
			Low:   price,
			Close: price,
		}
	}
	return cs
}

// genTrendCandles builds n 5-min candles linearly trending from startPrice
// to endPrice -- a strongly directional synthetic market for proving gamma
// risk (short straddle should lose on a sustained trend, CR-9).
func genTrendCandles(n int, startPrice, endPrice float64, start time.Time) []Candle {
	cs := make([]Candle, n)
	prev := startPrice
	for i := 0; i < n; i++ {
		frac := float64(i+1) / float64(n)
		price := startPrice + (endPrice-startPrice)*frac
		cs[i] = Candle{
			Time:  start.Add(time.Duration(i) * 5 * time.Minute),
			Open:  prev,
			High:  math.Max(prev, price),
			Low:   math.Min(prev, price),
			Close: price,
		}
		prev = price
	}
	return cs
}
