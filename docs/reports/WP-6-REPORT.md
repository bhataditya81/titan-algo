# WP-6 — Strategy Layer Fixes — Report

Scope: `internal/strategy/*` (all files) + `internal/broker/historical.go`, per the file
ownership matrix. No other files were edited.

## 1. Findings addressed

| ID | Finding | Fix location |
|---|---|---|
| ST-8 | Signal semantics overloaded — Buy meant both "go long" and "exit short" | `strategy.go`: added explicit `Exit` action; `Buy`/`Sell` are now pure directional entries. All strategies updated to emit `Exit` only, never `Buy`/`Sell`, to close a position. |
| ST-5 | Stateless strategies re-signal a fresh entry every candle → churn | `short_straddle.go`, `iron_fly.go`, `nine_twenty.go`: entry only when `!ctx.HasPosition`, `Exit` only when `ctx.HasPosition`. |
| ST-3 | nine_twenty (and historical fetch) used server-local clock, not IST | `strategy.go`: package-level `IST *time.Location`, loaded via `time.LoadLocation("Asia/Kolkata")`, **panics at init on failure**. `nine_twenty.go` and `sniper.go` convert `.In(IST)` before any hour/minute comparison. `broker/historical.go` formats the fetch window in IST and truncates using IST wall clock. |
| ST-4 | nine_twenty state in-memory, flips on signal generation not fill confirmation; day-reset compared `Day()` only | `nine_twenty.go`: `entered` flips true only via `ConfirmEntry()`; added `ConfirmExit()` for symmetry; `Snapshot()`/`Restore()` for WP-3 persistence; day reset now compares full IST calendar date (`sameISTDate`). |
| CR-14 (strategy side) | Naked short strategies (nine_twenty, short_straddle) had no stop-loss at all; Sniper's SL/Target fields were write-only | `nine_twenty.go` + `short_straddle.go`: combined-premium stop, `Exit` when current combined premium ≥ entry premium × `StopMultiplier` (default 1.4). `sniper.go`: `StopLossPct`/`TargetPct` now wired into `Signal.StopLossPrice`/`TargetPrice`. |
| CR-12 | iron_fly declared short body legs before hedge legs — a rejected wing leaves a naked short | `iron_fly.go`: `Legs` now BUY (hedge) first, SELL (body) last, with a comment explaining why this ordering is load-bearing. |
| ST-9 | Sniper: fake "5 ticks = 1 candle", no `Candle.Time`, volume abused as tick counter, re-signals every tick | `sniper.go`: real wall-clock IST 5-min candle boundaries (`floorToInterval`), `Candle.Time` set to bucket start, volume = delta of cumulative `ctx.Volumes`, one-signal-per-completed-candle via the `completed` return of `updateCandle`. |
| ST-1 | RSI: `avgLoss==0` returned `nil` (dropped the strongest overbought signal) | `indicators.go`: `avgLoss==0 && avgGain>0` → `RSI{100}`; both zero → `RSI{50}`. |
| ST-2 | VWAP used close price, not session-anchored, mis-weighted with cumulative volume | `indicators.go`: new `CalculateSessionVWAP(candles)` — resets at each IST calendar day boundary, uses typical price `(H+L+C)/3`. Old `CalculateVWAP` kept as an explicitly-documented **degraded** tick-mode fallback. `momentum.go` prefers session VWAP when candles are available. |
| ST-10 (candlestick) | Engulfing used `>=`/`<=`, degrading to false positives on `open≈prior close` intraday data | `candlestick.go`: requires `currBody > prevBody` AND at least one strict inequality on the open/close comparison. |
| ST-10 (momentum) | Fake normalization: `score/conditions*conditions` is a no-op; `conditions` was always 1.0 regardless of whether VWAP was available | `momentum.go`: `weight` now only accumulates for indicators actually evaluated (VWAP may be nil); score genuinely divides by that weight. |
| ST-10 (rsi_reversal) | No real exit condition beyond the opposite RSI extreme | `rsi_reversal.go`: added mean-reversion exit — close crosses back through `SMA(ExitSMA)` or RSI crosses back through 50, mirrored for long/short (side tracked internally since `EvalContext` carries no position-direction field). |
| ST-10 (historical parse errors) | `getFloat` swallowed parse errors into `0.0`, poisoning indicators | `broker/historical.go`: `getFloat` returns `(float64, error)`; malformed rows are skipped and logged, not zero-filled. |
| ST-10 (historical sanity) | No OHLC/gap/sort validation | `broker/historical.go`: rejects non-positive prices, `High<Low`, and out-of-order timestamps (relative to the last accepted row). |
| ST-7 (fetch window) | Historical fetch didn't truncate to a completed candle boundary, and used server-local time | `broker/historical.go`: `truncateToLastCompletedCandle(now, interval)` — IST-anchored, caps to `09:15`/`15:30`, handles `ONE_DAY` separately. |
| ST-10/M7 | Some `Evaluate`/`EvaluateCandles` implementations could panic on insufficient data | All strategies guard on `len(prices) < N` / `len(candles)==0` and return `Hold`. The shared `strategy.EvaluateCandles(s, history)` helper returns `Hold` on empty history before ever touching the strategy. |
| ST-10/L4 | `registry.Get` returned a fresh instance every call, silently dropping state for stateful strategies | `registry.go`: `Get` returns a cached, mutex-guarded singleton per name; added `Reset(name)`. |

## 2. Files changed

All under `internal/strategy/` (owned) and `internal/broker/historical.go` (owned):

- `strategy.go` — rewritten: new Signal/EvalContext/OrderLeg contract, `IST`, `Strategy` interface, `strategy.EvaluateCandles(s, history)` helper.
- `registry.go` — singleton cache + `Reset`.
- `indicators.go` — RSI fix, `CalculateSessionVWAP`, degraded-mode doc on `CalculateVWAP`.
- `candlestick.go` — strict-inequality engulfing.
- `momentum.go` — new `Evaluate(ctx)` signature, normalization fix, session-VWAP preference.
- `ema_crossover.go` — new `Evaluate(ctx)` signature (logic unchanged).
- `rsi_reversal.go` — new `Evaluate(ctx)` signature + mean-reversion exit.
- `nine_twenty.go` — full rewrite: IST, position-aware, premium stop, `ConfirmEntry`/`ConfirmExit`, `Snapshot`/`Restore`, full-date reset.
- `short_straddle.go` — position-aware, combined-premium stop, RSI exit kept as secondary.
- `iron_fly.go` — position-aware, hedge-first leg order.
- `sniper.go` — full rewrite: wall-clock IST candles, volume deltas, completion latch, wired stops; candle-mode bypass path retained for backtest-style callers.
- `price_history.go` — unchanged (gofmt only).
- `internal/broker/historical.go` — IST-formatted request window, completed-candle truncation, row validation, propagated parse errors.

New test files: `indicators_test.go`, `candlestick_test.go`, `sniper_test.go`, `nine_twenty_test.go`, `registry_test.go`, `position_aware_test.go` (strategy package); `historical_test.go` (broker package).

## 3. New contract — field by field

### `SignalAction`
```go
Buy  SignalAction = "BUY"   // open LONG. Never "exit a short".
Sell SignalAction = "SELL"  // open SHORT. Never "exit a long".
Exit SignalAction = "EXIT"  // close the strategy's current position (all legs). Only emitted when HasPosition.
Hold SignalAction = "HOLD"  // no action.
```

### `OrderLeg`
```go
type OrderLeg struct {
    Direction    LegDirection // LegBuy | LegSell
    StrikeOffset int          // points from ATM; 0 = ATM
    OptionType   string       // "CE" or "PE"
    Expiry       string       // NEW. Expiry selector, e.g. "2026-01-20"; "" = nearest weekly (engine resolves via instrument master)
    Quantity     int          // NEW. Number of LOTS (not shares). Strategies emit >= 1; engine multiplies by lot size.
}
```
Ordering contract (CR-12): within `Signal.Legs`, BUY legs MUST precede SELL legs.

### `Signal`
```go
type Signal struct {
    Action        SignalAction
    Strength      float64    // 0.0-1.0
    Reason        string
    Legs          []OrderLeg // non-empty => multi-leg structure, BUY legs first
    StopLossPrice float64    // NEW. Absolute stop price/premium; 0 = not set
    TargetPrice   float64    // NEW. Absolute target price/premium; 0 = not set
}
```

### `EvalContext` (new; replaces the old `Evaluate(symbol, prices, volumes, time.Time)` parameter list)
```go
type EvalContext struct {
    Symbol  string
    Prices  []float64   // tick-mode series, oldest→newest
    Volumes []float64   // aligned with Prices; typically CUMULATIVE day volume per tick
    Candles []Candle    // candle-mode series, oldest→newest
    Now     time.Time

    HasPosition  bool          // true while this strategy currently holds a position
    PositionAge  time.Duration // time since entry fill confirmation (informational)
    EntryPremium float64       // combined entry premium of all legs, for option structures; 0 if unknown
}
```
Helper methods: `ClosePrices()`, `VolumeSeries()`, `LastPrice()` — transparently read either `Prices` or `Candles`.

**Current combined premium for premium-stops** (nine_twenty, short_straddle) is read via `ctx.LastPrice()` — i.e. the last element of `Prices` (or last `Candles[].Close`). `EvalContext` intentionally has no separate "current premium" field per the WP-6 spec; **the caller (engine) is responsible for feeding the live combined-premium series into `Prices`/`Candles` for a symbol that has an open option structure.** This is the one place I made an explicit design call beyond the literal spec text — documented in `strategy.go` and in `nine_twenty.go`/`short_straddle.go` doc comments. WP-9 must wire this: when a nine_twenty/short_straddle position is open, `EvalContext.Prices` (or `.Candles`) for that symbol's evaluation must be the combined CE+PE premium series, not the underlying index price series used pre-entry.

### `Strategy` interface
```go
type Strategy interface {
    Name() string
    Evaluate(ctx EvalContext) Signal // implementations MUST return Hold on insufficient data, never panic
}
```
`EvaluateCandles(history []Candle) Signal` is **no longer a Strategy interface method**. It's now a free function:
```go
func EvaluateCandles(s Strategy, history []Candle) Signal
```
which builds a position-agnostic `EvalContext{Candles: history, Now: history[last].Time}` (`HasPosition=false`) and calls `s.Evaluate(ctx)`. Callers that need position-aware behavior (short_straddle/iron_fly/nine_twenty's entry/exit gating) MUST construct `EvalContext` directly with `HasPosition`/`EntryPremium` set — `strategy.EvaluateCandles` is only for simple candle-only callers with no position tracking.

`sniper.go` is the one exception with dual-mode `Evaluate`: if `len(ctx.Prices)==0 && len(ctx.Candles)>0` it treats the call as "caller supplied full closed-candle history" and evaluates directly (no tick aggregation, no completion latch — mirrors the old `EvaluateCandles` method's behavior for backtest-style callers). Otherwise it runs the tick-aggregation/latch path.

### Package-level `IST`
```go
var IST *time.Location // = time.LoadLocation("Asia/Kolkata"); panics at package init on failure
```
`internal/broker/historical.go` imports `internal/strategy` already (for `strategy.Candle`) and reuses `strategy.IST` rather than loading its own — one source of truth for the timezone, per Appendix B ("ALL market logic in Asia/Kolkata").

### `nine_twenty.go` new methods
```go
func (s *NineTwentyStrategy) ConfirmEntry()               // flips entered=true; call ONLY after a confirmed fill
func (s *NineTwentyStrategy) ConfirmExit()                // flips entered=false; symmetric addition, not in the literal spec (see §5)
func (s *NineTwentyStrategy) Snapshot() map[string]string  // entered, pending_exit, signaled_entry, last_date, entry_premium
func (s *NineTwentyStrategy) Restore(state map[string]string)
```

### `registry.go`
```go
func Get(name string) (Strategy, error) // now returns a cached, mutex-guarded SINGLETON per name
func Reset(name string)                 // NEW — discards the cached instance, next Get constructs fresh
```

## 4. BROKEN EXTERNAL CALL SITES — for WP-9 (critical section)

`go build ./...` from `go-engine/` fails in exactly two packages, both outside my ownership (`cmd/`), as expected/acceptable per the plan. `internal/app`, `internal/engine`, `internal/cli`, `internal/api`, `internal/discovery`, and `internal/backtest` (WP-7's new package) all compile cleanly against the new contract already — I verified each individually.

### Compile errors (verified via `go build ./...`)

**1. `cmd/main.go:677`**
```go
signal := activeStrategy.Evaluate(symbol, prices, volumes, time.Now())
```
`Evaluate` now takes `EvalContext`, not `(string, []float64, []float64, time.Time)`. Fix: build `strategy.EvalContext{Symbol: symbol, Prices: prices, Volumes: volumes, Now: time.Now(), HasPosition: <derive from risk manager>, EntryPremium: <derive from position/ledger>}`. Note the `currentPos, posExists := te.GetRiskManager().GetOpenPositions()[symbol]` lookup currently happens a few lines AFTER this call (around line 709) — it needs to move BEFORE this call so `HasPosition`/`EntryPremium` can be populated into the context.

**2. `cmd/backtest/main.go:162`**
```go
signal := strat.EvaluateCandles(currentHistory)
```
`EvaluateCandles` is no longer a `Strategy` method. Either call `strategy.EvaluateCandles(strat, currentHistory)` (free function, position-agnostic), or — strongly preferred — delete this entire legacy simulation loop (`cmd/backtest/main.go` lines ~90-320: manual delta-model P&L, hardcoded `Delta=0.5`, `estPremium:=150.0`, `LotSize=50` — this is exactly the CR-9/ST-6/ST-10(M3) fake-P&L logic the audit flags) and replace it with a call into **`internal/backtest`**, which WP-7 has already built against this exact contract (`internal/backtest/engine.go:273` calls `strat.Evaluate(ctx)` with real `EvalContext`, Black-Scholes repricing, per-leg costs, and already compiles cleanly against my changes). Per the plan, `cmd/backtest/main.go` is supposed to become "a thin CLI" over `internal/backtest` — that migration hasn't happened yet.

### Silent behavioral breaks (compile clean, but WRONG under the new contract — higher risk than the compile errors above)

**3. `cmd/main.go:810-1020` — the `switch signal.Action { case strategy.Buy: ... case strategy.Sell: ... case strategy.Hold: ... }` block has NO `case strategy.Exit` and no `default`.**
Every strategy that now emits `Exit` (nine_twenty square-off/premium-stop, short_straddle premium-stop/RSI-exit, iron_fly RSI-exit, rsi_reversal mean-reversion exit) will have that signal silently discarded by this switch — Go executes zero cases when none match, no error, no log. **This means positions will never be squared off or stopped out via strategy-driven exits once WP-9 wires the new `Evaluate` call** — a critical live-money bug if shipped as-is. WP-9 must add a `case strategy.Exit:` that calls `te.ClosePosition(symbol)` (and clears `openPositions[symbol]`/`activeOptions[symbol]`), covering both single-symbol and multi-leg (`activeOptions[symbol]` list) positions.

**4. `cmd/main.go:811-908` — the existing `case strategy.Buy:`/`case strategy.Sell:` bodies contain "cover the opposite side" reversal logic** (e.g. `if posExists && currentPos.Side == risk.Sell { ...close short... }` under `case strategy.Buy:`). That logic encoded the OLD overloaded semantics (ST-8: "Buy" used to mean both open-long and cover-a-short). Under the new contract `Buy`/`Sell` are pure directional entries — a strategy will never emit `Buy` to mean "cover my short". WP-9 should remove the implicit-cover branches from `case strategy.Buy`/`case strategy.Sell` (a reversal, if desired, is the engine's own decision when it sees a directional entry signal while holding the opposite side — not something the strategy signals) and route all closes through the new `case strategy.Exit`.

**5. `cmd/main.go:500` and `internal/app/modes.go:25` — both call `strategy.Get(stratName)` for potentially the same name.**
Compiles fine (signature unchanged), but `Get` now returns a **cached singleton**. If both code paths are ever live at once (they appear to be alternate/duplicate loops per audit CR — `internal/app/modes.go`'s loop looks like the "dead duplicate loop" the plan's WP-9 task 1 says to delete), they will now share the exact same strategy instance and its internal state (entered flags, candle buffers) instead of getting isolated copies. This is almost certainly what you want once the duplicate loop is deleted, but flagging it since it's a behavior change from "always fresh" to "always shared."

**6. `cmd/backtest/main.go:128` — `strat, err := strategy.Get(stratName)`.**
Also now returns a singleton. If a backtest CLI process were ever invoked to run multiple sequential backtests over the same strategy name within one process (e.g. a future test harness or parameter sweep), state (e.g. nine_twenty's `entered`, sniper's candle buffers) would leak across runs. Call `strategy.Reset(stratName)` before each independent run, or construct via `New<X>Strategy()` directly for backtest isolation instead of going through the registry.

**7. `cmd/backtest/main.go:171,173,251` — `signal.Action == strategy.Buy`/`strategy.Sell` used to mean "close the existing directional leg".**
Same ST-8 semantic issue as item 4, inside the legacy simulation loop. Superseded by item 2 (this whole loop should be replaced by `internal/backtest`).

### Summary table

| File:Line | Kind | Fix owner |
|---|---|---|
| `cmd/main.go:677` | Compile error | WP-9 |
| `cmd/backtest/main.go:162` | Compile error | WP-7/WP-9 |
| `cmd/main.go:810-1020` (missing `case strategy.Exit`) | Silent behavior break, critical | WP-9 |
| `cmd/main.go:811-908` (stale Buy/Sell-means-cover logic) | Silent behavior break | WP-9 |
| `cmd/main.go:500`, `internal/app/modes.go:25` | Behavior change (singleton) | WP-9 |
| `cmd/backtest/main.go:128` | Behavior change (singleton) | WP-7/WP-9 |
| `cmd/backtest/main.go:171,173,251` | Stale semantics, superseded by item 2 | WP-7/WP-9 |

Packages verified to compile cleanly, unaffected: `internal/app`, `internal/engine`, `internal/cli`, `internal/api`, `internal/discovery`, `internal/backtest`.

## 5. Discrepancies vs. the audit / discretionary design calls

- **Audit line numbers drifted in a few places** (expected — other WPs had already touched adjacent code by the time I read it): `nine_twenty.go` was 89 lines pre-fix (audit cites `:15-16, 33-37, 59-66`) — matched fine, no functional drift. `sniper.go` pre-fix `updateCandle` was at line 146-171 (audit cites `:164`, `:146`) — matched.
- **`internal/backtest` already exists** (WP-7's new package, ~1470 lines) and its `engine.go:273` already calls `strat.Evaluate(ctx)` against `EvalContext`/`Signal` fields (`Exit`, `Hold`, `OrderLeg.Expiry`, `OrderLeg.Quantity`) that match my contract exactly, and it compiles cleanly against my changes with zero edits from me. This means WP-7 built ahead against the WP-6 contract as the plan anticipated ("if racing ahead, stub locally") — worth WP-9 confirming with WP-7's report whether their stub needs reconciling with anything here (I did not need to change anything to make it compile, so likely no reconciliation needed).
- **`EvalContext` has no position-Side/direction field.** The WP-6 spec's field list (`Symbol, Prices, Volumes, Candles, Now, HasPosition, PositionAge, EntryPremium`) has no way to tell a strategy whether an open position is long or short. `rsi_reversal.go`'s new mean-reversion exit needs this to mirror the exit condition correctly, so it tracks `lastDirection` internally (set when it emits `Buy`/`Sell`, cleared when `ctx.HasPosition` goes false). Documented in the file. If WP-9's engine ever restores a position without the strategy having chosen the side itself (e.g., after a restart before any entry signal in this process), `rsi_reversal`'s exit defaults to the "long" mirror — a known limitation, called out in-code.
- **"Current combined premium" is not a distinct `EvalContext` field** (see §3) — I read `ctx.LastPrice()` as the current combined premium for nine_twenty/short_straddle premium stops. This is the single most load-bearing interpretation call in this package; WP-9 must feed the right series into `Prices`/`Candles` for these two strategies once a position is open, or the premium stop-loss will silently never trigger (it's fail-safe/no-op when `EntryPremium<=0` or no price is available, not fail-open, so worst case is "stop doesn't fire," not "false stop fires").
- **Sniper's `TrailingSL` is not copied into a `Signal` field.** The spec says "wire the existing but currently-dead StopLossPct/TargetPct/TrailingSL fields into Signal.StopLossPrice/TargetPrice... don't leave them unused." `StopLossPct`/`TargetPct` map cleanly onto the two `Signal` fields the contract defines. `TrailingSL` is inherently a *stateful, ongoing* concept (ratchet the stop as price moves favorably) that doesn't fit a one-shot `Signal` struct with only two static price fields — there's no `Signal.TrailingStopPct` in the WP-6 spec's field list, and I didn't invent one unilaterally. `TrailingSL` remains an exported strategy parameter; `sniper.go`'s doc comment tells WP-9 to read `Signal.StopLossPrice` as the initial stop and use `SniperStrategy.TrailingSL` (percent) to ratchet it in the engine's ongoing position-management loop. Flagging this as the one field I did NOT wire into `Signal`, with the reasoning above, rather than silently doing something clever.
- **`ConfirmExit()` on `nine_twenty.go`** is an addition beyond the literal spec (which only names `ConfirmEntry()`). Added for the identical reasoning ST-4 gives for entries: flipping `entered=false` at Exit-signal-generation time (rather than at confirmed-exit-fill time) has the same "reject a rejected order and the strategy still thinks it's flat" failure mode. WP-9 can choose to call it or ignore it (if unused, `entered` still gets cleared eventually via the next day's full-date reset, and `pendingExit` just prevents duplicate Exit signals in the meantime — nothing breaks if `ConfirmExit` is never called, it's strictly additive safety).
- **`price_history.go`** required no functional changes for this WP (already thread-safe, no time/signal/indicator logic); touched only by `gofmt`.

## 6. Test evidence

```
$ cd go-engine && go build ./internal/strategy/... ./internal/broker/...
(clean, no output)

$ go vet ./internal/strategy/... ./internal/broker/...
(clean, no output)

$ go test -race ./internal/strategy/... ./internal/broker/...
ok  	titan-algo/internal/strategy	2.9s
ok  	titan-algo/internal/broker	5.9s
```

28 tests in `internal/strategy` (all new/updated for this WP), all passing under `-race`:
- `TestRSIBoundaryCases`, `TestRSIInsufficientData` — RSI 100/0/50 boundary cases.
- `TestSessionVWAPResetsAtDayBoundary` (hand-computed expected value), `TestSessionVWAPEmpty`.
- `TestMomentumNormalizationHandlesMissingVWAP`.
- `TestBullishEngulfing_StrictInequalityRequired` / `TestBearishEngulfing_StrictInequalityRequired` — proves a case that fires under the old `>=`/`<=` logic no longer fires.
- `TestBullishEngulfing_TrueEngulfingFires` / `TestBearishEngulfing_TrueEngulfingFires` / rejection cases.
- `TestSniperUpdateCandle_WallClockBoundaries` — wall-clock bucketing + volume delta + `Candle.Time`.
- `TestSniperEvaluate_LatchesWithinCandle` — one signal per completed candle.
- `TestSniperEvaluate_CandleModeBypassesAggregation`, `TestSniperEvaluate_EmptyContextReturnsHold`, `TestSniperAttachStops`.
- `TestNineTwentyEntryWindowGating`, `TestNineTwentyOutsideWindowNoEntry` — IST entry-window gating.
- `TestNineTwentyPremiumStopLossTriggers`, `TestNineTwentyPremiumStopLossNotTriggeredBelowThreshold`.
- `TestNineTwentySquareOffTime`, `TestNineTwentySnapshotRestoreRoundTrip`, `TestNineTwentyFullDateReset`.
- `TestShortStraddle_NoChurnWhilePositionOpen`, `TestShortStraddle_PremiumStopExitsWhilePositionOpen`.
- `TestIronFly_NoChurnWhilePositionOpen_AndHedgeFirstLegOrder`.
- `TestRegistryGetReturnsSingleton`, `TestRegistryReset`, `TestRegistryUnknownStrategy`.

11 tests in `internal/broker` for `historical.go` (plus WP-1's pre-existing `angel_broker`/`instruments` tests, unaffected, still passing):
- `TestParseCandleRows_ValidRowsAccepted`.
- `TestParseCandleRows_RejectsBadNumericStrings` — propagated parse error, not zero-fill.
- `TestParseCandleRows_RejectsNonPositivePrices`, `TestParseCandleRows_RejectsHighLessThanLow`.
- `TestParseCandleRows_RejectsOutOfOrderTimestamps`, `TestParseCandleRows_ShortRowSkipped`.
- `TestGetFloat_PropagatesParseErrors`.
- `TestTruncateToLastCompletedCandle_IntradayDuringSession/BeforeMarketOpen/AfterMarketClose/DailyBeforeClose/ConvertsNonISTInput`.

`go build ./...` from the module root fails only in `cmd/main.go` and `cmd/backtest/main.go`, per §4 above — everything else, including WP-7's `internal/backtest`, compiles clean against the new contract.
