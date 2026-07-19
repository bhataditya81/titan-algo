# R2-5 — Ops: Watchdog, Rate Limit, Dashboard, Holidays, Mobile Assessment — Report

**Gaps closed:** G-8 (standalone watchdog), G-9 (API rate limiting + WS
connection cap), G-10 (dashboard CSV parsing/discovery), G-12 (holiday
calendar as a data file), G-14 (mobile client vs hardened API — assessed,
partially fixed, rest parked).

## Files touched

- `go-engine/cmd/watchdog/main.go` (new)
- `go-engine/cmd/watchdog/main_test.go` (new)
- `go-engine/internal/api/server.go` (rate limiting + WS connection cap added)
- `go-engine/internal/api/server_test.go` (tests added for both, alongside WP-4's existing tests)
- `go-engine/nse_holidays.yaml` (new)
- `py-brain/dashboard/app.py` (CSV file discovery + header-driven/defensive parsing)
- `mobile-app/www/app.js` (WS token fix)
- `mobile-app/android/app/src/main/assets/www/app.js` (same fix — see "two copies" note below)
- `mobile-app/COMPAT.md` (new — parked larger mobile gap)
- `docs/RUNBOOK.md` (appended sections 7, 8, 9 — no existing section edited)

No file outside this list was touched. `go-engine/internal/engine/runner.go`,
`go-engine/cmd/main.go`, `go-engine/internal/config/config.go`, and
`go-engine/mobile/titanmobile.go` were read but not edited (all off-limits
this round) — every place a change to one of them is required is called out
explicitly below and in RUNBOOK.md/COMPAT.md.

---

## 1. Watchdog (`cmd/watchdog`) — G-8

Standalone binary, imports nothing from `internal/engine`/`internal/app`, so
it keeps running even if the whole engine process/module is dead.

**Behavior:** polls the heartbeat file's mtime on `-poll` (default 15s).
Stale = age > `-max-age` (default 60s) **or file missing entirely**. Alerts
exactly once per breach episode (tracked via an `alerted bool`, reset when
the file becomes fresh again) — repeated polls while still stale only log a
low-volume status line, never re-alert. Optional `-restart-cmd` runs a
command on the first detection of a breach, gated by `-restart-cooldown`
(default 10m) so a flapping heartbeat can't restart-loop the engine (the
cooldown is checked in addition to the once-per-episode alert logic, since
the restart call site is naturally also once-per-episode — the cooldown adds
protection *across* episodes too).

Telegram alerting reuses `cmd/main.go`'s exact `TITAN_TG_TOKEN`/`TITAN_TG_CHAT`
env-var convention and Bot API call shape, so one env-var setup covers both
processes. `alert == nil` (either var unset) is a documented no-op, matching
`engine.AlertFunc`'s convention.

### Build / vet / test

```
$ go build ./cmd/watchdog/... && go vet ./cmd/watchdog/...
(no output — clean)

$ go test -race ./cmd/watchdog/... -v
--- PASS: TestWatchdogAlertsOncePerBreachEpisode
--- PASS: TestWatchdogMissingHeartbeatFileIsStale
--- PASS: TestWatchdogNoOpAlertWhenNil
--- PASS: TestWatchdogRestartCmdCooldown
--- PASS: TestWatchdogRestartCmdNotConfiguredIsNoOp
ok  	titan-algo/cmd/watchdog	3.306s
```

`TestWatchdogRestartCmdCooldown` injects a fake `runCmd` (no real process
spawn) and proves: 1st breach → 1 restart attempt; still-stale polls in the
same episode → no additional attempt (the call site itself is
once-per-episode); a 2nd episode within the cooldown window → still no
attempt; a 3rd episode after the cooldown is manually expired → attempt #2.

### Manual demonstration (acceptance criteria)

Built `watchdog.exe`, pointed it at a fake heartbeat file in the scratch
directory with `-max-age 2s -poll 1s`, no `TITAN_TG_TOKEN`/`TITAN_TG_CHAT`
set:

```
2026/07/19 17:14:59 titan-watchdog: monitoring "...\scratchpad/heartbeat" (max-age=2s poll=1s restart-cmd="" restart-cooldown=10m0s)
2026/07/19 17:14:59 titan-watchdog: TITAN_TG_TOKEN/TITAN_TG_CHAT not set — Telegram alerts are a no-op, only log lines will fire
2026/07/19 17:15:00 STALE HEARTBEAT: heartbeat file ...\heartbeat is 3s old (max-age 2s) — engine appears stuck or dead
2026/07/19 17:15:00 (alert suppressed, no TITAN_TG_TOKEN/TITAN_TG_CHAT: [heartbeat_stale] heartbeat file ...\heartbeat is 3s old (max-age 2s) — engine appears stuck or dead)
2026/07/19 17:15:01 heartbeat still stale (age=4s) — already alerted for this episode, not re-alerting
...
2026/07/19 17:15:08 heartbeat recovered (age now 0s)
2026/07/19 17:15:08 (alert suppressed, no TITAN_TG_TOKEN/TITAN_TG_CHAT: [heartbeat_recovered] heartbeat file ...\heartbeat is fresh again (age 0s))
2026/07/19 17:15:10 STALE HEARTBEAT: heartbeat file ...\heartbeat is 2s old (max-age 2s) — engine appears stuck or dead
2026/07/19 17:15:10 (alert suppressed, no TITAN_TG_TOKEN/TITAN_TG_CHAT: [heartbeat_stale] ...)
```

Confirms: staleness detection fires exactly once per breach, quietly repeats
while still stale, detects recovery when the file is touched again, and
correctly no-ops the Telegram call with no env vars set (matching the main
engine's convention). Process was then killed and the demo binary removed;
nothing was left running.

### Windows Task Scheduler

Documented in `docs/RUNBOOK.md` §7: an `ONLOGON`-triggered `schtasks /Create`
one-liner (the watchdog is a long-running poller, not a one-shot script, so
it's scheduled to start once and keep running, not re-triggered
periodically), plus query/delete commands and a note on how `-restart-cmd`
composes with the scheduled task line.

---

## 2. Rate limiting + WS connection cap (`internal/api/server.go`) — G-9

- **Per-client-IP token bucket** (`golang.org/x/time/rate`, already a direct
  dependency — no `go.mod` change needed) wraps every REST endpoint via
  `authMiddleware`. Chosen **per-IP, not per-token**, documented in-code and
  in RUNBOOK §8: this server currently issues one shared token, so per-token
  would collapse to the same single bucket anyway, while per-IP also
  throttles a client hammering the endpoint with a *wrong* token (the rate
  check runs **before** the token check, specifically so brute-forcing bad
  tokens is throttled too, not just valid traffic).
  - Defaults: `DefaultRateLimitRPS = 10`, `DefaultRateLimitBurst = 20`.
  - Breach → `429` with a `Retry-After` header (seconds, computed from the
    limiter's configured rate).
  - Tunable via new `Server.SetRateLimit(rps, burst)`, called before `Start()`
    (same pattern as the existing `SetBindAddr`/`SetTLS`).
  - `ponytail:` the per-IP limiter map (`rateLimiters map[string]*rate.Limiter`)
    grows unboundedly with distinct IPs and is never evicted — fine for a
    personal API with a handful of LAN/phone clients; add an LRU/eviction
    policy if this ever faces many distinct source IPs.
- **WS connection cap** on `/ws/live`: default `DefaultWSMaxConns = 5`.
  Checked **before** `Upgrader.Upgrade()` so a connection beyond the cap gets
  a clean `503` JSON error, not a half-open WebSocket handshake. Tunable via
  new `Server.SetWSMaxConns(n)`.

### Config wiring needed from R2-INT (not done here — `config.go` off-limits)

Add to `internal/config.APIConfig` (`go-engine/internal/config/config.go`,
next to the existing `BindAddr`/`Token`/`TLSCertFile` fields):

```go
RateLimitRPS   float64 `yaml:"rate_limit_rps"`   // default 10 if <= 0
RateLimitBurst int     `yaml:"rate_limit_burst"` // default 20 if <= 0
WSMaxConns     int     `yaml:"ws_max_conns"`      // default 5 if <= 0
```

Then at the same call site that already calls `SetBindAddr`/`SetTLS`
(`cmd/main.go`, `internal/app/titan.go`, both R2-INT-owned):

```go
apiServer.SetRateLimit(cfg.API.RateLimitRPS, cfg.API.RateLimitBurst)
apiServer.SetWSMaxConns(cfg.API.WSMaxConns)
```

Until wired, the built-in defaults (10 rps / burst 20 / 5 WS conns) apply
unconditionally — safe defaults for a personal control API, not a blocker.

### Build / vet / test

```
$ go build ./internal/api/... && go vet ./internal/api/...
(clean)

$ go test -race ./internal/api/... -v
... (all 24 previous WP-4 tests still pass, plus:)
--- PASS: TestRateLimitReturns429WithRetryAfter
--- PASS: TestRateLimitRejectsBadTokenTooAfterBurst
--- PASS: TestRateLimitIsPerIP
--- PASS: TestWebSocketConnectionCapRejectsBeyondLimit
ok  	titan-algo/internal/api	4.077s
```

`TestRateLimitRejectsBadTokenTooAfterBurst` specifically proves the ordering
decision above (rate limit before auth): two requests with a **wrong**
token, burst=1 → first is 401, second is 429, not another 401.

Whole-module sanity: `go build ./...`, `go vet ./...`, `go test -race ./...`
all clean/green after these changes (all packages, not just `internal/api`
and `cmd/watchdog`).

---

## 3. Dashboard (`py-brain/dashboard/app.py`) — G-10

**Root-cause finding, not just the symptom named in the gap report:** the
gap description worried about *column* parsing breaking on the 10→12-column
change. In fact `pd.read_csv()` (already used) parses by **header name**,
not position or column count — both the legacy 10-column format and the
current 12-column format (`OrderID`, `Status` appended, exact column list
confirmed against `docs/reports/WP-5-REPORT.md`) were already handled
correctly by the existing code, since every column access in the file
(`df['RiskBalance']`, `df['Action']`, etc.) is by name and the 12-column
format is a strict superset of the columns the dashboard actually reads.

**The real, bigger bug:** `internal/logger/csv_logger.go` (WP-5) writes
**date-stamped files** — `trades_YYYY-MM-DD.csv`, one per trading day — but
the dashboard hardcoded `LOGS_DIR / "trades.csv"` and
`LOGS_DIR / "live/trades.csv"`. Those exact filenames are **never created**
by the current logger, so the dashboard's `CSV_PATH.exists()` check always
failed and it permanently showed "No trading data available", regardless of
column format. This is the actual, currently-active defect; fixed both:

1. **File discovery** (`find_latest_trades_csv()`, new): globs
   `trades_*.csv` in the target directory (falling back to a legacy
   `trades.csv` for any old log directory that predates WP-5), returns the
   most recently modified match. Applied separately to the paper and live
   log directories, same live-preferred-over-paper precedence as before.
2. **Defensive column handling** (`REQUIRED_COLUMNS`/`OPTIONAL_COLUMNS` +
   updated `load_data()`): the 10 columns every format has in common are
   required (missing any → a clear `st.error` and "no data", never a raw
   `KeyError`/crash); `OrderID`/`Status` are optional and backfilled as
   empty strings when absent (legacy files) so any current or future code
   referencing them can't `KeyError` on an old file.

### Verification

**Attempted a real runtime test** (`streamlit run` against hand-crafted
10-column and 12-column fixture CSVs in the scratch directory) and hit a
**pre-existing, unrelated environment defect**: this machine's installed
`numpy`/`pandas` build is explicitly flagged by numpy itself at import time
as "Built with MINGW-W64 on Windows 64 bits... CRASHES ARE TO BE EXPECTED",
and reproducibly segfaults (confirmed via both the Bash tool's and
PowerShell's `python`) on **any** DataFrame operation — even a bare two-line
`pd.read_csv()` with no relation to this change. This is a local Python
environment issue, not a defect in the diff (it would crash identically on
the pre-existing code, on any CSV, in any format). `streamlit run` itself
did start cleanly against the new-format fixture (`HTTP 200`,
`/_stcore/health` → `ok`, no traceback in ~8s of log output) before it
appears to have hit the same underlying environment crash once a client
connected — consistent with the same numpy defect, not a code path unique to
this change.

Given the acceptance criteria explicitly allows "read the code path... if no
test runner is set up" as a fallback, verification here is by careful code
tracing plus hand-computed expected output for both fixture formats:

- **New format** (`trades_2026-07-19.csv`, 12 columns, 2 rows, `OrderID`
  `ORD123`/`ORD124`, `Status` `filled`/`filled`): `find_latest_trades_csv`
  matches the `trades_*.csv` glob, `pd.read_csv` parses all 12 named
  columns, `REQUIRED_COLUMNS` check passes (all 10 present),
  `OrderID`/`Status` pass through unchanged. Expected: 2 rows, last `NetPnL`
  = 250.
- **Old format** (`trades.csv`, 10 columns, 2 rows): `find_latest_trades_csv`
  falls back to the legacy-filename branch, `pd.read_csv` parses the 10
  present columns, `REQUIRED_COLUMNS` check passes, `OrderID`/`Status` are
  absent from the file and get backfilled as `""` by the loader (verified by
  reading the code: `for col in OPTIONAL_COLUMNS: if col not in df.columns: df[col] = ""`).
  Expected: 2 rows, last `NetPnL` = -450, `OrderID`/`Status` = `""`/`""`.
- **Missing log directory:** `find_latest_trades_csv` returns `None`
  (`base_dir.is_dir()` guard) → dashboard's existing "no data" message path,
  no exception.
- **A file missing a required column:** `REQUIRED_COLUMNS` check catches it
  and calls `st.error(...)` with the exact missing column names, returning
  `None` — never a raw `KeyError` propagating out of `load_data()`.

No pytest/test infra exists for this dashboard (confirmed: no `test_*.py`,
no pytest in `requirements.txt`) and the acceptance criteria explicitly says
not to stand up new test infra just for this — a fixture-based pytest file
was not added given the environment's inability to actually execute pandas
code here; the scratch verification script (traced logic above) is
preserved at
`C:\Users\bhata\AppData\Local\Temp\claude\...\scratchpad\verify_dashboard_csv.py`
for reference (not committed — scratch-only) and documents the same
assertions if the pandas segfault is a one-off local artifact and it's later
runnable in a working environment (e.g. CI, or this machine after a numpy
reinstall).

**Follow-up noted per the task's own allowance** ("if reading the state DB
is a larger change than fits this task, do the header-driven CSV fix as the
minimum and note the DB-read improvement as a follow-up"): the "Open
Positions" metric still uses the BUY-vs-SELL CSV row-counting heuristic
(`app.py`, "Open Positions" `st.metric` block) rather than reading
`data/titan_state.db` (`internal/state.Store.ListOpenPositions()`, read-only
SQLite, schema per `docs/reports/WP-3-REPORT.md`). This is a real
improvement opportunity flagged, not silently dropped — the file-discovery
and column-safety fixes above were the higher-priority, currently-broken
issue and are the ones actually fixed this round.

---

## 4. Holiday calendar (`go-engine/nse_holidays.yaml`) — G-12

New file at `go-engine/nse_holidays.yaml` (not `go-engine/data/`, which is
gitignored runtime data — correct per the plan's explicit note). Format: a
top-level `holidays:` list of `{date, description}` entries.

**Populated with confidence:** the 4 fixed national holidays already trusted
in `runner.go`'s `nseHolidays2026` table (Jan 26, Aug 15, Oct 2, Dec 25),
plus **Good Friday, 2026-04-03** — computed deterministically via the
Gregorian/Meeus Easter algorithm (Easter Sunday 2026 = 2026-04-05), not
looked up or guessed.

**Deliberately not populated:** movable, lunar/moon-sighting-calendar
festival holidays (Mahashivratri, Holi, Ram Navami, Mahavir Jayanti,
Id-Ul-Fitr, Bakri Id, Ganesh Chaturthi, Muharram, Dussehra, Diwali, Gurunanak
Jayanti, etc.) and all of 2027. Fabricating a specific date for a
moon-sighting-dependent festival without the actual NSE circular in hand
would be **worse** than the existing gap — a wrong date silently permits
trading on a real holiday or blocks a real trading day. The file's header
comment says this explicitly and points to the RUNBOOK procedure. This
mirrors the standing caveat already attached to `runner.go`'s own
`nseHolidays2026` table (same honesty standard, not a new gap).

### Wiring needed from R2-INT (not done here — both files off-limits)

1. `internal/config/config.go`: add a `HolidayFile string` field
   (`yaml:"holiday_file"`, default `"nse_holidays.yaml"`) to `Config` (or a
   sub-struct — either is fine, no existing convention forces one).
2. `internal/engine/runner.go`: at `Runner` construction (or lazily on first
   `marketState()` call), load `cfg.HolidayFile` (simple YAML → `map[string]bool`
   keyed by `"2006-01-02"`, same shape as `nseHolidays2026`) and use it
   **instead of, or merged with,** the hardcoded `nseHolidays2026` map in
   `marketState()`'s `if nseHolidays2026[nowIST.Format("2006-01-02")]` check
   (`runner.go` around line 506). Recommend: load-file-replaces-hardcoded
   (simplest — the YAML file becomes the one source of truth going forward)
   rather than a fragile OR-merge of two tables; fail-open on load error
   (log loudly, fall back to the existing hardcoded table) rather than
   fail-closed here, since a missing/malformed holiday file should not by
   itself prevent the engine from starting.

RUNBOOK.md §9 documents the annual-update procedure (what's safe to add from
memory/computation vs. what must come from the official circular only).

---

## 5. Mobile assessment (`mobile-app/**`) — G-14

Read in full: `mobile-app/www/{index.html,app.js,style.css}`,
`mobile-app/android/app/src/main/assets/www/*` (a **separate, duplicated
copy** — not a symlink; confirmed via `diff`), `mobile-app/android/app/src/main/java/com/titanalgo/mobile/MainActivity.kt`,
`mobile-app/README.md`, `mobile-app/BUILD_INSTRUCTIONS.md`,
`go-engine/mobile/titanmobile.go` (read-only — off-limits to edit).

**Architecture:** this is a *self-sufficient* app — `gomobile bind` compiles
the whole Go engine into the APK; `MainActivity.kt` starts the embedded
engine in-process (its own `internal/api.Server` on `localhost:8080`,
same-device) and loads `www/index.html`/`app.js` from
`file:///android_asset/www/` into a plain `WebView`, which then talks to
that same-device server. There is no separate desktop/PC server in this
deployment mode.

### Fixed directly (small, applied — file:line references)

**Bug:** `connect()` built the WebSocket URL as `.../ws/live` with no token
at all. The hardened server (`go-engine/internal/api/server.go`,
`handleWebSocket`) requires `?token=` or `X-API-Key` before it will upgrade
— every WS connection got `401`, so live push updates never worked (silent
reconnect-loop). REST calls were fine — `api()` already sent `X-API-Key`
correctly.

**Fixed in both copies** (kept in sync — `MainActivity.kt:55` loads the
`android/app/src/main/assets/www/` copy, not `mobile-app/www/`, so both had
to change or the fix would never reach the built APK):
- `mobile-app/www/app.js:248-256`
- `mobile-app/android/app/src/main/assets/www/app.js:248-256`

```js
const wsUrl = CONFIG.serverUrl.replace('http', 'ws') + '/ws/live?token=' + encodeURIComponent(CONFIG.apiKey);
```

Reuses the existing `CONFIG.apiKey` (already sourced from the existing
Settings-modal `#api-key` input, `index.html:146`) — no new UI needed for
this part.

### Parked (larger — requires a `go-engine/mobile/titanmobile.go` change, out of this package's ownership; full detail with file:line refs in `mobile-app/COMPAT.md`)

The fix above can't be exercised out-of-the-box in the actual self-sufficient
deployment, because:

1. `internal/api.NewServer` generates a **fresh random 64-char token every
   process start** when none is supplied, printed exactly once via
   `fmt.Println` — not `log.Printf`. `titanmobile.go`'s `Start()` redirects
   only the `log` package's output to `titan_mobile.log`
   (`log.SetOutput(logFile)`); the `fmt.Println` token banner is **not**
   captured, and Gomobile has no visible stdout on Android. Net effect: the
   token is regenerated every launch and never surfaces anywhere the
   operator can read it to type into the Settings modal — REST/WS auth can
   never succeed out-of-the-box in this mode.
2. `MainActivity.kt:24` passes a hardcoded literal, `"titan-mobile-secret"`,
   into `Mobile.start(dataDir, apiKey)` — `titanmobile.go` maps that
   argument to `cfg.Brokers.Angel.APIKey` (the **broker** credential, not the
   API server's token). This is a stale pre-WP-4 leftover; its own inline
   comment ("titan-mobile-secret matches the API key in app.js") is now
   false, since `app.js` no longer hardcodes that string anywhere
   (WP-4 removed it — confirmed via `grep -rn "titan-mobile-secret" mobile-app/`,
   no matches). No live-trading blast radius today (mobile always forces
   `PAPER` mode per `titanmobile.go:46`), but it's dead/misleading wiring.
3. **What a real fix needs** (sketched in `COMPAT.md`, not applied):
   `titanmobile.go`'s `Start` should generate-once-and-persist a stable API
   token under `dataDir` and return/expose it, and `MainActivity.kt` should
   inject it into the WebView's `localStorage` (e.g. via
   `webView.evaluateJavascript(...)`) right after `Mobile.start()` returns,
   so the Settings modal is pre-filled instead of requiring the operator to
   transcribe a token they currently have no way to see.

Full detail, exact file:line references, and the sketch above are in
`mobile-app/COMPAT.md` (new file, this package).

---

## Acceptance criteria — final check

- `cd go-engine && go build ./cmd/watchdog/... ./internal/api/...` — clean.
- `go vet` (same packages) — clean.
- `go test -race ./internal/api/...` and `go test -race ./cmd/watchdog/...` — all pass (see evidence above).
- Whole-module `go build ./...`, `go vet ./...`, `go test -race ./...` — all green (no regressions in any other package).
- Dashboard: manually verified via code-path tracing + hand-computed fixture expectations (real `pandas` execution blocked by a pre-existing, unrelated local numpy/pandas segfault, documented above); no test infra existed to extend, none added per the task's own guidance.
- Watchdog: built and run against a real fake heartbeat file; log output above shows stale-once detection, quiet repeats, recovery detection, and a correct Telegram no-op with no env vars set.
