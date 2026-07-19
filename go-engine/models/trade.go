// Package models holds plain Go data structures shared by the state
// persistence layer (internal/state) and, eventually, its callers.
//
// NOTE: earlier versions of this file carried GORM struct tags
// (`gorm:"primaryKey"` etc.) implying a database-backed ORM layer that never
// existed anywhere in this codebase (audit finding EX-8 — dead/misleading
// code). Those tags have been removed. These are now plain structs consumed
// directly by internal/state's hand-written SQL, with no ORM in between.
package models

import "time"

// PositionSide is the direction of a position (matches broker.OrderSide's
// string values so callers can convert with a simple string cast).
type PositionSide string

const (
	SideBuy  PositionSide = "BUY"
	SideSell PositionSide = "SELL"
)

// PositionStatus is the lifecycle status of a persisted Position.
type PositionStatus string

const (
	PositionOpen   PositionStatus = "OPEN"
	PositionClosed PositionStatus = "CLOSED"
)

// Position is a persisted representation of an open or closed trading
// position. ID is the store's primary key for the position record — it is
// distinct from Symbol because a symbol may be opened, closed, and reopened
// multiple times over the life of the system and each occurrence needs its
// own durable identity for audit purposes.
type Position struct {
	ID            string         // unique position id (see state.Store.NextPositionID)
	Symbol        string         // trading symbol / instrument token
	Exchange      string         // e.g. "NFO", "NSE"
	Side          PositionSide   // BUY (long) or SELL (short)
	Quantity      int            // signed-magnitude quantity (always positive; Side carries direction)
	EntryPrice    float64        // average entry price
	ExitPrice     float64        // average exit price (zero while open)
	Strategy      string         // strategy name that opened this position, e.g. "nine_twenty"
	Status        PositionStatus // OPEN or CLOSED
	EntryTime     time.Time      // UTC
	ExitTime      time.Time      // UTC, zero value while open
	BrokerOrderID string         // broker order id that opened the position, if known
	Note          string         // free-form annotation (e.g. reconciliation notes)
}

// OrderAttemptStatus is the lifecycle status of an OrderAttempt.
type OrderAttemptStatus string

const (
	// OrderIntent is recorded BEFORE the network call to the broker is made.
	OrderIntent OrderAttemptStatus = "intent"
	// OrderFilled means the broker confirmed a full fill.
	OrderFilled OrderAttemptStatus = "filled"
	// OrderPartial means the broker confirmed a partial fill.
	OrderPartial OrderAttemptStatus = "partial"
	// OrderRejected means the broker confirmed rejection; no position was opened.
	OrderRejected OrderAttemptStatus = "rejected"
	// OrderIndeterminate means the outcome is unknown (e.g. network timeout
	// after the request may have reached the broker). Callers must NOT
	// assume no fill occurred; this is the state that drives reconciliation.
	OrderIndeterminate OrderAttemptStatus = "indeterminate"
	// OrderCancelled means the order was cancelled before/without a fill.
	OrderCancelled OrderAttemptStatus = "cancelled"
)

// OrderAttempt records the "intent then resolution" audit trail for a single
// broker order. The intended usage pattern (see internal/state package docs
// and doc comments on Store.SaveOrderAttempt / Store.MarkOrderResolved):
//
//  1. Generate a ClientOrderID via Store.NextClientOrderID().
//  2. Call Store.SaveOrderAttempt with Status = OrderIntent BEFORE issuing
//     the network call to the broker.
//  3. Issue the order over the network.
//  4. Call Store.MarkOrderResolved with the outcome (filled/partial/
//     rejected/indeterminate) once known.
//
// This ordering guarantees that even if the process crashes or the network
// call times out ambiguously, there is a durable record of intent that a
// startup reconciliation pass can use to recover.
type OrderAttempt struct {
	ClientOrderID  string // e.g. "titan-<unixnano>-<seq>"
	Symbol         string
	Side           PositionSide
	Quantity       int // requested quantity
	Price          float64
	OrderType      string // e.g. "MARKET", "LIMIT", "STOPLOSS_MARKET"
	Strategy       string
	Status         OrderAttemptStatus
	BrokerOrderID  string  // populated once known
	FilledQuantity int     // populated on resolution
	FilledPrice    float64 // populated on resolution
	Error          string  // populated on rejection/error
	CreatedAt      time.Time
	ResolvedAt     time.Time // zero value until resolved
}

// RiskSnapshot is a persisted point-in-time view of the risk manager's
// aggregate state, sufficient to resume a session after a restart without
// losing track of realized P&L or capital already committed to positions.
type RiskSnapshot struct {
	Balance     float64   // current account balance
	RealizedPnL float64   // realized profit/loss for the session
	SessionUsed float64   // capital currently locked in open positions
	UpdatedAt   time.Time // UTC, when this snapshot was written
}
