package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"titan-algo/internal/broker"
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
			ActiveStrategy string   `yaml:"active_strategy"`
			Symbols        []string `yaml:"symbols"`
			OptionsConfig  struct {
				IndexSymbol string `yaml:"index_symbol"`
				Expiry      string `yaml:"expiry"`
				StrikeStep  int    `yaml:"strike_step"`
			} `yaml:"options_config"`
		} `yaml:"trading"`
	} `yaml:"brokers"`
	Risk struct {
		SessionBalanceLimit float64              `yaml:"session_balance_limit"`
		MaxDrawdownPercent  float64              `yaml:"max_drawdown_percent"`
		Brokerage           risk.BrokerageConfig `yaml:"brokerage"`
	} `yaml:"risk"`
}

func loadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cfg Config
	decoder := yaml.NewDecoder(f)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, err
	}
	// Defaults if missing
	if cfg.Brokers.Trading.OptionsConfig.IndexSymbol == "" {
		cfg.Brokers.Trading.OptionsConfig.IndexSymbol = "NIFTY"
	}
	return &cfg, nil
}

func main() {
	log.Println("🚀 Starting NIFTY Options Backtest (Delta Simulation)...")

	// 1. Load Config
	config, err := loadConfig("config.yaml")
	if err != nil {
		config, err = loadConfig("../config.yaml")
		if err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}
	}

	// 2. Initialize Broker
	angel := broker.NewAngelBroker(
		config.Brokers.Angel.ClientCode,
		config.Brokers.Angel.PIN,
		config.Brokers.Angel.APIKey,
		config.Brokers.Angel.TOTPSecret,
	)

	// 3. Connect
	if err := angel.Connect(); err != nil {
		log.Fatalf("Failed to connect to Angel One: %v", err)
	}

	// 4. Initialize Risk Manager (for Charges)
	riskMgr := risk.NewManager(
		config.Risk.MaxDrawdownPercent,
		config.Risk.SessionBalanceLimit,
		config.Risk.Brokerage,
		risk.StopLossConfig{
			Enabled:          false, // Disable stop-loss for backtesting by default
			Type:             "percentage",
			Value:            5.0,
			Trailing:         false,
			TrailingDistance: 2.0,
		},
		100,
	)

	// 5. Fetch Historical Data for INDEX
	symbol := config.Brokers.Trading.OptionsConfig.IndexSymbol

	days := 30
	log.Printf("⏳ Fetching %d days of history for %s...", days, symbol)

	candles, err := angel.FetchHistory(symbol, "FIVE_MINUTE", days)
	if err != nil {
		log.Fatalf("Failed to fetch history: %v", err)
	}

	if len(candles) < 60 {
		log.Fatalf("Insufficient data: %d candles. (Check if symbol '%s' is correct for history API)", len(candles), symbol)
	}

	// DEBUG: Print first 5 candles to verify Time parsing
	log.Println("🔍 Verifying Data Quality (First 5 Candles):")
	for k := 0; k < 5 && k < len(candles); k++ {
		log.Printf("[%d] Time: %s | Close: %.2f", k, candles[k].Time.Format(time.RFC3339), candles[k].Close)
	}

	// 6. Run Simulation
	stratName := config.Brokers.Trading.ActiveStrategy
	log.Printf("DEBUG: Loaded Strategy from Config: '%s'", stratName)
	if stratName == "" {
		stratName = "sniper"
	}
	strat, err := strategy.Get(stratName)
	if err != nil {
		log.Fatalf("Failed to initialize strategy '%s': %v", stratName, err)
	}
	log.Printf("🧪 Running %s on %d candles...", strat.Name(), len(candles))

	// Simulation State
	type ActiveLeg struct {
		LegType      string                // "CE" or "PE"
		Direction    strategy.LegDirection // "BUY" or "SELL"
		EntrySpot    float64
		StrikeOffset int // relative to spot
		EntryIndex   int
	}
	var activeLegs []ActiveLeg

	var totalGrossPnL float64
	var totalCharges float64
	var totalNetPnL float64
	var winCount, lossCount int

	const LotSize = 50
	const Delta = 0.5 // ATM Delta Approximation
	const TradeType = risk.FNO

	minHistory := 52
	fmt.Printf("\n--- OPTION TRADE LOG (Simulated ATM Delta %.2f, Lot %d) ---\n", Delta, LotSize)

	for i := minHistory; i < len(candles); i++ {
		currentHistory := candles[:i+1]
		currentCandle := candles[i]
		spotPrice := currentCandle.Close

		// Evaluate
		signal := strat.EvaluateCandles(currentHistory)

		// Check for Exit/Switch
		shouldExit := false
		shouldEnter := false

		if len(signal.Legs) > 0 {
			shouldEnter = true
			shouldExit = true // Close previous before new
		} else if signal.Action == strategy.Buy && len(activeLegs) > 0 && signal.Legs == nil {
			shouldExit = true
		} else if signal.Action == strategy.Sell && len(activeLegs) > 0 && signal.Legs == nil {
			shouldExit = true
		}

		// 1. Process Exit
		if shouldExit && len(activeLegs) > 0 {
			tradePnL := 0.0
			logStr := fmt.Sprintf("CLOSE @ %.2f | ", spotPrice)

			for _, leg := range activeLegs {
				points := 0.0
				if leg.LegType == "CE" {
					if leg.Direction == strategy.LegBuy {
						points = (spotPrice - leg.EntrySpot) * Delta
					} else { // Short CE
						points = (leg.EntrySpot - spotPrice) * Delta
					}
				} else { // PE
					if leg.Direction == strategy.LegBuy { // Long PE
						points = (leg.EntrySpot - spotPrice) * Delta
					} else { // Short PE
						points = (spotPrice - leg.EntrySpot) * Delta
					}
				}

				// Add Theta Decay
				duration := float64(i - leg.EntryIndex)
				theta := 0.0
				if leg.Direction == strategy.LegSell {
					theta = duration * 0.5
				}
				points += theta

				tradePnL += points * float64(LotSize)
				logStr += fmt.Sprintf("[%s %s: %.2f] ", leg.Direction, leg.LegType, points)
			}

			// Charges
			estPremium := 150.0 // Avg premium
			legCount := float64(len(activeLegs))
			totalTradeCharges := riskMgr.CalculateTotalCharges(estPremium, LotSize*int(legCount), TradeType, risk.Buy) * 2

			netPnL := tradePnL - totalTradeCharges

			totalGrossPnL += tradePnL
			totalCharges += totalTradeCharges
			totalNetPnL += netPnL
			// fmt.Printf("DEBUG: TradePnL: %.2f | TotalGross: %.2f\n", tradePnL, totalGrossPnL)

			if netPnL > 0 {
				winCount++
			} else {
				lossCount++
			}

			fmt.Printf("%s| Net: Rs. %.2f | %s\n", logStr, netPnL, signal.Reason)

			activeLegs = []ActiveLeg{} // Cleared
		}

		// 2. Process Entry
		if shouldEnter {
			if len(signal.Legs) > 0 {
				// Multi-Leg Strategy (Existing Logic)
				for _, leg := range signal.Legs {
					activeLegs = append(activeLegs, ActiveLeg{
						LegType:      leg.OptionType,
						Direction:    leg.Direction,
						EntrySpot:    spotPrice,
						StrikeOffset: leg.StrikeOffset,
						EntryIndex:   i,
					})
				}
				fmt.Printf("OPEN  %d Legs @ Spot %.2f | %s\n", len(activeLegs), spotPrice, signal.Reason)
			} else {
				// Single-Leg Strategy (Sniper/Momentum) - Legacy Support
				// Simulate: Buy Signal -> Long ATM CE, Sell Signal -> Long ATM PE
				legType := "CE"
				if signal.Action == strategy.Sell {
					legType = "PE"
				}

				activeLegs = append(activeLegs, ActiveLeg{
					LegType:      legType,
					Direction:    strategy.LegBuy, // Always Buy Option for Directional
					EntrySpot:    spotPrice,
					StrikeOffset: 0, // ATM
					EntryIndex:   i,
				})
				fmt.Printf("OPEN  1 Leg (%s Buy) @ Spot %.2f | %s\n", legType, spotPrice, signal.Reason)
			}
		}
	}

	// FORCE CLOSE EXISTING POSITIONS AT END OF DATA
	if len(activeLegs) > 0 {
		spotPrice := candles[len(candles)-1].Close
		fmt.Printf("⚠️  FORCE CLOSING %d Legs @ End Spot %.2f\n", len(activeLegs), spotPrice)

		tradePnL := 0.0
		for _, leg := range activeLegs {
			points := 0.0
			if leg.LegType == "CE" {
				if leg.Direction == strategy.LegBuy {
					points = (spotPrice - leg.EntrySpot) * Delta
				} else {
					points = (leg.EntrySpot - spotPrice) * Delta
				}
			} else { // PE
				if leg.Direction == strategy.LegBuy {
					points = (leg.EntrySpot - spotPrice) * Delta
				} else {
					points = (spotPrice - leg.EntrySpot) * Delta
				}
			}
			tradePnL += points * float64(LotSize)
		}

		// Charges
		estPremium := 150.0
		legCount := float64(len(activeLegs))
		totalTradeCharges := riskMgr.CalculateTotalCharges(estPremium, LotSize*int(legCount), TradeType, risk.Buy) * 2
		netPnL := tradePnL - totalTradeCharges

		totalGrossPnL += tradePnL
		totalCharges += totalTradeCharges
		totalNetPnL += netPnL

		if netPnL > 0 {
			winCount++
		} else {
			lossCount++
		}
		fmt.Printf("FORCE CLOSE | Net: Rs. %.2f\n", netPnL)
	}

	fmt.Println("---------------------------------------------------")
	fmt.Printf("\n📊 BACKTEST REPORT: %s\n", strat.Name())
	fmt.Printf("Total Trades: %d (Wins: %d | Losses: %d)\n", winCount+lossCount, winCount, lossCount)
	fmt.Printf("Gross P&L:    Rs. %.2f\n", totalGrossPnL)
	fmt.Printf("Charges (Est):Rs. %.2f\n", totalCharges)
	fmt.Printf("NET P&L:      Rs. %.2f\n", totalNetPnL)
	fmt.Println("---------------------------------------------------")
}
