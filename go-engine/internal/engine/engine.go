package engine

import (
	"fmt"
	"log"

	"titan-algo/internal/broker"
	"titan-algo/internal/logger"
	"titan-algo/internal/risk"
)

// TradingEngine orchestrates risk management and broker execution
type TradingEngine struct {
	riskManager *risk.Manager
	broker      broker.TradeService
	csvLogger   *logger.CSVLogger
}

// NewTradingEngine creates a new trading engine
func NewTradingEngine(
	riskMgr *risk.Manager,
	brokerService broker.TradeService,
	csvLog *logger.CSVLogger,
) *TradingEngine {
	return &TradingEngine{
		riskManager: riskMgr,
		broker:      brokerService,
		csvLogger:   csvLog,
	}
}

// PlaceOrder validates through risk manager and executes via broker
func (e *TradingEngine) PlaceOrder(symbol string, quantity int, side broker.OrderSide, tradeType risk.TradeType) error {
	log.Printf("\n=== Order Request: %s %s %d ===", side, symbol, quantity)

	// Get current market price from broker
	currentPrice := e.broker.GetCurrentPrice(symbol)
	if currentPrice == 0 {
		return fmt.Errorf("unable to get price for %s", symbol)
	}

	log.Printf("Current Market Price: ₹%.2f", currentPrice)

	// 1. Validate through Risk Manager
	valid, reason := e.riskManager.ValidateOrder(currentPrice, quantity, tradeType, convertToRiskSide(side))
	if !valid {
		log.Printf("❌ Order REJECTED by Risk Manager: %s", reason)
		return fmt.Errorf("risk validation failed: %s", reason)
	}

	log.Printf("✅ Order APPROVED by Risk Manager")

	// 2. Open position in Risk Manager (lock capital)
	e.riskManager.OpenPosition(symbol, currentPrice, quantity, tradeType, convertToRiskSide(side))

	// 3. Execute via Broker (with slippage/latency)
	order := broker.Order{
		Symbol:    symbol,
		Quantity:  quantity,
		Side:      side,
		OrderType: broker.Market,
	}

	filled, err := e.broker.PlaceOrder(order)
	if err != nil {
		// CRITICAL: Rollback risk manager position if broker fails
		log.Printf("❌ Broker execution failed: %v - Rolling back position", err)
		if rollbackErr := e.riskManager.RollbackPosition(symbol); rollbackErr != nil {
			log.Printf("⚠️ Rollback also failed: %v", rollbackErr)
		}
		return fmt.Errorf("broker execution failed: %w", err)
	}

	log.Printf("✅ Order FILLED by Broker: %s @ ₹%.2f (Slippage: ₹%.2f)",
		filled.Symbol, filled.FillPrice, filled.Slippage)

	// 4. CRITICAL: Update risk manager with actual fill price
	// This ensures accurate P&L when fill price differs from estimated price
	e.riskManager.UpdatePositionPrice(symbol, filled.FillPrice)

	// 5. Log trade to CSV
	if e.csvLogger != nil {
		e.csvLogger.LogTradeWithRisk(
			filled,
			e.broker.GetBalance(),
			e.riskManager.GetCurrentBalance(),
			e.riskManager.GetRealizedPnL(),
		)
	}

	return nil
}

// ClosePosition closes a position and realizes P&L
func (e *TradingEngine) ClosePosition(symbol string) error {
	log.Printf("\n=== Close Position Request: %s ===", symbol)

	// Check if position exists in risk manager
	positions := e.riskManager.GetOpenPositions()
	position, exists := positions[symbol]
	if !exists {
		return fmt.Errorf("no open position for %s", symbol)
	}

	// Get current market price
	currentPrice := e.broker.GetCurrentPrice(symbol)
	if currentPrice == 0 {
		return fmt.Errorf("unable to get price for %s", symbol)
	}

	log.Printf("Position: %s %d @ ₹%.2f | Current Price: ₹%.2f",
		position.Side, position.Quantity, position.EntryPrice, currentPrice)

	// Determine opposite side for closing
	var closeSide broker.OrderSide
	if position.Side == risk.Buy {
		closeSide = broker.Sell
	} else {
		closeSide = broker.Buy
	}

	// Execute close order via broker
	order := broker.Order{
		Symbol:    symbol,
		Quantity:  position.Quantity,
		Side:      closeSide,
		OrderType: broker.Market,
	}

	filled, err := e.broker.PlaceOrder(order)
	if err != nil {
		return fmt.Errorf("failed to close position: %w", err)
	}

	log.Printf("✅ Position CLOSED: %s @ ₹%.2f (Slippage: ₹%.2f)",
		filled.Symbol, filled.FillPrice, filled.Slippage)

	// Close position in risk manager and calculate P&L
	netPnL, err := e.riskManager.ClosePosition(symbol, filled.FillPrice)
	if err != nil {
		return fmt.Errorf("risk manager close failed: %w", err)
	}

	// Log trade to CSV
	if e.csvLogger != nil {
		e.csvLogger.LogTradeWithRisk(
			filled,
			e.broker.GetBalance(),
			e.riskManager.GetCurrentBalance(),
			e.riskManager.GetRealizedPnL(),
		)
	}

	log.Printf("💰 Net P&L: ₹%.2f | Risk Balance: ₹%.2f | Broker Balance: ₹%.2f",
		netPnL, e.riskManager.GetCurrentBalance(), e.broker.GetBalance())

	return nil
}

// GetSessionStats returns comprehensive session statistics including unrealized P&L
func (e *TradingEngine) GetSessionStats() map[string]interface{} {
	// Use broker's GetCurrentPrice as the price getter for unrealized P&L
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

// GetRiskManager returns the risk manager instance
func (e *TradingEngine) GetRiskManager() *risk.Manager {
	return e.riskManager
}

// GetBroker returns the broker instance
func (e *TradingEngine) GetBroker() broker.TradeService {
	return e.broker
}

// convertToRiskSide converts broker.OrderSide to risk.OrderSide
func convertToRiskSide(side broker.OrderSide) risk.OrderSide {
	if side == broker.Buy {
		return risk.Buy
	}
	return risk.Sell
}
