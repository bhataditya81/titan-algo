# WP-5 — Trade Ledger — Report

## Findings addressed

- **EX-8 (HIGH). Trade log truncated on every startup.** `internal/logger/csv_logger.go` opened `trades.csv` with `O_TRUNC`, wiping the only trade record on every process restart, and had no `OrderID` column so fills could not be reconciled against broker contract notes. The GORM tags in `models/trade.go` implied a DB that didn't exist (no DB driver in `go.mod`) — WP-5 is that DB.

Fix: a new `internal/ledger` package makes an append-only SQLite database (`modernc.org/sqlite`, pure-Go, no cgo) the system of record for trades, with broker order IDs, full order-lifecycle status, and a CSV export path. The old CSV logger is fixed (append, not truncate) and kept as a secondary/live-tail log for the dashboard.

## Files changed

- `go-engine/internal/ledger/ledger.go` (new) — the ledger package.
- `go-engine/internal/ledger/ledger_test.go` (new) — tests.
- `go-engine/internal/logger/csv_logger.go` (rewritten) — truncation fix, date-stamped files, `OrderID`/`Status` columns.
- `go-engine/internal/logger/csv_logger_test.go` (new) — tests.
- `go-engine/go.mod`, `go-engine/go.sum` — added `modernc.org/sqlite` (and its transitive deps: `modernc.org/libc`, `modernc.org/memory`, `modernc.org/mathutil`, `golang.org/x/sys`, `github.com/dustin/go-humanize`, `github.com/ncruces/go-strftime`, `github.com/remyoudompheng/bigfft`, `github.com/google/uuid`, `github.com/mattn/go-isatty`). This is the same driver WP-3's `internal/state` package is instructed to use — no duplicate/competing SQL driver introduced.

No other files were touched (file-ownership rule respected).

## Exported ledger API (for WP-9 / other consumers)

Package `titan-algo/internal/ledger`. Leaf library: no imports of `internal/broker` or any other TitanAlgo package (mirrors WP-3's `internal/state` convention) — callers convert broker-specific types to plain strings/floats when building a `Trade`.

```go
package ledger

const DefaultDBPath = "data/titan_ledger.db"

type Status string
const (
    StatusIntent        Status = "intent"
    StatusFilled        Status = "filled"
    StatusPartial       Status = "partial"
    StatusRejected      Status = "rejected"
    StatusIndeterminate Status = "indeterminate"
    StatusReconciled    Status = "reconciled"
)
func ValidStatuses() []Status
func (s Status) IsValid() bool

type Mode string
const (
    ModePaper Mode = "paper"
    ModeLive  Mode = "live"
)

type Trade struct {
    ID                int64     // populated on read; ignored on Append input
    Timestamp         time.Time
    ClientOrderID     string
    BrokerOrderID     string
    Symbol            string
    Side              string    // "BUY" / "SELL" — caller-defined, not validated
    Quantity          int       // actually executed quantity
    RequestedQuantity int       // originally requested quantity
    Price             float64
    Status            Status
    Charges           float64
    RealizedPnL       float64
    Strategy          string
    Mode              Mode
    Note              string
}

type DateRange struct{ From, To time.Time } // inclusive; zero To = unbounded

func Open(dbPath string) (*Ledger, error)                     // "" -> DefaultDBPath
func (l *Ledger) Append(t Trade) error                        // synchronous, transactional INSERT
func (l *Ledger) Query(dateRange DateRange) ([]Trade, error)  // ordered by timestamp, id
func (l *Ledger) ExportCSV(dateRange DateRange, path string) error
func (l *Ledger) Close() error
func (l *Ledger) Path() string
```

### Usage contract

- **Append-only.** `Append` only INSERTs; there is no Update/Delete. To record an order's lifecycle (e.g. intent → filled, or intent → indeterminate → reconciled), call `Append` again with the **same `ClientOrderID`** and the new `Status` — the full history stays visible as a sequence of rows. This is deliberate: an audit trail for real money must never let a later write erase what an earlier write said.
- **Synchronous & durable.** Every `Append` runs inside an explicit `BEGIN`/`INSERT`/`COMMIT`. The connection is opened with `PRAGMA journal_mode=WAL` and `PRAGMA synchronous=FULL`, and the pool is capped to a single connection (`SetMaxOpenConns(1)`), so a successful `Append` return means the row is fsynced to disk — no buffering that could lose data on crash.
- **Validation (fail-closed).** `Append` rejects empty `ClientOrderID`, an invalid `Status`, or a `Mode` other than `paper`/`live` — it returns an error rather than silently writing a garbage row.
- **Suggested call sites for WP-9:** write a `StatusIntent` row *before* sending the order to the broker (durable intent, matching WP-3's client-order-ID pattern for CR-7 recovery), then a `StatusFilled`/`StatusPartial`/`StatusRejected`/`StatusIndeterminate` row after the broker responds/poll resolves, and a `StatusReconciled` row if/when the reconciliation loop later resolves an ambiguous order.
- **`ExportCSV(dateRange, path)`** is the new "export" use case that replaces CSV-as-primary-record: it queries the ledger for the inclusive `[From, To]` window and writes a fresh CSV (header + one row per trade, including the DB `id`) to `path`, overwriting any existing file there. The SQLite DB remains the system of record; this is a derived, point-in-time snapshot (e.g. for a day's contract-note reconciliation).

### Schema

Table `trades` (SQLite, WAL mode):

| column | type | notes |
|---|---|---|
| id | INTEGER PK AUTOINCREMENT | |
| timestamp | TEXT | RFC3339Nano, stored in UTC |
| client_order_id | TEXT NOT NULL | |
| broker_order_id | TEXT | empty until the broker assigns one |
| symbol | TEXT NOT NULL | |
| side | TEXT NOT NULL | caller-defined string, e.g. BUY/SELL |
| quantity | INTEGER NOT NULL | actually executed |
| requested_quantity | INTEGER NOT NULL | originally requested |
| price | REAL NOT NULL | |
| status | TEXT NOT NULL | one of the `Status` enum values |
| charges | REAL | |
| realized_pnl | REAL | |
| strategy | TEXT | |
| mode | TEXT NOT NULL | `paper` / `live` |
| note | TEXT | |

Indexes on `timestamp` and `client_order_id` for query/export performance.

## CSV logger changes (`internal/logger/csv_logger.go`)

**Bug fixed:** the file was opened with `os.O_TRUNC`, so every process start wiped `logs/trades.csv` — the only trade record at the time, destroying the audit trail exactly when a crash/restart made it most necessary (EX-8). It is now opened with `os.O_APPEND` (no truncation), and the filename is date-stamped so each trading day gets its own file: `trades_YYYY-MM-DD.csv` instead of a single `trades.csv`. The header row is written only when the file is newly created or empty (checked via `os.Stat` size), never re-written on append/reopen. The logger also self-rotates mid-process if the wall-clock date rolls over while it's running (new file + fresh header for the new day).

This file is explicitly kept as the **secondary/live-tail log** the dashboard reads — it was not removed. The ledger (`internal/ledger`) is now the system of record; this CSV is a convenience mirror.

### CSV column format: old vs new

Old (10 columns, single file `trades.csv`, truncated on every start):

```
Timestamp,Symbol,Action,Quantity,FillPrice,Slippage,TransactionFee,BrokerBalance,RiskBalance,NetPnL
```

New (12 columns, one file per day `trades_YYYY-MM-DD.csv`, append-only):

```
Timestamp,Symbol,Action,Quantity,FillPrice,Slippage,TransactionFee,BrokerBalance,RiskBalance,NetPnL,OrderID,Status
```

Two columns were appended: `OrderID` (the broker order ID, matching what the ledger tracks — enables reconciling the CSV against contract notes) and `Status` (`filled` for the existing `LogTrade`/`LogTradeWithRisk` calls; a new `LogTradeWithStatus(filled, brokerBalance, riskBalance, netPnL, status)` method lets callers record `partial`/`rejected`/`indeterminate` etc., mirroring the ledger's status set).

**Forward-compatibility flag for WP-11/WP-9 (dashboard):** old CSV files written before this change have only the first 10 columns and no `OrderID`/`Status`. Any dashboard/parser reading these files by column index or fixed column count will break or misread new files (and choke on old ones if it assumes 12 columns). It should be made tolerant: read the header row to determine column positions/count rather than hardcoding indices, and treat missing trailing columns as empty rather than erroring. Flagging this explicitly since the dashboard code is outside WP-5's owned files.

## Test evidence

```
$ cd go-engine && go test -race ./internal/ledger/...
ok      titan-algo/internal/ledger     6.296s
```

`internal/ledger/ledger_test.go` covers:
- `TestAppendAndQuery` — append several rows, query back, verify every field round-trips.
- `TestCrashRestartPreservesRows` — the crash/restart simulation required by the acceptance criteria: append rows, `Close()`, reopen the **same file** (`t.TempDir()`), append more, verify all rows (old + new) are present; reopen a third time with no writes to prove the data survived on disk, not just in an in-memory cache.
- `TestAppendRejectsInvalidStatus` — invalid `Status`, invalid `Mode`, and empty `ClientOrderID` are all rejected (fail-closed).
- `TestExportCSVMatchesInsertedRows` — `ExportCSV` output is parsed back and compared field-by-field against the rows that were inserted, for both a full-range export and a narrower date range that excludes some rows.
- `TestOpenDefaultPathCreatesParentDir` — `Open` creates missing parent directories.

`internal/logger/csv_logger_test.go` covers:
- `TestNoTruncationOnRestart` — the exact acceptance scenario: write, close, reopen the logger against the same directory (simulating a process restart), write again, verify **both** entries are present in the file (proves it no longer truncates).
- `TestHeaderWrittenOnlyOnce` — reopening an existing non-empty file across three sessions never re-writes the header.
- `TestDateStampedFilename` — filename matches `trades_YYYY-MM-DD.csv`; header and data rows both carry the new `OrderID`/`Status` columns in the last two positions.
- `TestRotatesOnDateChange` — using an injectable clock (`nowFunc`), crossing a day boundary opens a new date-stamped file with its own header and does not carry rows over from the previous day's file.

```
$ cd go-engine && go test -race ./internal/logger/...
ok      titan-algo/internal/logger     3.447s
```

Combined run: `go test -race ./internal/ledger/... ./internal/logger/...` — both packages pass, all subtests listed above green.

`grep -n "O_TRUNC" internal/logger/csv_logger.go` → no matches (the file no longer contains the literal flag or even the string, to make the check unambiguous).

`gofmt -l internal/ledger internal/logger` → no output (both packages are gofmt-clean).

## Build/vet status

`internal/logger` imports `internal/broker` (pre-existing, unrelated to WP-5), and `internal/broker/historical.go` imports `internal/strategy`. While this package was being written, `internal/strategy` was mid-refactor by the sibling WP-6 agent (its `Signal`/`EvalContext` interface change, landing file-by-file), which transiently broke `go build ./...` for every downstream package including `internal/logger`. By the time WP-5 finished, WP-6's package-internal refactor was complete and `internal/strategy`/`internal/broker`/`internal/logger`/`internal/ledger` all build and vet cleanly:

```
$ go build ./internal/ledger/... ./internal/logger/...
$ go vet   ./internal/ledger/... ./internal/logger/...
(both exit 0, no output)
```

Repo-wide `go build ./...` / `go vet ./...` still fail, but **only** in `cmd` and `cmd/backtest` — both are outside every Wave-1 package's ownership (WP-9 and WP-7/WP-9 respectively) and the failures are exactly the call-site breakage WP-6's plan section calls out as expected/acceptable during Wave 1 ("`go build ./...` may break `cmd/` due to interface change — acceptable ONLY in `cmd/` and `internal/engine`"; WP-9 wires the new `Strategy.Evaluate(EvalContext)` signature at the call sites). No WP-5-owned file is implicated in either remaining failure.

## Discrepancies / notes for other packages

- **WP-9 (integration):** all order intents/results should be written to `ledger.Append` (see "Suggested call sites" above); CSV becomes secondary. Convert `broker.OrderSide`/status values to the plain strings/`ledger.Status` this package expects.
- **WP-8 (config):** `internal/ledger.Open` takes the DB path as a constructor parameter (default `data/titan_ledger.db` via `ledger.DefaultDBPath` if empty). Please add a `ledger.db_path` config field (mirrors WP-3's `state.db_path` ask) so WP-9 can wire it from config instead of hardcoding.
- **WP-11 (docs) / WP-9 (dashboard):** CSV column format changed (10 → 12 columns, see table above); dashboard CSV parser needs to become header-driven/tolerant of both old short rows and new files, per the flag above.
