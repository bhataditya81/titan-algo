# AUDIT R3 — State / Ledger / dbutil / Logger / CLI Entrypoints

**Date:** 2026-07-20
**Scope:** `go-engine/internal/state/*`, `go-engine/internal/ledger/ledger.go`, `go-engine/internal/dbutil/dbutil.go`, `go-engine/internal/logger/csv_logger.go`, `go-engine/cmd/main.go`, `go-engine/cmd/fetchdata/main.go`, `go-engine/cmd/backtest/main.go`, `go-engine/cmd/watchdog/main.go`.
**Method:** Read every file in scope in full (not summaries/prior-report claims), traced the real call graph into the one adjacent file needed to verify each claim (`internal/engine/engine.go`, `internal/engine/runner.go`, `internal/risk/risk.go`) rather than guessing from doc comments, and cross-checked against existing `*_test.go` files to see what's actually covered vs asserted-but-untested. Audit only — no source files were modified; this report is the only file written.

**Read first (per prior format):** `docs/PRODUCTION_READINESS_AUDIT.md` (R1, CR-1..CR-16/ST-1..), `docs/PRODUCTION_GAPS_R2.md` (R2, G-1..G-15), `docs/reports/R2-3-REPORT.md`. This round's findings are new (R1/R2 did not report them) except where explicitly cross-referenced.

---

## 1. Executive Summary

The `state`/`ledger`/`dbutil` packages themselves are well built: SQLite is opened in WAL mode with `synchronous=FULL` (verified — this fsyncs every commit, not just at checkpoint), every mutation is a real transaction, `ledger.Append` is genuinely insert-only (no UPDATE/DELETE anywhere in the package), and the crash-recovery tests (`TestCrashRecovery`, `TestCrashRestartPreservesRows`) actually prove durability across a close/reopen cycle, not just assert it in a comment.

**The bugs are not inside these packages — they are in how the one real production caller (`internal/engine/engine.go`, wired from `cmd/main.go`) uses them.** Three separate mechanisms these packages built specifically to make the system fail-closed around crashes and ambiguous fills are either silently bypassed or never invoked at all:

1. Durable-write failures (`SaveOrderAttempt`, `MarkOrderResolved`, `SavePosition`, `ledger.Append`) are logged and ignored — the order is placed/the trade proceeds regardless (§2.1). This is the same *category* of bug as the fixed ATR-default issue: a path that is supposed to fail closed instead swallows the error and carries on.
2. `Store.ListUnresolvedOrderAttempts` — built and unit-tested specifically for "a startup recovery pass [to] resolve against the broker's order book before trading resumes" — has **zero call sites in production code**. `cmd/main.go` never calls it (§2.2).
3. An entry order that comes back `ErrOrderIndeterminate` (broker's own explicit "we don't know if this filled" signal) is never added to the Runner's tracked-structures map, so if it actually filled at the broker, it runs with **zero per-tick software stop-loss protection** for the rest of the process's uptime — the exact CR-7 "unmonitored option position" scenario from the R1 audit, reincarnated for this one specific edge case that R1's fix didn't cover (§2.3).

None of these are theoretical: each is traced end-to-end below with file:line and the exact code path.

Separately, `cmd/backtest` has a lot-size default that silently mis-sizes any non-NIFTY symbol (§3.1), `cmd/watchdog`'s one-alert-per-episode design (confirmed intentional and tested) has no re-nag/re-retry mechanism for a still-down engine (§3.2), and `cmd/fetchdata`'s resume checkpoint was specifically re-verified **not** to have the "marks done when it wasn't" bug class from the prompt — it's correctly built (§3.3, listed as a clean pass).

| # | Finding | Severity | File |
|---|---|---|---|
| R3-1 | Durable-write failures on the order path are logged and ignored, not fail-closed | **HIGH** | `internal/engine/engine.go` |
| R3-2 | `ListUnresolvedOrderAttempts` — the store's own startup-recovery API — is dead code; indeterminate/intent orders are never checked against the broker at startup | **HIGH** | `internal/state/store.go`, `cmd/main.go` |
| R3-3 | Indeterminate entry orders that actually filled get zero stop-loss coverage until a whole-session risk breach / square-off / shutdown | **HIGH** | `internal/engine/runner.go` |
| R3-4 | API server starts (kill/pause/status reachable) before `RestoreState()` populates in-memory position tracking — narrow kill-during-startup race | MEDIUM | `cmd/main.go` |
| R3-5 | `cmd/backtest` silently defaults lot size to a hardcoded NIFTY value (75) for any symbol when no instrument cache is present, with no warning; file has zero test coverage | MEDIUM | `cmd/backtest/main.go` |
| R3-6 | `cmd/watchdog` alerts/restarts exactly once per stale episode with no further nagging or retry while the outage continues | MEDIUM | `cmd/watchdog/main.go` |
| R3-7 | `strToTime` silently returns the zero time on a parse error instead of propagating it | LOW | `internal/state/store.go` |
| R3-8 | `cmd/backtest` falls back to reading broker credentials from `config.yaml` (no read-only type-level guard, unlike `fetchdata`) | LOW | `cmd/backtest/main.go` |
| R3-9 | CSV logger and SQLite ledger are independently, unsynchronized, best-effort writes — by design, but worth restating given R3-1 | INFO | `internal/logger/csv_logger.go`, `internal/engine/engine.go` |
| R3-10 | `fetchdata`'s resume checkpoint correctly avoids the "marked done prematurely" bug class — verified clean | INFO (no action) | `cmd/fetchdata/main.go` |

---

## 2. Critical/High Findings

### R3-1 (HIGH). Durable-write failures on the order path are swallowed, not fail-closed

**File:** `go-engine/internal/engine/engine.go` — `PlaceEntryOrder` (lines 115-224) and `PlaceExitOrder` (226-319); the ledger sink `ledgerAppend` (83-91).

`internal/state/store.go`'s own doc comment on `SaveOrderAttempt` is explicit:

> "Callers MUST call this with Status = models.OrderIntent BEFORE issuing the order over the network"

But the real (and only) caller does not enforce this:

```go
// engine.go:138-148
if e.store != nil {
    clientOrderID = e.store.NextClientOrderID()
    positionID = e.store.NextPositionID()
    if err := e.store.SaveOrderAttempt(models.OrderAttempt{...}); err != nil {
        log.Printf("⚠️ state.SaveOrderAttempt(intent) failed for %s: %v", symbol, err)
    }
}
e.ledgerAppend(ledger.Trade{...})   // ledgerAppend itself also only logs (83-91)

// ... order is placed on the broker regardless, a few lines later:
order := broker.Order{...}
filled, err := e.broker.PlaceOrder(order)
```

Every downstream write on this path is the same shape — error is logged, execution continues:
- `MarkOrderResolved` on the indeterminate branch (170), the rejected branch (182), and the success branch (201) — all `_ = e.store.MarkOrderResolved(...)`.
- `SavePosition` on the success branch (203-209) — logged, not returned.
- `ClosePosition` in `PlaceExitOrder` (301-303) — logged, not returned.
- `ledger.Append` inside `ledgerAppend` (88-90) — logged, not returned; the helper's signature doesn't even have an error return, so no caller could react even if it wanted to.

**Concrete failure scenario:** the state DB file becomes temporarily unwritable (disk full, WAL file locked by another process — note the project still lives under OneDrive per R2's H-2, which actively syncs/locks files) at the exact moment a SELL entry order is about to be placed. `SaveOrderAttempt` fails, logs a warning, and the code proceeds to call `broker.PlaceOrder` anyway. The order fills for real money. `MarkOrderResolved`/`SavePosition` then also fail (same underlying disk condition), so **the entire durable record of this trade — order_attempts row, positions row, ledger row — never exists**, while the broker has a live position and the in-memory risk manager thinks it does too (since `riskManager.OpenPosition` is called unconditionally regardless of store write success). If the process now exits (crash or normal shutdown) before the underlying disk problem clears, the next startup's `Reconcile` will correctly flag this as an **Orphan** (broker-only) and refuse to start without `-accept-reconcile` — so there is a backstop for the "did the process ever restart" case. But **while the same process keeps running**, nothing about this failure mode causes it to stop trading, alert more loudly than a single log line, or even retry the write — the entire audit trail for that session can silently degrade to nothing while trading continues normally in memory. This directly contradicts the money-critical "no silent fallbacks on any path that affects money — must error instead of guessing" rule: the correct fix is that a `SaveOrderAttempt` failure on the *entry* path (before capital is committed) should abort the order, not merely log and proceed.

**Severity: HIGH.** Not "critical" only because the position-book Reconcile gate at next startup provides an eventual backstop for the crash-recovery case specifically — but it does nothing for the "audit trail silently degrades while the process keeps running" case, which is the actual point of building a synchronous, fsync'd, "crash-safety over throughput" store in the first place.

---

### R3-2 (HIGH). `ListUnresolvedOrderAttempts` is dead code — indeterminate/intent orders are never checked at startup

**File:** `go-engine/internal/state/store.go:426-465` (the API), `go-engine/internal/state/recover.go:23-35` (`RecoverSession`), `go-engine/cmd/main.go:224-255` (the only production call site).

`store.go`'s doc comment on `ListUnresolvedOrderAttempts`:

> "returns every order attempt still in the 'intent' or 'indeterminate' state — i.e. attempts a startup recovery pass needs to resolve against the broker's order book before trading resumes."

Grepping the entire `go-engine` tree for callers of this function turns up exactly two hits, both in `internal/state/state_test.go` — the unit test that proves the function itself works. There is **no production call site**. `cmd/main.go`'s actual startup sequence is:

```go
// cmd/main.go:224-236
openPositions, _, err := state.RecoverSession(store)   // note: risk snapshot return value discarded here too
...
report := state.Reconcile(openPositions, brokerPositions)
```

`RecoverSession` (`recover.go`) itself only calls `store.ListOpenPositions()` and `store.LoadRiskSnapshot()` — it never calls `ListUnresolvedOrderAttempts`, and neither does anything else in `cmd/main.go`, `internal/engine`, or `internal/app`.

**Concrete failure scenario:** an entry order attempt is saved as `OrderIntent`, the broker call then returns `ErrOrderIndeterminate` (network timeout after Angel accepted the order — this is exactly R1's CR-7 scenario), and the attempt is marked `OrderIndeterminate` in the `order_attempts` table (engine.go:169-171). The process crashes or is restarted before the position ever gets reconciled by an operator. On restart, `Reconcile` only compares the **positions** table against the broker's live book — if the indeterminate order in fact did *not* fill (a real, common outcome for a timed-out-but-rejected order), there is nothing in the broker's book and nothing in the positions table either, so `Reconcile` reports **clean** and the engine starts trading normally — while the `order_attempts` table still silently holds an unresolved `indeterminate` row nobody will ever look at unless an operator manually queries the SQLite file. The mechanism that exists specifically to close this gap (poll the broker's actual order-status endpoint for every unresolved attempt before declaring the startup safe) is fully built, unit-tested in isolation, and never wired in.

**Severity: HIGH.** This is a real gap in exactly the "reconciliation logic" category the audit asked about: a mismatch category (unresolved order attempts, as opposed to position-book mismatches) that is neither surfaced nor acted on at startup, even though the store was explicitly built to support doing so.

---

### R3-3 (HIGH). An indeterminate entry order that actually filled gets zero stop-loss coverage for the rest of the run

**File:** `go-engine/internal/engine/runner.go` — `placeSingleLeg` (726-759, the relevant branch at 740-746) and `enterMultiLeg` (765-874, relevant branch at 841-850); `softStopLossCheck` (988-1025); cross-checked against `internal/engine/engine.go`'s `PlaceEntryOrder` (124-159) and `internal/risk/risk.go`'s `OpenPosition`/`GetOpenPositions` (550-554, 768-778).

Traced end-to-end:

1. `PlaceEntryOrder` calls `riskManager.OpenPosition` (or `OpenPositionWithMargin`) **before** calling `broker.PlaceOrder` (engine.go:152-159) — so the risk manager's in-memory `OpenPositions` map already has this symbol regardless of what the broker call returns.
2. If `broker.PlaceOrder` returns `ErrOrderIndeterminate`, engine.go's comment says "do NOT roll back risk state... NOT rolling back risk state, marked for reconciliation" (168) — confirmed: no rollback call on this branch, and (per R3-1) `SavePosition` is also never called on this branch (only on the success branch, 199-211), so the position exists in the risk manager's memory but nowhere durable.
3. Back in `runner.go`, both call sites treat this exactly the same way — fire an alert (if configured) and `return`, **without adding the leg to `r.open`**:
   ```go
   // runner.go:740-746 (single-leg path)
   res, err := r.te.PlaceEntryOrder(...)
   if err != nil {
       if errors.Is(err, broker.ErrOrderIndeterminate) && r.cfg.Alert != nil {
           r.cfg.Alert("indeterminate_order", tradeSymbol)
       }
       return   // r.open[underlying] is never set
   }
   ```
   The multi-leg path (841-850) is the same, plus it unwinds the *other* already-filled legs (correctly, since those are known-good) but explicitly does not touch the indeterminate leg itself ("NOT unwinding this leg (fill state unknown)... structure needs MANUAL reconciliation").
4. `softStopLossCheck` — the per-tick software stop-loss loop — only iterates `r.open` (runner.go:993-1024). Since this leg was never added to `r.open`, **it is never checked for a stop-loss trigger, ever, for the remainder of the process's uptime.**

**What does eventually catch it:** `flattenAll` (runner.go:934-954) is called on graceful shutdown, a whole-session risk-manager drawdown breach, and intraday square-off — and its second loop iterates `riskManager.GetOpenPositions()` directly (946), which **does** include this position (per step 1). So the position is not permanently invisible — but it is unprotected by any leg-level stop-loss between the moment the order goes indeterminate and whichever of those three broader triggers fires first (which, for a SELL/short option leg on a bad trend day, can be a very expensive gap).

**Concrete failure scenario:** a `short_straddle` SELL leg's entry order times out mid-session → `ErrOrderIndeterminate` → alert fires once → the order in fact filled at the broker (the ambiguous case this mechanism exists for) → the short option position now moves against the account for the rest of the day with no per-tick stop-loss watching it, only the whole-session max-drawdown check (which reacts to aggregate P&L, not this leg specifically, and could be far too late for one large naked short) or 15:15 square-off.

**Severity: HIGH.** This is R1's CR-7/CR-14 pattern ("unmonitored option position, no stop-loss") surviving for exactly the one case (`ErrOrderIndeterminate`) that the rest of the R1/R2 remediation was built around handling safely.

---

## 3. Medium Findings

### R3-4 (MEDIUM). API server starts before `RestoreState()` runs — narrow kill-during-startup race

**File:** `go-engine/cmd/main.go:382-425`; `go-engine/internal/engine/runner.go:412-413` (`Run`'s first statement), `244-310` (`RestoreState`).

`cmd/main.go` wires the control API and starts it in a goroutine (`go func() { apiServer.Start() }()`, line 411-415) **before** setting up the shutdown context and calling `runner.Run(ctx)` (417-425). `Run`'s very first line is `r.RestoreState()`, which is what copies recovered positions from the state DB into `riskManager.OpenPositions` and rebuilds `r.open` (runner.go:244-310) — before this call, both maps are empty (a fresh `risk.NewManager(...)` was just constructed at cmd/main.go:340-349).

If an operator (or an automated script) hits `/api/kill` in the window between the API goroutine actually starting to accept connections and `RestoreState()` completing, `KillAndFlatten` → `flattenAll` (runner.go:374-382, 934-954) iterates an `r.open` that is still empty and a `riskManager.GetOpenPositions()` that is also still empty — it would report success ("flattened") having actually closed nothing, while positions recovered from a prior session sit un-flattened.

**Severity: MEDIUM**, not higher, because the window is genuinely narrow (goroutine dispatch + HTTP listener bind vs. the very next statement in `main()`), and the kill-switch's failure mode here is "did nothing," not "did the wrong thing" — but for a control that exists specifically as the emergency stop, "silently a no-op right after startup" is worth closing (e.g., call `RestoreState()` before starting the API server, or gate the control endpoints until restore completes).

### R3-5 (MEDIUM). `cmd/backtest` silently defaults lot size to a hardcoded NIFTY value for any symbol

**File:** `go-engine/cmd/backtest/main.go:141` (flag default), `236-241` (resolution).

```go
lotSize := flag.Int("lotsize", 75, "contract lot size (ST-10/M3: NIFTY default post-Apr-2025...)")
...
cfg.LotSize = *lotSize
if ls, ok := lotSizeFromInstrumentCache(*instrumentCacheDir, *symbol); ok {
    log.Printf("lot size for %s resolved from instrument cache...: %d (overrides -lotsize=%d)", *symbol, *instrumentCacheDir, ls, *lotSize)
    cfg.LotSize = ls
}
```

If `lotSizeFromInstrumentCache` fails to find an override (no cache directory present, or the symbol isn't in the cached scripmaster JSON — both silent, non-error returns per that function's own doc comment "never a guessed lot size... callers fall back to `-lotsize`"), the code falls through to `cfg.LotSize = 75` **with no warning printed at all** on the failure path (only the success path logs). Running `-symbol BANKNIFTY` (or any non-NIFTY underlying) without first populating `data/instruments/` silently backtests using NIFTY's lot size. Since every P&L, charge, and margin figure in the report scales with lot size, this produces a materially wrong report used to make real go/no-go capital decisions — the same "silent default on a path that affects money" pattern as the fixed ATR bug, one layer removed (tooling that informs live decisions, rather than the live path itself).

Compounding this: **`cmd/backtest` has zero `*_test.go` file** (confirmed — `cmd/backtest/` contains only `main.go`), unlike its siblings `cmd/fetchdata` (`main_test.go`, 5 test functions) and `cmd/watchdog` (`main_test.go`, 5 test functions). None of `parseParams`, `lotSizeFromInstrumentCache`, `filterRange`, or `loadRealIVIntoConfig` have any unit coverage at the CLI layer (the underlying `internal/backtest` package is well tested per R2-3, but this file's own logic is not), so this exact bug class would ship unnoticed.

**Fix suggestion:** log a `WARNING` on the fallback path (mirroring the pattern already used elsewhere in this same file, e.g. `-params` unsupported at line 228), and/or refuse to run for a non-NIFTY `-symbol` without either an instrument-cache hit or an explicit `-lotsize`.

### R3-6 (MEDIUM). Watchdog alerts/restarts exactly once per stale episode, then falls silent for the rest of the outage

**File:** `go-engine/cmd/watchdog/main.go` — `check()` (81-116), `maybeRestart()` (132-164); confirmed intentional and tested by `TestWatchdogRestartCmdCooldown` (`main_test.go:88-135`).

The state machine is: `stale && !alerted` → alert once + attempt restart-cmd once, set `alerted=true`; `stale && alerted` → log a quiet one-liner only, **no further alert, no further restart attempt**, for as long as the episode continues; `alerted` only resets back to `false` on recovery (`!stale`). `maybeRestart` is only ever invoked from the first branch, so even once `restartCooldown` has fully elapsed, a *continuing* (never-recovered) episode gets no second restart attempt — the cooldown only affects the next *separate* episode.

**Concrete failure scenario:** the engine hard-crashes with open positions. The watchdog fires exactly one Telegram alert and (if configured) one restart attempt. If the operator's phone is asleep/off/the message is missed, or the restart-cmd itself silently fails to actually revive the engine (e.g., a bad restart script, or the underlying crash cause — corrupted state DB, exhausted disk — reproduces immediately and the new process dies before its first heartbeat), **there is no second alert and no second restart attempt for the remainder of the outage**, no matter how long it lasts. This is precisely the "unattended crash with open positions goes unnoticed" scenario (R1 CR-15) this binary exists to close, and it is only partially closed: the *first* occurrence is covered, a *sustained* one is not.

**Severity: MEDIUM** — the design is deliberate and documented (avoids restart-looping on a single bad episode), and one alert is a real improvement over R1's "nothing watches the heartbeat at all." But for a money-critical dead-man's switch, a periodic re-nag (e.g., "still stale after N minutes") while the episode continues would materially reduce the chance of a missed single message leaving positions unattended for hours.

---

## 4. Low / Informational Findings

### R3-7 (LOW). `strToTime` silently returns the zero time on a parse error

**File:** `go-engine/internal/state/store.go:167-176`:
```go
func strToTime(s string) time.Time {
    if s == "" { return time.Time{} }
    t, err := time.Parse(time.RFC3339Nano, s)
    if err != nil { return time.Time{} }   // parse failure silently becomes "no time"
    return t
}
```
Used throughout `scanPosition`, `GetOrderAttempt`, `ListUnresolvedOrderAttempts` to decode `entry_time`/`exit_time`/`created_at`/`resolved_at`. Under the package's own normal write path this can't currently be hit — every write goes through `timeToStr` (well-formed RFC3339Nano) inside an ACID SQLite transaction, so a row's timestamp column is never partially written. It's flagged here purely because it is exactly the "silently substitute a default instead of erroring" shape the audit is watching for, and it *would* matter if the DB were ever hand-edited, migrated, or corrupted by something outside this package's control (e.g., an operator running `sqlite3 data/titan_state.db` directly during an incident) — a corrupted timestamp would silently read back as the zero value rather than surfacing a "state DB row is malformed" error. Low severity because it requires an out-of-band write to trigger; no fix required unless this pattern gets reused somewhere writes aren't transactional.

### R3-8 (LOW). `cmd/backtest` has no type-level read-only guard and falls back to YAML credentials

**File:** `go-engine/cmd/backtest/main.go:47-58` (`loadAngelConfig`), `186-203` (`fetch()`).

`cmd/backtest`'s `fetch()` closure resolves credentials as `envOr("ANGEL_CLIENT_CODE", cfg.Brokers.Angel.ClientCode)` — env var first, **but falling back to `config.yaml`'s value if the env var is unset**, with no refusal. This is the opposite priority/strictness of `cmd/fetchdata`'s gate (`resolveCreds`, which refuses outright if `config.yaml` carries *any* non-empty credential field, even with env vars also present — see `docs/reports/R2-3-REPORT.md` §1 "Credential gate"). Given `docs/PRODUCTION_GAPS_R2.md`'s H-1 states `config.yaml` still contains the original burned real credentials as of this writing, running `cmd/backtest` without a local CSV cache and without env vars set will silently authenticate against Angel One using those live credentials.

In practice this is low-risk today because `angel.Connect()` is only ever followed by `angel.FetchHistory(...)` in this file — no order-placement call exists in `cmd/backtest/main.go`. But unlike `cmd/fetchdata`, which deliberately narrows its broker seam to a `historyFetcher` interface with no `PlaceOrder` method (a compile-time guarantee, documented as a "hard constraint" in that file's package doc), `cmd/backtest` holds the full concrete `*broker.AngelBroker` (every method reachable) with nothing but the current absence of a call site preventing misuse. A future change to this file that adds any order-related helper would have no compiler-enforced guard against it also having live credentials in scope. Recommend either applying the same interface-narrowing pattern here, or at minimum matching `fetchdata`'s stricter env-only gate.

### R3-9 (INFO). CSV logger vs SQLite ledger — independent, unsynchronized, both fail silently (by design)

**File:** `go-engine/internal/logger/csv_logger.go` (package doc, lines 1-14) vs `go-engine/internal/ledger/ledger.go` (package doc, lines 1-21); call sites in `engine.go` (216-220, 311-315).

Both files' own doc comments already correctly declare the SQLite ledger authoritative and the CSV a "human/dashboard-friendly mirror" / "secondary log." Confirmed in the caller: `csvLogger.LogTradeWithStatus(...)` and `e.ledgerAppend(...)` are both called independently for the same event, and both swallow their own errors (`log.Printf` only) without any cross-check. This is intentional and documented, not a new bug — restated here only because R3-1 means the *ledger* write itself can also silently fail, at which point **neither** record of a real trade may exist, and an operator who trusts the CSV (e.g. via the dashboard, R2's G-10) would have no way to know the SQLite ledger — the actual system of record — is missing that row too.

### R3-10 (INFO, no action needed). `fetchdata`'s resume checkpoint correctly avoids the "marked done prematurely" bug class

**File:** `go-engine/cmd/fetchdata/main.go` — `runFetch` (291-336).

The prompt specifically asked to check for another instance of a known past checkpoint bug ("marks something 'done' when it wasn't actually"). Traced end-to-end: `cp.Done[tgt.Key] = true` (328) is only reached *after* `backtest.SaveCandlesCSV` has already returned successfully (324-326), and a zero-candle fetch is explicitly treated as an error rather than success (312-321, with a comment specifically calling out why: "Do NOT mark this target done: that would make -resume permanently skip it on every future run, leaving an empty CSV mistaken for a completed fetch"). If the process is killed between marking done in memory and `saveCheckpoint` persisting it (329-331), the in-memory mark is lost with the process, so a resume simply re-fetches that target — safe (redundant work, not data loss/silent incompleteness). This is a clean pass; no finding, listed for completeness since the prompt called this bug class out by name.

---

## 5. What Was Explicitly Verified as Correct (so it isn't re-litigated later)

- `dbutil.OpenSQLite`: WAL mode + `synchronous=FULL` confirmed via `PRAGMA` query in `dbutil_test.go` (not just asserted) — `synchronous=FULL` in WAL mode fsyncs the WAL file on every commit, not only at checkpoint, so a committed write really does survive a subsequent OS-level crash, not just a process-level one.
- `ledger.Append`: genuinely insert-only — no `UPDATE`/`DELETE` statement exists anywhere in `ledger.go`; a status transition is a new row sharing the same `ClientOrderID`, exactly as documented. `TestCrashRestartPreservesRows` proves rows survive a close/reopen cycle against the same file.
- `state.Reconcile`: the pure classification function itself (Matched/Phantom/Orphan/QuantityMismatch, net-quantity signed comparison, multi-record-per-symbol summing) is correct and thoroughly tested (`TestReconcile`'s 9 subtests including side-flip-is-a-mismatch and net-summing cases). The bug is not in this function — it's that one of its three siblings (unresolved order attempts) never gets fed into the equivalent startup gate (R3-2).
- `Runner.Shutdown` (`internal/engine/runner.go:1170-1211`): retries `flattenAll` up to 3 times with escalating backoff, verifies the broker's actual position book is empty (not just its own bookkeeping) before declaring success, and returns a non-nil error that `cmd/main.go` correctly turns into `os.Exit(1)` (not 0) on failure — this is a real, working "guarantee a flat book or fail loudly" shutdown path, not a fig leaf.
