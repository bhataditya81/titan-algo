package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"titan-algo/internal/backtest"
	"titan-algo/internal/config"

	"golang.org/x/time/rate"
	"gopkg.in/yaml.v2"
)

// ---------------------------------------------------------------------------
// Credential gate (env-only, refuses YAML-sourced creds)
// ---------------------------------------------------------------------------

func clearAngelEnv(t *testing.T) {
	t.Helper()
	for _, e := range []string{config.EnvClientCode, config.EnvPIN, config.EnvAPIKey, config.EnvTOTPSecret} {
		t.Setenv(e, "")
		os.Unsetenv(e)
	}
}

func TestResolveCreds_MissingEnvVarsRefuses(t *testing.T) {
	clearAngelEnv(t)
	_, _, _, _, err := resolveCreds(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	if err == nil {
		t.Fatal("expected an error when no env vars and no config.yaml are present")
	}
}

func TestResolveCreds_AllEnvVarsPresentSucceeds(t *testing.T) {
	clearAngelEnv(t)
	t.Setenv(config.EnvClientCode, "C123")
	t.Setenv(config.EnvPIN, "1234")
	t.Setenv(config.EnvAPIKey, "key")
	t.Setenv(config.EnvTOTPSecret, "secret")

	cc, pin, key, totp, err := resolveCreds(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cc != "C123" || pin != "1234" || key != "key" || totp != "secret" {
		t.Errorf("got %q %q %q %q, want the env values back verbatim", cc, pin, key, totp)
	}
}

func TestResolveCreds_YAMLCredentialsRefusedEvenWithEnvSet(t *testing.T) {
	clearAngelEnv(t)
	t.Setenv(config.EnvClientCode, "C123")
	t.Setenv(config.EnvPIN, "1234")
	t.Setenv(config.EnvAPIKey, "key")
	t.Setenv(config.EnvTOTPSecret, "secret")

	yamlPath := filepath.Join(t.TempDir(), "config.yaml")
	body, _ := yaml.Marshal(map[string]any{
		"brokers": map[string]any{
			"angel": map[string]any{"client_code": "BURNED-CREDS"},
		},
	})
	if err := os.WriteFile(yamlPath, body, 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, _, _, err := resolveCreds(yamlPath)
	if err == nil {
		t.Fatal("expected refusal when config.yaml carries broker credentials, even with env vars set")
	}
}

func TestResolveCreds_MissingYAMLFileIsFine(t *testing.T) {
	clearAngelEnv(t)
	t.Setenv(config.EnvClientCode, "C123")
	t.Setenv(config.EnvPIN, "1234")
	t.Setenv(config.EnvAPIKey, "key")
	t.Setenv(config.EnvTOTPSecret, "secret")

	_, _, _, _, err := resolveCreds(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Errorf("a missing config.yaml should not block a run with full env creds: %v", err)
	}
}

// ---------------------------------------------------------------------------
// runFetch: fake historyFetcher backed by a REAL local httptest server, so
// this exercises an actual HTTP round trip end to end (the rate limiting,
// resume/checkpointing and CSV output are all real; only AngelBroker's
// concrete network client is swapped out -- see main.go's package doc for
// why AngelBroker itself can't be redirected to httptest this round).
// ---------------------------------------------------------------------------

// fakeHTTPFetcher fetches candles by calling a local httptest server per
// symbol, proving the whole pipeline (rate limit -> HTTP call -> parse ->
// cache-format write) works, without touching internal/broker.
type fakeHTTPFetcher struct {
	srv        *httptest.Server
	connectErr error
	calls      []time.Time // one entry appended per FetchHistory call, for rate-limit assertions
	failOn     string      // symbol to fail on, once (simulates a kill mid-run)
	failed     map[string]bool
}

func (f *fakeHTTPFetcher) Connect() error { return f.connectErr }

func (f *fakeHTTPFetcher) FetchHistory(symbol, interval string, days int) ([]backtest.Candle, error) {
	f.calls = append(f.calls, time.Now())

	if f.failOn != "" && f.failOn == symbol && !f.failed[symbol] {
		f.failed[symbol] = true
		return nil, fmt.Errorf("simulated mid-run failure for %s", symbol)
	}

	resp, err := http.Get(f.srv.URL + "/candles?symbol=" + symbol)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var candles []backtest.Candle
	if err := json.NewDecoder(resp.Body).Decode(&candles); err != nil {
		return nil, err
	}
	return candles, nil
}

func newFakeServer(t *testing.T) *httptest.Server {
	t.Helper()
	base := time.Date(2026, 1, 5, 9, 15, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		symbol := r.URL.Query().Get("symbol")
		candles := []backtest.Candle{
			{Time: base, Open: 100, High: 101, Low: 99, Close: 100.5, Volume: 1000},
			{Time: base.Add(5 * time.Minute), Open: 100.5, High: 102, Low: 100, Close: 101.5, Volume: 1200},
		}
		_ = symbol
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(candles)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunFetch_CSVRoundTripMatchesCacheFormat(t *testing.T) {
	srv := newFakeServer(t)
	fetcher := &fakeHTTPFetcher{srv: srv, failed: map[string]bool{}}

	outDir := t.TempDir()
	cpPath := filepath.Join(outDir, "cp.json")
	limiter := rate.NewLimiter(rate.Inf, 1)

	targets := []target{{Key: "NIFTY", FetchSymbol: "NIFTY"}}
	if err := runFetch(fetcher, targets, "FIVE_MINUTE", 730, outDir, cpPath, limiter); err != nil {
		t.Fatalf("runFetch: %v", err)
	}

	got, err := backtest.LoadCandlesCSV(filepath.Join(outDir, "NIFTY.csv"))
	if err != nil {
		t.Fatalf("cache.go couldn't read fetchdata's output: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 round-tripped candles, got %d", len(got))
	}
	if got[0].Close != 100.5 || got[1].Close != 101.5 {
		t.Errorf("round-tripped candles don't match: %+v", got)
	}
}

func TestRunFetch_RespectsRateLimit(t *testing.T) {
	srv := newFakeServer(t)
	fetcher := &fakeHTTPFetcher{srv: srv, failed: map[string]bool{}}

	outDir := t.TempDir()
	cpPath := filepath.Join(outDir, "cp.json")
	// 5 requests/sec, burst 1 -> each call after the first must wait ~200ms.
	limiter := rate.NewLimiter(rate.Limit(5), 1)

	targets := []target{
		{Key: "A", FetchSymbol: "A"},
		{Key: "B", FetchSymbol: "B"},
		{Key: "C", FetchSymbol: "C"},
	}
	start := time.Now()
	if err := runFetch(fetcher, targets, "FIVE_MINUTE", 730, outDir, cpPath, limiter); err != nil {
		t.Fatalf("runFetch: %v", err)
	}
	elapsed := time.Since(start)

	// 3 calls at 5rps/burst1 needs >= 2 * 200ms = 400ms total.
	if elapsed < 350*time.Millisecond {
		t.Errorf("elapsed %v is too fast for a 5rps/burst1 limiter across 3 calls -- rate limiting not applied", elapsed)
	}
	if len(fetcher.calls) != 3 {
		t.Fatalf("expected 3 FetchHistory calls, got %d", len(fetcher.calls))
	}
}

func TestRunFetch_ResumesAfterInterrupt(t *testing.T) {
	srv := newFakeServer(t)
	outDir := t.TempDir()
	cpPath := filepath.Join(outDir, "cp.json")
	limiter := rate.NewLimiter(rate.Inf, 1)

	targets := []target{
		{Key: "NIFTY", FetchSymbol: "NIFTY"},
		{Key: "BANKNIFTY", FetchSymbol: "BANKNIFTY"},
		{Key: "FINNIFTY", FetchSymbol: "FINNIFTY"},
	}

	// First run: fails partway through BANKNIFTY (simulates the process
	// being killed mid-fetch).
	fetcher1 := &fakeHTTPFetcher{srv: srv, failOn: "BANKNIFTY", failed: map[string]bool{}}
	err := runFetch(fetcher1, targets, "FIVE_MINUTE", 730, outDir, cpPath, limiter)
	if err == nil {
		t.Fatal("expected the first run to fail on BANKNIFTY")
	}
	if _, statErr := os.Stat(filepath.Join(outDir, "NIFTY.csv")); statErr != nil {
		t.Fatalf("NIFTY should have been fetched and cached before the failure: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(outDir, "BANKNIFTY.csv")); statErr == nil {
		t.Fatalf("BANKNIFTY should NOT have been cached (it failed)")
	}

	// Second run ("resume"): NIFTY must be skipped (already done); BANKNIFTY
	// and FINNIFTY must be (re)fetched.
	fetcher2 := &fakeHTTPFetcher{srv: srv, failed: map[string]bool{}}
	if err := runFetch(fetcher2, targets, "FIVE_MINUTE", 730, outDir, cpPath, limiter); err != nil {
		t.Fatalf("resume run failed: %v", err)
	}
	if len(fetcher2.calls) != 2 {
		t.Fatalf("resume run should only re-fetch the 2 unfinished targets (BANKNIFTY, FINNIFTY), got %d calls", len(fetcher2.calls))
	}
	for _, key := range []string{"NIFTY", "BANKNIFTY", "FINNIFTY"} {
		if _, statErr := os.Stat(filepath.Join(outDir, key+".csv")); statErr != nil {
			t.Errorf("%s.csv missing after resume: %v", key, statErr)
		}
	}
}

func TestStrikeMatches_RawAndPaiseConventions(t *testing.T) {
	if !strikeMatches(22000, 22000) {
		t.Error("exact match should succeed")
	}
	if !strikeMatches(2200000, 22000) {
		t.Error("paise-scaled (x100) match should succeed")
	}
	if strikeMatches(21000, 22000) {
		t.Error("unrelated strikes should not match")
	}
}
