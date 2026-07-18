package config

import (
	"os"
	"titan-algo/internal/risk"

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

func Load(filename string) (*Config, error) {
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
