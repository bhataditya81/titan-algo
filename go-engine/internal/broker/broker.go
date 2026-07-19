package broker

import (
	"errors"
	"fmt"
	"time"
)

// OrderType represents the type of order
type OrderType string

const (
	Market OrderType = "MARKET"
	Limit  OrderType = "LIMIT"
	// StopLossMarket is used for broker-side stop-loss orders
	// (Angel One variety STOPLOSS, ordertype STOPLOSS_MARKET).
	StopLossMarket OrderType = "STOPLOSS_MARKET"
)

// OrderSide represents buy or sell
type OrderSide string

const (
	Buy  OrderSide = "BUY"
	Sell OrderSide = "SELL"
)

// OrderIntent classifies why an order is being placed. Position-reducing
// (exit / stop-loss / flatten) orders MUST use IntentReduceOnly: the broker's
// circuit breaker never blocks reduce-only orders (audit EX-2).
type OrderIntent string

const (
	// IntentEntry opens or increases risk. Subject to the circuit breaker.
	IntentEntry OrderIntent = "ENTRY"
	// IntentReduceOnly closes or reduces an existing position. Bypasses the
	// circuit breaker — exits must never be blocked by entry-side failures.
	IntentReduceOnly OrderIntent = "REDUCE_ONLY"
)

// Order represents a trading order.
//
// NOTE for WP-9 (integration): the Intent field is new. Exit / flatten /
// stop-loss order flows MUST set Intent: IntentReduceOnly so the circuit
// breaker cannot block them. Zero value ("") is treated as IntentEntry
// (fail-closed: unknown intent gets the stricter entry-side gating).
type Order struct {
	Symbol    string
	Quantity  int
	Side      OrderSide
	OrderType OrderType
	Price     float64     // For limit orders
	Intent    OrderIntent // ENTRY (default) or REDUCE_ONLY
}

// ReduceOnly reports whether this order only reduces an existing position.
func (o Order) ReduceOnly() bool {
	return o.Intent == IntentReduceOnly
}

// FilledOrder represents an executed order.
//
// Quantity is the ACTUAL filled quantity (broker "filledshares"), which may be
// less than the requested quantity on a partial fill. Callers must use this
// value — never the requested quantity — for position and P&L accounting.
type FilledOrder struct {
	OrderID        string
	Symbol         string
	Quantity       int // actual filled quantity (filledshares)
	RequestedQty   int // originally requested quantity
	Side           OrderSide
	FillPrice      float64
	Slippage       float64
	TransactionFee float64
	Timestamp      time.Time
	Status         string // broker status: "complete", "cancelled" (partial-then-cancelled), ...
}

// Partial reports whether the order filled for less than the requested quantity.
func (f *FilledOrder) Partial() bool {
	return f.RequestedQty > 0 && f.Quantity < f.RequestedQty
}

// Position represents an open position
type Position struct {
	Symbol       string
	Quantity     int
	AveragePrice float64
	Side         OrderSide
}

// ---------------------------------------------------------------------------
// Sentinel errors (cross-package contracts — see remediation plan Appendix B)
// ---------------------------------------------------------------------------

var (
	// ErrOrderIndeterminate means an order was accepted by the broker but its
	// terminal state (filled / rejected / cancelled) could not be confirmed
	// within the poll budget. The position may or may not exist at the broker.
	// Callers MUST NOT assume either outcome: do not roll back risk state, do
	// not fabricate a fill. Persist the order ID and resolve via the order
	// book. Use errors.As with *IndeterminateOrderError to get the order ID.
	ErrOrderIndeterminate = errors.New("order state indeterminate")

	// ErrStalePrice means a cached price is older than the caller's threshold.
	ErrStalePrice = errors.New("price is stale")

	// ErrNoPrice means no price has ever been cached for the symbol.
	ErrNoPrice = errors.New("no price available")

	// ErrWAFBlocked means the Angel One WAF / rate limiter returned an HTML
	// block page instead of a JSON API response. Data from such a response is
	// unusable; callers must treat the request as FAILED (never as "empty").
	ErrWAFBlocked = errors.New("angel API blocked by WAF/rate-limiter")

	// ErrAuthFailed means the session token was rejected and could not be
	// refreshed. The broker is unhealthy until a full re-login succeeds.
	ErrAuthFailed = errors.New("angel API authentication failed")

	// ErrCircuitOpen means the circuit breaker is open and the (entry) order
	// was refused locally. Reduce-only orders are never gated by this.
	ErrCircuitOpen = errors.New("circuit breaker open")
)

// IndeterminateOrderError carries the broker order ID of an order whose final
// state is unknown. errors.Is(err, ErrOrderIndeterminate) returns true.
type IndeterminateOrderError struct {
	OrderID    string
	Symbol     string
	LastStatus string // last observed non-terminal status ("open", "pending", ...)
}

func (e *IndeterminateOrderError) Error() string {
	return fmt.Sprintf("order %s (%s) state indeterminate after poll budget (last status %q): resolve via order book before assuming anything",
		e.OrderID, e.Symbol, e.LastStatus)
}

func (e *IndeterminateOrderError) Is(target error) bool {
	return target == ErrOrderIndeterminate
}

// HTTPStatusError is returned when an Angel API call returns a non-200 status.
type HTTPStatusError struct {
	StatusCode int
	Path       string
	Body       string // truncated
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("angel API %s returned HTTP %d: %s", e.Path, e.StatusCode, e.Body)
}

// ---------------------------------------------------------------------------
// Interfaces
// ---------------------------------------------------------------------------

// TradeService defines the interface for interacting with brokers
type TradeService interface {
	// Connect establishes connection to the broker
	Connect() error

	// Subscribe to market data for given symbols
	Subscribe(tokens []string) error

	// PlaceOrder places a new order and returns the filled order.
	//
	// Contract (WP-1):
	//   - FilledOrder.Quantity is the ACTUAL filled quantity, which may be
	//     less than order.Quantity (partial fill).
	//   - If the order's final state cannot be confirmed, the error satisfies
	//     errors.Is(err, ErrOrderIndeterminate) and no FilledOrder is returned.
	//     Callers must NOT roll back risk state on that error.
	//   - Orders with Intent == IntentReduceOnly are never blocked by the
	//     circuit breaker.
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

// ExtendedTradeService is implemented by brokers that support the hardened
// WP-1 order surface. AngelBroker implements it. WP-9 should type-assert:
//
//	if ext, ok := svc.(broker.ExtendedTradeService); ok { ... }
type ExtendedTradeService interface {
	TradeService

	// PlaceStopLossOrder places a broker-side stop-loss market order
	// (variety STOPLOSS, ordertype STOPLOSS_MARKET) and returns the broker
	// order ID. It does NOT wait for a fill — SL orders rest at the broker.
	PlaceStopLossOrder(symbol string, qty int, triggerPrice float64, side OrderSide) (string, error)

	// CancelOrder cancels an open order by broker order ID.
	CancelOrder(orderID string) error

	// GetCurrentPriceWithAge returns the cached price and how old it is.
	// Returns ErrNoPrice if the symbol has never been priced. Callers decide
	// their own staleness threshold (compare age; ErrStalePrice is provided
	// as the conventional sentinel for that decision).
	GetCurrentPriceWithAge(symbol string) (price float64, age time.Duration, err error)

	// Healthy reports whether the broker session is usable (connected and
	// not in a failed-auth state). When false, HealthError explains why.
	Healthy() bool

	// HealthError returns the error that made the broker unhealthy, or nil.
	HealthError() error

	// RefreshBalance fetches the real available balance from the broker
	// (RMS funds endpoint) and updates the cached value.
	RefreshBalance() (float64, error)
}
