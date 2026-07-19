// Command backtest is a thin CLI wrapper around internal/backtest. All
// simulation logic (portfolio, fills, Black-Scholes pricing, costs,
// reporting) lives in that package; this file only does flag parsing,
// candle sourcing (cache-or-fetch), and strategy lookup.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"titan-algo/internal/backtest"
	"titan-algo/internal/broker"
	"titan-algo/internal/strategy"

	"gopkg.in/yaml.v2"
)

// R2-2 has landed strategy.GetWithParams(name, map[string]float64) exactly
// matching the seam internal/backtest/params.go was built against -- wire
// it in for real (was a documented TODO/fallback while R2-2 was in
// flight; see docs/reports/R2-3-REPORT.md).
func init() {
	backtest.StrategyWithParams = strategy.GetWithParams
}

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

// parseParams parses "-params key=val,key2=val2" into a map (G-5).
func parseParams(s string) (map[string]float64, error) {
	out := map[string]float64{}
	s = strings.TrimSpace(s)
	if s == "" {
		return out, nil
	}
	for _, pair := range strings.Split(s, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("bad -params entry %q: expected key=val", pair)
		}
		key := strings.TrimSpace(kv[0])
		val, err := strconv.ParseFloat(strings.TrimSpace(kv[1]), 64)
		if err != nil {
			return nil, fmt.Errorf("bad -params value in %q: %w", pair, err)
		}
		out[key] = val
	}
	return out, nil
}

// lotSizeFromInstrumentCache looks up symbol's lot size from the most recent
// cached instrument-master JSON file in dir (the same
// data/instruments/scripmaster_YYYY-MM-DD.json cache internal/broker's
// InstrumentManager writes -- see instruments.go's cachePathForToday/
// buildIndex). Returns ok=false (never a guessed lot size) if dir doesn't
// exist, no cache file is found, or the symbol/lot size can't be resolved
// from it -- callers fall back to -lotsize.
func lotSizeFromInstrumentCache(dir, symbol string) (int, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, false
	}
	var latest string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "scripmaster_") || !strings.HasSuffix(name, ".json") {
			continue
		}
		if name > latest {
			latest = name
		}
	}
	if latest == "" {
		return 0, false
	}

	raw, err := os.ReadFile(filepath.Join(dir, latest))
	if err != nil {
		return 0, false
	}
	var instruments []broker.Instrument
	if err := json.Unmarshal(raw, &instruments); err != nil {
		return 0, false
	}
	for _, inst := range instruments {
		if inst.Symbol != symbol && !strings.EqualFold(inst.Name, symbol) {
			continue
		}
		ls := strings.TrimSpace(inst.LotSize)
		f, err := strconv.ParseFloat(ls, 64)
		if err != nil || f <= 0 {
			continue
		}
		return int(f), true
	}
	return 0, false
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
	paramsFlag := flag.String("params", "", "comma-separated key=val strategy parameters, e.g. rsi_period=14,ema_fast=9 (G-5; passed to strategy.GetWithParams once R2-2 lands -- see docs/reports/R2-3-REPORT.md)")
	costMultiplier := flag.Float64("cost-multiplier", 1.0, "scales all modeled transaction costs (brokerage/STT/txn/GST/stamp/spread) by this factor -- for cost-stress testing (replaces WP-10's manual arithmetic workaround)")
	instrumentCacheDir := flag.String("instrument-cache-dir", "data/instruments", "directory of cached instrument-master JSON (scripmaster_YYYY-MM-DD.json, written by internal/broker.InstrumentManager or cmd/fetchdata); if -symbol's lot size is found there it overrides -lotsize")
	optionCSV := flag.String("option-csv", "", "optional local option-candle CSV (fetched by cmd/fetchdata's option-chain mode) for an INFORMATIONAL implied-IV coverage summary; NOT applied to leg repricing this round (see the constant-IV banner)")
	optionStrike := flag.Float64("option-strike", 0, "strike price of the contract in -option-csv (required if -option-csv is set)")
	optionExpiryStr := flag.String("option-expiry", "", "expiry date YYYY-MM-DD of the contract in -option-csv (required if -option-csv is set)")
	optionType := flag.String("option-type", "CE", "CE or PE, matching -option-csv's contract")
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

	paramsMap, err := parseParams(*paramsFlag)
	if err != nil {
		log.Fatalf("bad -params: %v", err)
	}

	strat, err := backtest.ResolveStrategy(*strategyName, paramsMap)
	if errors.Is(err, backtest.ErrParamsUnsupported) {
		log.Printf("WARNING: -params=%q requested but strategy parameterization isn't wired yet (%v) -- running %q with its compiled-in defaults, params IGNORED this run", *paramsFlag, err, *strategyName)
		strat, err = strategy.Get(*strategyName)
	}
	if err != nil {
		log.Fatalf("failed to load strategy %q: %v", *strategyName, err)
	}

	cfg := backtest.DefaultConfig()
	cfg.Symbol = *symbol
	cfg.LotSize = *lotSize
	if ls, ok := lotSizeFromInstrumentCache(*instrumentCacheDir, *symbol); ok {
		log.Printf("lot size for %s resolved from instrument cache (%s): %d (overrides -lotsize=%d)", *symbol, *instrumentCacheDir, ls, *lotSize)
		cfg.LotSize = ls
	}
	cfg.IV = *iv
	cfg.RiskFreeRate = *riskFreeRate
	cfg.DefaultDTEDays = *dte
	cfg.StrikeStep = *strikeStep

	report, err := backtest.Run(candles, strat, cfg)
	if err != nil {
		log.Fatalf("backtest run failed: %v", err)
	}
	backtest.ScaleCosts(report, *costMultiplier)

	fmt.Println(backtest.ConstantIVBanner(cfg.IV))
	if *optionCSV != "" {
		printOptionIVCoverage(*optionCSV, *optionStrike, *optionExpiryStr, *optionType, candles, cfg, loc)
	}
	fmt.Println(report.String())
}

// printOptionIVCoverage loads a fetched option-candle CSV and prints an
// informational implied-IV coverage summary (G-4). This does NOT feed back
// into the simulation above -- see ConstantIVBanner's doc comment for why
// (engine.go's leg repricing is owned by R2-2 this round).
func printOptionIVCoverage(path string, strike float64, expiryStr, optType string, underlying []backtest.Candle, cfg backtest.Config, loc *time.Location) {
	if strike <= 0 || expiryStr == "" {
		log.Printf("WARNING: -option-csv given without -option-strike/-option-expiry -- skipping IV coverage summary")
		return
	}
	expiry, err := time.ParseInLocation("2006-01-02", expiryStr, loc)
	if err != nil {
		log.Printf("WARNING: bad -option-expiry %q: %v -- skipping IV coverage summary", expiryStr, err)
		return
	}
	expiry = time.Date(expiry.Year(), expiry.Month(), expiry.Day(), 15, 30, 0, 0, loc)

	optionCandles, err := backtest.LoadCandlesCSV(path)
	if err != nil {
		log.Printf("WARNING: couldn't load -option-csv %q: %v -- skipping IV coverage summary", path, err)
		return
	}

	kind := backtest.CallOption
	if strings.EqualFold(optType, "PE") {
		kind = backtest.PutOption
	}

	series := backtest.BuildIVSeries(underlying, optionCandles, strike, cfg.RiskFreeRate, expiry, kind)
	stats := backtest.SummarizeIVSeries(series)
	fmt.Printf("\n[informational] implied IV from %s (strike=%.2f, expiry=%s, %s): %d bars matched, mean=%.2f%% min=%.2f%% max=%.2f%%\n"+
		"  -- NOT applied to the repricing above (constant IV still governs); this is coverage/sanity info only.\n\n",
		path, strike, expiryStr, optType, stats.Count, stats.Mean*100, stats.Min*100, stats.Max*100)
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
