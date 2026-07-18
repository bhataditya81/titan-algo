package app

import (
	"log"
	"math/rand"
	"time"

	"titan-algo/internal/strategy"
)

// runModeALoop contains the logic from main.go's runModeA
func (app *TitanApp) runModeALoop(symbols []string) {
	_ = symbols // Suppress unused parameter warning
	rand.Seed(time.Now().UnixNano())
	log.Println("🚀 Starting Strategy Loop")

	// Initialize price history
	priceHistory := strategy.NewPriceHistory(app.Config.Engine.HistorySize)

	// Load Strategy
	stratName := app.Config.Brokers.Trading.ActiveStrategy
	if stratName == "" {
		stratName = "sniper"
	}
	activeStrategy, err := strategy.Get(stratName)
	if err != nil {
		log.Printf("Failed to initialize strategy '%s': %v", stratName, err)
		return
	}

	openPositions := make(map[string]bool)
	activeOptions := make(map[string][]string)

	pollInterval := time.Duration(app.Config.Engine.PollIntervalMs) * time.Millisecond
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	ticks := 0
	heartbeatTicks := (app.Config.Engine.HeartbeatIntervalSecs * 1000) / app.Config.Engine.PollIntervalMs
	if heartbeatTicks < 1 {
		heartbeatTicks = 1
	}

	for {
		select {
		case <-app.stopChan:
			log.Println("🛑 Strategy Loop Terminated")
			return
		case <-ticker.C:
			// ... logic from main.go loop ...
			ticks++

			// 1. Check Stop Losses
			if app.Config.Risk.StopLoss.Enabled {
				for symbol, open := range openPositions {
					if !open {
						continue
					}
					currentPrice := app.TradeService.GetCurrentPrice(symbol)
					if currentPrice <= 0 {
						continue
					}

					if app.RiskManager.ShouldTriggerStopLoss(symbol, currentPrice) {
						if err := app.TradingEngine.ClosePosition(symbol); err == nil {
							openPositions[symbol] = false
							log.Printf("🛑 STOP-LOSS TRIGGERED [%s]", symbol)
						}
					}
				}
			}

			// 2. Report Status to API
			if ticks%5 == 0 { // Update API every 5 ticks (~1 sec)
				stats := app.TradingEngine.GetSessionStats()

				// Convert risk positions to API positions
				// var apiPositions []api.PositionInfo // requires import cycle fix if we use api directly
				// Actually, we can pass generic structs or map interface

				// Safely type assert stats
				riskBal, _ := stats["risk_balance"].(float64)
				unrealized, _ := stats["unrealized_pnl"].(float64)
				realized, _ := stats["realized_pnl"].(float64)

				app.ApiServer.UpdateStatus(
					true,
					riskBal,
					unrealized,
					realized,
					nil, // Update detailed positions later
				)
			}

			// Heartbeat
			if ticks%heartbeatTicks == 0 {
				stats := app.TradingEngine.GetSessionStats()
				riskBal, _ := stats["risk_balance"].(float64)
				totalPnL, _ := stats["total_pnl"].(float64)
				log.Printf("💓 Heartbeat | Bal: ₹%.2f | P&L: ₹%.2f", riskBal, totalPnL)
			}

			// 3. Strategy Logic (Simplified for embedded - Placeholder)
			// TODO: Implement full strategy logic here using priceHistory and activeStrategy
			// For now, these variables are removed to fix linter errors.
			_ = priceHistory
			_ = activeStrategy
			_ = activeOptions
		}
	}
}
