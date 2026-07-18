package strategy

import (
	"math"
	"time"
)

// Candle represents a single candlestick data point
type Candle struct {
	Time   time.Time
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume int64
}

// IsDoji checks if the candle is a Doji (indecision)
// A Doji has a very small body relative to its range.
func IsDoji(c Candle) bool {
	bodySize := math.Abs(c.Close - c.Open)
	totalRange := c.High - c.Low
	if totalRange == 0 {
		return true
	}
	// Body is less than 10% of the total range
	return bodySize <= (totalRange * 0.1)
}

// IsHammer checks if the candle is a Hammer (Bullish Reversal)
// Small body at the top, long lower wick (2x body), little/no upper wick.
func IsHammer(c Candle) bool {
	bodySize := math.Abs(c.Close - c.Open)
	totalRange := c.High - c.Low
	lowerWick := math.Min(c.Open, c.Close) - c.Low
	upperWick := c.High - math.Max(c.Open, c.Close)

	// Rules:
	// 1. Lower wick is at least 2x body size
	// 2. Upper wick is very small (less than body size)
	// 3. Body is in the upper 30% of the range (implied by wicks)
	return lowerWick >= (2*bodySize) && upperWick <= bodySize && totalRange > 0
}

// IsShootingStar checks if the candle is a Shooting Star (Bearish Reversal)
// Small body at the bottom, long upper wick (2x body), little/no lower wick.
func IsShootingStar(c Candle) bool {
	bodySize := math.Abs(c.Close - c.Open)
	totalRange := c.High - c.Low
	lowerWick := math.Min(c.Open, c.Close) - c.Low
	upperWick := c.High - math.Max(c.Open, c.Close)

	return upperWick >= (2*bodySize) && lowerWick <= bodySize && totalRange > 0
}

// IsBullishEngulfing checks for a bullish engulfing pattern (2 candles)
// prev: Previous candle (Red/Bearish)
// curr: Current candle (Green/Bullish)
// Current body completely engulfs previous body.
func IsBullishEngulfing(prev, curr Candle) bool {
	// Prev must be bearish (Close < Open)
	if prev.Close >= prev.Open {
		return false
	}
	// Curr must be bullish (Close > Open)
	if curr.Close <= curr.Open {
		return false
	}

	// Engulfing Logic:
	// Curr Open <= Prev Close (Opened lower or equal)
	// Curr Close >= Prev Open (Closed higher or equal)
	return curr.Open <= prev.Close && curr.Close >= prev.Open
}

// IsBearishEngulfing checks for a bearish engulfing pattern (2 candles)
// prev: Previous candle (Green/Bullish)
// curr: Current candle (Red/Bearish)
// Current body completely engulfs previous body.
func IsBearishEngulfing(prev, curr Candle) bool {
	// Prev must be bullish
	if prev.Close <= prev.Open {
		return false
	}
	// Curr must be bearish
	if curr.Close >= curr.Open {
		return false
	}

	// Engulfing Logic:
	// Curr Open >= Prev Close (Opened higher or equal)
	// Curr Close <= Prev Open (Closed lower or equal)
	return curr.Open >= prev.Close && curr.Close <= prev.Open
}
