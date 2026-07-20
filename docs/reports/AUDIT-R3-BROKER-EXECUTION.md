# Round 3 Audit — Broker, Execution Engine, Discovery

**Date:** 2026-07-20
**Scope:** `go-engine/internal/broker/` (angel_broker.go, instruments.go, historical.go, ws_feed.go, live_paper_broker.go, broker.go), `go-engine/internal/engine/` (engine.go, runner.go), `go-engine/internal/discovery/` (discovery.go).
**Method:** Read the current code line-by-line (not comments/prior reports taken on faith), cross-checked against `internal/risk/risk.go` where the broker/engine boundary required it, read every `*_test.go` in scope to see what's actually exercised, and traced call graphs from `cmd/main.go` where needed to confirm real wiring (not assumed wiring). No files were modified — audit only, per instructions.

**Context:** This codebase has already been through two hardening rounds (Round 1 remediation, Round 2 work packages R2-1..R2-6/INT) and the code is visibly disciplined — fail-closed patterns, sentinel errors, lock-discipline comments, and prior-bug callouts (CR-5, CR-6, CR-7, EX-*, G-*) are everywhere. Most of what a first-pass audit would normally flag has already been fixed. The findings below are the gaps that survived that scrutiny: mostly places where a fix landed at one layer but not the layer that actually matters, or where a documented design tradeoff has a sharper edge than its comment admits.

---

## Findings, ranked by severity

### F1 — CRITICAL: Partial-fill quantity is fixed in `angel_broker.go` but the bug recurs one layer up in `engine.go` — risk manager's position book still uses the *requested*, not *filled*, quantity

**Files:** `go-engine/internal/engine/engine.go:115-224` (`PlaceEntryOrder`), `go-engine/internal/risk/risk.go:550-573` (`OpenPosition`/`OpenPositionWithMargin`), `:641-690` (`UpdatePositionPrice`).

Round 1's CR-6 ("Partial fills completely ignored... Buy 75, fill 25 → book says 75; the eventual close sells 75 → net short 50 naked options") was fixed correctly at the `AngelBroker` layer: `FilledOrder.Quantity` is the actual `filledshares` value, `angel_broker.go`'s own `a.positions` map is updated with the real fill quantity (`updatePosition`, verified via `TestPlaceOrder_PartialFill_RecordsActualQty` and `TestUpdatePosition_PartialReduce_KeepsSameSide` in `angel_broker_test.go`), and `FilledOrder.Partial()` correctly reports it.

But `engine.go`'s `TradingEngine` keeps a **second, independent** position book — `risk.Manager.OpenPositions` — and that book never learns the corrected quantity:

```go
// engine.go PlaceEntryOrder, BEFORE the order is even placed:
e.riskManager.OpenPosition(symbol, price, quantity, tradeType, convertToRiskSide(side))
//                                  ^^^^^^^^ the REQUESTED quantity

order := broker.Order{Symbol: symbol, Quantity: quantity, ...}
filled, err := e.broker.PlaceOrder(order)
...
// Success (full or partial fill) — CR-6: use the ACTUAL filled quantity.
e.riskManager.UpdatePositionPrice(symbol, filled.FillPrice)
//             ^^^^^^^^^^^^^^^^^^ only corrects PRICE, never touches Quantity
```

`risk.Manager.OpenPosition`/`OpenPositionWithMargin` store `quantity` (the pre-fill requested value) into `Position.Quantity` and nothing in `risk.go` ever mutates `Position.Quantity` afterward (confirmed by grep — no assignment to `.Quantity` exists anywhere else in the file). `UpdatePositionPrice` (called right after a successful/partial fill, its own comment literally invokes "CR-6") only recomputes `EntryPrice`, `PeakPrice`, `StopLossPrice`, and locked-capital/charges — quantity is left at the original requested value.

**Concrete failure scenario:** Strategy enters SELL 75 (1 lot) of a NIFTY option as part of `short_straddle`. Angel fills only 25 (partial, e.g. thin book at market). `filled.Quantity=25`, `filled.RequestedQty=75`, `filled.Partial()==true` — logged correctly, state-store `SavePosition` correctly records `Quantity: filled.Quantity` (25, correct). But `risk.Manager.OpenPositions["SYMBOL"].Quantity` is still 75. Later, on exit (software SL, risk breach, or normal signal), `PlaceExitOrder` reads `position.Quantity` (=75, wrong) and submits a BUY 75 to close what is actually a 25-lot short. The result is exactly CR-6's original scenario recurring at a different layer: a naked **long 50** (or short 50, depending on side) position is opened by the "closing" order, unbounded exposure, invisible until the broker app is checked. The state-store's copy of the position is correct, but nothing reads the state store mid-session to correct the live risk-manager number — it's only used at `RestoreState()` after a **process restart**, which is not a practical mitigation for an intraday position.

This is the same bug shape the CR-6 fix comment claims is closed; it isn't, because the fix landed on `AngelBroker`'s own bookkeeping and not on the risk manager's, which is what actually sizes the exit order.

**Coverage gap confirming this is untested:** grep for `Partial`/`RequestedQty` in `internal/engine/` returns only the two `filled.Partial()` reads used for logging/status — there is no test anywhere in `internal/engine` that opens a position with a partial fill and then asserts the risk manager's or exit order's quantity. `runner_smoke_test.go` explicitly opts OUT of the realistic (partial-fill-capable) paper fill model specifically to avoid this nondeterminism ("R2-4's realistic paper-fill defaults (partial fills / rejections) are nondeterministic by design — this smoke test... needs the deterministic legacy fill model"), which means the one smoke test that exercises the full entry→exit path never exercises a partial fill at all.

**Fix direction (not applied — audit only):** `PlaceEntryOrder` needs a `risk.Manager` method that corrects `Position.Quantity` (and re-derives `LockedCapital`/margin proportionally) to `filled.Quantity` after a successful fill, mirroring what `UpdatePositionPrice` already does for price. Until then, any partial fill on a real order silently desyncs the two books for the rest of the session.

---

### F2 — HIGH: `LivePaperBroker` doesn't implement `ExtendedTradeService`, which silently disables stale-price protection AND broker-health monitoring during "live data + paper execution" mode

**Files:** `go-engine/internal/broker/live_paper_broker.go` (whole file), `go-engine/internal/engine/runner.go:1027-1040` (`priceWithAge`), `go-engine/internal/engine/engine.go:74-81` (`BrokerHealthy`).

`cmd/main.go:131-136` wires a real, fully-hardened `*AngelBroker` (which implements `ExtendedTradeService` — staleness tracking, `Healthy()`/`HealthError()`, the works) as the data source inside `LivePaperBroker`, specifically for the "paper trading against real market data" mode operators are expected to use for the 1-month paper-trading go-live gate. But `LivePaperBroker` only implements the plain `TradeService` interface and does not forward `GetCurrentPriceWithAge`, `Healthy`, or `HealthError` from its embedded `liveBroker`, even though the embedded broker actually has correct, real answers for all three.

Two consequences, both in `runner.go`/`engine.go` via `broker.(ExtendedTradeService)` type assertions that fail for `*LivePaperBroker`:

1. `priceWithAge()` (runner.go:1027) falls to the `else` branch — `p := r.te.broker.GetCurrentPrice(symbol); return p, 0, true` — i.e. **every price is reported as age-zero/fresh, unconditionally**, even though the real `AngelBroker` underneath is tracking genuine staleness and would report it accurately if only it were exposed. `softStopLossCheck()`'s stale-price skip (`if age > r.cfg.StaleAge { ...skip... }`) can therefore never fire in this mode: if the live REST/WS feed silently goes stale for whatever reason during a paper session, the software stop-loss keeps firing (or not firing) off of a price that could be arbitrarily old, with no alert.
2. `BrokerHealthy()` (engine.go:74) returns `(true, nil)` unconditionally for any broker that isn't `ExtendedTradeService` — so if the underlying Angel session's auth genuinely fails mid-session (token refresh + TOTP re-login both fail — a real, tested failure mode in `angel_broker.go`), `LivePaperBroker` never surfaces it, and `runner.tick()`'s "🚨 BROKER UNHEALTHY" halt-new-entries path never engages during a live-data paper run.

**Why this matters more than a typical paper-mode cosmetic gap:** this is precisely the mode `docs/RUNBOOK.md`/`LIVE_GATE_CHECKLIST.md`'s 1-month paper-trading gate is meant to run in to validate the system before real money is at risk. A go-live decision built on a month of paper trading that silently never exercised the stale-price guard or the broker-health halt is validating less than it appears to.

**Fix direction:** add `GetCurrentPriceWithAge`, `Healthy`, `HealthError` (and ideally `RefreshBalance`) to `LivePaperBroker`, proxying to `liveBroker` via a type assertion at construction or call time (falling back to the current always-fresh/always-healthy behavior only when `liveBroker` itself doesn't implement `ExtendedTradeService`, e.g. plain `MockBroker`-backed paper mode).

---

### F3 — MEDIUM: WebSocket feed has no read-deadline / liveness check independent of the TCP layer — a silently-stalled-but-open connection is never detected or reconnected

**File:** `go-engine/internal/broker/ws_feed.go:385-513` (`connectAndServe`).

The reconnect loop (`run`) only reacts to `conn.ReadMessage()` returning an error. Nothing sets `conn.SetReadDeadline` and nothing tracks "time since last frame received" — the client-side heartbeat (`wsHeartbeatInterval`, 10s) only *sends* a ping; it never verifies the server is still responding to anything, and a successful `WriteMessage` doesn't prove the read side is alive (TCP can accept writes into its send buffer for a long time after the peer has gone silent, especially through NAT/firewalls that drop a connection's state without sending a RST/FIN). If Angel's feed process on the other end wedges or a middlebox silently drops the session without closing the socket, `connectAndServe` will block in `conn.ReadMessage()` forever: the goroutine believes it is "connected", `run()`'s reconnect logic never triggers, and no log line or alert ever fires about the WS feed specifically.

**Mitigating factor (why this is MEDIUM and not HIGH):** `cmd/main.go:321` calls `tradeService.Subscribe(symbols)` (REST polling every 5s) unconditionally, in addition to `ext.SubscribeLive(symbols)` (WS) at `:331` — the two are wired side-by-side, not as a WS-primary/REST-fallback pair that only activates on WS failure. This means `GetCurrentPriceWithAge`'s staleness clock keeps getting reset by REST polling regardless of whether the WS feed is silently dead, so the software stop-loss's stale-price guard (for brokers that DO implement `ExtendedTradeService` — see F2 for the case where it doesn't) is not actually blind to a dead WS feed in practice. The gap is entirely in **observability**: an operator (or the watchdog) has no signal that the WS feed specifically has gone silent — it just quietly reverts to REST-only latency with nothing to distinguish that from "WS was never subscribed" or "WS is fine."

**Test coverage gap confirming this:** `ws_feed_test.go` covers a hard disconnect (`conn.Close()` on the server side, `TestWSFeed_ForcedDisconnect_Reconnects`) and never-connects (`TestWSFeed_NeverConnects_RESTFallbackStillWorks`), but there is no test for "connection stays open, server stops sending frames" — because there is no code path that would make such a test pass.

**Fix direction:** track `lastFrameAt` (touched in the `ReadMessage` loop and by any inbound text frame), set `conn.SetReadDeadline(time.Now().Add(N * wsHeartbeatInterval))` before each read (extending it on every successful read, per the standard gorilla/websocket ping/pong-liveness pattern), so a genuinely silent-but-open connection triggers the same reconnect path as an explicit close.

---

### F4 — MEDIUM: Only order placement is rate-limited; every other Angel endpoint (quote, RMS, margin, order-book status polls, positions, historical) has no client-side governor

**Files:** `go-engine/internal/broker/angel_broker.go` (`orderLimiter`, only referenced in `PlaceOrder:540` and `PlaceStopLossOrder:764`), `GetRequiredMargin:882-967`, `fetchOrderStatus:717-740`, `FetchMarketDataBatch:436-508`, `RefreshBalance:852-873`.

`AngelBroker.orderLimiter` (`rate.NewLimiter(rate.Limit(10), 10)`) is waited on only before `PlaceOrder` and `PlaceStopLossOrder`. Every other authenticated endpoint — `GetRequiredMargin` (margin batch), `fetchOrderStatus` (the order-book poll used repeatedly inside `pollOrderFill`'s retry loop), `FetchMarketDataBatch`/`fetchMarketData` (quote), `RefreshBalance` (RMS), `fetchRemotePositions` — goes through `doAPIRequest` with no rate limiting at all. Per this codebase's own R2-1 research (`docs/reports/R2-1-REPORT.md`, §"Endpoint/schema verification"), the margin-batch endpoint itself is documented as **10 req/s** — the same limit `orderLimiter` enforces for orders — yet `GetRequiredMargin` never touches `orderLimiter` or any other governor.

**Why this is only MEDIUM, not HIGH, today:** tracing the actual call graph, every place that could plausibly burst is currently serialized by construction — `Runner.tick()` evaluates symbols in a single goroutine, one at a time; `enterMultiLeg` places legs sequentially and waits for each fill (via `pollOrderFill`, itself internally backed off 500ms→5s) before placing the next; `ScanTopChains` (discovery) runs once at startup, before the tick loop starts, and already caps market-data fetches to one batched call for ≤50 symbols specifically to avoid rate-limit exposure. So there is currently no code path that fires a burst of un-throttled calls at Angel.

**Why it's still worth flagging:** the safety property here is "nothing in the code races," not "the code enforces the documented limit." It is one refactor away from a real incident — e.g., a future change that fetches margin per-leg instead of per-basket, or that runs discovery re-scans periodically instead of once at startup, or that processes symbols concurrently in `tick()` for latency reasons, would silently reintroduce burst risk with no governor to catch it, and Angel's own WAF-block detection (`isWAFBlocked`) or a raw 429 would be the only thing standing between that change and a burned session (both handled today as hard errors, which is fail-closed and correct — but "correct failure" isn't the same as "doesn't happen").

**Fix direction:** give `AngelBroker` a small number of named `rate.Limiter`s (order, quote, margin, general-authenticated) matching Angel's per-endpoint published limits, and route every `doAPIRequest` call through the appropriate one, rather than relying on the current call graph happening to stay serial.

---

### F5 — MEDIUM/LOW: Market-hours gating has no concept of an exchange-declared half-day/special-session, compounding the already-known incomplete holiday table

**File:** `go-engine/internal/engine/runner.go:561-583` (`marketState`), `:548-559` (`nseHolidays2026`).

`marketState()` computes open/square-off/hard-close purely from three fixed daily clock times (`09:15 + buffer`, configurable square-off, hard `15:30`) plus a holiday-date lookup. This is already flagged (G-12 in `PRODUCTION_GAPS_R2.md`) as an incomplete, hardcoded-for-2026, movable-festival-blind table — that finding stands as documented.

What isn't yet documented: NSE occasionally runs **special-hours sessions** that are not full holidays (e.g. the annual Muhurat trading session, held outside normal hours on Diwali) and has, historically, had early-close days for reasons other than a full holiday declaration. Nothing in `marketState`/`RunnerConfig` can represent "trading today closes at 13:00" or "trading today is 18:15–19:15 instead of 09:15–15:30" — a day like that is either (a) not in the holiday table, so the runner behaves as if it's a completely normal session and tries to place entries and eventually flatten at the *configured* square-off time (15:20 default) — which, on a day the exchange actually closed at 13:00, means the square-off order fires against a market that has been closed for over two hours and simply fails/queues — or (b) if it *is* wrongly listed as a full holiday, the runner sits out a session NSE actually held (e.g. the evening Muhurat session), which is a missed-trading-day rather than a money-safety issue.

Scenario (a) is the one that matters for the "no silent fallback on money-critical paths" standing rule: the flatten-at-square-off path assumes the exchange is still open at the configured square-off time, and nothing checks that assumption.

**Fix direction:** extend the holiday-file schema (already config-file-driven per R2-5/G-12) to carry optional per-date open/close overrides, not just a boolean "is holiday," and have `marketState` consult them.

---

### F6 — LOW: `InstrumentManager.FindOption`'s paise/rupee ambiguity fallback can silently accept a coincidental ×100 strike match

**File:** `go-engine/internal/broker/instruments.go:350-392`.

`FindOption` accepts an instrument if its strike matches the requested strike **either** directly (`±0.5`) **or** at `×100` (`±0.5`), to handle the documented paise-vs-rupee ambiguity in Angel's instrument master. It correctly refuses (returns an ambiguity error) when the *same* expiry+type combination yields two different matching symbols. But if only one candidate matches — via either the raw or the ×100 branch — it is accepted without distinguishing "this matched because of the paise/rupee unit issue" from "this happens to be a genuinely different, real contract at 100× the requested strike." For an index whose valid strikes span a wide, overlapping range (plausible for BANKNIFTY/SENSEX, where a 250-strike request and a real 25000-strike contract could both theoretically exist for the same expiry+type), this could silently resolve to the wrong contract instead of erroring.

**Why this is LOW, not higher:** traced every caller — `FindOption` is used only by `cmd/fetchdata` (the read-only historical-data-fetch CLI, explicitly asserted elsewhere to never reach `PlaceOrder`). The live order-placement path (`runner.go`'s `buildOptionSymbol`) constructs option symbols directly as strings and never calls `FindOption`; the constructed symbol is validated against the instrument master inside `AngelBroker.PlaceOrder`'s own `GetInstrument` lookup (fails closed if it doesn't exist), so this ambiguity has no live-money path today. It would matter if `FindOption` is ever reused for a live-trading purpose (e.g. a future discovery/backtest-parity refactor).

---

### F7 — INFORMATIONAL: Position reconciliation is startup-only; nothing re-reconciles mid-session, including after an order marked "indeterminate"

**Files:** `go-engine/cmd/main.go:225-255` (startup `state.Reconcile` call), `go-engine/internal/state/reconcile.go`, `go-engine/internal/engine/engine.go:164-176` / `:270-278` (indeterminate-order handling).

`cmd/main.go` does the right thing at startup — `state.Reconcile(internalPositions, brokerPositions)`, refusing to start on any phantom/orphan/quantity-mismatch unless `-accept-reconcile` is passed. This is solid and correctly fail-closed.

But there is no periodic re-reconciliation after that point. When an order comes back `ErrOrderIndeterminate` mid-session (both `PlaceEntryOrder` and `PlaceExitOrder` handle this branch explicitly: log "CRITICAL... marked for reconciliation", alert, and stop touching internal state), nothing subsequently calls `Reconcile` again to actually discover what happened at the broker — the comments consistently say "MANUAL reconciliation" / "MANUAL INTERVENTION REQUIRED," which is an honest, deliberate design choice (documented, not hidden), not a silent gap. Flagging it here only because the task asked specifically about reconciliation-mismatch handling: the answer is "correct and fail-closed at startup, and correctly *refuses to guess* mid-session rather than silently resolving an indeterminate order — but there is no automated loop that ever closes that manual-intervention gap on its own," matching the still-open G-8 (no standalone watchdog) from Round 2.

---

## What was checked and found solid (no finding)

- **Order placement / fill lifecycle** (`angel_broker.go` `PlaceOrder`): circuit breaker correctly bypassed for `IntentReduceOnly`; rate-limiter waited outside the lock; indeterminate-order handling never fabricates a fill (no LTP/₹1.0 fallback — CR-5 stays fixed); sign-crossover position flip (`updatePosition`) correctly handles a same-side add, a partial reduce, an exact close, and a flip-through-zero, all tested.
- **Margin API (`GetRequiredMargin`)**: correctly fail-closed on every ambiguous/error condition (empty basket, >50 legs, unresolvable symbol, non-200, malformed JSON, `status:false`, `data:null`, non-positive total). Genuinely well-covered by `angel_broker_test.go`.
- **Lot size / expiry / strike-step resolution in the live path** (`runner.go` `lotSize`/`resolveExpiry`): every tier is either the real instrument master or an operator-set config override; never a bare hardcoded guess; correctly refuses the trade if none resolve.
- **Multi-leg entry unwind** (`enterMultiLeg`/`unwindLegs`): places hedge-first per the strategy's leg ordering, unwinds already-filled legs on a later leg's rejection, and explicitly does NOT unwind on an indeterminate leg (correctly leaves it for manual reconciliation rather than risking a double-unwind of a leg that may not have failed).
- **Shutdown/flatten path** (`runner.go` `Shutdown`): retries flatten up to 3 times with backoff, verifies the broker's position book is actually empty before returning success, and returns a non-nil error (forcing the caller to exit non-zero) if it can't confirm — no "shutdown cleanly" lie is possible here.
- **Discovery** (`discovery.go`): lot size, expiry, and index-symbol resolution all go through the instrument master and fail closed (skip the chain) rather than guess; ATM-distance pre-filter (top 50 per index) and a final volume filter are both reasonable illiquid-contract defenses; the affordability filter degrades safely (falls back to "show all" with a warning, doesn't silently trade something unaffordable).
- **Historical data fetch chunking** (`historical.go`): correctly pages across Angel's per-request day/record limits with a documented, conservative `maxChunkDays` table; rejects rather than zero-fills unparseable/inverted/non-positive candle rows; truncates to the last fully-completed candle to avoid repainting.

---

## Summary table

| # | Finding | Severity | File(s) |
|---|---|---|---|
| F1 | Partial-fill quantity never propagated from `FilledOrder.Quantity` to `risk.Manager.OpenPositions[].Quantity` — CR-6 recurs one layer up | **CRITICAL** | `internal/engine/engine.go`, `internal/risk/risk.go` |
| F2 | `LivePaperBroker` doesn't proxy `ExtendedTradeService` — stale-price guard and broker-health halt are both silently disabled in live-data paper mode | **HIGH** | `internal/broker/live_paper_broker.go`, `internal/engine/runner.go`, `internal/engine/engine.go` |
| F3 | WS feed has no read-deadline/liveness check independent of TCP errors; a silently-stalled-open connection never reconnects (mitigated by parallel REST polling) | MEDIUM | `internal/broker/ws_feed.go` |
| F4 | No client-side rate limiting outside order placement (margin/quote/RMS/order-book/positions all unthrottled); safe today only because the call graph happens to be serial | MEDIUM | `internal/broker/angel_broker.go` |
| F5 | No half-day/special-session concept in market-hours gating; square-off could fire against an already-closed market | MEDIUM/LOW | `internal/engine/runner.go` |
| F6 | `FindOption`'s paise/rupee ×100 fallback can accept a coincidental wrong-strike match when only one candidate matches; no live-order path uses it today | LOW | `internal/broker/instruments.go` |
| F7 | No periodic mid-session reconciliation; indeterminate orders are correctly flagged but never automatically resolved | INFORMATIONAL | `cmd/main.go`, `internal/state/reconcile.go`, `internal/engine/engine.go` |

**Bottom line:** the codebase's discipline around *known* failure modes (indeterminate fills, WAF blocks, auth expiry, sign-crossover, margin ambiguity) is genuinely strong and well-tested. The two findings that actually matter for money-safety (F1, F2) both share a pattern: a fix that is real and correctly tested *at the layer it was written*, but that doesn't reach the layer the rest of the system actually depends on (the risk manager's position book; the paper-mode staleness/health checks). That's the shape to watch for in any future remediation pass here — verify a fix's effect at the consumer, not just at the producer.
