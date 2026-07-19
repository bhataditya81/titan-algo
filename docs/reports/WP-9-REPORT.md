# WP-9 — Integration — Report

## Starting state (picked up cold, no memory of the prior instance)

`internal/engine/engine.go` was modified (order-flow primitives: `PlaceEntryOrder`
/ `PlaceExitOrder`, ledger/state wiring, `ErrOrderIndeterminate` handling) and
was sound — kept as-is except where noted. **`internal/engine/runner.go` (1043
lines) already existed on disk, untracked in git, and turned out to implement
almost the entire WP-9 task list already**: the tick loop, market-hours
gating, kill-sentinel + risk-breach handling, Pause/Resume/KillAndFlatten/
Status control hooks, multi-leg entry with hedge-first unwind-on-rejection,
broker-side + software stop-loss, combined-premium feed for multi-leg
positions, expiry/lot-size resolution via the instrument master, and
context-cancellation graceful shutdown with retry/backoff. None of this was
mentioned in the task's "current state" briefing, which only knew about
`engine.go` — the runner.go file is real, compiles, and (after one fix, see
below) is now covered by an automated test.

**What was actually broken:** every caller (`cmd/main.go`,
`internal/app/titan.go`, `internal/app/modes.go`) still called the *old*
3-arg `NewTradingEngine` and the removed `ClosePosition`/`PlaceOrder`
methods, and `cmd/main.go` ran its own hand-rolled duplicate strategy loop
that never used `Runner` at all. None of the 13 WP-9 tasks were actually
wired into either process entry point.

## What I did

### 1. Single engine path (task 1)
- Deleted `internal/app/modes.go` entirely (`runModeALoop` was a dead,
  half-implemented duplicate of the loop `Runner` already implements).
- Deleted `cmd/main.go`'s ~650-line duplicate strategy loop
  (`runModeA`/`runModeB`/`getOptionSymbol`/`findHighestVolumeSymbol`/
  `findTopNVolumeSymbols`) and replaced it with `engine.NewRunner(...)` +
  `runner.Run(ctx)`. `internal/app/titan.go.RunStrategy` now does the same.
  Both entry points construct exactly one `TradingEngine` and one `Runner`.
- `MODE_B` (a read-only price-monitor loop, no risk) was removed rather than
  ported — `TITAN_MODE` is now reserved for internal/config's live/paper
  gate (WP-8) and overloading it for A/B selection would conflict with that.
  If a monitor-only mode is wanted back, it's a `-search`-style side branch
  with no engine involvement — flagged as a follow-up, not done.
- `internal/feed/feed.go` / `internal/ipc/ipc.go` left untouched as stubs
  (out of scope per the plan).

### 2. Startup: config → broker → recover → reconcile (task 2)
`cmd/main.go` now: `config.Load` → (if `-live`) `cfg.ValidateLiveCredentials()`
→ build broker → open `state.Store` + `ledger.Ledger` → `tradeService.Connect()`
→ `state.RecoverSession` → `tradeService.GetPositions()` → `state.Reconcile`.
On a non-clean reconcile (phantom/orphan/quantity-mismatch), it logs every
item and **refuses to start** unless `-accept-reconcile` is passed (new flag).

**Found and fixed a real gap while building the manual-verification test**:
recovering positions from the store and reconciling them was purely a
startup *gate* — nothing ever fed the recovered positions back into the
*live* `risk.Manager`'s position book. A restarted process would enforce
drawdown/stop-loss against an empty risk book even though the store (and the
broker) still showed open risk, and a recovered leg had no `PositionID` so
closing it after restart would never mark the store row `CLOSED`. Fixed in
`Runner.RestoreState()` (`internal/engine/runner.go`): it now rebuilds
`riskManager.OpenPositions` and `CurrentBalance`/`RealizedPnL`/
`SessionBalanceUsed` from `store.ListOpenPositions()` /
`store.LoadRiskSnapshot()`, and cross-references store positions by symbol so
restored `legRecord`s keep their `PositionID`. This used `risk.Manager`'s
already-exported `OpenPositions` map/fields directly — no changes to
`internal/risk/risk.go` (an owned-by-another-WP file) were needed.

### 3. Risk loop (task 3)
Already correct in `runner.go`'s `tick()`: `CheckRisk()` every tick (breach →
pause + `flattenAll`), `checkKillSentinel()` every tick (stats `data/KILL`,
calls `TriggerKillSwitch()` if present — same effect as the runtime kill
switch). Verified in the smoke test.

### 4. Control hooks (task 4)
`cmd/main.go` and `titan.go` both call `apiServer.SetControlHooks(api.ControlHooks{...})`
wired to `runner.Pause` / `runner.Resume` / `runner.KillAndFlatten` /
`runner.Status` (converted to `api.EngineStatus`). Verified Pause/Resume via
the smoke test (`Status().Running` flips correctly); `/api/stop` and
`/api/kill` now reach a real engine instead of returning 503 "not wired".

### 5. Order flow / Order.Intent (task 5, issue 4)
Already correct in `engine.go`/`runner.go`: `IntentEntry` on entries,
`IntentReduceOnly` on every exit/unwind/flatten order. `ErrOrderIndeterminate`
on entry does not roll back risk state (persists to store, alerts); on exit,
same. Multi-leg entries place legs in the strategy's given order (hedge-first
per WP-6), await each fill, and on any leg's rejection or indeterminate
result immediately `unwindLegs` the ones already filled (reduce-only,
resting SL cancelled first) and alert.

### 6. Margin-aware sizing (task 6)
`runner.requiredMargin()` fails closed with a clear error, per the plan's
explicit instruction not to invent a margin endpoint WP-1 didn't build —
SELL derivative entries are blocked until Angel's margin-calculator API (A-6)
is wired. **Known, documented gap** (not new — inherited from the runner.go
I found on disk, verified correct against WP-1's report).

### 7. Stop-loss wiring (task 7)
`placeBrokerStop` (via `ExtendedTradeService.PlaceStopLossOrder`) on every
confirmed entry; falls back to software-only if the broker doesn't implement
the extended interface. `softStopLossCheck` uses
`GetCurrentPriceWithAge`; treats >15s-stale prices as invalid (skip + alert,
never fires blind). `PlaceExitOrder` (engine.go) never requires a fresh price
— EX-1 confirmed fixed by design (market exit proceeds even if
`GetCurrentPrice` returns 0, only logs a warning).

### 8. Market hours gate (task 8)
`marketState()` in `runner.go`: blocks entries outside 09:15(+buffer)–15:20
default, blocks weekends, square-off flatten at 15:20, hard close 15:30. A
small, **deliberately incomplete** 2026 fixed-holiday table is documented in
code (`nseHolidays2026`) — movable festival holidays are not covered
(best-effort per the plan).

### 9. Expiry/lot-size resolution (task 9)
`resolveExpiry`/`lotSize` in `runner.go` use `InstrumentManager.GetExpiries`/
`GetLotSize` first, falling back to config override then a hardcoded
constant, loudly logged at each fallback tier. No more weekday arithmetic.

### 10. Strategy wiring (task 10)
`EvalContext` built per-tick with `HasPosition`/`PositionAge`/`EntryPremium`
correctly populated; while a multi-leg structure is open, `Prices` carries
the **combined current premium** series (`combinedLegPremium`), not
underlying spot (issue 5 — mirrors WP-7's backtest engine pattern).
`ConfirmEntry`/`ConfirmExit` called only after real fill confirmation (i.e.
after `PlaceEntryOrder`/`PlaceExitOrder` succeed for every leg).
`persistStrategySnapshot`/`RestoreState` handle save/restore. Fixed the
risk-manager-recovery gap described under task 2 above.

### 11. Ledger (task 11)
Verified `engine.go`'s `ledgerAppend` — every entry/exit intent, fill,
rejection, and indeterminate outcome is routed through `ledger.Ledger.Append`
with the correct `ledger.Status*` and `ledger.Mode` (paper/live) stamped.
Found correct and complete as inherited; no changes needed.

### 12. Graceful shutdown (task 12)
`cmd/main.go` uses `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`
→ `runner.Run(ctx)` (blocks) → on cancellation, `Runner.Shutdown()` pauses
entries, flattens with 3 retries / escalating backoff, and verifies the
broker + risk-manager position books are both empty before returning. A
non-nil return causes `os.Exit(1)` with a CRITICAL log; nil returns exit 0.
Verified end-to-end in the smoke test (`TestRunnerSmoke`'s final phase).

### 13. Watchdog (task 13, best-effort)
Heartbeat file (`data/heartbeat`) written every tick. `buildAlertFunc()` in
`cmd/main.go` posts to the Telegram Bot API using `TITAN_TG_TOKEN`/
`TITAN_TG_CHAT` env vars; returns `nil` (no-op) if either is unset. Alerts
fire on kill-switch, drawdown breach, stale-price, indeterminate-order,
margin-unavailable, and flatten-failure events (already wired into
`runner.go`'s call sites; `cmd/main.go` only had to supply the `AlertFunc`).
A separate watchdog binary was **not built** — explicitly out of scope per
the plan when short on time; follow-up.

## Critical cross-cutting issues — verified status

1. **Exit-signal handling** — VERIFIED CORRECT. `evaluateSymbol`'s switch has
   an explicit `strategy.Exit` case calling `exitStructure`, which closes
   every leg via `PlaceExitOrder`.
2. **Mobile token/credential reuse** — FIXED. `internal/app/titan.go` no
   longer reads `Config.Brokers.Angel.APIKey` or falls back to
   `"titan-mobile-secret"`; it passes `app.Config.API.Token` (empty by
   default → `api.NewServer` self-generates a random token). Grep-confirmed
   zero occurrences of `titan-mobile-secret` in code (only in this report and
   in `titan.go`'s explanatory comment, as text, not a value).
3. **Config wiring** — FIXED. `cmd/main.go` now uses `internal/config.Load`
   + `ValidateLiveCredentials()` instead of its own local YAML struct/loader.
4. **Broker interface additions** — VERIFIED. `ExtendedTradeService` type
   assertions used at every call site (`BrokerHealthy`, `placeBrokerStop`,
   `unwindLegs`/`exitStructure`'s `CancelOrder`, `softStopLossCheck`'s
   `GetCurrentPriceWithAge`). `Order.Intent` set to `IntentReduceOnly` on
   every exit/unwind/flatten order, `IntentEntry` (default) on entries.
5. **Combined-premium feed** — VERIFIED. `combinedLegPremium` sums each
   open leg's current price and feeds the series into `EvalContext.Prices`
   while a multi-leg structure is open (`evaluateSymbol`).

## Build / test evidence

```
$ cd go-engine && go build ./...
(no output — success)

$ go vet ./...
(no output — success)

$ go test -race ./...
ok  	titan-algo/internal/api        (cached)
ok  	titan-algo/internal/backtest   (cached)
ok  	titan-algo/internal/broker     (cached)
ok  	titan-algo/internal/config     (cached)
ok  	titan-algo/internal/engine     7.621s
ok  	titan-algo/internal/ledger     (cached)
ok  	titan-algo/internal/logger     (cached)
ok  	titan-algo/internal/risk       (cached)
ok  	titan-algo/internal/state      (cached)
ok  	titan-algo/internal/strategy   (cached)
(all other packages: no test files, none expected)
```

## Manual verification

`config.yaml` in this repo contains **real, plaintext Angel One credentials**
(client code, PIN, API key, TOTP secret) — a pre-existing CR-1 condition
WP-8 addresses going forward, but the task's hard constraints forbid editing
`config.yaml`, and env-first resolution can't force a value to *empty* (an
unset env var is indistinguishable from `os.Getenv` returning `""`). Because
of this, actually launching `cmd/main.go -paper` here would have caused the
paper-mode "use live data" branch to connect `AngelBroker` with **real
credentials over the network** — exactly what "never use real credentials"
forbids, even in paper mode. I did not run the interactive CLI end-to-end for
this reason.

Instead, WP-9's acceptance criteria (paper-mode run with simulated
ticks/trades, kill-sentinel halting entries, `/api/stop`-equivalent pausing
entries, state DB populated, restart/recovery exercised, clean shutdown) are
proven by an automated, repeatable test using `MockBroker` (no network, no
credentials) added at `go-engine/internal/engine/runner_smoke_test.go`:

```
$ go test ./internal/engine/ -run TestRunnerSmoke -v
=== RUN   TestRunnerSmoke
... Position OPENED - BUY TESTSYM 10 @ ₹396.69 ...
    ✅ entry: open structures = 1, risk positions = map[TESTSYM:...]
🔴 KILL sentinel file detected (...\KILL) — triggering kill switch
    ✅ kill-sentinel file at ...\KILL triggered KillSwitchActive()=true
🛑 Closing structure [TESTSYM]: test: kill switch
... Position CLOSED - TESTSYM | Net P&L: ₹18.15
    ✅ flattenAll closed all structures; open=0
⏸️  Runner PAUSED via control hook (new entries blocked, exits still allowed)
    ✅ Pause() via ControlHooks -> Status().Running=false
▶️  Runner RESUMED via control hook
    ✅ Resume() via ControlHooks -> Status().Running=true
... Position OPENED - BUY TESTSYM 10 @ ₹397.44 ...
♻️  restored risk position TESTSYM: BUY 10 @ ₹397.72
♻️  restored risk snapshot: balance=₹10000.00 realizedPnL=₹0.00 sessionUsed=₹3978.86
♻️  restored open structure TESTSYM: 1 leg(s)
    ✅ restart recovery: runner3.open after RestoreState() = 1 structure(s)
🚀 Runner started [PAPER mode | strategy=dumb-test-strategy | symbols=[TESTSYM] | poll=1ms]
🛑 SHUTDOWN — stopping new entries
🛑 Closing structure [TESTSYM]: graceful shutdown
... Position CLOSED - TESTSYM | Net P&L: ₹319.39
✅ Graceful shutdown complete — position book verified empty
    ✅ graceful shutdown: Run(ctx) returned nil after cancel, position book empty
--- PASS: TestRunnerSmoke (0.76s)
PASS
```

This exercises, against real SQLite-backed `state.Store`/`ledger.Ledger`
files (temp dir): entry → durable position row written → kill-sentinel file
halts and flattens → Pause/Resume control hooks → **a second, independent
`Runner`/`risk.Manager`/`TradingEngine` sharing the same on-disk store**
recovering the open position (this is what a real process restart looks
like) → graceful, context-cancellation-driven shutdown that verifies an
empty position book before returning.

```
$ grep -rn "titan-mobile-secret" go-engine/
go-engine/internal/app/titan.go:139:  // ... (WP-4/CR-1 fix for the "titan-mobile-secret" / ...)  ← comment text only, not a value
```

## Honest gaps / follow-ups (not fixed, in priority order)

1. **`config.yaml` has real, plaintext broker credentials checked into the
   repo.** Out of scope to fix (file is off-limits per this task's
   constraints) but this is a live secret leak — rotate the Angel One
   API key/TOTP secret and move to env vars (`ANGEL_CLIENT_CODE` etc.)
   before any live use, per WP-8.
2. **Margin API (A-6) not implemented** — SELL derivative entries are
   blocked fail-closed. This is correct-but-limiting behavior, not a bug;
   short_straddle/iron_fly-style strategies cannot actually open a real
   position until this lands.
3. **Movable-holiday table is incomplete** (`nseHolidays2026` — fixed dates
   only). Verify against NSE's official circular before live trading near a
   festival holiday.
4. **No dedicated watchdog binary.** Heartbeat file + Telegram alerts exist
   in-process; there is nothing that notices the *whole process* died.
5. **`-search` mode and instrument-master loads still hit the network**
   (Angel's instrument-master JSON is public/unauthenticated, not user
   credentials) — unrelated to the credential-safety concern above, noted
   for completeness only.
6. **True end-to-end manual run of `cmd/main.go -paper` against the real
   interactive CLI/discovery flow was not performed**, for the credential
   reason above. The automated `TestRunnerSmoke` proves the engine/runner
   integration; it does not exercise `cmd/main.go`'s own flag parsing,
   symbol-discovery prompts, or `state.Reconcile` startup gate line-by-line.
   Recommend a supervised operator dry run in an environment where
   `config.yaml` can safely be replaced with empty/mock credentials before
   any live pilot.

## Files touched

- `go-engine/cmd/main.go` — full rewrite: internal/config, state+ledger
  wiring, recover/reconcile gate (`-accept-reconcile`), single `Runner`,
  control-hook API wiring, context-cancellation shutdown, Telegram alert
  wiring. Deleted the old duplicate strategy loop and its now-dead helpers.
- `go-engine/internal/app/titan.go` — fixed the mobile-token/API-key bug,
  wired state store + ledger + `Runner` + control hooks, `Stop()` now
  cancels the runner's context for graceful shutdown.
- `go-engine/internal/app/modes.go` — deleted (dead duplicate loop).
- `go-engine/internal/engine/runner.go` — one functional fix:
  `RestoreState()` now rebuilds `risk.Manager`'s live position book and
  balance/P&L from the durable store, and cross-references `PositionID`
  for restored legs (previously, recovered positions were invisible to risk
  checks and could never be marked `CLOSED` in the store after a restart).
- `go-engine/internal/engine/runner_smoke_test.go` — new: the manual/
  automated verification harness described above.
- `go-engine/internal/engine/engine.go` — reviewed in full, found correct,
  unchanged.
