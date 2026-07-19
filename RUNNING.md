# Running TitanAlgo

This describes the actual current startup flow, traced against
`docs/reports/WP-8-REPORT.md` (config/env/scripts) and `docs/reports/WP-9-REPORT.md`
(integration: startup sequence, recovery/reconcile, flags).

## 1. Prerequisites

- Go 1.22+ (module targets a newer toolchain; any Go that can build the module works).
- An Angel One SmartAPI account, if you intend to connect to the real broker
  even in paper mode's "live data" path. Pure offline/mock paper runs do not
  need this.

## 2. Credentials: environment variables, not `config.yaml`

`internal/config/config.go` resolves six credential fields from environment
variables first; `config.yaml` is only a fallback, and in **live mode** a
credential still sourced from YAML causes a hard startup error (WP-8's
`ValidateLiveCredentials`). Set these before running anything live:

| Env var | Purpose |
|---|---|
| `ANGEL_CLIENT_CODE` | Angel One client code |
| `ANGEL_PIN` | Login PIN |
| `ANGEL_API_KEY` | SmartAPI key |
| `ANGEL_API_SECRET` | SmartAPI secret (maps to `password` in config) |
| `ANGEL_TOTP_SECRET` | TOTP base32 seed for 2FA |
| `TITAN_API_TOKEN` | Control-API auth token (optional — if unset, the server generates a random 32-byte token at startup and prints it once) |

Never put real values in `config.yaml` and never commit it — it stays in
`.gitignore`. Rotate any credential you suspect has ever been on disk
unencrypted; see `docs/RUNBOOK.md` for the rotation procedure.

Optional (Telegram alerting, no-op if unset): `TITAN_TG_TOKEN`, `TITAN_TG_CHAT`.

## 3. Config file

Copy the template and edit non-secret fields (poll interval, risk limits, API
bind address, DB paths):

```powershell
cd go-engine
copy config.example.yaml config.yaml
```

Leave every credential field in `config.yaml` empty — env vars are the source
of truth. Notable fields (see `config.example.yaml` for the full, commented
list):

- `engine.poll_interval_ms` — default `2000` (the engine polls REST on this
  interval; there is no WebSocket market-data feed).
- `api.bind_addr` — default `127.0.0.1:8080`.
- `api.token` — leave empty; use `TITAN_API_TOKEN` instead.
- `state.db_path` / `ledger.db_path` — default `data/titan_state.db` /
  `data/titan_ledger.db`.
- `risk.kill_switch_enabled` — defaults `true` in the template (safer default
  for live use; the software flag alone is not a substitute for broker-side
  controls).

## 4. Build, don't `go run`

WP-8 fixed the launch scripts to build a binary once and run that, rather than
`go run` (which re-compiles on every start and produces confusing PID/process
lifecycle for the stop script):

```powershell
cd go-engine
go build -o titan.exe ./cmd
.\titan.exe -paper
```

The provided scripts (`start.ps1`, `start-paper.ps1`, `start-live.ps1`) do this
build step automatically and record the resulting process's PID to
`go-engine\titan.pid` so `stop.ps1` can find it.

## 5. Command-line flags (`cmd/main.go`)

| Flag | Meaning |
|---|---|
| `-paper` | Paper trading mode (`MockBroker`/`LivePaperBroker`) |
| `-live` | **Real broker, real orders.** Requires all credentials to resolve from env vars (`ValidateLiveCredentials` hard-fails otherwise). |
| `-search "QUERY"` | One-shot instrument master search, no trading |
| `-accept-reconcile` | See startup sequence below — required to proceed past a non-clean reconciliation |

## 6. Startup sequence (what actually happens)

In order, per `internal/engine/runner.go` and `cmd/main.go` (WP-9):

1. `config.Load` reads `config.yaml`, resolves env vars over it.
2. If `-live`, `cfg.ValidateLiveCredentials()` runs — hard error if any
   credential came from YAML instead of the environment.
3. The broker client is constructed and `Connect()`ed.
4. `state.Store` (`internal/state`) and `ledger.Ledger` (`internal/ledger`)
   are opened against their configured SQLite paths.
5. `state.RecoverSession(store)` reloads any positions and the last risk
   snapshot left over from a previous run.
6. The broker's live position book is fetched (`GetPositions`) and compared
   against the recovered internal state via `state.Reconcile`.
7. **If the reconciliation is not clean** (any phantom, orphan, or quantity
   mismatch), the process prints the mismatch report and refuses to trade
   unless `-accept-reconcile` was passed on the command line. See
   `docs/RUNBOOK.md` section (b) for what a mismatch report looks like and how
   to decide whether it's safe to accept it.
8. Only after a clean reconcile (or an explicit `-accept-reconcile`) does the
   engine's `Runner` start its tick loop: market-hours gating, strategy
   evaluation, order placement, risk checks, kill-sentinel checks, and
   heartbeat writes (`data/heartbeat`) each tick.

## 7. Stopping

Prefer the graceful path — `stop.ps1` (WP-8) calls `POST /api/stop` then
`POST /api/kill` against the running instance's API server (using
`TITAN_API_TOKEN`), which pauses entries and flattens open positions before
exit. If both calls fail (e.g. the API server itself is unresponsive),
`stop.ps1` falls back to killing the process by the PID recorded in
`titan.pid`, but this bypasses the graceful flatten sequence — check
`docs/RUNBOOK.md` for what to check afterward.

Ctrl+C on an interactively-run process also works: `cmd/main.go` installs a
signal handler that cancels the runner's context, triggering the same
pause → flatten-with-retries → verify-empty-position-book shutdown sequence.

## 8. Paper mode specifics

See `PAPER_TRADING.md`. In short: paper mode now goes through the same
`state`/`ledger` persistence and reconciliation flow as live mode — it is not
a separate, throwaway code path. Only the actual order execution differs
(mock/simulated fills instead of real broker orders).
