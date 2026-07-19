// Package config loads and validates TitanAlgo configuration.
//
// Credential resolution policy (WP-8, audit CR-1):
//
//	Every credential field resolves from an environment variable FIRST and
//	falls back to the YAML value only when the env var is empty/unset.
//
//	Field                     Env var
//	brokers.angel.client_code ANGEL_CLIENT_CODE
//	brokers.angel.pin         ANGEL_PIN
//	brokers.angel.api_key     ANGEL_API_KEY
//	brokers.angel.password    ANGEL_API_SECRET   (YAML key kept as "password" for
//	                                              backward compatibility; also
//	                                              accepts "api_secret")
//	brokers.angel.totp_secret ANGEL_TOTP_SECRET
//	api.token                 TITAN_API_TOKEN
//
// Live-mode hard gate: when the process is in live mode (TITAN_MODE=live, or
// the process was started with a -live/--live flag), Load FAILS with an error
// if any broker credential came from YAML instead of an env var, or is missing
// entirely. This is a safety gate, not a warning — plaintext credentials in
// config.yaml are treated as burned (audit CR-1). api.token from YAML is also
// rejected in live mode (an empty token is fine: the API server generates a
// random one).
//
// Engine block location: historically config.yaml nested `engine:` under
// `brokers:` while the struct parsed it as top-level, so user edits were
// silently ignored (audit EX-9). Both locations are now accepted; top-level is
// preferred and the nested location logs a deprecation notice.
package config

import (
	"fmt"
	"log"
	"os"
	"strings"

	"titan-algo/internal/risk"

	"gopkg.in/yaml.v2"
)

// Credential source values, exposed for callers (WP-9) that want to re-check
// the credential policy at a different point in the lifecycle.
const (
	SourceEnv     = "env"
	SourceYAML    = "yaml"
	SourceMissing = "missing"
)

// Env var names — single source of truth for scripts/docs/tests.
const (
	EnvClientCode = "ANGEL_CLIENT_CODE"
	EnvPIN        = "ANGEL_PIN"
	EnvAPIKey     = "ANGEL_API_KEY"
	EnvAPISecret  = "ANGEL_API_SECRET"
	EnvTOTPSecret = "ANGEL_TOTP_SECRET"
	EnvAPIToken   = "TITAN_API_TOKEN"
	EnvMode       = "TITAN_MODE"
)

// EngineConfig holds engine-loop tuning parameters. It is accepted at the
// top level (`engine:`, preferred) and, deprecated, nested under `brokers:`.
type EngineConfig struct {
	HistorySize           int `yaml:"history_size"`
	MinDataPoints         int `yaml:"min_data_points"`
	PollIntervalMs        int `yaml:"poll_interval_ms"`
	HeartbeatIntervalSecs int `yaml:"heartbeat_interval_seconds"`
	DefaultQuantity       int `yaml:"default_quantity"`
}

// AngelCredentials are the Angel One SmartAPI credentials. All fields are
// env-first (see package doc). Password is the API secret; the YAML key
// "password" is retained for backward compatibility with existing configs,
// and "api_secret" is accepted as an alias.
type AngelCredentials struct {
	ClientCode string `yaml:"client_code"`
	PIN        string `yaml:"pin"`
	APIKey     string `yaml:"api_key"`
	Password   string `yaml:"password"`   // Angel API secret (env: ANGEL_API_SECRET)
	APISecret  string `yaml:"api_secret"` // alias for password; merged in Load
	TOTPSecret string `yaml:"totp_secret"`
}

// APIConfig configures the local REST/WebSocket control server (WP-4).
type APIConfig struct {
	// BindAddr is the listen address. Default "127.0.0.1:8080". Never bind
	// 0.0.0.0 without TLS.
	BindAddr string `yaml:"bind_addr"`
	// AllowedOrigins is the WebSocket/CORS Origin allowlist. Empty = reject
	// browser origins (native apps send no Origin and are unaffected).
	AllowedOrigins []string `yaml:"allowed_origins"`
	// CORSAllowedOrigins is the REST CORS allowlist (separate from the WS
	// Origin allowlist above). Empty = no CORS headers (fine for the
	// mobile-only deployment; WP-4). Requested by WP-4's report.
	CORSAllowedOrigins []string `yaml:"cors_allowed_origins"`
	// Token authenticates REST (X-API-Key header) and WS (?token=) calls.
	// Env-first via TITAN_API_TOKEN. Empty → the API server generates a
	// random token at startup. MUST NOT be the broker API key.
	Token string `yaml:"token"`
	// TLSCertFile / TLSKeyFile enable HTTPS when both are set (optional).
	// Requested by WP-4's report.
	TLSCertFile string `yaml:"tls_cert_file"`
	TLSKeyFile  string `yaml:"tls_key_file"`
}

// StateConfig configures the durable state store (WP-3).
type StateConfig struct {
	DBPath string `yaml:"db_path"` // default "data/titan_state.db"
}

// LedgerConfig configures the append-only trade ledger (WP-5).
type LedgerConfig struct {
	DBPath string `yaml:"db_path"` // default "data/titan_ledger.db"
}

// Config is the parsed application configuration.
type Config struct {
	App struct {
		Name  string `yaml:"name"`
		Debug bool   `yaml:"debug"`
	} `yaml:"app"`

	Brokers struct {
		Angel   AngelCredentials `yaml:"angel"`
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
		// DeprecatedEngine is the legacy nested location of the engine
		// block (`brokers.engine`). Parsed for backward compatibility;
		// top-level `engine:` wins. Do not add new fields here.
		DeprecatedEngine *EngineConfig `yaml:"engine"`
	} `yaml:"brokers"`

	Risk struct {
		SessionBalanceLimit float64              `yaml:"session_balance_limit"`
		MaxDrawdownPercent  float64              `yaml:"max_drawdown_percent"`
		MaxOrdersPerMin     int                  `yaml:"max_orders_per_min"` // WP-2/EX-3; default 20
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

	Engine EngineConfig `yaml:"engine"` // preferred (top-level) location

	API    APIConfig    `yaml:"api"`
	State  StateConfig  `yaml:"state"`
	Ledger LedgerConfig `yaml:"ledger"`

	// credentialSources records where each credential value came from
	// ("env" | "yaml" | "missing"), keyed by env var name.
	credentialSources map[string]string
}

// CredentialSources returns a copy of the credential provenance map, keyed by
// env var name (ANGEL_CLIENT_CODE, ..., TITAN_API_TOKEN). Values are
// SourceEnv, SourceYAML or SourceMissing.
func (c *Config) CredentialSources() map[string]string {
	out := make(map[string]string, len(c.credentialSources))
	for k, v := range c.credentialSources {
		out[k] = v
	}
	return out
}

// IsLiveMode reports whether this process is (or is being started in) live
// trading mode: TITAN_MODE=live (case-insensitive) or a -live/--live CLI flag.
func IsLiveMode() bool {
	if strings.EqualFold(os.Getenv(EnvMode), "live") {
		return true
	}
	for _, a := range os.Args[1:] {
		if a == "-live" || a == "--live" {
			return true
		}
	}
	return false
}

// ValidateLiveCredentials enforces the live-mode credential policy:
//   - every broker credential MUST come from an env var (yaml or missing → error)
//   - api.token MUST NOT come from YAML (missing is fine — the API server
//     generates a random token)
//
// Load calls this automatically when IsLiveMode() is true; WP-9 should also
// call it explicitly once the definitive trading mode is known (e.g. after
// flag parsing), since config may be loaded before flags are inspected.
func (c *Config) ValidateLiveCredentials() error {
	var problems []string
	for _, env := range []string{EnvClientCode, EnvPIN, EnvAPIKey, EnvAPISecret, EnvTOTPSecret} {
		switch c.credentialSources[env] {
		case SourceYAML:
			problems = append(problems, fmt.Sprintf("%s came from config.yaml — live mode requires it as an environment variable (plaintext YAML credentials are treated as compromised)", env))
		case SourceMissing:
			problems = append(problems, fmt.Sprintf("%s is not set (env var required in live mode)", env))
		}
	}
	if c.credentialSources[EnvAPIToken] == SourceYAML {
		problems = append(problems, fmt.Sprintf("%s (api.token) came from config.yaml — set the env var or leave it empty to auto-generate", EnvAPIToken))
	}
	if len(problems) > 0 {
		return fmt.Errorf("LIVE MODE CREDENTIAL POLICY VIOLATION — refusing to start:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// resolveCredential applies env-first resolution and records provenance.
func (c *Config) resolveCredential(envName, yamlValue string) string {
	if v := os.Getenv(envName); v != "" {
		c.credentialSources[envName] = SourceEnv
		return v
	}
	if yamlValue != "" {
		c.credentialSources[envName] = SourceYAML
		return yamlValue
	}
	c.credentialSources[envName] = SourceMissing
	return ""
}

// Load reads, parses, resolves and validates the configuration file.
func Load(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	return parse(data)
}

// parse is Load without the file read (used by tests).
func parse(data []byte) (*Config, error) {
	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	config.credentialSources = make(map[string]string, 6)

	// Merge "api_secret" alias into Password before env resolution.
	if config.Brokers.Angel.Password == "" && config.Brokers.Angel.APISecret != "" {
		config.Brokers.Angel.Password = config.Brokers.Angel.APISecret
	}
	config.Brokers.Angel.APISecret = "" // canonical field is Password

	// Env-first credential resolution (CR-1).
	config.Brokers.Angel.ClientCode = config.resolveCredential(EnvClientCode, config.Brokers.Angel.ClientCode)
	config.Brokers.Angel.PIN = config.resolveCredential(EnvPIN, config.Brokers.Angel.PIN)
	config.Brokers.Angel.APIKey = config.resolveCredential(EnvAPIKey, config.Brokers.Angel.APIKey)
	config.Brokers.Angel.Password = config.resolveCredential(EnvAPISecret, config.Brokers.Angel.Password)
	config.Brokers.Angel.TOTPSecret = config.resolveCredential(EnvTOTPSecret, config.Brokers.Angel.TOTPSecret)
	config.API.Token = config.resolveCredential(EnvAPIToken, config.API.Token)

	// Engine block nesting fix (EX-9): prefer top-level `engine:`; fall back
	// to the deprecated `brokers.engine` location with a notice.
	nested := config.Brokers.DeprecatedEngine
	if config.Engine == (EngineConfig{}) {
		if nested != nil && *nested != (EngineConfig{}) {
			config.Engine = *nested
			log.Printf("config: DEPRECATED — `engine:` block found nested under `brokers:`; " +
				"move it to the top level of config.yaml (nested location still honored for now)")
		}
	} else if nested != nil && *nested != (EngineConfig{}) {
		log.Printf("config: `engine:` present both at top level and nested under `brokers:`; " +
			"using the top-level block, ignoring the deprecated nested one")
	}
	config.Brokers.DeprecatedEngine = nil

	// Ensure fallback symbols are initialized if missing in yaml
	if config.Brokers.Trading.FallbackSymbols == nil {
		config.Brokers.Trading.FallbackSymbols = []string{}
	}

	// Set discovery defaults
	if config.Brokers.Trading.Discovery.TopChainsCount == 0 {
		config.Brokers.Trading.Discovery.TopChainsCount = 10
	}

	// Engine defaults
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

	// Risk defaults
	if config.Risk.MaxOrdersPerMin == 0 {
		config.Risk.MaxOrdersPerMin = 20
	}

	// API / State / Ledger defaults
	if config.API.BindAddr == "" {
		config.API.BindAddr = "127.0.0.1:8080"
	}
	if config.API.AllowedOrigins == nil {
		config.API.AllowedOrigins = []string{}
	}
	if config.API.CORSAllowedOrigins == nil {
		config.API.CORSAllowedOrigins = []string{}
	}
	if config.State.DBPath == "" {
		config.State.DBPath = "data/titan_state.db"
	}
	if config.Ledger.DBPath == "" {
		config.Ledger.DBPath = "data/titan_ledger.db"
	}

	// Hard live-mode gate (CR-1). WP-9 should re-check after flag parsing.
	if IsLiveMode() {
		if err := config.ValidateLiveCredentials(); err != nil {
			return nil, err
		}
	}

	return &config, nil
}
