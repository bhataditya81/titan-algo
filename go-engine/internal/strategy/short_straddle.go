package strategy

import (
	"fmt"
)

// ShortStraddleStrategy implements a Naked Short Straddle.
//
// ST-5 fix: entry now fires ONLY when !ctx.HasPosition, and Exit ONLY when
// ctx.HasPosition — previously a fresh entry signal fired every evaluation
// while RSI stayed in the neutral band, causing the engine/backtest to
// close and reopen the straddle every candle.
//
// CR-14 fix: added a combined-premium stop-loss (exit when current combined
// premium >= entry premium * StopMultiplier). The RSI-band breakout exit is
// kept as a secondary/additional exit condition.
type ShortStraddleStrategy struct {
	RSILower       float64
	RSIUpper       float64
	StopMultiplier float64 // Combined-premium stop multiplier (default 1.4)
}

// ShortStraddleParams configures NewShortStraddle. The zero value produces
// today's hardcoded defaults (RSILower=45, RSIUpper=55, StopMultiplier=1.4)
// — this is what registry.Get("short_straddle") constructs internally, so
// existing callers are unaffected by this parameterization (G-5).
type ShortStraddleParams struct {
	RSILower       float64
	RSIUpper       float64
	StopMultiplier float64
}

// NewShortStraddle builds a ShortStraddleStrategy from params, substituting
// the hardcoded default for any zero-valued field.
func NewShortStraddle(p ShortStraddleParams) *ShortStraddleStrategy {
	if p.RSILower == 0 {
		p.RSILower = 45
	}
	if p.RSIUpper == 0 {
		p.RSIUpper = 55
	}
	if p.StopMultiplier == 0 {
		p.StopMultiplier = 1.4
	}
	return &ShortStraddleStrategy{RSILower: p.RSILower, RSIUpper: p.RSIUpper, StopMultiplier: p.StopMultiplier}
}

// NewShortStraddleStrategy creates a new instance. Preserved for existing
// callers; equivalent to NewShortStraddle(ShortStraddleParams{}).
func NewShortStraddleStrategy() *ShortStraddleStrategy {
	return NewShortStraddle(ShortStraddleParams{})
}

func (s *ShortStraddleStrategy) Name() string {
	return "Short Straddle (Naked)"
}

func (s *ShortStraddleStrategy) Evaluate(ctx EvalContext) Signal {
	prices := ctx.ClosePrices()
	if len(prices) < 15 {
		return Signal{Action: Hold, Reason: "Insuff Data"}
	}

	rsi := CalculateRSI(prices, 14)
	if rsi == nil {
		return Signal{Action: Hold, Reason: "RSI Fail"}
	}

	if ctx.HasPosition {
		// Primary exit: combined-premium stop-loss (CR-14). Skipped
		// (fail-safe to the RSI exit) when either premium is unknown.
		if ctx.EntryPremium > 0 {
			if cur, ok := ctx.LastPrice(); ok {
				stopLevel := ctx.EntryPremium * s.StopMultiplier
				if cur >= stopLevel {
					return Signal{
						Action:   Exit,
						Strength: 1.0,
						Reason:   fmt.Sprintf("Premium stop: combined %.2f >= entry %.2f x %.2f", cur, ctx.EntryPremium, s.StopMultiplier),
					}
				}
			}
		}

		// Secondary exit: RSI breakout out of the neutral band.
		if rsi.Value < s.RSILower || rsi.Value > s.RSIUpper {
			return Signal{
				Action:   Exit,
				Reason:   fmt.Sprintf("Exit: RSI %.2f (Trend)", rsi.Value),
				Strength: 1.0,
			}
		}

		return Signal{Action: Hold, Reason: fmt.Sprintf("Holding: RSI %.2f", rsi.Value)}
	}

	// Flat: look for a fresh entry. Neutral Market.
	if rsi.Value >= s.RSILower && rsi.Value <= s.RSIUpper {
		return Signal{
			Action:   Sell,
			Strength: 0.9,
			Reason:   fmt.Sprintf("Straddle Entry: RSI %.2f (Sideways)", rsi.Value),
			Legs: []OrderLeg{
				{Direction: LegSell, StrikeOffset: 0, OptionType: "CE", Quantity: 1},
				{Direction: LegSell, StrikeOffset: 0, OptionType: "PE", Quantity: 1},
			},
		}
	}

	return Signal{Action: Hold, Reason: fmt.Sprintf("RSI %.2f", rsi.Value)}
}

func init() {
	Register("short_straddle", func() Strategy {
		return NewShortStraddle(ShortStraddleParams{})
	})
	RegisterParams("short_straddle", func(params map[string]float64) (Strategy, error) {
		p := ShortStraddleParams{}
		if err := applyParams(&p, params); err != nil {
			return nil, fmt.Errorf("short_straddle: %w", err)
		}
		return NewShortStraddle(p), nil
	})
}
