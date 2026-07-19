package ledger

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sampleTrades(base time.Time) []Trade {
	return []Trade{
		{
			Timestamp:         base,
			ClientOrderID:     "titan-1-1",
			BrokerOrderID:     "",
			Symbol:            "NIFTY25JUL26000CE",
			Side:              "BUY",
			Quantity:          0,
			RequestedQuantity: 75,
			Price:             0,
			Status:            StatusIntent,
			Charges:           0,
			RealizedPnL:       0,
			Strategy:          "nine_twenty",
			Mode:              ModePaper,
			Note:              "entry intent",
		},
		{
			Timestamp:         base.Add(2 * time.Second),
			ClientOrderID:     "titan-1-1",
			BrokerOrderID:     "230718000123456",
			Symbol:            "NIFTY25JUL26000CE",
			Side:              "BUY",
			Quantity:          75,
			RequestedQuantity: 75,
			Price:             152.35,
			Status:            StatusFilled,
			Charges:           41.12,
			RealizedPnL:       0,
			Strategy:          "nine_twenty",
			Mode:              ModePaper,
			Note:              "entry filled",
		},
		{
			Timestamp:         base.Add(1 * time.Hour),
			ClientOrderID:     "titan-2-2",
			BrokerOrderID:     "230718000123457",
			Symbol:            "NIFTY25JUL26000CE",
			Side:              "SELL",
			Quantity:          75,
			RequestedQuantity: 75,
			Price:             168.90,
			Status:            StatusFilled,
			Charges:           43.87,
			RealizedPnL:       1155.75,
			Strategy:          "nine_twenty",
			Mode:              ModePaper,
			Note:              "exit filled",
		},
	}
}

func TestAppendAndQuery(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "titan_ledger.db")
	l, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer l.Close()

	base := time.Date(2026, 7, 18, 9, 20, 0, 0, time.UTC)
	trades := sampleTrades(base)
	for _, tr := range trades {
		if err := l.Append(tr); err != nil {
			t.Fatalf("Append failed for %+v: %v", tr, err)
		}
	}

	got, err := l.Query(DateRange{})
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	if len(got) != len(trades) {
		t.Fatalf("expected %d rows, got %d", len(trades), len(got))
	}
	for i, tr := range trades {
		if got[i].ClientOrderID != tr.ClientOrderID ||
			got[i].BrokerOrderID != tr.BrokerOrderID ||
			got[i].Symbol != tr.Symbol ||
			got[i].Side != tr.Side ||
			got[i].Quantity != tr.Quantity ||
			got[i].RequestedQuantity != tr.RequestedQuantity ||
			got[i].Status != tr.Status ||
			got[i].Strategy != tr.Strategy ||
			got[i].Mode != tr.Mode ||
			got[i].Note != tr.Note {
			t.Errorf("row %d mismatch: got %+v want %+v", i, got[i], tr)
		}
		if diff := got[i].Price - tr.Price; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("row %d price mismatch: got %v want %v", i, got[i].Price, tr.Price)
		}
		if !got[i].Timestamp.Equal(tr.Timestamp) {
			t.Errorf("row %d timestamp mismatch: got %v want %v", i, got[i].Timestamp, tr.Timestamp)
		}
	}
}

// TestCrashRestartPreservesRows simulates a process crash/restart: write
// rows, close the DB (as a normal shutdown would, but we intentionally do
// NOT call any "flush buffered writes" step because there must be none),
// reopen the SAME file, and verify every row is still present. This is the
// core acceptance test for EX-8 (CSV truncation destroyed the audit trail
// on restart) — the ledger must not lose data across a restart.
func TestCrashRestartPreservesRows(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "titan_ledger.db")

	base := time.Date(2026, 7, 18, 9, 20, 0, 0, time.UTC)
	trades := sampleTrades(base)

	// "Session 1": open, append, close (simulating a live process that then
	// stops or crashes after committing its writes).
	l1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (session 1) failed: %v", err)
	}
	for _, tr := range trades[:2] {
		if err := l1.Append(tr); err != nil {
			t.Fatalf("Append failed: %v", err)
		}
	}
	if err := l1.Close(); err != nil {
		t.Fatalf("Close (session 1) failed: %v", err)
	}

	// "Session 2": reopen the same file (simulating process restart) and
	// append more rows.
	l2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (session 2) failed: %v", err)
	}
	if err := l2.Append(trades[2]); err != nil {
		t.Fatalf("Append (session 2) failed: %v", err)
	}

	got, err := l2.Query(DateRange{})
	if err != nil {
		t.Fatalf("Query (session 2) failed: %v", err)
	}
	if err := l2.Close(); err != nil {
		t.Fatalf("Close (session 2) failed: %v", err)
	}

	if len(got) != len(trades) {
		t.Fatalf("after restart: expected %d rows, got %d — rows were lost across restart", len(trades), len(got))
	}
	seen := map[string]bool{}
	for _, r := range got {
		seen[r.ClientOrderID+"|"+string(r.Status)] = true
	}
	for _, tr := range trades {
		key := tr.ClientOrderID + "|" + string(tr.Status)
		if !seen[key] {
			t.Errorf("row %s missing after restart", key)
		}
	}

	// "Session 3": reopen once more with no writes, prove data is still
	// intact (the file itself, not just an in-memory cache, is durable).
	l3, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (session 3) failed: %v", err)
	}
	defer l3.Close()
	got3, err := l3.Query(DateRange{})
	if err != nil {
		t.Fatalf("Query (session 3) failed: %v", err)
	}
	if len(got3) != len(trades) {
		t.Fatalf("after second restart: expected %d rows, got %d", len(trades), len(got3))
	}
}

func TestAppendRejectsInvalidStatus(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "titan_ledger.db")
	l, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer l.Close()

	tr := Trade{
		Timestamp:     time.Now(),
		ClientOrderID: "titan-bad-1",
		Symbol:        "NIFTY",
		Side:          "BUY",
		Status:        Status("bogus"),
		Mode:          ModePaper,
	}
	if err := l.Append(tr); err == nil {
		t.Fatal("expected error for invalid status, got nil")
	}

	tr.Status = StatusIntent
	tr.Mode = Mode("bogus-mode")
	if err := l.Append(tr); err == nil {
		t.Fatal("expected error for invalid mode, got nil")
	}

	tr.Mode = ModePaper
	tr.ClientOrderID = ""
	if err := l.Append(tr); err == nil {
		t.Fatal("expected error for empty client_order_id, got nil")
	}
}

func TestExportCSVMatchesInsertedRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "titan_ledger.db")
	l, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer l.Close()

	base := time.Date(2026, 7, 18, 9, 20, 0, 0, time.UTC)
	trades := sampleTrades(base)
	for _, tr := range trades {
		if err := l.Append(tr); err != nil {
			t.Fatalf("Append failed: %v", err)
		}
	}

	exportPath := filepath.Join(t.TempDir(), "export.csv")
	dr := DateRange{From: base.Add(-time.Minute), To: base.Add(2 * time.Hour)}
	if err := l.ExportCSV(dr, exportPath); err != nil {
		t.Fatalf("ExportCSV failed: %v", err)
	}

	f, err := os.Open(exportPath)
	if err != nil {
		t.Fatalf("failed to open export file: %v", err)
	}
	defer f.Close()

	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("failed to parse export CSV: %v", err)
	}
	if len(rows) != len(trades)+1 { // +1 header
		t.Fatalf("expected %d CSV rows (incl. header), got %d", len(trades)+1, len(rows))
	}
	if rows[0][2] != "ClientOrderID" || rows[0][9] != "Status" {
		t.Fatalf("unexpected header: %v", rows[0])
	}
	for i, tr := range trades {
		row := rows[i+1]
		if row[2] != tr.ClientOrderID {
			t.Errorf("row %d ClientOrderID: got %s want %s", i, row[2], tr.ClientOrderID)
		}
		if row[3] != tr.BrokerOrderID {
			t.Errorf("row %d BrokerOrderID: got %s want %s", i, row[3], tr.BrokerOrderID)
		}
		if row[4] != tr.Symbol {
			t.Errorf("row %d Symbol: got %s want %s", i, row[4], tr.Symbol)
		}
		if row[9] != string(tr.Status) {
			t.Errorf("row %d Status: got %s want %s", i, row[9], tr.Status)
		}
	}

	// Narrower range excludes the trade an hour later.
	dr2 := DateRange{From: base.Add(-time.Minute), To: base.Add(time.Minute)}
	exportPath2 := filepath.Join(t.TempDir(), "export2.csv")
	if err := l.ExportCSV(dr2, exportPath2); err != nil {
		t.Fatalf("ExportCSV (narrow range) failed: %v", err)
	}
	f2, err := os.Open(exportPath2)
	if err != nil {
		t.Fatalf("failed to open export2 file: %v", err)
	}
	defer f2.Close()
	rows2, err := csv.NewReader(f2).ReadAll()
	if err != nil {
		t.Fatalf("failed to parse export2 CSV: %v", err)
	}
	if len(rows2) != 3 { // header + 2 rows within +/- 1 minute of base
		t.Fatalf("expected 3 rows (header + 2), got %d: %v", len(rows2), rows2)
	}
}

func TestOpenDefaultPathCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "nested", "sub", "titan_ledger.db")
	l, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer l.Close()

	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected db file to exist at %s: %v", dbPath, err)
	}
	if l.Path() != dbPath {
		t.Fatalf("Path() = %s, want %s", l.Path(), dbPath)
	}
}
