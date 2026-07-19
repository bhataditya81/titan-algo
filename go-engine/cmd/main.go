// Command titan-algo is the live/paper trading entry point (WP-9
// integration). It wires internal/config (env-first credentials, live-mode
// credential gate), the broker, risk manager, durable state store + ledger,
// and internal/engine's Runner (the single tick loop shared with
// internal/app/titan.go's mobile path — see internal/engine/runner.go) into
// one process, plus the local control API (pause/resume/kill/status).
//
// There is exactly one TradingEngine and one Runner constructed here (WP-9
// task 1) — the old duplicate hand-rolled strategy loop that used to live
// in this file (and its near-twin in internal/app/modes.go) has been
// deleted; both entry points now drive the same engine.Runner.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"titan-algo/internal/broker"
	"titan-algo/internal/cli"
	"titan-algo/internal/config"
	"titan-algo/internal/discovery"
	"titan-algo/internal/engine"
	"titan-algo/internal/ledger"
	"titan-algo/internal/logger"
	"titan-algo/internal/risk"
	"titan-algo/internal/state"
	"titan-algo/internal/strategy"
	"titan-algo/models"

	"titan-algo/internal/api"
)

func main() {
	paperMode := flag.Bool("paper", false, "Enable paper trading mode (uses MockBroker or LivePaperBroker)")
	liveMode := flag.Bool("live", false, "Enable LIVE trading mode (uses Angel One broker) — REAL MONEY")
	sessionBalance := flag.Float64("balance", 1000.0, "Session balance limit in INR")
	searchQuery := flag.String("search", "", "Search for symbols in Instrument Master (e.g., -search 'SILVER')")
	acceptReconcile := flag.Bool("accept-reconcile", false,
		"Acknowledge a startup mismatch between internal state and the broker's live position book and proceed anyway (task 2 / CR-7). Without this flag, a mismatch refuses to start.")
	flag.Parse()

	// Handle Search Mode (no engine/broker session needed).
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

	if *paperMode && *liveMode {
		log.Fatal("Cannot use both -paper and -live flags. Choose one.")
	}
	if !*paperMode && !*liveMode {
		log.Fatal("Must specify either -paper or -live flag")
	}

	// ── Config (task 3): internal/config, not a local duplicate loader ────
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	if *liveMode {
		if err := cfg.ValidateLiveCredentials(); err != nil {
			log.Fatal(err)
		}
	}

	modeLabel := "PAPER"
	ledgerMode := ledger.ModePaper
	if *liveMode {
		modeLabel = "LIVE"
		ledgerMode = ledger.ModeLive
	}

	reader := bufio.NewReader(os.Stdin)
	log.Printf("🧪 %s TRADING MODE ENABLED", modeLabel)
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
	fmt.Printf("  ✅ Session Balance: ₹%.2f\n\n", *sessionBalance)

	if *liveMode {
		log.Println("⚠️  ⚠️  ⚠️  LIVE TRADING MODE ⚠️  ⚠️  ⚠️")
		if !confirmLiveTrading(*sessionBalance) {
			log.Println("Live trading cancelled by user")
			return
		}
	}

	// ── Broker ──────────────────────────────────────────────────────────
	var tradeService broker.TradeService
	if *liveMode {
		tradeService = broker.NewAngelBroker(
			cfg.Brokers.Angel.ClientCode, cfg.Brokers.Angel.PIN,
			cfg.Brokers.Angel.APIKey, cfg.Brokers.Angel.TOTPSecret,
		)
		log.Println("🔴 LIVE TRADING ENABLED")
	} else {
		if cfg.Brokers.Angel.ClientCode != "" && cfg.Brokers.Angel.APIKey != "" {
			log.Println("✨ Using LIVE DATA for Paper Trading (Angel One Credentials Found)")
			angelBroker := broker.NewAngelBroker(
				cfg.Brokers.Angel.ClientCode, cfg.Brokers.Angel.PIN,
				cfg.Brokers.Angel.APIKey, cfg.Brokers.Angel.TOTPSecret,
			)
			tradeService = broker.NewLivePaperBroker(angelBroker, *sessionBalance)
		} else {
			log.Println("🎲 Using MOCK DATA for Paper Trading (No Angel One Credentials)")
			tradeService = broker.NewMockBroker(*sessionBalance)
		}
	}

	logDir := "logs"
	if *liveMode {
		logDir = "logs/live"
	}
	csvLogger, err := logger.NewCSVLogger(logDir)
	if err != nil {
		log.Fatalf("Failed to create CSV logger: %v", err)
	}
	defer csvLogger.Close()

	riskMgr := risk.NewManager(
		cfg.Risk.MaxDrawdownPercent, *sessionBalance, cfg.Risk.Brokerage,
		risk.StopLossConfig{
			Enabled: cfg.Risk.StopLoss.Enabled, Type: cfg.Risk.StopLoss.Type,
			Value: cfg.Risk.StopLoss.Value, Trailing: cfg.Risk.StopLoss.Trailing,
			TrailingDistance: cfg.Risk.StopLoss.TrailingDistance,
		},
		cfg.Risk.MaxOrdersPerMin,
	)
	riskMgr.KillSwitch = cfg.Risk.KillSwitchEnabled

	if riskMgr.KillSwitch {
		log.Println("⚠️  WARNING: KILL SWITCH IS ENABLED IN CONFIG — attempting emergency liquidation, then halting")
		if *liveMode {
			if err := tradeService.Connect(); err != nil {
				log.Printf("❌ Failed to connect for liquidation: %v", err)
			} else {
				positions := tradeService.GetPositions()
				if len(positions) == 0 {
					log.Println("✅ No open positions found on server.")
				}
				for symbol, pos := range positions {
					side := broker.Sell
					if pos.Side == broker.Sell {
						side = broker.Buy
					}
					log.Printf("🔥 Closing %s position: %s %d", symbol, pos.Side, pos.Quantity)
					order := broker.Order{Symbol: symbol, Quantity: pos.Quantity, Side: side, OrderType: broker.Market, Intent: broker.IntentReduceOnly}
					if _, err := tradeService.PlaceOrder(order); err != nil {
						log.Printf("❌ FAILED to close %s: %v", symbol, err)
					} else {
						log.Printf("✅ %s Liquidated", symbol)
					}
				}
			}
		} else {
			log.Println("ℹ️  Paper Trading Mode: No existing real positions to close.")
		}
		log.Println("🛑 Exiting due to Kill Switch.")
		os.Exit(0)
	}

	// ── Durable state + ledger (tasks 2, 11) ───────────────────────────
	statePath := cfg.State.DBPath
	if statePath == "" {
		statePath = state.DefaultDBPath
	}
	store, err := state.Open(statePath)
	if err != nil {
		log.Fatalf("Failed to open state store: %v", err)
	}
	defer store.Close()

	ledgerPath := cfg.Ledger.DBPath
	if ledgerPath == "" {
		ledgerPath = ledger.DefaultDBPath
	}
	ledgerDB, err := ledger.Open(ledgerPath)
	if err != nil {
		log.Fatalf("Failed to open ledger: %v", err)
	}
	defer ledgerDB.Close()

	tradingEngine := engine.NewTradingEngine(riskMgr, tradeService, csvLogger, store, ledgerDB, ledgerMode)

	// ── Connect broker ──────────────────────────────────────────────────
	if err := tradeService.Connect(); err != nil {
		log.Fatalf("Failed to connect to broker: %v", err)
	}
	defer tradeService.Close()

	// ── Recover + reconcile (task 2 / CR-7 / CR-8) ─────────────────────
	openPositions, _, err := state.RecoverSession(store)
	if err != nil {
		log.Fatalf("Failed to recover session state: %v", err)
	}
	brokerPositions := make([]models.Position, 0, len(tradeService.GetPositions()))
	for _, p := range tradeService.GetPositions() {
		brokerPositions = append(brokerPositions, models.Position{
			Symbol: p.Symbol, Quantity: p.Quantity, Side: models.PositionSide(p.Side),
			EntryPrice: p.AveragePrice, Status: models.PositionOpen,
		})
	}
	report := state.Reconcile(openPositions, brokerPositions)
	if !report.Clean() {
		log.Printf("🚨 RECONCILE MISMATCH at startup: %d phantom(s), %d orphan(s), %d quantity mismatch(es)",
			len(report.Phantoms), len(report.Orphans), len(report.QuantityMismatches))
		for _, it := range report.Phantoms {
			log.Printf("   PHANTOM (internal only): %s", it.Symbol)
		}
		for _, it := range report.Orphans {
			log.Printf("   ORPHAN (broker only, UNMONITORED): %s", it.Symbol)
		}
		for _, it := range report.QuantityMismatches {
			log.Printf("   QTY MISMATCH: %s internal=%d broker=%d", it.Symbol, it.InternalNetQty, it.BrokerNetQty)
		}
		if !*acceptReconcile {
			log.Fatal("🔴 Refusing to start with an unreconciled position book. Resolve manually or re-run with -accept-reconcile to proceed at your own risk.")
		}
		log.Println("⚠️  Proceeding despite mismatch per -accept-reconcile — VERIFY positions manually.")
	} else {
		log.Printf("✅ Reconcile clean: %d matched position(s)", len(report.Matched))
	}

	// ── Instrument master (task 9; best-effort load, but every consumer
	// below fails closed if it's actually needed and unavailable rather
	// than silently guessing) ───────────────────────────────────────────
	instruments := broker.NewInstrumentManager()
	if err := instruments.LoadInstruments(); err != nil {
		log.Printf("⚠️  instrument master load failed (expiry/lot-size resolution will require an explicit config override, or fail closed): %v", err)
		instruments = nil
	}

	// ── Symbol discovery (interactive) ──────────────────────────────────
	var sessionConfig *cli.SessionConfig
	var symbols []string
	usingFallbackSymbols := false

	if cfg.Brokers.Trading.Discovery.QuickStartEnabled {
		if savedSession, ok := cli.QuickStart(reader); ok {
			sessionConfig = savedSession
			symbols = savedSession.SelectedChains
			*sessionBalance = savedSession.Balance
			log.Printf("✨ Quick Start: Using saved session (Strategy: %s, Balance: ₹%.2f)", savedSession.Strategy, savedSession.Balance)
		}
	}
	if sessionConfig == nil {
		if cfg.Brokers.Trading.Discovery.Enabled {
			indices := cfg.Brokers.Trading.Discovery.Indices
			if len(indices) == 0 {
				indices = discovery.GetDefaultIndices()
			}
			sd := discovery.NewSymbolDiscovery(tradeService, indices, cfg.Brokers.Trading.Discovery.LotSizes, *sessionBalance)
			topChains, err := sd.ScanTopChains(cfg.Brokers.Trading.Discovery.TopChainsCount)
			if err != nil {
				log.Printf("⚠️  Discovery failed: %v. Using fallback symbols.", err)
				symbols = cfg.Brokers.Trading.FallbackSymbols
				usingFallbackSymbols = true
			} else {
				sessionConfig, err = cli.RunInteractiveSetupWithBalance(reader, topChains, *sessionBalance)
				if err != nil {
					log.Fatalf("Configuration cancelled: %v", err)
				}
				symbols = sessionConfig.SelectedChains
				*sessionBalance = sessionConfig.Balance
				cfg.Brokers.Trading.ActiveStrategy = sessionConfig.Strategy
				cli.SaveSession(sessionConfig)
			}
		} else {
			symbols = cfg.Brokers.Trading.FallbackSymbols
			usingFallbackSymbols = true
		}
	}
	if len(symbols) == 0 {
		log.Fatal("❌ No trading symbols selected. Cannot proceed.")
	}
	// FallbackSymbols is a hand-maintained config list (config.yaml); the
	// original audit found it could silently contain an already-expired
	// contract. Validate every derivative symbol's real expiry from the
	// instrument master before trading any of them — never trust a static
	// list to still be current.
	if usingFallbackSymbols {
		if err := validateFallbackSymbols(instruments, symbols); err != nil {
			log.Fatalf("🔴 Refusing to trade fallback_symbols: %v", err)
		}
	}
	log.Printf("📋 Trading %d symbols: %v", len(symbols), symbols)

	if err := tradeService.Subscribe(symbols); err != nil {
		log.Fatalf("Failed to subscribe: %v", err)
	}

	// R2-1/R2-INT wiring (G-7): start the WebSocket 2.0 live feed for these
	// symbols, additive to the REST polling Subscribe just did above. Best
	// effort — if the broker doesn't implement ExtendedTradeService
	// (MockBroker) or the feed never connects, REST polling keeps serving
	// prices exactly as before.
	if ext, ok := tradeService.(broker.ExtendedTradeService); ok {
		if err := ext.SubscribeLive(symbols); err != nil {
			log.Printf("⚠️ WS live feed subscribe failed for %v: %v (falling back to REST polling)", symbols, err)
		} else {
			log.Printf("📡 WS live feed subscribed for %d symbol(s)", len(symbols))
		}
	}

	// Re-create the risk manager with the final, user-confirmed balance
	// (mirrors the pre-remediation behavior of re-sizing after discovery).
	riskMgr = risk.NewManager(
		cfg.Risk.MaxDrawdownPercent, *sessionBalance, cfg.Risk.Brokerage,
		risk.StopLossConfig{
			Enabled: cfg.Risk.StopLoss.Enabled, Type: cfg.Risk.StopLoss.Type,
			Value: cfg.Risk.StopLoss.Value, Trailing: cfg.Risk.StopLoss.Trailing,
			TrailingDistance: cfg.Risk.StopLoss.TrailingDistance,
		},
		cfg.Risk.MaxOrdersPerMin,
	)
	riskMgr.KillSwitch = cfg.Risk.KillSwitchEnabled
	tradingEngine = engine.NewTradingEngine(riskMgr, tradeService, csvLogger, store, ledgerDB, ledgerMode)

	stratName := cfg.Brokers.Trading.ActiveStrategy
	if stratName == "" {
		stratName = "sniper"
	}

	runnerCfg := engine.RunnerConfig{
		Symbols:              symbols,
		StrategyName:         stratName,
		HistorySize:          cfg.Engine.HistorySize,
		MinDataPoints:        cfg.Engine.MinDataPoints,
		PollInterval:         time.Duration(cfg.Engine.PollIntervalMs) * time.Millisecond,
		HeartbeatInterval:    time.Duration(cfg.Engine.HeartbeatIntervalSecs) * time.Second,
		DefaultQuantity:      cfg.Engine.DefaultQuantity,
		LotSizeFallback:      cfg.Brokers.Trading.OptionsConfig.LotSize,
		LotSizes:             cfg.Brokers.Trading.Discovery.LotSizes,
		IndexSymbol:          cfg.Brokers.Trading.OptionsConfig.IndexSymbol,
		OptionExpiryOverride: cfg.Brokers.Trading.OptionsConfig.Expiry,
		StrikeStep:           cfg.Brokers.Trading.OptionsConfig.StrikeStep,
		ModeLabel:            modeLabel,
		KillSentinelPath:     "data/KILL",
		HeartbeatFilePath:    "data/heartbeat",
		Alert:                buildAlertFunc(),
		Instruments:          instruments,
		HolidayFile:          cfg.HolidayFile,
	}
	runner, err := engine.NewRunner(tradingEngine, runnerCfg)
	if err != nil {
		log.Fatalf("Failed to start runner: %v", err)
	}

	// ── Control API (task 4) ─────────────────────────────────────────────
	apiServer := api.NewServer(8080, cfg.API.Token)
	apiServer.SetRateLimit(cfg.API.RateLimitRPS, cfg.API.RateLimitBurst)
	apiServer.SetWSMaxConns(cfg.API.WSMaxConns)
	// G-13(e): wire the previously-dead TLS knob. Empty/empty (the default)
	// is a documented no-op inside SetTLS — Start() serves plaintext HTTP on
	// localhost exactly as before for anyone who hasn't set both fields.
	apiServer.SetTLS(cfg.API.TLSCertFile, cfg.API.TLSKeyFile)
	apiServer.SetControlHooks(api.ControlHooks{
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
				DiscoveryEnabled: cfg.Brokers.Trading.Discovery.Enabled,
				Indices:          cfg.Brokers.Trading.Discovery.Indices,
			}
		},
	})
	go func() {
		if err := apiServer.Start(); err != nil {
			log.Printf("⚠️ API server error: %v", err)
		}
	}()

	// ── Run until signalled, then graceful shutdown (task 12) ──────────
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Println("⚠️  Press Ctrl+C to stop trading")
	if err := runner.Run(ctx); err != nil {
		log.Printf("🔴 CRITICAL: shutdown did not verify a flat position book: %v", err)
		os.Exit(1)
	}
	log.Println("👋 Clean exit.")
}

// validateFallbackSymbols rejects any option/futures symbol in a
// hand-maintained fallback list whose real expiry (from the instrument
// master) has already passed, or that the instrument master can't confirm
// at all. fallback_symbols is a static config list an operator edits by
// hand; the original production-readiness audit found it could silently
// contain an already-expired contract (CR/EX findings) -- this must never
// be trusted blind. Plain equity/index symbols (no CE/PE/FUT suffix) have
// no expiry and pass through unchecked.
func validateFallbackSymbols(instruments *broker.InstrumentManager, symbols []string) error {
	today := time.Now().In(strategy.IST)
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location())

	for _, sym := range symbols {
		upper := strings.ToUpper(sym)
		isDerivative := strings.HasSuffix(upper, "CE") || strings.HasSuffix(upper, "PE") || strings.HasSuffix(upper, "FUT")
		if !isDerivative {
			continue
		}
		if instruments == nil {
			return fmt.Errorf("cannot verify expiry for derivative fallback symbol %q: instrument master unavailable", sym)
		}
		inst, err := instruments.GetInstrument(sym)
		if err != nil {
			return fmt.Errorf("fallback symbol %q not found in instrument master: %w", sym, err)
		}
		expiry, err := broker.ParseExpiry(inst.Expiry)
		if err != nil {
			return fmt.Errorf("fallback symbol %q has unparseable expiry %q: %w", sym, inst.Expiry, err)
		}
		if expiry.Before(today) {
			return fmt.Errorf("fallback symbol %q expired on %s -- refusing to trade a stale contract", sym, expiry.Format("02-Jan-2006"))
		}
	}
	return nil
}

// buildAlertFunc wires an optional Telegram alert sink (task 13):
// TITAN_TG_TOKEN / TITAN_TG_CHAT env vars. No-op (nil) if either is unset —
// callers must nil-check before invoking.
func buildAlertFunc() engine.AlertFunc {
	token := os.Getenv("TITAN_TG_TOKEN")
	chat := os.Getenv("TITAN_TG_CHAT")
	if token == "" || chat == "" {
		return nil
	}
	client := &http.Client{Timeout: 5 * time.Second}
	return func(event, detail string) {
		text := fmt.Sprintf("🚨 TitanAlgo [%s]: %s", event, detail)
		endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage?chat_id=%s&text=%s",
			token, url.QueryEscape(chat), url.QueryEscape(text))
		resp, err := client.Get(endpoint)
		if err != nil {
			log.Printf("⚠️ telegram alert failed: %v", err)
			return
		}
		resp.Body.Close()
	}
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
	fmt.Println("║  ✓ Max drawdown protection                                ║")
	fmt.Println("║  ✓ Order rate limiting                                    ║")
	fmt.Println("║  ✓ All trades logged to CSV + ledger                      ║")
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
