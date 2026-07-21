package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"titan-algo/internal/broker"
	"titan-algo/internal/config"

	"gopkg.in/yaml.v2"
)

func clearAngelEnv(t *testing.T) {
	t.Helper()
	for _, e := range []string{config.EnvClientCode, config.EnvPIN, config.EnvAPIKey, config.EnvTOTPSecret} {
		t.Setenv(e, "")
		os.Unsetenv(e)
	}
}

// TestResolveCreds_YAMLCredentialsRefusedEvenWithEnvSet is the R3-8
// regression test: cmd/backtest previously fell back to config.yaml
// credential values via envOr whenever an env var was unset -- a materially
// weaker gate than cmd/fetchdata's resolveCreds, which refuses outright if
// config.yaml carries broker credentials at all, even with env vars also
// set. This must now match that stricter contract.
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

func TestResolveCreds_MissingEnvVarsRefuses(t *testing.T) {
	clearAngelEnv(t)
	_, _, _, _, err := resolveCreds(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	if err == nil {
		t.Fatal("expected an error when no env vars and no config.yaml are present")
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

// TestLotSizeFromInstrumentCache_ResolvesFromCache is the R3-5 regression
// test: cmd/backtest previously had NO cache-lookup path at all, silently
// defaulting -lotsize's hardcoded 75 (NIFTY's post-Apr-2025 lot size) for
// every symbol. A non-NIFTY symbol's real lot size must resolve from the
// instrument-master cache instead of silently inheriting NIFTY's.
func TestLotSizeFromInstrumentCache_ResolvesFromCache(t *testing.T) {
	dir := t.TempDir()
	instruments := []broker.Instrument{
		{Symbol: "BANKNIFTY", Name: "BANKNIFTY", LotSize: "30"},
	}
	writeInstrumentCache(t, dir, "scripmaster_2026-07-20.json", instruments)

	ls, ok := lotSizeFromInstrumentCache(dir, "BANKNIFTY")
	if !ok || ls != 30 {
		t.Fatalf("expected BANKNIFTY lot size 30 from cache, got ls=%d ok=%v", ls, ok)
	}
}

func TestLotSizeFromInstrumentCache_UnresolvedSymbol_ReturnsNotOK(t *testing.T) {
	dir := t.TempDir()
	instruments := []broker.Instrument{
		{Symbol: "BANKNIFTY", Name: "BANKNIFTY", LotSize: "30"},
	}
	writeInstrumentCache(t, dir, "scripmaster_2026-07-20.json", instruments)

	if _, ok := lotSizeFromInstrumentCache(dir, "FINNIFTY"); ok {
		t.Fatal("expected ok=false for a symbol not present in the cache -- must never guess")
	}
}

func TestLotSizeFromInstrumentCache_MissingDir_ReturnsNotOK(t *testing.T) {
	if _, ok := lotSizeFromInstrumentCache(filepath.Join(t.TempDir(), "does-not-exist"), "NIFTY"); ok {
		t.Fatal("expected ok=false when the cache directory doesn't exist")
	}
}

func writeInstrumentCache(t *testing.T, dir, filename string, instruments []broker.Instrument) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, filename))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(instruments); err != nil {
		t.Fatal(err)
	}
}
