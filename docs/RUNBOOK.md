# TitanAlgo Runbook

Operational procedures for running TitanAlgo, written against the actual
implementation described in `docs/reports/WP-1-REPORT.md` (broker/session),
`WP-3-REPORT.md` (state/reconciliation), `WP-4-REPORT.md` (API/kill), and
`WP-9-REPORT.md` (integration — the authoritative source for end-to-end
runtime behavior). Where a capability doesn't exist yet, this runbook says so
explicitly rather than describing a procedure for something that isn't built.

## Contents

1. [Broker session/auth failure mid-day](#1-broker-sessionauth-failure-mid-day)
2. [Engine crash with open positions](#2-engine-crash-with-open-positions)
3. [NSE expiry day operations](#3-nse-expiry-day-operations)
4. [Using the kill switch](#4-using-the-kill-switch)
5. [Credential rotation](#5-credential-rotation)
6. [Watchdog/heartbeat gone stale](#6-watchdogheartbeat-gone-stale)

---

## 1. Broker session/auth failure mid-day

### What it looks like
Angel One sessions expire; the broker client also degrades on genuine network
faults. Per WP-1, every authenticated call goes through a central request
path that detects:
- HTTP 401, or
- Angel's `status:false` response body with an error code prefixed `AG`/`AB`.

In logs this shows up as failed order placements, failed price/position
fetches, or an explicit `ErrAuthFailed`/unhealthy broker log line. `Healthy()`
will report `false` and `HealthError()` will hold the error once the
automatic recovery below has been exhausted.

### What the system does automatically
1. On detecting an auth failure, the broker attempts a **token refresh**
   (Angel's `generateTokens` endpoint, using the stored refresh token).
2. If that fails, it attempts **one full TOTP re-login** (the same login flow
   used at initial `Connect()`).
3. If that also fails, the broker marks itself **unhealthy** (`Healthy() ==
   false`) rather than silently continuing to fail calls one by one.

None of this requires operator action if it succeeds — it's designed to be
invisible for a routine token refresh.

### What the operator should check
- If the broker stays unhealthy after the automatic refresh+re-login
  sequence, check: is the TOTP secret still valid (was it rotated at Angel
  One without updating `ANGEL_TOTP_SECRET`)? Is the PIN correct? Has Angel
  flagged the account (e.g. too many failed logins, WAF block)?
- Check for a WAF-block response — the broker treats a WAF-blocked position
  fetch as a hard error (never silently "no positions"), per WP-1. If you see
  WAF-related errors in the log, the account/IP may be temporarily blocked at
  the network layer; this is not something a re-login fixes.
- If positions were open when the broker went unhealthy: the software
  stop-loss loop (`softStopLossCheck` in `internal/engine/runner.go`) treats
  a price older than 15 seconds as invalid and will **not** fire blind — it
  logs/alerts instead of guessing. This means a dead broker connection leaves
  positions unprotected by the software stop until the connection recovers.
  **Broker-side stop-loss orders placed at entry (`PlaceStopLossOrder`) are
  the actual safety net here** — they rest at the exchange independent of
  whether the Go process can currently talk to Angel.
- If the broker cannot be recovered promptly, use the kill switch (section 4)
  to stop new entries, and manually verify open positions via the Angel One
  app/web terminal — the engine's own view of positions may be stale while
  the broker connection is down.

---

## 2. Engine crash with open positions

### What happens on restart (per WP-9)
1. `state.RecoverSession(store)` reloads whatever open positions and risk
   snapshot were last durably written to `data/titan_state.db`.
2. The broker's live position book is fetched fresh.
3. `state.Reconcile(recovered, brokerPositions)` compares the two and
   classifies every symbol into one of:
   - **Matched** — internal and broker agree (net signed quantity equal).
   - **Phantom** — internal thinks there's a position, broker shows none (or
     less than expected, netted).
   - **Orphan** — broker shows a position with no internal record.
   - **Quantity mismatch** — both sides have a position but the net signed
     quantity disagrees (this also catches a side flip — e.g. internal
     records long 75, broker actually shows short 75 — treated conservatively
     as a mismatch, not a match).
4. If the reconciliation is **not clean**, the process prints the full report
   (every mismatched symbol, both sides' recorded quantity) and refuses to
   start trading unless launched with `-accept-reconcile`.

### What a reconcile mismatch report looks like
Each mismatched item shows the symbol, the mismatch type, and both sides'
recorded net quantity, e.g. (illustrative — exact log format may vary):

```
RECONCILE MISMATCH: NIFTY27JAN2625000CE
  type: quantity_mismatch
  internal: SELL 75 (from state.db)
  broker:   SELL 150 (from live position book)
```

or for a phantom:

```
RECONCILE MISMATCH: NIFTY27JAN2625000PE
  type: phantom
  internal: BUY 75 (from state.db)
  broker:   (no position found)
```

### When `-accept-reconcile` is needed
- Any time the report is non-empty. Do **not** reach for the flag reflexively
  — first manually check the broker's own order/position book (Angel One
  app or web terminal) against what the report says, and decide:
  - If the broker is right and internal state is stale/wrong (e.g. an order
    filled that the process never learned about before crashing): accepting
    is safe — the engine's own risk book will be rebuilt from `store` and the
    next mismatch cycle should be clean once broker/internal converge.
  - If you cannot explain the mismatch, or it implies an unprotected naked
    position (e.g. an orphan short with no recorded stop-loss): **do not**
    accept blindly. Manually flatten the position via the Angel One
    app/terminal first, then restart so the next reconcile is clean without
    needing the override flag at all.
- `-accept-reconcile` lets the process proceed to trade despite the mismatch;
  it does not resolve the mismatch for you. Treat it as "I have manually
  verified this is safe to proceed," not a generic unblock switch.

---

## 3. NSE expiry day operations

- Expiries are resolved from the instrument master (`InstrumentManager.GetExpiries`,
  WP-1) rather than weekday-arithmetic guessing, with a config override and a
  hardcoded fallback constant as last resort — each fallback tier is logged
  loudly if it's used (WP-9). If you see a fallback-tier log line on expiry
  day, treat it as a signal to double check the resolved expiry/symbol
  manually before trusting the day's option selection.
- The market-hours gate (`internal/engine/runner.go`) squares off at a
  configurable time (default 15:20 IST) and hard-closes entries at 15:30 —
  the same gate applies on expiry day as any other day; there is no
  expiry-specific special-casing beyond correct expiry symbol resolution.
- `nseHolidays2026` is a fixed, incomplete table (documented gap — movable
  festival holidays are not covered). Before trading near a holiday, manually
  verify the date against NSE's official trading-holiday circular; do not
  trust the table alone to block a holiday session.
- Options near expiry have amplified gamma risk; the combined-premium stops
  on `nine_twenty`/`short_straddle` and broker-side SL-M orders are the same
  mechanisms used any other day — there is no separate expiry-day risk
  control layer. Consider manually reducing size or sitting out expiry day
  until the strategy set has been through the walk-forward validation
  described in `docs/REMEDIATION_PLAN.md` WP-10 (not yet executed).

---

## 4. Using the kill switch

Two independent mechanisms, both wired into the tick loop (WP-9):

### (a) Sentinel file: `data/KILL`
- Create an empty file at `data/KILL` (relative to wherever the engine
  process's working directory resolves that path — check `state.db_path`'s
  sibling location, typically `go-engine/data/KILL`).
- Every tick, `checkKillSentinel()` in `internal/engine/runner.go` checks for
  this file's existence and, if present, calls `risk.Manager.TriggerKillSwitch()`.
- Effect: same as the runtime kill switch — the risk manager's kill-switch
  flag is set (backed by `atomic.Bool`, safe from any goroutine), which
  `CheckRisk()` picks up on its next call, halting entries and triggering a
  flatten of all open positions.
- This mechanism does not require the API server to be reachable — useful if
  the control API itself is unresponsive.
- Delete the sentinel file before the next intended trading session, or the
  engine will refuse to (re-)enter as soon as it starts and sees the file.

### (b) `/api/kill` endpoint
- `POST /api/kill` (with the configured auth token) calls
  `ControlHooks.KillAndFlatten`, wired to the real `Runner.KillAndFlatten` by
  WP-9.
- If `SetControlHooks` was never called (should not happen in the normal
  `cmd/main.go` startup path, but matters if you're driving `internal/api`
  standalone), the endpoint returns `503 {"error":"not wired"}` — a real
  failure signal, not a fake success.
- Prefer this over the sentinel file when the API server is confirmed
  reachable — it gives you an HTTP response confirming the hook fired (or a
  clear error if it didn't), whereas the sentinel file's effect is only
  visible in the engine's own logs on its next tick.
- `stop.ps1` (WP-8) calls `POST /api/stop` then `POST /api/kill` as its
  primary path, falling back to a raw process kill (via the recorded PID)
  only if both HTTP calls fail — that fallback bypasses the graceful flatten
  sequence, so treat a fallback-triggered stop the same as an unplanned crash
  (see section 2) and check reconciliation on the next start.

---

## 5. Credential rotation

Per WP-8, **environment variables are the source of truth**, not
`config.yaml` — `config.yaml` should have every credential field left empty.

1. At Angel One: regenerate the API key, re-enroll TOTP (new base32 secret),
   and change the login PIN. Treat all previously-used values as burned the
   moment you suspect any exposure (e.g. they were ever committed to a repo,
   synced via OneDrive, or logged anywhere).
2. Update the environment variables the process reads at startup:
   `ANGEL_CLIENT_CODE`, `ANGEL_PIN`, `ANGEL_API_KEY`, `ANGEL_API_SECRET`,
   `ANGEL_TOTP_SECRET`. Do **not** write the new values into `config.yaml` —
   in live mode, `ValidateLiveCredentials()` will hard-fail startup if a
   credential resolves from YAML instead of the environment, specifically to
   prevent this.
3. Restart the engine process — credentials are read once at `config.Load`
   time; there is no hot-reload of credentials into a running process.
4. Rotate `TITAN_API_TOKEN` (the control-API auth token) independently if you
   suspect it's been exposed — it is unrelated to the broker credentials and
   was specifically designed (WP-4) to never default to or reuse the broker
   API key. If left unset, the server generates and prints a new random token
   on the next startup; there is no way to view a previously-generated token
   again after that startup's console output scrolls away, so set
   `TITAN_API_TOKEN` explicitly if you need a stable, recorded value.
5. If you're rotating because of a suspected leak (not routine hygiene): also
   check the broker's own login history/active-session list at Angel One
   for any session you don't recognize, and consider the account compromised
   until you've confirmed otherwise.

---

## 6. Watchdog/heartbeat gone stale

**Honest gap, stated plainly:** WP-9 built a heartbeat file
(`data/heartbeat`, touched once per tick) and optional Telegram alerting
(`TITAN_TG_TOKEN`/`TITAN_TG_CHAT` env vars, no-op if unset) for specific
in-process events (kill-switch trigger, drawdown breach, stale price,
indeterminate order, margin-unavailable, flatten failure). **No standalone
watchdog binary exists** that independently monitors whether the heartbeat
file has gone stale or whether the whole process has died — this was
explicitly deferred (WP-9's report lists it as a follow-up, not done).

Until that binary is built, this is a **manual procedure**:

1. Periodically (or via your own external cron/scheduled task — not provided
   by this repo) check the modification time of `data/heartbeat`. If it is
   older than a few multiples of `engine.poll_interval_ms` (default 2000ms,
   so anything more than a few seconds stale during market hours is
   suspicious), the process is likely stuck or dead.
2. Check whether the process is still running at the OS level (Task
   Manager, or `Get-Process -Id (Get-Content go-engine\titan.pid)` in
   PowerShell if the PID file is present).
3. If the process is dead: any open positions have broker-side stop-loss
   orders resting independently (placed at entry, per WP-1) — these remain
   active at the exchange even with the Go process down. Manually verify via
   the Angel One app/terminal that stops are in place and positions are
   otherwise as expected.
4. If the process is alive but the heartbeat is stale: this suggests the tick
   loop is blocked (e.g. hung HTTP call, deadlock). Do not assume it will
   self-recover — treat it as equivalent to "broker unreachable" (section 1)
   and consider a manual restart. A stuck loop does not, by itself, flatten
   positions — the broker-side stops are again the actual safety net during
   the stuck period.
5. If Telegram alerting is configured (`TITAN_TG_TOKEN`/`TITAN_TG_CHAT`), the
   in-process alert events listed above will still fire even without a
   separate watchdog — those cover several dangerous conditions (drawdown,
   kill-switch, stale price) but **do not** cover "the whole process
   crashed," since a crashed process cannot alert on its own crash. This is
   exactly the gap an external watchdog binary is meant to close; until it's
   built, an external, out-of-process check (per step 1) is the only
   coverage for that specific failure mode.

---

## 7. Standalone watchdog binary (`cmd/watchdog`) — R2-5

**Closes the gap described in section 6 above** (section 6 is left as-is,
above, since it's still useful background on what the engine itself does;
this section documents the external process that now covers "the whole
process crashed").

`cmd/watchdog` is a separate, small binary — it does not import the engine
and keeps running even if the engine process is completely dead. It polls
the heartbeat file's mtime on an interval and alerts **once per stale
episode** (not once per poll — it tracks whether it already alerted for the
current breach and resets when the heartbeat becomes fresh again).

### Flags / environment

| Flag | Default | Notes |
|---|---|---|
| `-heartbeat` | `data/heartbeat` (or `TITAN_HEARTBEAT_PATH` env var) | Must match the engine's `HeartbeatFilePath` (see `cmd/main.go`/`internal/app/titan.go`, both currently `"data/heartbeat"`). |
| `-max-age` | `60s` | Heartbeat older than this (or missing entirely) is stale. |
| `-poll` | `15s` | How often to check. |
| `-restart-cmd` | `""` (disabled) | Optional command to run when stale is first detected. Space-separated, no shell expansion — e.g. `-restart-cmd "C:\path\to\restart-titan.ps1"` needs a `.ps1` wrapper that itself invokes `powershell.exe` if you need shell features. |
| `-restart-cooldown` | `10m` | Minimum time between restart attempts, across separate breach episodes — prevents restart-looping if the heartbeat flaps. |

Telegram alerting uses the **same env vars as the main engine**
(`TITAN_TG_TOKEN`, `TITAN_TG_CHAT`, WP-9's convention) — set them once and
both the engine and the watchdog alert through the same bot/chat. If either
is unset, alerts are a no-op (logged locally only), same as the engine.

### Manual run

```
cd go-engine
go build -o watchdog.exe ./cmd/watchdog
./watchdog.exe -heartbeat data/heartbeat -max-age 60s -poll 15s
```

### Scheduling under Windows Task Scheduler

The watchdog is a long-running poller, not a one-shot script, so schedule it
to start **at logon** (or at system startup) and let it run continuously
alongside the engine, rather than scheduling repeated short-lived runs.

One-liner (run once, from an elevated PowerShell prompt, to register the
task; adjust paths):

```powershell
schtasks /Create /TN "TitanWatchdog" /TR "C:\dev\titan-algo\go-engine\watchdog.exe -heartbeat C:\dev\titan-algo\go-engine\data\heartbeat -max-age 60s -poll 15s" /SC ONLOGON /RL LIMITED /F
```

- `/SC ONLOGON` — starts when you log in (use `/SC ONSTART` instead if the
  engine itself runs as a service/at boot without a login).
- `/RL LIMITED` — runs with standard (non-admin) privileges; the watchdog
  only reads a file and optionally shells out to `-restart-cmd`, it doesn't
  need elevation unless your restart script does.
- `/F` — overwrite if a task with the same name already exists (safe to
  re-run this line after changing flags).
- Environment variables (`TITAN_TG_TOKEN`, `TITAN_TG_CHAT`,
  `TITAN_HEARTBEAT_PATH`) set via `setx` (system/user environment) are picked
  up automatically since Task Scheduler launches the process in the user's
  normal environment — no need to pass them on the command line.

To remove: `schtasks /Delete /TN "TitanWatchdog" /F`.
To check status: `schtasks /Query /TN "TitanWatchdog" /V /FO LIST`.

To attach a restart script, add `-restart-cmd "C:\dev\titan-algo\restart-titan.ps1"`
to the `/TR` command line above (quote the whole `/TR` value carefully — the
inner path must not itself contain unescaped spaces/quotes that confuse
`schtasks`' own parsing; simplest is to keep all paths space-free, or wrap
the restart logic in a `.cmd`/`.ps1` at a short path).

---

## 8. API rate limiting & WebSocket connection cap — R2-5 (G-9)

`internal/api/server.go` now enforces:

- **Per-client-IP token-bucket rate limiting** on every REST endpoint
  (`golang.org/x/time/rate`), default 10 requests/sec with a burst of 20.
  Exceeding it returns `429` with a `Retry-After` header (seconds). Chosen
  per-IP rather than per-token because this server currently issues a single
  shared token — per-token would collapse to an identical single bucket,
  while per-IP also throttles a client hammering the endpoint with a wrong
  token (rate limiting runs before the token check in `authMiddleware`).
  Tune via `Server.SetRateLimit(rps, burst)` before `Start()`.
- **A concurrent-connection cap on `/ws/live`**, default 5 — this is a
  personal control channel (one operator, one phone), not a public WS
  service. A connection beyond the cap is rejected with `503` **before** the
  WebSocket upgrade completes (a clean HTTP error, not a half-open socket).
  Tune via `Server.SetWSMaxConns(n)` before `Start()`.

**Config wiring needed from R2-INT** (not done here — `internal/config/config.go`
is off-limits to this package): add `api.rate_limit_rps` (float, default 10),
`api.rate_limit_burst` (int, default 20), and `api.ws_max_conns` (int,
default 5) to `APIConfig`, then call `apiServer.SetRateLimit(...)` /
`apiServer.SetWSMaxConns(...)` at the same call site that already calls
`SetBindAddr`/`SetTLS` (`cmd/main.go`, `internal/app/titan.go`). Until wired,
the built-in defaults above apply unconditionally.

---

## 9. Holiday calendar maintenance — R2-5 (G-12)

`go-engine/nse_holidays.yaml` holds the NSE trading-holiday list in a simple
`date`/`description` list. **It is not yet loaded by the engine** — see the
"R2-INT wiring needed" note in `docs/reports/R2-5-REPORT.md` for the exact
`internal/config/config.go` / `internal/engine/runner.go` change required
(both off-limits to this package this round). Until that wiring lands, the
engine still uses its own hardcoded, separately-incomplete
`nseHolidays2026` table in `runner.go`.

### Annual update procedure

1. Once NSE publishes the next calendar year's trading-holiday circular
   (typically Nov/Dec of the preceding year, at nseindia.com), copy every
   date into `go-engine/nse_holidays.yaml` under a new year's entries —
   `date: "YYYY-MM-DD"` + a short `description`.
2. Do **not** guess movable/lunar-calendar festival dates (Holi, Diwali,
   Eid, etc.) from memory or estimation — copy them from the official
   circular only. A wrong date here either blocks a real trading day or
   (worse) permits trading on a real holiday.
3. Fixed-date holidays (Republic Day Jan 26, Independence Day Aug 15, Gandhi
   Jayanti Oct 2, Christmas Dec 25) and Good Friday (computable from the
   Gregorian/Meeus Easter algorithm — Easter Sunday, minus 2 days) can be
   added confidently without waiting for the circular.
4. After updating the file, restart the engine process (no hot-reload) and
   verify the new year's dates are picked up — once wired per R2-INT's
   change, a log line at startup should confirm how many holiday entries
   were loaded and from which file; if that log line is absent, the wiring
   described above hasn't landed yet and the file is inert.
5. Before any live trading session near a holiday (movable or fixed),
   manually cross-check the date against NSE's official circular regardless
   of what this file says — same standing caution as section 3 above.
