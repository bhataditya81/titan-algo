package strategy

import (
	"fmt"
)

// IronFlyStrategy implements a hedged Short Straddle.
//
// ST-5 fix: entry only when !ctx.HasPosition; Exit only when ctx.HasPosition
// (previously re-signaled a fresh entry every candle while RSI stayed
// neutral, causing churn).
type IronFlyStrategy struct {
	WingWidth int // Points away from ATM for hedges
	RSILower  float64
	RSIUpper  float64
}

func NewIronFlyStrategy() *IronFlyStrategy {
	return &IronFlyStrategy{
		WingWidth: 200, // Default 200 points wing
		RSILower:  45,
		RSIUpper:  55,
	}
}

func (s *IronFlyStrategy) Name() string {
	return "Iron Fly (Hedged)"
}

func (s *IronFlyStrategy) Evaluate(ctx EvalContext) Signal {
	prices := ctx.ClosePrices()
	if len(prices) < 15 {
		return Signal{Action: Hold, Reason: "Insufficient data"}
	}

	rsi := CalculateRSI(prices, 14)
	if rsi == nil {
		return Signal{Action: Hold, Reason: "RSI calc failed"}
	}

	if ctx.HasPosition {
		// Exit once the market leaves the neutral regime the fly was sold
		// into. Wings bound the tail risk, but there's no reason to hold a
		// fly through a trend once the edge that justified entry is gone.
		if rsi.Value < s.RSILower || rsi.Value > s.RSIUpper {
			return Signal{
				Action:   Exit,
				Strength: 1.0,
				Reason:   fmt.Sprintf("Iron Fly Exit: RSI left neutral band (%.2f)", rsi.Value),
			}
		}
		return Signal{Action: Hold, Reason: fmt.Sprintf("Holding Iron Fly: RSI %.2f", rsi.Value)}
	}

	// Flat: Entry Condition: Market is Neutral (RSI between 45 and 55)
	if rsi.Value >= s.RSILower && rsi.Value <= s.RSIUpper {
		return Signal{
			Action:   Sell, // Short-premium structure overall
			Strength: 0.8,
			Reason:   fmt.Sprintf("Iron Fly Entry: Stable RSI %.2f", rsi.Value),
			// CR-12 fix: BUY (hedge/wing) legs are declared BEFORE the SELL
			// (body) legs. Leg order is load-bearing — the execution layer
			// places legs in this slice order, so the long hedges fill
			// first. If a later leg is then rejected (margin, RMS, freeze
			// qty, illiquidity), the worst case is an incomplete hedge or a
			// harmless unhedged long — never a naked short straddle. The
			// old order (short body first, wings last) risked exactly that:
			// a rejected wing left a naked ATM short straddle while the
			// system believed it held a defined-risk fly. This also lines
			// up with NSE hedged-margin rules, which price the short legs
			// cheaper once the offsetting long hedge is already on book.
			Legs: []OrderLeg{
				{Direction: LegBuy, StrikeOffset: s.WingWidth, OptionType: "CE", Quantity: 1},  // Buy OTM Call (hedge)
				{Direction: LegBuy, StrikeOffset: -s.WingWidth, OptionType: "PE", Quantity: 1}, // Buy OTM Put (hedge)
				{Direction: LegSell, StrikeOffset: 0, OptionType: "CE", Quantity: 1},           // Sell ATM Call (body)
				{Direction: LegSell, StrikeOffset: 0, OptionType: "PE", Quantity: 1},           // Sell ATM Put (body)
			},
		}
	}

	return Signal{Action: Hold, Reason: fmt.Sprintf("RSI %.2f (Waiting for %.0f-%.0f)", rsi.Value, s.RSILower, s.RSIUpper)}
}

func init() {
	Register("iron_fly", func() Strategy {
		return NewIronFlyStrategy()
	})
}
