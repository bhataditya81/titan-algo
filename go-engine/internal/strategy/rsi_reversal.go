package strategy

import (
	"fmt"
	"time"
)

// RSIReversalStrategy implements Connors RSI 2-period mean reversion
type RSIReversalStrategy struct {
	Period     int
	Oversold   float64
	Overbought float64
}

// NewRSIReversalStrategy creates a new instance
func NewRSIReversalStrategy() *RSIReversalStrategy {
	return &RSIReversalStrategy{
		Period:     2,
		Oversold:   10,
		Overbought: 90,
	}
}

func (s *RSIReversalStrategy) Name() string {
	return "RSI Reversion (2-Period)"
}

func (s *RSIReversalStrategy) Evaluate(symbol string, prices []float64, volumes []float64, currentTime time.Time) Signal {
	if len(prices) < s.Period+1 {
		return Signal{Action: Hold, Reason: "Insufficient data"}
	}

	rsi := CalculateRSI(prices, s.Period)
	if rsi == nil {
		return Signal{Action: Hold, Reason: "RSI calc failed"}
	}

	// Mean Reversion Logic
	// Buy the dip (extreme oversold)
	if rsi.Value < s.Oversold {
		return Signal{
			Action:   Buy,
			Strength: 1.0,
			Reason:   fmt.Sprintf("Oversold: RSI(2)=%.1f < %.1f", rsi.Value, s.Oversold),
		}
	}

	// Sell the rip (extreme overbought)
	if rsi.Value > s.Overbought {
		return Signal{
			Action:   Sell,
			Strength: 1.0,
			Reason:   fmt.Sprintf("Overbought: RSI(2)=%.1f > %.1f", rsi.Value, s.Overbought),
		}
	}

	return Signal{
		Action:   Hold,
		Strength: 0,
		Reason:   fmt.Sprintf("Neutral: RSI(2)=%.1f", rsi.Value),
	}
}

// EvaluateCandles implements the Strategy interface
func (s *RSIReversalStrategy) EvaluateCandles(history []Candle) Signal {
	prices := make([]float64, len(history))
	volumes := make([]float64, len(history))
	for i, c := range history {
		prices[i] = c.Close
		volumes[i] = float64(c.Volume)
	}
	lastTime := history[len(history)-1].Time
	return s.Evaluate("", prices, volumes, lastTime)
}

func init() {
	Register("rsi_reversal", func() Strategy {
		return NewRSIReversalStrategy()
	})
}
