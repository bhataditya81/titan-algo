package strategy

import (
	"fmt"
	"sync"
)

// RSIReversalStrategy implements Connors-style RSI mean reversion.
//
// ST-10 exit fix: previously the only "exit" was the opposite extreme
// (e.g. enter oversold, wait for overbought) which can take many periods
// and gives back most of the reversion move. Now adds a proper
// mean-reversion exit: close crossing back above a short SMA, or RSI
// crossing back through the neutral 50 line — whichever comes first.
//
// EvalContext carries no explicit position "side", so the strategy tracks
// which side it last entered on internally (lastDirection) to know which
// mirrored exit condition applies. This is reset whenever the caller
// reports HasPosition==false (flat), so a fresh entry re-establishes it.
//
// lastDirection is keyed by ctx.Symbol (audit R3): registry.Get caches one
// shared singleton per strategy name, and runner.go's tick loop calls
// Evaluate for every configured symbol against that same instance (a real,
// discovery-driven multi-symbol feature). An un-keyed field let one
// symbol's flat state silently reset another symbol's still-open-position
// direction, applying the wrong-side exit rule.
type RSIReversalStrategy struct {
	Period     int
	Oversold   float64
	Overbought float64
	ExitSMA    int // Period for the mean-reversion exit SMA (default 5)

	mu            sync.Mutex
	lastDirection map[string]SignalAction // symbol -> Buy or Sell; absent/"" when unknown/flat
}

// RSIReversalParams configures NewRSIReversal. The zero value produces
// today's hardcoded defaults (Period=2, Oversold=10, Overbought=90,
// ExitSMA=5) — this is what registry.Get("rsi_reversal") constructs
// internally, so existing callers are unaffected by this parameterization
// (G-5).
type RSIReversalParams struct {
	Period     int
	Oversold   float64
	Overbought float64
	ExitSMA    int
}

// NewRSIReversal builds an RSIReversalStrategy from params, substituting the
// hardcoded default for any zero-valued field.
func NewRSIReversal(p RSIReversalParams) *RSIReversalStrategy {
	if p.Period == 0 {
		p.Period = 2
	}
	if p.Oversold == 0 {
		p.Oversold = 10
	}
	if p.Overbought == 0 {
		p.Overbought = 90
	}
	if p.ExitSMA == 0 {
		p.ExitSMA = 5
	}
	return &RSIReversalStrategy{
		Period: p.Period, Oversold: p.Oversold, Overbought: p.Overbought, ExitSMA: p.ExitSMA,
		lastDirection: make(map[string]SignalAction),
	}
}

// NewRSIReversalStrategy creates a new instance. Preserved for existing
// callers; equivalent to NewRSIReversal(RSIReversalParams{}).
func NewRSIReversalStrategy() *RSIReversalStrategy {
	return NewRSIReversal(RSIReversalParams{})
}

func (s *RSIReversalStrategy) Name() string {
	return "RSI Reversion (2-Period)"
}

func (s *RSIReversalStrategy) Evaluate(ctx EvalContext) Signal {
	prices := ctx.ClosePrices()
	if len(prices) < s.Period+1 {
		return Signal{Action: Hold, Reason: "Insufficient data"}
	}

	rsi := CalculateRSI(prices, s.Period)
	if rsi == nil {
		return Signal{Action: Hold, Reason: "RSI calc failed"}
	}

	if !ctx.HasPosition {
		s.mu.Lock()
		delete(s.lastDirection, ctx.Symbol)
		s.mu.Unlock()

		// Buy the dip (extreme oversold)
		if rsi.Value < s.Oversold {
			s.mu.Lock()
			s.lastDirection[ctx.Symbol] = Buy
			s.mu.Unlock()
			return Signal{
				Action:   Buy,
				Strength: 1.0,
				Reason:   fmt.Sprintf("Oversold: RSI(%d)=%.1f < %.1f", s.Period, rsi.Value, s.Oversold),
			}
		}

		// Sell the rip (extreme overbought)
		if rsi.Value > s.Overbought {
			s.mu.Lock()
			s.lastDirection[ctx.Symbol] = Sell
			s.mu.Unlock()
			return Signal{
				Action:   Sell,
				Strength: 1.0,
				Reason:   fmt.Sprintf("Overbought: RSI(%d)=%.1f > %.1f", s.Period, rsi.Value, s.Overbought),
			}
		}

		return Signal{
			Action:   Hold,
			Strength: 0,
			Reason:   fmt.Sprintf("Neutral: RSI(%d)=%.1f", s.Period, rsi.Value),
		}
	}

	// HasPosition: look for the mean-reversion exit.
	s.mu.Lock()
	dir := s.lastDirection[ctx.Symbol]
	s.mu.Unlock()

	price := prices[len(prices)-1]
	sma := CalculateSMA(prices, s.ExitSMA)
	var smaVal float64
	if len(sma) > 0 {
		smaVal = sma[len(sma)-1]
	}

	switch dir {
	case Sell: // currently short: exit once momentum reverts downward
		if (smaVal > 0 && price < smaVal) || rsi.Value < 50 {
			return Signal{
				Action:   Exit,
				Strength: 1.0,
				Reason:   fmt.Sprintf("Mean-reversion exit (short): price %.2f vs SMA%d %.2f, RSI %.1f", price, s.ExitSMA, smaVal, rsi.Value),
			}
		}
	default: // Buy or unknown: exit once momentum reverts upward
		if (smaVal > 0 && price > smaVal) || rsi.Value > 50 {
			return Signal{
				Action:   Exit,
				Strength: 1.0,
				Reason:   fmt.Sprintf("Mean-reversion exit (long): price %.2f vs SMA%d %.2f, RSI %.1f", price, s.ExitSMA, smaVal, rsi.Value),
			}
		}
	}

	return Signal{Action: Hold, Reason: fmt.Sprintf("Holding: RSI(%d)=%.1f", s.Period, rsi.Value)}
}

func init() {
	Register("rsi_reversal", func() Strategy {
		return NewRSIReversal(RSIReversalParams{})
	})
	RegisterParams("rsi_reversal", func(params map[string]float64) (Strategy, error) {
		p := RSIReversalParams{}
		if err := applyParams(&p, params); err != nil {
			return nil, fmt.Errorf("rsi_reversal: %w", err)
		}
		return NewRSIReversal(p), nil
	})
}
