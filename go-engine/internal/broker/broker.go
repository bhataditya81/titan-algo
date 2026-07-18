package broker

import "time"

// OrderType represents the type of order
type OrderType string

const (
	Market OrderType = "MARKET"
	Limit  OrderType = "LIMIT"
)

// OrderSide represents buy or sell
type OrderSide string

const (
	Buy  OrderSide = "BUY"
	Sell OrderSide = "SELL"
)

// Order represents a trading order
type Order struct {
	Symbol    string
	Quantity  int
	Side      OrderSide
	OrderType OrderType
	Price     float64 // For limit orders
}

// FilledOrder represents an executed order
type FilledOrder struct {
	OrderID        string
	Symbol         string
	Quantity       int
	Side           OrderSide
	FillPrice      float64
	Slippage       float64
	TransactionFee float64
	Timestamp      time.Time
}

// Position represents an open position
type Position struct {
	Symbol       string
	Quantity     int
	AveragePrice float64
	Side         OrderSide
}

// TradeService defines the interface for interacting with brokers
type TradeService interface {
	// Connect establishes connection to the broker
	Connect() error

	// Subscribe to market data for given symbols
	Subscribe(tokens []string) error

	// PlaceOrder places a new order and returns the filled order
	PlaceOrder(order Order) (*FilledOrder, error)

	// GetBalance returns the current account balance
	GetBalance() float64

	// GetPositions returns all open positions
	GetPositions() map[string]*Position

	// GetCurrentPrice returns the current market price for a symbol
	GetCurrentPrice(symbol string) float64

	// GetCurrentVolume returns the current market volume for a symbol
	GetCurrentVolume(symbol string) float64

	// FetchMarketDataBatch fetches price and volume for multiple symbols in a single API call
	// Returns maps of symbol -> price and symbol -> volume
	FetchMarketDataBatch(symbols []string) (map[string]float64, map[string]float64)

	// Close closes the connection
	Close() error
}
