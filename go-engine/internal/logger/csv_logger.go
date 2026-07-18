package logger

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"titan-algo/internal/broker"
)

// CSVLogger logs trades to a CSV file
type CSVLogger struct {
	mu       sync.Mutex
	file     *os.File
	writer   *csv.Writer
	filePath string
}

// NewCSVLogger creates a new CSV logger
func NewCSVLogger(logDir string) (*CSVLogger, error) {
	// Create logs directory if it doesn't exist
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	// Create CSV file
	filePath := filepath.Join(logDir, "trades.csv")
	// Changed to O_TRUNC to clear file on startup
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open CSV file: %w", err)
	}

	writer := csv.NewWriter(file)

	logger := &CSVLogger{
		file:     file,
		writer:   writer,
		filePath: filePath,
	}

	// Always write header since we are starting fresh
	header := []string{
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
	}
	if err := writer.Write(header); err != nil {
		return nil, fmt.Errorf("failed to write header: %w", err)
	}
	writer.Flush()

	return logger, nil
}

// LogTrade logs a filled order to the CSV file (basic version)
func (l *CSVLogger) LogTrade(filled *broker.FilledOrder, balance float64) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	record := []string{
		filled.Timestamp.Format("2006-01-02 15:04:05"),
		filled.Symbol,
		string(filled.Side),
		fmt.Sprintf("%d", filled.Quantity),
		fmt.Sprintf("%.2f", filled.FillPrice),
		fmt.Sprintf("%.2f", filled.Slippage),
		fmt.Sprintf("%.2f", filled.TransactionFee),
		fmt.Sprintf("%.2f", balance),
		"0.00", // RiskBalance placeholder
		"0.00", // NetPnL placeholder
	}

	if err := l.writer.Write(record); err != nil {
		return fmt.Errorf("failed to write trade: %w", err)
	}

	// CRITICAL: Flush immediately so dashboard can read
	l.writer.Flush()

	if err := l.writer.Error(); err != nil {
		return fmt.Errorf("CSV writer error: %w", err)
	}

	return nil
}

// LogTradeWithRisk logs a filled order with risk manager data
func (l *CSVLogger) LogTradeWithRisk(filled *broker.FilledOrder, brokerBalance, riskBalance, netPnL float64) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	record := []string{
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
	}

	if err := l.writer.Write(record); err != nil {
		return fmt.Errorf("failed to write trade: %w", err)
	}

	// CRITICAL: Flush immediately so dashboard can read
	l.writer.Flush()

	if err := l.writer.Error(); err != nil {
		return fmt.Errorf("CSV writer error: %w", err)
	}

	return nil
}

// Close closes the CSV file
func (l *CSVLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.writer.Flush()
	return l.file.Close()
}

// GetFilePath returns the path to the CSV file
func (l *CSVLogger) GetFilePath() string {
	return l.filePath
}
