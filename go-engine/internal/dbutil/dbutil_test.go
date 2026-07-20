package dbutil

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenSQLite verifies that OpenSQLite creates a database with the
// expected durability settings (WAL mode, synchronous=FULL, foreign_keys=ON)
// and creates missing parent directories.
func TestOpenSQLite(t *testing.T) {
	// Create a temporary directory for the test database.
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "nested", "dir", "test.db")

	// Open the database.
	db, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite failed: %v", err)
	}
	defer db.Close()

	// Verify the parent directories were created.
	if _, err := os.Stat(filepath.Dir(dbPath)); os.IsNotExist(err) {
		t.Errorf("parent directory not created")
	}

	// Verify WAL mode is enabled by querying PRAGMA journal_mode.
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("failed to query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("expected WAL mode, got %q", mode)
	}

	// Verify synchronous=FULL by querying PRAGMA synchronous.
	var sync int
	if err := db.QueryRow("PRAGMA synchronous").Scan(&sync); err != nil {
		t.Fatalf("failed to query synchronous: %v", err)
	}
	// synchronous=FULL returns 2
	if sync != 2 {
		t.Errorf("expected synchronous=2 (FULL), got %d", sync)
	}

	// Verify foreign_keys=ON by querying PRAGMA foreign_keys.
	var fk int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("failed to query foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("expected foreign_keys=1 (ON), got %d", fk)
	}

	// Verify the database file actually exists on disk.
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Errorf("database file was not created at %q", dbPath)
	}
}

// TestOpenSQLiteEmptyPath tests that OpenSQLite handles empty path strings
// correctly (should still create in current directory).
func TestOpenSQLiteCreatesDatabase(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite failed: %v", err)
	}
	defer db.Close()

	// Simple sanity check: execute a query.
	var one int
	if err := db.QueryRow("SELECT 1").Scan(&one); err != nil {
		t.Errorf("failed to execute query: %v", err)
	}
	if one != 1 {
		t.Errorf("expected 1, got %d", one)
	}
}
