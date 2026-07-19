package app

import (
	"context"
	"log"
	"sync"
	"time"

	"titan-algo/internal/api"
	"titan-algo/internal/broker"
	"titan-algo/internal/config"
	"titan-algo/internal/engine"
	"titan-algo/internal/ledger"
	"titan-algo/internal/logger"
	"titan-algo/internal/risk"
	"titan-algo/internal/state"
)

func durationMs(ms int) time.Duration { return time.Duration(ms) * time.Millisecond }
func durationSec(s int) time.Duration { return time.Duration(s) * time.Second }

// TitanApp represents the main trading application
type TitanApp struct {
	Config        *config.Config
	TradeService  broker.TradeService
	RiskManager   *risk.Manager
	TradingEngine *engine.TradingEngine
	CSVLogger     *logger.CSVLogger
	ApiServer     *api.Server
	Store         *state.Store
	Ledger        *ledger.Ledger
	Runner        *engine.Runner

	mode           string // "MODE_A", "MODE_B"
	tradingMode    string // "LIVE" or "PAPER"
	sessionBalance float64
	isRunning      bool
	mu             sync.Mutex
	cancelRun      context.CancelFunc
}

// NewApp creates a new TitanApp instance
func NewApp(cfg *config.Config, mode string, tradingMode string, balance float64) *TitanApp {
	return &TitanApp{
		Config:         cfg,
		mode:           mode,
		tradingMode:    tradingMode,
		sessionBalance: balance,
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

	// Durable state store + ledger (tasks 2, 11) — same stores cmd/main.go
	// uses, so a position opened from the CLI and one opened from the
	// mobile app share one reconciled book.
	statePath := app.Config.State.DBPath
	if statePath == "" {
		statePath = state.DefaultDBPath
	}
	var err2 error
	app.Store, err2 = state.Open(statePath)
	if err2 != nil {
		return err2
	}
	ledgerPath := app.Config.Ledger.DBPath
	if ledgerPath == "" {
		ledgerPath = ledger.DefaultDBPath
	}
	app.Ledger, err2 = ledger.Open(ledgerPath)
	if err2 != nil {
		return err2
	}

	ledgerMode := ledger.ModePaper
	if app.tradingMode == "LIVE" {
		ledgerMode = ledger.ModeLive
	}
	app.TradingEngine = engine.NewTradingEngine(app.RiskManager, app.TradeService, app.CSVLogger, app.Store, app.Ledger, ledgerMode)

	// api.token (WP-8) — NEVER the broker API key, and no hardcoded
	// fallback: an empty string tells api.NewServer to self-generate a
	// crypto/rand token (WP-4/CR-1 fix for the "titan-mobile-secret" /
	// broker-key-reuse bug).
	app.ApiServer = api.NewServer(8080, app.Config.API.Token)

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

// Stop halts the engine, cancelling the Runner's context so it flattens and
// shuts down gracefully (task 12) before the broker connection is closed.
func (app *TitanApp) Stop() {
	app.mu.Lock()
	defer app.mu.Unlock()
	if !app.isRunning {
		return
	}
	app.isRunning = false
	if app.cancelRun != nil {
		app.cancelRun()
	}
	app.TradeService.Close()
	app.CSVLogger.Close()
	if app.Store != nil {
		app.Store.Close()
	}
	if app.Ledger != nil {
		app.Ledger.Close()
	}
	log.Println("🛑 TitanAlgo Engine Stopped")
}

// RunStrategy starts the single, shared Runner (engine.Runner — see
// internal/engine/runner.go) against symbols. This replaces the old
// duplicate hand-rolled strategy loop that used to live in modes.go
// (WP-9 task 1: exactly one engine/loop implementation, shared with
// cmd/main.go).
func (app *TitanApp) RunStrategy(symbols []string) {
	log.Printf("📋 Trading %d symbols: %v", len(symbols), symbols)

	if err := app.TradeService.Subscribe(symbols); err != nil {
		log.Printf("Failed to subscribe: %v", err)
		return
	}

	stratName := app.Config.Brokers.Trading.ActiveStrategy
	if stratName == "" {
		stratName = "sniper"
	}
	modeLabel := "PAPER"
	if app.tradingMode == "LIVE" {
		modeLabel = "LIVE"
	}

	runnerCfg := engine.RunnerConfig{
		Symbols:              symbols,
		StrategyName:         stratName,
		HistorySize:          app.Config.Engine.HistorySize,
		MinDataPoints:        app.Config.Engine.MinDataPoints,
		PollInterval:         durationMs(app.Config.Engine.PollIntervalMs),
		HeartbeatInterval:    durationSec(app.Config.Engine.HeartbeatIntervalSecs),
		DefaultQuantity:      app.Config.Engine.DefaultQuantity,
		LotSizeFallback:      app.Config.Brokers.Trading.OptionsConfig.LotSize,
		LotSizes:             app.Config.Brokers.Trading.Discovery.LotSizes,
		IndexSymbol:          app.Config.Brokers.Trading.OptionsConfig.IndexSymbol,
		OptionExpiryOverride: app.Config.Brokers.Trading.OptionsConfig.Expiry,
		StrikeStep:           app.Config.Brokers.Trading.OptionsConfig.StrikeStep,
		ModeLabel:            modeLabel,
		KillSentinelPath:     "data/KILL",
		HeartbeatFilePath:    "data/heartbeat",
	}
	runner, err := engine.NewRunner(app.TradingEngine, runnerCfg)
	if err != nil {
		log.Printf("Failed to start runner: %v", err)
		return
	}
	app.Runner = runner

	app.ApiServer.SetControlHooks(api.ControlHooks{
		Pause:          runner.Pause,
		Resume:         runner.Resume,
		KillAndFlatten: runner.KillAndFlatten,
		Status: func() api.EngineStatus {
			st := runner.Status()
			return api.EngineStatus{
				Running: st.Running, Mode: st.Mode, Strategy: st.Strategy,
				Balance: st.Balance, UnrealizedPnL: st.UnrealizedPnL, RealizedPnL: st.RealizedPnL,
				PositionsCount:  st.PositionsCount,
				StopLossEnabled: st.StopLossEnabled, StopLossPercent: st.StopLossPercent,
			}
		},
	})
	app.ApiServer.UpdateStatus(true, app.sessionBalance, 0, 0, nil)

	ctx, cancel := context.WithCancel(context.Background())
	app.mu.Lock()
	app.cancelRun = cancel
	app.mu.Unlock()

	go func() {
		if err := runner.Run(ctx); err != nil {
			log.Printf("🔴 CRITICAL: runner shutdown could not verify a flat position book: %v", err)
		}
	}()
}
