# TitanAlgo — Remediation Work Plan (Parallel Agent Execution)

**Source:** `docs/PRODUCTION_READINESS_AUDIT.md` (finding IDs CR-*, ST-*, EX-* referenced below come from that document — read it first).
**Repo root:** `titan-algo/` (all paths below relative to it).

---

## 0. Rules for Every Agent (read before starting)

1. **Read your work package only, plus the audit findings it references.** Do not fix issues outside your package — another agent owns them.
2. **File ownership is exclusive.** Only edit files listed under "Owned files" in your package. If your fix seems to require editing a file you don't own, STOP and report the required interface change instead — the integration package (WP-9) will wire it.
3. **Never invent APIs.** Angel One SmartAPI endpoints you may use are listed in Appendix A. If an endpoint you need is not listed, report it; do not guess URLs or response shapes.
4. **Verify before claiming done.** Every package must pass: `go build ./...` and `go vet ./...` from `go-engine/`, plus the package-specific acceptance criteria. If you wrote tests: `go test -race ./...` on your packages.
5. **If the code does not match the audit's description** (line numbers drift, function renamed), re-read the actual file and act on the actual code. Report the discrepancy; do not force the described change onto the wrong location.
6. **No live trading, ever.** Do not run any binary with `-live`. Do not use credentials. Paper/mock mode only for manual verification.
7. **No new third-party dependencies** except those explicitly allowed in your package. Standard library preferred.
8. **Keep existing behavior for paper mode working.** If a change breaks the paper flow, fix forward within your owned files or report.
9. **Deliverable format:** code changes + a short `WP-<n>-REPORT.md` in `docs/reports/` listing: findings addressed, files changed, interface changes exported for WP-9, test evidence, discrepancies found.
10. **Commit discipline (once repo is git-initialized in Phase 0):** one branch per package, `wp<n>-<slug>`; small commits; never touch `config.yaml` credentials.

---

## 1. Execution Order

```
Phase 0 (HUMAN — blocking, do first)
   └─► Wave 1: WP-1 … WP-8  (all parallel, disjoint files)
          └─► Wave 2: WP-9 integration (single agent, serial)
                 └─► Wave 3: WP-10 validation + WP-11 docs (parallel)
```

- Wave 1 packages share **no files** with each other. Safe to run 8 agents concurrently.
- WP-9 is the ONLY package allowed to edit `cmd/main.go`, `internal/app/titan.go`, `internal/app/modes.go`, `internal/engine/engine.go`, `internal/cli/interactive.go`.
- Wave 3 starts only after WP-9 builds and paper mode runs.

### File Ownership Matrix

| Package | Owned files (exclusive) |
|---|---|
| WP-1 | `internal/broker/angel_broker.go`, `internal/broker/instruments.go`, `internal/broker/broker.go` |
| WP-2 | `internal/risk/risk.go`, `examples/risk_example.go` |
| WP-3 | `internal/state/` (new package), `models/trade.go` |
| WP-4 | `internal/api/server.go`, `mobile/titanmobile.go` |
| WP-5 | `internal/logger/csv_logger.go`, `internal/ledger/` (new package) |
| WP-6 | `internal/strategy/*` (all files), `internal/broker/historical.go` |
| WP-7 | `cmd/backtest/main.go`, `internal/backtest/` (new package, optional) |
| WP-8 | `docker-compose.yml`, `go-engine/Dockerfile` (new), `py-brain/*`, `*.ps1`, `.gitignore` (new), `.github/` (new), `internal/config/config.go`, `config.example.yaml` (new) |
| WP-9 | `cmd/main.go`, `internal/engine/engine.go`, `internal/app/*`, `internal/cli/interactive.go`, `internal/feed/feed.go`, `internal/ipc/ipc.go` + wiring-only edits anywhere needed |
| WP-10 | new validation scripts/docs only |
| WP-11 | `README.md`, `RUNNING.md`, `PAPER_TRADING.md`, `docs/MOBILE_APP_DESIGN.md`, `docs/RUNBOOK.md` (new) |

`config.yaml`: NOBODY edits it except the human (Phase 0). Agents add new config FIELDS via `internal/config/config.go` (WP-8 owns the struct; other packages report needed fields in their REPORT for WP-8/WP-9 to add) and document defaults in `config.example.yaml`.

---

## 2. Phase 0 — HUMAN TASKS (no agent; blocking)

- [ ] **Rotate Angel One credentials**: re-enroll TOTP, generate new API key, change PIN. The values currently in `go-engine/config.yaml:6-11` are burned (CR-1).
- [ ] Move project out of OneDrive (e.g. `C:\dev\titan-algo`).
- [ ] Create `.gitignore` FIRST (WP-8 provides content; interim minimal: `config.yaml`, `*.exe`, `logs/`, `*.csv`, `.idea/`), then `git init`, initial commit.
- [ ] Delete from tree: `*.exe` (5 binaries), `logs/`, `pnl_verify.txt`, `search_nifty.txt`.
- [ ] Set new credentials as environment variables (names defined by WP-8: `ANGEL_CLIENT_CODE`, `ANGEL_PIN`, `ANGEL_API_KEY`, `ANGEL_API_SECRET`, `ANGEL_TOTP_SECRET`, `TITAN_API_TOKEN`).
- [ ] Do not run `-live` until WP-9 acceptance passes.

---

## 3. Wave 1 Work Packages (parallel)

---

### WP-1 — Broker Hardening (Angel One client)

**Objective:** Order lifecycle correctness, lock hygiene, session refresh, real balance, price staleness.
**Findings:** CR-5, CR-6, CR-7 (broker side), EX-2, EX-5, EX-6, EX-7, parts of EX-9.
**Owned files:** `internal/broker/angel_broker.go`, `internal/broker/instruments.go`, `internal/broker/broker.go`.
**Allowed deps:** stdlib only (`golang.org/x/time/rate` permitted for rate limiting).

Tasks:
1. **Indeterminate orders (CR-5).** In `PlaceOrder` fill-poll (`angel_broker.go:751-787`): if status still pending after poll budget, return sentinel error `ErrOrderIndeterminate` (exported) with the broker order ID attached. Delete the LTP fallback and the `fillPrice = 1.0` fallback (`:784`). Never fabricate a `FilledOrder`.
2. **Partial fills (CR-6).** Use `filledshares` (`fillQty`) for `FilledOrder.Quantity` and `updatePosition`. Handle statuses `open`, `partially filled`, `complete` explicitly. If `complete` with `fillQty < requested`, report actual qty.
3. **Position sign crossover (EX-9).** Fix `updatePosition` (`angel_broker.go:1016-1021`): opposite-side qty > existing must produce a new position on the opposite side with remainder qty, not deletion.
4. **Lock hygiene (EX-5).** Restructure `PlaceOrder` (`:616-822`) so the mutex is NOT held during: rate-limit sleeps, HTTP calls, fill polling. Lock only to read/refresh session state and to commit position/cache mutations. Replace the unlock-sleep-relock rate limiter (`:638-647`) with a token bucket.
5. **Exits bypass circuit breaker (EX-2).** Add `reduceOnly bool` (or an `OrderIntent` enum) to the internal order path; circuit breaker (`:625-628, 729-737`) must never block position-reducing orders. Expose intent in the `Broker` interface (`broker.go`) — document the signature change for WP-9.
6. **Session refresh (EX-6).** Detect auth failure (HTTP 401 or Angel `status:false` with auth error codes) centrally; attempt token refresh via endpoint A-4; on failure, one TOTP re-login attempt; on failure, mark broker unhealthy via exported `Healthy() bool` + error state. Use the stored `refreshToken` (`:197-199`).
7. **Real balance (EX-7).** Implement `GetBalance` via RMS endpoint A-5, replacing hardcoded `10000.0` (`:992-997`).
8. **Price staleness (EX-6/CR-14 support).** Timestamp every cached price. Add `GetCurrentPriceWithAge(symbol) (price float64, age time.Duration, err error)`. Keep old `GetCurrentPrice` working (returns cached).
9. **HTTP status checks (EX-9).** Check `resp.StatusCode` on every Angel call before parsing; treat non-200 and HTML bodies (WAF) as typed errors. Position fetch during WAF block must return error, NOT empty positions (`:281-285`).
10. **Instruments (EX-9).** `instruments.go:49`: use an `http.Client` with timeout; load instruments OUTSIDE any broker lock; cache the JSON to disk keyed by date. Add `GetLotSize(symbol) (int, error)` and `GetExpiries(underlying) ([]time.Time, error)` from the instrument master (needed by WP-6/WP-9).
11. **Stop-loss orders at broker.** Add `PlaceStopLossOrder(symbol, qty, triggerPrice, side)` using variety `STOPLOSS`, ordertype `STOPLOSS_MARKET` (endpoint A-2, same as regular order with different variety/ordertype). Add `CancelOrder(orderID)` via endpoint A-3.
12. **No secrets in logs.** Remove/redact token and key logging (`:202` and anywhere else found in owned files).

Acceptance:
- `go build ./... && go vet ./...` pass.
- New unit tests in `internal/broker/angel_broker_test.go` using `httptest` fake Angel server covering: pending→indeterminate, partial fill qty, sign crossover, WAF position fetch = error, 401→refresh flow. `go test -race ./internal/broker/` passes.
- Grep proves: no `1.0` fill fallback, no lock held across `httpClient.Do` in order path.

---

### WP-2 — Risk Engine Correctness (math + charges FY 2025-26)

**Objective:** Correct charge model, working throttle, thread-safe state, margin-aware validation hooks.
**Findings:** CR-2 (function side), CR-13 (validation side), EX-3, EX-4, parts of EX-9 (races).
**Owned files:** `internal/risk/risk.go`, `examples/risk_example.go`.
**Allowed deps:** stdlib only.

Tasks:
1. **Charges (EX-4).** Split `TradeType` `FNO` into `FutIntraday`, `FutCarry`, `OptIntraday`, `OptCarry` (keep `FNO` as deprecated alias mapping to options to avoid breaking callers). Rates:
   - STT: options **0.1% of premium, sell side only**; futures **0.02%, sell side only**; equity intraday 0.025% sell; equity delivery 0.1% both sides.
   - Exchange txn (NSE): options **0.03503% of premium**; futures **0.00173%**; equity 0.00297%.
   - Stamp duty (buy side only): options **0.003%**; futures **0.002%**; equity delivery 0.015%; equity intraday 0.003%.
   - SEBI fee ₹10/crore (0.0001%) both sides; **GST 18% on brokerage + txn + SEBI fee**.
   - Brokerage: min(₹20, 0.03% of turnover) per executed order; **flat ₹20 per order for F&O**.
   These rates are current as of FY 2025-26; put them in one table-like struct so they are editable in one place, and note the as-of date in a comment.
2. **Throttle (EX-3).** Make the per-minute order counter actually per-minute: sliding window or ticker-reset under lock. `ResetOrderCount` must be locked. Export `SetMaxOrdersPerMin`.
3. **Thread safety (EX-9).** Every getter (`GetCurrentBalance`, `GetRealizedPnL`, `GetRemainingBalance`, `GetSessionStatsWithPrices`, `StopLossConfig` access) must take the mutex. Run `go test -race` on new tests to prove.
4. **Margin-aware SELL validation (CR-13 hook).** Add `ValidateOrderWithMargin(price, qty, tradeType, side, requiredMargin float64)`: for option/future SELL, validate `requiredMargin` (caller supplies from broker margin API — WP-9 wires it) against available balance instead of premium×qty. Premium×qty path stays for BUY. If `requiredMargin <= 0` on a SELL, REJECT (fail-closed, not fail-open).
5. **Locked-capital correctness (EX-9).** SELL entries must lock margin (from #4), not turnover+charges. `UpdatePositionPrice` must recompute `EntryCharges` at the corrected fill price.
6. **CheckRisk hardening (CR-2 function side).** Make `CheckRisk()` cheap, side-effect-free, and safe to call every tick; return a typed result `{Breached bool, Reason string}`. (WP-9 wires the call site.) Make `KillSwitch` an `atomic.Bool` with `TriggerKillSwitch()` / `KillSwitchActive()` exported.
7. **Unified fee model.** Export `EstimateCharges(...)` so the broker/backtest stop maintaining their own disagreeing fee math (audit: `angel_broker.go:811` vs risk model). Document for WP-1/WP-7 consumption.

Acceptance:
- Unit tests `internal/risk/risk_test.go`: golden-value tests for each trade type/side charge calc (write expected values by hand from the rates above), throttle reset over simulated time, race test on concurrent open/close/getters. `go test -race ./internal/risk/` passes.
- A worked example in the report: full round-trip cost for 1 lot (75 qty) NIFTY option at ₹150 premium, buy+sell. (Expected order of magnitude: ~₹47: brokerage ₹40 + STT ₹11.25 sell + txn ₹7.88 + GST ~₹8.6 + stamp ₹0.34 + SEBI ~₹0.02 — verify precisely with your implementation.)

---

### WP-3 — State Persistence & Reconciliation Library

**Objective:** Durable position/risk state so restart never orphans positions.
**Findings:** CR-8, ST-4 (persistence side), CR-7 (reconciliation logic).
**Owned files:** NEW package `internal/state/` (create), `models/trade.go`.
**Allowed deps:** `modernc.org/sqlite` (pure-Go, no cgo) OR stdlib JSON+WAL file. Choose sqlite.

Tasks:
1. Design `state.Store` with: `SavePosition`, `ClosePosition`, `ListOpenPositions`, `SaveOrderAttempt(clientOrderID, ...)`, `MarkOrderResolved`, `SaveRiskSnapshot(balance, realizedPnL, sessionUsed)`, `LoadRiskSnapshot`, `SaveStrategyState(name, key, value)` / `LoadStrategyState` (for nine_twenty's `entered` flag etc.).
2. Every mutation is a synchronous transactional write (WAL mode). DB file path configurable; default `data/titan_state.db`.
3. **Client order IDs (CR-7).** Generate unique client order IDs (`titan-<unixnano>-<seq>`); persist BEFORE the network call ("intent"), resolve after ("filled/rejected/indeterminate"). This gives WP-1/WP-9 the audit chain for ambiguous-timeout recovery.
4. **Reconciliation engine (CR-7/CR-8).** Pure function: `Reconcile(internal []Position, broker []Position) ReconcileReport` producing: matched, missing-at-broker (phantom internal), missing-internal (orphan at broker), qty mismatches. No I/O in this function — WP-9 feeds it broker data.
5. Rewrite `models/trade.go`: drop GORM tags (no DB behind them — audit EX-8), define plain structs used by the store: `Position`, `OrderAttempt`, `RiskSnapshot`.
6. Startup helper: `RecoverSession(store) (openPositions, riskSnapshot, error)` — what WP-9 calls before trading.

Acceptance:
- `go test -race ./internal/state/`: crash-simulation test (write, reopen store, verify state), reconcile-report table tests covering all 4 mismatch classes.
- No imports of broker/engine/strategy packages (keep it a leaf library).

---

### WP-4 — API Server & Mobile Security

**Objective:** Auth everywhere, no credential reuse, real stop control surface, no WS panics.
**Findings:** CR-1 (API-key reuse/logging parts), CR-4 (server side), §5 API findings.
**Owned files:** `internal/api/server.go`, `mobile/titanmobile.go`.
**Allowed deps:** stdlib + existing `gorilla/websocket`.

Tasks:
1. **Token (CR-1).** `NewServer` takes a dedicated token; if empty → generate `crypto/rand` 32-byte hex, print ONCE to console at startup, never log afterwards. Delete the `apiKey[:8]` log line (`server.go:120`) and the `titan-mobile-secret` fallback (here and `titanmobile.go:78`). Token must never default to the broker API key — enforce inside server (reject construction if caller passes something equal to an obvious broker key? no — just document; WP-9 fixes the caller in `titan.go`).
2. **Bind + TLS (§5).** Default bind `127.0.0.1:8080`; config field `api.bind_addr` (report to WP-8). Optional TLS via cert/key paths if provided.
3. **WS auth (§5).** `/ws/live` requires the token (query param `?token=` or header) and validates `Origin` against an allowlist; wrap with the same middleware.
4. **WS write serialization (§5).** One writer goroutine per connection consuming a buffered channel; heartbeat and broadcast both enqueue. Slow/full client → drop connection, never block or double-write.
5. **CORS (§5).** Replace `*` with configurable allowlist; default: no CORS headers (native app needs none).
6. **Real control surface (CR-4).** Server exposes `SetControlHooks(hooks ControlHooks)` where `ControlHooks{ Pause func() error; Resume func() error; KillAndFlatten func() error; Status func() EngineStatus }`. `/api/stop` → `Pause`, `/api/start` → `Resume`, new `/api/kill` → `KillAndFlatten`. If hooks are nil, endpoints return `503 not wired`, NOT fake success. WP-9 wires the hooks.
7. **Config endpoint (§5).** Validate ranges (session balance > 0, ≤ configurable max; strategy ∈ registered list passed in via hooks); reject otherwise; `GET /api/config` returns actual values from hooks, not hardcoded.
8. **Mobile (`titanmobile.go`).** Keep paper-mode hardcode (`:46`) — do not change. Config file written `0600` not `0644` (`:96`). Remove hardcoded secret (#1).

Acceptance:
- `go test ./internal/api/`: auth-required tests (REST + WS), WS concurrent broadcast+heartbeat stress test with `-race`, stop-without-hooks returns 503.
- `go vet` clean; grep proves no `titan-mobile-secret` and no token logging.

---

### WP-5 — Trade Ledger

**Objective:** Append-only durable trade record with broker order IDs; CSV demoted to export.
**Findings:** EX-8.
**Owned files:** `internal/logger/csv_logger.go`, NEW package `internal/ledger/`.
**Allowed deps:** same sqlite driver as WP-3 (coordinate: both use `modernc.org/sqlite`).

Tasks:
1. `internal/ledger`: append-only table `trades` (ts, client_order_id, broker_order_id, symbol, side, qty, req_qty, price, status, charges, realized_pnl, strategy, mode paper/live, note). Statuses: intent, filled, partial, rejected, indeterminate, reconciled.
2. Writes are transactional and synchronous. Daily file or single DB — single DB `data/titan_ledger.db`, plus `ExportCSV(dateRange, path)`.
3. Fix `csv_logger.go:31`: `O_TRUNC` → `O_APPEND`; date-stamped filename `trades_YYYY-MM-DD.csv`; write header only when creating. Keep CSV logger functional (dashboard reads it) but mark as secondary.
4. Add `OrderID` and `Status` columns to the CSV row format; tolerate old rows (dashboard compatibility note in report for WP-11).

Acceptance:
- `go test ./internal/ledger/`: append-restart-append preserves rows; export matches inserted rows.
- Grep proves no `O_TRUNC` in logger.

---

### WP-6 — Strategy Layer Fixes

**Objective:** Correct indicators, IST time, position-aware signals, SL semantics, safe data handling.
**Findings:** CR-14 (strategy side), ST-1…ST-5, ST-8, ST-9, ST-10 items, CR-12 (leg ordering).
**Owned files:** ALL of `internal/strategy/`, `internal/broker/historical.go`.
**Allowed deps:** stdlib only.

Tasks:
1. **Signal semantics (ST-8).** Extend `Signal`: add actions `Exit` and `Hold` distinct from directional `Buy`/`Sell`; add optional fields `StopLossPrice float64`, `TargetPrice float64`, `Legs []OrderLeg`. Add `Expiry string` and `Quantity int` (in lots) to `OrderLeg`. Document the new contract in `strategy.go` comments — WP-7/WP-9 consume it.
2. **Position awareness (ST-5).** Change interface: `Evaluate(ctx EvalContext) Signal` where `EvalContext{Symbol string; Prices []float64; Volumes []float64; Candles []Candle; Now time.Time; HasPosition bool; PositionAge time.Duration; EntryPremium float64}`. Update every strategy. `short_straddle` and `iron_fly`: emit entry only when `!HasPosition`; emit `Exit` only when `HasPosition`.
3. **IST (ST-3).** Package-level `var IST = time.LoadLocation("Asia/Kolkata")` (panic at init on failure); all clock logic (`nine_twenty.go:39-44`, any `Hour()`/`Clock()` use) converts via `.In(IST)`.
4. **nine_twenty (ST-4 + CR-14).** Add combined-premium stop: `Exit` when current combined premium ≥ entry premium × configurable multiplier (default 1.4). State: expose `Snapshot() map[string]string` / `Restore(map[string]string)` so WP-9 can persist via WP-3's store; `entered` flips only on `ConfirmEntry()` callback (WP-9 calls after fills confirm), not on signal generation. Full-date day reset, not `Day()` only.
5. **short_straddle (CR-14).** Same combined-premium stop; RSI-band exit stays as secondary.
6. **iron_fly (CR-12).** Reorder declared legs: BUY wings first, SELL body last. Comment explains hedge-first requirement. Wing width: add config in points but validate against strike step; document delta-based widths as TODO.
7. **sniper (ST-9).** Wall-clock 5-min IST candle aggregation (use `EvalContext.Now`); set `Candle.Time`; volume = delta of cumulative; one-signal-per-completed-candle latch; wire `StopLossPct`/`TargetPct` into `Signal.StopLossPrice/TargetPrice` (computed off entry reference) or delete the fields — wire them.
8. **Indicators (ST-1, ST-2, ST-10).** RSI: `avgLoss==0 → 100`, both zero → 50. VWAP: session-anchored (reset at 09:15 IST), typical price (H+L+C)/3 when candles available, per-interval volume deltas; when only tick LTP available, document degraded mode. Engulfing: strict inequality on at least one side + current body > previous body. Fix momentum's fake normalization (`momentum.go:149,194`): divide by actual accumulated weight.
9. **rsi_reversal (ST-10).** Add mean-reversion exit: exit long when close > SMA(5) or RSI > 50 (mirror for short). Keep entry thresholds configurable.
10. **historical.go (ST-7, ST-10).** Propagate parse errors (`getFloat`); reject rows with non-positive prices or High<Low; assert non-decreasing timestamps; truncate `to` to last completed interval boundary (IST); format request times in IST.
11. **Empty-history guards (ST-10/M7).** Every `EvaluateCandles`/`Evaluate` returns Hold on insufficient data, no panics.
12. **registry (ST-10/L4).** Cache instances: `Get` returns the same instance per name (mutex-guarded); add `Reset(name)` for tests.

Acceptance:
- `go test -race ./internal/strategy/...`: table tests for RSI(100/0/50 cases), session VWAP vs hand-computed values, engulfing strict cases, sniper one-signal-per-candle, nine_twenty IST entry window + premium stop + snapshot/restore round-trip, historical row rejection.
- `go build ./...` may break `cmd/` due to interface change — acceptable ONLY in `cmd/` and `internal/engine`; list every broken call site precisely in your report for WP-9. Everything under `internal/strategy/` and `internal/broker/historical.go` must compile and pass tests standalone.

---

### WP-7 — Backtest Engine Rebuild

**Objective:** Backtest that produces meaningful numbers: real entries, real option pricing, real costs, next-open fills.
**Findings:** CR-9, CR-10, CR-11 (backtest side), ST-6, ST-7, ST-10 (M3, M5).
**Owned files:** `cmd/backtest/main.go`, NEW `internal/backtest/` package.
**Depends on interfaces from:** WP-6 (Signal/EvalContext — code against the contract in WP-6's task 1-2; if racing ahead, stub locally and note for WP-9), WP-2 (`EstimateCharges`).
**Allowed deps:** stdlib only.

Tasks:
1. Move engine logic into `internal/backtest` (portfolio, fills, costs); `cmd/backtest/main.go` becomes a thin CLI.
2. **Entries (CR-10).** Handle leg-less directional signals: enter when flat on `Buy`/`Sell`, exit on `Exit`/opposite. Handle `Legs` signals as multi-leg position. Remove the dead-code structure.
3. **Fills (ST-7).** Signal on candle *i* close → fill at candle *i+1* open + slippage (configurable, default 0.05% equity / 1 tick minimum options). Never fill on the signal candle.
4. **Option pricing (CR-9).** Replace constant-delta model with Black-Scholes repricing per candle: inputs spot (underlying candle close), strike, time-to-expiry, r (default 6.5%), IV. IV source: constant per-run configurable (default 12% NIFTY) as v1 — document clearly that historical-IV feed is required for production-grade results (Phase 3). Compute leg P&L as premium difference from full repricing — this gives real delta, gamma, and theta including long-leg decay. Include `Expiry` from `OrderLeg` (WP-6). Implement BS in `internal/backtest/bs.go` with put/call closed forms + tests against known values.
5. **Costs (ST-6).** Per-leg, per-side charges via WP-2's `EstimateCharges` with the leg's simulated premium; flat brokerage per order per leg per side. Add per-leg spread cost (half-spread configurable, default 0.3% of premium).
6. **Lot size (ST-10/M3).** From instrument master when available (WP-1's `GetLotSize`), else CLI flag `-lotsize`, no hardcoded 50.
7. **Reporting (ST-10/M5).** Output: trades, win rate, gross/net P&L, max drawdown, profit factor, expectancy/trade, avg win/loss, worst day, per-month table. Add `-from/-to` date range flags (not fixed 30 days), `-csv` candle-cache path so runs are reproducible offline (cache fetched candles to disk; reuse if present).
8. **No credentials required for cached runs.** If cache exists, run without broker login.

Acceptance:
- `go test ./internal/backtest/`: BS pricing golden tests (e.g., ATM call, known σ/T), next-open fill test, short-straddle-on-trend-day test proving LOSS is now possible (construct synthetic trending candles; assert negative P&L), charge-per-leg test.
- Report includes one full sample run output on synthetic data.

---

### WP-8 — Infrastructure, Config & CI

**Objective:** Buildable deployment, secret-free config, CI gates.
**Findings:** CR-16, §5 infra items, EX-9 (config wiring), CR-1 (config side).
**Owned files:** `docker-compose.yml`, NEW `go-engine/Dockerfile`, `py-brain/**` (all), all `*.ps1`, NEW `.gitignore`, NEW `.github/workflows/ci.yml`, `internal/config/config.go`, NEW `config.example.yaml`.

Tasks:
1. **Config struct (CR-1, EX-9).** `internal/config/config.go`: every credential field resolves from env first (`ANGEL_CLIENT_CODE`, `ANGEL_PIN`, `ANGEL_API_KEY`, `ANGEL_API_SECRET`, `ANGEL_TOTP_SECRET`, `TITAN_API_TOKEN`), YAML value used only if env empty; startup ERROR (not warning) if live mode and any credential came from YAML. Add missing fields: `risk.max_orders_per_min` (EX-3), top-level `engine:` block (fix the nesting bug — accept BOTH old nested location and top-level, prefer top-level, log deprecation), `api.bind_addr`, `api.allowed_origins`, `state.db_path`, `ledger.db_path`, plus fields requested in Wave-1 reports (collect at end).
2. **config.example.yaml.** Full template, placeholders only, every field commented. `config.yaml` goes into `.gitignore`.
3. **.gitignore.** `config.yaml`, `*.exe`, `logs/`, `data/`, `*.csv`, `.idea/`, `mobile-app/android/.gradle/`, `mobile-app/android/app/build/`, `__pycache__/`.
4. **Dockerfiles.** `go-engine/Dockerfile` (multi-stage: golang build → distroless/alpine run). Fix `py-brain/Dockerfile.gpu:19` out-of-context COPY (root build context or vendor protos). Compose: remove published ports for postgres/redis (internal only), env-var DB password, `restart: unless-stopped`, healthchecks (`pg_isready`, `redis-cli ping`), remove obsolete `version:` key. Dashboard: CSV path via env var.
5. **requirements.txt.** Fix invalid `--index-url` per-requirement line (own line); pin exact versions; drop `cudf`/GPU deps from default requirements into `requirements-gpu.txt` (they're unused stubs — audit §6).
6. **py-brain stubs.** `src/strategies/indicators.py`: remove hard `cudf` import (pandas implementation or explicit `NotImplementedError` with message); `dashboard/app.py`: read initial balance from env `TITAN_SESSION_BALANCE` (default 10000, matching scripts) instead of hardcoded 1000 (§5); parameterize CSV path via env.
7. **Scripts.** `stop.ps1`: replace force-kill-by-window-title with: call `/api/stop` then `/api/kill` (token from env), fallback instruction printed. `start-*.ps1`: build binary (`go build -o titan.exe ./cmd`) and run it, not `go run`; read env vars; never echo secrets.
8. **CI.** `.github/workflows/ci.yml`: `go build ./...`, `go vet ./...`, `go test -race ./...` (working dir `go-engine`), `pip install -r py-brain/requirements.txt --dry-run` sanity, plus `gitleaks` or a simple grep-based secret scan.

Acceptance:
- `docker compose config` validates; `docker build` of go-engine Dockerfile succeeds (compose full build may need WP-9 merge — note if so).
- `go build ./...` passes with new config fields; unit test for env-over-YAML precedence and the engine-block nesting fallback.
- Fresh-clone simulation: no secrets tracked, example config complete.

---

## 4. Wave 2 — WP-9 Integration (single agent, after ALL Wave-1 reports)

**Objective:** Wire everything into one safe live/paper engine loop. THE ONLY package editing `cmd/main.go`, `internal/engine/engine.go`, `internal/app/*`, `internal/cli/interactive.go`, `internal/feed/feed.go`, `internal/ipc/ipc.go`.
**Findings:** CR-2, CR-3, CR-4 (wiring), CR-7, CR-8 (wiring), CR-12, CR-13 (wiring), CR-14 (engine SL), CR-15 (partial), EX-1, EX-9 (market hours, expiry selection, dead codepaths).
**Prerequisite reading:** every `docs/reports/WP-*-REPORT.md`.

Tasks:
1. **Single engine path.** Delete or fold the dead `internal/app` duplicate loop (`modes.go:103-108`) — one engine construction, one risk manager (fix the double-build at `cmd/main.go:210` vs `:352`). Delete stub `ipc.go`/`feed.go` or leave one-line no-op with honest comments.
2. **Startup sequence:** load config → connect broker → `state.RecoverSession` → fetch broker positions → `state.Reconcile` → if mismatch: print report, require operator flag `-accept-reconcile` or flatten; refuse to trade otherwise (CR-8).
3. **Risk loop (CR-2/CR-3).** Every tick: `CheckRisk()`; on breach → stop entries, flatten all, alert. Kill switch: wire `/api/kill` hook + a sentinel file check (`data/KILL`) each tick → `TriggerKillSwitch()`.
4. **Control hooks (CR-4).** Implement WP-4's `ControlHooks` against the real loop: Pause blocks new entries (exits still allowed), Resume, KillAndFlatten, Status from real state.
5. **Order flow.** Use WP-1's intents: entries normal, exits reduce-only. On `ErrOrderIndeterminate`: do NOT roll back risk state; persist attempt via WP-3; poll order book resolution in background; alert. Multi-leg (CR-12): place BUY legs first, await confirmed fills, then SELL legs; any rejection → unwind filled legs immediately (reduce-only, bypasses circuit breaker), alert, mark strategy flat.
6. **Margin (CR-13).** Before SELL option/future orders, fetch required margin (Angel margin API A-6); pass to `ValidateOrderWithMargin`. If margin API fails → REJECT entry (fail-closed).
7. **Stops (CR-14).** On every confirmed entry: place broker-side SL-M via WP-1 (per-leg for multi-leg; combined-premium software stop supplements). Keep the software SL loop; use `GetCurrentPriceWithAge` — if age > threshold (default 15s), treat price invalid: no SL decisions on stale data, alert instead. Market-order closes must NOT require a price fetch (EX-1) — pass last-known for logging only.
8. **Market hours (EX-9).** Gate entries to 09:15–15:30 IST minus configurable buffer; intraday square-off at configurable time (default 15:20); block everything on weekends/NSE holidays (hardcode 2026 NSE holiday list in a table with as-of comment).
9. **Expiry/symbols (EX-9).** Replace weekday-arithmetic expiry logic in discovery usage with WP-1's `GetExpiries` from instrument master; nearest expiry ≥ today; delete hardcoded `20JAN26` dependence (config value becomes optional override, validated against master). Lot sizes via `GetLotSize` everywhere (kill 25/50 fallbacks).
10. **Strategy wiring.** New `EvalContext` interface from WP-6: pass `HasPosition`, entry premium, IST now; call `ConfirmEntry` only after fills; persist strategy snapshots via WP-3 each mutation, restore at startup.
11. **Ledger.** All order intents/results into WP-5 ledger; CSV secondary.
12. **Shutdown (EX-9).** Context-based: signal → stop entries → cancel open orders → flatten with retries (3x, escalating log) → verify broker position book empty → exit 0; else exit 1 with loud CRITICAL log. No `os.Exit` mid-flight; WaitGroup for the loop.
13. **Watchdog (CR-15).** Heartbeat file `data/heartbeat` touched each tick + optional Telegram alert on: kill switch, drawdown breach, reconcile mismatch, indeterminate order, flatten failure, auth failure. Telegram via simple bot-token env vars (`TITAN_TG_TOKEN`, `TITAN_TG_CHAT`); no-op if unset. Separate tiny `cmd/watchdog/main.go` binary: alerts if heartbeat stale > N sec.

Acceptance:
- `go build ./... && go vet ./... && go test -race ./...` all green.
- Paper-mode session runs: start → mock trades → Ctrl+C → clean shutdown, state DB populated; restart → recovery + reconcile path exercised.
- Kill sentinel file test: create `data/KILL` mid-run → entries stop, flatten attempted.
- Demonstrate `/api/stop` actually halts entries (log evidence).

---

## 5. Wave 3 (parallel, after WP-9)

### WP-10 — Validation Harness
**Owned files:** NEW `docs/validation/`, NEW scripts under `cmd/backtest` flags only via report (no code edits outside new files).
1. Fetch + cache ≥2 years NIFTY 5-min candles (and option candles where available) using the WP-7 cache format.
2. Walk-forward protocol: rolling 6-month train / 1-month test for each strategy; parameter sensitivity sweeps; report expectancy, PF, max DD, Sharpe, worst day per window.
3. Produce `docs/validation/RESULTS.md` with go/no-go per strategy against gates: PF > 1.3 OOS, max DD < 15% of deployed margin, positive expectancy after 2x modeled costs.
4. Define the live gate checklist (1 lot, broker-side daily loss cap, watchdog verified, 1 month reconciled paper).

### WP-11 — Documentation Truth Pass
**Owned files:** `README.md`, `RUNNING.md`, `PAPER_TRADING.md`, `docs/MOBILE_APP_DESIGN.md`, NEW `docs/RUNBOOK.md`.
1. Rewrite README to describe ONLY what exists post-remediation (no TimescaleDB/Redis/gRPC/Arrow/GPU/Zerodha/HFT claims — audit §6). Roadmap section for the rest, clearly marked NOT IMPLEMENTED.
2. Mobile design doc: security checklist reflects actual WP-4 state.
3. `docs/RUNBOOK.md`: procedures for broker outage, crash with open positions, session expiry, expiry day, kill-switch use, reconcile mismatch, credential rotation.
4. Update setup docs for env-var credentials + new scripts.

---

## Appendix A — Angel One SmartAPI Endpoints (only these; do not invent others)

Base: `https://apiconnect.angelone.in` (as used in existing `angel_broker.go` — verify the base constant in code and keep consistent).

| ID | Purpose | Method/Path |
|---|---|---|
| A-1 | Login | POST `/rest/auth/angelbroking/user/v1/loginByPassword` (already implemented) |
| A-2 | Place order | POST `/rest/secure/angelbroking/order/v1/placeOrder` (already implemented; variety `NORMAL`/`STOPLOSS`, ordertype `MARKET`/`LIMIT`/`STOPLOSS_MARKET`, producttype `INTRADAY`/`CARRYFORWARD`) |
| A-3 | Cancel order | POST `/rest/secure/angelbroking/order/v1/cancelOrder` |
| A-4 | Token refresh | POST `/rest/auth/angelbroking/jwt/v1/generateTokens` (body: `refreshToken`) |
| A-5 | Funds/RMS | GET `/rest/secure/angelbroking/user/v1/getRMS` |
| A-6 | Margin calculator | POST `/rest/secure/angelbroking/margin/v1/batch` |
| A-7 | Order book | GET `/rest/secure/angelbroking/order/v1/getOrderBook` (already used in fill polling) |
| A-8 | Positions | GET `/rest/secure/angelbroking/order/v1/getPosition` (already implemented) |
| A-9 | Historical candles | POST `/rest/secure/angelbroking/historical/v1/getCandleData` (already implemented) |
| A-10 | Quotes | POST `/rest/secure/angelbroking/market/v1/quote/` (already implemented) |
| A-11 | Instrument master | GET `https://margincalculator.angelone.in/OpenAPI_File/files/OpenAPIScripMaster.json` (already implemented) |

Response envelope: `{"status": bool, "message": string, "errorcode": string, "data": {...}}`. Auth failures surface as HTTP 401 or `status:false` with error codes prefixed `AG`/`AB` — handle both.

## Appendix B — Shared Conventions

- Time: ALL market logic in `Asia/Kolkata`; UTC only for storage timestamps (store both where useful).
- Money: `float64` retained for now (matches codebase); round display to 2dp; no equality comparisons on floats in tests without epsilon.
- Errors: typed sentinel errors for cross-package contracts (`ErrOrderIndeterminate`, `ErrStalePrice`, `ErrKillSwitch`).
- Logging: `log.Printf` retained this round; structured logging is deliberately OUT OF SCOPE (avoid cross-package churn); never log secrets/tokens.
- Tests: table-driven, `-race` mandatory where goroutines exist.
- Fail-closed principle: when uncertain (margin unknown, price stale, order indeterminate) the system must refuse to open risk, never assume.
