// Command backtest is a thin CLI wrapper around internal/backtest. All
// simulation logic (portfolio, fills, Black-Scholes pricing, costs,
// reporting) lives in that package; this file only does flag parsing,
// candle sourcing (cache-or-fetch), and strategy lookup.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"titan-algo/internal/backtest"
	"titan-algo/internal/broker"
	"titan-algo/internal/strategy"

	"gopkg.in/yaml.v2"
)

// angelConfig is the minimal slice of config.yaml this CLI needs to reach
// the broker when no local candle cache exists yet. Credentials resolve
// from ANGEL_* env vars first (matching the WP-8 convention), YAML only as
// a fallback -- this binary never requires them at all for a cached run.
type angelConfig struct {
	Brokers struct {
		Angel struct {
			ClientCode string `yaml:"client_code"`
			PIN        string `yaml:"pin"`
			APIKey     string `yaml:"api_key"`
			TOTPSecret string `yaml:"totp_secret"`
		} `yaml:"angel"`
	} `yaml:"brokers"`
}

func loadAngelConfig(path string) (*angelConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var cfg angelConfig
	if err := yaml.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	strategyName := flag.String("strategy", "sniper", "strategy name (see -list-strategies)")
	symbol := flag.String("symbol", "NIFTY", "underlying index symbol")
	interval := flag.String("interval", "FIVE_MINUTE", "candle interval (broker API value)")
	lotSize := flag.Int("lotsize", 75, "contract lot size (ST-10/M3: NIFTY default post-Apr-2025; real fix is WP-1 GetLotSize via instrument master, wired by the integration agent)")
	fromStr := flag.String("from", "", "start date YYYY-MM-DD (IST); default 30 days before -to")
	toStr := flag.String("to", "", "end date YYYY-MM-DD (IST); default today")
	csvPath := flag.String("csv", "", "local candle cache CSV path; loaded if present, else fetched from the broker and saved here (empty = always fetch, never cache)")
	configPath := flag.String("config", "config.yaml", "path to config.yaml (only read if a broker fetch is needed)")
	iv := flag.Float64("iv", 0.12, "constant annualized IV used for Black-Scholes repricing (v1 limitation, see CR-9 docs)")
	riskFreeRate := flag.Float64("rate", 0.065, "annualized risk-free rate for Black-Scholes")
	dte := flag.Int("dte", 7, "fallback days-to-expiry when a signal leg doesn't specify Expiry")
	strikeStep := flag.Float64("strikestep", 50, "ATM strike rounding step")
	listStrategies := flag.Bool("list-strategies", false, "print registered strategy names and exit")
	flag.Parse()

	if *listStrategies {
		fmt.Println(strategy.GetAvailableStrategies())
		return
	}

	loc := backtest.IST
	to := time.Now().In(loc)
	if *toStr != "" {
		t, err := time.ParseInLocation("2006-01-02", *toStr, loc)
		if err != nil {
			log.Fatalf("bad -to date %q: %v", *toStr, err)
		}
		to = t
	}
	from := to.AddDate(0, 0, -30)
	if *fromStr != "" {
		t, err := time.ParseInLocation("2006-01-02", *fromStr, loc)
		if err != nil {
			log.Fatalf("bad -from date %q: %v", *fromStr, err)
		}
		from = t
	}
	if !from.Before(to) {
		log.Fatalf("-from (%s) must be before -to (%s)", from.Format("2006-01-02"), to.Format("2006-01-02"))
	}

	fetch := func() ([]backtest.Candle, error) {
		cfg, err := loadAngelConfig(*configPath)
		if err != nil {
			return nil, fmt.Errorf("no cache at %q and couldn't load broker config %q for a live fetch: %w", *csvPath, *configPath, err)
		}
		angel := broker.NewAngelBroker(
			envOr("ANGEL_CLIENT_CODE", cfg.Brokers.Angel.ClientCode),
			envOr("ANGEL_PIN", cfg.Brokers.Angel.PIN),
			envOr("ANGEL_API_KEY", cfg.Brokers.Angel.APIKey),
			envOr("ANGEL_TOTP_SECRET", cfg.Brokers.Angel.TOTPSecret),
		)
		if err := angel.Connect(); err != nil {
			return nil, fmt.Errorf("broker connect failed: %w", err)
		}
		days := int(to.Sub(from).Hours()/24) + 1
		log.Printf("fetching %d days of %s history for %s from broker (no cache found)...", days, *interval, *symbol)
		return angel.FetchHistory(*symbol, *interval, days)
	}

	var candles []backtest.Candle
	var err error
	if *csvPath != "" {
		candles, err = backtest.LoadOrFetch(*csvPath, fetch)
	} else {
		candles, err = fetch()
	}
	if err != nil {
		log.Fatalf("failed to obtain candle data: %v", err)
	}

	candles = filterRange(candles, from, to)
	if len(candles) == 0 {
		log.Fatalf("no candles in range %s -> %s", from.Format("2006-01-02"), to.Format("2006-01-02"))
	}

	strat, err := strategy.Get(*strategyName)
	if err != nil {
		log.Fatalf("failed to load strategy %q: %v", *strategyName, err)
	}

	cfg := backtest.DefaultConfig()
	cfg.Symbol = *symbol
	cfg.LotSize = *lotSize
	cfg.IV = *iv
	cfg.RiskFreeRate = *riskFreeRate
	cfg.DefaultDTEDays = *dte
	cfg.StrikeStep = *strikeStep

	report, err := backtest.Run(candles, strat, cfg)
	if err != nil {
		log.Fatalf("backtest run failed: %v", err)
	}

	fmt.Println(report.String())
}

// filterRange trims candles to [from, to] inclusive, assuming candles are
// already sorted oldest-first (broker/cache convention).
func filterRange(candles []backtest.Candle, from, to time.Time) []backtest.Candle {
	end := to.AddDate(0, 0, 1) // inclusive of the whole -to day
	out := make([]backtest.Candle, 0, len(candles))
	for _, c := range candles {
		if !c.Time.Before(from) && c.Time.Before(end) {
			out = append(out, c)
		}
	}
	return out
}
