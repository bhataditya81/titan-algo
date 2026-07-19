package strategy

import (
	"math"
)

// CalculateEMA calculates Exponential Moving Average
// prices: array of prices (oldest to newest)
// period: EMA period
func CalculateEMA(prices []float64, period int) []float64 {
	if len(prices) < period {
		return nil
	}

	ema := make([]float64, len(prices))
	multiplier := 2.0 / float64(period+1)

	// First EMA is SMA of first 'period' values
	sum := 0.0
	for i := 0; i < period; i++ {
		sum += prices[i]
	}
	ema[period-1] = sum / float64(period)

	// Calculate EMA for remaining values
	for i := period; i < len(prices); i++ {
		ema[i] = (prices[i]-ema[i-1])*multiplier + ema[i-1]
	}

	return ema
}

// CalculateSMA calculates Simple Moving Average
func CalculateSMA(prices []float64, period int) []float64 {
	if len(prices) < period {
		return nil
	}

	sma := make([]float64, len(prices))
	sum := 0.0

	for i := 0; i < len(prices); i++ {
		sum += prices[i]
		if i >= period {
			sum -= prices[i-period]
		}
		if i >= period-1 {
			sma[i] = sum / float64(period)
		}
	}

	return sma
}

// RSI represents the RSI indicator result
type RSI struct {
	Value float64 // Current RSI value (0-100)
}

// CalculateRSI calculates Relative Strength Index
// prices: array of prices (oldest to newest)
// period: RSI period (typically 14)
func CalculateRSI(prices []float64, period int) *RSI {
	if len(prices) < period+1 {
		return nil
	}

	gains := make([]float64, len(prices)-1)
	losses := make([]float64, len(prices)-1)

	for i := 1; i < len(prices); i++ {
		change := prices[i] - prices[i-1]
		if change > 0 {
			gains[i-1] = change
		} else {
			losses[i-1] = -change
		}
	}

	// Calculate initial average gain/loss
	avgGain := 0.0
	avgLoss := 0.0
	for i := 0; i < period; i++ {
		avgGain += gains[i]
		avgLoss += losses[i]
	}
	avgGain /= float64(period)
	avgLoss /= float64(period)

	// Smooth the averages
	for i := period; i < len(gains); i++ {
		avgGain = (avgGain*float64(period-1) + gains[i]) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + losses[i]) / float64(period)
	}

	// Calculate RSI.
	// ST-1 fix: avgLoss==0 with avgGain>0 means every move was a gain — that
	// is RSI=100 by definition (max overbought), NOT "insufficient data".
	// The old code returned nil here, which silently dropped the strongest
	// possible sell signal and biased the whole system long (asymmetric with
	// the avgGain==0 case, which already correctly produced RSI=0 via the
	// formula below). Both-zero (flat prices) is genuinely undefined; we
	// return the neutral midpoint (50) rather than nil so callers don't have
	// to special-case a nil RSI on dead/flat data.
	if avgGain == 0 && avgLoss == 0 {
		return &RSI{Value: 50}
	}
	if avgLoss == 0 {
		return &RSI{Value: 100}
	}
	rs := avgGain / avgLoss
	rsi := 100 - (100 / (1 + rs))

	return &RSI{Value: rsi}
}

// MACD represents the MACD indicator result
type MACD struct {
	MACDLine   float64 // MACD line value
	SignalLine float64 // Signal line value
	Histogram  float64 // MACD - Signal
	Bullish    bool    // True if MACD crossed above signal
	Bearish    bool    // True if MACD crossed below signal
}

// CalculateMACD calculates Moving Average Convergence Divergence
// prices: array of prices (oldest to newest)
// fastPeriod: fast EMA period (typically 12)
// slowPeriod: slow EMA period (typically 26)
// signalPeriod: signal line EMA period (typically 9)
func CalculateMACD(prices []float64, fastPeriod, slowPeriod, signalPeriod int) *MACD {
	if len(prices) < slowPeriod+signalPeriod {
		return nil
	}

	// Calculate fast and slow EMAs
	fastEMA := CalculateEMA(prices, fastPeriod)
	slowEMA := CalculateEMA(prices, slowPeriod)

	if fastEMA == nil || slowEMA == nil {
		return nil
	}

	// Calculate MACD line
	macdLine := make([]float64, len(prices))
	for i := slowPeriod - 1; i < len(prices); i++ {
		macdLine[i] = fastEMA[i] - slowEMA[i]
	}

	// Calculate signal line (EMA of MACD line)
	macdValues := macdLine[slowPeriod-1:]
	signalEMA := CalculateEMA(macdValues, signalPeriod)

	if len(signalEMA) < 2 {
		return nil
	}

	lastIdx := len(signalEMA) - 1
	prevIdx := lastIdx - 1

	currentMACD := macdValues[lastIdx]
	currentSignal := signalEMA[lastIdx]
	prevMACD := macdValues[prevIdx]
	prevSignal := signalEMA[prevIdx]

	// Detect crossovers
	bullish := prevMACD <= prevSignal && currentMACD > currentSignal
	bearish := prevMACD >= prevSignal && currentMACD < currentSignal

	return &MACD{
		MACDLine:   currentMACD,
		SignalLine: currentSignal,
		Histogram:  currentMACD - currentSignal,
		Bullish:    bullish,
		Bearish:    bearish,
	}
}

// BollingerBands represents the Bollinger Bands indicator result
type BollingerBands struct {
	Upper  float64 // Upper band
	Middle float64 // Middle band (SMA)
	Lower  float64 // Lower band
	Width  float64 // Band width (volatility measure)
}

// CalculateBollingerBands calculates Bollinger Bands
// prices: array of prices (oldest to newest)
// period: SMA period (typically 20)
// stdDevMultiplier: standard deviation multiplier (typically 2)
func CalculateBollingerBands(prices []float64, period int, stdDevMultiplier float64) *BollingerBands {
	if len(prices) < period {
		return nil
	}

	// Calculate SMA for middle band
	sma := CalculateSMA(prices, period)
	if sma == nil {
		return nil
	}

	middle := sma[len(sma)-1]

	// Calculate standard deviation
	recentPrices := prices[len(prices)-period:]
	variance := 0.0
	for _, p := range recentPrices {
		diff := p - middle
		variance += diff * diff
	}
	variance /= float64(period)
	stdDev := math.Sqrt(variance)

	upper := middle + (stdDev * stdDevMultiplier)
	lower := middle - (stdDev * stdDevMultiplier)

	return &BollingerBands{
		Upper:  upper,
		Middle: middle,
		Lower:  lower,
		Width:  (upper - lower) / middle * 100, // Width as percentage
	}
}

// VWAP represents the VWAP indicator result
type VWAP struct {
	Value float64 // VWAP value
}

// CalculateVWAP computes a DEGRADED tick-mode VWAP (ST-2): it uses close/LTP
// as a stand-in for typical price and is NOT session-anchored — it simply
// weights whatever price/volume window the caller passes in, however many
// days that window spans. Use this ONLY when candle (OHLC) data is
// unavailable and you have nothing better than a raw tick/LTP series with a
// matching volume series (per-interval, not cumulative).
//
// Prefer CalculateSessionVWAP whenever Candle data is available — it is
// anchored to the trading session and uses proper typical price.
//
// prices: array of prices (oldest to newest)
// volumes: array of volumes (oldest to newest)
func CalculateVWAP(prices []float64, volumes []float64) *VWAP {
	if len(prices) == 0 || len(prices) != len(volumes) {
		return nil
	}

	totalPriceVolume := 0.0
	totalVolume := 0.0

	for i := 0; i < len(prices); i++ {
		totalPriceVolume += prices[i] * volumes[i]
		totalVolume += volumes[i]
	}

	if totalVolume == 0 {
		return &VWAP{Value: prices[len(prices)-1]}
	}

	return &VWAP{Value: totalPriceVolume / totalVolume}
}

// CalculateSessionVWAP computes a session-anchored VWAP from candle data
// (fixes ST-2): it resets accumulation at the first candle of the current
// IST trading day (rather than rolling over whatever window the caller
// happens to hand it, possibly spanning multiple days), and uses typical
// price (High+Low+Close)/3 instead of close alone. candles must be sorted
// oldest to newest; Candle.Volume is treated as PER-INTERVAL volume (the
// semantics already produced by broker/historical.go), not cumulative.
func CalculateSessionVWAP(candles []Candle) *VWAP {
	if len(candles) == 0 {
		return nil
	}

	// Find the start of the current IST calendar day's session: walk back
	// from the end until the IST date changes.
	sessionStart := 0
	lastDate := candles[len(candles)-1].Time.In(IST)
	for i := len(candles) - 1; i >= 0; i-- {
		d := candles[i].Time.In(IST)
		y1, m1, d1 := d.Date()
		y2, m2, d2 := lastDate.Date()
		if y1 != y2 || m1 != m2 || d1 != d2 {
			sessionStart = i + 1
			break
		}
	}

	var totalPV, totalVol float64
	for i := sessionStart; i < len(candles); i++ {
		c := candles[i]
		typical := (c.High + c.Low + c.Close) / 3.0
		vol := float64(c.Volume)
		if vol < 0 {
			vol = 0
		}
		totalPV += typical * vol
		totalVol += vol
	}

	if totalVol == 0 {
		return &VWAP{Value: candles[len(candles)-1].Close}
	}
	return &VWAP{Value: totalPV / totalVol}
}
