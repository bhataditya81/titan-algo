package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"titan-algo/internal/strategy"
)

// TestLoadHolidays_ValidFile_ReplacesHardcodedTable is the R2-INT acceptance
// test for wiring task 6 (R2-5-REPORT.md §4): a valid holiday YAML file
// (the exact go-engine/nse_holidays.yaml shape: a top-level `holidays:` list
// of {date, description}) replaces the built-in nseHolidays2026 table, and
// marketState() actually honors a date that is ONLY in the file (not in the
// hardcoded table), proving the file — not the fallback — is what's driving
// the decision.
func TestLoadHolidays_ValidFile_ReplacesHardcodedTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "holidays.yaml")
	// 2026-03-02 is NOT in nseHolidays2026 — proves the file is authoritative.
	content := "holidays:\n  - date: \"2026-03-02\"\n    description: \"Test-only holiday\"\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write holiday file: %v", err)
	}

	holidays, err := loadHolidays(path)
	if err != nil {
		t.Fatalf("loadHolidays: %v", err)
	}
	if !holidays["2026-03-02"] {
		t.Fatalf("expected the file's date to be loaded, got %v", holidays)
	}
	if holidays["2026-01-26"] {
		t.Fatalf("expected the hardcoded table's dates to be REPLACED, not merged, but 2026-01-26 leaked through")
	}

	r := &Runner{holidays: holidays, cfg: RunnerConfig{SquareOffHour: 15, SquareOffMinute: 20}}
	testHolidayDate := time.Date(2026, 3, 2, 10, 0, 0, 0, strategy.IST) // a Monday
	if got := r.marketState(testHolidayDate); got != marketClosedHoliday {
		t.Fatalf("expected marketClosedHoliday for the file-driven date, got %v", got)
	}
}

// TestLoadHolidays_MissingOrMalformedFile_FailsClosed proves the FAIL-CLOSED
// policy: once an operator has explicitly pointed HolidayFile at a path, a
// missing/malformed/empty file is a configuration error that must stop
// startup (via NewRunner propagating it) rather than silently substitute the
// hardcoded table — the earlier "fail open" behavior could mean the engine
// trades on what is actually an NSE holiday without anyone knowing the real
// calendar never loaded.
func TestLoadHolidays_MissingOrMalformedFile_FailsClosed(t *testing.T) {
	t.Run("missing file returns an error", func(t *testing.T) {
		_, err := loadHolidays(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
		if err == nil {
			t.Fatal("expected an error for a missing, explicitly-configured holiday file, got nil (would have silently used the stale hardcoded table)")
		}
	})

	t.Run("malformed YAML returns an error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bad.yaml")
		if err := os.WriteFile(path, []byte("not: [valid: yaml: at all"), 0644); err != nil {
			t.Fatalf("write bad holiday file: %v", err)
		}
		if _, err := loadHolidays(path); err == nil {
			t.Fatal("expected an error for malformed holiday YAML, got nil")
		}
	})

	t.Run("file with zero dates returns an error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "empty.yaml")
		if err := os.WriteFile(path, []byte("holidays: []\n"), 0644); err != nil {
			t.Fatalf("write empty holiday file: %v", err)
		}
		if _, err := loadHolidays(path); err == nil {
			t.Fatal("expected an error for a holiday file with zero usable dates, got nil")
		}
	})

	t.Run("empty path (no file configured) uses the built-in table, not a failure", func(t *testing.T) {
		holidays, err := loadHolidays("")
		if err != nil {
			t.Fatalf("empty path is the documented default and must not error: %v", err)
		}
		if !holidays["2026-12-25"] {
			t.Fatalf("expected the hardcoded table when no path is configured, got %v", holidays)
		}
	})
}

// TestNewRunner_InvalidHolidayFile_RefusesToStart proves the fail-closed
// wiring end-to-end: NewRunner itself must refuse to construct a Runner
// when the configured holiday file can't be loaded, not just loadHolidays
// in isolation.
func TestNewRunner_InvalidHolidayFile_RefusesToStart(t *testing.T) {
	te := &TradingEngine{}
	cfg := RunnerConfig{
		StrategyName: "ema_crossover",
		HolidayFile:  filepath.Join(t.TempDir(), "does-not-exist.yaml"),
	}
	if _, err := NewRunner(te, cfg); err == nil {
		t.Fatal("expected NewRunner to refuse to start with an unloadable, explicitly-configured holiday file")
	}
}

// TestLoadHolidays_RealShippedFile loads the actual go-engine/nse_holidays.yaml
// R2-5 created, proving this wiring works against the real file, not just a
// synthetic fixture.
func TestLoadHolidays_RealShippedFile(t *testing.T) {
	holidays, err := loadHolidays("../../nse_holidays.yaml")
	if err != nil {
		t.Fatalf("loadHolidays real file: %v", err)
	}
	for _, want := range []string{"2026-01-26", "2026-04-03", "2026-08-15", "2026-10-02", "2026-12-25"} {
		if !holidays[want] {
			t.Fatalf("expected %s to be loaded from the real nse_holidays.yaml, got %v", want, holidays)
		}
	}
}
