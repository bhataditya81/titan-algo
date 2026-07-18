package strategy

import (
	"fmt"
	"time"
)

// IronFlyStrategy implements a hedged Short Straddle
type IronFlyStrategy struct {
	WingWidth int // Points away from ATM for hedges
}

func NewIronFlyStrategy() *IronFlyStrategy {
	return &IronFlyStrategy{
		WingWidth: 200, // Default 200 points wing
	}
}

func (s *IronFlyStrategy) Name() string {
	return "Iron Fly (Hedged)"
}

func (s *IronFlyStrategy) Evaluate(symbol string, prices []float64, volumes []float64, currentTime time.Time) Signal {
	if len(prices) < 14 {
		return Signal{Action: Hold, Reason: "Insufficient data"}
	}

	rsi := CalculateRSI(prices, 14)
	if rsi == nil {
		return Signal{Action: Hold, Reason: "RSI calc failed"}
	}

	// Entry Condition: Market is Neutral (RSI between 45 and 55)
	if rsi.Value >= 45 && rsi.Value <= 55 {
		return Signal{
			Action:   Sell, // Short Strategy
			Strength: 0.8,
			Reason:   fmt.Sprintf("Iron Fly Entry: Stable RSI %.2f", rsi.Value),
			Legs: []OrderLeg{
				// Short Straddle (ATM)
				{Direction: LegSell, StrikeOffset: 0, OptionType: "CE"},
				{Direction: LegSell, StrikeOffset: 0, OptionType: "PE"},
				// Long Strangulation (Hedges) - OTM
				{Direction: LegBuy, StrikeOffset: s.WingWidth, OptionType: "CE"},  // Buy OTM Call
				{Direction: LegBuy, StrikeOffset: -s.WingWidth, OptionType: "PE"}, // Buy OTM Put
			},
		}
	}

	// Exit Condition: RSI becomes extreme?
	// For now, simple fixed entry. Exit handled by Engine SL/Target or End of Day.

	return Signal{Action: Hold, Reason: fmt.Sprintf("RSI %.2f (Waiting for 45-55)", rsi.Value)}
}

// EvaluateCandles implements the Strategy interface
func (s *IronFlyStrategy) EvaluateCandles(history []Candle) Signal {
	prices := make([]float64, len(history))
	volumes := make([]float64, len(history))
	for i, c := range history {
		prices[i] = c.Close
		volumes[i] = float64(c.Volume)
	}
	// Use last candle time
	lastTime := history[len(history)-1].Time
	return s.Evaluate("", prices, volumes, lastTime)
}

func init() {
	Register("iron_fly", func() Strategy {
		return NewIronFlyStrategy()
	})
}
