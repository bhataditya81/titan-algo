package strategy

import (
	"fmt"
	"sync"
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

	mu       sync.Mutex
	candles  map[string][]Candle           // Symbol -> completed candle history
	building map[string]*sniperCandleState // Symbol -> in-progress candle
}

type sniperCandleState struct {
	bucketStart    time.Time
	candle         Candle
	lastCumVolume  float64
	haveLastVolume bool
}

// NewSniperStrategy creates a new instance with default "Sniper" settings
func NewSniperStrategy() *SniperStrategy {
	return &SniperStrategy{
		StrategyName:  "Sniper F&O Strategy",
		StopLossPct:   1.0, // 1% Tight Stop
		TargetPct:     2.0, // 2% Target (1:2 Risk/Reward)
		TrailingSL:    0.5, // Trailing stop distance (%), applied by the execution layer
		EMAPeriod:     50,  // 50-EMA for Trend
		RSIPeriod:     14,  // Standard RSI
		CandleMinutes: 5,   // 5-min timeframe
		candles:       make(map[string][]Candle),
		building:      make(map[string]*sniperCandleState),
	}
}

func (s *SniperStrategy) Name() string {
	return s.StrategyName
}

func (s *SniperStrategy) Evaluate(ctx EvalContext) Signal {
	// Candle-mode: caller already supplied full, closed candle history
	// (backtest / EvaluateCandles helper) with no live tick series. Skip
	// tick aggregation entirely and evaluate directly against the given
	// bars — every call here is a fresh, fully-formed bar, so the
	// one-signal-per-completed-candle latch does not apply (the caller
	// controls how often it calls Evaluate in this mode).
	if len(ctx.Prices) == 0 && len(ctx.Candles) > 0 {
		if len(ctx.Candles) < s.EMAPeriod+2 {
			return Signal{Action: Hold, Reason: "Insufficient History"}
		}
		sig := s.EvaluateLogic(ctx.Candles)
		if sig.Action == Buy || sig.Action == Sell {
			s.attachStops(&sig, ctx.Candles[len(ctx.Candles)-1].Close)
		}
		return sig
	}

	if ctx.Symbol == "" || len(ctx.Prices) == 0 {
		return Signal{Action: Hold, Reason: "No data"}
	}

	completed := s.updateCandle(ctx)

	s.mu.Lock()
	history := append([]Candle(nil), s.candles[ctx.Symbol]...)
	s.mu.Unlock()

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

// updateCandle aggregates the latest tick from ctx into a real wall-clock
// IST candle for ctx.Symbol. Returns true exactly on the tick that
// completes a prior candle (crosses a bucket boundary), false otherwise.
func (s *SniperStrategy) updateCandle(ctx EvalContext) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	price := ctx.Prices[len(ctx.Prices)-1]
	haveVol := len(ctx.Volumes) > 0
	var cumVol float64
	if haveVol {
		cumVol = ctx.Volumes[len(ctx.Volumes)-1]
	}

	now := ctx.Now
	if now.IsZero() {
		now = time.Now()
	}
	interval := time.Duration(s.CandleMinutes) * time.Minute
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	bucketStart := floorToInterval(now.In(IST), interval)

	symbol := ctx.Symbol
	st, exists := s.building[symbol]

	if !exists || !st.bucketStart.Equal(bucketStart) {
		completed := false
		if exists {
			s.candles[symbol] = append(s.candles[symbol], st.candle)
			completed = true
		}
		s.building[symbol] = &sniperCandleState{
			bucketStart:    bucketStart,
			candle:         Candle{Time: bucketStart, Open: price, High: price, Low: price, Close: price},
			lastCumVolume:  cumVol,
			haveLastVolume: haveVol,
		}
		return completed
	}

	// Same bucket: update the in-progress candle in place.
	st.candle.Close = price
	if price > st.candle.High {
		st.candle.High = price
	}
	if price < st.candle.Low {
		st.candle.Low = price
	}
	if haveVol && st.haveLastVolume && cumVol >= st.lastCumVolume {
		st.candle.Volume += int64(cumVol - st.lastCumVolume)
	}
	st.lastCumVolume = cumVol
	st.haveLastVolume = haveVol
	return false
}

// floorToInterval floors t (already in the desired location) down to the
// nearest multiple of interval since local midnight.
func floorToInterval(t time.Time, interval time.Duration) time.Time {
	if interval <= 0 {
		return t
	}
	y, mo, d := t.Date()
	dayStart := time.Date(y, mo, d, 0, 0, 0, 0, t.Location())
	elapsed := t.Sub(dayStart)
	floored := (elapsed / interval) * interval
	return dayStart.Add(floored)
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
		return NewSniperStrategy()
	})
}
