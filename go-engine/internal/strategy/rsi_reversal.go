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
type RSIReversalStrategy struct {
	Period     int
	Oversold   float64
	Overbought float64
	ExitSMA    int // Period for the mean-reversion exit SMA (default 5)

	mu            sync.Mutex
	lastDirection SignalAction // Buy or Sell; "" when unknown/flat
}

// NewRSIReversalStrategy creates a new instance
func NewRSIReversalStrategy() *RSIReversalStrategy {
	return &RSIReversalStrategy{
		Period:     2,
		Oversold:   10,
		Overbought: 90,
		ExitSMA:    5,
	}
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
		s.lastDirection = ""
		s.mu.Unlock()

		// Buy the dip (extreme oversold)
		if rsi.Value < s.Oversold {
			s.mu.Lock()
			s.lastDirection = Buy
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
			s.lastDirection = Sell
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
	dir := s.lastDirection
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
		return NewRSIReversalStrategy()
	})
}
