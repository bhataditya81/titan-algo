package strategy

import (
	"fmt"
	"time"
)

// ShortStraddleStrategy implements a Naked Short Straddle
type ShortStraddleStrategy struct {
	RSILower float64
	RSIUpper float64
}

func NewShortStraddleStrategy() *ShortStraddleStrategy {
	return &ShortStraddleStrategy{
		RSILower: 45,
		RSIUpper: 55,
	}
}

func (s *ShortStraddleStrategy) Name() string {
	return "Short Straddle (Naked)"
}

func (s *ShortStraddleStrategy) Evaluate(symbol string, prices []float64, volumes []float64, currentTime time.Time) Signal {
	if len(prices) < 14 {
		return Signal{Action: Hold, Reason: "Insuff Data"}
	}

	rsi := CalculateRSI(prices, 14)
	if rsi == nil {
		return Signal{Action: Hold, Reason: "RSI Fail"}
	}

	// Entry: Neutral Market
	if rsi.Value >= s.RSILower && rsi.Value <= s.RSIUpper {
		return Signal{
			Action:   Sell,
			Strength: 0.9,
			Reason:   fmt.Sprintf("Straddle Entry: RSI %.2f (Sideways)", rsi.Value),
			Legs: []OrderLeg{
				{Direction: LegSell, StrikeOffset: 0, OptionType: "CE"},
				{Direction: LegSell, StrikeOffset: 0, OptionType: "PE"},
			},
		}

	}

	// Exit: Trend detected (RSI breakout)
	if rsi.Value < s.RSILower || rsi.Value > s.RSIUpper {
		return Signal{
			Action:   Buy, // Exit
			Reason:   fmt.Sprintf("Exit: RSI %.2f (Trend)", rsi.Value),
			Strength: 1.0,
		}
	}

	return Signal{Action: Hold, Reason: fmt.Sprintf("RSI %.2f", rsi.Value)}
}

func (s *ShortStraddleStrategy) EvaluateCandles(history []Candle) Signal {
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
	Register("short_straddle", func() Strategy {
		return NewShortStraddleStrategy()
	})
}
