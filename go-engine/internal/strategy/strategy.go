package strategy

import "time"

// SignalAction represents the trading action to take
type SignalAction string

const (
	Buy  SignalAction = "BUY"
	Sell SignalAction = "SELL"
	Hold SignalAction = "HOLD"
)

// LegDirection for multi-leg orders
type LegDirection string

const (
	LegBuy  LegDirection = "BUY"
	LegSell LegDirection = "SELL"
)

// OrderLeg defines a single leg of a multi-leg strategy
type OrderLeg struct {
	Direction    LegDirection
	StrikeOffset int    // e.g., 0 for ATM, 100 for +100 OTM
	OptionType   string // "CE" or "PE"
}

// Signal represents a trading signal from a strategy
type Signal struct {
	Action   SignalAction
	Strength float64 // 0.0 to 1.0 (confidence level)
	Reason   string
	Legs     []OrderLeg // Multi-leg orders
}

// Strategy defines the interface for trading strategies
type Strategy interface {
	// Name returns the strategy name
	Name() string

	// Evaluate analyzes price/volume data and returns a trading signal
	Evaluate(symbol string, prices []float64, volumes []float64, currentTime time.Time) Signal

	// EvaluateCandles analyzes full candle history (OHLCV)
	EvaluateCandles(history []Candle) Signal
}
