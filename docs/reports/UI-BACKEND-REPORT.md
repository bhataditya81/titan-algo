# UI-BACKEND — Web Control-Panel API Additions — Report

Extends the existing hardened API server (`go-engine/internal/api/server.go`)
with four things for the new `web-ui/` control panel: `GET /api/strategies`,
`GET /api/candles`, a ledger-backed `GET /api/trades`, and a static file
handler serving `web-ui/`'s HTML/CSS/JS at `/`.

## Files touched

- `go-engine/internal/api/server.go` (edited)
- `go-engine/internal/api/server_ui_endpoints_test.go` (new)

No other file was touched, per the task's hard constraint.

## New endpoints — exact response shapes (as actually produced, verified via a real `httptest` round trip, not as planned)

### `GET /api/strategies`

Auth: same `authMiddleware` as every other `/api/*` route (rate limit then
`X-API-Key`). Source: `strategy.GetAvailableStrategies()` (the exact function
`cmd/backtest/main.go`'s `-list-strategies` flag calls), sorted with
`sort.Strings`.

```json
{"strategies":["ema_crossover","iron_fly","momentum","nine_twenty","rsi_reversal","short_straddle","sniper"]}
```

(The 7 names above are whatever is actually registered in this checkout's
`internal/strategy` package `init()`s — captured from a real test run, not
guessed.)

### `GET /api/candles?symbol=NIFTY&limit=500`

Auth: same middleware. Reads `{candlesDir}/{symbol}.csv` via
`internal/backtest.LoadCandlesCSV` (same CSV format/parser the backtest CLI
already uses — reused directly, no import cycle: `internal/backtest` and
`internal/strategy` import nothing from `internal/api`).

- `symbol`: validated with a local `isAlphanumericSymbol` (ASCII
  letters/digits only, non-empty) *before* it's used to build a file path —
  rejects `/`, `\`, `..`, and anything else. Invalid/empty → `400`.
- `limit`: optional, default `500`, must be a positive integer if present,
  capped at `5000`. Invalid (non-integer or `<=0`) → `400`.
- Takes the **last** `limit` rows of the file (most recent candles), returns
  them in file order (oldest of the kept window first, matching how a chart
  reads left-to-right).

Success (`200`):
```json
{"candles":[{"time":"2026-07-17T15:20:00+05:30","open":24330,"high":24340,"low":24325,"close":24335,"volume":0},{"time":"2026-07-17T15:25:00+05:30","open":24341.55,"high":24352.65,"low":24339.2,"close":24346.7,"volume":0}],"symbol":"NIFTY"}
```
Unknown symbol (`404`):
```json
{"error":"no cached candle data for symbol BOGUS"}
```
Bad symbol/limit (`400`): `{"error":"symbol must be a non-empty alphanumeric string"}` or `{"error":"limit must be a positive integer"}`.

Note: `map[string]interface{}` keys are alphabetized by `encoding/json` when
marshaling a map, so the real wire order is `candles` before `symbol` (shown
above) — flagging this since it differs from the task's illustrative
`{"symbol":...,"candles":...}` ordering; JSON object key order is not
semantically meaningful so no fix was needed, just noting the actual bytes.

### `GET /api/trades?limit=100`

Auth: same middleware. **This reuses the existing `/api/trades` route**
(previously backed by a hardcoded `logs/trades.csv` CSV tail-reader that no
longer matched WP-5's date-stamped `trades_YYYY-MM-DD.csv` log files and so
always returned an empty list) — the handler's *implementation* now reads
from the durable ledger (`internal/ledger`) instead. No new path was needed
and no existing test asserted on the old CSV-shaped response body, so this
is a safe, non-breaking replacement (verified: full existing test suite
still green). The dead CSV-parsing code (`TradeRecord` type,
`loadTradesFromCSV`, `tradesLogPath` field) was deleted rather than left
unused.

- `limit`: optional, default `100`, capped at `5000`, must be a positive
  integer if present (`400` otherwise).
- Rows come from `Ledger.Query(DateRange{})` (unbounded range — there's no
  `LIMIT`/"recent N" query on `*ledger.Ledger`, only date-range `Query`), then
  the tail `limit` rows are taken and reversed for most-recent-first.
  `ponytail:` this is a full-table scan of the ledger on every call; fine for
  a personal system with a modest trade count, add a real
  `ORDER BY id DESC LIMIT ?` query method on `*ledger.Ledger` if the table
  ever grows large enough for this to matter.
- Trade fields are `ledger.Trade`'s actual exported fields, serialized with
  Go's default (no `json` struct tags exist on `ledger.Trade`) — i.e. the
  JSON keys are the literal Go field names, capitalized:

Ledger connected (`200`):
```json
{"trades":[{"ID":1,"Timestamp":"2026-07-18T09:15:00Z","ClientOrderID":"ORD-1","BrokerOrderID":"B-1","Symbol":"NIFTY","Side":"BUY","Quantity":75,"RequestedQuantity":75,"Price":100.5,"Status":"filled","Charges":12.3,"RealizedPnL":45.6,"Strategy":"sniper","Mode":"paper","Note":"test"}]}
```
No ledger wired (`200`, not an error):
```json
{"note":"ledger not connected","trades":[]}
```

### Static UI files at `/`

`http.FileServer(http.Dir(webUIDir))` registered as the mux's `/` (catch-all)
pattern in `Start()`, **not** wrapped in `authMiddleware` — the page itself
has no secrets; only the `/api/*`/`/ws/live` calls it makes need the token
(collected from the user in a settings field and attached by the page
itself). Every other route (`/api/*`, `/health`, `/ws/live`) is a more
specific `http.ServeMux` pattern, so they still win over `/` regardless of
registration order — confirmed no route conflict.

## Server constructor/setter changes (for the later `cmd/main.go` wiring)

No constructor signature change — `NewServer(port int, token string)` is
unchanged. Three new setters, following the exact existing convention (call
before `Start()`, same style as `SetBindAddr`/`SetTLS`/`SetRateLimit`):

```go
func (s *Server) SetLedger(l *ledger.Ledger)      // GET /api/trades source. nil (default) -> "ledger not connected" shape, never errors.
func (s *Server) SetCandlesDir(dir string)         // GET /api/candles source dir. Default "data/historical". No-op if dir == "".
func (s *Server) SetWebUIDir(dir string)           // static files dir for "/". Default "../web-ui". No-op if dir == "".
```

Integration point for `cmd/main.go` (mirrors the existing
`apiServer.SetRateLimit(...)` / `SetWSMaxConns(...)` / `SetTLS(...)` block,
around line 383-389): `cmd/main.go` already opens a `ledgerDB` via
`ledger.Open(ledgerPath)` a few lines above (line ~210) for the trading
engine itself — the natural wiring is simply:

```go
apiServer.SetLedger(ledgerDB)
```

`SetCandlesDir`/`SetWebUIDir` only need to be called if the defaults below
don't fit; otherwise no call is required.

## Static-file-serving configuration

- **Default**: `"../web-ui"`, i.e. the sibling `web-ui/` directory one level
  up from `go-engine/`'s working directory. Verified this matches how the
  binary is actually run: every other relative default already on `Server`
  (`candlesDir` = `"data/historical"`, `configPath` = `"config.yaml"`,
  the old `tradesLogPath` = `"logs/trades.csv"`) resolves correctly only if
  the process's cwd is `go-engine/` itself (confirmed: `go-engine/data/historical`
  exists on disk; there is no `data/` at the repo root) — and the sibling
  `web-ui/` directory the other agent is building lives at the **repo
  root**, one level above that, hence `"../web-ui"`.
- **Override**: `apiServer.SetWebUIDir("/absolute/or/relative/path")` before
  `Start()`.
- **New default** (`candlesDir`): `"data/historical"`, same directory
  `cmd/fetchdata` and the backtest CLI's `-csv`/cache convention already use.
  Override: `apiServer.SetCandlesDir(dir)`.

## Test evidence

```
$ cd go-engine
$ go build ./internal/api/... && go vet ./internal/api/...
(clean, no output)

$ gofmt -l internal/api/server.go internal/api/server_ui_endpoints_test.go
(clean, no output)

$ go test -race ./internal/api/...
ok      titan-algo/internal/api        2.186s
```

34 tests total (24 pre-existing WP-4/R2-5 tests, unchanged and still green +
10 new in `server_ui_endpoints_test.go`), run under `-race`:

- `TestStrategiesEndpointReturnsRealSortedList` — non-empty, actually
  registered strategy names, sorted.
- `TestCandlesEndpointReturnsParsedData` — real temp-fixture CSV parsed and
  returned correctly (time/OHLCV values checked field-by-field).
- `TestCandlesEndpointLimitsAndCaps` — `?limit=3` against a 10-row fixture
  returns exactly the **last** 3 rows, not the first 3.
- `TestCandlesEndpoint404ForUnknownSymbol` — clean `404` with the symbol
  named in the error message, no path-existence leak.
- `TestCandlesEndpointRejectsPathTraversalSymbol` — `../secret`, `..\secret`,
  `a/b`, `a\b`, `..`, `NIFTY.csv`, and empty-string symbols all rejected
  `400` (never reach `os.Open`).
- `TestTradesEndpointReturnsLedgerRowsMostRecentFirst` — real
  `ledger.Open`+`Append` (temp SQLite file), `?limit=2` returns exactly the 2
  newest rows, newest first.
- `TestTradesEndpointNotConnectedShapeWhenNoLedger` — `200` with
  `{"trades":[],"note":"ledger not connected"}` when `SetLedger` was never
  called.
- `TestNewEndpointsRejectMissingOrBadToken` — `/api/strategies`,
  `/api/candles`, `/api/trades` all `401` on missing/wrong `X-API-Key`,
  exactly like every pre-existing endpoint.
- `TestStaticFileHandlerServesWithoutToken` — a temp `index.html` served at
  `/` with **no** `X-API-Key` header at all, `200` with the fixture content.

Grep confirmation that no new route bypasses `authMiddleware` (the only
unauthenticated routes are the pre-existing `/health` and the new, by-design
static file handler; `/ws/live` keeps its own inline token check as before):

```
$ grep -n 'mux\.Handle' internal/api/server.go
mux.HandleFunc("/api/status", s.authMiddleware(s.handleStatus))
mux.HandleFunc("/api/positions", s.authMiddleware(s.handlePositions))
mux.HandleFunc("/api/trades", s.authMiddleware(s.handleTrades))
mux.HandleFunc("/api/config", s.authMiddleware(s.handleConfig))
mux.HandleFunc("/api/start", s.authMiddleware(s.handleStart))
mux.HandleFunc("/api/stop", s.authMiddleware(s.handleStop))
mux.HandleFunc("/api/kill", s.authMiddleware(s.handleKill))
mux.HandleFunc("/api/strategies", s.authMiddleware(s.handleStrategies))
mux.HandleFunc("/api/candles", s.authMiddleware(s.handleCandles))
mux.HandleFunc("/ws/live", s.handleWebSocket)
mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { ... })
mux.Handle("/", http.FileServer(http.Dir(webUIDir)))
```

Whole-module sanity (not required by the task, run anyway):
`go build ./...` and `go vet ./...` from `go-engine/` — both clean.

## Discrepancies / notes

1. **`/api/trades` is a behavior change to an existing route, not a new
   path.** The task described it as one of "four things to add" and gave it
   the same path the server already had registered. Since the pre-existing
   implementation was already effectively dead (hardcoded
   `logs/trades.csv`, never matching WP-5's date-stamped log filenames — see
   WP-5-REPORT.md's own forward-compat flag on this exact mismatch) and no
   test asserted on its response shape, this was implemented as replacing
   `handleTrades`'s body rather than adding a second route, which would have
   collided in `net/http.ServeMux` (duplicate pattern registration panics at
   startup). Flagging this clearly in case a reviewer expected the old
   CSV-tail behavior preserved somewhere.
2. **No dedicated "recent N" query exists on `*ledger.Ledger`.** Only
   `Query(DateRange)` (full inclusive range, ascending, no `LIMIT`). Given
   the ledger package is off-limits to edit for this task, `/api/trades`
   fetches the full range and slices/reverses in `internal/api`. Flagged
   with a `ponytail:` comment in the code (`server.go`, `handleTrades`) as
   the known ceiling — a real `ORDER BY id DESC LIMIT ?` method on the
   ledger would be the fix if this table ever grows large.
3. **`web-ui/` is currently empty** (confirmed via directory listing) — the
   sibling agent's static files don't exist yet, so
   `TestStaticFileHandlerServesWithoutToken` uses its own temp directory
   fixture rather than the real `web-ui/`, per the task's own httptest-only
   testing constraint.
