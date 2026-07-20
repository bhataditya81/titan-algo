# Round 3 Audit — API Server, Risk Manager, Config, Web UI

**Date:** 2026-07-20
**Scope:** `go-engine/internal/api/server.go`, `go-engine/internal/risk/risk.go`,
`go-engine/internal/config/config.go`, `web-ui/{index.html,app.js,api.js,charts.js,icons.js,styles.css}`,
plus the wiring code that connects them (`go-engine/cmd/main.go`,
`go-engine/internal/engine/{engine.go,runner.go}`) where a scoped file's
behavior turns out to depend on how (or whether) it's actually called.
**Method:** read the current code (not comments/reports), traced every config
field and every `Set*` setter from declaration to call site, traced the
concurrency of the kill-switch/exit/restore paths, and re-verified the
FY 2025-26 charge-rate table against the rates it claims to implement.
**Not done:** no fixes applied. This is audit-only, per instructions.

---

## Executive summary

The single most important finding is that **the web UI / mobile control
surface's two headline safety actions behave differently than they appear
to**: changing the session balance cap through `POST /api/config` reports
`{"success": true}` but has **zero effect** on the actual risk manager once
the engine is wired the way `cmd/main.go` wires it today (Finding 1) — an
operator trying to *reduce* risk mid-session via the app has no way to know
it didn't work. Separately, the kill switch and the software stop-loss can
race each other into placing **duplicate exit orders at the broker** for the
same position (Finding 2), and a normal process restart with open positions
races the newly-started control-API server against unlocked risk-state
mutation (Finding 3). All three are money-critical and none of them require
an attacker — they reproduce under entirely legitimate operation.

Below that tier: several config knobs that read as safety controls
(`api.bind_addr`, `api.allowed_origins`, `api.cors_allowed_origins`,
`risk.session_balance_limit`) are parsed, validated, and defaulted by
`config.go` but are **never actually consulted** by any running code path,
because the integration file that was supposed to wire them
(`internal/app/titan.go`, referenced throughout `server.go`'s own doc
comments and in `docs/reports/R2-5-REPORT.md`) **does not exist anywhere in
this repository**. `cmd/main.go` is the only entry point, and it wires a
strict subset of the available setters.

The previously-fixed "ATR stop-loss silently defaults to 5%" bug (the
pattern this audit was primed to look for) is **confirmed still fixed** and
its fail-closed gate is airtight for every reachable code path — see the
"Re-verification" note near the end.

The FY 2025-26 brokerage/STT/stamp-duty/GST/SEBI charge table and its
buy/sell-side application logic were checked line by line and are correct —
no charge-calculation bug found (see the note at the end).

---

## Findings

### CRITICAL

**F1. `POST /api/config` session_balance change is a silent no-op against the real risk manager**
- **Where:** `go-engine/internal/api/server.go:859-981` (`handleConfig` /
  `handleConfigPost`), combined with `go-engine/cmd/main.go:382-410` (API
  server wiring).
- **What's wrong:** `handleConfigPost` (server.go:896-981) validates and
  stores an updated `session_balance` into the `Server` struct's own
  `s.balance` field, then calls `cfgHooks.Apply(balanceOut, strategyOut)`
  **only if `s.configHooks != nil`** (server.go:970-975). `cmd/main.go` calls
  `apiServer.SetControlHooks(...)` (line 395) but **never calls
  `apiServer.SetConfigHooks(...)`** anywhere — grep across the whole repo
  shows `SetConfigHooks` is exercised only in `server_test.go`, never in
  production wiring. So in the actual running binary, `cfgHooks` is always
  `nil`: the POST handler still writes `200 {"success": true}` (line 980),
  but `Apply` is never called and the real `risk.Manager` (`riskMgr` in
  `cmd/main.go`, which owns `InitialBalance`/`CurrentBalance`, the numbers
  `CheckRisk`/`ValidateOrder` actually enforce) is never touched.
  Worse: because `SetControlHooks` **is** wired, `handleConfig`'s GET branch
  (server.go:873-881) and `/api/status` both override every display field
  from `hooks.Status()` (the real engine state) whenever hooks are present —
  so the stored `s.balance` the POST just updated isn't even echoed back;
  it's invisible from the moment it's written.
- **Scenario:** an operator watching a losing session opens the web UI (or
  the mobile app), lowers "Session balance" from ₹50,000 to ₹10,000 to cut
  losses, and gets a success response. The live `risk.Manager`'s
  `CurrentBalance`/`InitialBalance` are unchanged; `ValidateOrder` and
  `CheckRisk` keep evaluating against the old, larger cap. The engine keeps
  taking on risk the operator believed they had just capped.
- **Severity: CRITICAL** — a documented, money-critical control silently
  does nothing, and reports success while doing it.

**F2. Kill switch / stop-loss race can submit duplicate exit orders to the broker**
- **Where:** `go-engine/internal/engine/engine.go:236-296` (`PlaceExitOrder`),
  called from `go-engine/internal/engine/runner.go:911` (`exitStructure`,
  itself invoked from the tick-loop's stop-loss scan around line 1018-1021)
  **and** from `runner.go:947` (`flattenAll`, invoked by
  `KillAndFlatten` → the `/api/kill` HTTP handler, `server.go:1064-1098`).
- **What's wrong:** `PlaceExitOrder` reads the position via
  `e.riskManager.GetOpenPositions()` (a snapshot copy, properly locked) and
  checks `exists` (engine.go:237-241), then — with **no lock held across the
  gap** — calls `e.broker.PlaceOrder(order)` (line 268), and only afterward
  calls `e.riskManager.ClosePosition(symbol, ...)` (line 288) to remove the
  position from the risk manager's map. The code even acknowledges the
  hazard directly in a comment at engine.go:290-293: *"a risk-manager-side
  bookkeeping error must not be treated as 'the exit failed' (that would
  make a caller retry a close that already happened)"* — i.e. the authors
  knew `ClosePosition` can fail after the broker call already succeeded, but
  the fix was to swallow that error rather than prevent the double call.
  Nothing in `runner.go` serializes `exitStructure`/`flattenAll` against each
  other: `r.mu` is only held briefly around the runner's own `r.open` map
  (runner.go:897-899, 919-921), never across the `PlaceExitOrder` call
  itself, and the tick-loop goroutine (`Run`'s ticker, runner.go:412-427) and
  an HTTP handler goroutine (`/api/kill` → `KillAndFlatten`,
  runner.go:373-382) run concurrently with no shared mutex over this path.
- **Scenario:** a position's software stop-loss fires in the same instant an
  operator presses the (correctly double-confirmed) kill switch — no
  attacker needed, just an ordinary bad-market moment where both triggers
  are likely to co-occur. Both goroutines see the position as open, both
  place a broker exit order for the full quantity; only one
  `ClosePosition` call succeeds, the other logs a swallowed warning. At the
  broker this can mean a doubled exit (over-selling into a naked reversed
  position) for a live derivative position — real money, real unbounded
  directional exposure created by the "safety" action itself.
- **Severity: CRITICAL** — this is exactly the "kill-switch race condition"
  category called out in scope, and it's a real, unguarded TOCTOU gap on the
  order-placement path, not a theoretical one.

**F3. Unsynchronized write to `risk.Manager` state races the just-started API server on every restart with open positions**
- **Where:** `go-engine/internal/engine/runner.go:244-276` (`RestoreState`),
  invoked as the first line of `Run` (`runner.go:413`); concurrently,
  `go-engine/cmd/main.go:411-415` starts the API server
  (`go apiServer.Start()`) **before** calling `runner.Run(ctx)` at line 422.
- **What's wrong:** `RestoreState` writes directly into
  `r.te.riskManager.OpenPositions[p.Symbol] = &risk.Position{...}`
  (runner.go:264) and `r.te.riskManager.CurrentBalance` /
  `.RealizedPnL` / `.SessionBalanceUsed` (runner.go:271-273) — all without
  acquiring `risk.Manager`'s own `mu`. Every other access to these exact
  fields elsewhere in `risk.go` (`GetSessionStatsWithPrices`,
  `GetOpenPositions`, `GetCurrentBalance`, etc.) takes `m.mu.RLock()` first.
  Because `cmd/main.go` starts the API server goroutine *before* calling
  `runner.Run` (which calls `RestoreState` as its very first action), any
  incoming `/api/status` request, the WS heartbeat ticker (every 5s,
  `server.go:1145-1169`), or a broadcast can call
  `hooks.Status() → runner.Status() → e.GetSessionStats()` — which goes
  through the mutex-protected getters — at the exact moment `RestoreState`
  is writing the same fields/map without the mutex. This is a textbook Go
  data race (unsynchronized concurrent map write vs. locked reads on the
  same map; unsynchronized read/write on the same float64 fields) on the
  balance, realized P&L, and open-position book.
- **Scenario:** any process restart while positions are open (a crash
  recovery, a planned redeploy) that receives even a single `/api/status`
  poll or has the WS heartbeat already ticking during the recovery window
  hits this. Best case: `go test -race` would catch it (worth checking — see
  Recommendation R2), but there is no existing integration test that starts
  the API server and calls `RestoreState` concurrently, so `-race` being
  clean on the current test suite does not mean this path is clean. Worst
  case in production: a `fatal error: concurrent map read and write` panic —
  unrecoverable, kills the whole process (including its open positions
  monitoring) at the single moment recovery matters most.
- **Severity: CRITICAL** — unsynchronized concurrent access to the
  money-critical fields (balance, P&L, open positions), triggered by normal
  restart behavior, not attacker input.

---

### HIGH

**F4. `risk.session_balance_limit` in `config.yaml` is parsed but never read anywhere**
- **Where:** `go-engine/internal/config/config.go:161`
  (`Risk.SessionBalanceLimit float64`); the only production consumers of a
  session limit are `go-engine/cmd/main.go:47` (`-balance` CLI flag,
  default 1000.0, further overridden by an interactive stdin prompt at
  lines 103-111) and lines 153/340 (`risk.NewManager(cfg.Risk.MaxDrawdownPercent,
  *sessionBalance, ...)` — note the second argument is `*sessionBalance`, the
  CLI flag, **not** `cfg.Risk.SessionBalanceLimit`).
- **What's wrong:** grepping the entire `go-engine` tree for
  `SessionBalanceLimit` outside its own declaration returns nothing. The
  field is unmarshalled from YAML and never consulted by `NewManager`, by
  any validation step, or by anything else. An operator editing
  `risk.session_balance_limit` in `config.yaml` — a natural place to look
  for "how much capital can this session risk" — is silently editing a
  value that does nothing; the actual cap is whatever is typed at the
  interactive `₹` prompt (or the `-balance` flag) at process start.
- **Scenario:** an operator sets `risk.session_balance_limit: 5000` intending
  a hard cap, but at startup either accepts the CLI flag's default (₹1000,
  wrong in the *other* direction) or types a different number at the
  prompt — either way the YAML value they configured is irrelevant, and
  nothing tells them so.
- **Severity: HIGH** — a money-critical config field that silently does
  nothing is exactly the class of bug this audit exists to catch, even
  though its net effect happens to be confusing rather than unsafe (the CLI
  flag/prompt path is itself validated: `cmd/main.go:107` only accepts
  `bal > 0`).

**F5. `api.bind_addr` / `api.allowed_origins` / `api.cors_allowed_origins` are parsed and defaulted, but never wired to the running server**
- **Where:** `go-engine/internal/config/config.go:86-113` (`APIConfig`);
  `go-engine/cmd/main.go:383-394` (actual server construction).
- **What's wrong:** `cmd/main.go:383` calls `api.NewServer(8080, cfg.API.Token)`
  — the port is a hardcoded literal, not `cfg.API.BindAddr`. `SetBindAddr`,
  `SetAllowedOrigins`, and `SetCORSAllowedOrigins` are defined on `*Server`
  (server.go:367-393, 399-403) and are exercised in `server_test.go`, but a
  repo-wide grep shows they are **never called from any non-test code path**.
  `docs/reports/R2-5-REPORT.md:141-142` explicitly says its new setters
  should be wired "at the same call site that already calls
  `SetBindAddr`/`SetTLS` (`cmd/main.go`, `internal/app/titan.go`, both
  R2-INT-owned)" — but `internal/app/titan.go` **does not exist anywhere in
  this repository** (confirmed via `find`), and `cmd/main.go` never calls
  `SetBindAddr` either. This means a chunk of integration work multiple
  prior reports assumed was done (or would be done in a file that was
  supposed to exist) never happened.
- **Impact:** this fails *safe*, not open — the WS Origin allowlist and CORS
  allowlist default to empty (deny all browser-origin cross-origin access),
  and the bind address defaults to `127.0.0.1:8080` either way, so there is
  no auth-bypass here. But it means: (a) an operator cannot actually change
  the bind address via config (e.g. to run behind a reverse proxy, or to
  intentionally expose it on the LAN for the phone use case the rest of the
  codebase's comments assume — see `server.go`'s extensive "mobile app"
  references), and (b) an operator cannot legitimately allow a browser
  origin to talk to the API even by configuring `allowed_origins` /
  `cors_allowed_origins` correctly — the config is decorative.
- **Severity: HIGH** — not an active vulnerability, but three security-
  relevant config surfaces are silently inert, which is a serious footgun
  the next time someone believes they've locked down (or opened up) network
  exposure by editing `config.yaml`.

**F6. `/ws/live` bypasses the per-IP rate limiter that protects every other endpoint**
- **Where:** `go-engine/internal/api/server.go:506-515` (route table) and
  `1100-1132` (`handleWebSocket`).
- **What's wrong:** every REST endpoint is registered as
  `s.authMiddleware(s.handleX)` (lines 506-514), and `authMiddleware`
  (line 612-621) explicitly composes `s.rateLimitMiddleware` around the
  token check specifically "so a client hammering the endpoint with bad
  tokens gets throttled too" (comment at line 609-611). `/ws/live` is
  registered directly — `mux.HandleFunc("/ws/live", s.handleWebSocket)`
  (line 515) — with no `rateLimitMiddleware` wrapper. `handleWebSocket` does
  its own inline token check (lines 1105-1112) and its own connection-count
  cap (`wsMaxConns`, lines 1118-1126), but neither of those throttles the
  *rate* of incoming upgrade attempts — only the count of connections that
  successfully reach `Upgrade()`.
- **Scenario:** a client (with or without a valid token) can send unlimited
  HTTP requests to `/ws/live` per second — each one costs a constant-time
  token compare, a JSON error encode, and (once at the connection cap) an
  early-return — with zero throttle, unlike literally every other endpoint
  in this file. This is precisely the "rate-limiting gap" category in
  scope; the fact that every *other* endpoint was deliberately hardened
  (G-9 in `docs/PRODUCTION_GAPS_R2.md`, closed by R2-5) makes this the one
  gap in an otherwise-closed control.
- **Severity: HIGH** — resource-exhaustion vector on the one endpoint that
  was supposed to be covered by the same hardening pass as everything else.

**F7. Web UI loads a third-party CDN script with no integrity check, on a page that stores the control-API token in `localStorage`**
- **Where:** `web-ui/index.html:226`
  (`<script src="https://unpkg.com/lightweight-charts@4.1.3/...">`, no
  `integrity=`/`crossorigin=` attributes); `web-ui/api.js:6-16` (token
  persisted in `localStorage` under `titan_api_token`, readable by any
  script executing on the page).
- **What's wrong:** the page's entire security model for the token is "it
  never leaves this browser except to the configured base URL" (the help
  text at `index.html:183` says exactly this), but that guarantee only holds
  if every script running on the page is trusted. The chart library is
  fetched from `unpkg.com` (a public CDN mirroring live npm package
  contents) with no Subresource Integrity hash pinning its exact bytes. If
  that CDN response is ever compromised, MITM'd (relevant if the panel is
  ever used over plain HTTP per F9 below), or the pinned version is ever
  yanked/republished maliciously, the injected script runs with full page
  privileges — including reading `localStorage.titan_api_token` and issuing
  authenticated `fetch()` calls to `/api/kill`, `/api/start`, or exfiltrating
  it outright.
- **Severity: HIGH** — this is the one supply-chain-shaped path by which the
  "token never leaves this browser" claim in the UI's own help text can be
  broken, on a control panel whose actions include an emergency stop and a
  live-trading toggle.

---

### MEDIUM

**F8. Ledger-derived free-form strings render into the DOM via unescaped `innerHTML`**
- **Where:** `web-ui/app.js:269-278` (`formatCell`), `284-326`
  (`renderTradesTable`, the actual `tbody.innerHTML = rows.map(...)` at
  line 313-315), and `web-ui/charts.js:83-95` (`renderTable`,
  `tr.innerHTML = cols.map(...)` at line 92). Upstream, the data being
  rendered comes from `ledger.Trade` (`go-engine/internal/ledger/ledger.go:96-115`),
  whose `Symbol`, `Note`, and `Strategy` fields are plain `string` with no
  format constraint, and `Note` is very frequently populated with raw
  `err.Error()` output — e.g. `go-engine/internal/engine/engine.go:174, 186,
  276, 284`.
- **What's wrong:** `formatCell` returns `String(value)` (or
  `JSON.stringify` for objects) with no HTML-escaping, and the caller
  concatenates that directly into an `innerHTML` template string per row/
  cell. There is no escaping step anywhere between the ledger and the DOM.
  If any upstream text (a broker error message, an instrument/symbol string
  from discovery, a strategy name) ever contains `<`, `>`, or `"`, it is
  parsed as markup by the browser instead of displayed as text.
- **Scenario:** this is a single-operator local tool, so the realistic
  threat model is narrow — but "no attacker" is exactly the class of
  wishful assumption this audit is meant to challenge. A broker API error
  message that happens to echo back malformed request content containing
  HTML metacharacters (not implausible: broker error bodies are external
  input), or a manually-typed symbol that makes it into a `Note`, would
  render as markup rather than text with zero defense in depth.
- **Severity: MEDIUM** — real, traced gap; capped below HIGH because
  exploitability requires attacker-influenceable content to already be
  flowing through a system whose other input paths (symbol validation,
  broker responses) are largely trusted/internal today.

**F9. API token travels in a URL query string and, by default, over plaintext HTTP, with no scheme enforcement when the base URL is changed**
- **Where:** `web-ui/api.js:61-71` (`connectWs` builds
  `${wsBase}/ws/live?token=${token}`), `api.js:8`
  (`DEFAULT_BASE_URL = 'http://127.0.0.1:8080'`), `web-ui/app.js:342-352`
  (`openSettings`/`saveSettings` let the operator repoint `baseUrlInput` to
  any URL with no scheme check).
- **What's wrong:** the WS token-in-query-string is effectively unavoidable
  in a browser client (the `WebSocket` constructor can't set custom
  headers), and the server explicitly supports it for that reason
  (`server.go:1105-1108`) — so this half is a known, accepted tradeoff, not
  a fresh bug. What compounds it: the settings modal lets the base URL be
  changed to anything (`type="url"`, no host/scheme restriction), and
  nothing warns if that URL is non-loopback and non-HTTPS. The codebase's
  own framing (extensive "mobile app" comments throughout `server.go`,
  `docs/PRODUCTION_GAPS_R2.md` G-14) implies this panel is meant to be
  reached from a phone on the same network, i.e. genuinely off-loopback —
  at which point the token goes out in a URL query string (loggable by any
  intermediate proxy/reverse-proxy/access log) over plain HTTP (sniffable
  on the LAN) with the UI giving no indication anything is different from
  the safe localhost case.
- **Severity: MEDIUM** — compounds with F7; on its own it's a known,
  bounded tradeoff for the localhost case, but becomes a real cleartext-
  credential-exposure issue the moment the (clearly intended) non-loopback
  use case is exercised, with no guardrail in the UI.

**F10. `risk.max_orders_per_min` / `risk.max_drawdown_percent` bounds validation is inconsistent and one direction is a silent, undocumented 5x relaxation**
- **Where:** `go-engine/internal/config/config.go:333-335`
  (`if config.Risk.MaxOrdersPerMin == 0 { config.Risk.MaxOrdersPerMin = 20 }`)
  vs. `go-engine/internal/risk/risk.go:342-345`
  (`func NewManager(...) { if maxOrdersPerMin <= 0 { maxOrdersPerMin = 100 } ... }`);
  `risk.MaxDrawdownPercent` has **no** default-assignment anywhere in
  `config.go`'s `parse()` (contrast with the explicit defaults block at
  lines 315-335 that covers `Engine.*` and `Risk.MaxOrdersPerMin` but not
  `Risk.MaxDrawdownPercent`), and is read directly with no bounds check at
  `risk.go:884` (`if drawdownPercent >= m.MaxDrawdownPercent`).
- **What's wrong (two distinct issues):**
  1. `config.go` only special-cases the *exact* value `0` for
     `MaxOrdersPerMin`. A negative value (a plausible typo, e.g. `-1`) is
     **not** caught by config's own default logic and flows unchanged into
     `risk.NewManager`, which then applies its *own*, different, silent
     fallback of **100** orders/min — 5x looser than config's documented
     default of 20 — with no log line, no error, nothing visible to the
     operator. Two independent silent defaults for the same field, in two
     different files, disagreeing on the fallback value.
  2. `MaxDrawdownPercent` has no default in `config.go` at all. Omitting
     `risk.max_drawdown_percent` from `config.yaml` silently yields Go's
     zero value `0.0`. `CheckRisk` (risk.go:882-888) then computes
     `drawdownPercent >= m.MaxDrawdownPercent` — with `MaxDrawdownPercent ==
     0.0`, this trips as soon as `CurrentBalance` dips even fractionally
     below `InitialBalance` (i.e., on the very first losing tick), silently
     halting/flattening the session with a "max drawdown reached (0.0X% >=
     0.00%)" message that gives no hint the real cause is a missing config
     value rather than an actual 0%-tolerance policy someone intended.
- **Scenario:** (1) an operator who mistypes `max_orders_per_min: -1`
  (perhaps intending "disable throttling", a reasonable but wrong guess)
  silently gets a 100/min cap instead of an error or the documented
  20/min default. (2) an operator who forgets to set
  `max_drawdown_percent` in a new `config.yaml` gets a session that
  self-halts on the first tick with a loss of any size, with no error at
  load time (unlike the stop-loss-type fail-closed check right next to it
  at `config.go:364-376`, which *does* validate and error at load).
- **Severity: MEDIUM** — neither direction creates unsafe (fail-open)
  behavior in the reachable range (both end up more conservative, not
  less, except for the 20→100 orders/min case which *is* a real,
  undocumented loosening), but both are exactly the "no validation of sane
  bounds on a money/risk field" pattern called out in scope, and the
  disagreement between config.go's and risk.go's defaults for the same
  field is a maintenance hazard in its own right.

---

## Re-verification: the previously-fixed ATR/stop-loss default

`risk.go:897-924` (`calculateStopLossPrice`) still contains a `default:`
branch that falls back to a hardcoded 5% stop
(`entryPrice * 0.95` / `* 1.05`) for any `StopLossConfig.Type` other than
`"percentage"`/`"points"` — structurally the same shape as the original
bug. However:

- `config.go:364-376` fails closed at load time for exactly this: any
  `risk.stop_loss.type` other than `"percentage"`/`"points"` (when
  `stop_loss.enabled` is true) makes `config.Load` return an error, so the
  process never starts trading with an invalid type.
- Both production call sites (`cmd/main.go:153-161` and `:340-348`) build
  `risk.StopLossConfig` exclusively from `cfg.Risk.StopLoss.*`, i.e. always
  through that validated path.
- A repo-wide grep for `risk.Manager{` (direct struct-literal construction,
  which would bypass `NewManager`/`config.Load` entirely) finds **zero**
  matches outside test files.

**Conclusion: the fix holds for every reachable production path.** The
`default:` branch is truly unreachable in `cmd/main.go`'s wiring today; it's
only live for direct/test construction, matching its own comment. No
residual issue here — flagging this only because the brief asked to
specifically re-check it.

## Re-verification: FY 2025-26 charge calculation

`EstimateChargesWithRates` (`risk.go:178-241`) and `DefaultChargeRates`
(`risk.go:129-151`) were checked line by line:

- STT: 0.1% options-sell, 0.02% futures-sell (both match the October 2024
  STT revision), 0.025% equity-intraday-sell, 0.1% equity-delivery-both-
  sides — correct, and correctly gated to the sell leg only where the real
  rule is sell-side-only (note: this is charged on whichever leg is
  literally a `Sell` — a short entry correctly attracts STT at entry, not
  at exit, matching how STT actually works).
- Exchange transaction charges (0.03503% options / 0.00173% futures /
  0.00297% equity), stamp duty (buy-side only, 0.003%/0.002%/0.015%/0.003%),
  SEBI fee (0.0001%, i.e. ₹10/crore), and GST (18%, applied to
  brokerage + exchange txn + SEBI fee only, never to STT/stamp — correct
  per Indian tax treatment) all check out against current published rates.
- Brokerage: flat ₹20/order for F&O, `min(₹20, 0.03% turnover)` for
  equity — matches the standard discount-broker model this codebase targets.

**No charge-calculation bug found.** This area appears to have already
received careful, correct attention (consistent with `risk.go`'s own
"single source of truth" framing and the EX-4/EX-9 fix history referenced
in its comments).

---

## Recommendations (not applied — audit only)

- **R1 (F1):** either wire `SetConfigHooks` in `cmd/main.go` so
  `session_balance` changes actually reach `riskMgr`, or make
  `POST /api/config` return an explicit error/no-op status when no
  `ConfigHooks.Apply` is wired — never `200 success` for a change that did
  nothing.
- **R2 (F2, F3):** add a single mutex (or reuse `risk.Manager`'s own) around
  the "check open → place broker order → close position" sequence in
  `PlaceExitOrder`, keyed per symbol; move `RestoreState()` before the API
  server goroutine is started in `cmd/main.go`, or hold `riskManager`'s lock
  for the whole restore. Add an integration test that starts the API server
  and calls `RestoreState` concurrently under `-race` to catch regressions.
- **R3 (F4, F5):** either delete the dead config fields
  (`risk.session_balance_limit`, and reconsider whether
  `allowed_origins`/`cors_allowed_origins`/`bind_addr` are still wanted) or
  actually wire them from `cmd/main.go`. `internal/app/titan.go` should
  either be created (if the mobile entry point is still intended) or every
  reference to it in comments/reports should be corrected to stop implying
  it exists.
- **R4 (F6):** wrap `/ws/live`'s registration in the same
  `rateLimitMiddleware` (skip only the token-gate half, which
  `handleWebSocket` already does itself).
- **R5 (F7, F9):** add an SRI hash to the CDN `<script>` tag (or vendor the
  library locally, avoiding the CDN dependency entirely); add a UI warning
  when the configured base URL is non-loopback and not `https://`.
- **R6 (F8):** escape ledger-derived string fields before inserting into
  `innerHTML` (or switch these render paths to `textContent`/DOM node
  construction instead of template-string `innerHTML`).
- **R7 (F10):** make `config.go`'s bounds check for `MaxOrdersPerMin`
  `<= 0` (not `== 0`) so it never silently disagrees with `risk.NewManager`'s
  own fallback; add an explicit default (and/or a validation error) for
  `MaxDrawdownPercent` so an omitted value doesn't silently produce a
  0%-tolerance session.
