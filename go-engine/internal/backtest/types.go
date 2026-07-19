// Package backtest is a standalone, offline backtesting engine for TitanAlgo.
//
// Findings addressed here (see docs/PRODUCTION_READINESS_AUDIT.md):
//   - CR-9:  real Black-Scholes repricing of option legs (bs.go)
//   - CR-10: leg-less directional signals actually enter positions (engine.go)
//   - ST-6:  per-leg, per-side FY 2025-26 charges + spread cost (charges.go)
//   - ST-7:  next-candle-open fills, never signal-candle close (engine.go)
//   - ST-10/M3: no hardcoded lot size (Config.LotSize, CLI -lotsize)
//   - ST-10/M5: rich reporting incl. drawdown/PF/expectancy (report.go)
//
// Types below are aliases onto internal/strategy's WP-6 contract (Signal
// with Exit/Hold actions, OrderLeg with Expiry+Quantity, EvalContext-based
// Evaluate) -- WP-6 landed with the exact contract this package was written
// against, so there is no adapter layer: a strategy.Strategy IS a
// backtest.Strategy.
package backtest

import "titan-algo/internal/strategy"

type (
	Candle       = strategy.Candle
	SignalAction = strategy.SignalAction
	LegDirection = strategy.LegDirection
	OrderLeg     = strategy.OrderLeg
	Signal       = strategy.Signal
	EvalContext  = strategy.EvalContext
	Strategy     = strategy.Strategy
)

const (
	Buy  = strategy.Buy
	Sell = strategy.Sell
	Exit = strategy.Exit
	Hold = strategy.Hold

	LegBuy  = strategy.LegBuy
	LegSell = strategy.LegSell
)

// IST is the canonical Asia/Kolkata location, re-exported from
// internal/strategy so callers (cmd/backtest) don't need a second import
// for date-range flag parsing.
var IST = strategy.IST
