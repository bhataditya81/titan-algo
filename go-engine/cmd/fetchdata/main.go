// Command fetchdata pulls real historical candle data from Angel One and
// caches it in exactly the CSV format internal/backtest/cache.go reads, so
// the backtest/validation harness (WP-7/WP-10/R2-3) can finally run against
// real NSE/BSE market data instead of synthetic data (G-3).
//
// READ-ONLY GUARD (hard constraint, R2-3): this binary talks to the broker
// ONLY through the historyFetcher interface below (Connect + FetchHistory).
// That interface has no PlaceOrder/PlaceStopLossOrder/CancelOrder method, so
// there is NO code path in this file that can reach an order-placement
// broker method even though the concrete *broker.AngelBroker passed into it
// has them. Do not widen this interface to add order methods.
//
// Credentials resolve from ANGEL_* environment variables ONLY (never
// config.yaml) -- matching the WP-8 live-mode gate philosophy (internal/
// config's env-first policy). If any required credential is missing from
// the environment, this refuses to run; if config.yaml appears to carry
// broker credentials at all, it refuses even more loudly (see resolveCreds).
//
// This tool has been run only against a local httptest fake server in this
// repository (see main_test.go). Running it against real Angel One
// endpoints with real, rotated credentials is a human-supervised task
// (H-4 in docs/PRODUCTION_GAPS_R2.md) -- see docs/reports/R2-3-REPORT.md for
// exact usage instructions.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"titan-algo/internal/backtest"
	"titan-algo/internal/broker"
	"titan-algo/internal/config"

	"golang.org/x/time/rate"
	"gopkg.in/yaml.v2"
)

// historyFetcher is the ONLY seam this binary uses to reach a broker. It
// deliberately excludes every order-placement method -- see the package
// doc's READ-ONLY GUARD. *broker.AngelBroker satisfies this today.
type historyFetcher interface {
	Connect() error
	FetchHistory(symbol, interval string, days int) ([]backtest.Candle, error)
}

// target is one thing to fetch: Key names the checkpoint/output file,
// FetchSymbol is what's passed to FetchHistory (for options, the exact NFO
// trading symbol resolved from the instrument master).
type target struct {
	Key         string
	FetchSymbol string
}

func main() {
	underlyings := flag.String("underlyings", "NIFTY,BANKNIFTY", "comma-separated underlying index symbols to fetch")
	years := flag.Int("years", 2, "years of history to fetch, ending now")
	interval := flag.String("interval", "FIVE_MINUTE", "candle interval (broker API value)")
	outDir := flag.String("out", "data/historical", "output directory for cached candle CSVs (one file per target, cache.go format)")
	checkpointPath := flag.String("checkpoint", "", "resume checkpoint JSON path (default: <out>/.fetchdata_checkpoint.json)")
	configPath := flag.String("config", "config.yaml", "config.yaml path, read ONLY to detect (and refuse) YAML-sourced credentials -- never used for credential VALUES")
	rps := flag.Float64("rps", 0.33, "polite rate limit: max historical-data requests per second (Angel's endpoint has its own server-side limit; this just avoids hammering it)")

	optionUnderlying := flag.String("option-underlying", "", "optional: underlying for option-chain candle fetch (e.g. NIFTY); empty = skip option fetching")
	optionExpiryStr := flag.String("option-expiry", "", "expiry date YYYY-MM-DD for -option-underlying's contracts")
	optionStrikes := flag.String("option-strikes", "", "comma-separated strike prices for -option-underlying (e.g. 21800,21900,22000)")
	optionTypes := flag.String("option-types", "CE,PE", "comma-separated option types to fetch per strike")
	flag.Parse()

	if *checkpointPath == "" {
		*checkpointPath = filepath.Join(*outDir, ".fetchdata_checkpoint.json")
	}

	clientCode, pin, apiKey, totp, err := resolveCreds(*configPath)
	if err != nil {
		log.Fatalf("refusing to run: %v", err)
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("failed to create -out directory %q: %v", *outDir, err)
	}

	targets, err := buildTargets(strings.Split(*underlyings, ","), *optionUnderlying, *optionExpiryStr, *optionStrikes, *optionTypes)
	if err != nil {
		log.Fatalf("failed to build fetch target list: %v", err)
	}

	angel := broker.NewAngelBroker(clientCode, pin, apiKey, totp)
	limiter := rate.NewLimiter(rate.Limit(*rps), 1)

	if err := runFetch(angel, targets, *interval, *years*365, *outDir, *checkpointPath, limiter); err != nil {
		log.Fatalf("fetch failed: %v", err)
	}
	log.Printf("done: %d targets fetched into %s", len(targets), *outDir)
}

// ---------------------------------------------------------------------------
// Credential gate (env-only, refuses YAML-sourced creds)
// ---------------------------------------------------------------------------

type yamlAngelCreds struct {
	Brokers struct {
		Angel struct {
			ClientCode string `yaml:"client_code"`
			PIN        string `yaml:"pin"`
			APIKey     string `yaml:"api_key"`
			TOTPSecret string `yaml:"totp_secret"`
		} `yaml:"angel"`
	} `yaml:"brokers"`
}

// resolveCreds resolves the four Angel credentials this tool needs (matching
// AngelBroker.NewAngelBroker's constructor) from environment variables ONLY
// (internal/config.EnvClientCode etc. -- single source of truth for the
// names). If any is missing, it refuses. If config.yaml (at configPath) has
// non-empty broker credentials, it refuses even when env vars ARE present --
// this tool must never read real credential material from YAML, full stop,
// matching WP-8's live-mode gate philosophy (config.ValidateLiveCredentials)
// applied unconditionally here (fetching real market data always deserves
// the live-mode-strength gate).
func resolveCreds(configPath string) (clientCode, pin, apiKey, totp string, err error) {
	if yamlHasCreds(configPath) {
		return "", "", "", "", fmt.Errorf(
			"%q appears to contain broker credentials (client_code/pin/api_key/totp_secret) -- fetchdata requires "+
				"credentials ONLY via environment variables (%s, %s, %s, %s) and refuses to run if config.yaml carries "+
				"them at all, even if the env vars are also set. Blank the credential fields in config.yaml (per H-4) "+
				"before running this tool",
			configPath, config.EnvClientCode, config.EnvPIN, config.EnvAPIKey, config.EnvTOTPSecret)
	}

	env := map[string]string{
		config.EnvClientCode: os.Getenv(config.EnvClientCode),
		config.EnvPIN:        os.Getenv(config.EnvPIN),
		config.EnvAPIKey:     os.Getenv(config.EnvAPIKey),
		config.EnvTOTPSecret: os.Getenv(config.EnvTOTPSecret),
	}
	var missing []string
	for k, v := range env {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return "", "", "", "", fmt.Errorf("missing required environment variables: %v (fetchdata never reads credential VALUES from config.yaml)", missing)
	}
	return env[config.EnvClientCode], env[config.EnvPIN], env[config.EnvAPIKey], env[config.EnvTOTPSecret], nil
}

// yamlHasCreds reports whether path parses as YAML with any non-empty Angel
// credential field. A missing/unreadable file is NOT an error here (that's
// the common/expected case for a dev machine using pure env vars) -- it
// just means "no YAML credentials to worry about".
func yamlHasCreds(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var cfg yamlAngelCreds
	if err := yaml.NewDecoder(f).Decode(&cfg); err != nil {
		return false
	}
	a := cfg.Brokers.Angel
	return a.ClientCode != "" || a.PIN != "" || a.APIKey != "" || a.TOTPSecret != ""
}

// ---------------------------------------------------------------------------
// Target resolution (underlyings + optional option chain)
// ---------------------------------------------------------------------------

func buildTargets(underlyings []string, optUnderlying, optExpiryStr, optStrikesStr, optTypesStr string) ([]target, error) {
	var targets []target
	for _, u := range underlyings {
		u = strings.ToUpper(strings.TrimSpace(u))
		if u == "" {
			continue
		}
		targets = append(targets, target{Key: u, FetchSymbol: u})
	}

	optUnderlying = strings.TrimSpace(optUnderlying)
	if optUnderlying == "" {
		return targets, nil
	}
	if optExpiryStr == "" || optStrikesStr == "" {
		return nil, fmt.Errorf("-option-underlying set but -option-expiry/-option-strikes missing")
	}
	expiry, err := time.Parse("2006-01-02", optExpiryStr)
	if err != nil {
		return nil, fmt.Errorf("bad -option-expiry %q: %w", optExpiryStr, err)
	}

	im := broker.NewInstrumentManager()
	if err := im.LoadInstruments(); err != nil {
		return nil, fmt.Errorf("loading instrument master for option resolution: %w", err)
	}

	var strikes []float64
	for _, s := range strings.Split(optStrikesStr, ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, fmt.Errorf("bad strike %q in -option-strikes: %w", s, err)
		}
		strikes = append(strikes, v)
	}

	for _, strike := range strikes {
		for _, optType := range strings.Split(optTypesStr, ",") {
			optType = strings.ToUpper(strings.TrimSpace(optType))
			sym, err := findOptionSymbol(im, optUnderlying, expiry, strike, optType)
			if err != nil {
				return nil, err
			}
			key := fmt.Sprintf("%s_%s_%s_%s", strings.ToUpper(optUnderlying), expiry.Format("20060102"), strconv.FormatFloat(strike, 'f', -1, 64), optType)
			targets = append(targets, target{Key: key, FetchSymbol: sym})
		}
	}
	return targets, nil
}

// findOptionSymbol resolves the exact NFO trading symbol for one contract
// via the public instrument master (broker.InstrumentManager.Search), so
// this tool never has to guess/hand-construct Angel's option symbol naming
// convention -- it reuses whatever the master actually says (per the task's
// "reuse it, don't reimplement" instruction, applied to symbol resolution
// too, not just the HTTP call).
//
// KNOWN AMBIGUITY (documented, not guessed around): the instrument master's
// strike field is sometimes in paise (strike*100) rather than rupees,
// depending on segment/version; instruments.go's StrikeFloat is a straight
// parse with no /100 adjustment either way. This matches against BOTH the
// raw value and value/100 so it works under either convention; if a real
// fetch mismatches, that's the actual discrepancy to check first (see
// docs/reports/R2-3-REPORT.md).
func findOptionSymbol(im *broker.InstrumentManager, underlying string, expiry time.Time, strike float64, optType string) (string, error) {
	for _, inst := range im.Search(underlying) {
		if inst.ExchSeg != "NFO" || !strings.EqualFold(inst.Name, underlying) {
			continue
		}
		if !strings.HasSuffix(strings.ToUpper(inst.Symbol), optType) {
			continue
		}
		if !strikeMatches(inst.StrikeFloat, strike) {
			continue
		}
		instExpiry, err := parseExpiry(inst.Expiry)
		if err != nil || !sameDay(instExpiry, expiry) {
			continue
		}
		return inst.Symbol, nil
	}
	return "", fmt.Errorf("no NFO option instrument found for %s expiry=%s strike=%.2f type=%s",
		underlying, expiry.Format("2006-01-02"), strike, optType)
}

func strikeMatches(instStrike, wantStrike float64) bool {
	const eps = 0.5
	return math.Abs(instStrike-wantStrike) < eps || math.Abs(instStrike-wantStrike*100) < eps
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// parseExpiry parses Angel's instrument-master expiry format ("30JAN2025"
// or "30JAN25"). Duplicated in miniature from internal/broker's unexported
// parseAngelExpiry (instruments.go) since it isn't exported and this
// package is out of internal/broker's edit scope this round.
func parseExpiry(s string) (time.Time, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) < 5 {
		return time.Time{}, fmt.Errorf("unparseable expiry %q", s)
	}
	norm := s[:2] + s[2:3] + strings.ToLower(s[3:5]) + s[5:]
	for _, layout := range []string{"02Jan2006", "02Jan06"} {
		if t, err := time.Parse(layout, norm); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable expiry %q", s)
}

// ---------------------------------------------------------------------------
// Resumable, rate-limited fetch loop
// ---------------------------------------------------------------------------

type checkpoint struct {
	Done map[string]bool `json:"done"`
}

func loadCheckpoint(path string) checkpoint {
	cp := checkpoint{Done: map[string]bool{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cp
	}
	_ = json.Unmarshal(raw, &cp) // best-effort; a corrupt checkpoint just means "start fresh"
	if cp.Done == nil {
		cp.Done = map[string]bool{}
	}
	return cp
}

func saveCheckpoint(path string, cp checkpoint) error {
	raw, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

// runFetch fetches every target not already marked done in the checkpoint,
// rate-limited via limiter, saving each target's candles in cache.go's exact
// CSV format and marking it done immediately after a successful save -- so
// a killed process resumes at the next un-fetched target instead of
// re-fetching everything (G-3's "support resuming an interrupted fetch").
func runFetch(fetcher historyFetcher, targets []target, interval string, days int, outDir, checkpointPath string, limiter *rate.Limiter) error {
	cp := loadCheckpoint(checkpointPath)

	if err := fetcher.Connect(); err != nil {
		return fmt.Errorf("broker connect failed: %w", err)
	}

	for _, tgt := range targets {
		if cp.Done[tgt.Key] {
			log.Printf("skip %s: already fetched (resume)", tgt.Key)
			continue
		}

		if err := limiter.Wait(context.Background()); err != nil {
			return fmt.Errorf("rate limiter: %w", err)
		}

		candles, err := fetcher.FetchHistory(tgt.FetchSymbol, interval, days)
		if err != nil {
			return fmt.Errorf("fetch %s (%s): %w", tgt.Key, tgt.FetchSymbol, err)
		}

		outPath := filepath.Join(outDir, tgt.Key+".csv")
		if err := backtest.SaveCandlesCSV(outPath, candles); err != nil {
			return fmt.Errorf("save %s: %w", tgt.Key, err)
		}

		cp.Done[tgt.Key] = true
		if err := saveCheckpoint(checkpointPath, cp); err != nil {
			log.Printf("WARNING: fetched %s but failed to persist checkpoint: %v (a restart will re-fetch it)", tgt.Key, err)
		}

		log.Printf("fetched %d candles for %s (%s) -> %s", len(candles), tgt.Key, tgt.FetchSymbol, outPath)
	}
	return nil
}
