package strategy

import (
	"fmt"
)

// EMACrossoverStrategy implements a classic trend following strategy
type EMACrossoverStrategy struct {
	FastPeriod int
	SlowPeriod int
}

// EMACrossoverParams configures NewEMACrossover. The zero value produces
// today's hardcoded defaults (FastPeriod=9, SlowPeriod=21) — this is what
// registry.Get("ema_crossover") constructs internally, so existing callers
// are unaffected by this parameterization (G-5).
type EMACrossoverParams struct {
	FastPeriod int
	SlowPeriod int
}

// NewEMACrossover builds an EMACrossoverStrategy from params, substituting
// the hardcoded default for any zero-valued field.
func NewEMACrossover(p EMACrossoverParams) *EMACrossoverStrategy {
	if p.FastPeriod == 0 {
		p.FastPeriod = 9
	}
	if p.SlowPeriod == 0 {
		p.SlowPeriod = 21
	}
	return &EMACrossoverStrategy{FastPeriod: p.FastPeriod, SlowPeriod: p.SlowPeriod}
}

// NewEMACrossoverStrategy creates a new instance. Preserved for existing
// callers; equivalent to NewEMACrossover(EMACrossoverParams{}).
func NewEMACrossoverStrategy() *EMACrossoverStrategy {
	return NewEMACrossover(EMACrossoverParams{})
}

func (s *EMACrossoverStrategy) Name() string {
	return "EMA Crossover (9/21)"
}

func (s *EMACrossoverStrategy) Evaluate(ctx EvalContext) Signal {
	prices := ctx.ClosePrices()
	if len(prices) < s.SlowPeriod+1 {
		return Signal{Action: Hold, Reason: "Insufficient data"}
	}

	fastEMA := CalculateEMA(prices, s.FastPeriod)
	slowEMA := CalculateEMA(prices, s.SlowPeriod)

	if fastEMA == nil || slowEMA == nil {
		return Signal{Action: Hold, Reason: "Calculation error"}
	}

	// Current and Previous values for crossover detection
	currFast := fastEMA[len(fastEMA)-1]
	currSlow := slowEMA[len(slowEMA)-1]
	prevFast := fastEMA[len(fastEMA)-2]
	prevSlow := slowEMA[len(slowEMA)-2]

	// Buy: Fast crosses ABOVE Slow
	if prevFast <= prevSlow && currFast > currSlow {
		return Signal{
			Action:   Buy,
			Strength: 1.0,
			Reason:   fmt.Sprintf("Golden Cross: EMA%d (%.2f) > EMA%d (%.2f)", s.FastPeriod, currFast, s.SlowPeriod, currSlow),
		}
	}

	// Sell: Fast crosses BELOW Slow
	if prevFast >= prevSlow && currFast < currSlow {
		return Signal{
			Action:   Sell,
			Strength: 1.0,
			Reason:   fmt.Sprintf("Death Cross: EMA%d (%.2f) < EMA%d (%.2f)", s.FastPeriod, currFast, s.SlowPeriod, currSlow),
		}
	}

	return Signal{
		Action:   Hold,
		Strength: 0,
		Reason:   fmt.Sprintf("Trending: EMA%d=%.2f, EMA%d=%.2f", s.FastPeriod, currFast, s.SlowPeriod, currSlow),
	}
}

func init() {
	Register("ema_crossover", func() Strategy {
		return NewEMACrossover(EMACrossoverParams{})
	})
	RegisterParams("ema_crossover", func(params map[string]float64) (Strategy, error) {
		p := EMACrossoverParams{}
		if err := applyParams(&p, params); err != nil {
			return nil, fmt.Errorf("ema_crossover: %w", err)
		}
		return NewEMACrossover(p), nil
	})
}
