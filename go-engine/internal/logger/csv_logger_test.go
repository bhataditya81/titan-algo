package logger

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"titan-algo/internal/broker"
)

func sampleFilledOrder(orderID, symbol string, qty int, price float64) *broker.FilledOrder {
	return &broker.FilledOrder{
		OrderID:        orderID,
		Symbol:         symbol,
		Quantity:       qty,
		Side:           broker.Buy,
		FillPrice:      price,
		Slippage:       0.5,
		TransactionFee: 12.34,
		Timestamp:      time.Date(2026, 7, 18, 9, 20, 0, 0, time.UTC),
	}
}

// TestNoTruncationOnRestart proves the truncate-on-open bug is fixed (EX-8):
// write, close, reopen the SAME logger (simulating a process restart), write
// again, and verify BOTH entries are present in the file.
func TestNoTruncationOnRestart(t *testing.T) {
	dir := t.TempDir()

	l1, err := NewCSVLogger(dir)
	if err != nil {
		t.Fatalf("NewCSVLogger (session 1) failed: %v", err)
	}
	if err := l1.LogTrade(sampleFilledOrder("ORD-1", "NIFTY", 75, 150.5), 100000); err != nil {
		t.Fatalf("LogTrade (session 1) failed: %v", err)
	}
	path := l1.GetFilePath()
	if err := l1.Close(); err != nil {
		t.Fatalf("Close (session 1) failed: %v", err)
	}

	// "Restart": open a new logger pointed at the same directory.
	l2, err := NewCSVLogger(dir)
	if err != nil {
		t.Fatalf("NewCSVLogger (session 2) failed: %v", err)
	}
	if err := l2.LogTrade(sampleFilledOrder("ORD-2", "NIFTY", 75, 151.0), 100000); err != nil {
		t.Fatalf("LogTrade (session 2) failed: %v", err)
	}
	if err := l2.Close(); err != nil {
		t.Fatalf("Close (session 2) failed: %v", err)
	}

	rows := readCSV(t, path)
	// header + 2 data rows
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows (header + 2 trades) after restart, got %d: %v", len(rows), rows)
	}
	if rows[1][10] != "ORD-1" {
		t.Errorf("row 1 OrderID = %s, want ORD-1", rows[1][10])
	}
	if rows[2][10] != "ORD-2" {
		t.Errorf("row 2 OrderID = %s, want ORD-2 (row from session 1 was NOT truncated)", rows[2][10])
	}
}

// TestHeaderWrittenOnlyOnce ensures reopening an existing non-empty file
// does not duplicate the header row.
func TestHeaderWrittenOnlyOnce(t *testing.T) {
	dir := t.TempDir()

	for i := 0; i < 3; i++ {
		l, err := NewCSVLogger(dir)
		if err != nil {
			t.Fatalf("NewCSVLogger iteration %d failed: %v", i, err)
		}
		if err := l.LogTrade(sampleFilledOrder("ORD-X", "NIFTY", 75, 150), 100000); err != nil {
			t.Fatalf("LogTrade iteration %d failed: %v", i, err)
		}
		path := l.GetFilePath()
		if err := l.Close(); err != nil {
			t.Fatalf("Close iteration %d failed: %v", i, err)
		}
		rows := readCSV(t, path)
		headerCount := 0
		for _, r := range rows {
			if len(r) > 0 && r[0] == "Timestamp" {
				headerCount++
			}
		}
		if headerCount != 1 {
			t.Fatalf("iteration %d: expected exactly 1 header row, found %d", i, headerCount)
		}
		if len(rows) != i+2 { // header + (i+1) data rows
			t.Fatalf("iteration %d: expected %d rows, got %d", i, i+2, len(rows))
		}
	}
}

// TestDateStampedFilename verifies the filename format and that OrderID +
// Status columns are present in both the header and data rows.
func TestDateStampedFilename(t *testing.T) {
	dir := t.TempDir()
	l, err := NewCSVLogger(dir)
	if err != nil {
		t.Fatalf("NewCSVLogger failed: %v", err)
	}
	defer l.Close()

	path := l.GetFilePath()
	base := filepath.Base(path)
	wantPrefix := "trades_" + time.Now().Format("2006-01-02")
	if !strings.HasPrefix(base, wantPrefix) || !strings.HasSuffix(base, ".csv") {
		t.Fatalf("filename = %q, want prefix %q and .csv suffix", base, wantPrefix)
	}

	if err := l.LogTradeWithStatus(sampleFilledOrder("ORD-77", "BANKNIFTY", 30, 500.25), 100000, 95000, 500, "partial"); err != nil {
		t.Fatalf("LogTradeWithStatus failed: %v", err)
	}

	rows := readCSV(t, path)
	header := rows[0]
	if header[len(header)-2] != "OrderID" || header[len(header)-1] != "Status" {
		t.Fatalf("header last two columns = %v, want OrderID, Status", header[len(header)-2:])
	}
	data := rows[1]
	if data[len(data)-2] != "ORD-77" {
		t.Errorf("OrderID column = %s, want ORD-77", data[len(data)-2])
	}
	if data[len(data)-1] != "partial" {
		t.Errorf("Status column = %s, want partial", data[len(data)-1])
	}
}

// TestRotatesOnDateChange verifies that when the injected clock crosses a
// day boundary, the logger closes the old file and opens a new
// date-stamped one (with its own header), rather than writing into
// yesterday's file forever.
func TestRotatesOnDateChange(t *testing.T) {
	dir := t.TempDir()
	l, err := NewCSVLogger(dir)
	if err != nil {
		t.Fatalf("NewCSVLogger failed: %v", err)
	}
	defer l.Close()

	day1 := time.Date(2026, 7, 18, 15, 0, 0, 0, time.UTC)
	l.nowFunc = func() time.Time { return day1 }
	if err := l.rotateIfNeeded(); err != nil { // force open under the injected clock
		t.Fatalf("rotateIfNeeded (day1) failed: %v", err)
	}
	if err := l.LogTrade(sampleFilledOrder("ORD-D1", "NIFTY", 75, 150), 100000); err != nil {
		t.Fatalf("LogTrade (day1) failed: %v", err)
	}
	day1Path := l.GetFilePath()
	if !strings.Contains(day1Path, "trades_2026-07-18.csv") {
		t.Fatalf("day1 path = %s, want trades_2026-07-18.csv", day1Path)
	}

	day2 := time.Date(2026, 7, 19, 9, 15, 0, 0, time.UTC)
	l.nowFunc = func() time.Time { return day2 }
	if err := l.LogTrade(sampleFilledOrder("ORD-D2", "NIFTY", 75, 151), 100000); err != nil {
		t.Fatalf("LogTrade (day2) failed: %v", err)
	}
	day2Path := l.GetFilePath()
	if !strings.Contains(day2Path, "trades_2026-07-19.csv") {
		t.Fatalf("day2 path = %s, want trades_2026-07-19.csv", day2Path)
	}
	if day2Path == day1Path {
		t.Fatal("expected a new file path after date rollover")
	}

	rows2 := readCSV(t, day2Path)
	if len(rows2) != 2 { // header + 1 data row, NOT carrying over day1's rows
		t.Fatalf("day2 file: expected 2 rows (header + 1), got %d: %v", len(rows2), rows2)
	}
	rows1 := readCSV(t, day1Path)
	if len(rows1) != 2 {
		t.Fatalf("day1 file: expected 2 rows (header + 1), got %d: %v", len(rows1), rows1)
	}
}

func readCSV(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("failed to open %s: %v", path, err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("failed to parse CSV %s: %v", path, err)
	}
	return rows
}
