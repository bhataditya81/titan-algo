package strategy

import (
	"fmt"
	"time"
)

// SniperStrategy implements a high-probability "Sniper" approach
// It combines Trend (EMA), Momentum (RSI), and Price Action (Candlesticks).
//
// ST-9 fixes:
//   - Real wall-clock 5-minute IST candle boundaries (via EvalContext.Now)
//     replace the old "complete a candle every 5 ticks" fake aggregation,
//     whose bar duration depended entirely on poll interval.
//   - Candle.Time is now set to the bucket start time (was always zero).
//   - Volume is now a delta of cumulative volume (ctx.Volumes), not a tick
//     counter abusing the field.
//   - A latch (gated by the candle-completion transition itself) ensures
//     exactly one signal per completed candle, not one per tick.
//   - StopLossPct/TargetPct are wired into Signal.StopLossPrice/TargetPrice,
//     computed off the entry reference price (the closing price of the
//     candle that triggered the signal). TrailingSL remains an exported
//     strategy-level parameter (percentage trail distance) for the
//     execution layer to ratchet the broker-side stop as price moves
//     favorably post-entry — that is inherently stateful/ongoing behavior
//     that belongs in the engine's position-management loop (WP-9), not in
//     a single one-shot Signal.
//
// R2-2 / G-2 fix: EvalContext.Candles is now the PRIMARY contract (checked
// first, regardless of whether Prices is also populated). Before this fix,
// the candle-mode bypass required Prices to be EMPTY, but both
// internal/engine/runner.go (live) and internal/backtest/engine.go always
// populate Prices too, so candle-mode was structurally unreachable and
// every call fell through to tick-aggregation — which, fed one price per
// Evaluate call (as the backtest driver does, and as a low-frequency poller
// can), builds single-tick zero-range doji candles that IsHammer /
// IsBullishEngulfing / etc. can never match. Tick-aggregation (via the
// embedded CandleAggregator) is now an explicit fallback used ONLY when the
// caller supplies no Candles at all.
type SniperStrategy struct {
	// Strategy Properties
	StrategyName string
	StopLossPct  float64
	TargetPct    float64
	TrailingSL   float64

	// Parameters
	EMAPeriod     int
	RSIPeriod     int
	CandleMinutes int // e.g. 5 minutes

	agg *CandleAggregator // degraded-mode tick aggregation, keyed by symbol
}

// SniperParams configures NewSniper. The zero value produces today's
// hardcoded defaults — this is what registry.Get("sniper") constructs
// internally, so existing callers are unaffected by this parameterization
// (G-5).
type SniperParams struct {
	StopLossPct   float64
	TargetPct     float64
	TrailingSL    float64
	EMAPeriod     int
	RSIPeriod     int
	CandleMinutes int
}

// NewSniper builds a SniperStrategy from params, substituting the hardcoded
// default for any zero-valued field.
func NewSniper(p SniperParams) *SniperStrategy {
	if p.StopLossPct == 0 {
		p.StopLossPct = 1.0 // 1% Tight Stop
	}
	if p.TargetPct == 0 {
		p.TargetPct = 2.0 // 2% Target (1:2 Risk/Reward)
	}
	if p.TrailingSL == 0 {
		p.TrailingSL = 0.5 // Trailing stop distance (%), applied by the execution layer
	}
	if p.EMAPeriod == 0 {
		p.EMAPeriod = 50 // 50-EMA for Trend
	}
	if p.RSIPeriod == 0 {
		p.RSIPeriod = 14 // Standard RSI
	}
	if p.CandleMinutes == 0 {
		p.CandleMinutes = 5 // 5-min timeframe
	}
	return &SniperStrategy{
		StrategyName:  "Sniper F&O Strategy",
		StopLossPct:   p.StopLossPct,
		TargetPct:     p.TargetPct,
		TrailingSL:    p.TrailingSL,
		EMAPeriod:     p.EMAPeriod,
		RSIPeriod:     p.RSIPeriod,
		CandleMinutes: p.CandleMinutes,
		agg:           NewCandleAggregator(time.Duration(p.CandleMinutes)*time.Minute, 0),
	}
}

// NewSniperStrategy creates a new instance with default "Sniper" settings.
// Preserved for existing callers; equivalent to NewSniper(SniperParams{}).
func NewSniperStrategy() *SniperStrategy {
	return NewSniper(SniperParams{})
}

func (s *SniperStrategy) Name() string {
	return s.StrategyName
}

func (s *SniperStrategy) Evaluate(ctx EvalContext) Signal {
	// Candle-mode is the PRIMARY contract (R2-2 / G-2 fix): whenever the
	// caller supplies EvalContext.Candles, use it directly — regardless of
	// whether Prices is ALSO populated (both runner.go and backtest/engine.go
	// populate Prices unconditionally, which is exactly why this branch used
	// to require Prices to be empty and was therefore dead in practice).
	// Every call here is a fresh, fully-formed bar, so the
	// one-signal-per-completed-candle latch does not apply (the caller
	// controls how often it calls Evaluate in this mode).
	if len(ctx.Candles) > 0 {
		if len(ctx.Candles) < s.EMAPeriod+2 {
			return Signal{Action: Hold, Reason: "Insufficient History"}
		}
		sig := s.EvaluateLogic(ctx.Candles)
		if sig.Action == Buy || sig.Action == Sell {
			s.attachStops(&sig, ctx.Candles[len(ctx.Candles)-1].Close)
		}
		return sig
	}

	// Degraded fallback: no candle history supplied at all, so aggregate raw
	// ticks into real-range OHLC candles ourselves. Only reachable when the
	// caller has no candle buffer of its own (today: internal/engine/runner.go
	// — see docs/reports/R2-2-REPORT.md for the exact change that would let
	// it supply real Candles instead and retire this path to true live-tick
	// degraded mode).
	if ctx.Symbol == "" || len(ctx.Prices) == 0 {
		return Signal{Action: Hold, Reason: "No data"}
	}

	completed := s.updateCandle(ctx)

	history := s.agg.Completed(ctx.Symbol)

	if len(history) < s.EMAPeriod+2 {
		return Signal{Action: Hold, Reason: fmt.Sprintf("Building History: %d/%d", len(history), s.EMAPeriod+2)}
	}

	// One-signal-per-completed-candle latch: only run the strategy logic on
	// the tick that actually crosses a candle boundary. All ticks within the
	// same forming candle return Hold here instead of re-running/re-emitting.
	if !completed {
		return Signal{Action: Hold, Reason: "Waiting for candle close"}
	}

	sig := s.EvaluateLogic(history)
	if sig.Action == Buy || sig.Action == Sell {
		s.attachStops(&sig, history[len(history)-1].Close)
	}
	return sig
}

// EvaluateLogic contains the pure strategy rules
func (s *SniperStrategy) EvaluateLogic(history []Candle) Signal {
	// 2. Calculate Indicators on Candle Close Data
	closes := s.extractCloses(history)
	emaValues := CalculateEMA(closes, s.EMAPeriod)
	rsiResult := CalculateRSI(closes, s.RSIPeriod)

	if len(emaValues) == 0 || rsiResult == nil {
		return Signal{Action: Hold, Reason: "Calc Error"}
	}

	currentEMA := emaValues[len(emaValues)-1]
	currentRSI := rsiResult.Value

	// Last Completed Candle
	lastCandle := history[len(history)-1]
	prevCandle := history[len(history)-2]

	// 3. Logic Execution

	// --- BUY SETUP ---
	// Trend: Price > 50 EMA (Uptrend)
	// Momentum: RSI > 50 (Bullish)
	// Trigger: Hammer OR Bullish Engulfing

	isUptrend := lastCandle.Close > currentEMA
	isBullishMomentum := currentRSI > 50.0

	if isUptrend && isBullishMomentum {
		if IsHammer(lastCandle) {
			return Signal{
				Action:   Buy,
				Strength: 0.9,
				Reason:   fmt.Sprintf("Sniper Buy: Hammer in Uptrend (EMA %.2f, RSI %.2f)", currentEMA, currentRSI),
			}
		}
		if IsBullishEngulfing(prevCandle, lastCandle) {
			return Signal{
				Action:   Buy,
				Strength: 0.95,
				Reason:   fmt.Sprintf("Sniper Buy: Bullish Engulfing (EMA %.2f)", currentEMA),
			}
		}
	}

	// --- SELL SETUP ---
	isDowntrend := lastCandle.Close < currentEMA
	isBearishMomentum := currentRSI < 50.0

	if isDowntrend && isBearishMomentum {
		if IsShootingStar(lastCandle) {
			return Signal{
				Action:   Sell,
				Strength: 0.9,
				Reason:   fmt.Sprintf("Sniper Sell: Shooting Star in Downtrend (EMA %.2f)", currentEMA),
			}
		}
		if IsBearishEngulfing(prevCandle, lastCandle) {
			return Signal{
				Action:   Sell,
				Strength: 0.95,
				Reason:   fmt.Sprintf("Sniper Sell: Bearish Engulfing (EMA %.2f)", currentEMA),
			}
		}
	}

	return Signal{Action: Hold, Reason: fmt.Sprintf("Tracking: EMA %.2f RSI %.2f", currentEMA, currentRSI)}
}

// attachStops wires StopLossPct/TargetPct into the signal's absolute
// StopLossPrice/TargetPrice, computed off referencePrice (the entry
// reference — the closing price of the candle that triggered the signal).
func (s *SniperStrategy) attachStops(sig *Signal, referencePrice float64) {
	if referencePrice <= 0 {
		return
	}
	switch sig.Action {
	case Buy:
		if s.StopLossPct > 0 {
			sig.StopLossPrice = referencePrice * (1 - s.StopLossPct/100)
		}
		if s.TargetPct > 0 {
			sig.TargetPrice = referencePrice * (1 + s.TargetPct/100)
		}
	case Sell:
		if s.StopLossPct > 0 {
			sig.StopLossPrice = referencePrice * (1 + s.StopLossPct/100)
		}
		if s.TargetPct > 0 {
			sig.TargetPrice = referencePrice * (1 - s.TargetPct/100)
		}
	}
}

// updateCandle feeds the latest tick from ctx into s.agg (a real wall-clock
// IST CandleAggregator) for ctx.Symbol. Returns true exactly on the tick
// that completes a prior candle (crosses a bucket boundary), false
// otherwise. See CandleAggregator for the real-range OHLC guarantee.
func (s *SniperStrategy) updateCandle(ctx EvalContext) bool {
	price := ctx.Prices[len(ctx.Prices)-1]
	var cumVol float64
	if len(ctx.Volumes) > 0 {
		cumVol = ctx.Volumes[len(ctx.Volumes)-1]
	}

	now := ctx.Now
	if now.IsZero() {
		now = time.Now()
	}
	return s.agg.Add(ctx.Symbol, price, cumVol, now)
}

func (s *SniperStrategy) extractCloses(candles []Candle) []float64 {
	closes := make([]float64, len(candles))
	for i, c := range candles {
		closes[i] = c.Close
	}
	return closes
}

// init registers the strategy with the registry
func init() {
	Register("sniper", func() Strategy {
		return NewSniper(SniperParams{})
	})
	RegisterParams("sniper", func(params map[string]float64) (Strategy, error) {
		p := SniperParams{}
		if err := applyParams(&p, params); err != nil {
			return nil, fmt.Errorf("sniper: %w", err)
		}
		return NewSniper(p), nil
	})
}
