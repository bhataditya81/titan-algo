package backtest

import "titan-algo/internal/risk"

// LegCharge computes total statutory+brokerage charges for one order (one
// side of one leg), given the premium it executed at and share quantity
// (lots * lot size). side is the order's own direction (BUY to open a long
// leg or to close a short leg; SELL to open a short leg or close a long
// one).
//
// WP-2's internal/risk.EstimateCharges landed with an FY 2025-26 options
// rate card matching the one this package originally shipped with (ST-6 /
// EX-4), so this delegates to it directly rather than maintaining a second,
// possibly-drifting fee table (per WP-2 task 7: "unified fee model").
// TradeType is always risk.OptCarry: the backtest engine only prices index
// option premium, and OptCarry is what CARRYFORWARD short-option strategies
// (straddle/iron-fly/nine_twenty) actually trade per the audit (CR-13).
func LegCharge(premium float64, quantity int, side LegDirection) float64 {
	return risk.EstimateCharges(premium, quantity, risk.OptCarry, toRiskSide(side)).Total
}

func toRiskSide(d LegDirection) risk.OrderSide {
	if d == LegSell {
		return risk.Sell
	}
	return risk.Buy
}

// SpreadCost is the modeled half-spread cost (ST-6) charged per leg per
// side: a fraction of premium representing the cost of crossing the
// bid/ask on an instrument the backtest only has a single (mid-ish)
// theoretical price for. This is NOT a statutory charge, so it lives here
// rather than in internal/risk.
func SpreadCost(halfSpreadPct, premium float64, quantity int) float64 {
	return premium * halfSpreadPct * float64(quantity)
}
