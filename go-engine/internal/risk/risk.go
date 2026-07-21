// Package risk implements session risk management for TitanAlgo:
// charge estimation (single source of truth for Indian market fees),
// order validation (including margin-aware SELL validation), position
// capital locking, per-minute order throttling, stop-loss checks, and
// a runtime kill switch.
//
// Concurrency: every exported method on Manager is safe for concurrent
// use. All mutable state reads/writes go through m.mu; the kill switch
// is additionally backed by an atomic.Bool so it can be flipped from
// any goroutine without contending the main lock.
package risk

import (
	"fmt"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// Trade types and sides
// ─────────────────────────────────────────────────────────────────────────

// TradeType represents the type of trade for charge calculation.
type TradeType string

const (
	EquityDelivery TradeType = "equity_delivery"
	EquityIntraday TradeType = "equity_intraday"

	// F&O trade types (FY 2025-26 charge schedule; futures and options
	// attract very different STT / txn / stamp rates, so they must not
	// share one bucket).
	FutIntraday TradeType = "fut_intraday"
	FutCarry    TradeType = "fut_carry"
	OptIntraday TradeType = "opt_intraday"
	OptCarry    TradeType = "opt_carry"

	// FNO is DEPRECATED. It conflated futures and options under one
	// (wrong) rate. Retained only for backward compatibility; treated as
	// an OPTIONS trade (OptCarry) for all charge math, matching how the
	// engine actually trades index option premium on CARRYFORWARD.
	FNO TradeType = "fno"
)

// OrderSide represents buy or sell.
type OrderSide string

const (
	Buy  OrderSide = "BUY"
	Sell OrderSide = "SELL"
)

// normalizeTradeType maps the deprecated FNO alias onto options.
func normalizeTradeType(t TradeType) TradeType {
	if t == FNO {
		return OptCarry
	}
	return t
}

// IsDerivative reports whether the trade type is an F&O product (margin-aware
// SELL validation applies).
func IsDerivative(t TradeType) bool {
	switch normalizeTradeType(t) {
	case FutIntraday, FutCarry, OptIntraday, OptCarry:
		return true
	}
	return false
}

func isOption(t TradeType) bool {
	switch normalizeTradeType(t) {
	case OptIntraday, OptCarry:
		return true
	}
	return false
}

func isFuture(t TradeType) bool {
	switch normalizeTradeType(t) {
	case FutIntraday, FutCarry:
		return true
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────
// Charge rates — the single editable table
// ─────────────────────────────────────────────────────────────────────────

// ChargeRates holds every statutory/exchange/broker rate used for fee math.
// All *Pct fields are percentages (0.1 == 0.1%).
//
// Rates are current AS OF FY 2025-26 (India, NSE, discount-broker flat-fee
// model such as Angel One / Zerodha). If SEBI/NSE/government revise rates,
// edit ONLY this table (DefaultChargeRates) — do not duplicate rates
// anywhere else in this codebase.
type ChargeRates struct {
	// Brokerage
	BrokerageFlatPerOrder float64 // flat ₹ per executed order (F&O always flat; equity capped at this)
	BrokerageEquityPct    float64 // % of turnover for equity; charged as min(flat, pct)

	// STT (Securities Transaction Tax)
	STTOptionSellPct         float64 // % of premium, SELL side only
	STTFutureSellPct         float64 // % of turnover, SELL side only
	STTEquityIntradaySellPct float64 // % of turnover, SELL side only
	STTEquityDeliveryPct     float64 // % of turnover, BOTH sides

	// Exchange transaction charges (NSE)
	TxnOptionPct float64 // % of premium
	TxnFuturePct float64 // % of turnover
	TxnEquityPct float64 // % of turnover

	// Stamp duty (BUY side only)
	StampOptionBuyPct         float64
	StampFutureBuyPct         float64
	StampEquityDeliveryBuyPct float64
	StampEquityIntradayBuyPct float64

	SEBIPct float64 // both sides; 0.0001% == ₹10/crore
	GSTPct  float64 // applied on (brokerage + exchange txn charges + SEBI fee)
}

// DefaultChargeRates returns the FY 2025-26 rate card.
// AS OF FY 2025-26 — re-verify against NSE/SEBI circulars each fiscal year.
func DefaultChargeRates() ChargeRates {
	return ChargeRates{
		BrokerageFlatPerOrder: 20.0,
		BrokerageEquityPct:    0.03,

		STTOptionSellPct:         0.1,
		STTFutureSellPct:         0.02,
		STTEquityIntradaySellPct: 0.025,
		STTEquityDeliveryPct:     0.1,

		TxnOptionPct: 0.03503,
		TxnFuturePct: 0.00173,
		TxnEquityPct: 0.00297,

		StampOptionBuyPct:         0.003,
		StampFutureBuyPct:         0.002,
		StampEquityDeliveryBuyPct: 0.015,
		StampEquityIntradayBuyPct: 0.003,

		SEBIPct: 0.0001,
		GSTPct:  18.0,
	}
}

// ChargeBreakdown itemizes the cost of one executed order (one side, one leg).
type ChargeBreakdown struct {
	Turnover    float64 // price × quantity
	Brokerage   float64
	STT         float64
	ExchangeTxn float64
	SEBIFee     float64
	GST         float64
	StampDuty   float64
	Total       float64
}

// EstimateCharges is the single source of truth for Indian market fee math.
// Other packages (broker, backtest) must call this instead of maintaining
// their own (disagreeing) fee calculations.
//
// price is the per-unit traded price (option premium per unit for options);
// quantity is the total units (lots × lot size). Uses DefaultChargeRates
// (FY 2025-26).
func EstimateCharges(price float64, quantity int, tradeType TradeType, side OrderSide) ChargeBreakdown {
	return EstimateChargesWithRates(DefaultChargeRates(), price, quantity, tradeType, side)
}

// EstimateChargesWithRates computes the full charge breakdown using an
// explicit rate card (for tests, or a future config-driven override).
func EstimateChargesWithRates(rates ChargeRates, price float64, quantity int, tradeType TradeType, side OrderSide) ChargeBreakdown {
	t := normalizeTradeType(tradeType)
	turnover := price * float64(quantity)
	var b ChargeBreakdown
	b.Turnover = turnover

	// 1. Brokerage: flat per order for F&O; min(flat, pct of turnover) for equity.
	if IsDerivative(t) {
		b.Brokerage = rates.BrokerageFlatPerOrder
	} else {
		b.Brokerage = math.Min(rates.BrokerageFlatPerOrder, turnover*rates.BrokerageEquityPct/100.0)
	}

	// 2. STT
	switch {
	case isOption(t):
		if side == Sell {
			b.STT = turnover * rates.STTOptionSellPct / 100.0
		}
	case isFuture(t):
		if side == Sell {
			b.STT = turnover * rates.STTFutureSellPct / 100.0
		}
	case t == EquityIntraday:
		if side == Sell {
			b.STT = turnover * rates.STTEquityIntradaySellPct / 100.0
		}
	case t == EquityDelivery:
		b.STT = turnover * rates.STTEquityDeliveryPct / 100.0
	}

	// 3. Exchange transaction charges (both sides)
	switch {
	case isOption(t):
		b.ExchangeTxn = turnover * rates.TxnOptionPct / 100.0
	case isFuture(t):
		b.ExchangeTxn = turnover * rates.TxnFuturePct / 100.0
	default:
		b.ExchangeTxn = turnover * rates.TxnEquityPct / 100.0
	}

	// 4. SEBI turnover fee (both sides)
	b.SEBIFee = turnover * rates.SEBIPct / 100.0

	// 5. GST on brokerage + exchange txn + SEBI fee
	b.GST = (b.Brokerage + b.ExchangeTxn + b.SEBIFee) * rates.GSTPct / 100.0

	// 6. Stamp duty (buy side only)
	if side == Buy {
		switch {
		case isOption(t):
			b.StampDuty = turnover * rates.StampOptionBuyPct / 100.0
		case isFuture(t):
			b.StampDuty = turnover * rates.StampFutureBuyPct / 100.0
		case t == EquityDelivery:
			b.StampDuty = turnover * rates.StampEquityDeliveryBuyPct / 100.0
		default: // equity intraday
			b.StampDuty = turnover * rates.StampEquityIntradayBuyPct / 100.0
		}
	}

	b.Total = b.Brokerage + b.STT + b.ExchangeTxn + b.SEBIFee + b.GST + b.StampDuty
	return b
}

// ─────────────────────────────────────────────────────────────────────────
// Config structs
// ─────────────────────────────────────────────────────────────────────────

// StopLossConfig holds stop-loss configuration.
type StopLossConfig struct {
	Enabled          bool
	Type             string  // "percentage" or "points" (config.Load rejects anything else)
	Value            float64 // Loss threshold value
	Trailing         bool    // Enable trailing stop-loss
	TrailingDistance float64 // Trailing distance from peak
}

// BrokerageConfig holds legacy charge parameters as loaded from config.yaml.
//
// DEPRECATED for charge math: the YAML rate card conflated futures/options
// and carried wrong FY rates (audit EX-4). Kept only so existing config
// files/structs keep compiling and parsing; all charge calculations now go
// through EstimateCharges (DefaultChargeRates, FY 2025-26) and IGNORE this
// struct's values.
type BrokerageConfig struct {
	FlatFeePerOrder float64 `yaml:"flat_fee_per_order"`
	PercentageFee   float64 `yaml:"percentage_fee"`
	STT             struct {
		EquityDelivery float64 `yaml:"equity_delivery"`
		EquityIntraday float64 `yaml:"equity_intraday"`
		FNO            float64 `yaml:"fno"`
	} `yaml:"stt"`
	TransactionCharges struct {
		NSEEquity float64 `yaml:"nse_equity"`
		NSEFNO    float64 `yaml:"nse_fno"`
		BSEEquity float64 `yaml:"bse_equity"`
	} `yaml:"transaction_charges"`
	GSTPercent      float64 `yaml:"gst_percent"`
	SEBITurnoverFee float64 `yaml:"sebi_turnover_fee"`
	StampDuty       float64 `yaml:"stamp_duty"`
}

// ─────────────────────────────────────────────────────────────────────────
// Positions and Manager
// ─────────────────────────────────────────────────────────────────────────

// Position tracks an open position.
type Position struct {
	Symbol        string
	EntryPrice    float64
	Quantity      int
	Side          OrderSide
	TradeType     TradeType
	EntryCharges  float64
	EntryTime     int64   // unix seconds
	StopLossPrice float64 // Stop-loss trigger price
	PeakPrice     float64 // Highest price for trailing stop (Long) or lowest (Short)
	Margin        float64 // broker-required margin locked for SELL derivatives (0 = not margin-locked)
	LockedCapital float64 // exact capital locked against SessionBalanceUsed for this position
}

// RiskCheckResult is the typed result of CheckRisk.
type RiskCheckResult struct {
	Breached bool
	Reason   string
}

// Manager handles risk management including balance limits.
type Manager struct {
	mu sync.RWMutex

	MaxDrawdownPercent float64

	// KillSwitch is DEPRECATED as a directly-set field. It exists only so
	// legacy callers (cmd/main.go, internal/app/titan.go) that read/assign
	// it as a plain bool at startup keep compiling. KillSwitchActive()
	// honors both this field and the atomic runtime switch. New code must
	// use TriggerKillSwitch() / KillSwitchActive().
	KillSwitch bool

	killSwitch atomic.Bool // runtime kill switch set by TriggerKillSwitch

	InitialBalance     float64 // Starting balance for the session
	CurrentBalance     float64 // Current balance (Initial + realized P&L)
	SessionBalanceUsed float64 // Capital currently locked in positions
	RealizedPnL        float64 // Total realized profit/loss

	// BrokerageConfig is DEPRECATED and ignored for fee math (see type docs).
	BrokerageConfig BrokerageConfig
	StopLossConfig  StopLossConfig

	maxOrdersPerMin int
	orderTimes      []time.Time // sliding 60s window of entry-order timestamps

	now func() time.Time // injectable clock (tests); nil => time.Now

	OpenPositions map[string]*Position // open positions by symbol
}

// NewManager creates a new risk manager.
//
// brokerageConfig is accepted for backward compatibility but IGNORED for
// charge math — DefaultChargeRates (FY 2025-26) is authoritative.
func NewManager(maxDrawdown, sessionLimit float64, brokerageConfig BrokerageConfig, stopLossConfig StopLossConfig, maxOrdersPerMin int) *Manager {
	if maxOrdersPerMin <= 0 {
		// R3 fix: this used to default to 100, a DIFFERENT value than
		// config.Load's own 20 -- config.Load always sanitizes this before
		// NewManager is called via the normal cmd/main.go path, so this is
		// only a defensive backstop for direct construction (tests, or a
		// caller bypassing config.Load); it should match, not silently
		// diverge from, the documented default.
		maxOrdersPerMin = 20
	}
	return &Manager{
		MaxDrawdownPercent: maxDrawdown,
		InitialBalance:     sessionLimit,
		CurrentBalance:     sessionLimit,
		BrokerageConfig:    brokerageConfig,
		StopLossConfig:     stopLossConfig,
		maxOrdersPerMin:    maxOrdersPerMin,
		OpenPositions:      make(map[string]*Position),
		now:                time.Now,
	}
}

func (m *Manager) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// ─────────────────────────────────────────────────────────────────────────
// Kill switch (atomic, runtime-safe)
// ─────────────────────────────────────────────────────────────────────────

// TriggerKillSwitch activates the kill switch. One-way for the session: once
// triggered, all new order validation fails until process restart. Safe to
// call from any goroutine (API handler, sentinel-file watcher, tick loop).
func (m *Manager) TriggerKillSwitch() {
	m.killSwitch.Store(true)
	m.mu.Lock()
	m.KillSwitch = true // keep legacy field consistent for legacy readers
	m.mu.Unlock()
	log.Println("🛑 KILL SWITCH TRIGGERED — all new orders will be rejected")
}

// KillSwitchActive reports whether the kill switch is active, honoring both
// the runtime atomic switch and the legacy boot-time KillSwitch field.
func (m *Manager) KillSwitchActive() bool {
	if m.killSwitch.Load() {
		return true
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.KillSwitch
}

// killSwitchActiveLocked requires m.mu already held (read or write).
func (m *Manager) killSwitchActiveLocked() bool {
	return m.killSwitch.Load() || m.KillSwitch
}

// ─────────────────────────────────────────────────────────────────────────
// Order throttle (sliding 60-second window)
// ─────────────────────────────────────────────────────────────────────────

// pruneOrderTimesLocked drops window entries older than 1 minute. mu held (write).
func (m *Manager) pruneOrderTimesLocked(now time.Time) {
	cutoff := now.Add(-time.Minute)
	i := 0
	for i < len(m.orderTimes) && !m.orderTimes[i].After(cutoff) {
		i++
	}
	if i > 0 {
		m.orderTimes = append(m.orderTimes[:0], m.orderTimes[i:]...)
	}
}

// ordersInWindowLocked returns the entry-order count in the trailing 60s.
// mu held (read or write).
func (m *Manager) ordersInWindowLocked(now time.Time) int {
	cutoff := now.Add(-time.Minute)
	n := 0
	for _, t := range m.orderTimes {
		if t.After(cutoff) {
			n++
		}
	}
	return n
}

// SetMaxOrdersPerMin updates the per-minute entry-order limit at runtime.
// Values <= 0 are ignored (limit unchanged).
func (m *Manager) SetMaxOrdersPerMin(n int) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxOrdersPerMin = n
}

// GetMaxOrdersPerMin returns the current per-minute entry-order limit.
func (m *Manager) GetMaxOrdersPerMin() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.maxOrdersPerMin
}

// ResetOrderCount clears the throttle window.
//
// DEPRECATED: no longer required for correctness — the sliding window
// expires entries automatically. Retained (now correctly locked) for
// backward compatibility with callers that invoke it on a minute ticker.
func (m *Manager) ResetOrderCount() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.orderTimes = m.orderTimes[:0]
}

// ─────────────────────────────────────────────────────────────────────────
// Charges (Manager façade over EstimateCharges)
// ─────────────────────────────────────────────────────────────────────────

// CalculateTotalCharges computes all charges for a trade. Delegates to
// EstimateCharges (FY 2025-26 rate card); the YAML-loaded BrokerageConfig is
// intentionally ignored (audit EX-4). Rates are immutable after
// construction, so this is safe to call with or without m.mu held.
func (m *Manager) CalculateTotalCharges(price float64, quantity int, tradeType TradeType, side OrderSide) float64 {
	return EstimateCharges(price, quantity, tradeType, side).Total
}

// ─────────────────────────────────────────────────────────────────────────
// Validation
// ─────────────────────────────────────────────────────────────────────────

// ValidateOrder checks if an order can be placed within the session balance.
//
// WARNING (CR-13): for SELL derivative orders this legacy path validates
// premium×qty, which massively understates real short-option/future margin.
// It is retained so the existing paper flow keeps working. Live SELL
// derivative orders must go through ValidateOrderWithMargin using the broker
// margin API value (WP-9 wires it).
func (m *Manager) ValidateOrder(price float64, quantity int, tradeType TradeType, side OrderSide) (bool, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.validateLocked(price, quantity, tradeType, side, -1)
}

// ValidateOrderWithMargin validates an order using broker-supplied margin for
// SELL derivative orders (options/futures). requiredMargin must come from the
// broker margin API (Angel A-6); this package does not estimate SPAN.
//
// Fail-closed: if side is SELL on a derivative and requiredMargin <= 0
// (unknown / API failure / bogus), the order is REJECTED.
// BUY orders (and equity, any side) validate premium/turnover×qty + charges
// as before; requiredMargin is ignored for them.
func (m *Manager) ValidateOrderWithMargin(price float64, quantity int, tradeType TradeType, side OrderSide, requiredMargin float64) (bool, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if side == Sell && IsDerivative(tradeType) {
		if requiredMargin <= 0 {
			return false, fmt.Sprintf(
				"REJECTED (fail-closed): SELL %s requires broker margin; requiredMargin=%.2f is unknown or invalid",
				tradeType, requiredMargin)
		}
		return m.validateLocked(price, quantity, tradeType, side, requiredMargin)
	}
	return m.validateLocked(price, quantity, tradeType, side, -1)
}

// validateLocked performs the shared validation. mu held (read or write).
// requiredMargin >= 0 => margin-based capital requirement (SELL derivative);
// requiredMargin < 0  => turnover-based requirement.
func (m *Manager) validateLocked(price float64, quantity int, tradeType TradeType, side OrderSide, requiredMargin float64) (bool, string) {
	if m.killSwitchActiveLocked() {
		return false, "Kill Switch is ACTIVE"
	}

	if m.ordersInWindowLocked(m.clock()) >= m.maxOrdersPerMin {
		return false, fmt.Sprintf("Order rate limit exceeded (%d orders/min)", m.maxOrdersPerMin)
	}

	if price <= 0 || quantity <= 0 {
		return false, fmt.Sprintf("Invalid order: price=%.2f qty=%d", price, quantity)
	}

	charges := EstimateCharges(price, quantity, tradeType, side).Total

	var totalCost float64
	if requiredMargin >= 0 {
		totalCost = requiredMargin + charges
	} else {
		totalCost = price*float64(quantity) + charges
	}

	availableBalance := m.CurrentBalance - m.SessionBalanceUsed
	if totalCost > availableBalance {
		return false, fmt.Sprintf("Insufficient balance. Need: ₹%.2f, Available: ₹%.2f (Current: ₹%.2f, Locked: ₹%.2f)",
			totalCost, availableBalance, m.CurrentBalance, m.SessionBalanceUsed)
	}

	return true, ""
}

// ─────────────────────────────────────────────────────────────────────────
// Position lifecycle
// ─────────────────────────────────────────────────────────────────────────

// OpenPosition records opening a new position, locking turnover + charges.
//
// WARNING (CR-13): for SELL derivative entries this legacy path locks
// premium×qty + charges, NOT real margin. Retained for paper-flow
// compatibility; live SELL derivative entries must use
// OpenPositionWithMargin (WP-9 wires it).
func (m *Manager) OpenPosition(symbol string, price float64, quantity int, tradeType TradeType, side OrderSide) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.openPositionLocked(symbol, price, quantity, tradeType, side, 0)
}

// OpenPositionWithMargin records opening a position. For SELL derivative
// entries it locks requiredMargin + charges (the broker-quoted margin);
// fail-closed: requiredMargin <= 0 on a SELL derivative returns an error and
// records nothing. BUY/equity entries behave exactly like OpenPosition.
func (m *Manager) OpenPositionWithMargin(symbol string, price float64, quantity int, tradeType TradeType, side OrderSide, requiredMargin float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if side == Sell && IsDerivative(tradeType) {
		if requiredMargin <= 0 {
			return fmt.Errorf("fail-closed: SELL %s %s requires broker margin; got %.2f", symbol, tradeType, requiredMargin)
		}
		m.openPositionLocked(symbol, price, quantity, tradeType, side, requiredMargin)
		return nil
	}
	m.openPositionLocked(symbol, price, quantity, tradeType, side, 0)
	return nil
}

// openPositionLocked does the shared open. mu held (write).
// margin > 0 => lock margin + charges (SELL derivative); else turnover + charges.
func (m *Manager) openPositionLocked(symbol string, price float64, quantity int, tradeType TradeType, side OrderSide, margin float64) {
	charges := EstimateCharges(price, quantity, tradeType, side).Total

	var locked float64
	if margin > 0 {
		locked = margin + charges
	} else {
		locked = price*float64(quantity) + charges
	}

	now := m.clock()
	m.SessionBalanceUsed += locked
	m.pruneOrderTimesLocked(now)
	m.orderTimes = append(m.orderTimes, now)

	var stopLossPrice float64
	if m.StopLossConfig.Enabled {
		stopLossPrice = m.calculateStopLossPrice(price, side)
	}

	m.OpenPositions[symbol] = &Position{
		Symbol:        symbol,
		EntryPrice:    price,
		Quantity:      quantity,
		Side:          side,
		TradeType:     tradeType,
		EntryCharges:  charges,
		EntryTime:     now.Unix(),
		StopLossPrice: stopLossPrice,
		PeakPrice:     price,
		Margin:        margin,
		LockedCapital: locked,
	}

	log.Printf("Position OPENED - %s %s %d @ ₹%.2f | Margin: ₹%.2f | Charges: ₹%.2f | Locked: ₹%.2f | Available: ₹%.2f/₹%.2f",
		side, symbol, quantity, price, margin, charges, m.SessionBalanceUsed,
		m.CurrentBalance-m.SessionBalanceUsed, m.CurrentBalance)
}

// RestorePosition reinstates a position recovered from durable state after a
// restart (see engine.Runner.RestoreState). Previously this was a raw,
// unlocked map write directly from the caller -- a real data race against
// any concurrent GetOpenPositions/status call during recovery (the API
// server goroutine is already running by the time RestoreState runs).
//
// EntryCharges/LockedCapital/StopLossPrice are recomputed the same way
// openPositionLocked computes them for a fresh entry, because state.Position
// (the durable record) doesn't persist those derived fields. For a
// margin-locked SELL derivative this is an approximation -- the real
// broker-quoted margin from entry time isn't persisted either.
// ponytail: known ceiling; exact restoration needs models.Position to carry
// Margin/EntryCharges/LockedCapital, which is a bigger, separate change.
// Call RestoreSnapshot after all RestorePosition calls -- its SessionBalanceUsed
// is the authoritative aggregate and overwrites whatever this approximated.
func (m *Manager) RestorePosition(symbol string, entryPrice float64, quantity int, tradeType TradeType, side OrderSide, entryTimeUnix int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	charges := EstimateCharges(entryPrice, quantity, tradeType, side).Total
	locked := entryPrice*float64(quantity) + charges

	var stopLossPrice float64
	if m.StopLossConfig.Enabled {
		stopLossPrice = m.calculateStopLossPrice(entryPrice, side)
	}

	m.OpenPositions[symbol] = &Position{
		Symbol: symbol, EntryPrice: entryPrice, Quantity: quantity, Side: side,
		TradeType: tradeType, EntryCharges: charges, EntryTime: entryTimeUnix,
		StopLossPrice: stopLossPrice, PeakPrice: entryPrice, LockedCapital: locked,
	}
	log.Printf("♻️  restored risk position %s: %s %d @ ₹%.2f (charges/locked-capital recomputed, not persisted)",
		symbol, side, quantity, entryPrice)
}

// RestoreSnapshot reinstates the aggregate balance/realized-PnL/session-used
// figures from the last saved risk snapshot after a restart. Authoritative
// over any per-position LockedCapital RestorePosition approximated -- call
// this AFTER all RestorePosition calls for the session.
func (m *Manager) RestoreSnapshot(balance, realizedPnL, sessionUsed float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CurrentBalance = balance
	m.RealizedPnL = realizedPnL
	m.SessionBalanceUsed = sessionUsed
}

// RollbackPosition reverses a pending position when the broker order fails.
// Releases exactly the capital that was locked and un-counts the entry
// against the throttle window.
func (m *Manager) RollbackPosition(symbol string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	position, exists := m.OpenPositions[symbol]
	if !exists {
		return fmt.Errorf("no position to rollback for %s", symbol)
	}

	m.SessionBalanceUsed -= position.LockedCapital
	if n := len(m.orderTimes); n > 0 {
		m.orderTimes = m.orderTimes[:n-1] // failed order shouldn't count against throttle
	}
	delete(m.OpenPositions, symbol)

	log.Printf("⚠️ Position ROLLED BACK - %s %s %d @ ₹%.2f | Released: ₹%.2f | Available: ₹%.2f/₹%.2f",
		position.Side, symbol, position.Quantity, position.EntryPrice, position.LockedCapital,
		m.CurrentBalance-m.SessionBalanceUsed, m.CurrentBalance)

	return nil
}

// UpdatePositionPrice updates the entry price AND quantity after the broker
// confirms the actual fill. EntryCharges are RECOMPUTED at the corrected
// fill price/quantity (EX-9 fix — they previously stayed frozen at the
// estimated price) and the locked capital is adjusted to match. The margin
// component (if any) is left unchanged — it came from the broker margin
// API, not the premium.
//
// actualFillQuantity corrects a real bug (audit R3): OpenPosition records
// the REQUESTED quantity before the order is placed, and nothing previously
// ever corrected it after a partial fill. PlaceExitOrder then closed using
// the wrong (too-large) quantity, which on a partial fill would try to exit
// more than was actually bought — risking an unintended opposite-side
// position rather than a clean flatten. Pass filled.Quantity here, not the
// originally requested quantity.
func (m *Manager) UpdatePositionPrice(symbol string, actualFillPrice float64, actualFillQuantity int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	position, exists := m.OpenPositions[symbol]
	if !exists {
		log.Printf("⚠️ Cannot update price for %s: position not found", symbol)
		return
	}
	if actualFillPrice <= 0 {
		log.Printf("⚠️ Ignoring invalid fill price %.2f for %s", actualFillPrice, symbol)
		return
	}

	oldPrice := position.EntryPrice
	position.EntryPrice = actualFillPrice
	position.PeakPrice = actualFillPrice

	if actualFillQuantity > 0 && actualFillQuantity != position.Quantity {
		log.Printf("⚠️ Partial fill for %s: requested %d, broker filled %d — correcting risk-tracked quantity so exit closes the right size",
			symbol, position.Quantity, actualFillQuantity)
		position.Quantity = actualFillQuantity
	}

	if m.StopLossConfig.Enabled {
		position.StopLossPrice = m.calculateStopLossPrice(actualFillPrice, position.Side)
	}

	newCharges := EstimateCharges(actualFillPrice, position.Quantity, position.TradeType, position.Side).Total

	var newLocked float64
	if position.Margin > 0 {
		newLocked = position.Margin + newCharges
	} else {
		newLocked = actualFillPrice*float64(position.Quantity) + newCharges
	}

	m.SessionBalanceUsed += newLocked - position.LockedCapital
	position.EntryCharges = newCharges
	position.LockedCapital = newLocked

	if actualFillPrice != oldPrice {
		log.Printf("📊 Position price updated [%s]: ₹%.2f → ₹%.2f | Charges: ₹%.2f | New Stop-Loss: ₹%.2f",
			symbol, oldPrice, actualFillPrice, newCharges, position.StopLossPrice)
	}
}

// ClosePosition closes an existing position and realizes P&L. Releases
// exactly the capital that was locked at entry (LockedCapital), never a
// recomputed turnover+charges figure — this keeps margin-locked SELL
// entries correct (EX-9 locked-capital fix).
func (m *Manager) ClosePosition(symbol string, exitPrice float64) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	position, exists := m.OpenPositions[symbol]
	if !exists {
		return 0, fmt.Errorf("no open position found for %s", symbol)
	}

	exitCharges := EstimateCharges(exitPrice, position.Quantity, position.TradeType,
		oppositeOrderSide(position.Side)).Total

	var pnl float64
	if position.Side == Buy {
		pnl = (exitPrice - position.EntryPrice) * float64(position.Quantity)
	} else {
		pnl = (position.EntryPrice - exitPrice) * float64(position.Quantity)
	}

	totalCharges := position.EntryCharges + exitCharges
	netPnL := pnl - totalCharges

	m.SessionBalanceUsed -= position.LockedCapital
	m.CurrentBalance += netPnL
	m.RealizedPnL += netPnL

	delete(m.OpenPositions, symbol)

	log.Printf("Position CLOSED - %s | Entry: ₹%.2f, Exit: ₹%.2f | Gross P&L: ₹%.2f | Charges: ₹%.2f | Net P&L: ₹%.2f",
		symbol, position.EntryPrice, exitPrice, pnl, totalCharges, netPnL)
	log.Printf("Balance Update - Current: ₹%.2f (Initial: ₹%.2f, Realized P&L: ₹%.2f) | Available: ₹%.2f",
		m.CurrentBalance, m.InitialBalance, m.RealizedPnL, m.CurrentBalance-m.SessionBalanceUsed)

	return netPnL, nil
}

// oppositeOrderSide returns the opposite side for closing positions.
func oppositeOrderSide(side OrderSide) OrderSide {
	if side == Buy {
		return Sell
	}
	return Buy
}

// ─────────────────────────────────────────────────────────────────────────
// Getters (all mutex-guarded — EX-9 race fix)
// ─────────────────────────────────────────────────────────────────────────

// GetRemainingBalance returns available session balance.
func (m *Manager) GetRemainingBalance() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.CurrentBalance - m.SessionBalanceUsed
}

// GetCurrentBalance returns the current balance including realized P&L.
func (m *Manager) GetCurrentBalance() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.CurrentBalance
}

// GetRealizedPnL returns total realized profit/loss.
func (m *Manager) GetRealizedPnL() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.RealizedPnL
}

// GetStopLossConfig returns a copy of the stop-loss configuration.
func (m *Manager) GetStopLossConfig() StopLossConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.StopLossConfig
}

// GetOpenPositions returns a copy of open positions.
func (m *Manager) GetOpenPositions() map[string]*Position {
	m.mu.RLock()
	defer m.mu.RUnlock()

	positions := make(map[string]*Position, len(m.OpenPositions))
	for k, v := range m.OpenPositions {
		cp := *v
		positions[k] = &cp
	}
	return positions
}

// unrealizedPnLLocked computes unrealized P&L. mu held (read or write).
// priceGetter must NOT call back into this Manager (would deadlock).
func (m *Manager) unrealizedPnLLocked(priceGetter func(symbol string) float64) float64 {
	var unrealizedPnL float64
	for symbol, pos := range m.OpenPositions {
		currentPrice := priceGetter(symbol)
		if currentPrice <= 0 {
			continue
		}
		if pos.Side == Buy {
			unrealizedPnL += (currentPrice - pos.EntryPrice) * float64(pos.Quantity)
		} else {
			unrealizedPnL += (pos.EntryPrice - currentPrice) * float64(pos.Quantity)
		}
	}
	return unrealizedPnL
}

// GetUnrealizedPnL calculates unrealized P&L for all open positions.
// priceGetter returns the current price for a symbol and must not call back
// into this Manager.
func (m *Manager) GetUnrealizedPnL(priceGetter func(symbol string) float64) float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.unrealizedPnLLocked(priceGetter)
}

// GetTotalPnL returns realized + unrealized P&L.
func (m *Manager) GetTotalPnL(priceGetter func(symbol string) float64) float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.RealizedPnL + m.unrealizedPnLLocked(priceGetter)
}

// GetSessionStats returns session statistics.
func (m *Manager) GetSessionStats() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	pnlPct := 0.0
	if m.InitialBalance != 0 {
		pnlPct = (m.RealizedPnL / m.InitialBalance) * 100
	}
	return map[string]interface{}{
		"initial_balance": m.InitialBalance,
		"current_balance": m.CurrentBalance,
		"locked_capital":  m.SessionBalanceUsed,
		"available":       m.CurrentBalance - m.SessionBalanceUsed,
		"realized_pnl":    m.RealizedPnL,
		"pnl_percentage":  pnlPct,
		"open_positions":  len(m.OpenPositions),
	}
}

// GetSessionStatsWithPrices returns session statistics including unrealized
// P&L. All fields are read under a single lock acquisition (EX-9 race fix —
// previously GetUnrealizedPnL locked/unlocked separately from the rest).
func (m *Manager) GetSessionStatsWithPrices(priceGetter func(symbol string) float64) map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	unrealizedPnL := m.unrealizedPnLLocked(priceGetter)
	totalPnL := m.RealizedPnL + unrealizedPnL
	pnlPct := 0.0
	if m.InitialBalance != 0 {
		pnlPct = (totalPnL / m.InitialBalance) * 100
	}

	return map[string]interface{}{
		"initial_balance": m.InitialBalance,
		"current_balance": m.CurrentBalance,
		"locked_capital":  m.SessionBalanceUsed,
		"available":       m.CurrentBalance - m.SessionBalanceUsed,
		"realized_pnl":    m.RealizedPnL,
		"unrealized_pnl":  unrealizedPnL,
		"total_pnl":       totalPnL,
		"pnl_percentage":  pnlPct,
		"open_positions":  len(m.OpenPositions),
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Risk check (cheap, side-effect-free, call every tick)
// ─────────────────────────────────────────────────────────────────────────

// CheckRisk performs the per-tick risk check. It is cheap (one RLock, a few
// float comparisons), has NO side effects (no logging, no state mutation),
// and is safe to call every tick from the engine loop (CR-2). The caller
// (WP-9) decides what to do on breach: halt entries, flatten, alert.
func (m *Manager) CheckRisk() RiskCheckResult {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.killSwitchActiveLocked() {
		return RiskCheckResult{Breached: true, Reason: "kill switch active"}
	}

	if m.CurrentBalance <= 0 {
		return RiskCheckResult{Breached: true,
			Reason: fmt.Sprintf("session balance depleted (₹%.2f)", m.CurrentBalance)}
	}

	if m.InitialBalance > 0 {
		drawdownPercent := ((m.InitialBalance - m.CurrentBalance) / m.InitialBalance) * 100
		if drawdownPercent >= m.MaxDrawdownPercent {
			return RiskCheckResult{Breached: true,
				Reason: fmt.Sprintf("max drawdown reached (%.2f%% >= %.2f%%)", drawdownPercent, m.MaxDrawdownPercent)}
		}
	}

	return RiskCheckResult{}
}

// ─────────────────────────────────────────────────────────────────────────
// Stop-loss
// ─────────────────────────────────────────────────────────────────────────

// calculateStopLossPrice calculates the stop-loss trigger price. mu held.
func (m *Manager) calculateStopLossPrice(entryPrice float64, side OrderSide) float64 {
	var stopPrice float64

	switch m.StopLossConfig.Type {
	case "percentage":
		lossPercent := m.StopLossConfig.Value / 100.0
		if side == Buy {
			stopPrice = entryPrice * (1.0 - lossPercent)
		} else {
			stopPrice = entryPrice * (1.0 + lossPercent)
		}
	case "points":
		if side == Buy {
			stopPrice = entryPrice - m.StopLossConfig.Value
		} else {
			stopPrice = entryPrice + m.StopLossConfig.Value
		}
	default: // unreachable via config.Load (fail-closed there); defensive only for direct Manager construction (e.g. tests)
		if side == Buy {
			stopPrice = entryPrice * 0.95
		} else {
			stopPrice = entryPrice * 1.05
		}
	}

	return stopPrice
}

// ShouldTriggerStopLoss checks if a position's stop-loss should trigger.
// Always takes the write lock: trailing mode mutates position state, and the
// trailing flag itself must be read under the lock (EX-9 race fix — it was
// previously read before acquiring the appropriate lock).
func (m *Manager) ShouldTriggerStopLoss(symbol string, currentPrice float64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.StopLossConfig.Enabled {
		return false
	}

	position, exists := m.OpenPositions[symbol]
	if !exists {
		return false
	}

	if m.StopLossConfig.Trailing {
		m.updateTrailingStop(position, currentPrice)
	}

	if position.Side == Buy {
		return currentPrice <= position.StopLossPrice
	}
	return currentPrice >= position.StopLossPrice
}

// updateTrailingStop updates the stop-loss for trailing stops. mu held (write).
func (m *Manager) updateTrailingStop(position *Position, currentPrice float64) {
	trailPercent := m.StopLossConfig.TrailingDistance / 100.0

	if position.Side == Buy {
		if currentPrice > position.PeakPrice {
			position.PeakPrice = currentPrice
			newStop := currentPrice * (1.0 - trailPercent)
			if newStop > position.StopLossPrice {
				position.StopLossPrice = newStop
			}
		}
	} else {
		if currentPrice < position.PeakPrice {
			position.PeakPrice = currentPrice
			newStop := currentPrice * (1.0 + trailPercent)
			if newStop < position.StopLossPrice {
				position.StopLossPrice = newStop
			}
		}
	}
}
