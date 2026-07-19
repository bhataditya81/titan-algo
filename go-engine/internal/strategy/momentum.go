package strategy

import (
	"fmt"
)

// MomentumStrategy implements a multi-indicator momentum strategy
// combining RSI, MACD, Bollinger Bands, and VWAP
type MomentumStrategy struct {
	// RSI parameters
	RSIPeriod     int
	RSIOversold   float64 // Buy when RSI below this (default: 35)
	RSIOverbought float64 // Sell when RSI above this (default: 65)

	// MACD parameters
	MACDFast   int
	MACDSlow   int
	MACDSignal int

	// Bollinger Bands parameters
	BollingerPeriod int
	BollingerStdDev float64

	// Minimum signal strength to trigger trade
	MinSignalStrength float64
}

// NewMomentumStrategy creates a new momentum strategy with default parameters
func NewMomentumStrategy() *MomentumStrategy {
	return &MomentumStrategy{
		RSIPeriod:         14,
		RSIOversold:       35,
		RSIOverbought:     65,
		MACDFast:          12,
		MACDSlow:          26,
		MACDSignal:        9,
		BollingerPeriod:   20,
		BollingerStdDev:   2.0,
		MinSignalStrength: 0.6,
	}
}

// Name returns the strategy name
func (m *MomentumStrategy) Name() string {
	return "Momentum (RSI + MACD + Bollinger)"
}

// Evaluate analyzes price data and returns a trading signal
func (m *MomentumStrategy) Evaluate(ctx EvalContext) Signal {
	prices := ctx.ClosePrices()
	volumes := ctx.VolumeSeries()

	// Need minimum data points for all indicators
	minRequired := m.MACDSlow + m.MACDSignal + 5
	if len(prices) < minRequired {
		return Signal{
			Action:   Hold,
			Strength: 0,
			Reason:   fmt.Sprintf("Insufficient data: have %d, need %d", len(prices), minRequired),
		}
	}

	// Calculate indicators
	rsi := CalculateRSI(prices, m.RSIPeriod)
	macd := CalculateMACD(prices, m.MACDFast, m.MACDSlow, m.MACDSignal)
	bb := CalculateBollingerBands(prices, m.BollingerPeriod, m.BollingerStdDev)

	// Prefer session-anchored candle VWAP (ST-2 fix) when candles are
	// available; fall back to the degraded tick-mode VWAP otherwise.
	var vwap *VWAP
	if len(ctx.Candles) > 0 {
		vwap = CalculateSessionVWAP(ctx.Candles)
	} else {
		vwap = CalculateVWAP(prices, volumes)
	}

	if rsi == nil || macd == nil || bb == nil {
		return Signal{
			Action:   Hold,
			Strength: 0,
			Reason:   "Failed to calculate indicators",
		}
	}

	currentPrice := prices[len(prices)-1]

	// Evaluate BUY conditions
	buyScore := m.evaluateBuyConditions(currentPrice, rsi, macd, bb, vwap)

	// Evaluate SELL conditions
	sellScore := m.evaluateSellConditions(currentPrice, rsi, macd, bb, vwap)

	// Determine signal based on scores
	if buyScore >= m.MinSignalStrength && buyScore > sellScore {
		return Signal{
			Action:   Buy,
			Strength: buyScore,
			Reason:   m.generateBuyReason(rsi, macd, bb, vwap, currentPrice),
		}
	}

	if sellScore >= m.MinSignalStrength && sellScore > buyScore {
		return Signal{
			Action:   Sell,
			Strength: sellScore,
			Reason:   m.generateSellReason(rsi, macd, bb, vwap, currentPrice),
		}
	}

	return Signal{
		Action:   Hold,
		Strength: 0,
		Reason:   fmt.Sprintf("RSI=%.1f, MACD=%.2f, Price=%.2f", rsi.Value, macd.Histogram, currentPrice),
	}
}

// evaluateBuyConditions calculates buy signal strength (0-1).
//
// ST-10 fix: previously this divided the accumulated score by `conditions`
// then multiplied by `conditions` again — a mathematical no-op
// (score/conditions*conditions == score) that did nothing, and `conditions`
// was ALWAYS 1.0 anyway (0.3+0.3+0.2+0.2, added unconditionally regardless
// of whether VWAP was actually available). Now `weight` only accumulates for
// conditions that were actually evaluable (VWAP may be nil), and the score is
// genuinely normalized by that weight — so a missing VWAP no longer silently
// caps the maximum achievable score at 0.8.
func (m *MomentumStrategy) evaluateBuyConditions(price float64, rsi *RSI, macd *MACD, bb *BollingerBands, vwap *VWAP) float64 {
	score := 0.0
	weight := 0.0

	// Condition 1: RSI oversold (weight: 0.3)
	const rsiWeight = 0.3
	if rsi.Value < m.RSIOversold {
		score += rsiWeight
	} else if rsi.Value < 45 { // Moderately low RSI
		score += rsiWeight * 0.5
	}
	weight += rsiWeight

	// Condition 2: MACD bullish crossover or positive histogram (weight: 0.3)
	const macdWeight = 0.3
	if macd.Bullish {
		score += macdWeight
	} else if macd.Histogram > 0 && macd.MACDLine > macd.SignalLine {
		score += macdWeight * 0.5
	}
	weight += macdWeight

	// Condition 3: Price at or below lower Bollinger Band (weight: 0.2)
	const bbWeight = 0.2
	if price <= bb.Lower {
		score += bbWeight
	} else if price < bb.Middle {
		distanceRatio := (bb.Middle - price) / (bb.Middle - bb.Lower)
		score += (bbWeight / 2) * distanceRatio
	}
	weight += bbWeight

	// Condition 4: Price below VWAP (weight: 0.2) — only counted when VWAP
	// is actually available.
	const vwapWeight = 0.2
	if vwap != nil {
		if price < vwap.Value {
			discount := (vwap.Value - price) / vwap.Value * 100
			if discount > 1.0 { // More than 1% below VWAP
				score += vwapWeight
			} else {
				score += vwapWeight / 2
			}
		}
		weight += vwapWeight
	}

	if weight == 0 {
		return 0
	}
	return score / weight
}

// evaluateSellConditions calculates sell signal strength (0-1). See
// evaluateBuyConditions for the ST-10 normalization fix (mirrored here).
func (m *MomentumStrategy) evaluateSellConditions(price float64, rsi *RSI, macd *MACD, bb *BollingerBands, vwap *VWAP) float64 {
	score := 0.0
	weight := 0.0

	// Condition 1: RSI overbought (weight: 0.3)
	const rsiWeight = 0.3
	if rsi.Value > m.RSIOverbought {
		score += rsiWeight
	} else if rsi.Value > 55 { // Moderately high RSI
		score += rsiWeight * 0.5
	}
	weight += rsiWeight

	// Condition 2: MACD bearish crossover or negative histogram (weight: 0.3)
	const macdWeight = 0.3
	if macd.Bearish {
		score += macdWeight
	} else if macd.Histogram < 0 && macd.MACDLine < macd.SignalLine {
		score += macdWeight * 0.5
	}
	weight += macdWeight

	// Condition 3: Price at or above upper Bollinger Band (weight: 0.2)
	const bbWeight = 0.2
	if price >= bb.Upper {
		score += bbWeight
	} else if price > bb.Middle {
		distanceRatio := (price - bb.Middle) / (bb.Upper - bb.Middle)
		score += (bbWeight / 2) * distanceRatio
	}
	weight += bbWeight

	// Condition 4: Price above VWAP (weight: 0.2) — only counted when VWAP
	// is actually available.
	const vwapWeight = 0.2
	if vwap != nil {
		if price > vwap.Value {
			premium := (price - vwap.Value) / vwap.Value * 100
			if premium > 1.0 { // More than 1% above VWAP
				score += vwapWeight
			} else {
				score += vwapWeight / 2
			}
		}
		weight += vwapWeight
	}

	if weight == 0 {
		return 0
	}
	return score / weight
}

func (m *MomentumStrategy) generateBuyReason(rsi *RSI, macd *MACD, bb *BollingerBands, vwap *VWAP, price float64) string {
	reasons := []string{}

	if rsi.Value < m.RSIOversold {
		reasons = append(reasons, fmt.Sprintf("RSI=%.1f (oversold)", rsi.Value))
	}
	if macd.Bullish {
		reasons = append(reasons, "MACD bullish crossover")
	}
	if price <= bb.Lower {
		reasons = append(reasons, "Price at lower BB")
	}
	if vwap != nil && price < vwap.Value {
		reasons = append(reasons, fmt.Sprintf("Price below VWAP (%.2f)", vwap.Value))
	}

	if len(reasons) == 0 {
		return "Multiple moderate buy signals"
	}

	result := reasons[0]
	for i := 1; i < len(reasons); i++ {
		result += " + " + reasons[i]
	}
	return result
}

func (m *MomentumStrategy) generateSellReason(rsi *RSI, macd *MACD, bb *BollingerBands, vwap *VWAP, price float64) string {
	reasons := []string{}

	if rsi.Value > m.RSIOverbought {
		reasons = append(reasons, fmt.Sprintf("RSI=%.1f (overbought)", rsi.Value))
	}
	if macd.Bearish {
		reasons = append(reasons, "MACD bearish crossover")
	}
	if price >= bb.Upper {
		reasons = append(reasons, "Price at upper BB")
	}
	if vwap != nil && price > vwap.Value {
		reasons = append(reasons, fmt.Sprintf("Price above VWAP (%.2f)", vwap.Value))
	}

	if len(reasons) == 0 {
		return "Multiple moderate sell signals"
	}

	result := reasons[0]
	for i := 1; i < len(reasons); i++ {
		result += " + " + reasons[i]
	}
	return result
}

func init() {
	Register("momentum", func() Strategy {
		return NewMomentumStrategy()
	})
}
