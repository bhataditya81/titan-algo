// Package state is the durable persistence layer for TitanAlgo's
// risk/position/order/strategy state (audit findings CR-8, CR-7, ST-4).
//
// It is a LEAF library: it must never import internal/broker,
// internal/engine, or internal/strategy. Other packages depend on it, not
// the other way around. It is fully testable with temp files and has no
// network or broker dependency.
//
// Every mutation is a synchronous, transactional SQLite write in WAL mode,
// so a crash or `kill -9` at any point leaves the database in a consistent
// state reflecting the last committed write — never a half-written record.
package state

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registers itself as "sqlite"

	"titan-algo/models"
)

// DefaultDBPath is used when Open is called with an empty path.
const DefaultDBPath = "data/titan_state.db"

// Store is the durable state store. All exported methods are safe for
// concurrent use by multiple goroutines: writes are serialized with an
// internal mutex (SQLite in WAL mode allows only one writer at a time
// anyway, so this simply avoids busy-retry churn) and reads use the
// underlying *sql.DB's own connection pool.
type Store struct {
	db   *sql.DB
	mu   sync.Mutex // serializes writers; WAL allows concurrent readers
	path string

	orderSeq    uint64 // atomic counter feeding NextClientOrderID
	positionSeq uint64 // atomic counter feeding NextPositionID
}

// Open opens (creating if necessary) a SQLite-backed Store at path. If path
// is empty, DefaultDBPath is used. The parent directory is created if it
// does not exist. WAL mode is enabled for concurrent-safe durability, and
// synchronous=FULL is used because this store may hold the only durable
// record of live option positions — throughput is not the priority here,
// crash-safety is.
func Open(path string) (*Store, error) {
	if path == "" {
		path = DefaultDBPath
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("state: create db directory %q: %w", dir, err)
		}
	}

	// A single shared connection avoids "database is locked" errors that
	// can occur with SQLite + WAL under multiple concurrent writer
	// connections from the same process; internal/state also serializes
	// writers itself via mu for clarity and to keep transactions atomic
	// end-to-end.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("state: open db %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	s := &Store{db: db, path: path}

	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}

	return s, nil
}

// Path returns the filesystem path this Store was opened against.
func (s *Store) Path() string { return s.path }

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) init() error {
	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=FULL;",
		"PRAGMA foreign_keys=ON;",
	}
	for _, p := range pragmas {
		if _, err := s.db.Exec(p); err != nil {
			return fmt.Errorf("state: pragma %q: %w", p, err)
		}
	}

	schema := []string{
		`CREATE TABLE IF NOT EXISTS positions (
			id              TEXT PRIMARY KEY,
			symbol          TEXT NOT NULL,
			exchange        TEXT NOT NULL DEFAULT '',
			side            TEXT NOT NULL,
			quantity        INTEGER NOT NULL,
			entry_price     REAL NOT NULL,
			exit_price      REAL NOT NULL DEFAULT 0,
			strategy        TEXT NOT NULL DEFAULT '',
			status          TEXT NOT NULL,
			entry_time      TEXT NOT NULL,
			exit_time       TEXT,
			broker_order_id TEXT NOT NULL DEFAULT '',
			note            TEXT NOT NULL DEFAULT ''
		);`,
		`CREATE INDEX IF NOT EXISTS idx_positions_status ON positions(status);`,
		`CREATE INDEX IF NOT EXISTS idx_positions_symbol ON positions(symbol);`,

		`CREATE TABLE IF NOT EXISTS order_attempts (
			client_order_id TEXT PRIMARY KEY,
			symbol          TEXT NOT NULL,
			side            TEXT NOT NULL,
			quantity        INTEGER NOT NULL,
			price           REAL NOT NULL DEFAULT 0,
			order_type      TEXT NOT NULL DEFAULT '',
			strategy        TEXT NOT NULL DEFAULT '',
			status          TEXT NOT NULL,
			broker_order_id TEXT NOT NULL DEFAULT '',
			filled_quantity INTEGER NOT NULL DEFAULT 0,
			filled_price    REAL NOT NULL DEFAULT 0,
			error           TEXT NOT NULL DEFAULT '',
			created_at      TEXT NOT NULL,
			resolved_at     TEXT
		);`,
		`CREATE INDEX IF NOT EXISTS idx_order_attempts_status ON order_attempts(status);`,

		`CREATE TABLE IF NOT EXISTS risk_snapshot (
			id           INTEGER PRIMARY KEY CHECK (id = 1),
			balance      REAL NOT NULL,
			realized_pnl REAL NOT NULL,
			session_used REAL NOT NULL,
			updated_at   TEXT NOT NULL
		);`,

		`CREATE TABLE IF NOT EXISTS strategy_state (
			strategy   TEXT NOT NULL,
			key        TEXT NOT NULL,
			value      TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (strategy, key)
		);`,
	}
	for _, stmt := range schema {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("state: schema init: %w", err)
		}
	}
	return nil
}

// withTx runs fn inside a synchronous transaction, serialized against other
// writers on this Store, and commits iff fn returns nil.
func (s *Store) withTx(fn func(tx *sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("state: begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: commit tx: %w", err)
	}
	return nil
}

func timeToStr(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func strToTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func nullableStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// ---- Client order ID / position ID generation --------------------------

// NextClientOrderID returns a new, process-unique client order ID of the
// form "titan-<unixnano>-<seq>". Callers should generate this BEFORE
// placing an order over the network, persist it via SaveOrderAttempt, and
// only then issue the broker call — see the OrderAttempt doc comment in
// models/trade.go for the full intended usage pattern.
func (s *Store) NextClientOrderID() string {
	n := atomic.AddUint64(&s.orderSeq, 1)
	return fmt.Sprintf("titan-%d-%d", time.Now().UnixNano(), n)
}

// NextPositionID returns a new, process-unique position ID of the form
// "pos-<unixnano>-<seq>", for callers that don't want to choose their own
// Position.ID (e.g. reusing the client order ID that opened the position is
// also a perfectly valid choice).
func (s *Store) NextPositionID() string {
	n := atomic.AddUint64(&s.positionSeq, 1)
	return fmt.Sprintf("pos-%d-%d", time.Now().UnixNano(), n)
}

// ---- Positions -----------------------------------------------------------

// SavePosition persists a position (insert or full replace by ID) as an
// open position record. If p.ID is empty, a new ID is generated via
// NextPositionID and written back into a copy (the caller's original struct
// is not mutated). If p.Status is empty, it defaults to PositionOpen. If
// p.EntryTime is zero, it defaults to time.Now().UTC().
func (s *Store) SavePosition(p models.Position) error {
	if p.ID == "" {
		p.ID = s.NextPositionID()
	}
	if p.Status == "" {
		p.Status = models.PositionOpen
	}
	if p.EntryTime.IsZero() {
		p.EntryTime = time.Now().UTC()
	}

	return s.withTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO positions (id, symbol, exchange, side, quantity, entry_price, exit_price, strategy, status, entry_time, exit_time, broker_order_id, note)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				symbol=excluded.symbol, exchange=excluded.exchange, side=excluded.side,
				quantity=excluded.quantity, entry_price=excluded.entry_price, exit_price=excluded.exit_price,
				strategy=excluded.strategy, status=excluded.status, entry_time=excluded.entry_time,
				exit_time=excluded.exit_time, broker_order_id=excluded.broker_order_id, note=excluded.note
		`,
			p.ID, p.Symbol, p.Exchange, string(p.Side), p.Quantity, p.EntryPrice, p.ExitPrice,
			p.Strategy, string(p.Status), timeToStr(p.EntryTime), nullableStr(timeToStr(p.ExitTime)),
			p.BrokerOrderID, p.Note,
		)
		if err != nil {
			return fmt.Errorf("state: save position %q: %w", p.ID, err)
		}
		return nil
	})
}

// ClosePosition marks the position identified by id as closed, recording
// the exit price and exit time (time.Now().UTC() if exitTime is zero). It
// returns an error if no position with that ID exists.
func (s *Store) ClosePosition(id string, exitPrice float64, exitTime time.Time) error {
	if exitTime.IsZero() {
		exitTime = time.Now().UTC()
	}
	return s.withTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`
			UPDATE positions SET status = ?, exit_price = ?, exit_time = ?
			WHERE id = ?
		`, string(models.PositionClosed), exitPrice, timeToStr(exitTime), id)
		if err != nil {
			return fmt.Errorf("state: close position %q: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("state: close position %q: rows affected: %w", id, err)
		}
		if n == 0 {
			return fmt.Errorf("state: close position %q: %w", id, sql.ErrNoRows)
		}
		return nil
	})
}

// ListOpenPositions returns all positions currently marked OPEN, ordered by
// entry time ascending.
func (s *Store) ListOpenPositions() ([]models.Position, error) {
	rows, err := s.db.Query(`
		SELECT id, symbol, exchange, side, quantity, entry_price, exit_price, strategy, status, entry_time, exit_time, broker_order_id, note
		FROM positions WHERE status = ? ORDER BY entry_time ASC
	`, string(models.PositionOpen))
	if err != nil {
		return nil, fmt.Errorf("state: list open positions: %w", err)
	}
	defer rows.Close()

	var out []models.Position
	for rows.Next() {
		p, err := scanPosition(rows)
		if err != nil {
			return nil, fmt.Errorf("state: list open positions: scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list open positions: %w", err)
	}
	return out, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanPosition(row rowScanner) (models.Position, error) {
	var (
		p            models.Position
		side, status string
		entryTimeStr string
		exitTimeStr  sql.NullString
	)
	if err := row.Scan(
		&p.ID, &p.Symbol, &p.Exchange, &side, &p.Quantity, &p.EntryPrice, &p.ExitPrice,
		&p.Strategy, &status, &entryTimeStr, &exitTimeStr, &p.BrokerOrderID, &p.Note,
	); err != nil {
		return models.Position{}, err
	}
	p.Side = models.PositionSide(side)
	p.Status = models.PositionStatus(status)
	p.EntryTime = strToTime(entryTimeStr)
	if exitTimeStr.Valid {
		p.ExitTime = strToTime(exitTimeStr.String)
	}
	return p, nil
}

// ---- Order attempts --------------------------------------------------------

// SaveOrderAttempt persists an order intent or its current known state,
// keyed by attempt.ClientOrderID. Callers MUST call this with
// Status = models.OrderIntent BEFORE issuing the order over the network —
// see models.OrderAttempt's doc comment. If attempt.CreatedAt is zero, it
// defaults to time.Now().UTC().
func (s *Store) SaveOrderAttempt(attempt models.OrderAttempt) error {
	if attempt.ClientOrderID == "" {
		return fmt.Errorf("state: save order attempt: ClientOrderID is required")
	}
	if attempt.CreatedAt.IsZero() {
		attempt.CreatedAt = time.Now().UTC()
	}
	if attempt.Status == "" {
		attempt.Status = models.OrderIntent
	}

	return s.withTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO order_attempts (client_order_id, symbol, side, quantity, price, order_type, strategy, status, broker_order_id, filled_quantity, filled_price, error, created_at, resolved_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(client_order_id) DO UPDATE SET
				symbol=excluded.symbol, side=excluded.side, quantity=excluded.quantity, price=excluded.price,
				order_type=excluded.order_type, strategy=excluded.strategy, status=excluded.status,
				broker_order_id=excluded.broker_order_id, filled_quantity=excluded.filled_quantity,
				filled_price=excluded.filled_price, error=excluded.error, resolved_at=excluded.resolved_at
		`,
			attempt.ClientOrderID, attempt.Symbol, string(attempt.Side), attempt.Quantity, attempt.Price,
			attempt.OrderType, attempt.Strategy, string(attempt.Status), attempt.BrokerOrderID,
			attempt.FilledQuantity, attempt.FilledPrice, attempt.Error,
			timeToStr(attempt.CreatedAt), nullableStr(timeToStr(attempt.ResolvedAt)),
		)
		if err != nil {
			return fmt.Errorf("state: save order attempt %q: %w", attempt.ClientOrderID, err)
		}
		return nil
	})
}

// MarkOrderResolved records the final (or latest known) outcome of a
// previously-saved order attempt: status (filled/partial/rejected/
// indeterminate/cancelled), the broker order ID if known, the filled
// quantity/price, and an error message if any. resolvedAt defaults to
// time.Now().UTC() if zero. Returns an error if clientOrderID was never
// saved via SaveOrderAttempt.
func (s *Store) MarkOrderResolved(clientOrderID string, status models.OrderAttemptStatus, brokerOrderID string, filledQuantity int, filledPrice float64, errMsg string, resolvedAt time.Time) error {
	if resolvedAt.IsZero() {
		resolvedAt = time.Now().UTC()
	}
	return s.withTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`
			UPDATE order_attempts
			SET status = ?, broker_order_id = ?, filled_quantity = ?, filled_price = ?, error = ?, resolved_at = ?
			WHERE client_order_id = ?
		`, string(status), brokerOrderID, filledQuantity, filledPrice, errMsg, timeToStr(resolvedAt), clientOrderID)
		if err != nil {
			return fmt.Errorf("state: mark order resolved %q: %w", clientOrderID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("state: mark order resolved %q: rows affected: %w", clientOrderID, err)
		}
		if n == 0 {
			return fmt.Errorf("state: mark order resolved %q: %w", clientOrderID, sql.ErrNoRows)
		}
		return nil
	})
}

// GetOrderAttempt returns the order attempt for clientOrderID. The second
// return value is false if no such attempt exists.
func (s *Store) GetOrderAttempt(clientOrderID string) (models.OrderAttempt, bool, error) {
	row := s.db.QueryRow(`
		SELECT client_order_id, symbol, side, quantity, price, order_type, strategy, status, broker_order_id, filled_quantity, filled_price, error, created_at, resolved_at
		FROM order_attempts WHERE client_order_id = ?
	`, clientOrderID)

	var (
		a             models.OrderAttempt
		side, status  string
		createdAtStr  string
		resolvedAtStr sql.NullString
	)
	err := row.Scan(
		&a.ClientOrderID, &a.Symbol, &side, &a.Quantity, &a.Price, &a.OrderType, &a.Strategy, &status,
		&a.BrokerOrderID, &a.FilledQuantity, &a.FilledPrice, &a.Error, &createdAtStr, &resolvedAtStr,
	)
	if err == sql.ErrNoRows {
		return models.OrderAttempt{}, false, nil
	}
	if err != nil {
		return models.OrderAttempt{}, false, fmt.Errorf("state: get order attempt %q: %w", clientOrderID, err)
	}
	a.Side = models.PositionSide(side)
	a.Status = models.OrderAttemptStatus(status)
	a.CreatedAt = strToTime(createdAtStr)
	if resolvedAtStr.Valid {
		a.ResolvedAt = strToTime(resolvedAtStr.String)
	}
	return a, true, nil
}

// ListUnresolvedOrderAttempts returns every order attempt still in the
// "intent" or "indeterminate" state — i.e. attempts a startup recovery pass
// needs to resolve against the broker's order book before trading resumes.
func (s *Store) ListUnresolvedOrderAttempts() ([]models.OrderAttempt, error) {
	rows, err := s.db.Query(`
		SELECT client_order_id, symbol, side, quantity, price, order_type, strategy, status, broker_order_id, filled_quantity, filled_price, error, created_at, resolved_at
		FROM order_attempts WHERE status IN (?, ?) ORDER BY created_at ASC
	`, string(models.OrderIntent), string(models.OrderIndeterminate))
	if err != nil {
		return nil, fmt.Errorf("state: list unresolved order attempts: %w", err)
	}
	defer rows.Close()

	var out []models.OrderAttempt
	for rows.Next() {
		var (
			a             models.OrderAttempt
			side, status  string
			createdAtStr  string
			resolvedAtStr sql.NullString
		)
		if err := rows.Scan(
			&a.ClientOrderID, &a.Symbol, &side, &a.Quantity, &a.Price, &a.OrderType, &a.Strategy, &status,
			&a.BrokerOrderID, &a.FilledQuantity, &a.FilledPrice, &a.Error, &createdAtStr, &resolvedAtStr,
		); err != nil {
			return nil, fmt.Errorf("state: list unresolved order attempts: scan: %w", err)
		}
		a.Side = models.PositionSide(side)
		a.Status = models.OrderAttemptStatus(status)
		a.CreatedAt = strToTime(createdAtStr)
		if resolvedAtStr.Valid {
			a.ResolvedAt = strToTime(resolvedAtStr.String)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list unresolved order attempts: %w", err)
	}
	return out, nil
}

// ---- Risk snapshot ---------------------------------------------------------

// SaveRiskSnapshot persists the risk manager's current balance, realized
// P&L, and capital locked in open positions (sessionUsed) as a single-row
// snapshot, overwriting any previous snapshot. UpdatedAt is set to
// time.Now().UTC().
func (s *Store) SaveRiskSnapshot(balance, realizedPnL, sessionUsed float64) error {
	now := time.Now().UTC()
	return s.withTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO risk_snapshot (id, balance, realized_pnl, session_used, updated_at)
			VALUES (1, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				balance=excluded.balance, realized_pnl=excluded.realized_pnl,
				session_used=excluded.session_used, updated_at=excluded.updated_at
		`, balance, realizedPnL, sessionUsed, timeToStr(now))
		if err != nil {
			return fmt.Errorf("state: save risk snapshot: %w", err)
		}
		return nil
	})
}

// LoadRiskSnapshot returns the most recently saved risk snapshot. If no
// snapshot has ever been saved (first run), it returns a zero-value
// RiskSnapshot and a nil error — callers should treat that as "start
// fresh", not as a failure.
func (s *Store) LoadRiskSnapshot() (models.RiskSnapshot, error) {
	row := s.db.QueryRow(`SELECT balance, realized_pnl, session_used, updated_at FROM risk_snapshot WHERE id = 1`)
	var (
		snap         models.RiskSnapshot
		updatedAtStr string
	)
	err := row.Scan(&snap.Balance, &snap.RealizedPnL, &snap.SessionUsed, &updatedAtStr)
	if err == sql.ErrNoRows {
		return models.RiskSnapshot{}, nil
	}
	if err != nil {
		return models.RiskSnapshot{}, fmt.Errorf("state: load risk snapshot: %w", err)
	}
	snap.UpdatedAt = strToTime(updatedAtStr)
	return snap, nil
}

// ---- Strategy state (key/value) --------------------------------------------

// SaveStrategyState persists an arbitrary string value under
// (strategyName, key), e.g. SaveStrategyState("nine_twenty", "entered",
// "true"). Values are opaque strings; callers own their own encoding
// (booleans as "true"/"false", numbers via strconv, timestamps via
// time.RFC3339, etc).
func (s *Store) SaveStrategyState(strategyName, key, value string) error {
	if strategyName == "" || key == "" {
		return fmt.Errorf("state: save strategy state: strategyName and key are required")
	}
	now := time.Now().UTC()
	return s.withTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO strategy_state (strategy, key, value, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(strategy, key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at
		`, strategyName, key, value, timeToStr(now))
		if err != nil {
			return fmt.Errorf("state: save strategy state %q/%q: %w", strategyName, key, err)
		}
		return nil
	})
}

// LoadStrategyState returns the value previously saved under
// (strategyName, key). found is false if nothing has been saved for that
// key yet (this is not an error — strategies should treat it as "no prior
// state, use defaults").
func (s *Store) LoadStrategyState(strategyName, key string) (value string, found bool, err error) {
	row := s.db.QueryRow(`SELECT value FROM strategy_state WHERE strategy = ? AND key = ?`, strategyName, key)
	err = row.Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("state: load strategy state %q/%q: %w", strategyName, key, err)
	}
	return value, true, nil
}

// LoadAllStrategyState returns every key/value pair persisted for
// strategyName, e.g. for a strategy's Restore(map[string]string) hook.
func (s *Store) LoadAllStrategyState(strategyName string) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM strategy_state WHERE strategy = ?`, strategyName)
	if err != nil {
		return nil, fmt.Errorf("state: load all strategy state %q: %w", strategyName, err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("state: load all strategy state %q: scan: %w", strategyName, err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: load all strategy state %q: %w", strategyName, err)
	}
	return out, nil
}
