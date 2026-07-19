package backtest

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCandleCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "candles.csv")

	want := genFlatCandles(5, 20000, time.Date(2026, 1, 5, 9, 15, 0, 0, time.UTC))
	want[2].Close = 20123.45
	want[2].Volume = 98765

	if err := SaveCandlesCSV(path, want); err != nil {
		t.Fatalf("SaveCandlesCSV: %v", err)
	}
	got, err := LoadCandlesCSV(path)
	if err != nil {
		t.Fatalf("LoadCandlesCSV: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d candles, want %d", len(got), len(want))
	}
	for i := range want {
		if !got[i].Time.Equal(want[i].Time) || got[i].Close != want[i].Close || got[i].Volume != want[i].Volume {
			t.Errorf("candle %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestLoadOrFetch_UsesCacheWhenPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "candles.csv")

	seed := genFlatCandles(3, 100, time.Now())
	if err := SaveCandlesCSV(path, seed); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	fetchCalled := false
	got, err := LoadOrFetch(path, func() ([]Candle, error) {
		fetchCalled = true
		return nil, os.ErrInvalid // would fail if ever called -- proves cache short-circuits fetch
	})
	if err != nil {
		t.Fatalf("LoadOrFetch: %v", err)
	}
	if fetchCalled {
		t.Error("fetch was called despite cache file existing -- offline reproducibility broken")
	}
	if len(got) != 3 {
		t.Errorf("got %d candles from cache, want 3", len(got))
	}
}

func TestLoadOrFetch_FetchesAndCachesWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "candles.csv")

	seed := genFlatCandles(4, 200, time.Now())
	got, err := LoadOrFetch(path, func() ([]Candle, error) { return seed, nil })
	if err != nil {
		t.Fatalf("LoadOrFetch: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d candles, want 4", len(got))
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected cache file to be written at %s: %v", path, err)
	}
}
