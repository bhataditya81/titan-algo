package main

import (
	"log"
	"time"
	"titan-algo/internal/risk"
)

func main() {
	// Initialize brokerage config from config.yaml
	// Initialize brokerage config from config.yaml
	brokerageConfig := risk.BrokerageConfig{
		FlatFeePerOrder: 20.0,
		PercentageFee:   0.03,
		STT: struct {
			EquityDelivery float64 `yaml:"equity_delivery"`
			EquityIntraday float64 `yaml:"equity_intraday"`
			FNO            float64 `yaml:"fno"`
		}{
			EquityDelivery: 0.1,
			EquityIntraday: 0.025,
			FNO:            0.0125,
		},
		TransactionCharges: struct {
			NSEEquity float64 `yaml:"nse_equity"`
			NSEFNO    float64 `yaml:"nse_fno"`
			BSEEquity float64 `yaml:"bse_equity"`
		}{
			NSEEquity: 0.00325,
			NSEFNO:    0.00190,
			BSEEquity: 0.00375,
		},
		GSTPercent:      18.0,
		SEBITurnoverFee: 0.0001,
		StampDuty:       0.015,
	}

	// Create risk manager with stop-loss disabled for example
	stopLossConfig := risk.StopLossConfig{
		Enabled:          false,
		Type:             "percentage",
		Value:            5.0,
		Trailing:         false,
		TrailingDistance: 2.0,
	}

	rm := risk.NewManager(5.0, 10000.0, brokerageConfig, stopLossConfig, 100)

	log.Println("=== TitanAlgo Session Started ===")
	log.Printf("Initial Balance: ₹%.2f\n", rm.GetCurrentBalance())

	// Trade 1: Buy RELIANCE (Profitable Trade)
	symbol1 := "RELIANCE"
	entryPrice1 := 100.0
	quantity1 := 5

	valid, reason := rm.ValidateOrder(entryPrice1, quantity1, risk.EquityIntraday, risk.Buy)
	if !valid {
		log.Printf("Order REJECTED: %s", reason)
		return
	}

	log.Println("\n--- Trade 1: Opening Position ---")
	rm.OpenPosition(symbol1, entryPrice1, quantity1, risk.EquityIntraday, risk.Buy)

	// Simulate price movement - PROFIT
	time.Sleep(1 * time.Second)
	exitPrice1 := 110.0 // ₹10 profit per share
	log.Println("\n--- Trade 1: Closing Position (PROFIT) ---")
	pnl1, _ := rm.ClosePosition(symbol1, exitPrice1)
	log.Printf("Trade 1 Result: ₹%.2f\n", pnl1)

	// Trade 2: Buy TCS (Loss Trade)
	symbol2 := "TCS"
	entryPrice2 := 150.0
	quantity2 := 3

	valid, reason = rm.ValidateOrder(entryPrice2, quantity2, risk.EquityIntraday, risk.Buy)
	if !valid {
		log.Printf("Order REJECTED: %s", reason)
		return
	}

	log.Println("\n--- Trade 2: Opening Position ---")
	rm.OpenPosition(symbol2, entryPrice2, quantity2, risk.EquityIntraday, risk.Buy)

	// Simulate price movement - LOSS
	time.Sleep(1 * time.Second)
	exitPrice2 := 140.0 // ₹10 loss per share
	log.Println("\n--- Trade 2: Closing Position (LOSS) ---")
	pnl2, _ := rm.ClosePosition(symbol2, exitPrice2)
	log.Printf("Trade 2 Result: ₹%.2f\n", pnl2)

	// Trade 3: Try another trade with updated balance
	symbol3 := "INFY"
	entryPrice3 := 200.0
	quantity3 := 2

	log.Println("\n--- Trade 3: Attempting New Position ---")
	valid, reason = rm.ValidateOrder(entryPrice3, quantity3, risk.EquityIntraday, risk.Buy)
	if !valid {
		log.Printf("Order REJECTED: %s", reason)
	} else {
		rm.OpenPosition(symbol3, entryPrice3, quantity3, risk.EquityIntraday, risk.Buy)
		// Close with small profit
		exitPrice3 := 205.0
		time.Sleep(1 * time.Second)
		log.Println("\n--- Trade 3: Closing Position (PROFIT) ---")
		pnl3, _ := rm.ClosePosition(symbol3, exitPrice3)
		log.Printf("Trade 3 Result: ₹%.2f\n", pnl3)
	}

	// Print session summary
	log.Println("\n=== Session Summary ===")
	stats := rm.GetSessionStats()
	log.Printf("Initial Balance: ₹%.2f", stats["initial_balance"])
	log.Printf("Current Balance: ₹%.2f", stats["current_balance"])
	log.Printf("Realized P&L: ₹%.2f (%.2f%%)", stats["realized_pnl"], stats["pnl_percentage"])
	log.Printf("Available Balance: ₹%.2f", stats["available"])
	log.Printf("Open Positions: %d", stats["open_positions"])

	// Reset order counter every minute (in production, use a ticker)
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		for range ticker.C {
			rm.ResetOrderCount()
			log.Println("Order counter reset")
		}
	}()

	// Keep alive
	select {}
}
