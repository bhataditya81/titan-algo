// Package strategy contains all trading strategies and the shared
// signal/evaluation contract consumed by the engine (WP-9) and the
// backtester (WP-7).
//
// ============================================================================
// SIGNAL CONTRACT (WP-6, post-remediation)
// ============================================================================
//
// Actions are now explicit and NOT overloaded (fixes ST-8):
//
//   - Buy:  open a LONG position (directional intent). Never means "exit".
//   - Sell: open a SHORT position (directional intent). Never means "exit".
//   - Exit: close the currently open position (whatever its side/legs).
//     Strategies emit Exit ONLY when EvalContext.HasPosition is true.
//   - Hold: do nothing.
//
// A consumer (engine/backtest) MUST:
//   - Treat Buy/Sell strictly as entry/directional intent. If a position is
//     already open in the opposite direction, the consumer decides whether
//     that is a reversal (close + open) — the strategy will NOT emit Buy to
//     mean "cover a short".
//   - Treat Exit as "flatten this strategy's position", including all legs
//     of a multi-leg position.
//   - Honor StopLossPrice / TargetPrice when non-zero by placing broker-side
//     protective orders (SL-M) alongside the entry.
//
// Multi-leg signals: Legs non-empty means the signal describes an option
// structure. LEG ORDER IS SIGNIFICANT: BUY (hedge) legs are listed BEFORE
// SELL legs so the execution layer can place hedges first (CR-12).
// ============================================================================
package strategy

import (
	"time"

	// Embed IANA tzdata so Asia/Kolkata resolves even on hosts without a
	// system zoneinfo database (stripped Windows/containers). Stdlib only.
	_ "time/tzdata"
)

// IST is the canonical timezone for ALL market-clock logic in this package
// (fixes ST-3). Every strategy converts EvalContext.Now via .In(IST) before
// comparing hours/minutes. Loaded at init; the process panics at startup if
// timezone data is unavailable — failing loudly beats silently trading on
// server-local wall-clock time.
var IST *time.Location

func init() {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		panic("strategy: cannot load Asia/Kolkata timezone data: " + err.Error())
	}
	IST = loc
}

// SignalAction represents the trading action to take.
type SignalAction string

const (
	// Buy means OPEN LONG. It never means "exit a short" (ST-8 fix).
	Buy SignalAction = "BUY"
	// Sell means OPEN SHORT. It never means "exit a long" (ST-8 fix).
	Sell SignalAction = "SELL"
	// Exit means CLOSE the strategy's currently open position (all legs).
	// Emitted only when EvalContext.HasPosition is true.
	Exit SignalAction = "EXIT"
	// Hold means do nothing this evaluation.
	Hold SignalAction = "HOLD"
)

// LegDirection for multi-leg orders.
type LegDirection string

const (
	LegBuy  LegDirection = "BUY"
	LegSell LegDirection = "SELL"
)

// OrderLeg defines a single leg of a multi-leg strategy.
//
// Leg ordering contract (CR-12): within Signal.Legs, BUY (hedge/wing) legs
// MUST appear before SELL (body) legs. The execution layer places legs in
// slice order, so hedges fill first and a later rejection never leaves a
// naked short exposed.
type OrderLeg struct {
	Direction    LegDirection
	StrikeOffset int    // Points from ATM: 0 = ATM, +200 = 200 pts toward call side, -200 = toward put side.
	OptionType   string // "CE" or "PE"
	Expiry       string // Expiry selector, e.g. "2026-01-20"; "" = nearest weekly (engine resolves via instrument master).
	Quantity     int    // Number of LOTS (not shares/units). Strategies emit >= 1; engine multiplies by lot size.
}

// Signal represents a trading signal from a strategy.
type Signal struct {
	Action   SignalAction
	Strength float64 // 0.0 to 1.0 (confidence level)
	Reason   string

	// Legs: non-empty => multi-leg option structure. BUY legs first (CR-12).
	Legs []OrderLeg

	// StopLossPrice, when non-zero, is the absolute price/premium at which
	// the position must be stopped out (CR-14). The engine should place a
	// broker-side SL-M order at this level.
	StopLossPrice float64

	// TargetPrice, when non-zero, is the absolute take-profit price/premium.
	TargetPrice float64
}

// EvalContext carries everything a strategy needs for one evaluation. The
// engine/backtest builds one per tick/candle. Strategies are now
// position-aware (fixes ST-5): short_straddle, iron_fly and nine_twenty emit
// an entry signal only when !HasPosition, and Exit only when HasPosition —
// they no longer re-signal a fresh entry every tick while conditions hold.
//
// Data fields — at least one of Prices or Candles should be populated; use
// ClosePrices()/VolumeSeries()/LastPrice() to read either transparently:
//   - Prices/Volumes: tick-mode series, oldest→newest. Volumes, when sourced
//     from the live feed, are typically CUMULATIVE day volume per tick;
//     strategies that need per-interval volume compute deltas themselves
//     (see sniper.go).
//   - Candles: candle-mode series (backtest/historical), oldest→newest.
//
// Position fields — supplied by the caller from its own risk/position book;
// strategies trust these over any internal latch, since only the caller
// knows about confirmed fills:
//   - HasPosition:   true while this strategy currently holds a position.
//   - PositionAge:   time since entry fill confirmation (informational).
//   - EntryPremium:  for option structures, the combined entry premium of all
//     legs (sum of fill premiums). 0 if unknown/not applicable.
//
// Current premium for premium-based stops (nine_twenty, short_straddle) is
// read via LastPrice() — the caller is responsible for feeding the live
// combined-premium series into Prices (or Candles) for a symbol tracking an
// open option structure. This keeps EvalContext to the fields specified by
// the WP-6 contract without a dedicated "current premium" field.
type EvalContext struct {
	Symbol  string
	Prices  []float64
	Volumes []float64
	Candles []Candle
	Now     time.Time

	HasPosition  bool
	PositionAge  time.Duration
	EntryPremium float64
}

// ClosePrices returns the close-price series: tick prices when available,
// otherwise candle closes. Returns nil when neither is populated.
func (ctx *EvalContext) ClosePrices() []float64 {
	if len(ctx.Prices) > 0 {
		return ctx.Prices
	}
	if len(ctx.Candles) == 0 {
		return nil
	}
	closes := make([]float64, len(ctx.Candles))
	for i, c := range ctx.Candles {
		closes[i] = c.Close
	}
	return closes
}

// VolumeSeries returns the volume series aligned with ClosePrices.
func (ctx *EvalContext) VolumeSeries() []float64 {
	if len(ctx.Prices) > 0 {
		return ctx.Volumes
	}
	if len(ctx.Candles) == 0 {
		return nil
	}
	vols := make([]float64, len(ctx.Candles))
	for i, c := range ctx.Candles {
		vols[i] = float64(c.Volume)
	}
	return vols
}

// LastPrice returns the most recent price and whether one exists.
func (ctx *EvalContext) LastPrice() (float64, bool) {
	if len(ctx.Prices) > 0 {
		return ctx.Prices[len(ctx.Prices)-1], true
	}
	if len(ctx.Candles) > 0 {
		return ctx.Candles[len(ctx.Candles)-1].Close, true
	}
	return 0, false
}

// candlesToEvalContext builds a position-agnostic EvalContext from candle
// history alone (HasPosition defaults to false). Used by simple
// candle-history entry points that have no visibility into position state.
// Callers that DO know position state (the live engine, or a backtest engine
// tracking its own portfolio) must construct EvalContext directly with
// HasPosition/EntryPremium set — that is the only path that gets
// position-aware behavior out of short_straddle/iron_fly/nine_twenty.
func candlesToEvalContext(history []Candle) EvalContext {
	ctx := EvalContext{Candles: history}
	if len(history) > 0 {
		ctx.Now = history[len(history)-1].Time
	}
	return ctx
}

// Strategy defines the interface for trading strategies.
//
// BREAKING CHANGE (WP-6): the old two-method interface
//
//	Evaluate(symbol string, prices, volumes []float64, t time.Time) Signal
//	EvaluateCandles(history []Candle) Signal
//
// is replaced by a single position-aware method taking EvalContext. Callers
// with only candle history and no position tracking can still get a Signal
// via EvaluateCandles(history), a free function in this package that wraps
// candlesToEvalContext + Evaluate. See docs/reports/WP-6-REPORT.md for the
// full list of external call sites this breaks (cmd/, internal/engine).
type Strategy interface {
	// Name returns the strategy name.
	Name() string

	// Evaluate analyzes the context and returns a trading signal.
	// Implementations MUST return Hold (never panic) on insufficient data.
	Evaluate(ctx EvalContext) Signal
}

// EvaluateCandles is a convenience entry point for callers that only have
// candle history and no position tracking (e.g. simple backtests). It builds
// a position-agnostic EvalContext (HasPosition=false) and delegates to
// Evaluate. Returns Hold on empty history.
func EvaluateCandles(s Strategy, history []Candle) Signal {
	if len(history) == 0 {
		return Signal{Action: Hold, Reason: "No data"}
	}
	return s.Evaluate(candlesToEvalContext(history))
}
