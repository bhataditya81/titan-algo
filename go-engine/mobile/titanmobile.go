package mobile

import (
	"log"
	"os"
	"path/filepath"

	"titan-algo/internal/app"
	"titan-algo/internal/config"
)

var currentApp *app.TitanApp

// Start initializes and starts the TitanAlgo trading engine
// dataDir: Android internal storage path for config/logs
// apiKey: API key for local REST communication
func Start(dataDir string, apiKey string) string {
	// Set up logging to a file in dataDir since stdout might be lost on Android
	logFile, err := os.OpenFile(filepath.Join(dataDir, "titan_mobile.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err == nil {
		log.SetOutput(logFile)
	}
	log.Printf("📱 Mobile Engine Starting in %s", dataDir)

	// Create default config if not exists
	configPath := filepath.Join(dataDir, "config.yaml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		log.Println("Creating default config...")
		createDefaultConfig(configPath)
	}

	// Load Config
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Printf("Failed to load config: %v", err)
		return "Error loading config: " + err.Error()
	}

	// Override API Key from argument if provided
	if apiKey != "" {
		cfg.Brokers.Angel.APIKey = apiKey
	}

	// Create App (Default to Paper Request)
	// For mobile, we force paper mode initially for safety
	currentApp = app.NewApp(cfg, "MODE_A", "PAPER", 1000.0)

	// Initialize
	if err := currentApp.Initialize(); err != nil {
		log.Printf("Failed to initialize: %v", err)
		return "Error initializing: " + err.Error()
	}

	// Start
	if err := currentApp.Start(); err != nil {
		log.Printf("Failed to start: %v", err)
		return "Error starting: " + err.Error()
	}

	return "SUCCESS"
}

// Stop halts the engine
func Stop() string {
	if currentApp != nil {
		currentApp.Stop()
		currentApp = nil
		return "Stopped"
	}
	return "Not running"
}

func createDefaultConfig(path string) {
	// NOTE: api_key is intentionally left blank. It must never default to a
	// hardcoded placeholder secret (CR-1: a shared, guessable fallback string
	// reused as a credential is exactly the bug this fixes). The mobile
	// app forces PAPER mode on every Start() call above regardless of what's
	// in this file, so a missing/blank broker API key is safe here; a real
	// key must be supplied explicitly (via the apiKey argument to Start, or
	// by editing the generated file) before any live use.
	defaultYaml := `
brokers:
  angel:
    client_code: ""
    api_key: ""
  trading:
    active_strategy: "sniper"
    symbol_selection: "top_n_volume"
    top_n_symbols: 3
    discovery:
      enabled: true
      indices: ["NIFTY", "BANKNIFTY"]
risk:
  session_balance_limit: 10000.0
  max_drawdown_percent: 5.0
  stop_loss:
    enabled: true
    type: "percentage"
    value: 5.0
engine:
  poll_interval_ms: 2000
`
	// 0600: this file can hold a broker API key once the operator fills one
	// in; it must not be world/group readable.
	os.WriteFile(path, []byte(defaultYaml), 0600)
}
