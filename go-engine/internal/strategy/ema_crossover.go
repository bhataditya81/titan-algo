package strategy

import (
	"fmt"
	"time"
)

// EMACrossoverStrategy implements a classic trend following strategy
type EMACrossoverStrategy struct {
	FastPeriod int
	SlowPeriod int
}

// NewEMACrossoverStrategy creates a new instance
func NewEMACrossoverStrategy() *EMACrossoverStrategy {
	return &EMACrossoverStrategy{
		FastPeriod: 9,
		SlowPeriod: 21,
	}
}

func (s *EMACrossoverStrategy) Name() string {
	return "EMA Crossover (9/21)"
}

func (s *EMACrossoverStrategy) Evaluate(symbol string, prices []float64, volumes []float64, currentTime time.Time) Signal {
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

// EvaluateCandles implements the Strategy interface
func (s *EMACrossoverStrategy) EvaluateCandles(history []Candle) Signal {
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
	Register("ema_crossover", func() Strategy {
		return NewEMACrossoverStrategy()
	})
}
