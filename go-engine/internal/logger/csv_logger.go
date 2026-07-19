// Package logger provides the SECONDARY CSV trade log (live-tail file the
// dashboard reads). As of WP-5 (audit finding EX-8) the system of record for
// trades is internal/ledger (append-only SQLite); this CSV logger is kept
// only as a human/dashboard-friendly mirror.
//
// WP-5 fixes applied here:
//   - The file is opened in append mode, not the old truncate-on-open mode,
//     so it is no longer wiped on process restart.
//   - Date-stamped filenames (trades_YYYY-MM-DD.csv): each trading day gets
//     its own file.
//   - The header row is written only when the file is newly created (or
//     empty), never re-written on append.
//   - Row format extended with OrderID and Status columns (see csvHeader).
package logger

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"titan-algo/internal/broker"
)

// csvHeader is the current CSV column set. NOTE (dashboard compatibility):
// OrderID and Status were appended in WP-5; files written before WP-5 have
// only the first 10 columns. Consumers should parse tolerantly (treat
// missing trailing columns as empty).
var csvHeader = []string{
	"Timestamp",
	"Symbol",
	"Action",
	"Quantity",
	"FillPrice",
	"Slippage",
	"TransactionFee",
	"BrokerBalance",
	"RiskBalance",
	"NetPnL",
	"OrderID",
	"Status",
}

// CSVLogger logs trades to a date-stamped, append-only CSV file. It is a
// secondary log — the durable record is internal/ledger.
type CSVLogger struct {
	mu       sync.Mutex
	file     *os.File
	writer   *csv.Writer
	filePath string
	logDir   string
	// nowFunc returns the current time; overridable in tests.
	nowFunc func() time.Time
	// curDate is the YYYY-MM-DD the open file belongs to; when the date
	// rolls over, the logger rotates to a new file.
	curDate string
}

// NewCSVLogger creates a CSV logger writing to logDir/trades_YYYY-MM-DD.csv.
// The file is opened in APPEND mode — existing rows are preserved across
// restarts — and the header row is written only if the file is new/empty.
func NewCSVLogger(logDir string) (*CSVLogger, error) {
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	l := &CSVLogger{
		logDir:  logDir,
		nowFunc: time.Now,
	}
	if err := l.openForDate(l.nowFunc()); err != nil {
		return nil, err
	}
	return l, nil
}

// openForDate opens (append mode) the file for the given date, writing the
// header only if the file is newly created or empty. Caller must hold l.mu
// (or be the constructor before the logger is shared).
func (l *CSVLogger) openForDate(now time.Time) error {
	date := now.Format("2006-01-02")
	filePath := filepath.Join(l.logDir, "trades_"+date+".csv")

	// Append mode, never truncate: a restart must never destroy the audit
	// trail (audit finding EX-8).
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open CSV file: %w", err)
	}

	writer := csv.NewWriter(file)

	// Header only when the file is brand new (or empty) — never on append
	// to an existing day file.
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return fmt.Errorf("failed to stat CSV file: %w", err)
	}
	if info.Size() == 0 {
		if err := writer.Write(csvHeader); err != nil {
			file.Close()
			return fmt.Errorf("failed to write header: %w", err)
		}
		writer.Flush()
		if err := writer.Error(); err != nil {
			file.Close()
			return fmt.Errorf("failed to flush header: %w", err)
		}
	}

	l.file = file
	l.writer = writer
	l.filePath = filePath
	l.curDate = date
	return nil
}

// rotateIfNeeded switches to a new date-stamped file when the day changes.
// Caller must hold l.mu.
func (l *CSVLogger) rotateIfNeeded() error {
	now := l.nowFunc()
	if now.Format("2006-01-02") == l.curDate {
		return nil
	}
	l.writer.Flush()
	if err := l.file.Close(); err != nil {
		return fmt.Errorf("failed to close previous day file: %w", err)
	}
	return l.openForDate(now)
}

// writeRecord appends one record and flushes immediately (dashboard tails
// the file). Caller must hold l.mu.
func (l *CSVLogger) writeRecord(rec []string) error {
	if err := l.rotateIfNeeded(); err != nil {
		return err
	}
	if err := l.writer.Write(rec); err != nil {
		return fmt.Errorf("failed to write trade: %w", err)
	}
	// CRITICAL: Flush immediately so dashboard can read
	l.writer.Flush()
	if err := l.writer.Error(); err != nil {
		return fmt.Errorf("CSV writer error: %w", err)
	}
	return nil
}

func buildRecord(filled *broker.FilledOrder, brokerBalance, riskBalance, netPnL float64, status string) []string {
	return []string{
		filled.Timestamp.Format("2006-01-02 15:04:05"),
		filled.Symbol,
		string(filled.Side),
		fmt.Sprintf("%d", filled.Quantity),
		fmt.Sprintf("%.2f", filled.FillPrice),
		fmt.Sprintf("%.2f", filled.Slippage),
		fmt.Sprintf("%.2f", filled.TransactionFee),
		fmt.Sprintf("%.2f", brokerBalance),
		fmt.Sprintf("%.2f", riskBalance),
		fmt.Sprintf("%.2f", netPnL),
		filled.OrderID,
		status,
	}
}

// LogTrade logs a filled order to the CSV file (basic version). Status is
// recorded as "filled" — use LogTradeWithStatus for other statuses.
func (l *CSVLogger) LogTrade(filled *broker.FilledOrder, balance float64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.writeRecord(buildRecord(filled, balance, 0, 0, "filled"))
}

// LogTradeWithRisk logs a filled order with risk manager data.
func (l *CSVLogger) LogTradeWithRisk(filled *broker.FilledOrder, brokerBalance, riskBalance, netPnL float64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.writeRecord(buildRecord(filled, brokerBalance, riskBalance, netPnL, "filled"))
}

// LogTradeWithStatus logs an order event with an explicit status string
// (e.g. "partial", "rejected", "indeterminate" — mirroring the
// internal/ledger status set).
func (l *CSVLogger) LogTradeWithStatus(filled *broker.FilledOrder, brokerBalance, riskBalance, netPnL float64, status string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.writeRecord(buildRecord(filled, brokerBalance, riskBalance, netPnL, status))
}

// Close flushes and closes the CSV file.
func (l *CSVLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.writer.Flush()
	return l.file.Close()
}

// GetFilePath returns the path of the currently open CSV file.
func (l *CSVLogger) GetFilePath() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.filePath
}
