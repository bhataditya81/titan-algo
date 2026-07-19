# Paper Trading Mode

This describes the current paper-trading flow, corrected against
`docs/reports/WP-9-REPORT.md` (integration), `WP-3-REPORT.md` (state
persistence), and `WP-5-REPORT.md` (ledger). Paper mode is not a disposable
demo path — it runs through the same state/reconciliation/ledger machinery as
live mode.

## What changed from earlier drafts of this document

Older instructions here described `go run cmd/main.go -paper` executing a
fixed set of 4 sample trades and printing a summary, with an in-memory-only
mock broker. That demo loop was deleted during integration (WP-9): `cmd/main.go`
now runs the same `Runner`/tick-loop engine used for live trading, driven by
strategy evaluation rather than a hardcoded trade script.

## 1. Build and run

```powershell
cd go-engine
go build -o titan.exe ./cmd
.\titan.exe -paper
```

(See `RUNNING.md` for why this is `go build` + run, not `go run`.)

## 2. What paper mode actually does

- Uses `MockBroker` (or `LivePaperBroker`, depending on configuration) instead
  of the real Angel One client for order execution — no real orders are ever
  placed.
- Still opens `internal/state`'s SQLite store (`data/titan_state.db`) and
  `internal/ledger`'s SQLite ledger (`data/titan_ledger.db`), and still runs
  the startup recover/reconcile sequence described in `RUNNING.md` — a paper
  session that crashes with open positions recovers on restart the same way a
  live session would.
- Runs the real strategy set (`internal/strategy`) against the real
  `Runner` tick loop (`internal/engine/runner.go`) — the same market-hours
  gating, kill-sentinel check, risk checks (`CheckRisk` every tick), and
  stop-loss logic that live mode uses.
- Charges are computed with the same `risk.EstimateCharges` FY 2025-26 rate
  table as live mode — paper P&L reflects real transaction costs, not a
  simplified flat fee.

## 3. Trade records

- **System of record:** `data/titan_ledger.db` (append-only SQLite via
  `internal/ledger`) — every order intent, fill, partial, rejection, and
  indeterminate outcome, with client/broker order IDs.
- **Secondary/dashboard log:** `go-engine/logs/trades_YYYY-MM-DD.csv`
  (date-stamped, append-only as of WP-5 — it no longer truncates on restart).
  Columns: `Timestamp,Symbol,Action,Quantity,FillPrice,Slippage,TransactionFee,BrokerBalance,RiskBalance,NetPnL,OrderID,Status`.
  Note: files written before this fix have only the first 10 columns and no
  `OrderID`/`Status` — any custom parser should read the header row rather
  than assume a fixed column count.

## 4. Dashboard (Streamlit, `py-brain/dashboard`)

```bash
cd py-brain/dashboard
pip install -r requirements.txt
streamlit run app.py
```

Open `http://localhost:8501`. The dashboard reads the CSV log described above.
Initial session balance and the CSV log directory are configurable via
`TITAN_SESSION_BALANCE` and `TITAN_CSV_LOG_DIR` env vars (WP-8 fixed these
from being hardcoded and mismatched against the engine's own default).

## 5. Control API in paper mode

The same `internal/api` server runs in paper mode: `/api/status`,
`/api/positions`, `/api/start`, `/api/stop`, `/api/kill`, `/ws/live` are all
live against the real paper-mode `Runner` via `ControlHooks` — `/api/stop` and
`/api/kill` genuinely pause/flatten the paper session, they are not
cosmetic. See `RUNNING.md` for the auth token and bind-address defaults.

## 6. Docker Compose

`docker-compose.yml` (WP-8) no longer publishes Postgres/Redis ports to the
host, requires `POSTGRES_PASSWORD`/`REDIS_PASSWORD` env vars (no default —
`docker compose config` fails loudly if unset), and includes healthchecks and
`restart: unless-stopped`. Postgres/Redis are present in the compose file for
future use (see README's Roadmap section) but nothing in the Go engine
currently reads or writes to them.

```bash
docker compose up --build
```

## 7. Important notes

- Paper mode uses simulated fills (mock broker), not real market microstructure
  — no real bid/ask spread beyond whatever slippage model the mock broker
  applies. Do not treat paper P&L as a prediction of live P&L.
- No strategy here has been through the walk-forward/out-of-sample validation
  process described in `docs/REMEDIATION_PLAN.md` WP-10 — that work has not
  been executed as of this writing.
- To exercise the crash-recovery/reconciliation path yourself: start a paper
  session, let it open a position, kill the process (not via `stop.ps1`, to
  simulate a real crash), then restart — you should see the recovered
  position and a reconciliation report. See `docs/RUNBOOK.md` section (b).
