# WP-3 — State Persistence & Reconciliation Library

## Findings addressed

- **CR-8** (all risk/position state in-memory — restart orphans live positions): `internal/state.Store` gives every mutation a synchronous, transactional, WAL-mode SQLite write. `RecoverSession` lets the integration layer reload open positions + last risk snapshot before trading resumes.
- **CR-7** (ambiguous order errors orphan live positions; no reconciliation loop): `Store.NextClientOrderID` + `SaveOrderAttempt`/`MarkOrderResolved` implement the intent-before-network-call audit trail. `ListUnresolvedOrderAttempts` surfaces attempts stuck in `intent`/`indeterminate` for startup recovery. `Reconcile` is the pure comparison engine (matched/phantom/orphan/quantity-mismatch) the integration agent will run against real broker data.
- **ST-4** (nine_twenty state in-memory, flips on signal not fill): `SaveStrategyState`/`LoadStrategyState`/`LoadAllStrategyState` give strategies a durable key/value slot per strategy name, restorable at startup.
- **EX-8** (dead GORM tags implying a persistence layer that never existed): `models/trade.go` rewritten — no GORM, no tags, just plain structs (`Position`, `OrderAttempt`, `RiskSnapshot`) that are the actual store's actual schema.

## Files changed

- `go-engine/models/trade.go` — rewritten (was `Trade`/`Order` with GORM tags; now `Position`, `OrderAttempt`, `RiskSnapshot`, `PositionSide`, `PositionStatus`, `OrderAttemptStatus`).
- `go-engine/internal/state/store.go` — new. `Store` type, schema, all mutation/query methods, ID generators.
- `go-engine/internal/state/reconcile.go` — new. `Reconcile`, `ReconcileReport`, `ReconcileItem`, `MismatchType`, `NetQuantity`.
- `go-engine/internal/state/recover.go` — new. `RecoverSession`.
- `go-engine/internal/state/state_test.go` — new. Crash-simulation test, store behavior tests, concurrency test, `Reconcile` table tests.
- `go-engine/go.mod` / `go-engine/go.sum` — added `modernc.org/sqlite v1.54.0` (direct) and its transitive deps via `go get` + `go mod tidy`. (`go mod tidy` also promoted a pre-existing `golang.org/x/time` import to direct — that's WP-1's dependency, not touched otherwise.)

No other files were edited, per the file-ownership rule.

## SQLite driver

Used **`modernc.org/sqlite`** (pure-Go, no cgo) as instructed — fetch succeeded, no fallback needed. WAL mode + `synchronous=FULL` are set via `PRAGMA` on every `Open`. `db.SetMaxOpenConns(1)` plus an internal `sync.Mutex` around every transaction serialize writers explicitly (SQLite only allows one writer at a time under WAL anyway; this just avoids busy-retry churn and keeps each exported method's transaction atomic end-to-end).

## Exported API (package `internal/state`, import path `titan-algo/internal/state`)

```go
const DefaultDBPath = "data/titan_state.db"

type Store struct { /* unexported fields */ }

func Open(path string) (*Store, error)   // path == "" -> DefaultDBPath; creates parent dir; WAL+FULL sync
func (s *Store) Path() string
func (s *Store) Close() error

// ID generation
func (s *Store) NextClientOrderID() string   // "titan-<unixnano>-<seq>"
func (s *Store) NextPositionID() string      // "pos-<unixnano>-<seq>"

// Positions
func (s *Store) SavePosition(p models.Position) error
    // upsert by p.ID; if p.ID=="" a new one is generated (caller's struct not mutated);
    // if p.Status=="" defaults to models.PositionOpen; if p.EntryTime.IsZero() defaults to now (UTC)
func (s *Store) ClosePosition(id string, exitPrice float64, exitTime time.Time) error
    // exitTime.IsZero() -> time.Now().UTC(); error (wraps sql.ErrNoRows) if id unknown
func (s *Store) ListOpenPositions() ([]models.Position, error)
    // all Status==OPEN, ordered by entry_time ASC

// Order attempts (intent -> resolution audit trail)
func (s *Store) SaveOrderAttempt(attempt models.OrderAttempt) error
    // ClientOrderID required; upserts by ClientOrderID; CreatedAt defaults to now; Status defaults to models.OrderIntent
func (s *Store) MarkOrderResolved(clientOrderID string, status models.OrderAttemptStatus, brokerOrderID string, filledQuantity int, filledPrice float64, errMsg string, resolvedAt time.Time) error
    // resolvedAt.IsZero() -> now; error (wraps sql.ErrNoRows) if clientOrderID was never saved
func (s *Store) GetOrderAttempt(clientOrderID string) (models.OrderAttempt, bool, error)
func (s *Store) ListUnresolvedOrderAttempts() ([]models.OrderAttempt, error)
    // status IN (intent, indeterminate), ordered by created_at ASC

// Risk snapshot (single row)
func (s *Store) SaveRiskSnapshot(balance, realizedPnL, sessionUsed float64) error
func (s *Store) LoadRiskSnapshot() (models.RiskSnapshot, error)
    // no snapshot ever saved -> zero value, nil error (normal first-run condition)

// Strategy key/value state
func (s *Store) SaveStrategyState(strategyName, key, value string) error
func (s *Store) LoadStrategyState(strategyName, key string) (value string, found bool, err error)
func (s *Store) LoadAllStrategyState(strategyName string) (map[string]string, error)

// Reconciliation (pure, no I/O)
type MismatchType string
const (
    Matched          MismatchType = "matched"
    Phantom          MismatchType = "phantom"           // internal only, missing at broker
    Orphan           MismatchType = "orphan"             // broker only, missing internally
    QuantityMismatch MismatchType = "quantity_mismatch"  // both present, net qty/side disagree
)

type ReconcileItem struct {
    Symbol         string
    Type           MismatchType
    Internal       *models.Position // nil if no internal record (Orphan)
    Broker         *models.Position // nil if no broker record (Phantom)
    InternalNetQty int              // signed: +qty long, -qty short
    BrokerNetQty   int
}

type ReconcileReport struct {
    Matched            []ReconcileItem
    Phantoms           []ReconcileItem
    Orphans            []ReconcileItem
    QuantityMismatches []ReconcileItem
}
func (r ReconcileReport) Clean() bool // true iff no phantoms/orphans/mismatches

func Reconcile(internalPositions []models.Position, brokerPositions []models.Position) ReconcileReport
func NetQuantity(p models.Position) int // +Quantity for BUY, -Quantity for SELL

// Startup helper
func RecoverSession(store *Store) (openPositions []models.Position, riskSnapshot models.RiskSnapshot, err error)
```

### `models` package (`titan-algo/models`, file `trade.go`)

```go
type PositionSide string
const ( SideBuy PositionSide = "BUY"; SideSell PositionSide = "SELL" )

type PositionStatus string
const ( PositionOpen PositionStatus = "OPEN"; PositionClosed PositionStatus = "CLOSED" )

type Position struct {
    ID, Symbol, Exchange string
    Side                 PositionSide
    Quantity             int
    EntryPrice, ExitPrice float64
    Strategy             string
    Status               PositionStatus
    EntryTime, ExitTime  time.Time // UTC
    BrokerOrderID, Note  string
}

type OrderAttemptStatus string
const (
    OrderIntent OrderAttemptStatus = "intent"
    OrderFilled OrderAttemptStatus = "filled"
    OrderPartial OrderAttemptStatus = "partial"
    OrderRejected OrderAttemptStatus = "rejected"
    OrderIndeterminate OrderAttemptStatus = "indeterminate"
    OrderCancelled OrderAttemptStatus = "cancelled"
)

type OrderAttempt struct {
    ClientOrderID string
    Symbol        string
    Side          PositionSide
    Quantity      int
    Price         float64
    OrderType     string
    Strategy      string
    Status        OrderAttemptStatus
    BrokerOrderID string
    FilledQuantity int
    FilledPrice    float64
    Error          string
    CreatedAt, ResolvedAt time.Time
}

type RiskSnapshot struct {
    Balance, RealizedPnL, SessionUsed float64
    UpdatedAt time.Time
}
```

## Intended usage patterns (for the integration agent / WP-9)

**Order intent-then-resolve (CR-7 audit trail):**
```go
coid := store.NextClientOrderID()
_ = store.SaveOrderAttempt(models.OrderAttempt{
    ClientOrderID: coid, Symbol: sym, Side: models.SideSell,
    Quantity: qty, OrderType: "MARKET", Strategy: "nine_twenty",
})            // <-- persist BEFORE the network call
filled, err := broker.PlaceOrder(...)   // network call
switch {
case err == nil:
    _ = store.MarkOrderResolved(coid, models.OrderFilled, filled.OrderID, filled.Quantity, filled.FillPrice, "", time.Time{})
case errors.Is(err, broker.ErrOrderIndeterminate):
    _ = store.MarkOrderResolved(coid, models.OrderIndeterminate, "", 0, 0, err.Error(), time.Time{})
    // do NOT roll back risk state; poll order book in background using
    // store.ListUnresolvedOrderAttempts() to find this attempt again after a crash
default:
    _ = store.MarkOrderResolved(coid, models.OrderRejected, "", 0, 0, err.Error(), time.Time{})
}
```
On startup, call `store.ListUnresolvedOrderAttempts()` and resolve each against the broker's order book (via `GetOrderID`/order-book lookup on `BrokerOrderID` if set, or by client-order-ID if the broker echoes it) before allowing new entries.

**Startup sequence (CR-8 fix, matches REMEDIATION_PLAN.md WP-9 task 2):**
```go
store, err := state.Open(cfg.State.DBPath) // config field name is WP-9/WP-8's to add
open, snap, err := state.RecoverSession(store)
brokerPositions := broker.GetPositions() // -> []models.Position (map to models.Position)
report := state.Reconcile(open, brokerPositionsSlice)
if !report.Clean() {
    // print report; require -accept-reconcile or flatten; refuse to trade otherwise
}
```

**`SaveStrategyState` key/value scheme for `nine_twenty`'s "entered" flag (ST-4):**
Values are opaque strings — callers own encoding. Suggested keys for `nine_twenty`:
- `SaveStrategyState("nine_twenty", "entered", "true"|"false")` — flips only after `ConfirmEntry()` (post-fill), per WP-6's task 4, not on signal generation.
- `SaveStrategyState("nine_twenty", "entry_date", "2026-02-10")` — full-date reset guard (ST-3's "Day() only" bug fix belongs to WP-6, but the persisted value needs a full date string, not just a weekday/day-of-month, to survive a restart on a different day).
- `SaveStrategyState("nine_twenty", "entry_premium", "300.5")` — combined entry premium for the ×1.4 stop (CR-14), `strconv.ParseFloat` on read.
On startup: `all, _ := store.LoadAllStrategyState("nine_twenty")` and feed into the strategy's `Restore(map[string]string)` hook (WP-6 owns adding that hook to the strategy itself; this store only stores/returns strings).

**Position IDs:** `Position.ID` is distinct from `Symbol` because a symbol can be opened/closed/reopened many times; each occurrence gets its own durable ID (`Store.NextPositionID()`, or reuse the `ClientOrderID` that opened it — either is valid). `ListOpenPositions`/`Reconcile` group by `Symbol`, not `ID`.

**Reconciliation semantics:** `Reconcile` nets multiple internal (or broker) records for the same symbol by signed quantity (`NetQuantity`: `+Quantity` for BUY, `-Quantity` for SELL) before comparing, so partial-fill records or a long+short pair that nets to the broker's reported quantity classify as `Matched` rather than a false mismatch. A side flip (internal long 75 vs broker short 75) is **not** treated as a match — it lands in `QuantityMismatch` since the signed quantities differ (-150 net difference), which is deliberately conservative per Appendix B's fail-closed principle.

## Test evidence

```
cd go-engine
go build ./models/... ./internal/state/...   # OK
go vet ./models/... ./internal/state/...     # OK
go test -race -v ./internal/state/...
```
20 subtests, all `PASS`, total run `10.994s`:
- `TestCrashRecovery` — writes 3 positions (1 closed), 2 order attempts (1 resolved, 1 left as bare `intent`), a risk snapshot, and 2 strategy-state keys; closes the `Store`; opens a **new** `Store` against the same file; asserts every value round-trips exactly, including the still-unresolved order attempt being found via `ListUnresolvedOrderAttempts`.
- `TestSavePositionUpsertAndDefaults`, `TestClosePositionUnknownID`, `TestMarkOrderResolvedUnknownID`, `TestSaveOrderAttemptRequiresClientOrderID`, `TestLoadRiskSnapshotFirstRun`, `TestRiskSnapshotOverwrite`, `TestStrategyStateNotFound`, `TestStrategyStateOverwrite`, `TestOpenDefaultPathParam` — behavior/edge-case coverage.
- `TestNextClientOrderIDUnique` — 2000 IDs generated from 8 goroutines, all unique, all prefixed `titan-`.
- `TestConcurrentMutations` — 4 position-writer + 2 snapshot/strategy-state/read goroutines running concurrently under `-race`; clean.
- `TestReconcile` (9 table cases) — both empty, all matched, phantom-only, orphan-only, quantity mismatch, side-flip-as-mismatch, all-four-classes-mixed, multi-record-net-sum-to-match (both same-side and opposing-side records netting to the broker's reported quantity).
- `TestReconcileItemDetails`, `TestNetQuantity` — field-level assertions on a `QuantityMismatch` item and the sign convention.

Whole-module `go build ./...` currently fails, but **only** in `internal/strategy` (7 files) and `internal/backtest` — pre-existing signature/type mismatches from other in-flight work packages (WP-6's `EvalContext` interface change, WP-7's `Trade`/`Report` types), none of which touch `internal/state` or `models`. Confirmed by building the owned packages in isolation (`go build ./models/... ./internal/state/...` — clean) and re-confirmed after `go mod tidy`.

## Leaf-package verification

```
grep -rn "titan-algo/internal/(broker|engine|strategy)" go-engine/internal/state
# no matches
```
`internal/state` imports only: `database/sql`, `fmt`, `os`, `path/filepath`, `sync`, `sync/atomic`, `time`, `modernc.org/sqlite` (blank import, driver registration), and `titan-algo/models`. No dependency on `internal/broker`, `internal/engine`, or `internal/strategy`.

## Design decisions / notes for the integration agent

1. **Durability tradeoff:** `synchronous=FULL` is used (not the more common WAL+`NORMAL` combo) because this store may be the only durable record of a live option position — the extra fsync cost is deliberate given real money is involved and mutation volume is low (order-of-magnitude: a handful of writes per trade, not per tick).
2. **Single connection, serialized writes:** `db.SetMaxOpenConns(1)` plus an internal mutex around every transaction. This trades a little write concurrency for simplicity and to guarantee each exported method is a single atomic unit — acceptable given the low write volume here.
3. **`ClosePosition`/`MarkOrderResolved` return an error (wrapping `sql.ErrNoRows`) if the target ID is unknown** rather than silently no-op'ing — fail-closed per Appendix B.
4. **`LoadRiskSnapshot`/`LoadStrategyState` return zero-value/`found=false` with `nil` error when nothing has been saved yet** — this is the normal first-run path, distinguished from an actual DB error.
5. **Reconciliation is keyed on `Symbol`**, matching how `internal/broker`'s `GetPositions()` already returns `map[string]*Position`. If a future need arises to track multiple simultaneous positions per symbol as separately-reconciled units, `Reconcile` would need a compound key — not needed today since option symbols are already strike/expiry-specific.
6. **Config field needed from WP-8/WP-9:** a `state.db_path` config field (defaults to `internal/state.DefaultDBPath` = `"data/titan_state.db"`) so operators can relocate the DB file; `Store.Open` already accepts any path.
