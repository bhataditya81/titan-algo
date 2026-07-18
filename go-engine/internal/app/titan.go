package app

import (
	"log"
	"sync"

	"titan-algo/internal/api"
	"titan-algo/internal/broker"
	"titan-algo/internal/config"
	"titan-algo/internal/engine"
	"titan-algo/internal/logger"
	"titan-algo/internal/risk"
)

// TitanApp represents the main trading application
type TitanApp struct {
	Config        *config.Config
	TradeService  broker.TradeService
	RiskManager   *risk.Manager
	TradingEngine *engine.TradingEngine
	CSVLogger     *logger.CSVLogger
	ApiServer     *api.Server

	mode           string // "MODE_A", "MODE_B"
	tradingMode    string // "LIVE" or "PAPER"
	sessionBalance float64
	isRunning      bool
	mu             sync.Mutex
	stopChan       chan struct{}
}

// NewApp creates a new TitanApp instance
func NewApp(cfg *config.Config, mode string, tradingMode string, balance float64) *TitanApp {
	return &TitanApp{
		Config:         cfg,
		mode:           mode,
		tradingMode:    tradingMode,
		sessionBalance: balance,
		stopChan:       make(chan struct{}),
	}
}

// Initialize sets up the broker, risk manager, and engine
func (app *TitanApp) Initialize() error {
	log.Printf("🚀 Initializing TitanAlgo in %s mode (%s)", app.mode, app.tradingMode)

	// Initialize Broker
	if app.tradingMode == "LIVE" {
		log.Println("⚠️  ⚠️  ⚠️  LIVE TRADING MODE ⚠️  ⚠️  ⚠️")
		app.TradeService = broker.NewAngelBroker(
			app.Config.Brokers.Angel.ClientCode,
			app.Config.Brokers.Angel.PIN,
			app.Config.Brokers.Angel.APIKey,
			app.Config.Brokers.Angel.TOTPSecret,
		)
	} else {
		log.Println("🧪 PAPER TRADING MODE ENABLED")
		// Check for Angel credentials for Live Data
		if app.Config.Brokers.Angel.ClientCode != "" && app.Config.Brokers.Angel.APIKey != "" {
			log.Println("✨ Using LIVE DATA for Paper Trading")
			angelBroker := broker.NewAngelBroker(
				app.Config.Brokers.Angel.ClientCode,
				app.Config.Brokers.Angel.PIN,
				app.Config.Brokers.Angel.APIKey,
				app.Config.Brokers.Angel.TOTPSecret,
			)
			app.TradeService = broker.NewLivePaperBroker(angelBroker, app.sessionBalance)
		} else {
			log.Println("🎲 Using MOCK DATA for Paper Trading")
			app.TradeService = broker.NewMockBroker(app.sessionBalance)
		}
	}

	// Initialize Logger
	logDir := "logs"
	if app.tradingMode == "LIVE" {
		logDir = "logs/live"
	}
	var err error
	app.CSVLogger, err = logger.NewCSVLogger(logDir)
	if err != nil {
		return err
	}

	// Initialize Risk Manager
	app.RiskManager = risk.NewManager(
		app.Config.Risk.MaxDrawdownPercent,
		app.sessionBalance,
		app.Config.Risk.Brokerage,
		risk.StopLossConfig{
			Enabled:          app.Config.Risk.StopLoss.Enabled,
			Type:             app.Config.Risk.StopLoss.Type,
			Value:            app.Config.Risk.StopLoss.Value,
			Trailing:         app.Config.Risk.StopLoss.Trailing,
			TrailingDistance: app.Config.Risk.StopLoss.TrailingDistance,
		},
		100, // Max quantity per order (default)
	)
	app.RiskManager.KillSwitch = app.Config.Risk.KillSwitchEnabled

	// Create Trading Engine
	app.TradingEngine = engine.NewTradingEngine(app.RiskManager, app.TradeService, app.CSVLogger)

	// Initialize API Server (hardcoded API key for now, should be from config)
	// For mobile, we'll use a fixed key or generated one.
	apiKey := app.Config.Brokers.Angel.APIKey
	if apiKey == "" {
		apiKey = "titan-mobile-secret"
	}
	app.ApiServer = api.NewServer(8080, apiKey)

	return nil
}

// Start begins the trading engine and API server
func (app *TitanApp) Start() error {
	app.mu.Lock()
	if app.isRunning {
		app.mu.Unlock()
		return nil
	}
	app.isRunning = true
	app.stopChan = make(chan struct{})
	app.mu.Unlock()

	// Connect to broker
	if err := app.TradeService.Connect(); err != nil {
		return err
	}

	// Start API Server
	go func() {
		if err := app.ApiServer.Start(); err != nil {
			log.Printf("API Server error: %v", err)
		}
	}()

	log.Println("✅ TitanAlgo Engine Started")
	return nil
}

// Stop halts the engine
func (app *TitanApp) Stop() {
	app.mu.Lock()
	defer app.mu.Unlock()
	if !app.isRunning {
		return
	}
	app.isRunning = false
	close(app.stopChan)
	app.TradeService.Close()
	app.CSVLogger.Close()
	log.Println("🛑 TitanAlgo Engine Stopped")
}

// RunStrategy executes the selected strategy loop
// This logic was previously in runModeA
func (app *TitanApp) RunStrategy(symbols []string) {
	// Need to move runModeA logic here or call it
	// For now, we'll keep it simple and just log
	log.Printf("📋 Trading %d symbols: %v", len(symbols), symbols)

	if err := app.TradeService.Subscribe(symbols); err != nil {
		log.Printf("Failed to subscribe: %v", err)
		return
	}

	// Update API status
	app.ApiServer.UpdateStatus(true, app.sessionBalance, 0, 0, nil)

	// Start the main loop (Mode A logic) in a goroutine
	go app.runModeALoop(symbols)
}
