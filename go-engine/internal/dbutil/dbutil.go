// Package dbutil provides shared SQLite database utilities for TitanAlgo's
// durable storage packages (state, ledger).
package dbutil

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registers itself as "sqlite"
)

// OpenSQLite opens (creating if necessary) a SQLite database at path,
// configures it for durability (WAL mode, synchronous=FULL, foreign keys on),
// and returns a ready-to-use *sql.DB connection. The parent directory is
// created if it does not exist.
//
// The connection is configured for a single writer (SetMaxOpenConns(1)) to
// serialize writes at the connection level in addition to any higher-level
// synchronization in the caller.
func OpenSQLite(path string) (*sql.DB, error) {
	// Create parent directory if missing.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("dbutil: create db directory %q: %w", dir, err)
		}
	}

	// Open the database.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("dbutil: open db %q: %w", path, err)
	}

	// Single connection to serialize writers.
	db.SetMaxOpenConns(1)

	// Apply durability pragmas.
	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=FULL;",
		"PRAGMA foreign_keys=ON;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("dbutil: pragma %q: %w", p, err)
		}
	}

	return db, nil
}
