package strategy

import (
	"fmt"
	"time"
)

// SniperStrategy implements a high-probability "Sniper" approach
// It combines Trend (EMA), Momentum (RSI), and Price Action (Candlesticks).
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

	// Data Management
	candles       map[string][]Candle // Symbol -> History
	currentCandle map[string]*Candle  // Symbol -> In-progress candle
}

// NewSniperStrategy creates a new instance with default "Sniper" settings
func NewSniperStrategy() *SniperStrategy {
	return &SniperStrategy{
		StrategyName:  "Sniper F&O Strategy",
		StopLossPct:   1.0, // 1% Tight Stop
		TargetPct:     2.0, // 2% Target (1:2 Risk/Reward)
		TrailingSL:    0.5, // Trailing stop start
		EMAPeriod:     50,  // 50-EMA for Trend
		RSIPeriod:     14,  // Standard RSI
		CandleMinutes: 5,   // 5-min timeframe
		candles:       make(map[string][]Candle),
		currentCandle: make(map[string]*Candle),
	}
}

func (s *SniperStrategy) Name() string {
	return s.StrategyName
}

func (s *SniperStrategy) Evaluate(symbol string, prices []float64, volumes []float64, currentTime time.Time) Signal {
	if len(prices) == 0 {
		return Signal{Action: Hold, Reason: "No data"}
	}

	price := prices[len(prices)-1]

	// 1. Manage Candle Building (Convert Ticks -> Candles)
	s.updateCandle(symbol, price)

	// Check history
	history := s.candles[symbol]
	if len(history) < s.EMAPeriod+2 { // Need enough data for EMA
		return Signal{Action: Hold, Reason: fmt.Sprintf("Building History: %d/%d", len(history), s.EMAPeriod+2)}
	}

	// Delegate to core logic
	return s.EvaluateLogic(history)
}

// EvaluateCandles allows backtesting on pre-built history
func (s *SniperStrategy) EvaluateCandles(history []Candle) Signal {
	if len(history) < s.EMAPeriod+2 {
		return Signal{Action: Hold, Reason: "Insufficient History"}
	}
	return s.EvaluateLogic(history)
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

	// --- BUY SETUP ---
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

// updateCandle simulates candle formation from ticks
// In production, this tracks time. Here, we simulate a new candle every 5 ticks for demo speed.
func (s *SniperStrategy) updateCandle(symbol string, price float64) {
	candle, exists := s.currentCandle[symbol]
	if !exists {
		// Start new candle
		s.currentCandle[symbol] = &Candle{Open: price, High: price, Low: price, Close: price}
		return
	}

	// Update High/Low
	candle.Close = price
	if price > candle.High {
		candle.High = price
	}
	if price < candle.Low {
		candle.Low = price
	}
	candle.Volume++

	// Complete candle condition (every 5 update calls = 1 candle for fast Paper Trading)
	if candle.Volume >= 5 {
		// Finalize candle
		s.candles[symbol] = append(s.candles[symbol], *candle)
		// Start new
		delete(s.currentCandle, symbol)
	}
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
