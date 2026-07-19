// Package ledger implements the durable, append-only trade record for
// TitanAlgo (WP-5, audit finding EX-8).
//
// The previous system of record was a CSV file opened with O_TRUNC, which
// wiped the entire trade history on every process restart. For a real-money
// system that destroys the audit trail exactly when a crash/restart makes
// it most necessary.
//
// This package makes a single SQLite database file (via the pure-Go
// modernc.org/sqlite driver — no cgo) the system of record. Every write is a
// synchronous, transactional INSERT; rows are never updated or deleted, so
// the full lifecycle of an order (intent -> filled/partial/rejected/
// indeterminate -> reconciled) is preserved as a sequence of rows sharing
// the same ClientOrderID. Callers should append a new row for every status
// transition rather than mutating history.
//
// This package intentionally has no dependency on internal/broker or any
// other TitanAlgo package (leaf library), matching the convention used by
// WP-3's internal/state package. Callers convert broker-specific types
// (order side, status, etc.) to the plain strings used here.
package ledger

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// DefaultDBPath is the default location of the ledger database file, used
// when an empty path is passed to Open.
const DefaultDBPath = "data/titan_ledger.db"

// Status is the enum-like set of values the "status" column may hold.
// Rows are append-only: a status transition is recorded as a NEW row with
// the same ClientOrderID, not an update of an existing row.
type Status string

const (
	// StatusIntent marks the row written BEFORE the order is sent to the
	// broker (the durable intent record used for indeterminate-order
	// recovery).
	StatusIntent Status = "intent"
	// StatusFilled marks a fully filled order.
	StatusFilled Status = "filled"
	// StatusPartial marks a partially filled order.
	StatusPartial Status = "partial"
	// StatusRejected marks a broker-rejected order.
	StatusRejected Status = "rejected"
	// StatusIndeterminate marks an order whose outcome is unknown (e.g. the
	// fill-poll budget was exhausted without a terminal status).
	StatusIndeterminate Status = "indeterminate"
	// StatusReconciled marks a row written by the reconciliation process to
	// record the resolved truth for a previously indeterminate/ambiguous
	// order.
	StatusReconciled Status = "reconciled"
)

// ValidStatuses returns the complete set of accepted Status values, in a
// stable order suitable for error messages.
func ValidStatuses() []Status {
	return []Status{
		StatusIntent,
		StatusFilled,
		StatusPartial,
		StatusRejected,
		StatusIndeterminate,
		StatusReconciled,
	}
}

// IsValid reports whether s is one of the accepted Status values.
func (s Status) IsValid() bool {
	switch s {
	case StatusIntent, StatusFilled, StatusPartial, StatusRejected, StatusIndeterminate, StatusReconciled:
		return true
	default:
		return false
	}
}

// Mode identifies whether a trade row was produced by paper or live trading.
type Mode string

const (
	ModePaper Mode = "paper"
	ModeLive  Mode = "live"
)

// Trade is one row of the append-only trades table.
type Trade struct {
	// ID is populated by the database on read (0 for rows not yet
	// persisted / on Append input, it is ignored).
	ID int64

	Timestamp         time.Time
	ClientOrderID     string
	BrokerOrderID     string
	Symbol            string
	Side              string // "BUY" / "SELL" (caller-defined; not validated here)
	Quantity          int    // actually executed quantity
	RequestedQuantity int    // originally requested quantity
	Price             float64
	Status            Status
	Charges           float64
	RealizedPnL       float64
	Strategy          string
	Mode              Mode
	Note              string
}

// DateRange is an inclusive [From, To] timestamp range used by ExportCSV
// and Query.
type DateRange struct {
	From time.Time
	To   time.Time
}

// Ledger is a handle to the append-only SQLite trade ledger. It is safe for
// concurrent use.
type Ledger struct {
	mu   sync.Mutex
	db   *sql.DB
	path string
}

// Open opens (creating if necessary) the SQLite ledger database at dbPath.
// If dbPath is empty, DefaultDBPath ("data/titan_ledger.db") is used. The
// parent directory is created if it does not exist.
//
// Open configures the connection for durability: WAL journal mode (so a
// crash mid-write cannot corrupt the database) and synchronous=FULL (every
// commit is fsynced before returning), and restricts the pool to a single
// connection so writes are serialized at the database-connection level in
// addition to the in-process mutex.
func Open(dbPath string) (*Ledger, error) {
	if dbPath == "" {
		dbPath = DefaultDBPath
	}

	if dir := filepath.Dir(dbPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("ledger: failed to create db directory %q: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("ledger: failed to open db %q: %w", dbPath, err)
	}

	// Synchronous, transactional writes with no lost data on crash: WAL for
	// crash-safety + concurrent readers, FULL sync so every commit is
	// durable on disk before we report success. Single connection so the
	// in-process mutex and the DB serialize identically (avoids
	// SQLITE_BUSY under -race/concurrent callers).
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode = WAL;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger: failed to set WAL mode: %w", err)
	}
	if _, err := db.Exec(`PRAGMA synchronous = FULL;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger: failed to set synchronous=FULL: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger: failed to enable foreign_keys: %w", err)
	}

	l := &Ledger{db: db, path: dbPath}
	if err := l.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return l, nil
}

func (l *Ledger) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS trades (
	id                  INTEGER PRIMARY KEY AUTOINCREMENT,
	timestamp           TEXT    NOT NULL,
	client_order_id     TEXT    NOT NULL,
	broker_order_id     TEXT    NOT NULL DEFAULT '',
	symbol              TEXT    NOT NULL,
	side                TEXT    NOT NULL,
	quantity            INTEGER NOT NULL,
	requested_quantity  INTEGER NOT NULL,
	price               REAL    NOT NULL,
	status              TEXT    NOT NULL,
	charges             REAL    NOT NULL DEFAULT 0,
	realized_pnl        REAL    NOT NULL DEFAULT 0,
	strategy            TEXT    NOT NULL DEFAULT '',
	mode                TEXT    NOT NULL,
	note                TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_trades_timestamp       ON trades(timestamp);
CREATE INDEX IF NOT EXISTS idx_trades_client_order_id ON trades(client_order_id);
`
	if _, err := l.db.Exec(schema); err != nil {
		return fmt.Errorf("ledger: schema migration failed: %w", err)
	}
	return nil
}

// Append durably records t as a new row in the trades table. The write is
// wrapped in an explicit transaction and committed synchronously (with
// synchronous=FULL, the commit is fsynced before Append returns), so a
// successful return guarantees the row survives a subsequent crash.
//
// Append never updates or deletes existing rows: to record a status
// transition for the same order, call Append again with the same
// ClientOrderID and the new Status. This keeps the full lifecycle of every
// order visible in the ledger.
func (l *Ledger) Append(t Trade) error {
	if t.Status == "" {
		return fmt.Errorf("ledger: status is required")
	}
	if !t.Status.IsValid() {
		return fmt.Errorf("ledger: invalid status %q (valid: %v)", t.Status, ValidStatuses())
	}
	if t.ClientOrderID == "" {
		return fmt.Errorf("ledger: client_order_id is required")
	}
	if t.Mode != ModePaper && t.Mode != ModeLive {
		return fmt.Errorf("ledger: invalid mode %q (valid: %q, %q)", t.Mode, ModePaper, ModeLive)
	}
	if t.Timestamp.IsZero() {
		t.Timestamp = time.Now()
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	tx, err := l.db.Begin()
	if err != nil {
		return fmt.Errorf("ledger: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	_, err = tx.Exec(
		`INSERT INTO trades (
			timestamp, client_order_id, broker_order_id, symbol, side,
			quantity, requested_quantity, price, status, charges,
			realized_pnl, strategy, mode, note
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Timestamp.UTC().Format(time.RFC3339Nano),
		t.ClientOrderID,
		t.BrokerOrderID,
		t.Symbol,
		t.Side,
		t.Quantity,
		t.RequestedQuantity,
		t.Price,
		string(t.Status),
		t.Charges,
		t.RealizedPnL,
		t.Strategy,
		string(t.Mode),
		t.Note,
	)
	if err != nil {
		return fmt.Errorf("ledger: insert failed: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ledger: commit failed: %w", err)
	}
	return nil
}

// Query returns all trades whose timestamp falls within the inclusive
// [dateRange.From, dateRange.To] window, ordered by timestamp then id
// ascending. A zero-value From or To leaves that side of the range
// unbounded.
func (l *Ledger) Query(dateRange DateRange) ([]Trade, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	from := dateRange.From
	to := dateRange.To
	if to.IsZero() {
		to = time.Now().AddDate(100, 0, 0) // effectively unbounded upper edge
	}

	rows, err := l.db.Query(
		`SELECT id, timestamp, client_order_id, broker_order_id, symbol, side,
			quantity, requested_quantity, price, status, charges,
			realized_pnl, strategy, mode, note
		 FROM trades
		 WHERE timestamp >= ? AND timestamp <= ?
		 ORDER BY timestamp ASC, id ASC`,
		from.UTC().Format(time.RFC3339Nano),
		to.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, fmt.Errorf("ledger: query failed: %w", err)
	}
	defer rows.Close()

	var out []Trade
	for rows.Next() {
		var t Trade
		var ts, status, mode string
		if err := rows.Scan(
			&t.ID, &ts, &t.ClientOrderID, &t.BrokerOrderID, &t.Symbol, &t.Side,
			&t.Quantity, &t.RequestedQuantity, &t.Price, &status, &t.Charges,
			&t.RealizedPnL, &t.Strategy, &mode, &t.Note,
		); err != nil {
			return nil, fmt.Errorf("ledger: scan failed: %w", err)
		}
		parsedTS, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return nil, fmt.Errorf("ledger: bad timestamp %q in row %d: %w", ts, t.ID, err)
		}
		t.Timestamp = parsedTS
		t.Status = Status(status)
		t.Mode = Mode(mode)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: row iteration failed: %w", err)
	}
	return out, nil
}

// csvHeader is the column order used by ExportCSV.
var csvHeader = []string{
	"ID", "Timestamp", "ClientOrderID", "BrokerOrderID", "Symbol", "Side",
	"Quantity", "RequestedQuantity", "Price", "Status", "Charges",
	"RealizedPnL", "Strategy", "Mode", "Note",
}

// ExportCSV dumps all ledger rows whose timestamp falls within dateRange to
// a CSV file at path, replacing any existing file at that path. This is the
// intended way to produce a point-in-time trade record for compliance /
// contract-note reconciliation — the SQLite database remains the system of
// record; the CSV is a derived export.
func (l *Ledger) ExportCSV(dateRange DateRange, path string) error {
	trades, err := l.Query(dateRange)
	if err != nil {
		return fmt.Errorf("ledger: export query failed: %w", err)
	}

	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("ledger: failed to create export directory %q: %w", dir, err)
		}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("ledger: failed to create export file %q: %w", path, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if err := w.Write(csvHeader); err != nil {
		return fmt.Errorf("ledger: failed to write export header: %w", err)
	}
	for _, t := range trades {
		record := []string{
			fmt.Sprintf("%d", t.ID),
			t.Timestamp.UTC().Format(time.RFC3339Nano),
			t.ClientOrderID,
			t.BrokerOrderID,
			t.Symbol,
			t.Side,
			fmt.Sprintf("%d", t.Quantity),
			fmt.Sprintf("%d", t.RequestedQuantity),
			fmt.Sprintf("%.2f", t.Price),
			string(t.Status),
			fmt.Sprintf("%.2f", t.Charges),
			fmt.Sprintf("%.2f", t.RealizedPnL),
			t.Strategy,
			string(t.Mode),
			t.Note,
		}
		if err := w.Write(record); err != nil {
			return fmt.Errorf("ledger: failed to write export row: %w", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("ledger: csv writer error: %w", err)
	}
	return nil
}

// Close closes the underlying database handle.
func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.db.Close()
}

// Path returns the filesystem path of the open database file.
func (l *Ledger) Path() string {
	return l.path
}
