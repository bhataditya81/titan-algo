# TitanAlgo

A Go trading engine for NIFTY/BANKNIFTY options on **Angel One** (SmartAPI), with
per-mode paper/live execution, persisted state with crash recovery and startup
reconciliation, an FY 2025-26 risk/charges engine, a hardened REST/WebSocket
control API, an append-only SQLite trade ledger, and position-aware strategies.

This README describes what the codebase actually does today, traced against the
implementation reports in `docs/reports/WP-1-REPORT.md` through `WP-9-REPORT.md`.
Where something is not built yet, it is listed in **Roadmap**, not implied to work.

> **This is a paper-trading-tested prototype, not a proven money-maker.** No
> multi-year walk-forward validation has been run (that is WP-10's separate,
> not-yet-executed scope). Do not run `-live` with real capital based on
> anything in this document.

## What it is not

Earlier drafts of this README described a different, larger system. None of the
following exists in the current codebase — corrected per the production
readiness audit (`docs/PRODUCTION_READINESS_AUDIT.md`, section 6):

- **Not Zerodha-compatible.** Only Angel One SmartAPI is implemented (`internal/broker`). No Zerodha/Kite Connect code exists.
- **Not "HFT" or ultra-low-latency.** The engine polls REST endpoints on a fixed interval — the default is `engine.poll_interval_ms: 2000` (2 seconds) in `config.example.yaml`. There is also a WS live-data path (`internal/broker/ws_feed.go`) used opportunistically when the broker connection supports it.
- **No TimescaleDB.** No time-series database of any kind is used or configured.
- **No Redis / Asynq job queues** wired into the Go engine.
- **No gRPC ML/analytics service.** The old `py-brain/src` gRPC "brain" stub (never wired to anything, no generated proto code) was removed. `py-brain/dashboard/` (Streamlit trade dashboard) is the only remaining py-brain piece, and it's a real, working service.
- **No GORM/ORM-backed database.** `models/trade.go` was rewritten (WP-3) to plain structs backed by SQLite via `internal/state` and `internal/ledger`, not GORM.

## What exists and works

### Broker: Angel One (`internal/broker`)
- Full order lifecycle correctness (WP-1): timed-out/ambiguous fills return a
  typed `ErrOrderIndeterminate` instead of being fabricated as filled; partial
  fills use the broker-reported filled quantity; positions correctly flip
  through zero on an opposite-side overfill.
- Session/token refresh: on HTTP 401 or an Angel `status:false` auth error, the
  broker attempts a token refresh, then one full TOTP re-login, before marking
  itself unhealthy (`Healthy()`/`HealthError()`).
- Real account balance via Angel's RMS endpoint (no more hardcoded `10000.0`).
- Price staleness tracking (`GetCurrentPriceWithAge`) and a instrument master
  cached to disk per day (no repeated ~100MB downloads).
- Broker-side stop-loss orders (`PlaceStopLossOrder`, `STOPLOSS_MARKET`) and
  `CancelOrder`.
- The circuit breaker never blocks position-reducing (`IntentReduceOnly`)
  orders, so exits and stop unwinds cannot be blocked by an open breaker.

### State persistence & reconciliation (`internal/state`)
- Every position, order-attempt, risk snapshot, and strategy state mutation is
  a synchronous, transactional write to a WAL-mode SQLite database
  (`data/titan_state.db` by default).
- Client order IDs are generated and persisted **before** the network call, so
  an ambiguous timeout can later be resolved against the broker's order book.
- On startup, `state.RecoverSession` reloads open positions and the last risk
  snapshot; `state.Reconcile` compares them against the broker's live position
  book and classifies mismatches (matched / phantom / orphan / quantity
  mismatch). See `docs/RUNBOOK.md` for what happens on a mismatch.

### Risk & charges engine (`internal/risk`)
- FY 2025-26 charge rates (as-of date noted in code): options STT 0.1%
  sell-side, futures STT 0.02% sell-side, exchange transaction charges split
  by instrument, GST 18% on brokerage+txn+SEBI fee, stamp duty by trade type,
  brokerage ₹20 flat for F&O.
- `EstimateCharges(...)` is the single source of truth for fee math, used by
  both the live risk manager and the backtest engine (WP-7 delegates to it).
- `CheckRisk()` is a cheap, side-effect-free call made every tick; on breach
  (kill switch, balance depletion, max drawdown) the engine halts entries and
  flattens.
- `TriggerKillSwitch()`/`KillSwitchActive()` are backed by `atomic.Bool` and
  can be triggered at runtime (API endpoint or the `data/KILL` sentinel file),
  not only at config-load time.
- A per-minute order throttle is a real sliding 60-second window.
- `ValidateOrderWithMargin`/`OpenPositionWithMargin` validate SELL-derivative
  orders against broker-supplied required margin instead of premium×qty — see
  Roadmap for the current gap (no margin API caller wired yet).

### Trade ledger (`internal/ledger`)
- Append-only SQLite table (`data/titan_ledger.db`) recording every order
  intent, fill, partial, rejection, indeterminate outcome, and later
  reconciliation, with broker order IDs and client order IDs.
- The old CSV logger (`internal/logger/csv_logger.go`) is fixed (no longer
  truncates on restart; appends to date-stamped files) and kept as a secondary
  log for the dashboard; the SQLite ledger is the system of record.

### API server (`internal/api`)
- Dedicated auth token (never the broker API key), generated with
  `crypto/rand` if not configured, printed once at startup and never logged
  again.
- Binds to `127.0.0.1` by default; optional TLS if cert/key are configured.
- `/ws/live` requires the token and validates the WebSocket `Origin` against a
  configurable allowlist; one writer goroutine per connection so a slow client
  is dropped, not blocking, and can't cause a concurrent-write panic.
- Configurable CORS allowlist (default: no CORS headers at all).
- `/api/start`, `/api/stop`, `/api/kill` are wired to real engine control hooks
  (`Pause`/`Resume`/`KillAndFlatten`) — if hooks are unset the endpoints return
  `503 not wired` rather than a fake success response.

### Strategies (`internal/strategy`)
Seven strategies, all rewritten for IST-aware timing, position-aware signal
semantics (an `Exit` action distinct from directional `Buy`/`Sell`, gated by
`EvalContext.HasPosition`), and (where applicable) premium-based stops:

- `ema_crossover` — directional EMA(9/21) crossover.
- `rsi_reversal` — RSI-2 mean reversion with an SMA/RSI-50 cross exit.
- `momentum` — multi-indicator momentum score (VWAP/RSI/EMA), normalization
  bug fixed.
- `nine_twenty` — 9:20 AM IST short straddle; combined-premium stop-loss
  (default 1.4x entry premium), state persisted and restored via
  `internal/state`, entry/exit flags flip only on confirmed fills.
- `sniper` — wall-clock 5-minute IST candle aggregation (not tick-count
  candles), one signal per completed candle.
- `iron_fly` — hedge legs (BUY wings) placed and confirmed before the short
  body legs, so a rejected wing cannot leave a naked short straddle.
- `short_straddle` — combined-premium stop-loss, RSI-band exit as secondary.

All strategies return `Hold` on insufficient data rather than panicking, and
`internal/strategy/registry.go` returns a cached singleton per strategy name
(stateful strategies keep their state across lookups).

### Backtest engine (`internal/backtest`)
- Real Black-Scholes repricing of every option leg on every candle (not a
  constant-delta model) — full delta/gamma/theta fall out of repricing, so a
  short straddle can actually lose money on a trending synthetic dataset
  (proven in WP-7's test suite and sample runs).
- Fills execute at the next candle's open (never the signal candle's own
  close), with configurable slippage.
- Charges computed per leg, per side, via `internal/risk.EstimateCharges`, plus
  a configurable spread cost.
- Reports: win rate, gross/net P&L, max drawdown, profit factor, expectancy,
  average win/loss, worst day, and a per-month breakdown; `-from`/`-to` date
  range and a local CSV candle cache (`-csv`) for reproducible offline runs.
- **Known v1 limitation, documented in code and here on purpose:** implied
  volatility is a single constant per run (default 12% for NIFTY, `-iv` flag),
  not a real historical-IV feed. This fixes the audit's core defect (the old
  model had zero gamma and made every short straddle mathematically guaranteed
  to profit) but a production-grade IV surface is future work.

## Roadmap / Not Yet Implemented

These are honestly incomplete — do not assume they work:

- **GPU/ML analytics ("Mode B").** Removed — the gRPC "brain" (`py-brain/src`)
  registered zero real services and had no trained model behind it. If this
  gets built for real later, it starts from scratch rather than resurrecting
  the stub.
- **Broker margin-API integration.** Angel's margin-calculator endpoint (A-6)
  is not called anywhere. Per WP-9, SELL-derivative order entries currently
  fail closed with an explicit error rather than being sized incorrectly —
  `short_straddle`/`iron_fly`-style strategies cannot open a real short
  position until this is wired.
- **Standalone watchdog binary.** A heartbeat file (`data/heartbeat`, touched
  every tick) and optional Telegram alerting exist in-process. Nothing
  external watches whether the whole engine process has died — see
  `docs/RUNBOOK.md` for the current manual procedure.
- **Auto-updating NSE holiday calendar.** `nseHolidays2026` in
  `internal/engine/runner.go` is a fixed table of known 2026 dates; it does not
  cover every movable festival holiday and has no update mechanism. Verify
  against NSE's official circular near any holiday.
- **Historical implied-volatility feed for the backtest engine** (see above).
- **Walk-forward / out-of-sample validation.** No multi-year validation run
  has been produced yet; that is WP-10's separate scope and has not been
  executed as of this writing.

## Repository layout (as it actually is)

```
titan-algo/
├── go-engine/
│   ├── cmd/
│   │   ├── main.go            # single entry point: paper/live via -paper/-live
│   │   └── backtest/main.go   # thin CLI over internal/backtest
│   ├── internal/
│   │   ├── api/               # REST + WebSocket control server
│   │   ├── backtest/          # Black-Scholes backtest engine
│   │   ├── broker/            # Angel One SmartAPI client
│   │   ├── config/            # env-first config loader
│   │   ├── engine/            # engine.go + runner.go (the trading loop)
│   │   ├── ledger/            # append-only SQLite trade ledger
│   │   ├── logger/            # secondary CSV trade log
│   │   ├── risk/              # risk manager + FY26 charge model
│   │   ├── state/             # SQLite position/order state + reconciliation
│   │   └── strategy/          # the 7 strategies listed above
│   ├── models/trade.go        # plain structs (Position/OrderAttempt/RiskSnapshot)
│   └── config.example.yaml
├── py-brain/dashboard/          # Streamlit trade dashboard (real, working)
├── web-ui/                      # browser control panel (strategy select, start/pause/kill, charts)
└── docs/
    ├── REMEDIATION_PLAN.md
    ├── PRODUCTION_READINESS_AUDIT.md
    ├── reports/WP-1..9-REPORT.md
    └── RUNBOOK.md
```

## Getting started

See `RUNNING.md` for the full startup sequence (environment variables,
config template, state/reconciliation flow) and `PAPER_TRADING.md` for the
paper-mode-specific walkthrough. See `docs/RUNBOOK.md` for operational
procedures (broker outage, crash recovery, expiry day, kill switch, credential
rotation).

## Disclaimer

This is a personal/proprietary trading system. Use at your own risk. The
authors are not responsible for any financial losses incurred through the use
of this software. No strategy in this repository has a demonstrated,
validated statistical edge as of this writing.
