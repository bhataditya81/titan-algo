package strategy

import (
	"fmt"
	"time"
)

// NineTwentyStrategy implements the famous time-based Short Straddle
type NineTwentyStrategy struct {
	EntryHour       int
	EntryMinute     int
	SquareOffHour   int
	SquareOffMinute int
	entered         bool // State to track if we entered today
	lastDay         int
}

// NewNineTwentyStrategy creates a new instance
func NewNineTwentyStrategy() *NineTwentyStrategy {
	return &NineTwentyStrategy{
		EntryHour:       9,
		EntryMinute:     20,
		SquareOffHour:   15,
		SquareOffMinute: 15,
	}
}

func (s *NineTwentyStrategy) Name() string {
	return "9:20 Straddle"
}

func (s *NineTwentyStrategy) Evaluate(symbol string, prices []float64, volumes []float64, currentTime time.Time) Signal {
	// Reset state if new day
	if currentTime.Day() != s.lastDay {
		s.entered = false
		s.lastDay = currentTime.Day()
	}

	h, m, _ := currentTime.Clock()

	// 1. Check Entry Time (Window Match)
	// Allow 5-minute window (e.g. 9:20 to 9:24)
	if !s.entered {
		if h == s.EntryHour && m >= s.EntryMinute && m < s.EntryMinute+5 {
			s.entered = true
			return Signal{
				Action:   Sell, // "Sell" implies Short Strategy
				Strength: 1.0,
				Reason:   "9:20 Straddle Trigger",
				Legs: []OrderLeg{
					{Direction: LegSell, StrikeOffset: 0, OptionType: "CE"},
					{Direction: LegSell, StrikeOffset: 0, OptionType: "PE"},
				},
			}
		}
	}

	// 2. Check Exit (Square Off Time)
	if s.entered {
		if h > s.SquareOffHour || (h == s.SquareOffHour && m >= s.SquareOffMinute) {
			s.entered = false // Reset
			return Signal{
				Action:   Buy, // "Buy" implies Exit/Cover
				Reason:   "Intraday Squareoff",
				Strength: 1.0,
			}
		}
	}

	return Signal{Action: Hold, Reason: fmt.Sprintf("Waiting for 09:20 (Current: %02d:%02d)", h, m)}
}

// EvaluateCandles implements the Strategy interface
func (s *NineTwentyStrategy) EvaluateCandles(history []Candle) Signal {
	// Backtesting logic: iterate day by day?
	// Simplified: Check the last candle. If it's 9:20, trigger.
	if len(history) == 0 {
		return Signal{Action: Hold}
	}

	lastCandle := history[len(history)-1]
	return s.Evaluate("", nil, nil, lastCandle.Time)
}

func init() {
	Register("nine_twenty", func() Strategy {
		return NewNineTwentyStrategy()
	})
}
