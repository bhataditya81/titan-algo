// Package engine wires the risk manager, broker and durable record-keeping
// (state store + ledger) into one order-flow, and drives the live tick loop
// (Runner, in runner.go) consumed by both cmd/main.go and internal/app
// (mobile). This file (WP-9) owns TradingEngine: the order-placement
// primitives every entry/exit goes through.
package engine

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"titan-algo/internal/broker"
	"titan-algo/internal/ledger"
	"titan-algo/internal/logger"
	"titan-algo/internal/risk"
	"titan-algo/internal/state"
	"titan-algo/models"
)

// TradingEngine orchestrates risk management, broker execution and durable
// record-keeping for one trading session. store/ledgerDB may be nil (state
// persistence is optional for quick/manual runs, but Runner always tries to
// wire it — see cmd/main.go).
type TradingEngine struct {
	riskManager *risk.Manager
	broker      broker.TradeService
	csvLogger   *logger.CSVLogger
	store       *state.Store
	ledger      *ledger.Ledger
	mode        ledger.Mode

	// exitMu/exitInFlight guard PlaceExitOrder against a real race (audit
	// R3): the tick loop's software stop-loss and the /api/kill handler run
	// on different goroutines and can both call PlaceExitOrder for the same
	// symbol. Between the "position still exists" check and the risk
	// manager actually closing it lies a real network call to the broker
	// (e.broker.PlaceOrder) — with no guard, both callers could pass the
	// exists-check before either one closes the position, placing two live
	// exit orders at the broker for the same position.
	exitMu       sync.Mutex
	exitInFlight map[string]bool
}

// NewTradingEngine creates a new trading engine. store and ledgerDB may be
// nil (persistence disabled); mode is "paper" or "live" (ledger.ModePaper /
// ledger.ModeLive) and is stamped on every ledger row.
func NewTradingEngine(
	riskMgr *risk.Manager,
	brokerService broker.TradeService,
	csvLog *logger.CSVLogger,
	store *state.Store,
	ledgerDB *ledger.Ledger,
	mode ledger.Mode,
) *TradingEngine {
	if mode == "" {
		mode = ledger.ModePaper
	}
	return &TradingEngine{
		riskManager:  riskMgr,
		broker:       brokerService,
		csvLogger:    csvLog,
		store:        store,
		ledger:       ledgerDB,
		mode:         mode,
		exitInFlight: make(map[string]bool),
	}
}

// GetRiskManager returns the risk manager instance.
func (e *TradingEngine) GetRiskManager() *risk.Manager { return e.riskManager }

// GetBroker returns the broker instance.
func (e *TradingEngine) GetBroker() broker.TradeService { return e.broker }

// GetStore returns the state store (may be nil).
func (e *TradingEngine) GetStore() *state.Store { return e.store }

// GetLedger returns the trade ledger (may be nil).
func (e *TradingEngine) GetLedger() *ledger.Ledger { return e.ledger }

// BrokerHealthy reports whether the broker session is usable. Brokers that
// don't implement ExtendedTradeService (MockBroker, LivePaperBroker) have no
// health concept and are always reported healthy.
func (e *TradingEngine) BrokerHealthy() (bool, error) {
	if ext, ok := e.broker.(broker.ExtendedTradeService); ok {
		if !ext.Healthy() {
			return false, ext.HealthError()
		}
	}
	return true, nil
}

func (e *TradingEngine) ledgerAppend(t ledger.Trade) {
	if e.ledger == nil {
		return
	}
	t.Mode = e.mode
	if err := e.ledger.Append(t); err != nil {
		log.Printf("⚠️ ledger append failed (clientOrderID=%s symbol=%s): %v", t.ClientOrderID, t.Symbol, err)
	}
}

// EntryFill is the result of a successful PlaceEntryOrder call. PositionID
// (when a state store is wired) is the durable position record's primary
// key — callers MUST hold onto it to later close the same record via
// PlaceExitOrder, since the store has no "find open position by symbol"
// lookup of its own (a symbol can be opened/closed/reopened many times).
type EntryFill struct {
	Filled        *broker.FilledOrder
	ClientOrderID string
	PositionID    string
}

// PlaceEntryOrder places an ENTRY order (opens/increases risk) through the
// full hardened flow (WP-9 task 5, issue 2/4 from the plan):
//   - state-store "intent" persisted BEFORE the network call (CR-7)
//   - margin-aware validation for derivative SELLs when requiredMargin > 0;
//     fail-closed (CR-13) when requiredMargin <= 0 on a derivative SELL
//   - broker.Order.Intent = IntentEntry (subject to the circuit breaker)
//   - on broker.ErrOrderIndeterminate: risk state is NOT rolled back (the
//     fill may have happened at the broker); the attempt is persisted for
//     reconciliation and the error is returned so the caller can alert
//   - on success: risk position opened/priced, ledger + CSV + state position
//     recorded
func (e *TradingEngine) PlaceEntryOrder(symbol string, quantity int, side broker.OrderSide, tradeType risk.TradeType, strategyName string, requiredMargin float64) (*EntryFill, error) {
	if quantity <= 0 {
		return nil, fmt.Errorf("invalid quantity %d for %s", quantity, symbol)
	}
	price := e.broker.GetCurrentPrice(symbol)
	if price <= 0 {
		return nil, fmt.Errorf("unable to get price for %s", symbol)
	}

	useMargin := side == broker.Sell && risk.IsDerivative(tradeType)
	var valid bool
	var reason string
	if useMargin {
		valid, reason = e.riskManager.ValidateOrderWithMargin(price, quantity, tradeType, convertToRiskSide(side), requiredMargin)
	} else {
		valid, reason = e.riskManager.ValidateOrder(price, quantity, tradeType, convertToRiskSide(side))
	}
	if !valid {
		log.Printf("❌ Entry REJECTED by risk manager [%s]: %s", symbol, reason)
		return nil, fmt.Errorf("risk validation failed: %s", reason)
	}

	var clientOrderID, positionID string
	if e.store != nil {
		clientOrderID = e.store.NextClientOrderID()
		positionID = e.store.NextPositionID()
		if err := e.store.SaveOrderAttempt(models.OrderAttempt{
			ClientOrderID: clientOrderID, Symbol: symbol, Side: models.PositionSide(side),
			Quantity: quantity, Price: price, OrderType: "MARKET", Strategy: strategyName,
			Status: models.OrderIntent,
		}); err != nil {
			log.Printf("⚠️ state.SaveOrderAttempt(intent) failed for %s: %v", symbol, err)
		}
	}
	e.ledgerAppend(ledger.Trade{ClientOrderID: clientOrderID, Symbol: symbol, Side: string(side),
		RequestedQuantity: quantity, Price: price, Status: ledger.StatusIntent, Strategy: strategyName})

	if useMargin {
		if err := e.riskManager.OpenPositionWithMargin(symbol, price, quantity, tradeType, convertToRiskSide(side), requiredMargin); err != nil {
			log.Printf("❌ Margin lock failed [%s]: %v", symbol, err)
			return nil, err
		}
	} else {
		e.riskManager.OpenPosition(symbol, price, quantity, tradeType, convertToRiskSide(side))
	}

	order := broker.Order{Symbol: symbol, Quantity: quantity, Side: side, OrderType: broker.Market, Intent: broker.IntentEntry}
	filled, err := e.broker.PlaceOrder(order)
	if err != nil {
		if errors.Is(err, broker.ErrOrderIndeterminate) {
			// CR-7 / plan: do NOT roll back risk state on an indeterminate
			// order — the fill may have happened at the broker. Persist for
			// reconciliation and let the caller alert loudly.
			log.Printf("🟡 CRITICAL: entry order for %s is INDETERMINATE: %v — NOT rolling back risk state, marked for reconciliation", symbol, err)
			if e.store != nil && clientOrderID != "" {
				_ = e.store.MarkOrderResolved(clientOrderID, models.OrderIndeterminate, "", 0, 0, err.Error(), time.Time{})
			}
			e.ledgerAppend(ledger.Trade{ClientOrderID: clientOrderID, Symbol: symbol, Side: string(side),
				RequestedQuantity: quantity, Price: price, Status: ledger.StatusIndeterminate, Strategy: strategyName,
				Note: err.Error()})
			return nil, err
		}
		log.Printf("❌ Entry order failed [%s]: %v — rolling back risk state", symbol, err)
		if rbErr := e.riskManager.RollbackPosition(symbol); rbErr != nil {
			log.Printf("⚠️ rollback also failed: %v", rbErr)
		}
		if e.store != nil && clientOrderID != "" {
			_ = e.store.MarkOrderResolved(clientOrderID, models.OrderRejected, "", 0, 0, err.Error(), time.Time{})
		}
		e.ledgerAppend(ledger.Trade{ClientOrderID: clientOrderID, Symbol: symbol, Side: string(side),
			RequestedQuantity: quantity, Price: price, Status: ledger.StatusRejected, Strategy: strategyName,
			Note: err.Error()})
		return nil, err
	}

	// Success (full or partial fill) — CR-6/R3: correct the risk-tracked
	// position to the ACTUAL filled price AND quantity, so a later exit
	// closes the right size instead of the originally requested one.
	e.riskManager.UpdatePositionPrice(symbol, filled.FillPrice, filled.Quantity)

	status := models.OrderFilled
	ledgerStatus := ledger.StatusFilled
	if filled.Partial() {
		status = models.OrderPartial
		ledgerStatus = ledger.StatusPartial
	}
	if e.store != nil {
		if clientOrderID != "" {
			_ = e.store.MarkOrderResolved(clientOrderID, status, filled.OrderID, filled.Quantity, filled.FillPrice, "", time.Time{})
		}
		if err := e.store.SavePosition(models.Position{
			ID: positionID, Symbol: symbol, Exchange: "NFO", Side: models.PositionSide(side),
			Quantity: filled.Quantity, EntryPrice: filled.FillPrice, Strategy: strategyName,
			Status: models.PositionOpen, BrokerOrderID: filled.OrderID,
		}); err != nil {
			log.Printf("⚠️ state.SavePosition failed for %s: %v", symbol, err)
		}
		_ = e.store.SaveRiskSnapshot(e.riskManager.GetCurrentBalance(), e.riskManager.GetRealizedPnL(), e.riskManager.GetCurrentBalance()-e.riskManager.GetRemainingBalance())
	}
	e.ledgerAppend(ledger.Trade{ClientOrderID: clientOrderID, BrokerOrderID: filled.OrderID, Symbol: symbol,
		Side: string(side), Quantity: filled.Quantity, RequestedQuantity: quantity, Price: filled.FillPrice,
		Status: ledgerStatus, Strategy: strategyName})

	if e.csvLogger != nil {
		if err := e.csvLogger.LogTradeWithStatus(filled, e.broker.GetBalance(), e.riskManager.GetCurrentBalance(), e.riskManager.GetRealizedPnL(), string(ledgerStatus)); err != nil {
			log.Printf("⚠️ csv log failed: %v", err)
		}
	}

	log.Printf("✅ Entry FILLED [%s]: %s %d/%d @ ₹%.2f (OrderID=%s)", symbol, side, filled.Quantity, quantity, filled.FillPrice, filled.OrderID)
	return &EntryFill{Filled: filled, ClientOrderID: clientOrderID, PositionID: positionID}, nil
}

// PlaceExitOrder places an EXIT (reduce-only) order for an existing risk
// position. positionID, if known (from the matching EntryFill), is used to
// mark the durable state-store position record CLOSED; pass "" when the
// caller doesn't have it (e.g. an orphan discovered by flattenAll) — the
// order still executes, only the state-store bookkeeping is skipped.
//
// EX-1 fix: a market order needs no price. A fresh/last-known price is used
// only for logging and slippage estimation — its absence is NEVER a
// precondition for placing the closing order (the one time you must exit,
// e.g. a feed outage, is exactly when a price precondition would block you).
//
// R3 fix: only one exit for a given symbol may be in flight at a time (see
// exitInFlight's doc comment on TradingEngine) — a concurrent second call
// (e.g. the software stop-loss loop and a manual /api/kill racing) is
// refused immediately rather than silently placing a second live order.
func (e *TradingEngine) PlaceExitOrder(symbol string, strategyName string, positionID string) (*broker.FilledOrder, float64, error) {
	e.exitMu.Lock()
	if e.exitInFlight[symbol] {
		e.exitMu.Unlock()
		return nil, 0, fmt.Errorf("exit already in flight for %s — refusing a concurrent duplicate", symbol)
	}
	e.exitInFlight[symbol] = true
	e.exitMu.Unlock()
	defer func() {
		e.exitMu.Lock()
		delete(e.exitInFlight, symbol)
		e.exitMu.Unlock()
	}()

	positions := e.riskManager.GetOpenPositions()
	position, exists := positions[symbol]
	if !exists {
		return nil, 0, fmt.Errorf("no open position for %s", symbol)
	}

	lastPrice := e.broker.GetCurrentPrice(symbol)
	if lastPrice <= 0 {
		log.Printf("⚠️ %s: no fresh price available — closing at MARKET anyway (EX-1: a price is never a precondition for an exit)", symbol)
	}

	closeSide := broker.Buy
	if position.Side == risk.Buy {
		closeSide = broker.Sell
	}

	var clientOrderID string
	if e.store != nil {
		clientOrderID = e.store.NextClientOrderID()
		if err := e.store.SaveOrderAttempt(models.OrderAttempt{
			ClientOrderID: clientOrderID, Symbol: symbol, Side: models.PositionSide(closeSide),
			Quantity: position.Quantity, Price: lastPrice, OrderType: "MARKET", Strategy: strategyName,
			Status: models.OrderIntent,
		}); err != nil {
			log.Printf("⚠️ state.SaveOrderAttempt(intent) failed for exit %s: %v", symbol, err)
		}
	}
	e.ledgerAppend(ledger.Trade{ClientOrderID: clientOrderID, Symbol: symbol, Side: string(closeSide),
		RequestedQuantity: position.Quantity, Price: lastPrice, Status: ledger.StatusIntent, Strategy: strategyName, Note: "exit"})

	order := broker.Order{Symbol: symbol, Quantity: position.Quantity, Side: closeSide, OrderType: broker.Market, Intent: broker.IntentReduceOnly}
	filled, err := e.broker.PlaceOrder(order)
	if err != nil {
		if errors.Is(err, broker.ErrOrderIndeterminate) {
			log.Printf("🟡 CRITICAL: exit order for %s is INDETERMINATE: %v — the position may or may not be closed at the broker; NOT clearing internal state, marked for reconciliation", symbol, err)
			if e.store != nil && clientOrderID != "" {
				_ = e.store.MarkOrderResolved(clientOrderID, models.OrderIndeterminate, "", 0, 0, err.Error(), time.Time{})
			}
			e.ledgerAppend(ledger.Trade{ClientOrderID: clientOrderID, Symbol: symbol, Side: string(closeSide),
				RequestedQuantity: position.Quantity, Price: lastPrice, Status: ledger.StatusIndeterminate, Strategy: strategyName, Note: err.Error()})
			return nil, 0, err
		}
		log.Printf("❌ Exit order failed [%s]: %v", symbol, err)
		if e.store != nil && clientOrderID != "" {
			_ = e.store.MarkOrderResolved(clientOrderID, models.OrderRejected, "", 0, 0, err.Error(), time.Time{})
		}
		e.ledgerAppend(ledger.Trade{ClientOrderID: clientOrderID, Symbol: symbol, Side: string(closeSide),
			RequestedQuantity: position.Quantity, Price: lastPrice, Status: ledger.StatusRejected, Strategy: strategyName, Note: err.Error()})
		return nil, 0, fmt.Errorf("failed to close position: %w", err)
	}

	netPnL, closeErr := e.riskManager.ClosePosition(symbol, filled.FillPrice)
	if closeErr != nil {
		// The broker has already closed the position; a risk-manager-side
		// bookkeeping error must not be treated as "the exit failed" (that
		// would make a caller retry a close that already happened).
		log.Printf("⚠️ risk manager close failed for %s (broker already closed it!): %v", symbol, closeErr)
	}

	if e.store != nil {
		if clientOrderID != "" {
			_ = e.store.MarkOrderResolved(clientOrderID, models.OrderFilled, filled.OrderID, filled.Quantity, filled.FillPrice, "", time.Time{})
		}
		if positionID != "" {
			if err := e.store.ClosePosition(positionID, filled.FillPrice, time.Time{}); err != nil {
				log.Printf("⚠️ state.ClosePosition(%s) failed for %s: %v", positionID, symbol, err)
			}
		}
		_ = e.store.SaveRiskSnapshot(e.riskManager.GetCurrentBalance(), e.riskManager.GetRealizedPnL(), e.riskManager.GetCurrentBalance()-e.riskManager.GetRemainingBalance())
	}
	e.ledgerAppend(ledger.Trade{ClientOrderID: clientOrderID, BrokerOrderID: filled.OrderID, Symbol: symbol,
		Side: string(closeSide), Quantity: filled.Quantity, RequestedQuantity: position.Quantity, Price: filled.FillPrice,
		Status: ledger.StatusFilled, RealizedPnL: netPnL, Strategy: strategyName, Note: "exit"})

	if e.csvLogger != nil {
		if err := e.csvLogger.LogTradeWithStatus(filled, e.broker.GetBalance(), e.riskManager.GetCurrentBalance(), netPnL, "filled"); err != nil {
			log.Printf("⚠️ csv log failed: %v", err)
		}
	}

	log.Printf("✅ Exit FILLED [%s]: %s %d @ ₹%.2f | Net P&L: ₹%.2f", symbol, closeSide, filled.Quantity, filled.FillPrice, netPnL)
	return filled, netPnL, nil
}

// GetSessionStats returns comprehensive session statistics including
// unrealized P&L.
func (e *TradingEngine) GetSessionStats() map[string]interface{} {
	priceGetter := func(symbol string) float64 {
		return e.broker.GetCurrentPrice(symbol)
	}
	riskStats := e.riskManager.GetSessionStatsWithPrices(priceGetter)

	stats := make(map[string]interface{})
	stats["risk_balance"] = riskStats["current_balance"]
	stats["broker_balance"] = e.broker.GetBalance()
	stats["realized_pnl"] = riskStats["realized_pnl"]
	stats["unrealized_pnl"] = riskStats["unrealized_pnl"]
	stats["total_pnl"] = riskStats["total_pnl"]
	stats["pnl_percentage"] = riskStats["pnl_percentage"]
	stats["open_positions"] = riskStats["open_positions"]
	stats["available_balance"] = riskStats["available"]

	return stats
}

// convertToRiskSide converts broker.OrderSide to risk.OrderSide.
func convertToRiskSide(side broker.OrderSide) risk.OrderSide {
	if side == broker.Buy {
		return risk.Buy
	}
	return risk.Sell
}
