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

	holidays := loadHolidays(path)
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

// TestLoadHolidays_MissingOrMalformedFile_FailsOpenToHardcodedTable proves
// the fail-open behavior R2-5's report explicitly recommended: a
// missing/malformed holiday file must not prevent the engine from starting
// — it falls back to the existing built-in table instead.
func TestLoadHolidays_MissingOrMalformedFile_FailsOpenToHardcodedTable(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		holidays := loadHolidays(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
		if !holidays["2026-01-26"] {
			t.Fatalf("expected fallback to the hardcoded table for a missing file, got %v", holidays)
		}
	})

	t.Run("malformed YAML", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bad.yaml")
		if err := os.WriteFile(path, []byte("not: [valid: yaml: at all"), 0644); err != nil {
			t.Fatalf("write bad holiday file: %v", err)
		}
		holidays := loadHolidays(path)
		if !holidays["2026-01-26"] {
			t.Fatalf("expected fallback to the hardcoded table for malformed YAML, got %v", holidays)
		}
	})

	t.Run("empty path uses hardcoded table", func(t *testing.T) {
		holidays := loadHolidays("")
		if !holidays["2026-12-25"] {
			t.Fatalf("expected the hardcoded table when no path is configured, got %v", holidays)
		}
	})
}

// TestLoadHolidays_RealShippedFile loads the actual go-engine/nse_holidays.yaml
// R2-5 created, proving this wiring works against the real file, not just a
// synthetic fixture.
func TestLoadHolidays_RealShippedFile(t *testing.T) {
	holidays := loadHolidays("../../nse_holidays.yaml")
	for _, want := range []string{"2026-01-26", "2026-04-03", "2026-08-15", "2026-10-02", "2026-12-25"} {
		if !holidays[want] {
			t.Fatalf("expected %s to be loaded from the real nse_holidays.yaml, got %v", want, holidays)
		}
	}
}
