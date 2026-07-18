package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"titan-algo/internal/broker"
	"titan-algo/internal/cli"
	"titan-algo/internal/discovery"
	"titan-algo/internal/engine"
	"titan-algo/internal/logger"
	"titan-algo/internal/risk"
	"titan-algo/internal/strategy"

	"gopkg.in/yaml.v2"
)

// Config structure
type Config struct {
	Brokers struct {
		Angel struct {
			ClientCode string `yaml:"client_code"`
			PIN        string `yaml:"pin"`
			APIKey     string `yaml:"api_key"`
			TOTPSecret string `yaml:"totp_secret"`
		} `yaml:"angel"`
		Trading struct {
			ActiveStrategy  string   `yaml:"active_strategy"`
			SymbolSelection string   `yaml:"symbol_selection"`
			TopNSymbols     int      `yaml:"top_n_symbols"`
			FallbackSymbols []string `yaml:"fallback_symbols"`
			Discovery       struct {
				Enabled               bool           `yaml:"enabled"`
				Indices               []string       `yaml:"indices"`
				LotSizes              map[string]int `yaml:"lot_sizes"`
				TopChainsCount        int            `yaml:"top_chains_count"`
				QuickStartEnabled     bool           `yaml:"quick_start_enabled"`
				FilterByAffordability bool           `yaml:"filter_by_affordability"`
			} `yaml:"discovery"`
			OptionsConfig struct {
				IndexSymbol string `yaml:"index_symbol"`
				Expiry      string `yaml:"expiry"`
				StrikeStep  int    `yaml:"strike_step"`
				LotSize     int    `yaml:"lot_size"`
			} `yaml:"options_config"`
		} `yaml:"trading"`
	} `yaml:"brokers"`
	Risk struct {
		SessionBalanceLimit float64              `yaml:"session_balance_limit"`
		MaxDrawdownPercent  float64              `yaml:"max_drawdown_percent"`
		KillSwitchEnabled   bool                 `yaml:"kill_switch_enabled"`
		Brokerage           risk.BrokerageConfig `yaml:"brokerage"`
		StopLoss            struct {
			Enabled          bool    `yaml:"enabled"`
			Type             string  `yaml:"type"`              // "percentage", "points", or "atr"
			Value            float64 `yaml:"value"`             // Loss threshold
			Trailing         bool    `yaml:"trailing"`          // Enable trailing stop
			TrailingDistance float64 `yaml:"trailing_distance"` // Trail distance
		} `yaml:"stop_loss"`
	} `yaml:"risk"`
	Engine struct {
		HistorySize           int `yaml:"history_size"`
		MinDataPoints         int `yaml:"min_data_points"`
		PollIntervalMs        int `yaml:"poll_interval_ms"`
		HeartbeatIntervalSecs int `yaml:"heartbeat_interval_seconds"`
		DefaultQuantity       int `yaml:"default_quantity"`
	} `yaml:"engine"`
}

func main() {
	// Parse command-line flags
	paperMode := flag.Bool("paper", false, "Enable paper trading mode (uses MockBroker)")
	liveMode := flag.Bool("live", false, "Enable LIVE trading mode (uses Angel One broker)")
	sessionBalance := flag.Float64("balance", 1000.0, "Session balance limit in INR")
	searchQuery := flag.String("search", "", "Search for symbols in Instrument Master (e.g., -search 'SILVER')")
	flag.Parse()

	// Handle Search Mode
	if *searchQuery != "" {
		fmt.Printf("🔍 Searching for '%s' in Angel One Instrument Master...\n", *searchQuery)
		im := broker.NewInstrumentManager()
		if err := im.LoadInstruments(); err != nil {
			log.Fatalf("Failed to load instruments: %v", err)
		}

		matches := im.Search(*searchQuery)
		fmt.Printf("\nFound %d matches:\n", len(matches))
		fmt.Println("----------------------------------------------------------------")
		fmt.Printf("%-25s | %-10s | %-10s | %-15s\n", "Symbol", "Exchange", "Token", "Type")
		fmt.Println("----------------------------------------------------------------")
		for _, m := range matches {
			fmt.Printf("%-25s | %-10s | %-10s | %-15s\n", m.Symbol, m.ExchSeg, m.Token, m.InstrumentType)
		}
		return
	}

	// Validate flags
	if *paperMode && *liveMode {
		log.Fatal("Cannot use both -paper and -live flags. Choose one.")
	}

	if !*paperMode && !*liveMode {
		log.Fatal("Must specify either -paper or -live flag")
	}

	mode := os.Getenv("TITAN_MODE")
	if mode == "" {
		mode = "MODE_A" // Default to F&O Predator
	}

	log.Printf("Starting TitanAlgo in %s mode", mode)

	// Load configuration
	config, err := loadConfig("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize broker
	var tradeService broker.TradeService
	var csvLogger *logger.CSVLogger
	var riskMgr *risk.Manager
	var tradingEngine *engine.TradingEngine

	// Ask for balance for BOTH Paper and Live modes
	// This ensures affordability filtering works correctly
	reader := bufio.NewReader(os.Stdin)
	if *paperMode || *liveMode {
		log.Println("🧪 PAPER TRADING MODE ENABLED")
		fmt.Println()
		fmt.Println("╔════════════════════════════════════════════════════════════╗")
		fmt.Println("║                  💰 SESSION CONFIGURATION                   ║")
		fmt.Println("╚════════════════════════════════════════════════════════════╝")
		fmt.Println()
		fmt.Printf("  Enter Session Balance (INR) [default: %.0f]: ₹", *sessionBalance)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)
		if input != "" {
			if bal, err := strconv.ParseFloat(input, 64); err == nil && bal > 0 {
				*sessionBalance = bal
			}
		}
		fmt.Printf("  ✅ Session Balance: ₹%.2f\n", *sessionBalance)
		fmt.Println()
	}

	if *liveMode {
		// LIVE TRADING MODE
		log.Println("⚠️  ⚠️  ⚠️  LIVE TRADING MODE ⚠️  ⚠️  ⚠️")

		// Safety confirmation
		if !confirmLiveTrading(*sessionBalance) {
			log.Println("Live trading cancelled by user")
			return
		}

		tradeService = broker.NewAngelBroker(
			config.Brokers.Angel.ClientCode,
			config.Brokers.Angel.PIN,
			config.Brokers.Angel.APIKey,
			config.Brokers.Angel.TOTPSecret,
		)
		log.Println("🔴 LIVE TRADING ENABLED")

	} else {
		// PAPER TRADING MODE
		log.Println("🧪 PAPER TRADING MODE ENABLED")

		// Check if we have Angel credentials to enable "Live Data Paper Trading"
		if config.Brokers.Angel.ClientCode != "" && config.Brokers.Angel.APIKey != "" {
			log.Println("✨ Using LIVE DATA for Paper Trading (Angel One Credentials Found)")
			angelBroker := broker.NewAngelBroker(
				config.Brokers.Angel.ClientCode,
				config.Brokers.Angel.PIN,
				config.Brokers.Angel.APIKey,
				config.Brokers.Angel.TOTPSecret,
			)
			tradeService = broker.NewLivePaperBroker(angelBroker, *sessionBalance)
		} else {
			log.Println("🎲 Using MOCK DATA for Paper Trading (No Angel One Credentials)")
			tradeService = broker.NewMockBroker(*sessionBalance)
		}

		log.Printf("💰 Session Balance Limit: ₹%.2f", *sessionBalance)
	}

	// Initialize CSV logger
	logDir := "logs"
	if *liveMode {
		logDir = "logs/live"
	}
	csvLogger, err = logger.NewCSVLogger(logDir)
	if err != nil {
		log.Fatalf("Failed to create CSV logger: %v", err)
	}
	defer csvLogger.Close()

	// Initialize Risk Manager

	riskMgr = risk.NewManager(
		config.Risk.MaxDrawdownPercent,
		*sessionBalance,
		config.Risk.Brokerage,
		risk.StopLossConfig{
			Enabled:          config.Risk.StopLoss.Enabled,
			Type:             config.Risk.StopLoss.Type,
			Value:            config.Risk.StopLoss.Value,
			Trailing:         config.Risk.StopLoss.Trailing,
			TrailingDistance: config.Risk.StopLoss.TrailingDistance,
		},
		100,
	)
	riskMgr.KillSwitch = config.Risk.KillSwitchEnabled

	// Create Trading Engine
	tradingEngine = engine.NewTradingEngine(riskMgr, tradeService, csvLogger)

	log.Printf("CSV Logger initialized: %s", csvLogger.GetFilePath())
	log.Printf("Risk Manager initialized with ₹%.2f session balance", *sessionBalance)
	if riskMgr.KillSwitch {
		log.Println("⚠️  WARNING: KILL SWITCH IS ENABLED IN CONFIG - TRADING HALTED ⚠️")

		if *liveMode {
			log.Println("🔥 ATTEMPTING EMERGENCY LIQUIDATION (Live Mode Only) 🔥")
			if err := tradeService.Connect(); err != nil {
				log.Printf("❌ Failed to connect for liquidation: %v", err)
			} else {
				// Refresh positions (AngelBroker Connect calls fetchRemotePositions)
				positions := tradeService.GetPositions()
				if len(positions) == 0 {
					log.Println("✅ No open positions found on server.")
				} else {
					for symbol, pos := range positions {
						log.Printf("🔥 Closing %s position: %s %d", symbol, pos.Side, pos.Quantity)

						// Determine closing side
						side := broker.Sell
						if pos.Side == broker.Sell {
							side = broker.Buy
						}

						order := broker.Order{
							Symbol:    symbol,
							Quantity:  pos.Quantity,
							Side:      side,
							OrderType: broker.Market, // Market order for emergency
						}

						if _, err := tradeService.PlaceOrder(order); err != nil {
							log.Printf("❌ FAILED to close %s: %v", symbol, err)
						} else {
							log.Printf("✅ %s Liquidated", symbol)
						}
					}
				}
			}
		} else {
			log.Println("ℹ️  Paper Trading Mode: No existing real positions to close.")
		}

		log.Println("🛑 Exiting due to Kill Switch.")
		os.Exit(0)
	}

	// Connect to broker
	if err := tradeService.Connect(); err != nil {
		log.Fatalf("Failed to connect to broker: %v", err)
	}
	defer tradeService.Close()

	// ═══════════════════════════════════════════════════════════════════════════
	// INTERACTIVE SYMBOL DISCOVERY
	// ═══════════════════════════════════════════════════════════════════════════

	var sessionConfig *cli.SessionConfig
	var symbols []string

	// Check for Quick Start (reuse last session)
	if config.Brokers.Trading.Discovery.QuickStartEnabled {
		if savedSession, ok := cli.QuickStart(reader); ok {
			sessionConfig = savedSession
			symbols = savedSession.SelectedChains
			*sessionBalance = savedSession.Balance
			log.Printf("✨ Quick Start: Using saved session (Strategy: %s, Balance: ₹%.2f)",
				savedSession.Strategy, savedSession.Balance)
		}
	}

	// If no quick start, run full interactive setup
	if sessionConfig == nil {
		if config.Brokers.Trading.Discovery.Enabled {
			// Get discovery indices
			indices := config.Brokers.Trading.Discovery.Indices
			if len(indices) == 0 {
				indices = discovery.GetDefaultIndices()
			}

			// Scan for top volume chains (filtered by user's balance entered earlier)
			sd := discovery.NewSymbolDiscovery(tradeService, indices, config.Brokers.Trading.Discovery.LotSizes, *sessionBalance)
			topChains, err := sd.ScanTopChains(config.Brokers.Trading.Discovery.TopChainsCount)

			if err != nil {
				log.Printf("⚠️  Discovery failed: %v. Using fallback symbols.", err)
				symbols = config.Brokers.Trading.FallbackSymbols
			} else {
				// STEP 3: Run interactive CLI (balance already collected, skip that step)
				sessionConfig, err = cli.RunInteractiveSetupWithBalance(reader, topChains, *sessionBalance)
				if err != nil {
					log.Fatalf("Configuration cancelled: %v", err)
				}

				symbols = sessionConfig.SelectedChains
				*sessionBalance = sessionConfig.Balance

				// Override config from CLI selections
				config.Brokers.Trading.ActiveStrategy = sessionConfig.Strategy
				config.Brokers.Trading.SymbolSelection = sessionConfig.SymbolSelection
				config.Brokers.Trading.TopNSymbols = sessionConfig.TopNSymbols

				// Save session for next quick start
				cli.SaveSession(sessionConfig)
			}
		} else {
			// Discovery disabled, use fallback symbols
			symbols = config.Brokers.Trading.FallbackSymbols
		}
	}

	// Validate we have symbols to trade
	if len(symbols) == 0 {
		log.Fatal("❌ No trading symbols selected. Cannot proceed.")
	}

	log.Printf("📋 Trading %d symbols: %v", len(symbols), symbols)

	// Subscribe to selected symbols
	if err := tradeService.Subscribe(symbols); err != nil {
		log.Fatalf("Failed to subscribe: %v", err)
	}

	// Update risk manager with selected balance
	riskMgr = risk.NewManager(
		config.Risk.MaxDrawdownPercent,
		*sessionBalance,
		config.Risk.Brokerage,
		risk.StopLossConfig{
			Enabled:          config.Risk.StopLoss.Enabled,
			Type:             config.Risk.StopLoss.Type,
			Value:            config.Risk.StopLoss.Value,
			Trailing:         config.Risk.StopLoss.Trailing,
			TrailingDistance: config.Risk.StopLoss.TrailingDistance,
		},
		100,
	)
	riskMgr.KillSwitch = config.Risk.KillSwitchEnabled
	tradingEngine = engine.NewTradingEngine(riskMgr, tradeService, csvLogger)

	switch mode {
	case "MODE_A":
		runModeA(tradingEngine, tradeService, *liveMode, symbols, config)
	case "MODE_B":
		runModeB(tradeService, symbols)
	default:
		log.Fatalf("Unknown mode: %s", mode)
	}
}

func loadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	// Ensure fallback symbols are initialized if missing in yaml
	if config.Brokers.Trading.FallbackSymbols == nil {
		config.Brokers.Trading.FallbackSymbols = []string{}
	}

	// Set discovery defaults
	if config.Brokers.Trading.Discovery.TopChainsCount == 0 {
		config.Brokers.Trading.Discovery.TopChainsCount = 10
	}

	// Set defaults for Engine config if missing
	if config.Engine.HistorySize == 0 {
		config.Engine.HistorySize = 100
	}
	if config.Engine.MinDataPoints == 0 {
		config.Engine.MinDataPoints = 20
	}
	if config.Engine.PollIntervalMs == 0 {
		config.Engine.PollIntervalMs = 2000
	}
	if config.Engine.HeartbeatIntervalSecs == 0 {
		config.Engine.HeartbeatIntervalSecs = 10
	}
	if config.Engine.DefaultQuantity == 0 {
		config.Engine.DefaultQuantity = 1
	}

	return &config, nil
}

func confirmLiveTrading(balance float64) bool {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║                  ⚠️  LIVE TRADING WARNING ⚠️                 ║")
	fmt.Println("╠════════════════════════════════════════════════════════════╣")
	fmt.Println("║  You are about to enable LIVE TRADING with REAL MONEY     ║")
	fmt.Printf("║  Session Balance: ₹%.2f                                   ║\n", balance)
	fmt.Println("║                                                            ║")
	fmt.Println("║  RISKS:                                                    ║")
	fmt.Println("║  • Real money will be used                                ║")
	fmt.Println("║  • Losses are REAL and can exceed session balance         ║")
	fmt.Println("║  • Market volatility can cause unexpected losses          ║")
	fmt.Println("║  • API failures may leave positions open                  ║")
	fmt.Println("║                                                            ║")
	fmt.Println("║  SAFETY MEASURES:                                          ║")
	fmt.Println("║  ✓ Session balance limit enforced                         ║")
	fmt.Println("║  ✓ Max drawdown protection (5%)                           ║")
	fmt.Println("║  ✓ Order rate limiting                                    ║")
	fmt.Println("║  ✓ All trades logged to CSV                               ║")
	fmt.Println("║                                                            ║")
	fmt.Println("║  RECOMMENDATIONS:                                          ║")
	fmt.Println("║  • Start with small amounts (₹100-500)                    ║")
	fmt.Println("║  • Monitor continuously during trading                    ║")
	fmt.Println("║  • Keep broker support number handy                       ║")
	fmt.Println("║  • Test thoroughly in paper mode first                    ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println("")

	for i := 0; i < 3; i++ {
		fmt.Print("Type 'I UNDERSTAND THE RISKS' to proceed (or 'cancel' to abort): ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		if strings.ToLower(input) == "cancel" {
			return false
		}

		if input == "I UNDERSTAND THE RISKS" {
			fmt.Println("")
			fmt.Print("Final confirmation - Type 'YES' to start live trading: ")
			confirm, _ := reader.ReadString('\n')
			confirm = strings.TrimSpace(confirm)

			if strings.ToUpper(confirm) == "YES" {
				fmt.Println("")
				fmt.Println("✅ Live trading confirmed. Starting in 3 seconds...")
				time.Sleep(3 * time.Second)
				return true
			}
			return false
		}

		fmt.Printf("Incorrect input. %d attempts remaining.\n\n", 2-i)
	}

	fmt.Println("Too many incorrect attempts. Aborting.")
	return false
}

func runModeA(te *engine.TradingEngine, ts broker.TradeService, liveMode bool, symbols []string, config *Config) {
	// Seed random number generator
	rand.Seed(time.Now().UnixNano())

	log.Println("Initializing Mode A: F&O Predator (Auto-Trade)")

	modeStr := "PAPER"
	if liveMode {
		modeStr = "LIVE"
	}
	log.Printf("🚀 Starting Strategy Loop [%s MODE]", modeStr)
	log.Println("⚠️  Press Ctrl+C to stop trading")

	// Initialize price history tracker
	priceHistory := strategy.NewPriceHistory(config.Engine.HistorySize)

	// Initialize Strategy from Config
	stratName := config.Brokers.Trading.ActiveStrategy
	if stratName == "" {
		stratName = "sniper"
	}
	activeStrategy, err := strategy.Get(stratName)
	if err != nil {
		log.Fatalf("Failed to initialize strategy '%s': %v", stratName, err)
	}

	log.Printf("📊 Strategy: %s (LIVE RESEARCH MODE)", activeStrategy.Name())
	// Check if castable to print rules? Strategies differ.
	log.Printf("   Strategy logic now driven by registry selection.")

	// Track positions per symbol (max 1 position per symbol)
	openPositions := make(map[string]bool)
	// Track active Option symbols for indices
	activeOptions := make(map[string][]string)

	// Ticker for strategy execution
	pollInterval := time.Duration(config.Engine.PollIntervalMs) * time.Millisecond
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Counter for heartbeat logging
	ticks := 0

	// Calculate heartbeat ticks based on config
	heartbeatTicks := (config.Engine.HeartbeatIntervalSecs * 1000) / config.Engine.PollIntervalMs
	if heartbeatTicks < 1 {
		heartbeatTicks = 1
	}

	// 🛡️ GRACEFUL SHUTDOWN HANDLER
	// Captures Ctrl+C and closes all positions before exit
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("\n🛑 SHUTDOWN SIGNAL RECEIVED - Closing all positions...")

		// Close all tracked positions
		positions := te.GetRiskManager().GetOpenPositions()
		for symbol := range positions {
			log.Printf("   Closing: %s", symbol)
			if err := te.ClosePosition(symbol); err != nil {
				log.Printf("   ❌ FAILED to close %s: %v", symbol, err)
			}
		}

		// Also close any tracked option legs
		for _, legs := range activeOptions {
			for _, legSym := range legs {
				log.Printf("   Closing leg: %s", legSym)
				if err := te.ClosePosition(legSym); err != nil {
					log.Printf("   ❌ FAILED to close leg %s: %v", legSym, err)
				}
			}
		}

		// Print final stats
		stats := te.GetSessionStats()
		log.Printf("📊 FINAL SESSION STATS:")
		log.Printf("   Realized P&L: ₹%.2f", stats["realized_pnl"])
		log.Printf("   Final Balance: ₹%.2f", stats["risk_balance"])

		log.Println("✅ Graceful shutdown complete. Goodbye!")
		os.Exit(0)
	}()

	for range ticker.C {
		ticks++

		// 🛑 CHECK STOP-LOSSES FIRST (EVERY TICK)
		// This ensures positions are closed immediately when stop-loss is hit
		if te.GetRiskManager().StopLossConfig.Enabled {
			for symbol := range openPositions {
				// Skip if position already closed
				if !openPositions[symbol] {
					continue
				}

				currentPrice := ts.GetCurrentPrice(symbol)
				if currentPrice <= 0 {
					continue
				}

				if te.GetRiskManager().ShouldTriggerStopLoss(symbol, currentPrice) {
					pos, exists := te.GetRiskManager().GetOpenPositions()[symbol]
					if exists {
						log.Printf("🛑 STOP-LOSS TRIGGERED [%s] | Entry: ₹%.2f | Stop: ₹%.2f | Current: ₹%.2f",
							symbol, pos.EntryPrice, pos.StopLossPrice, currentPrice)
						if err := te.ClosePosition(symbol); err == nil {
							openPositions[symbol] = false
							// Also close any tracked option legs
							if legs, ok := activeOptions[symbol]; ok {
								for _, legSym := range legs {
									te.ClosePosition(legSym)
								}
								activeOptions[symbol] = []string{}
							}
						}
					}
				}
			}
		}

		// Heartbeat
		if ticks%heartbeatTicks == 0 {
			// 🔄 SYNC: Fetch fresh prices for ALL held positions before calculating P&L
			// This ensures accurate unrealized P&L even if positions aren't in targetSymbols
			riskPositions := te.GetRiskManager().GetOpenPositions()
			if len(riskPositions) > 0 {
				heldSymbols := make([]string, 0, len(riskPositions))
				for sym := range riskPositions {
					heldSymbols = append(heldSymbols, sym)
				}
				// Fetch latest prices for held positions
				ts.FetchMarketDataBatch(heldSymbols)
			}

			// Get stats (now with fresh prices)
			stats := te.GetSessionStats()
			log.Printf("💓 Heartbeat | Balance: ₹%.2f | Positions: %d | Unrealized: ₹%.2f | Realized: ₹%.2f | Total P&L: ₹%.2f",
				stats["risk_balance"], stats["open_positions"],
				stats["unrealized_pnl"], stats["realized_pnl"], stats["total_pnl"])
		}

		// DETERMINE TARGET SYMBOLS FOR THIS TICK
		targetSymbols := symbols

		// For "highest_volume" mode: dynamically find the single best symbol at each tick
		// This is useful when you want to focus on whichever symbol is hottest RIGHT NOW
		if config.Brokers.Trading.SymbolSelection == "highest_volume" {
			bestSymbol, maxVol := findHighestVolumeSymbol(ts, symbols)
			if bestSymbol != "" {
				targetSymbols = []string{bestSymbol}
				if ticks%5 == 0 { // Log occasionally
					log.Printf("🔥 Highest Volume Target: %s (Vol: %.0f)", bestSymbol, maxVol)
				}
			}
		}

		// For "top_n_volume" and "manual" modes: use the pre-selected chains directly
		// These were already sorted by volume during discovery and selected by user
		// No need to re-filter - just use what the user selected
		if config.Brokers.Trading.SymbolSelection == "top_n_volume" || config.Brokers.Trading.SymbolSelection == "manual" {
			// targetSymbols is already set to 'symbols' which contains user's selections
			if ticks%5 == 0 { // Log occasionally
				log.Printf("📈 Trading %d selected chains: %v", len(targetSymbols), targetSymbols)
			}
		}

		for _, symbol := range targetSymbols {
			// Get current price
			price := ts.GetCurrentPrice(symbol)
			if price <= 0 {
				continue
			}

			// Get current volume
			volume := ts.GetCurrentVolume(symbol)

			// Add to price history
			priceHistory.Add(symbol, price, volume)

			// Get price/volume history for strategy evaluation
			prices := priceHistory.GetPrices(symbol)
			volumes := priceHistory.GetVolumes(symbol)

			// Need minimum data points before evaluating strategy
			dataPoints := priceHistory.GetDataPoints(symbol)
			if dataPoints < config.Engine.MinDataPoints {
				if dataPoints == 1 {
					log.Printf("📈 %s: ₹%.2f (Collecting data: %d/%d)", symbol, price, dataPoints, config.Engine.MinDataPoints)
				} else if dataPoints%5 == 0 {
					log.Printf("📈 %s: ₹%.2f (Data: %d/%d)", symbol, price, dataPoints, config.Engine.MinDataPoints)
				}
				continue
			}

			// Evaluate strategy
			signal := activeStrategy.Evaluate(symbol, prices, volumes, time.Now())

			// Check if we have active positions for this symbol
			activeLegs := activeOptions[symbol]

			// Process signals

			// Determine Trade Type
			isIndex := symbol == config.Brokers.Trading.OptionsConfig.IndexSymbol
			isOption := strings.HasSuffix(symbol, "CE") || strings.HasSuffix(symbol, "PE")
			tradeType := risk.EquityIntraday
			if isIndex || isOption {
				tradeType = risk.FNO
			}

			// Check current position state from Risk Manager
			// This gives us the TRUE state (Direction, Quantity)
			currentPos, posExists := te.GetRiskManager().GetOpenPositions()[symbol]

			if len(signal.Legs) > 0 {
				// --- MULTI-LEG ENTRY LOGIC (e.g., Straddles, Iron Fly) ---
				log.Printf("⚡ MULTI-LEG SIGNAL [%s] | %s", symbol, signal.Reason)

				// Close existing positions if signal indicates a new structure
				if posExists || len(activeLegs) > 0 {
					log.Printf("🔄 Closing existing positions for Multi-Leg re-entry...")
					for _, legSymbol := range activeLegs {
						te.ClosePosition(legSymbol)
					}
					if posExists {
						te.ClosePosition(symbol)
					}
					activeOptions[symbol] = []string{}
					openPositions[symbol] = false
				}

				// Place new legs
				// 🧠 SMART CONTEXT: If running Multi-Leg strategy on an Option Symbol,
				// we need the UNDERLYING index and its REAL spot price, not the option's LTP
				effectiveIndex := symbol
				effectiveExpiry := config.Brokers.Trading.OptionsConfig.Expiry
				effectiveSpot := price

				// Check if symbol looks like an option (contains digits = has strike price)
				if strings.ContainsAny(symbol, "0123456789") {
					// Extract underlying: remove trailing digits, CE/PE
					// e.g., BANKNIFTY27JAN2663000CE -> BANKNIFTY
					cleanIndex := symbol
					for _, idx := range []string{"NIFTY", "BANKNIFTY", "FINNIFTY", "SENSEX", "MIDCPNIFTY"} {
						if strings.HasPrefix(symbol, idx) {
							cleanIndex = idx
							break
						}
					}

					if cleanIndex != symbol {
						effectiveIndex = cleanIndex

						// Extract expiry: characters after index name, before strike
						// e.g., BANKNIFTY27JAN2663000CE -> 27JAN26
						remaining := strings.TrimPrefix(symbol, cleanIndex)
						if len(remaining) >= 7 {
							effectiveExpiry = remaining[:7] // DDMMMYY format
						}

						// Fetch REAL spot price of underlying index
						spot := ts.GetCurrentPrice(effectiveIndex)
						if spot <= 0 {
							// Force fetch if not cached
							prices, _ := ts.FetchMarketDataBatch([]string{effectiveIndex})
							if p, ok := prices[effectiveIndex]; ok && p > 0 {
								spot = p
							}
						}

						if spot > 0 {
							effectiveSpot = spot
							log.Printf("🧠 Smart Context: Using %s Spot ₹%.2f (Expiry: %s) for leg generation",
								effectiveIndex, effectiveSpot, effectiveExpiry)
						} else {
							log.Printf("⚠️ Smart Context: Could not fetch %s spot price, using option LTP", effectiveIndex)
						}
					}
				}

				var newLegs []string
				for _, leg := range signal.Legs {
					// Determine strike step based on underlying (BANKNIFTY=100, NIFTY=50)
					strikeStep := config.Brokers.Trading.OptionsConfig.StrikeStep
					switch effectiveIndex {
					case "BANKNIFTY":
						strikeStep = 100
					case "NIFTY", "FINNIFTY":
						strikeStep = 50
					case "SENSEX":
						strikeStep = 100
					case "MIDCPNIFTY":
						strikeStep = 25
					}

					// Construct symbol using EFFECTIVE (derived) context
					targetSymbol := getOptionSymbol(effectiveIndex,
						effectiveExpiry,
						effectiveSpot,
						strikeStep,
						leg.StrikeOffset,
						leg.OptionType)

					// Determine Order Side
					side := broker.Buy
					if leg.Direction == strategy.LegSell {
						side = broker.Sell
					}

					log.Printf("🎯 Leg: %s %s", leg.Direction, targetSymbol)

					// Lot Size Config
					lotSize := config.Brokers.Trading.OptionsConfig.LotSize
					if lotSize <= 0 {
						lotSize = 25
					}

					if err := te.PlaceOrder(targetSymbol, lotSize, side, risk.FNO); err != nil {
						log.Printf("❌ Leg Order Failed: %v", err)
					} else {
						newLegs = append(newLegs, targetSymbol)
					}
				}
				activeOptions[symbol] = newLegs
				openPositions[symbol] = len(newLegs) > 0

			} else {
				// --- SINGLE-LEG / DIRECTIONAL LOGIC ---
				switch signal.Action {
				case strategy.Buy:
					log.Printf("🟢 BUY SIGNAL [%s] | %s", symbol, signal.Reason)

					// If purely an exit signal (Squareoff), close everything
					if strings.Contains(signal.Reason, "Squareoff") {
						if posExists {
							log.Printf("🛑 Squareoff Signal - Closing Position")
							te.ClosePosition(symbol)
							openPositions[symbol] = false
						}
						continue
					}

					// Logic:
					// 1. If Short -> CLOSE (Cover)
					// 2. If Long -> HOLD (Already Long)
					// 3. If Flat -> OPEN LONG

					if posExists {
						if currentPos.Side == risk.Sell {
							// Reversal: Short -> Long
							log.Printf("🔄 Reversal: Closing SHORT position")
							te.ClosePosition(symbol)
							openPositions[symbol] = false
							// Proceed to Open Long
						} else {
							// Already Long - Check for profit-taking opportunity
							currentPrice := ts.GetCurrentPrice(symbol)
							entryCost := currentPos.EntryPrice * float64(currentPos.Quantity)
							currentValue := currentPrice * float64(currentPos.Quantity)
							profitPct := ((currentValue - entryCost) / entryCost) * 100

							// Take profit if > 2% gain
							if profitPct >= 2.0 {
								log.Printf("💰 PROFIT TAKING [%s]: %.2f%% gain (Entry: ₹%.2f, Current: ₹%.2f)",
									symbol, profitPct, currentPos.EntryPrice, currentPrice)
								te.ClosePosition(symbol)
								openPositions[symbol] = false
								continue
							}

							log.Printf("✋ Already LONG on %s - Holding (P&L: %.2f%%)", symbol, profitPct)
							continue
						}
					}

					// DETERMINE LOT SIZE DYNAMICALLY
					lotSize := 25 // Default
					// Check prefix to find correct lot size from map
					for idx, size := range config.Brokers.Trading.Discovery.LotSizes {
						if strings.HasPrefix(symbol, idx) {
							lotSize = size
							break
						}
					}
					// Fallback to global setting if not found
					if lotSize == 25 && config.Brokers.Trading.OptionsConfig.LotSize > 0 {
						lotSize = config.Brokers.Trading.OptionsConfig.LotSize
					}

					// Open Long with 50% of available balance
					availableBalance := te.GetRiskManager().GetRemainingBalance()
					useBalance := availableBalance * 0.50 // Use 50% of available balance
					costPerLot := price * float64(lotSize)

					// Calculate how many lots we can buy
					numLots := 1
					if costPerLot > 0 {
						numLots = int(useBalance / costPerLot)
						if numLots < 1 {
							numLots = 1 // Minimum 1 lot
						}
					}
					qty := numLots * lotSize

					log.Printf("💰 Order Sizing (%s): Balance ₹%.2f | 50%% = ₹%.2f | Lot Size: %d | Cost/Lot ₹%.2f | Lots: %d | Qty: %d",
						symbol, availableBalance, useBalance, lotSize, costPerLot, numLots, qty)

					// If Index, we trade Options (Call)
					if isIndex {
						targetSymbol := getOptionSymbol(symbol,
							config.Brokers.Trading.OptionsConfig.Expiry,
							price,
							config.Brokers.Trading.OptionsConfig.StrikeStep,
							0, "CE")

						log.Printf("🎯 Opening Long Call: %s (Qty: %d)", targetSymbol, qty)
						if err := te.PlaceOrder(targetSymbol, qty, broker.Buy, risk.FNO); err == nil {
							activeOptions[symbol] = []string{targetSymbol}
							openPositions[symbol] = true
						}
					} else {
						// Equity/Direct Option Buy
						if err := te.PlaceOrder(symbol, qty, broker.Buy, tradeType); err == nil {
							openPositions[symbol] = true
						}
					}
				case strategy.Sell:
					log.Printf("🔴 SELL SIGNAL [%s] | %s", symbol, signal.Reason)

					// Logic:
					// 1. If Long -> CLOSE (Sell)
					// 2. If Short -> HOLD (Already Short)
					// 3. If Flat -> OPEN SHORT

					if posExists {
						if currentPos.Side == risk.Buy {
							// Reversal: Long -> Short
							log.Printf("🔄 Reversal: Closing LONG position")
							te.ClosePosition(symbol)
							openPositions[symbol] = false
							// Proceed to Open Short
						} else {
							// Already Short - Check for profit-taking opportunity
							currentPrice := ts.GetCurrentPrice(symbol)
							entryCost := currentPos.EntryPrice * float64(currentPos.Quantity)
							currentValue := currentPrice * float64(currentPos.Quantity)
							// For shorts: profit = entry - current (price went down)
							profitPct := ((entryCost - currentValue) / entryCost) * 100

							// Take profit if > 2% gain
							if profitPct >= 2.0 {
								log.Printf("💰 PROFIT TAKING [%s]: %.2f%% gain (Entry: ₹%.2f, Current: ₹%.2f)",
									symbol, profitPct, currentPos.EntryPrice, currentPrice)
								te.ClosePosition(symbol)
								openPositions[symbol] = false
								continue
							}

							log.Printf("✋ Already SHORT on %s - Holding (P&L: %.2f%%)", symbol, profitPct)
							continue
						}
					}

					// Open Short
					// DETERMINE LOT SIZE DYNAMICALLY
					lotSize := 25 // Default
					// Check prefix to find correct lot size from map
					for idx, size := range config.Brokers.Trading.Discovery.LotSizes {
						if strings.HasPrefix(symbol, idx) {
							lotSize = size
							break
						}
					}
					// Fallback
					if lotSize == 25 && config.Brokers.Trading.OptionsConfig.LotSize > 0 {
						lotSize = config.Brokers.Trading.OptionsConfig.LotSize
					}

					qty := config.Engine.DefaultQuantity
					// For Options/Indices, use Lot Size based calculation
					if isOption || isIndex {
						// Calculate quantity based on 50% of available balance (Shorting margin roughly same as buying for simple logic, though in reality it's higher. We stick to 50% balance rule)
						// NOTE: Ideally for selling options we need margin calc, but for now using same balance-based sizing
						availableBalance := te.GetRiskManager().GetRemainingBalance()
						useBalance := availableBalance * 0.50

						// For shorting, price is the premium we collect, but margin required is roughly (Price * Lot) + Margin.
						// To keep it simple and consistent with user request "50% of available balance", we treat 'price' as cost basis approximation or just use standard lot sizing.
						// Actually, user said: "buy 50%... also sell accordingly".
						// We'll stick to the same logic: (Balance * 50%) / (Price * Lot)
						// This is conservative for buying, but for selling (shorting) option margin is huge.
						// However, if we are just closing positions it's fine. IF we are opening NEW shorts, we might fail margin check.
						// But let's implement the sizing logic requested.

						costPerLot := price * float64(lotSize)
						numLots := 1
						if costPerLot > 0 {
							numLots = int(useBalance / costPerLot)
							if numLots < 1 {
								numLots = 1
							}
						}
						qty = numLots * lotSize

						log.Printf("💰 Short Sizing (%s): Bal ₹%.2f | 50%% = ₹%.2f | Lot: %d | Qty: %d",
							symbol, availableBalance, useBalance, lotSize, qty)
					}

					// If Index, we trade Options (Put)
					if isIndex {
						targetSymbol := getOptionSymbol(symbol,
							config.Brokers.Trading.OptionsConfig.Expiry,
							price,
							config.Brokers.Trading.OptionsConfig.StrikeStep,
							0, "PE")

						log.Printf("🎯 Opening Long Put (Short Proxy): %s (Qty: %d)", targetSymbol, qty)
						// Wait! "Sell" signal on Index usually means buying a PUT for directional trade.
						// The previous logic was placing a BUY order for a PE (Long Put).
						// So this is actually a BUY logic for Puts.

						if err := te.PlaceOrder(targetSymbol, qty, broker.Buy, risk.FNO); err == nil {
							activeOptions[symbol] = []string{targetSymbol}
							openPositions[symbol] = true
						}
					} else {
						// Equity Short
						if err := te.PlaceOrder(symbol, qty, broker.Sell, tradeType); err == nil {
							openPositions[symbol] = true
						}
					}

				case strategy.Hold:
					if ticks%15 == 0 {
						log.Printf("⏸️  %s: ₹%.2f | %s", symbol, price, signal.Reason)
					}
				}
			}
		}
	}

	// Keep running until killed
	select {}
}

// getOptionSymbol constructs an Angel One NFO option symbol
// Format: {INDEX}{DDMMMYY}{STRIKE}{CE/PE}
// Example: NIFTY16JAN2624500CE = NIFTY Call, 24500 Strike, 16 Jan 2026 Expiry
//
// Parameters:
//   - index: Base symbol (e.g., "NIFTY", "BANKNIFTY", "SBIN") - NOT "SBIN-EQ"
//   - expiry: Date in format "DDMMMYY" (e.g., "16JAN26", "30JAN26")
//   - ltp: Current spot price for ATM strike calculation
//   - step: Strike interval (NIFTY=50, BANKNIFTY=100, SBIN=5)
//   - offset: Points away from ATM (0 for ATM, +100 for 1 strike OTM call, -100 for 1 strike OTM put)
//   - optionType: "CE" for Call, "PE" for Put
func getOptionSymbol(index, expiry string, ltp float64, step int, offset int, optionType string) string {
	// SANITIZE INDEX NAME:
	// If 'index' is passed as "BANKNIFTY20JAN26...", we extract just "BANKNIFTY"
	// This prevents malformed symbols like "BANKNIFTY...CE...CE"
	if strings.ContainsAny(index, "0123456789") {
		if strings.HasPrefix(index, "BANKNIFTY") {
			index = "BANKNIFTY"
		} else if strings.HasPrefix(index, "FINNIFTY") {
			index = "FINNIFTY"
		} else if strings.HasPrefix(index, "MIDCPNIFTY") {
			index = "MIDCPNIFTY"
		} else if strings.HasPrefix(index, "NIFTY") {
			index = "NIFTY"
		} else if strings.HasPrefix(index, "SENSEX") {
			index = "SENSEX"
		}
	}

	// Calculate ATM strike rounded to nearest step
	strike := math.Round(ltp/float64(step)) * float64(step)
	// Apply offset for OTM/ITM strikes
	strike += float64(offset)

	// Angel One NFO symbol format: {INDEX}{EXPIRY}{STRIKE}{TYPE}
	// e.g., NIFTY16JAN2624500CE, BANKNIFTY15JAN2652000PE
	symbol := fmt.Sprintf("%s%s%.0f%s", index, expiry, strike, optionType)

	log.Printf("📋 Generated Option Symbol: %s (Spot: %.2f, ATM Strike: %.0f)", symbol, ltp, strike-float64(offset))
	return symbol
}

func findHighestVolumeSymbol(ts broker.TradeService, symbols []string) (string, float64) {
	var bestSymbol string
	var maxVol float64 = -1

	for _, sym := range symbols {
		vol := ts.GetCurrentVolume(sym)
		if vol > maxVol {
			maxVol = vol
			bestSymbol = sym
		}
	}
	return bestSymbol, maxVol
}

// findTopNVolumeSymbols returns top N symbols sorted by volume
func findTopNVolumeSymbols(ts broker.TradeService, symbols []string, n int) []string {
	type volPair struct {
		symbol string
		volume float64
	}

	var pairs []volPair
	for _, sym := range symbols {
		vol := ts.GetCurrentVolume(sym)
		if vol > 0 { // Only include symbols with volume data
			pairs = append(pairs, volPair{sym, vol})
		}
	}

	// Sort by volume descending
	for i := 0; i < len(pairs); i++ {
		for j := i + 1; j < len(pairs); j++ {
			if pairs[j].volume > pairs[i].volume {
				pairs[i], pairs[j] = pairs[j], pairs[i]
			}
		}
	}

	// Take top N
	limit := n
	if limit > len(pairs) {
		limit = len(pairs)
	}

	result := make([]string, limit)
	for i := 0; i < limit; i++ {
		result[i] = pairs[i].symbol
	}
	return result
}

func runModeB(ts broker.TradeService, symbols []string) {
	log.Println("Initializing Mode B: Stock Vision (Analytics Mode)")
	log.Println("📊 Monitoring market data without taking positions...")

	// Ticker for monitoring
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	lastPrices := make(map[string]float64)

	for range ticker.C {
		fmt.Print(".") // Pulse

		for _, symbol := range symbols {
			price := ts.GetCurrentPrice(symbol)
			if price <= 0 {
				continue
			}

			prev := lastPrices[symbol]
			if prev == 0 {
				lastPrices[symbol] = price
				log.Printf("  %s: ₹%.2f (Initial)", symbol, price)
				continue
			}

			// Calculate change
			change := ((price - prev) / prev) * 100
			lastPrices[symbol] = price

			// Log if interesting movement (>0.01%)
			if change > 0.01 || change < -0.01 {
				trend := "➖"
				if change > 0 {
					trend = "📈"
				} else if change < 0 {
					trend = "📉"
				}

				log.Printf("%s %s: ₹%.2f (%+.2f%%)", trend, symbol, price, change)
			}
		}
	}
}
