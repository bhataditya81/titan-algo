# WP-1 — Broker Hardening — Report

Scope: `internal/broker/angel_broker.go`, `internal/broker/instruments.go`, `internal/broker/broker.go` only. No other files touched.

## Findings addressed

| ID | Finding | Fix |
|---|---|---|
| CR-5 | Timed-out pending order treated as FILLED with ₹1.0 fallback | `PlaceOrder` now returns `*IndeterminateOrderError` (wraps `ErrOrderIndeterminate`) with the broker order ID on poll-budget exhaustion. No LTP fallback, no `1.0` fallback, no fabricated `FilledOrder` anywhere in the timeout/ambiguous path. |
| CR-6 | Partial fills ignored; requested qty used everywhere | `FilledOrder.Quantity` is now the broker-reported `filledshares`; `FilledOrder.RequestedQty` holds the original ask; `Partial()` helper added. `updatePosition` is called with actual fill qty, never requested qty. Order-book statuses `open`/`pending`/`partially filled` keep polling; only `complete`/`rejected`/`cancelled` are terminal. |
| EX-9 (sign crossover) | Opposite-side fill > existing qty just deleted the position | `updatePosition` now flips through zero: creates a new position on the opposite side with the remainder quantity and the new fill price as its average. Covered by 3 unit tests (crossover, exact close, partial reduce). |
| EX-5 | `PlaceOrder` held the write lock across sleeps/HTTP/poll | Rewritten: `a.mu` is taken only to (1) snapshot connected/circuit-breaker state at entry, (2) read cached LTP for slippage, (3) commit the position mutation at the end. Rate limiting, the order HTTP call, and the entire fill-poll loop run with no lock held. Old unlock-sleep-relock rate limiter replaced by `golang.org/x/time/rate.Limiter` (10/sec, burst 10), waited on via `Wait(ctx)` outside any lock. |
| EX-2 | Circuit breaker blocked exits too | `Order.Intent` (`IntentEntry` / `IntentReduceOnly`) added; `Order.ReduceOnly()` helper. Circuit-breaker gate in `PlaceOrder` is skipped entirely when `order.ReduceOnly() == true`. `PlaceStopLossOrder` never checks the circuit breaker at all (a protective stop must never be blocked). |
| EX-6 | No session refresh; mid-day expiry kills the system silently; token/key logged | Every authenticated Angel call now goes through `doAPIRequest`, which detects HTTP 401 or `status:false` + error code prefixed `AG`/`AB` (Appendix A), attempts `POST /jwt/v1/generateTokens` (A-4) with the stored `refreshToken`, and — if that fails — falls back to one full TOTP re-login (A-1, reusing the same `login()` used by `Connect`). On total failure the broker flips `healthy=false` with `HealthError()` set (`ErrAuthFailed`). Exported `Healthy() bool` / `HealthError() error`. Removed the `fmt.Printf("📊 Access Token: %s...\n", a.accessToken[:20])` line and confirmed via grep no token/key/secret value is logged anywhere in the file. |
| EX-7 | `GetBalance` hardcoded to 10000.0 | `RefreshBalance()` calls `GET /rest/secure/angelbroking/user/v1/getRMS` (A-5), parses `data.availablecash`, updates the cached balance. Called at `Connect()`; also exported for WP-9 to call periodically. `GetBalance()` still returns the cached value (interface-compatible). |
| CR-14/EX-6 (staleness) | No price-age tracking | `priceUpdated map[string]time.Time` added; every place a price is cached now stamps the time. New `GetCurrentPriceWithAge(symbol) (float64, time.Duration, error)`; old `GetCurrentPrice` unchanged (still returns 0 on miss, cache-then-fetch semantics preserved). |
| EX-9 (HTTP status / WAF) | Status codes never checked; WAF block during position fetch returned "no positions" | All authenticated calls funnel through `doAPIRequest`, which checks WAF/HTML-block bodies (`isWAFBlocked`) and non-200 status (`*HTTPStatusError`) before returning. `fetchRemotePositions` now propagates that error instead of swallowing it — a WAF block during position fetch is a hard error, not an empty map. (A truly empty 200-OK body — Angel's legitimate "flat" response — is still treated as zero positions, which is correct.) |
| EX-9 (instruments) | Bare `http.Get`, no timeout, loaded while holding broker lock; ~100MB re-downloaded every run | `InstrumentManager` now owns an `http.Client{Timeout: 180s}`. `Connect()` was restructured so `LoadInstruments()` runs with **no `AngelBroker` lock held at all** (the old code held `a.mu` for the entire `Connect()` body via `defer a.mu.Unlock()`; now `Connect()` only locks briefly around field writes). Instrument master is cached to disk at `data/instruments/scripmaster_<IST-date>.json`; a same-day rerun loads from disk instead of re-downloading. `GetLotSize(symbol) (int, error)` and `GetExpiries(underlying) ([]time.Time, error)` added, derived from the parsed master (NFO segment, `Name` field match for expiries). Previously-dead `Instrument.LotSizeInt/StrikeFloat/TickSizeFloat` fields (declared, never populated) are now parsed once at index-build time. |
| Task 11 | No broker-side SL, no cancel | `PlaceStopLossOrder(symbol, qty, triggerPrice, side) (orderID string, err error)` — variety `STOPLOSS`, ordertype `STOPLOSS_MARKET` (A-2). Does not poll for a fill (SL orders rest until triggered). `CancelOrder(orderID string) error` — endpoint A-3; broker remembers the variety used at placement (`orderVariety` map) so cancel sends the right `variety` without the caller tracking it. |

## Files changed

- `go-engine/internal/broker/broker.go` — rewritten. Added `Order.Intent`/`ReduceOnly()`, `FilledOrder.RequestedQty`/`Partial()`, sentinel errors (`ErrOrderIndeterminate`, `ErrStalePrice`, `ErrNoPrice`, `ErrWAFBlocked`, `ErrAuthFailed`, `ErrCircuitOpen`), `IndeterminateOrderError`, `HTTPStatusError`, and a new `ExtendedTradeService` interface. **`TradeService` itself is unchanged** (see below).
- `go-engine/internal/broker/angel_broker.go` — rewritten (see findings table).
- `go-engine/internal/broker/instruments.go` — rewritten (see findings table).
- `go-engine/internal/broker/angel_broker_test.go` — new, 15 tests.
- `go-engine/internal/broker/instruments_test.go` — new, 3 tests.
- `go-engine/go.mod` / `go.sum` — added `golang.org/x/time v0.15.0` (the one dependency explicitly permitted). `go mod tidy` also promoted `modernc.org/sqlite` from indirect to direct in `go.mod`; that dependency was already present in the graph (added by another in-flight package, presumably WP-3/WP-5) — I did not add it and did not touch any code that uses it.

## Interface changes for WP-9 (and anyone else consuming `internal/broker`)

**`TradeService.PlaceOrder(order Order) (*FilledOrder, error)` signature is UNCHANGED.** I deliberately did not add a parameter or touch the interface method set that `MockBroker` / `LivePaperBroker` implement, because those files are owned by other/no packages and changing the interface would have broken `go build ./...` outside files I'm allowed to touch. Instead:

1. **`Order.Intent OrderIntent`** (new field, additive/backward-compatible — existing `broker.Order{...}` field-literal construction sites are unaffected). Values: `IntentEntry` (default, zero value `""` also behaves as entry) and `IntentReduceOnly`. **Action for WP-9:** every exit / flatten / unwind order built in `engine.go` / `cmd/main.go` must set `Intent: broker.IntentReduceOnly` so it bypasses `AngelBroker`'s circuit breaker. `engine.ClosePosition` currently builds `broker.Order{...}` with no `Intent` — that needs the field added when WP-9 wires WP-1 in.
2. **`FilledOrder.Quantity` is now the actual filled quantity, not the requested quantity.** `FilledOrder.RequestedQty` carries the original ask; `FilledOrder.Partial()` tells you if they differ. Any caller that assumed `Quantity == order.Quantity` (I did not find such an assumption in `engine.go`, which already just reads `filled.Quantity`/`filled.FillPrice`) should re-check.
3. **New sentinel error `broker.ErrOrderIndeterminate`.** WP-9's order-flow task explicitly needs this: "On `ErrOrderIndeterminate`: do NOT roll back risk state; persist attempt; poll order book resolution in background; alert." Use `errors.Is(err, broker.ErrOrderIndeterminate)`, and `errors.As(err, &ioe)` to get `ioe.OrderID` for the reconciliation record.
4. **New `broker.ExtendedTradeService` interface** (embeds `TradeService`) exposing everything `*AngelBroker` gained that isn't on the base interface: `PlaceStopLossOrder`, `CancelOrder`, `GetCurrentPriceWithAge`, `Healthy`, `HealthError`, `RefreshBalance`. `*AngelBroker` implements it; `*MockBroker`/`*LivePaperBroker` do not (out of scope for me to touch). WP-9 should type-assert: `if ext, ok := svc.(broker.ExtendedTradeService); ok { ... }`, or extend `MockBroker`/`LivePaperBroker` with no-op/paper-mode implementations if paper mode needs the same call sites to work uniformly.
5. **`InstrumentManager.GetLotSize(symbol) (int, error)` and `GetExpiries(underlying) ([]time.Time, error)`** — exported on `*broker.InstrumentManager` (reachable via `AngelBroker.instruments`... note that field is unexported; WP-9 will need `*AngelBroker` to expose its `*InstrumentManager`, or WP-1's next iteration should add an `AngelBroker.Instruments() *InstrumentManager` accessor. **I did not add that accessor** since it wasn't explicitly requested and I wanted to keep the diff minimal — flagging it here since WP-9/WP-6 will need it to reach `GetLotSize`/`GetExpiries` off a live `*AngelBroker`.** Easiest fix: WP-9 constructs its own `*broker.InstrumentManager` at startup (as `cmd/main.go` already does for the `-search` flag) and calls `LoadInstruments()` once, sharing that instance.
6. **`PlaceStopLossOrder`/`CancelOrder` are not on `TradeService`**, for the same don't-break-other-implementers reason as above — use `ExtendedTradeService`.

## Endpoint / discrepancy notes (rule 5 — code vs. audit/plan)

- **Instrument master URL mismatch.** Existing code used `https://margincalculator.angelbroking.com/OpenAPI_File/files/OpenAPIScripMaster.json` (legacy domain). Appendix A-11 specifies `https://margincalculator.angelone.in/...`. Updated to match Appendix A (the authoritative allowed-endpoint list).
- **Positions endpoint path mismatch.** Existing code called `GET /rest/secure/angelbroking/portfolio/v1/getPosition`. Appendix A-8 specifies `GET /rest/secure/angelbroking/order/v1/getPosition`. Updated to match Appendix A.
- **CancelOrder variety.** The plan text just says "Add `CancelOrder(orderID)`", but Angel's real cancel endpoint requires a `variety` (`NORMAL` vs `STOPLOSS`) matching how the order was placed. Kept the single-argument signature (`CancelOrder(orderID string) error`) as specified, and solved the variety problem internally with an `orderVariety map[string]string` populated at order-submission time — no interface deviation, just noting the internal solution.
- **Line numbers in the audit** (`angel_broker.go:751-787`, `:616-822`, `:1016-1021`, `:992-997`, `:197-199`, etc.) were all accurate to within a few lines of the actual pre-fix code — no material drift, no forced changes to the wrong location.
- **Dead fields discovered, not previously flagged by an ID:** `Instrument.LotSizeInt`, `StrikeFloat`, `TickSizeFloat` were declared but never populated anywhere in the original `LoadInstruments`, silently forcing every caller (`discovery.go`) to re-parse the raw strings on every call. Fixed by populating them once at index-build time in `instruments.go` — a strict improvement inside my owned file, doesn't touch `discovery.go`.

## Test evidence

`go test -race ./internal/broker/...` — **PASS**, 18 tests, 5.6s, no race detector warnings.

```
--- PASS: TestPlaceOrder_PendingTimesOut_ReturnsIndeterminate (0.06s)     [CR-5, required]
--- PASS: TestPlaceOrder_CancelledNoFill_ReturnsErrorNotFill (0.01s)
--- PASS: TestPlaceOrder_PartialFill_RecordsActualQty (0.01s)            [CR-6, required]
--- PASS: TestUpdatePosition_SignCrossover_FlipsToOppositeSideWithRemainder (0.00s)  [EX-9, required]
--- PASS: TestUpdatePosition_ExactClose_Deletes (0.00s)
--- PASS: TestUpdatePosition_PartialReduce_KeepsSameSide (0.00s)
--- PASS: TestFetchRemotePositions_WAFBlocked_ReturnsError (0.01s)       [EX-9, required]
--- PASS: TestFetchRemotePositions_NonOKStatus_ReturnsError (0.00s)
--- PASS: TestDoAPIRequest_401_TriggersRefreshThenSucceeds (0.01s)       [EX-6, required]
--- PASS: TestDoAPIRequest_401_RefreshAndReloginBothFail_MarksUnhealthy (0.01s)
--- PASS: TestPlaceOrder_DoesNotBlockConcurrentReadsDuringHTTP (0.02s)   [EX-5, lock hygiene]
--- PASS: TestPlaceOrder_ReduceOnly_BypassesOpenCircuitBreaker (0.01s)   [EX-2]
--- PASS: TestGetLotSize (0.00s)
--- PASS: TestGetExpiries (0.00s)
--- PASS: TestGetCurrentPriceWithAge (0.00s)
--- PASS: TestInstrumentManager_HasHTTPTimeout (0.00s)
--- PASS: TestLoadInstruments_DownloadsThenUsesDiskCacheOnSecondCall (0.04s)
--- PASS: TestLoadInstruments_NonOKStatus_ReturnsError (0.01s)
PASS
ok  	titan-algo/internal/broker	5.595s
```

Required grep proofs:
```
$ grep -n "= 1\.0\|fillPrice = 1" internal/broker/angel_broker.go
(no matches — no literal 1.0 fill-price fallback remains)

$ grep -n "Could not fetch actual fill price" internal/broker/angel_broker.go
(no matches — LTP fallback path deleted)
```
No-lock-across-I/O proof (grep on `PlaceOrder` body): `a.mu.RLock()`/`a.mu.Lock()` appear only at function entry (state snapshot), around the LTP read for slippage, and around the final `updatePosition` call — `submitAngelOrder` (→`doAPIRequest`→`httpClient.Do`) and `pollOrderFill` (→ `fetchOrderStatus` → `httpClient.Do`, with `time.Sleep` between attempts) run entirely outside any held lock. `TestPlaceOrder_DoesNotBlockConcurrentReadsDuringHTTP` proves this behaviorally: `GetBalance()`/`GetCurrentPrice()` return immediately while an order's HTTP call is deliberately held open by the test server.

## Build/vet status

- `go build internal/broker/angel_broker.go internal/broker/broker.go internal/broker/instruments.go internal/broker/mock.go internal/broker/live_paper_broker.go` (isolated file-set build of every file in the `internal/broker` package except `historical.go`, which is WP-6's, not mine) — **clean**.
- `go vet` on that same file set — **clean**.
- **Whole-repo `go build ./...` currently fails**, but only in `cmd/main.go` and `cmd/backtest/main.go`, both entirely outside WP-1's ownership:
  ```
  cmd\backtest\main.go:162:19: strat.EvaluateCandles undefined (type strategy.Strategy has no field or method EvaluateCandles)
  cmd\main.go:677:46: too many arguments in call to activeStrategy.Evaluate
      have (string, []float64, []float64, time.Time)
      want (strategy.EvalContext)
  ```
  This is WP-6's in-flight `Signal`/`EvalContext` interface refactor (`internal/strategy/registry.go`, `iron_fly.go`, `nine_twenty.go`, `rsi_reversal.go`, `short_straddle.go`, `sniper.go` — file mtimes show them mid-edit at the time I ran this). `internal/broker/historical.go` (owned by WP-6) imports `internal/strategy`, so the whole `internal/broker` package — including my files — fails `go build ./...`/`go vet ./...` only when compiled as part of the full module graph, purely as a transitive consequence of that unrelated, incomplete change. Confirmed via file mtimes and via the isolated file-set build above (my three owned files plus every other file *I* didn't touch in the package build and vet clean on their own). Once WP-6 lands, whole-repo `go build ./...` should pass; nothing in WP-1 needs further change for that.

## Not done / explicitly out of scope

- Did not add an `AngelBroker.Instruments()` accessor (see point 5 above) — flagging for WP-9 rather than guessing at API shape beyond what was asked.
- Did not touch `mock.go` / `live_paper_broker.go` to implement `ExtendedTradeService` — not owned files; paper-mode parity for `PlaceStopLossOrder`/`CancelOrder`/`Healthy`/etc. is a WP-9 (or a follow-up) decision.
- `PlaceStopLossOrder` does not poll for a fill by design (SL orders rest until triggered) — it returns the broker order ID only. WP-9's stop-loss wiring should track that ID and rely on the periodic order-book reconciliation (or a future dedicated poll) to learn when it fires.
