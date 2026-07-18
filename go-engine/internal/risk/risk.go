package risk

import (
	"fmt"
	"log"
	"math"
	"sync"
)

// StopLossConfig holds stop-loss configuration
type StopLossConfig struct {
	Enabled          bool
	Type             string  // "percentage", "points", or "atr"
	Value            float64 // Loss threshold value
	Trailing         bool    // Enable trailing stop-loss
	TrailingDistance float64 // Trailing distance from peak
}

// BrokerageConfig holds all charge parameters
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

// Position tracks an open position
type Position struct {
	Symbol        string
	EntryPrice    float64
	Quantity      int
	Side          OrderSide
	TradeType     TradeType
	EntryCharges  float64
	EntryTime     int64
	StopLossPrice float64 // Stop-loss trigger price
	PeakPrice     float64 // Highest price for trailing stop (Long) or lowest for Short
}

// Manager handles risk management including balance limits
type Manager struct {
	mu                   sync.RWMutex
	MaxDrawdownPercent   float64
	KillSwitch           bool
	InitialBalance       float64 // Starting balance for the session
	CurrentBalance       float64 // Current balance (Initial + P&L)
	SessionBalanceUsed   float64 // Capital currently locked in positions
	RealizedPnL          float64 // Total realized profit/loss
	BrokerageConfig      BrokerageConfig
	StopLossConfig       StopLossConfig // Stop-loss configuration
	MaxOrdersPerMin      int
	OrderCountThisMinute int
	OpenPositions        map[string]*Position // Track open positions by symbol
}

// NewManager creates a new risk manager
func NewManager(maxDrawdown, sessionLimit float64, brokerageConfig BrokerageConfig, stopLossConfig StopLossConfig, maxOrdersPerMin int) *Manager {
	return &Manager{
		MaxDrawdownPercent:   maxDrawdown,
		KillSwitch:           false,
		InitialBalance:       sessionLimit,
		CurrentBalance:       sessionLimit,
		SessionBalanceUsed:   0.0,
		RealizedPnL:          0.0,
		BrokerageConfig:      brokerageConfig,
		StopLossConfig:       stopLossConfig,
		MaxOrdersPerMin:      maxOrdersPerMin,
		OrderCountThisMinute: 0,
		OpenPositions:        make(map[string]*Position),
	}
}

// TradeType represents the type of trade
type TradeType string

const (
	EquityDelivery TradeType = "equity_delivery"
	EquityIntraday TradeType = "equity_intraday"
	FNO            TradeType = "fno"
)

// OrderSide represents buy or sell
type OrderSide string

const (
	Buy  OrderSide = "BUY"
	Sell OrderSide = "SELL"
)

// CalculateTotalCharges computes all charges for a trade
func (m *Manager) CalculateTotalCharges(price float64, quantity int, tradeType TradeType, side OrderSide) float64 {
	turnover := price * float64(quantity)

	// 1. Brokerage
	// For F&O Options: Flat ₹20 per order (Angel One/Zerodha style)
	// For Equity: ₹20 or 0.03% of turnover, whichever is lower
	var brokerage float64
	if tradeType == FNO {
		brokerage = m.BrokerageConfig.FlatFeePerOrder // Flat fee for F&O
	} else {
		brokerage = math.Min(m.BrokerageConfig.FlatFeePerOrder, turnover*m.BrokerageConfig.PercentageFee/100.0)
	}

	// 2. STT (Securities Transaction Tax)
	var stt float64
	switch tradeType {
	case EquityDelivery:
		stt = turnover * m.BrokerageConfig.STT.EquityDelivery / 100.0
	case EquityIntraday:
		if side == Sell {
			stt = turnover * m.BrokerageConfig.STT.EquityIntraday / 100.0
		}
	case FNO:
		if side == Sell {
			stt = turnover * m.BrokerageConfig.STT.FNO / 100.0
		}
	}

	// 3. Transaction Charges
	var txnCharges float64
	switch tradeType {
	case EquityDelivery, EquityIntraday:
		txnCharges = turnover * m.BrokerageConfig.TransactionCharges.NSEEquity / 100.0
	case FNO:
		txnCharges = turnover * m.BrokerageConfig.TransactionCharges.NSEFNO / 100.0
	}

	// 4. GST on (Brokerage + Transaction Charges)
	gst := (brokerage + txnCharges) * m.BrokerageConfig.GSTPercent / 100.0

	// 5. SEBI Turnover Fee
	sebiCharges := turnover * m.BrokerageConfig.SEBITurnoverFee / 100.0

	// 6. Stamp Duty (only on buy side)
	var stampDuty float64
	if side == Buy {
		stampDuty = turnover * m.BrokerageConfig.StampDuty / 100.0
	}

	totalCharges := brokerage + stt + txnCharges + gst + sebiCharges + stampDuty

	// log.Printf("Charge Breakdown - Brokerage: ₹%.2f, STT: ₹%.2f, Txn: ₹%.2f, GST: ₹%.2f, SEBI: ₹%.2f, Stamp: ₹%.2f | Total: ₹%.2f",
	// 	brokerage, stt, txnCharges, gst, sebiCharges, stampDuty, totalCharges)

	return totalCharges
}

// ValidateOrder checks if order can be placed within session balance limit
func (m *Manager) ValidateOrder(price float64, quantity int, tradeType TradeType, side OrderSide) (bool, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Check kill switch
	if m.KillSwitch {
		return false, "Kill Switch is ACTIVE"
	}

	// Check order rate limit
	if m.OrderCountThisMinute >= m.MaxOrdersPerMin {
		return false, fmt.Sprintf("Order rate limit exceeded (%d orders/min)", m.MaxOrdersPerMin)
	}

	// Calculate total cost including charges
	turnover := price * float64(quantity)
	charges := m.CalculateTotalCharges(price, quantity, tradeType, side)
	totalCost := turnover + charges

	// Check if this trade would exceed current balance
	availableBalance := m.CurrentBalance - m.SessionBalanceUsed
	if totalCost > availableBalance {
		return false, fmt.Sprintf("Insufficient balance. Need: ₹%.2f, Available: ₹%.2f (Current: ₹%.2f, Locked: ₹%.2f)",
			totalCost, availableBalance, m.CurrentBalance, m.SessionBalanceUsed)
	}

	return true, ""
}

// OpenPosition records opening a new position
func (m *Manager) OpenPosition(symbol string, price float64, quantity int, tradeType TradeType, side OrderSide) {
	m.mu.Lock()
	defer m.mu.Unlock()

	turnover := price * float64(quantity)
	charges := m.CalculateTotalCharges(price, quantity, tradeType, side)
	totalCost := turnover + charges

	// Lock the capital
	m.SessionBalanceUsed += totalCost
	m.OrderCountThisMinute++

	// Calculate stop-loss price if enabled
	var stopLossPrice float64
	if m.StopLossConfig.Enabled {
		stopLossPrice = m.calculateStopLossPrice(price, side)
	}

	// Store position
	m.OpenPositions[symbol] = &Position{
		Symbol:        symbol,
		EntryPrice:    price,
		Quantity:      quantity,
		Side:          side,
		TradeType:     tradeType,
		EntryCharges:  charges,
		EntryTime:     0, // TODO: Add timestamp
		StopLossPrice: stopLossPrice,
		PeakPrice:     price, // Initialize peak to entry price
	}

	if m.StopLossConfig.Enabled {
		log.Printf("Position OPENED - %s %s %d @ ₹%.2f | Stop-Loss: ₹%.2f | Charges: ₹%.2f | Locked: ₹%.2f | Available: ₹%.2f/₹%.2f",
			side, symbol, quantity, price, stopLossPrice, charges, m.SessionBalanceUsed,
			m.CurrentBalance-m.SessionBalanceUsed, m.CurrentBalance)
	} else {
		log.Printf("Position OPENED - %s %s %d @ ₹%.2f | Charges: ₹%.2f | Locked: ₹%.2f | Available: ₹%.2f/₹%.2f",
			side, symbol, quantity, price, charges, m.SessionBalanceUsed,
			m.CurrentBalance-m.SessionBalanceUsed, m.CurrentBalance)
	}
}

// RollbackPosition reverses a pending position when broker order fails
// This releases locked capital and removes the position from tracking
func (m *Manager) RollbackPosition(symbol string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	position, exists := m.OpenPositions[symbol]
	if !exists {
		return fmt.Errorf("no position to rollback for %s", symbol)
	}

	// Calculate what was locked
	turnover := position.EntryPrice * float64(position.Quantity)
	totalCost := turnover + position.EntryCharges

	// Release the locked capital
	m.SessionBalanceUsed -= totalCost
	m.OrderCountThisMinute-- // Also decrement order count

	// Remove the position
	delete(m.OpenPositions, symbol)

	log.Printf("⚠️ Position ROLLED BACK - %s %s %d @ ₹%.2f | Released: ₹%.2f | Available: ₹%.2f/₹%.2f",
		position.Side, symbol, position.Quantity, position.EntryPrice, totalCost,
		m.CurrentBalance-m.SessionBalanceUsed, m.CurrentBalance)

	return nil
}

// UpdatePositionPrice updates the entry price after broker confirms actual fill price
// This ensures accurate P&L tracking when fill price differs from estimated price
func (m *Manager) UpdatePositionPrice(symbol string, actualFillPrice float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	position, exists := m.OpenPositions[symbol]
	if !exists {
		log.Printf("⚠️ Cannot update price for %s: position not found", symbol)
		return
	}

	oldPrice := position.EntryPrice
	priceDiff := actualFillPrice - oldPrice

	// Update entry price
	position.EntryPrice = actualFillPrice
	position.PeakPrice = actualFillPrice // Reset peak to new price

	// Recalculate stop-loss with new entry price
	if m.StopLossConfig.Enabled {
		position.StopLossPrice = m.calculateStopLossPrice(actualFillPrice, position.Side)
	}

	// Recalculate locked capital based on actual fill price
	oldTurnover := oldPrice * float64(position.Quantity)
	newTurnover := actualFillPrice * float64(position.Quantity)
	m.SessionBalanceUsed += (newTurnover - oldTurnover)

	if priceDiff != 0 {
		log.Printf("📊 Position price updated [%s]: ₹%.2f → ₹%.2f | New Stop-Loss: ₹%.2f",
			symbol, oldPrice, actualFillPrice, position.StopLossPrice)
	}
}

// ClosePosition closes an existing position and calculates P&L
func (m *Manager) ClosePosition(symbol string, exitPrice float64) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	position, exists := m.OpenPositions[symbol]
	if !exists {
		return 0, fmt.Errorf("no open position found for %s", symbol)
	}

	// Calculate exit charges
	exitCharges := m.CalculateTotalCharges(exitPrice, position.Quantity, position.TradeType,
		oppositeOrderSide(position.Side))

	// Calculate P&L
	var pnl float64
	if position.Side == Buy {
		// Bought low, selling high (hopefully)
		pnl = (exitPrice - position.EntryPrice) * float64(position.Quantity)
	} else {
		// Sold high, buying back low (hopefully)
		pnl = (position.EntryPrice - exitPrice) * float64(position.Quantity)
	}

	// Subtract all charges from P&L
	totalCharges := position.EntryCharges + exitCharges
	netPnL := pnl - totalCharges

	// Update balances
	entryTurnover := position.EntryPrice * float64(position.Quantity)
	entryCost := entryTurnover + position.EntryCharges

	// Release locked capital
	m.SessionBalanceUsed -= entryCost

	// Adjust current balance with realized P&L
	m.CurrentBalance += netPnL
	m.RealizedPnL += netPnL

	// Remove position
	delete(m.OpenPositions, symbol)

	log.Printf("Position CLOSED - %s | Entry: ₹%.2f, Exit: ₹%.2f | Gross P&L: ₹%.2f | Charges: ₹%.2f | Net P&L: ₹%.2f",
		symbol, position.EntryPrice, exitPrice, pnl, totalCharges, netPnL)
	log.Printf("Balance Update - Current: ₹%.2f (Initial: ₹%.2f, Realized P&L: ₹%.2f) | Available: ₹%.2f",
		m.CurrentBalance, m.InitialBalance, m.RealizedPnL, m.CurrentBalance-m.SessionBalanceUsed)

	return netPnL, nil
}

// oppositeOrderSide returns the opposite side for closing positions
func oppositeOrderSide(side OrderSide) OrderSide {
	if side == Buy {
		return Sell
	}
	return Buy
}

// ResetOrderCount resets the per-minute order counter (call this every minute)
func (m *Manager) ResetOrderCount() {
	m.OrderCountThisMinute = 0
}

// GetRemainingBalance returns available session balance
func (m *Manager) GetRemainingBalance() float64 {
	return m.CurrentBalance - m.SessionBalanceUsed
}

// GetCurrentBalance returns the current balance including realized P&L
func (m *Manager) GetCurrentBalance() float64 {
	return m.CurrentBalance
}

// GetRealizedPnL returns total realized profit/loss
func (m *Manager) GetRealizedPnL() float64 {
	return m.RealizedPnL
}

// GetOpenPositions returns a copy of open positions
func (m *Manager) GetOpenPositions() map[string]*Position {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Return a copy to prevent race conditions
	positions := make(map[string]*Position)
	for k, v := range m.OpenPositions {
		positions[k] = &Position{
			Symbol:        v.Symbol,
			EntryPrice:    v.EntryPrice,
			Quantity:      v.Quantity,
			Side:          v.Side,
			TradeType:     v.TradeType,
			EntryCharges:  v.EntryCharges,
			EntryTime:     v.EntryTime,
			StopLossPrice: v.StopLossPrice,
			PeakPrice:     v.PeakPrice,
		}
	}
	return positions
}

// GetUnrealizedPnL calculates unrealized P&L for all open positions
// priceGetter is a function that returns current price for a symbol
func (m *Manager) GetUnrealizedPnL(priceGetter func(symbol string) float64) float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var unrealizedPnL float64
	for symbol, pos := range m.OpenPositions {
		currentPrice := priceGetter(symbol)
		if currentPrice <= 0 {
			continue
		}

		var pnl float64
		if pos.Side == Buy {
			// Long position: profit when price goes up
			pnl = (currentPrice - pos.EntryPrice) * float64(pos.Quantity)
		} else {
			// Short position: profit when price goes down
			pnl = (pos.EntryPrice - currentPrice) * float64(pos.Quantity)
		}
		unrealizedPnL += pnl
	}
	return unrealizedPnL
}

// GetTotalPnL returns realized + unrealized P&L
func (m *Manager) GetTotalPnL(priceGetter func(symbol string) float64) float64 {
	return m.RealizedPnL + m.GetUnrealizedPnL(priceGetter)
}

// GetSessionStats returns session statistics
func (m *Manager) GetSessionStats() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return map[string]interface{}{
		"initial_balance": m.InitialBalance,
		"current_balance": m.CurrentBalance,
		"locked_capital":  m.SessionBalanceUsed,
		"available":       m.CurrentBalance - m.SessionBalanceUsed,
		"realized_pnl":    m.RealizedPnL,
		"pnl_percentage":  (m.RealizedPnL / m.InitialBalance) * 100,
		"open_positions":  len(m.OpenPositions),
	}
}

// GetSessionStatsWithPrices returns session statistics including unrealized P&L
func (m *Manager) GetSessionStatsWithPrices(priceGetter func(symbol string) float64) map[string]interface{} {
	unrealizedPnL := m.GetUnrealizedPnL(priceGetter)
	totalPnL := m.RealizedPnL + unrealizedPnL

	return map[string]interface{}{
		"initial_balance": m.InitialBalance,
		"current_balance": m.CurrentBalance,
		"locked_capital":  m.SessionBalanceUsed,
		"available":       m.CurrentBalance - m.SessionBalanceUsed,
		"realized_pnl":    m.RealizedPnL,
		"unrealized_pnl":  unrealizedPnL,
		"total_pnl":       totalPnL,
		"pnl_percentage":  (totalPnL / m.InitialBalance) * 100,
		"open_positions":  len(m.OpenPositions),
	}
}

// CheckRisk performs general risk checks
func (m *Manager) CheckRisk() bool {
	if m.KillSwitch {
		log.Println("RISK ALERT: Kill Switch is ACTIVE. Halting trading.")
		return false
	}

	// Check if current balance has dropped below minimum threshold
	if m.CurrentBalance <= 0 {
		log.Printf("RISK ALERT: Current balance depleted (₹%.2f)", m.CurrentBalance)
		return false
	}

	// Check max drawdown
	drawdownPercent := ((m.InitialBalance - m.CurrentBalance) / m.InitialBalance) * 100
	if drawdownPercent >= m.MaxDrawdownPercent {
		log.Printf("RISK ALERT: Max drawdown reached (%.2f%% >= %.2f%%)", drawdownPercent, m.MaxDrawdownPercent)
		return false
	}

	return true
}

// calculateStopLossPrice calculates the stop-loss trigger price
func (m *Manager) calculateStopLossPrice(entryPrice float64, side OrderSide) float64 {
	var stopPrice float64

	switch m.StopLossConfig.Type {
	case "percentage":
		lossPercent := m.StopLossConfig.Value / 100.0
		if side == Buy {
			// Long position: stop below entry
			stopPrice = entryPrice * (1.0 - lossPercent)
		} else {
			// Short position: stop above entry
			stopPrice = entryPrice * (1.0 + lossPercent)
		}
	case "points":
		if side == Buy {
			// Long position: stop below entry
			stopPrice = entryPrice - m.StopLossConfig.Value
		} else {
			// Short position: stop above entry
			stopPrice = entryPrice + m.StopLossConfig.Value
		}
	case "atr":
		// ATR-based stop-loss not implemented yet
		// Fallback to 5% percentage
		if side == Buy {
			stopPrice = entryPrice * 0.95
		} else {
			stopPrice = entryPrice * 1.05
		}
	default:
		// Default to 5%
		if side == Buy {
			stopPrice = entryPrice * 0.95
		} else {
			stopPrice = entryPrice * 1.05
		}
	}

	return stopPrice
}

// ShouldTriggerStopLoss checks if a position's stop-loss should trigger
// Uses write lock when trailing stops are enabled since updateTrailingStop modifies position data
func (m *Manager) ShouldTriggerStopLoss(symbol string, currentPrice float64) bool {
	// Use write lock if trailing stop is enabled (we may modify position)
	if m.StopLossConfig.Trailing {
		m.mu.Lock()
		defer m.mu.Unlock()
	} else {
		m.mu.RLock()
		defer m.mu.RUnlock()
	}

	if !m.StopLossConfig.Enabled {
		return false
	}

	position, exists := m.OpenPositions[symbol]
	if !exists {
		return false
	}

	// Update trailing stop if enabled
	if m.StopLossConfig.Trailing {
		m.updateTrailingStop(position, currentPrice)
	}

	// Check if stop-loss triggered
	if position.Side == Buy {
		// Long position: trigger if price drops below stop
		return currentPrice <= position.StopLossPrice
	} else {
		// Short position: trigger if price rises above stop
		return currentPrice >= position.StopLossPrice
	}
}

// updateTrailingStop updates the stop-loss for trailing stops
func (m *Manager) updateTrailingStop(position *Position, currentPrice float64) {
	trailPercent := m.StopLossConfig.TrailingDistance / 100.0

	if position.Side == Buy {
		// Long: track highest price, trail stop below it
		if currentPrice > position.PeakPrice {
			position.PeakPrice = currentPrice
			newStop := currentPrice * (1.0 - trailPercent)
			// Only move stop up, never down
			if newStop > position.StopLossPrice {
				position.StopLossPrice = newStop
			}
		}
	} else {
		// Short: track lowest price, trail stop above it
		if currentPrice < position.PeakPrice {
			position.PeakPrice = currentPrice
			newStop := currentPrice * (1.0 + trailPercent)
			// Only move stop down, never up
			if newStop < position.StopLossPrice {
				position.StopLossPrice = newStop
			}
		}
	}
}
