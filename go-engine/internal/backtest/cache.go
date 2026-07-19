package backtest

import (
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
	"time"
)

// LoadCandlesCSV reads a local candle cache file (RFC3339 time, OHLCV) so
// backtests are reproducible offline without a broker login (WP-7 task 7/8).
func LoadCandlesCSV(path string) ([]Candle, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	rows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("backtest: empty cache file %s", path)
	}
	start := 0
	if len(rows[0]) > 0 && rows[0][0] == "time" {
		start = 1 // skip header
	}

	candles := make([]Candle, 0, len(rows)-start)
	for i := start; i < len(rows); i++ {
		row := rows[i]
		if len(row) < 6 {
			return nil, fmt.Errorf("backtest: cache file %s row %d: expected 6 columns, got %d", path, i, len(row))
		}
		t, err := time.Parse(time.RFC3339, row[0])
		if err != nil {
			return nil, fmt.Errorf("backtest: cache file %s row %d: bad time %q: %w", path, i, row[0], err)
		}
		o, err1 := strconv.ParseFloat(row[1], 64)
		h, err2 := strconv.ParseFloat(row[2], 64)
		l, err3 := strconv.ParseFloat(row[3], 64)
		c, err4 := strconv.ParseFloat(row[4], 64)
		v, err5 := strconv.ParseInt(row[5], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil {
			return nil, fmt.Errorf("backtest: cache file %s row %d: malformed OHLCV", path, i)
		}
		candles = append(candles, Candle{Time: t, Open: o, High: h, Low: l, Close: c, Volume: v})
	}
	return candles, nil
}

// SaveCandlesCSV writes candles to a local cache file for reuse.
func SaveCandlesCSV(path string, candles []Candle) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	if err := w.Write([]string{"time", "open", "high", "low", "close", "volume"}); err != nil {
		return err
	}
	for _, c := range candles {
		row := []string{
			c.Time.Format(time.RFC3339),
			strconv.FormatFloat(c.Open, 'f', 4, 64),
			strconv.FormatFloat(c.High, 'f', 4, 64),
			strconv.FormatFloat(c.Low, 'f', 4, 64),
			strconv.FormatFloat(c.Close, 'f', 4, 64),
			strconv.FormatInt(c.Volume, 10),
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	return w.Error()
}

// LoadOrFetch returns cached candles from path if present, otherwise calls
// fetch(), caches the result to path, and returns it. This is what lets a
// backtest run entirely offline once a cache file exists (WP-7 task 8: "no
// credentials required for cached runs").
func LoadOrFetch(path string, fetch func() ([]Candle, error)) ([]Candle, error) {
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			return LoadCandlesCSV(path)
		}
	}
	candles, err := fetch()
	if err != nil {
		return nil, err
	}
	if path != "" {
		if err := SaveCandlesCSV(path, candles); err != nil {
			return nil, fmt.Errorf("backtest: fetched candles but failed to cache to %s: %w", path, err)
		}
	}
	return candles, nil
}
