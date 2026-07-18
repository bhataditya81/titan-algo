# TitanAlgo — Production Readiness & Trading Soundness Audit

**Date:** 2026-07-18
**Scope:** Full codebase — `go-engine` (broker, engine, risk, strategies, backtest, API), `py-brain`, `mobile-app`, Docker/deployment, documentation, process.
**Method:** Three independent specialist reviews (quantitative strategy audit, execution/risk-systems audit, infrastructure/security audit), consolidated and cross-verified.

---

## 1. Executive Summary

**Verdict: NOT production-ready. Do not run the live path (`-live`) with real money in its current state.**

TitanAlgo is a promising paper-trading prototype with a clean module layout, but it currently fails on every dimension that matters for a real-money trading system:

| Dimension | Status | Headline problem |
|---|---|---|
| Security | 🔴 FAIL | Live broker credentials (client code, PIN, API key, **TOTP secret**) committed in plaintext and synced to OneDrive. Account is compromised-by-design. |
| Risk management | 🔴 FAIL | Max-drawdown check is dead code (never called). Kill switch is boot-time-only. Stop-loss is software-only and dies with the process. |
| Order execution | 🔴 FAIL | Unconfirmed orders treated as filled (with a literal ₹1.0 fallback price). Partial fills ignored. Ambiguous timeouts orphan live positions. |
| State management | 🔴 FAIL | All position/risk state in-memory. A crash or restart orphans live option positions with zero protection. |
| Strategy validity | 🔴 FAIL | Backtest math guarantees short straddles "profit" (delta cancels, free theta, no gamma). Directional strategies produce zero backtest trades (dead code). Backtest ≠ live code path. |
| Cost model | 🔴 FAIL | STT/txn/stamp rates wrong for FY 2025-26 (options STT understated ~8x). Margin for short options confused with premium (~15x understatement). |
| Infrastructure | 🔴 FAIL | No git, no tests, no CI, docker-compose cannot build, remote "Stop" button does nothing, no alerting/dead-man switch. |
| Documentation | 🟠 POOR | README describes a system (TimescaleDB, Redis, gRPC, Arrow Flight, GPU/LSTM, Zerodha, "HFT") that largely does not exist. |

**The paper-trading path is reasonably safe to keep running.** The live path must be considered blocked until Phase 0 and Phase 1 of the remediation roadmap (§9) are complete.

**On "high success rating of trading":** as built, the system's expected live P&L is *negative by construction* — real charges are 3–5x what is modeled, fills are modeled with optimistic fantasy (no spread, no partial fills, no liquidity), and the only strategies the backtest "validates" are validated by broken math. Fixing the software is necessary but not sufficient; §8 covers what a statistically honest edge-validation process requires.

---

## 2. CRITICAL Findings (fix before anything else)

Consolidated from all three reviews. Each item lists file:line evidence.

### CR-1. Live broker credentials committed in plaintext — account takeover
`go-engine/config.yaml:6-11`
Contains real Angel One client code, login PIN, password/API-secret, API key, and **TOTP secret** (base32 seed). The TOTP seed lets anyone generate valid 2FA codes forever. Combined with client code + PIN this is complete, durable account takeover: login, place orders, square positions, drain the account via deliberate bad trades.

Amplifiers:
- Project lives under **OneDrive** — secrets replicate to Microsoft cloud and every synced device.
- `internal/app/titan.go:106-110` reuses the **broker API key as the mobile REST auth token**, so every phone/network hop learns a broker credential.
- `internal/api/server.go:120` logs `apiKey[:8]+"..."` — the Angel key is exactly 8 chars, so **the full broker key is printed to the log** (and this slice panics on shorter keys).
- `angel_broker.go:202` prints access-token prefix to stdout.
- `config.yaml:72` has a second, confusing empty `risk.totp_secret` field.

**Action (today):**
1. Regenerate TOTP enrollment, API key, and change the PIN at Angel One. Treat all five values as burned.
2. Move secrets to environment variables / Windows Credential Manager; keep `config.yaml` as a non-secret template.
3. Move the project out of OneDrive (e.g., `C:\dev\titan-algo`).
4. `git init` only after a `.gitignore` covering `config.yaml`, `*.exe`, `logs/`, `*.csv`.

### CR-2. Max drawdown is never enforced — `CheckRisk()` is dead code
`internal/risk/risk.go:463-483`
`CheckRisk()` (kill switch + balance depletion + max drawdown) has **zero call sites**. The advertised "Max drawdown protection (5%)" (`cmd/main.go:436`, README) does nothing at runtime. Losses continue until the balance can't fund the next entry — for small option premiums that is near-total loss of session capital.
**Fix:** Call `CheckRisk` every tick; on breach, halt entries and flatten all positions.

### CR-3. Kill switch is config-load-time only; no runtime trigger
`cmd/main.go:223`, `internal/app/titan.go:99`
`KillSwitch` is read once from YAML at boot (and defaults to `false`, `config.yaml:71`). No API endpoint, file watcher, or signal can flip it while running; the flag is an unsynchronized raw bool. During a malfunction the only stop is Ctrl+C on the console.
**Fix:** Guarded runtime kill endpoint/file trigger; atomic flag; on activation cancel open orders and flatten. Default `true` for live mode.

### CR-4. Remote "Stop" button is cosmetic
`internal/api/server.go:294-348`
`/api/start` and `/api/stop` only flip the API server's own `s.running` flag and log. Nothing is wired into the engine loop (`cmd/main.go:565`). The mobile app's emergency stop — the operator's remote kill switch — does not stop trading, and the engine's status push overwrites the flag.
**Fix:** Plumb a channel/callback from `Server` into the engine loop; stop must pause order placement and optionally square off.

### CR-5. Timed-out "pending" order treated as FILLED, with ₹1.0 fallback price
`internal/broker/angel_broker.go:751-787`
The fill poll runs 10 attempts (~37s). If the order is still pending, the code fabricates a `FilledOrder` at cached LTP — or literally **₹1.0** (`:784`) — updates positions (`:816`), and returns success. A later-cancelled order leaves a phantom position; "closing" it places a naked reverse order at the exchange. The ₹1 fallback corrupts every downstream P&L and balance figure.
**Fix:** Return a distinct `ErrOrderIndeterminate`; keep polling in background or cancel via the cancel API; never synthesize a fill price.

### CR-6. Partial fills completely ignored
`angel_broker.go:754-776, 803-816`
`fillQty` (`filledshares`) is fetched but never used — positions are updated with the *requested* quantity. Buy 75, fill 25 → book says 75; the eventual close sells 75 → **net short 50 naked options** (unbounded loss on a short call).
**Fix:** Use `fillQty` everywhere; treat `status=="complete" && fillQty<requested` remainder as open; handle `open`/`partially filled` statuses explicitly.

### CR-7. Ambiguous order errors orphan live positions; no reconciliation loop
`angel_broker.go:705-714` + `internal/engine/engine.go:64-71`
If the HTTP call times out *after* Angel accepted the order, `PlaceOrder` errors, the engine rolls back the risk position, and the real fill sits at the exchange untracked — no client-order-ID idempotency, no periodic broker-vs-internal position reconciliation to ever discover it. Unmonitored option position, no stop-loss, invisible until the broker app is opened.
**Fix:** On ambiguous errors, query the order book before rolling back; add a 30–60s reconciliation loop that alerts/flattens on mismatch.

### CR-8. All risk/position state is in-memory — restart orphans live positions
`internal/risk/risk.go:68-82`, `cmd/main.go:210-226, 352-366, 510, 571`
On crash/restart: `OpenPositions` empty, P&L zero, session balance reset. `AngelBroker.Connect` syncs broker positions into *its own* map, but nothing copies them into the risk manager, and the stop-loss loop iterates a *third*, also-empty local map. Any position open at crash time runs forever with no stop-loss, no drawdown accounting, no exit logic. **This is the single most likely way this system loses a full account.**
**Fix:** Persist risk state (SQLite/JSON WAL) on every mutation; on startup reconcile persisted ↔ broker ↔ strategy state; refuse to trade until reconciled or flattened.

### CR-9. Backtest option P&L model makes every short straddle mathematically guaranteed to profit
`cmd/backtest/main.go:150, 182-206`
Constant `Delta = 0.5` for all legs → short CE and short PE deltas cancel exactly for any spot move. Then `theta = duration * 0.5` points is *credited* per short leg (long legs pay no decay). Net: short straddle P&L = free theta − charges, strictly increasing with holding time. **Gamma — the entire risk of a short straddle — does not exist in the model.** A user could deploy a naked straddle believing it's a validated edge, then take unbounded losses on the first trending day.
**Fix:** Price legs from actual historical option candles (Angel provides them) or Black-Scholes with an IV series; full repricing per candle; realistic theta curve debiting long legs too.

### CR-10. Directional strategies never enter a position in the backtest — dead entry branch
`cmd/backtest/main.go:168-175, 234-263`
`shouldEnter` is only set inside `if len(signal.Legs) > 0`. Sniper, EMA crossover, RSI reversal, momentum return `Legs == nil` — so the single-leg entry branch is unreachable and these backtests produce **zero trades**. All directional backtest results are void; any tuning against them is fiction.
**Fix:** Set `shouldEnter` for leg-less Buy/Sell signals when flat; separate "exit" from "reverse" semantics (see ST-8).

### CR-11. Backtest and live trading do not share a code path
`cmd/backtest/main.go:162` vs `cmd/main.go:677`
Backtest evaluates real 5-min exchange candles; live evaluates a rolling buffer of polled LTP ticks. Sniper live builds pseudo-candles from **every 5 ticks** (`sniper.go:164` — "for demo speed"), so live EMA(9/21) is a completely different indicator than backtest EMA(9/21). Lot sizes differ (50 backtest / 25 live fallback / 75 actual NIFTY). SL disabled in backtest (`backtest/main.go:92`). Whatever the backtest says has no bearing on live behavior.
**Fix:** One execution/portfolio engine consumed by both a historical feed and a live feed; time-based (wall-clock 5-min IST) candle aggregation in both modes.

### CR-12. Partial multi-leg fills leave naked shorts; hedge legs placed LAST
`cmd/main.go:762-806`, `internal/strategy/iron_fly.go:39-46`
Legs are placed sequentially; a failed leg is simply skipped — no rollback, no alert. Iron fly declares short ATM legs *first* and protective wings *last*: a rejected wing (margin, freeze qty, RMS, illiquidity) leaves a **naked short straddle** while the system believes it holds a hedged fly.
**Fix:** Buy hedges first, sell only after hedges confirm (also cheaper under NSE hedged-margin rules); on any rejection, unwind filled legs immediately.

### CR-13. No margin model — short-option sizing uses premium, not SPAN
`internal/risk/risk.go:174-186`, `cmd/main.go:874-887, 960-988`, `angel_broker.go:661-665`
SELL orders are sized/validated by `premium × qty` against the session balance, while the broker product is `CARRYFORWARD` (NRML) — real margin for one short NIFTY lot is ~₹1–1.7L. The ₹10k "session balance" is fictional for `short_straddle`/`iron_fly`/`nine_twenty`: either broker RMS rejects (feeding CR-12) or, with a funded account, real exposure is 10–20x what the risk manager believes.
**Fix:** Integrate Angel's margin API pre-order; choose INTRADAY vs CARRYFORWARD deliberately; block short-option strategies until margin-aware.

### CR-14. Naked short strategies have no stop-loss at all
`internal/strategy/nine_twenty.go:59-68`, `short_straddle.go`, `sniper.go:13-16`
- `nine_twenty` sells an ATM straddle at 9:20 and holds to 15:15 with **no SL of any kind** — trend/gap days taken at full force.
- `short_straddle`'s only "exit" is spot-RSI leaving 45–55 — not a premium-based stop.
- Sniper's `StopLossPct`/`TargetPct`/`TrailingSL` fields are **write-only** (never referenced).
- Broker-side SL is never placed: `angel_broker.go:678-679` hardcodes `StopLoss:"0"`, variety `NORMAL`. Software SL runs in a 2s loop against a 5s-stale price cache (~7s+ worst-case detection), and dies with the process (CR-8).
**Fix:** Broker-side SL-M (`STOPLOSS_MARKET`) placed with every entry; combined-premium stop (e.g., exit at entry premium ×1.25–1.5) for straddles; make SL non-optional for short options; wire or delete Sniper's dead fields.

### CR-15. No dead-man switch, no alerting, single unsupervised process
Whole system. One Go process, launched via `go run` in an interactive PowerShell window, in a OneDrive folder, on a Windows desktop. No supervisor/auto-restart, nothing pages the operator when the engine crashes with open positions, the feed stalls, or drawdown breaches. `stop.ps1:6` force-kills (bypassing the graceful-shutdown handler) and its process filter almost never matches anyway.
**Fix:** Run a built binary under a supervisor (NSSM/Windows service or Linux systemd); watchdog with Telegram/SMS alerts on missed heartbeat; broker-side GTT stop orders as the safety net that survives engine death.

### CR-16. No version control, no tests, no CI
No `.git`, zero `*_test.go`, zero Python tests, no CI config. A live-money system with no record of what code was running when a trade fired, and unverified P&L/charge/stop math.
**Fix:** git init (after CR-1 step 4); unit tests for risk math, charge calc, order lifecycle; `go vet`, `go test -race`, `golangci-lint`, `pip-audit` in CI.

---

## 3. Strategy Layer Findings (beyond criticals)

### ST-1 (HIGH). RSI=100 case silently dropped
`internal/strategy/indicators.go:97-99` — when `avgLoss == 0` (all up-moves), RSI is by definition 100; code returns `nil` → Hold. For RSI-2 (`rsi_reversal.go:18`) two consecutive up-closes kill the strongest sell signal. Asymmetric (RSI=0 side works) → long bias. **Fix:** return `RSI{Value:100}`; both-zero → 50.

### ST-2 (HIGH). VWAP is not VWAP
`indicators.go:222-240` — uses close instead of typical price, is not session-anchored to 09:15 IST (rolls over whatever the buffer covers, possibly days), and if the live feed supplies cumulative day volume the weighting is mathematically wrong. Momentum strategy weights this meaningless number at 0.4 combined. **Fix:** session anchor, typical price, per-interval delta volume.

### ST-3 (HIGH). nine_twenty uses server-local clock, not IST
`nine_twenty.go:39-44` with `cmd/main.go:677` (`time.Now()`) — on a UTC VPS it would enter at 14:50 IST and never square off. No `Asia/Kolkata` anywhere in the strategy layer. Same defect in historical fetch windows (`broker/historical.go:43-50`) and discovery expiry logic (`discovery.go:227, 283`). **Fix:** explicit IST conversion everywhere; fail startup if tz data unavailable.

### ST-4 (HIGH). nine_twenty state is in-memory and flips on signal generation, not fill confirmation
`nine_twenty.go:15-16, 33-37, 59-66` — restart at 11:00 with a straddle open → `entered=false` → the 15:15 exit never fires; straddle rides into expiry. Also marks itself in/flat even when orders reject. **Fix:** persist/reconcile state from broker positions; confirm fills before state flips.

### ST-5 (HIGH). Stateless strategies re-signal every candle → close/reopen churn
`short_straddle.go:36-47`, `iron_fly.go:34-48` — while conditions hold, a fresh entry signal fires every evaluation; the backtest flattens and reopens every 5-min candle (up to ~75 round trips/day of charges + spread on 2–4 legs). Strategies have no way to know a position exists. **Fix:** pass position state into `Evaluate` or make them stateful (enter once, then monitor exit).

### ST-6 (HIGH). Backtest charge model understates real costs
`cmd/backtest/main.go:211-213, 292-294` — charges computed as buy-side ×2 (misses sell-side STT on premium — the largest cost for option sellers); one flat brokerage for what is really 8 orders on an iron-fly round trip; hardcoded `estPremium := 150.0`; zero slippage/spread. **Fix:** per-leg, per-side charges at the leg's simulated premium; slippage parameter.

### ST-7 (HIGH). Fills at signal-candle close + possibly incomplete last candle
`cmd/backtest/main.go:156-166`, `broker/historical.go:43-44` — signals computed on candle *i* close and filled at that same close (unattainable); running intraday includes the currently-forming candle as the last bar (repainting patterns). **Fix:** fill at `candles[i+1].Open` + slippage; truncate fetch to last completed interval.

### ST-8 (HIGH). Signal semantics overloaded — "Buy" means both "go long" and "exit short"
`strategy.go:8-12`; `short_straddle.go:50-56` emits Buy(=exit) every candle even when flat. Any consumer that reads Buy directionally would buy options continuously. **Fix:** explicit `Exit`/`Reverse` actions or `ClosePosition` flag on Signal.

### ST-9 (HIGH). Sniper: duplicate signals per candle, tick-count "candles", no timestamps
`sniper.go:46-72, 146-171` — same completed candle re-signals every poll tick (no latch); "5-minute" candles complete every 5 ticks, so bar duration depends on poll config; `Candle.Time` never set; `Volume` abused as tick counter. **Fix:** one signal per completed candle; wall-clock IST aggregation; real volume deltas.

### ST-10 (MEDIUM, selection). Other medium items
- `broker/historical.go:124-134` — parse errors ignored → silent 0.0 candles poison indicators; no OHLC sanity/gap/sort validation.
- `cmd/backtest/main.go:149` — lot size hardcoded 50; NIFTY is 75 (post-Apr-2025); three different lot sizes across the codebase.
- `candlestick.go:73, 93` — engulfing uses `>=`/`<=`; intraday candles open ≈ prior close, so the pattern degrades to half-engulfing and fires far too often.
- `rsi_reversal.go` — Connors RSI-2 is a *daily equity* system; applied intraday to index options with the wrong exit (opposite extreme instead of short-SMA cross).
- `iron_fly.go:14-16` — fixed 200-pt wings ignore index level/IV regime; `OrderLeg` has no expiry attribute, so weekly vs monthly vs expiry-day gamma are indistinguishable.
- `cmd/backtest/main.go:104` — 30 days, single instrument, in-sample only; no walk-forward, no OOS, no drawdown/Sharpe/profit-factor reporting. All parameter defaults are folklore.
- `registry.go:12-14` — `Get` returns a fresh instance per call: stateful strategies (nine_twenty, sniper) silently lose state if fetched twice.
- `momentum.go:149, 194` — `score / conditions * conditions` is a no-op; "normalization" is false.
- SuperTrend/ATR **do not exist in the codebase** despite README and Sniper's framing.
- `py-brain/src/strategies/indicators.py` — 9-line stub with a hard `cudf` (GPU) import; computes nothing.

---

## 4. Execution / Broker / Risk Findings (beyond criticals)

### EX-1 (HIGH). `ClosePosition` refuses to close when the price feed is down
`internal/engine/engine.go:106-109` — returns an error if `currentPrice == 0` before placing a **market** order that needs no price. The one time you must exit (feed outage, broker degradation) is exactly when exit is blocked.

### EX-2 (HIGH). Circuit breaker blocks exits too
`angel_broker.go:625-628, 729-737` — five consecutive failures (including business rejections) block **all** orders for 30s, including stop-loss closes. Never gate position-reducing orders.

### EX-3 (HIGH). Order throttle broken
`risk.go:355-357` — `ResetOrderCount` has no callers: "100 orders/min" is actually a 100-orders-per-session cap that then rejects all entries (but not closes) forever. YAML `max_orders_per_min` isn't even in the config struct (hardcoded at `cmd/main.go:221`; mislabeled "Max quantity per order" at `titan.go:97`).

### EX-4 (HIGH). Charge rates wrong for FY 2025-26; futures/options conflated
`config.yaml:91-110`, `risk.go:102-157` — STT `fno: 0.0125%` vs actual **0.1% of premium on options sell** (~8x understated) and 0.02% futures sell; txn charges use the futures rate (options ~0.035% of premium, ~18x understated); stamp duty wrong for F&O. Real round-trip costs ≈ 3–5x modeled. A strategy that papers as profitable can be a guaranteed live loser. **Fix:** split `FNO` into `FUT`/`OPT`; update all rates; unify with the separate (disagreeing) fee estimate at `angel_broker.go:811`.

### EX-5 (CRITICAL-adjacent). `PlaceOrder` holds the broker write-lock through sleeps and HTTP calls
`angel_broker.go:616-822` — the mutex is held across order placement *and* the ~37s fill poll (worst case minutes with HTTP timeouts). Meanwhile every price read blocks: **no stop-loss checks on any other position while one order is pending.** Fix: lock only around state mutation; never around network I/O.

### EX-6 (HIGH). No session-token refresh — mid-day expiry silently kills the system
`angel_broker.go:197-199` — `refreshToken` stored, never used. After expiry, quote fetches fail into empty maps, cached prices freeze (no staleness timestamps — `angel_broker.go:527-580`), the loop looks healthy, orders fail, circuit opens, positions unmanageable. **Fix:** central 401 detection → token refresh or TOTP re-login → loud alert + flatten on failure.

### EX-7 (HIGH). `GetBalance` is hardcoded
`angel_broker.go:992-997` — `a.balance = 10000.0 // Placeholder`. No real RMS funds check before orders; logs record a fictional balance. Implement the RMS API.

### EX-8 (HIGH). Trade log truncated on every startup
`internal/logger/csv_logger.go:31` — `O_TRUNC` wipes `logs/trades.csv`, the **only** trade record, on every start. A crash-restart destroys the audit trail exactly when needed. No OrderID column → cannot reconcile with broker contract notes. GORM `models/trade.go` implies a DB that doesn't exist (no DB driver in `go.mod`). **Fix:** append-only, date-stamped files; SQLite WAL ledger with broker order IDs as the system of record.

### EX-9 (MEDIUM, selection).
- No market-hours gate anywhere — trades/SL logic run 24/7; off-hours rejections trip the circuit breaker; pre-open freak prints can fire signals.
- Expiry rules outdated: `discovery.go:275-278` assumes NIFTY-Thursday/BANKNIFTY-Wednesday weeklies; BANKNIFTY weeklies were discontinued (Nov 2024) and NSE index expiries moved to Tuesday (2025). `config.yaml:52` hardcodes expiry `20JAN26` — **already six months in the past**; every generated option symbol currently fails lookup. Derive expiries from the instrument master.
- Product type silently forced to `CARRYFORWARD` for all NFO — no broker-side intraday auto-square-off if the bot dies.
- HTTP status codes never checked on Angel calls; WAF HTML handled by string-sniffing in only two places; WAF block during position fetch returns "no positions" while positions exist (`angel_broker.go:281-285`) — poisoning the emergency-liquidation path (`cmd/main.go:233-266`).
- Position math can't flip through zero (`angel_broker.go:1016-1021`, `mock.go:161-167`) — sell 75 against long 25 = internally flat, actually short 50.
- Unsynchronized reads on risk-manager getters (`risk.go:360-372, 446-447`) and `InstrumentManager` loading a ~100MB JSON via bare `http.Get` (no timeout) while holding the broker lock (`instruments.go:49`) — a hung CDN connection deadlocks the broker forever. `go run -race` will light up.
- Graceful shutdown: close failures only logged, then `os.Exit(0)` from a goroutine while the loop may be mid-order (`cmd/main.go:528-563`); can exit "cleanly" with live positions.
- Paper fill model is fantasy: fills at LTP (no bid/ask), 0.05–0.1% uniform slippage vs real 0.5–5% NFO spreads, always-complete instant fills, and `mock.go:112-116` **credits full turnover on shorts with no margin** — shorting mints money in paper mode.
- WebSocket feed doesn't exist: `feed/feed.go` is an empty stub; market data is 5s REST polling. The "HFT / ultra-low latency" claim is off by ~4 orders of magnitude.
- Two engines/risk managers constructed in `main.go` (`:210` then replaced `:352`); the whole `internal/app` path is a second half-finished codepath whose strategy loop does nothing (`modes.go:103-108`).
- `config.yaml`'s `engine:` block is nested under `brokers:` but parsed as top-level → **silently ignored**; edits to poll interval etc. do nothing (defaults coincidentally match).

---

## 5. API / Mobile / Infrastructure Findings (beyond criticals)

- **(HIGH)** Control API binds `:8080` on all interfaces, plaintext HTTP (`server.go:118-122`); design doc claims HTTPS/localhost-only + CORS restriction — neither implemented (`docs/MOBILE_APP_DESIGN.md:66-72,186-198` marks them ✅ done).
- **(HIGH)** `/ws/live` WebSocket has **no auth** (`server.go:110`) and `CheckOrigin` allows all (`:92-95`) — any LAN device or webpage can stream live balance/P&L. CORS is `*` (`:140-142`).
- **(MEDIUM)** WS concurrent-write race: `broadcast` (`:392-398`) and per-conn heartbeat (`:370-388`) can write the same conn simultaneously — gorilla/websocket panics → **the whole trading process dies because a phone connected**. One writer goroutine per conn.
- **(MEDIUM)** `/api/config` accepts unvalidated values, applies nothing to the real engine, persists nothing; GET returns hardcoded lies (`server.go:253-288`).
- **(HIGH, if compose used)** Postgres published to host with password `password`; Redis with no auth; gRPC `add_insecure_port` on 50051; Streamlit dashboard unauthenticated (`docker-compose.yml:8-23,57-58`).
- **(HIGH)** docker-compose cannot build: no `go-engine/Dockerfile`; `Dockerfile.gpu:19` COPYs outside build context (illegal); dashboard container reads a CSV path that doesn't exist inside it; no restart policies or healthchecks.
- **(MEDIUM)** `py-brain/requirements.txt:7` is invalid pip syntax (`--index-url` per-requirement) — documented install fails; floors unpinned; 2023-era RAPIDS pin.
- **(MEDIUM)** 5 compiled `.exe` binaries (~53MB) and real trade logs sit in the source tree; `go run` used as the production launcher; interactive-window lifecycle.
- **(MEDIUM)** Dashboard hardcodes `initial_risk_balance = 1000.0` while scripts default 10000 (`dashboard/app.py:115`) — drawdown/usage/alerts wrong by 10x; open positions inferred by counting BUY vs SELL CSV rows (breaks across restarts); baseline ₹10,00,000 hardcoded (`app.py:168,242`).
- **(LOW)** `mobile-app` hardcodes paper mode (`titanmobile.go:46`) — good; keep it that way. But it writes config world-readable with the hardcoded fallback key `titan-mobile-secret` (`titanmobile.go:78,96`).

---

## 6. Documentation vs Reality (planning integrity)

The README/docs describe a materially different system than exists. This is a planning failure with operational risk: an operator will make decisions assuming capabilities that aren't there.

| Claimed | Reality |
|---|---|
| TimescaleDB tick store, Postgres+GORM trade logs | No DB driver in `go.mod`; nothing reads/writes any DB |
| Redis hot state / Asynq queues | No Redis client anywhere |
| gRPC Go↔Python | Proto imports commented out; protos never compiled; no gRPC on Go side |
| Apache Arrow Flight zero-copy IPC | `ipc.go` is a 16-line stub that logs a string |
| RAPIDS/cuDF GPU indicators, LSTM/Transformer | 9-line stub; no models exist |
| Zerodha Kite Connect | Zero Zerodha code; README config example doesn't match actual schema |
| "HFT, ultra-low latency" | 2-second poll loop over REST, 5s price cache |
| Mobile security checklist "✅ rate limiting, CORS, HTTPS" | None implemented |
| WebSocket market feed | Empty stub |
| VWAP/SuperTrend indicators | VWAP broken (ST-2); SuperTrend/ATR absent |

**Fix:** Rewrite README to describe what exists; move the rest to an explicit, honest roadmap. Delete or clearly quarantine the dead `internal/app` codepath, `ipc.go`, `feed.go`, GORM models, and py-brain stubs.

---

## 7. Process / Planning Gaps

1. **No version control** — cannot answer "what code placed this trade?"
2. **No tests** — P&L, charge, and stop math unverified; risk of regression on every edit.
3. **No CI / static analysis** — `go vet -race` would already catch several data races.
4. **No staging discipline** — no defined paper→small-live→scale gate criteria.
5. **No runbook** — no documented procedure for: broker outage, engine crash with positions, session expiry, expiry-day operations.
6. **No trade journal/reconciliation process** — CSV (truncated on restart) vs broker contract notes never reconciled.
7. **Aspirational docs presented as done** — security checklists marked complete that were never implemented; this pattern is dangerous in a finance context.
8. **Dead/duplicate codepaths** — two engine paths, dead risk functions, dead SL fields: ambiguity about which code actually governs money.
9. **Config drift** — silently-ignored config blocks, hardcoded values disagreeing with config/scripts/dashboard (session balance 1000 vs 10000; lot sizes 25/50/65/75).
10. **Stale market assumptions unowned** — expiry weekday rules, lot sizes, and charge rates all reflect 2023–24 reality; no process to track NSE/SEBI changes (which have been frequent).

---

## 8. Honest Assessment: Probability of Trading Success

Even after all software defects are fixed, note:

1. **Current strategies have no demonstrated edge.** The only "validation" is a backtest whose option math guarantees profit (CR-9) or produces zero trades (CR-10). RSI-2 is a daily-equity system applied intraday; 9:20 straddle without stops is a known negative-skew strategy that works until one trend day erases months.
2. **Cost floor is high.** With correct FY26 charges, a NIFTY option round trip costs roughly ₹40–60+ per lot in charges alone, plus 0.5–5% spread on non-ATM strikes. Any strategy trading every 5 minutes is structurally handicapped.
3. **Required validation process before live money:**
   - ≥2–3 years of 5-min data including 2024/2025 regime changes; real historical option prices for option strategies.
   - Walk-forward (e.g., 6-month train / 1-month test rolling), parameter-sensitivity sweeps, and out-of-sample holdback.
   - Report expectancy, profit factor, max drawdown, Sharpe, and worst-day loss — not just win rate.
   - ≥1–2 months of paper trading through the *same* code path as live, reconciled daily against realistic fill/charge models.
   - Go-live gate: small size (1 lot), hard daily loss cap enforced broker-side, scale only after N profitable weeks with live-vs-paper tracking error within bounds.
4. **Positive-expectancy candidates to prioritize:** hedged structures (iron fly with wings placed first) over naked straddles; fewer, higher-conviction trades over 5-min churn; strategies whose edge survives 3x the modeled costs.

---

## 9. Remediation Roadmap

### Phase 0 — Stop the bleeding (today)
1. Rotate all Angel One credentials (TOTP re-enroll, new API key, new PIN) — CR-1.
2. Move project out of OneDrive; externalize secrets to env/credential store.
3. `git init` with proper `.gitignore`; delete committed binaries and logs from the tree.
4. Do not run `-live` again until Phase 1 complete. Paper mode OK.

### Phase 1 — Make the live path safe (1–2 weeks)
5. Persist + reconcile position state; startup reconciliation with broker; periodic reconciliation loop — CR-7, CR-8.
6. Wire `CheckRisk` into the loop; runtime kill switch; flatten-on-breach — CR-2, CR-3.
7. Fix order lifecycle: indeterminate ≠ filled, use `filledshares`, drop ₹1 fallback, handle partials and sign-crossover — CR-5, CR-6.
8. Broker-side SL-M with every entry; market-hours gate (IST); remove price-fetch dependency from market-order closes; exempt exits from the circuit breaker — CR-14, EX-1, EX-2.
9. Move network I/O out of the broker mutex; fix throttle reset; token refresh; real RMS balance; price-staleness guards — EX-3, EX-5, EX-6, EX-7.
10. Fix API server: independent random token, localhost bind/TLS, authenticated WS, real `/api/stop`, WS write serialization — CR-4, §5.
11. Append-only SQLite trade ledger with broker order IDs; fix CSV truncation — EX-8.
12. Dead-man watchdog + Telegram/SMS alerts; supervisor for the process — CR-15.
13. Unit tests for risk/charge/order math + CI (`go vet`, `go test -race`, lint) — CR-16.

### Phase 2 — Make results meaningful (2–4 weeks)
14. Unify live/backtest code path; time-based candle aggregation — CR-11, ST-9.
15. Real option pricing in backtest (historical option candles or BS+IV); fix dead entry branch — CR-9, CR-10.
16. Correct FY26 charges (split FUT/OPT), per-leg per-side, real premiums, slippage/spread model, next-open fills — EX-4, ST-6, ST-7.
17. Margin-aware sizing via broker margin API; hedge-first leg ordering with rollback — CR-12, CR-13.
18. Position-state-aware strategies (kill churn); explicit Exit/Reverse signal semantics; premium-based stops for all short-option strategies — ST-5, ST-8, CR-14.
19. IST-explicit time handling everywhere; expiry from instrument master (Tuesday regime, no BANKNIFTY weeklies); current lot sizes from master — ST-3, EX-9.
20. Fix indicators: RSI=100, session-anchored VWAP, engulfing strictness; implement or remove SuperTrend/ATR claims — ST-1, ST-2.
21. Realistic paper fills (spread, size-scaled slippage, occasional partials/rejects, margin on shorts).

### Phase 3 — Earn the right to trade live (4+ weeks, ongoing)
22. Multi-year walk-forward + OOS validation with metrics reporting (§8).
23. ≥1 month reconciled paper trading on the unified code path.
24. Go-live gate: 1 lot, broker-side daily loss cap, dead-man verified, runbook written.
25. Docs rewritten to match reality; delete dead codepaths (`internal/app` stub loop, `ipc.go`, `feed.go`, GORM models, py-brain stubs) or build them for real.
26. Scale rules: increase size only on sustained live-vs-paper tracking within tolerance.

---

## 10. What Is Already Good

- Clean module boundaries (`broker`/`engine`/`risk`/`strategy` separation) — the right skeleton to fix.
- EMA, SMA, MACD, Bollinger math is correct.
- TOTP-based auto-login works; instrument master download works.
- A graceful-shutdown handler with auto-liquidation exists (needs hardening, not creation).
- Live mode requires an explicit "I UNDERSTAND THE RISKS" confirmation.
- Mobile build hardcodes paper mode — correct default.
- Per-tick software stop-loss checking exists as a supplement layer.
- Charge-aware session-balance concept is the right idea — the rates and margin model just need fixing.

---

*Audit produced by three independent specialist reviews (quant strategy, execution/risk systems, infrastructure/security), consolidated 2026-07-18. File/line references verified against the tree as of this date.*
