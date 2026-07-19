# TitanAlgo — Round 2: Remaining Gaps to Production (Parallel Work Plan)

**Date:** 2026-07-19
**Prerequisite reading:** `docs/PRODUCTION_READINESS_AUDIT.md` (Round 1 audit), `docs/REMEDIATION_PLAN.md` (Round 1 plan), `docs/reports/WP-1..11-REPORT.md` (what was actually built), `docs/validation/RESULTS.md` (validation outcome).
**Status after Round 1:** all 11 work packages complete; whole module builds/vets/tests clean with `-race`; docs match reality. The system is now *structurally* sound but **not yet production-ready**, for the specific reasons below.

---

## 1. Executive Summary — What Still Blocks Production

Two kinds of gap remain:

**A. Engineering gaps** — features that are fail-closed stubs, unverified paths, or known bugs. These are enumerated as work packages R2-1 … R2-7 below, designed for parallel assignment with disjoint file ownership (same discipline as Round 1).

**B. The strategy gap** — the biggest one, and it is not a coding task: **zero of the 7 strategies pass the go-live gates** (profit factor > 1.3 OOS, max drawdown < 15% of margin, positive expectancy at 2x costs) in walk-forward validation (`docs/validation/RESULTS.md`). A production-grade *system* running a losing strategy just loses money reliably. R2-3 (real data) and R2-7 (strategy R&D) address this; no live deployment should happen before at least one strategy passes on **real** data.

**C. Human/process gaps** — things no agent can do (§5). Two of these have been flagged since the original audit and are STILL outstanding as of today:
- ❗ **`go-engine/config.yaml` still contains the original compromised Angel One credentials** (client code, PIN, API key, TOTP secret — verified present 2026-07-19). These were declared burned in the Round 1 audit. Rotation has been requested three times. Until rotated, the account must be considered takeover-able by anyone who ever saw this file (it was OneDrive-synced for months).
- ❗ **The project still lives under OneDrive** (`...\OneDrive\Desktop\development\...`). Sync locking/corruption risk for the state DB, ledger DB, and logs — worse now that SQLite WAL files are being written continuously.

---

## 2. Gap Inventory (detailed)

Each gap: what it is, evidence, impact, and which work package owns it.

### G-1. Broker margin API not implemented → all short-option strategies unusable
- **Evidence:** WP-9-REPORT ("margin API (A-6) still unimplemented so SELL-derivative entries stay fail-closed").
- **Detail:** `risk.ValidateOrderWithMargin` (WP-2) requires the caller to supply real SPAN+exposure margin for SELL derivative orders and rejects when it is unknown (fail-closed, correct). But nothing fetches margin: Angel's margin-calculator endpoint (Appendix A-6 in REMEDIATION_PLAN: `POST /rest/secure/angelbroking/margin/v1/batch`) was never implemented in `internal/broker`. Net effect: `short_straddle`, `iron_fly`, `nine_twenty` (all SELL-leg strategies) can never open a position in live mode.
- **Impact:** 3 of 7 strategies dead in live mode; safe but non-functional.
- **Owner:** R2-1 (broker) + R2-INT (wiring).

### G-2. Sniper strategy generates zero trades — real integration bug
- **Evidence:** WP-10-REPORT (0 trades in all 24 walk-forward windows, root cause traced).
- **Detail:** `internal/engine/runner.go` always populates `EvalContext.Prices` when calling `Evaluate`, so sniper's candle-mode path never activates; its fallback tick-aggregation path builds candles from near-identical consecutive ticks → zero-range doji candles that its own pattern detectors (hammer/engulfing in `candlestick.go`) can never match. Same defect affects the backtest path via `internal/backtest/engine.go` feeding it.
- **Impact:** 1 of 7 strategies silently non-functional in both live and backtest.
- **Owner:** R2-2. (A spawned task chip "Fix sniper strategy: 0 trades in all backtests" already exists for this — assigning R2-2 supersedes it.)

### G-3. No real historical market data — all validation is synthetic
- **Evidence:** WP-10-REPORT caveat 1; `docs/validation/RESULTS.md` disclaimers.
- **Detail:** No cached real NIFTY candles exist in-repo, and validation could not fetch any without credentials. Every number in RESULTS.md is a methodology demonstration on synthetic data. **No strategy decision (go/no-go, parameter choice) can be made from it.** The backtest cache format and `-csv` flag exist (WP-7); what's missing is a fetch-and-cache tool that pulls 2–3 years of real 5-min NIFTY/BANKNIFTY candles (and ideally option-chain candles) via the historical endpoint (A-9) once credentials are available, plus re-running the walk-forward harness against it.
- **Impact:** Blocks any evidence-based go-live decision. Root blocker for the strategy gap.
- **Owner:** R2-3 (tooling) + H-4 (human supplies rotated credentials for the one-time fetch).

### G-4. Constant-IV Black-Scholes — vega risk invisible in backtests
- **Evidence:** WP-7-REPORT (documented v1 limitation); WP-10-REPORT caveat 2 (short_straddle PF flips 0.29→1.66 purely on assumed IV).
- **Detail:** The backtest reprices options with a single constant IV per run. Real IV varies by strike/expiry/time and spikes exactly when short-option strategies are losing. Any short-vol strategy result is unreliable until IV is modeled — either from real historical option prices (preferred: implies IV directly, comes free with G-3's option-candle fetch) or an IV time-series input.
- **Impact:** Backtest results for 3 of 7 strategies structurally untrustworthy even on real underlying data.
- **Owner:** R2-3.

### G-5. Backtest CLI cannot sweep strategy parameters
- **Evidence:** WP-10-REPORT blocker 4.
- **Detail:** Strategy-internal parameters (RSI thresholds, EMA periods, wing width, premium-stop multiplier) are compile-time defaults in `internal/strategy`; the backtest CLI has no way to vary them, so parameter-sensitivity/robustness testing (the core of walk-forward) is impossible without recompiling. Also: lot size is a CLI flag, not read from the instrument master (WP-7 noted this as a follow-up); and there is no cost-multiplier flag (WP-10 computed the 2x-cost stress arithmetically).
- **Impact:** Overfitting undetectable; validation harness hobbled.
- **Owner:** R2-3 (CLI/backtest side) with a small strategy-package hook (constructor-with-params or options struct) in R2-2's files — coordinate: R2-2 owns `internal/strategy`, R2-3 consumes the new constructor.

### G-6. Paper-trading fill model is still fantasy
- **Evidence:** Round 1 audit EX-9 / §7 (paper vs live parity) — **never assigned to any Round 1 work package** (WP-1 owned only `angel_broker.go`/`broker.go`/`instruments.go`; `mock.go` and `live_paper_broker.go` were nobody's).
- **Detail:** `internal/broker/mock.go` fills at LTP with uniform 0.05–0.1% slippage (real NFO spreads: 0.5–5%), always complete/instant/any-size, and **SELL orders credit full turnover with no margin — shorting mints money in paper mode**. `live_paper_broker.go` inherits this. The 1-month paper-trading gate (LIVE_GATE_CHECKLIST) is meaningless while paper fills systematically flatter every strategy.
- **Impact:** Paper results will overstate live results; the paper gate can pass a losing strategy.
- **Owner:** R2-4.

### G-7. Market data is 5-second REST polling, no live feed
- **Evidence:** Round 1 audit 5.7/EX; `feed.go` stub deleted in cleanup; nothing replaced it.
- **Detail:** Prices come from REST quote polling on a 5s cache. Angel One SmartAPI provides a WebSocket streaming feed (SmartAPI WebSocket 2.0) that would cut price latency from ~0–7s to sub-second — this directly tightens stop-loss reaction time (the software SL loop can only be as fast as its price source) and removes rate-limit pressure from polling. Not required for correctness (staleness guards exist), but required for the stated goal of a serious intraday options system.
- **Impact:** Wide worst-case SL latency; polling load; no tick-level volume (degrades VWAP quality — WP-6 documented its tick-mode as "degraded").
- **Owner:** R2-1.

### G-8. No standalone watchdog process
- **Evidence:** WP-9-REPORT honest gaps; RUNBOOK section (f) says heartbeat monitoring is currently "a manual log-check procedure".
- **Detail:** The engine touches `data/heartbeat` each tick and fires Telegram alerts on defined events (if `TITAN_TG_TOKEN`/`TITAN_TG_CHAT` set), but nothing watches the heartbeat from *outside* the process. If the engine hard-crashes or the host reboots with open positions, no alert fires — the exact dead-man scenario the audit called CRITICAL (CR-15).
- **Impact:** Unattended crash with open positions goes unnoticed.
- **Owner:** R2-5.

### G-9. API server has no rate limiting
- **Evidence:** WP-11-REPORT (marked "not implemented" in the corrected mobile design doc).
- **Detail:** Auth exists (token), but an attacker/buggy client on the (localhost-default) API can hammer endpoints without throttle. Low severity while bind is localhost; becomes real if TLS+LAN exposure is enabled.
- **Owner:** R2-5.

### G-10. Dashboard CSV parser vs new 12-column format
- **Evidence:** WP-5-REPORT flag ("old files are not forward-compatible with a fixed-column-index parser… make it header-driven"); WP-8 parameterized paths/balance but the parser itself was not confirmed header-driven.
- **Detail:** `py-brain/dashboard/app.py` reads the trade CSV; the format grew from 10 to 12 columns (OrderID, Status). If the parser is positional it now misreads or crashes; also open-position inference by counting BUY/SELL rows was flagged in Round 1 and never reworked (the dashboard should read the state DB or engine API instead).
- **Impact:** Operator dashboard shows wrong data — dangerous during a live session.
- **Owner:** R2-5.

### G-11. End-to-end paper run against real Angel endpoints never performed
- **Evidence:** WP-9-REPORT honest gaps ("a true interactive end-to-end run of cmd/main.go -paper wasn't performed" — correctly refused because config.yaml holds live credentials).
- **Detail:** Everything is proven against MockBroker + httptest fakes. Real-network behaviors — TOTP login, token refresh at Angel's actual expiry cadence, instrument-master download, WAF behavior, real order-book polling latency — have zero live verification on the post-remediation code.
- **Impact:** Unknown-unknowns in the broker integration surface exactly once real money is near.
- **Owner:** R2-INT (agent-driven checks) + H-5 (human supervises the session after credential rotation).

### G-12. NSE holiday calendar is a fixed table
- **Evidence:** WP-9-REPORT gaps.
- **Detail:** Hardcoded 2026 dates; goes stale next year; no source-of-truth refresh. Acceptable short-term; needs an update procedure documented at minimum.
- **Owner:** R2-5 (small: config-file-driven list + RUNBOOK entry).

### G-13. Internal duplication/cleanups (from over-engineering scan)
- **Evidence:** Ponytail audit in-session (2026-07-19).
- **Detail:** (a) duplicated SQLite open/pragma boilerplate in `internal/state` + `internal/ledger`; (b) two SQLite DB files where one DB with two tables would do; (c) legacy `Manager.KillSwitch bool` alongside the atomic (`risk.go:312-393`) with OR-merge semantics; (d) two origin-allowlist knobs (`allowed_origins` + `cors_allowed_origins`) for one question; (e) `TLSCertFile`/`TLSKeyFile` config + `SetTLS` exist but `SetTLS` is never called — dead knob; (f) HistorySize/MinDataPoints defaults duplicated in config.go and runner.go.
- **Impact:** Maintenance drag and one genuinely misleading knob (TLS). Not a launch blocker except (e), which either gets wired or deleted.
- **Owner:** R2-6 (optional wave; TLS wiring specifically goes to R2-INT since it touches `cmd/main.go`/`titan.go`).

### G-14. Mobile app not verified against the hardened API
- **Evidence:** WP-4 changed auth (token header/query param, Origin checks, CORS removal); `mobile-app/` client code predates that; WP-11 marked the mobile client "not implemented" against the current design.
- **Detail:** The Android app's JS (`www/app.js`) presumably still sends the old header/no token and connects to `/ws/live` without `?token=`. It will fail auth against the new server. Either update the client or explicitly park it.
- **Impact:** Mobile monitoring/kill-switch surface currently broken.
- **Owner:** R2-5 (assessment + minimal fix or explicit parking note).

### G-15. No git remote / off-site backup
- **Evidence:** `git remote -v` empty (verified 2026-07-19).
- **Detail:** The repo (and its audit trail) exists only on this one OneDrive-synced disk. CI workflow (WP-8) also never runs — it's GitHub Actions with no GitHub.
- **Impact:** Single-disk risk; CI dead.
- **Owner:** H-3 (human: create private remote, push).

---

## 3. Work Packages (parallel-assignable)

Same rules as Round 1 (`REMEDIATION_PLAN.md` §0 applies verbatim: exclusive file ownership, no invented APIs, fail-closed, `go build/vet/test -race` before done, report to `docs/reports/R2-<n>-REPORT.md`, never run `-live`, never touch `config.yaml`).

### Execution order

```
Wave A (parallel): R2-1, R2-2, R2-3, R2-4, R2-5      ← disjoint files
   └─► Wave B: R2-INT (single agent, integration + E2E paper vs real endpoints*)
          └─► Wave C (parallel): R2-7 (strategy R&D on real data), R2-6 (optional cleanups)
* R2-INT's real-endpoint phase requires H-1/H-2/H-4 (rotated creds, project moved, env vars) done first.
```

### File Ownership Matrix

| Package | Owned files (exclusive) |
|---|---|
| R2-1 | `internal/broker/angel_broker.go`, `internal/broker/broker.go`, `internal/broker/ws_feed.go` (new) |
| R2-2 | `internal/strategy/*` (all), `internal/backtest/engine.go` (EvalContext-construction section only — coordinate via report if more needed) |
| R2-3 | `cmd/backtest/main.go`, `internal/backtest/*` except engine.go, `cmd/fetchdata/main.go` (new), `docs/validation/*` |
| R2-4 | `internal/broker/mock.go`, `internal/broker/live_paper_broker.go` |
| R2-5 | `cmd/watchdog/main.go` (new), `internal/api/server.go`, `py-brain/dashboard/app.py`, `mobile-app/**` (assessment), holiday-list config file (new), `docs/RUNBOOK.md` (append only) |
| R2-INT | `cmd/main.go`, `internal/engine/*`, `internal/app/titan.go`, `internal/config/config.go` + wiring-only edits anywhere |
| R2-6 | `internal/state/*`, `internal/ledger/*`, `internal/risk/risk.go` (cleanup items only) — runs in Wave C to avoid conflicts |
| R2-7 | `docs/validation/*` (new analyses), new strategy files in `internal/strategy/` only if R2-2 has completed (else wait) |

---

### R2-1 — Broker: Margin API + WebSocket Market Feed
**Gaps:** G-1, G-7. **Files:** see matrix. **Deps allowed:** existing `gorilla/websocket`, `golang.org/x/time/rate`.

1. Implement `GetRequiredMargin(orders []MarginOrderInput) (float64, error)` on `AngelBroker` using Angel's margin batch endpoint (`POST /rest/secure/angelbroking/margin/v1/batch` — verify exact request/response shape against SmartAPI docs before coding; if the real shape differs from the plan's Appendix A-6, follow reality and document it). Add it to `ExtendedTradeService`. Fail-closed: any error/ambiguity → return error, never a guess. Support multi-leg baskets (hedged-margin benefit for iron_fly).
2. Implement SmartAPI WebSocket 2.0 market feed in `ws_feed.go`: connect/auth with the session token, subscribe LTP+volume for a symbol set, heartbeat per spec, exponential-backoff reconnect, and on each tick update the existing price cache **through the same staleness-timestamped path** WP-1 built (`GetCurrentPriceWithAge` must reflect WS ticks). REST polling remains as automatic fallback when WS is down (degrade, don't die). Expose `SubscribeLive(symbols []string) error` / `UnsubscribeLive`.
3. Unit tests with `httptest`/fake WS server: margin happy-path + error path, WS reconnect, fallback-to-polling on WS death, price-age reflects WS ticks.

**Acceptance:** build/vet/`go test -race ./internal/broker/...`; report documents the new `ExtendedTradeService` additions for R2-INT.

### R2-2 — Sniper Fix + Strategy Parameterization
**Gaps:** G-2, G-5 (strategy side). **Files:** see matrix.

1. Fix sniper (G-2): decide the correct layer. Preferred: make `EvalContext.Candles` the sniper contract — populate real rolling candles (the engine/backtest already have candle data) and have sniper use them directly; keep tick-aggregation only as an explicit degraded mode that builds *real-range* candles (min/max over the window, not consecutive-tick dojis). If the fix requires `internal/backtest/engine.go`'s EvalContext-construction to also pass Candles, you own that section. If it requires `internal/engine/runner.go` changes, do NOT edit it — specify the exact required change in your report for R2-INT.
2. Regression test: sniper emits ≥1 signal on a synthetic candle series containing a textbook hammer + engulfing sequence; zero signals on a flat series.
3. Parameterization (G-5): add an options-struct constructor per strategy (e.g., `NewRSIReversal(RSIReversalParams{...})`, zero-value = current defaults) and a `registry.GetWithParams(name string, params map[string]float64)` (or equivalent typed mechanism) so the backtest CLI can vary parameters without recompiling. Keep `registry.Get` working unchanged.
4. All existing strategy tests keep passing; add param-plumbing tests.

**Acceptance:** build/vet/`go test -race ./internal/strategy/... ./internal/backtest/...`; report lists exact new constructor signatures for R2-3 and any runner change requests for R2-INT.

### R2-3 — Real Data, IV, and Backtest Harness Upgrades
**Gaps:** G-3, G-4, G-5 (CLI side). **Files:** see matrix. **Depends on:** R2-2's param constructors (code against its report; stub locally if racing ahead).

1. `cmd/fetchdata` (new): CLI that logs in (credentials via env vars only — refuses to run if creds resolve from YAML), pulls N years of 5-min candles for configured underlyings via the historical endpoint (A-9) with polite rate-limiting and resume-on-interrupt, and writes the WP-7 cache CSV format. Extend to option-chain candles for chosen strikes/expiries where the endpoint allows. THIS TOOL ONLY READS DATA — it must never place orders; assert the code path cannot reach PlaceOrder.
2. IV modeling (G-4): given cached option candles, back out implied vol per bar (invert Black-Scholes — `internal/backtest/bs.go` already has the pricer; add a bisection/Newton inverter) and feed a per-bar IV series into leg repricing instead of the constant. Where option data is absent, keep constant-IV mode but print a loud caveat banner in the report output.
3. CLI flags (G-5): `-params key=val,...` passed through to R2-2's parameterized constructors; `-cost-multiplier` (default 1.0); lot size from the instrument master when a fetched instrument cache is present (fallback to `-lotsize`).
4. Update `docs/validation/run_walkforward.py` to sweep parameter grids via the new flags and rerun everything automatically once real data lands (H-4). Do not fetch with real creds yourself — build and test the tool against a fake/httptest server; the actual fetch is executed under H-4 supervision.

**Acceptance:** build/vet/tests; fetchdata proven against a fake server incl. resume; IV inverter golden-tested (price→IV→price round trip); report documents exact usage commands for the human-supervised real fetch.

### R2-4 — Honest Paper Fills
**Gap:** G-6. **Files:** `internal/broker/mock.go`, `internal/broker/live_paper_broker.go`.

1. Spread model: fills at LTP ± configurable half-spread (default 0.3% of premium for options, 0.02% equity), buys worse / sells worse respectively.
2. Slippage scaled by order size; occasional partial fills (deterministic seed option for tests) surfacing through the same `FilledOrder.RequestedQty`/partial semantics WP-1 defined — paper mode must exercise the SAME partial-fill handling code paths as live.
3. Margin on shorts: SELL derivative orders in paper mode must consume a margin estimate (simple SPAN approximation: e.g., premium + X% of notional, configurable), not credit turnover. Kill the money-printing bug.
4. Occasional rejections (configurable rate, default small) so multi-leg unwind paths get exercised in paper.
5. Tests for all four behaviors.

**Acceptance:** build/vet/`go test -race ./internal/broker/...` (coordinate file-level: R2-1 owns angel_broker/broker/ws_feed; you own mock/live_paper — no shared files, both may add tests in separate `_test.go` files).

### R2-5 — Ops: Watchdog, Rate Limit, Dashboard, Holidays, Mobile Assessment
**Gaps:** G-8, G-9, G-10, G-12, G-14. **Files:** see matrix.

1. `cmd/watchdog` (new): tiny standalone binary — reads heartbeat file path + max-age + Telegram env vars; alerts once per breach episode when heartbeat is stale; optional `-restart-cmd` to attempt supervised restart. Must be runnable under Windows Task Scheduler; document the scheduling one-liner in RUNBOOK (append).
2. Rate limiting (G-9): token-bucket per client IP on the API server (stdlib or `x/time/rate`), config-driven rps/burst, 429 on breach; WS connection cap. Tests.
3. Dashboard (G-10): make CSV parsing header-driven (tolerate 10-col legacy and 12-col current); prefer reading open positions from the state DB (`data/titan_state.db`, read-only SQLite) instead of BUY/SELL row counting — if that's too large a change, header-driven parsing is the minimum and note the rest.
4. Holidays (G-12): move the fixed table to a config/data file (e.g., `go-engine/data/nse_holidays.yaml` — but note `data/` is gitignored; put it at `go-engine/nse_holidays.yaml` instead) with a RUNBOOK procedure for annual update. Coordinate the load-site change via report → R2-INT (you don't own the loader's file).
5. Mobile (G-14): read `mobile-app/` client code; determine exactly what breaks against the WP-4 API (auth header, WS token, endpoints); apply the minimal client fix if it's small (token input field + `?token=` on WS), else write `mobile-app/COMPAT.md` documenting precisely what's broken and park it. Do not redesign the app.

**Acceptance:** build/vet/tests for Go parts; dashboard runs against both CSV formats; report per item.

### R2-INT — Wave B Integration (single agent)
**Gaps:** G-1/G-2/G-5 wiring, G-11, G-13(e). **Files:** see matrix. **Starts after all Wave A reports.**

1. Wire margin: runner's SELL-entry path calls R2-1's `GetRequiredMargin` and passes the result to `ValidateOrderWithMargin`; margin-API failure → entry rejected + alert (fail-closed stays).
2. Wire WS feed: runner subscribes symbols on startup/discovery; verify staleness guards and the software SL loop actually benefit (price age should drop); REST fallback verified by killing the fake WS in a test.
3. Apply any runner-side changes R2-2's report requested (sniper candle feed); rerun one backtest window to confirm sniper trades.
4. Wire holiday file loading (R2-5) and TLS (`SetTLS` from config — G-13(e)) — or delete the TLS knob if the operator decision is localhost-only; decide once, document.
5. **E2E paper session against real Angel endpoints (G-11)** — ONLY after H-1/H-2/H-4 are confirmed done (rotated creds in env vars, project off OneDrive). Supervised: login, instrument download, WS feed live, paper orders through the full state/ledger path, token-refresh observed (run past a refresh boundary), Ctrl+C graceful shutdown, restart + reconcile. Capture logs into the report. If credentials are still not rotated, STOP and report blocked — do not run with the burned ones.
6. Full-module acceptance: `go build/vet/test -race ./...` green; updated smoke test covering margin + WS paths with fakes.

### R2-6 — Cleanups (Wave C, optional)
**Gap:** G-13 a,b,c,d,f. **Files:** see matrix. One `internal/dbutil` helper for SQLite open/pragma; merge the two DBs only if migration cost is trivial (else document and skip — ponytail: don't force it); delete legacy `KillSwitch bool` (fix the 4 call sites — coordinate with R2-INT who owns them, or run after); merge origin knobs; dedupe defaults. Nothing here blocks launch.

### R2-7 — Strategy R&D (Wave C, the long pole)
**Gap:** §1-B. **Files:** `docs/validation/*`; new strategies only via R2-2's param framework. **Depends on:** real data (R2-3 + H-4).

1. Rerun the full walk-forward harness on real data with parameter sweeps; produce `docs/validation/RESULTS_REAL.md`.
2. For any strategy near the gates, run robustness checks: parameter-neighborhood stability, regime breakdown (trend/range/high-IV months separately), Monte Carlo trade-order shuffles for drawdown confidence intervals.
3. Candidate improvements to test (hypotheses, not commitments): hedged-only short-vol (iron_fly with margin-aware sizing) vs naked; EMA-crossover with regime filter (only trade when ADX/realized-vol confirms trend); time-of-day filters for nine_twenty (skip event days).
4. Output: a go/no-go per strategy on real data against the LIVE_GATE_CHECKLIST gates. **If nothing passes, the honest recommendation is: do not go live; iterate.** That outcome is a valid deliverable.

---

## 4. Known Bugs Ledger (quick reference)

| # | Bug | Severity | Package |
|---|---|---|---|
| B-1 | Sniper: 0 trades ever (EvalContext.Prices always populated; doji tick-candles) | HIGH | R2-2 |
| B-2 | Paper shorts credit turnover, no margin — money printer | HIGH (paper only) | R2-4 |
| B-3 | Dashboard positional CSV parse vs 12-col format | MEDIUM | R2-5 |
| B-4 | Mobile client auth broken vs hardened API | MEDIUM | R2-5 |
| B-5 | `SetTLS` never called — dead config knob | LOW | R2-INT |
| B-6 | Dual kill-switch fields OR-merged | LOW | R2-6 |

---

## 5. Human Tasks (no agent can do these — several are BLOCKING)

- **H-1 (BLOCKING, overdue):** Rotate Angel One credentials — re-enroll TOTP, new API key, change PIN. The values in `config.yaml` today are the same ones declared burned in the Round 1 audit.
- **H-2 (BLOCKING, overdue):** Move the project out of OneDrive (e.g., `C:\dev\titan-algo`). SQLite WAL + OneDrive sync is a corruption risk for the ledger that is now the system of record.
- **H-3:** Create a private git remote (GitHub/GitLab), push, confirm CI runs green.
- **H-4:** After H-1: set the six env vars (`ANGEL_*`, `TITAN_API_TOKEN`), blank the credential fields in `config.yaml`, then run R2-3's `fetchdata` once (supervised) to populate the historical cache.
- **H-5:** Supervise R2-INT's real-endpoint paper session; then run the 1-month paper-trading gate per `docs/validation/LIVE_GATE_CHECKLIST.md`.
- **H-6:** Go-live decision: only if some strategy passes gates on real data (R2-7), and then only at 1 lot with a broker-side daily loss cap, per the checklist.

---

## 6. Definition of "Production Ready" (exit criteria for Round 2)

1. All Wave A + R2-INT packages merged; full module green under `-race`.
2. Real-endpoint paper session completed and reconciled cleanly (G-11 closed).
3. Watchdog running under the OS scheduler; alert test fired and received (G-8 closed).
4. Real historical data cached; walk-forward rerun on it (G-3 closed).
5. Credentials rotated + env-var-only + project off OneDrive + remote backup (H-1..H-3 closed).
6. ≥1 strategy passes all three gates on real data with robustness checks (R2-7) — **or** an explicit, documented decision to run paper-only until one does.
7. 1 month reconciled paper trading on the final build; then 1-lot live pilot per checklist.

Items 1–5 are engineering-completable. Items 6–7 are where "production ready" meets reality: the system can be ready before the strategy is. Do not conflate the two.
