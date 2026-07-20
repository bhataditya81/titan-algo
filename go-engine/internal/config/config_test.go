package config

import (
	"strings"
	"testing"
)

const minimalYAML = `
brokers:
  angel:
    client_code: "yaml-client"
    pin: "yaml-pin"
    api_key: "yaml-key"
    password: "yaml-secret"
    totp_secret: "yaml-totp"
`

func clearCredEnv(t *testing.T) {
	t.Helper()
	for _, e := range []string{EnvClientCode, EnvPIN, EnvAPIKey, EnvAPISecret, EnvTOTPSecret, EnvAPIToken, EnvMode} {
		t.Setenv(e, "")
		// t.Setenv sets to "", which os.Getenv treats as unset for our
		// resolveCredential (empty string check), so this is sufficient.
	}
}

func TestParse_RejectsUnimplementedStopLossType(t *testing.T) {
	clearCredEnv(t)
	yaml := minimalYAML + "\nrisk:\n  stop_loss:\n    enabled: true\n    type: \"atr\"\n"
	_, err := parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected an error for risk.stop_loss.type \"atr\" (never implemented, must not silently become a hardcoded 5%)")
	}
	if !strings.Contains(err.Error(), "atr") {
		t.Errorf("error %q should name the offending type", err.Error())
	}
}

func TestParse_AcceptsImplementedStopLossTypes(t *testing.T) {
	clearCredEnv(t)
	for _, typ := range []string{"percentage", "points"} {
		yaml := minimalYAML + "\nrisk:\n  stop_loss:\n    enabled: true\n    type: \"" + typ + "\"\n"
		if _, err := parse([]byte(yaml)); err != nil {
			t.Errorf("type %q: unexpected error: %v", typ, err)
		}
	}
}

func TestParse_StopLossTypeIgnoredWhenDisabled(t *testing.T) {
	clearCredEnv(t)
	yaml := minimalYAML + "\nrisk:\n  stop_loss:\n    enabled: false\n    type: \"atr\"\n"
	if _, err := parse([]byte(yaml)); err != nil {
		t.Errorf("unexpected error when stop_loss.enabled is false: %v", err)
	}
}

func TestEnvOverridesYAML(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvClientCode, "env-client")
	t.Setenv(EnvAPIKey, "env-key")

	cfg, err := parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if cfg.Brokers.Angel.ClientCode != "env-client" {
		t.Errorf("ClientCode = %q, want env value", cfg.Brokers.Angel.ClientCode)
	}
	if cfg.Brokers.Angel.APIKey != "env-key" {
		t.Errorf("APIKey = %q, want env value", cfg.Brokers.Angel.APIKey)
	}
	// PIN not set via env -> falls back to YAML.
	if cfg.Brokers.Angel.PIN != "yaml-pin" {
		t.Errorf("PIN = %q, want yaml fallback", cfg.Brokers.Angel.PIN)
	}

	src := cfg.CredentialSources()
	if src[EnvClientCode] != SourceEnv {
		t.Errorf("ClientCode source = %q, want env", src[EnvClientCode])
	}
	if src[EnvPIN] != SourceYAML {
		t.Errorf("PIN source = %q, want yaml", src[EnvPIN])
	}
}

func TestLiveModeRejectsYAMLCredentials(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvMode, "live")
	// No env credentials set -> everything falls back to (or is missing from) YAML.

	_, err := parse([]byte(minimalYAML))
	if err == nil {
		t.Fatal("expected error in live mode with YAML-sourced credentials, got nil")
	}
	if !strings.Contains(err.Error(), "ANGEL_CLIENT_CODE") {
		t.Errorf("error should name the offending env var, got: %v", err)
	}
}

func TestLiveModeAcceptsEnvCredentials(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvMode, "live")
	t.Setenv(EnvClientCode, "c")
	t.Setenv(EnvPIN, "p")
	t.Setenv(EnvAPIKey, "k")
	t.Setenv(EnvAPISecret, "s")
	t.Setenv(EnvTOTPSecret, "t")

	cfg, err := parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("expected live mode to start with all-env credentials, got: %v", err)
	}
	if cfg.Brokers.Angel.ClientCode != "c" {
		t.Errorf("ClientCode = %q, want env value", cfg.Brokers.Angel.ClientCode)
	}
}

func TestEngineBlockTopLevelPreferred(t *testing.T) {
	clearCredEnv(t)
	yamlDoc := `
engine:
  poll_interval_ms: 500
brokers:
  engine:
    poll_interval_ms: 9999
`
	cfg, err := parse([]byte(yamlDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Engine.PollIntervalMs != 500 {
		t.Errorf("PollIntervalMs = %d, want top-level value 500", cfg.Engine.PollIntervalMs)
	}
}

func TestEngineBlockNestedFallback(t *testing.T) {
	clearCredEnv(t)
	yamlDoc := `
brokers:
  engine:
    poll_interval_ms: 1234
    default_quantity: 7
`
	cfg, err := parse([]byte(yamlDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Engine.PollIntervalMs != 1234 {
		t.Errorf("PollIntervalMs = %d, want nested fallback value 1234", cfg.Engine.PollIntervalMs)
	}
	if cfg.Engine.DefaultQuantity != 7 {
		t.Errorf("DefaultQuantity = %d, want nested fallback value 7", cfg.Engine.DefaultQuantity)
	}
}

func TestDefaultsAppliedWhenEngineAbsent(t *testing.T) {
	clearCredEnv(t)
	cfg, err := parse([]byte(`app: {name: "x"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Engine.PollIntervalMs != 2000 {
		t.Errorf("PollIntervalMs default = %d, want 2000", cfg.Engine.PollIntervalMs)
	}
	if cfg.Risk.MaxOrdersPerMin != 20 {
		t.Errorf("MaxOrdersPerMin default = %d, want 20", cfg.Risk.MaxOrdersPerMin)
	}
	if cfg.API.BindAddr != "127.0.0.1:8080" {
		t.Errorf("API.BindAddr default = %q, want 127.0.0.1:8080", cfg.API.BindAddr)
	}
	if cfg.State.DBPath != "data/titan_state.db" {
		t.Errorf("State.DBPath default = %q", cfg.State.DBPath)
	}
	if cfg.Ledger.DBPath != "data/titan_ledger.db" {
		t.Errorf("Ledger.DBPath default = %q", cfg.Ledger.DBPath)
	}
}
