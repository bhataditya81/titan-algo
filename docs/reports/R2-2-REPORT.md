# R2-2 — Sniper Fix + Strategy Parameterization — Report

Scope: `internal/strategy/*` (all files). No edit to `internal/backtest/engine.go` was
needed (see §2) — its EvalContext-construction section was read in full and found already
correct against the new contract, so it was left untouched, per the hard constraint that I
should only touch what's necessary and note the rest.

## 1. Root cause, confirmed by reading the actual code (not just the docs)

Read `internal/strategy/strategy.go`, `sniper.go`, `candlestick.go`, `internal/engine/runner.go`
(`evaluateSymbol`, lines ~530-593), and `internal/backtest/engine.go`'s `Run()` (lines ~247-330)
in full before changing anything, per the task's mandatory-reading requirement.

**Correction to the plan doc's assumption:** `EvalContext.Candles` already existed —
WP-6 added it, and `sniper.go` already had a "candle-mode" branch. The bug was narrower and
more precise than "Candles doesn't exist yet": the branch's gate was

```go
if len(ctx.Prices) == 0 && len(ctx.Candles) > 0 { ... use ctx.Candles directly ... }
```

Both callers that matter always populate `ctx.Prices` too:
- `internal/engine/runner.go:561-571` (`evaluateSymbol`) always sets `ctx.Prices` from
  `r.priceHistory` (tick data), for every strategy, every call.
- `internal/backtest/engine.go:276-289` sets `ctx.Candles = history` (the real historical
  bars) **and** `ctx.Prices = closesOf(history)` (when flat) or the combined-premium series
  (when in a position) — also always non-empty.

So `len(ctx.Prices) == 0` was **never true** for either real caller, and the candle-mode
branch was dead code. Every call fell through to sniper's own tick-aggregation fallback
(`updateCandle`), which — when driven one candle-close-as-one-tick per `Evaluate` call
(exactly `internal/backtest/engine.go`'s driving pattern: one `Evaluate` call per historical
bar) — builds a candle from a single data point: `Open=High=Low=Close`, a structural doji.
`IsHammer`/`IsShootingStar`/`IsBullishEngulfing`/`IsBearishEngulfing` (candlestick.go) all
require `totalRange > 0` or strict body/wick inequalities that a doji can never satisfy. Zero
trades in all 24 WP-10 walk-forward windows, confirmed mechanism.

One nuance vs. WP-10's wording ("near-identical consecutive ticks... zero-range doji"): the
tick-aggregation code itself (`updateCandle`, now `CandleAggregator.Add`) was already
correctly tracking real min/max/open/close across ticks *within* a bucket — verified by
`TestSniperUpdateCandle_WallClockBoundaries`, which predates this WP and already asserted
volume-delta and bucket-boundary behavior. The doji wasn't an aggregation-math bug; it was a
**single-sample-per-bucket** artifact of the calling pattern (backtest evaluates once per
historical bar, so one "tick" == one whole bar's worth of time). This is a real distinction
because it changes the fix: the aggregation code didn't need repair, the *priority order*
between Candles and Prices did.

## 2. The fix

### `internal/strategy/sniper.go`

Changed the mode-selection gate from `len(ctx.Prices) == 0 && len(ctx.Candles) > 0` to simply
`len(ctx.Candles) > 0`. Candles now wins unconditionally whenever the caller supplies any —
Prices being simultaneously populated (as both real callers do) no longer suppresses it. The
tick-aggregation fallback now only runs when `ctx.Candles` is completely empty.

```go
func (s *SniperStrategy) Evaluate(ctx EvalContext) Signal {
	if len(ctx.Candles) > 0 {
		if len(ctx.Candles) < s.EMAPeriod+2 {
			return Signal{Action: Hold, Reason: "Insufficient History"}
		}
		sig := s.EvaluateLogic(ctx.Candles)
		if sig.Action == Buy || sig.Action == Sell {
			s.attachStops(&sig, ctx.Candles[len(ctx.Candles)-1].Close)
		}
		return sig
	}
	// degraded fallback: tick-aggregation via s.agg, only when Candles is empty
	...
}
```

Because `internal/backtest/engine.go` already sets `ctx.Candles = history` (real OHLC bars)
on every call, **this one-line priority change alone makes sniper trade in backtests** — no
change to `internal/backtest/engine.go` was required. I verified this is genuinely the case
by re-reading `Run()`'s EvalContext construction line by line (§4 below); it was not an
assumption.

### Tick-aggregation fallback: extracted into `internal/strategy/candle_aggregator.go`

Sniper's own private per-instance aggregation (`sniperCandleState`, `s.candles`/`s.building`
maps, `floorToInterval`) was factored out into a new, generic, thread-safe
`CandleAggregator` type (same real-range OHLC math, unchanged behavior — same
`Add(symbol, price, cumVolume, now) bool` semantics, same real min/max/open/close tracking,
same cumulative-volume-delta handling). `SniperStrategy` now holds one `*CandleAggregator`
instance instead of its own maps + mutex. This was done for two reasons:

1. **DRY / fix-once**: the exact same generic tick→candle aggregation logic is what
   `internal/engine/runner.go` needs to build in order to give the live path real Candles
   too (see §3) — better to have one tested, reusable type than sniper's private copy plus a
   second hand-rolled one in `runner.go`.
2. It let me delete `SniperStrategy`'s own mutex entirely (`CandleAggregator` does its own
   locking), a small simplification.

`CandleAggregator` API:
```go
func NewCandleAggregator(interval time.Duration, maxKeep int) *CandleAggregator
func (a *CandleAggregator) Add(symbol string, price, cumVolume float64, now time.Time) bool
func (a *CandleAggregator) Completed(symbol string) []Candle
```
`Add` returns `true` exactly on the tick that completes a bucket (crosses an interval
boundary). `Completed` returns a defensive copy of the finished-candle history for a symbol,
oldest→newest. `maxKeep=0` means unbounded (matches the pre-existing, never-capped behavior
of sniper's old `s.candles` map).

## 3. EXACT required change in `internal/engine/runner.go` (for R2-INT — I did not make this edit)

**File:** `internal/engine/runner.go`
**Function:** `Runner.evaluateSymbol` (currently ~line 530-593), plus `Runner` struct
(~line 128-139) and `NewRunner` (~line 145-158).

Today, `evaluateSymbol` only ever populates `ctx.Prices`/`ctx.Volumes` from
`r.priceHistory` (a raw tick series) — it never sets `ctx.Candles`. This is why sniper (and
any future candle-only strategy) runs in degraded tick-aggregation mode live, forever, even
after this WP's fix. To close that gap:

1. Add a field to the `Runner` struct: `candleAgg *strategy.CandleAggregator`.
2. In `NewRunner`, construct it: `candleAgg: strategy.NewCandleAggregator(5*time.Minute, cfg.HistorySize)`
   (use whatever bar size the deployment wants live strategies to see — 5 minutes matches
   sniper's own default `CandleMinutes`; make it a `RunnerConfig` field if different
   strategies need different bar sizes, or hardcode 5m if one size is acceptable for now).
3. In `evaluateSymbol`, immediately after each `r.priceHistory.Add(symbol, price, volume)`
   call (there are two call sites, lines ~561 and ~566), add:
   `r.candleAgg.Add(symbol, price, volume, nowIST)`.
4. Immediately after constructing `ctx := strategy.EvalContext{...}` (line 541) and before
   `sig := r.strat.Evaluate(ctx)` (line 574), add: `ctx.Candles = r.candleAgg.Completed(symbol)`.

**Do NOT remove the existing `ctx.Prices`/`ctx.Volumes` population** — `nine_twenty` and
`short_straddle`'s premium-based stops read `ctx.LastPrice()`, which prefers `Prices` over
`Candles` (see `strategy.go`'s `LastPrice()`), and that contract must keep working exactly as
WP-6/WP-9 built it. This change is purely additive: `ctx.Candles` gets populated *alongside*
the existing fields, giving `sniper` (and any future Candles-primary strategy) a real
same-symbol OHLC buffer to trade off of, without touching any other strategy's contract.

This is a ~4-line change once `strategy.CandleAggregator` exists (it does, as of this WP) —
I did not make it myself because `internal/engine/runner.go` is outside my file ownership for
this work package.

## 4. `internal/backtest/engine.go` — confirmed no edit needed

Read `Run()` (lines 247-330) in full. It already does exactly the right thing:

```go
ctx := EvalContext{
    Symbol:       cfg.Symbol,
    Candles:      history,       // real OHLC bars, growing prefix, every call
    Now:          evalCandle.Time,
    HasPosition:  pos != nil,
    EntryPremium: combinedEntryPremium(pos),
}
if pos != nil {
    ...
    ctx.Prices = premiumHistory // combined premium series, for nine_twenty/short_straddle stops
} else {
    ctx.Prices = closesOf(history)
    ctx.Volumes = volumesOf(history)
}
```
(`EvalContext`/`Candle`/`Strategy` here are type aliases to the `strategy` package's types —
confirmed in `internal/backtest/types.go`.) Since sniper's fix makes it prefer `Candles`
unconditionally, and sniper does not consult `ctx.HasPosition` (it has no position-aware
premium-stop logic — that's only nine_twenty/short_straddle/iron_fly), there is no conflict
between the "combined premium in Prices while in-position" contract and sniper's own use of
Candles. No changes were made to this file.

## 5. New per-strategy constructor signatures (for R2-3 / G-5)

All seven strategies now follow the same pattern: a `<Strategy>Params` struct whose zero
value reproduces today's hardcoded defaults exactly, a `New<Strategy>(params) *<Type>`
constructor, and the original zero-arg `New<Strategy>Strategy()` preserved (delegating to the
new constructor with a zero-value Params) so no existing caller anywhere in the codebase
needed to change.

| Strategy name | Params struct | Constructor | Returns |
|---|---|---|---|
| `ema_crossover` | `EMACrossoverParams{FastPeriod, SlowPeriod int}` | `NewEMACrossover(EMACrossoverParams) *EMACrossoverStrategy` | defaults: 9, 21 |
| `rsi_reversal` | `RSIReversalParams{Period, ExitSMA int; Oversold, Overbought float64}` | `NewRSIReversal(RSIReversalParams) *RSIReversalStrategy` | defaults: 2, 5, 10, 90 |
| `momentum` | `MomentumParams{RSIPeriod, MACDFast, MACDSlow, MACDSignal, BollingerPeriod int; RSIOversold, RSIOverbought, BollingerStdDev, MinSignalStrength float64}` | `NewMomentum(MomentumParams) *MomentumStrategy` | defaults: 14, 12, 26, 9, 20, 35, 65, 2.0, 0.6 |
| `nine_twenty` | `NineTwentyParams{EntryHour, EntryMinute, SquareOffHour, SquareOffMinute int; StopMultiplier float64}` | `NewNineTwenty(NineTwentyParams) *NineTwentyStrategy` | defaults: 9, 20, 15, 15, 1.4 |
| `sniper` | `SniperParams{EMAPeriod, RSIPeriod, CandleMinutes int; StopLossPct, TargetPct, TrailingSL float64}` | `NewSniper(SniperParams) *SniperStrategy` | defaults: 50, 14, 5, 1.0, 2.0, 0.5 |
| `iron_fly` | `IronFlyParams{WingWidth int; RSILower, RSIUpper float64}` | `NewIronFly(IronFlyParams) *IronFlyStrategy` | defaults: 200, 45, 55 |
| `short_straddle` | `ShortStraddleParams{RSILower, RSIUpper, StopMultiplier float64}` | `NewShortStraddle(ShortStraddleParams) *ShortStraddleStrategy` | defaults: 45, 55, 1.4 |

**Zero-value-means-default caveat** (documented in each file): a params struct has no way to
distinguish "field not set" from "field explicitly set to its zero value" (e.g.
`NineTwentyParams{EntryMinute: 0}` cannot select "on the hour exactly" — it falls back to the
default 20). This is the standard, accepted limitation of the Go options-struct pattern and
is called out in each `<Strategy>Params` doc comment. None of these strategies' real defaults
are legitimately 0, so this never bites in practice.

`registry.Get(name)` internally calls `New<Strategy>(<Strategy>Params{})` for every strategy
(updated each `init()`), so existing callers are provably unaffected — see the
`TestParamsZeroValueMatchesOldDefaults` test in §7.

## 6. `registry.GetWithParams` — exact signature and design

```go
func GetWithParams(name string, params map[string]float64) (Strategy, error)
```

Chosen design (documented in `registry.go`):
- **Not the singleton cache.** `GetWithParams` always builds a fresh, independent instance —
  never touches or mutates the `Get`/`Reset` singleton map. Parameter-sweep tooling (R2-3's
  backtest CLI) needs N independent instances per process (one per grid point), not one
  shared/mutated instance; conflating the two would silently corrupt sweeps.
- **Per-strategy `ParamFactory`, not a big switch here.** Each strategy's `init()` also calls
  a new `RegisterParams(name, factory)` alongside `Register(name, factory)`. `GetWithParams`
  just looks up and calls the registered factory — `registry.go` has zero strategy-specific
  knowledge.
- **Generic field-mapping via `reflect`, not hand-written per-strategy switches.** Each
  strategy's `ParamFactory` body is 3 lines: build a zero `<Strategy>Params{}`, call
  `applyParams(&p, params)`, construct via `New<Strategy>(p)`. `applyParams` (in
  `registry.go`) uses `reflect.Value.FieldByName` to copy each map entry onto the matching
  exported field (`float64` fields via `SetFloat`, `int` fields via `SetInt(int64(v))`), and
  returns an error naming the first key that doesn't match any field on that struct — this is
  the "fail clearly on a typo" requirement. Stdlib only (`reflect`), no new dependency.

Error cases, both proven by tests (§7):
- Unregistered strategy name → `strategy '<name>' not found. Available: [...]`.
- Unknown parameter key (typo) → `<strategy>: unknown parameter "<key>"`.

Usage example for R2-3's CLI (`-params FastPeriod=5,SlowPeriod=30`):
```go
strat, err := strategy.GetWithParams("ema_crossover", map[string]float64{
    "FastPeriod": 5, "SlowPeriod": 30,
})
```

## 7. Test evidence

```
$ cd go-engine && go build ./internal/strategy/...
(clean)
$ go vet ./internal/strategy/...
(clean)
$ go test -race -count=1 ./internal/strategy/...
ok  	titan-algo/internal/strategy	2.4s   (33 tests, all pass)
```

Whole-module check (not required to pass per the task, but confirmed clean since no other WP
had landed conflicting changes at the time of this work):
```
$ go build ./...       (clean)
$ go vet ./...          (clean)
$ go test -race -count=1 ./internal/backtest/...
ok  	titan-algo/internal/backtest	2.8s
```

New/changed tests, `internal/strategy` package:
- `TestSniper_HammerThenEngulfing_CandleMode_EmitsBuy` — synthetic 8-candle series (clean
  uptrend, a textbook Hammer at index 5, a small dip, a textbook Bullish Engulfing at index
  7), driven exactly like `internal/backtest/engine.go` drives strategies (growing prefix of
  `ctx.Candles`, one bar at a time). Asserts at least one `Buy` signal — **this is the direct
  regression test for the "sniper produced zero trades in all 24 WP-10 windows" bug.**
- `TestSniper_CandlesTakePriorityOverPrices` — same series, but additionally populates
  `ctx.Prices` on every call (exactly reproducing what both `runner.go` and
  `backtest/engine.go` really do). Proves the priority-gate fix specifically: before this fix
  this test would have produced zero Buy signals (Candles ignored because Prices was
  non-empty); after the fix it passes.
- `TestSniper_FlatDojiSeries_NoSignal` — 12 candles, all `Open=High=Low=Close=100`. Asserts
  zero Buy/Sell signals ever (only Hold), proving the patterns genuinely gate on real
  candle shape and don't false-positive on flat data.
- `TestCandleAggregator_TickFallbackBuildsRealRangeCandles` — feeds 4 varying-price ticks
  into one bucket, then a tick that crosses into the next bucket; asserts the completed
  candle has `Open=100, Close=101, High=103, Low=98` (i.e. `High != Low`, a genuine range),
  not a doji.
- `TestSniper_SingleTickPerBucket_IsLegitimateDoji_NotABug` — documents that ONE tick per
  bucket legitimately produces `O=H=L=C` (correct behavior, not the bug — the bug was the
  priority gate, fixed above).
- `TestSniperUpdateCandle_WallClockBoundaries`, `TestSniperEvaluate_LatchesWithinCandle`,
  `TestSniperEvaluate_CandleModeBypassesAggregation`, `TestSniperEvaluate_EmptyContextReturnsHold`,
  `TestSniperAttachStops` — pre-existing WP-6 tests, still pass unchanged (one, at line ~32,
  was updated to call `s.agg.Completed("NIFTY")` instead of reaching into the now-removed
  private `s.candles` map — same assertion, new internal API).
- `TestParamsZeroValueMatchesOldDefaults` (new, `params_test.go`) — constructs all seven
  strategies via `New<Strategy>(<Strategy>Params{})` and asserts every field equals the
  pre-change hardcoded constant.
- `TestGetWithParams_HappyPath_OverridesOnlyGivenFields` — overrides `FastPeriod` only,
  confirms `SlowPeriod` keeps its default.
- `TestGetWithParams_ReturnsIndependentInstances_NotTheSingleton` — two `GetWithParams` calls
  with different `WingWidth` produce two distinct, correctly-parameterized instances, and
  `Get("iron_fly")`'s singleton is confirmed unaffected (still default `WingWidth=200`).
- `TestGetWithParams_UnknownStrategyName_Errors`, `TestGetWithParams_UnknownParameterKey_Errors`.

All existing pre-R2-2 tests in the package (registry, indicators, candlestick, nine_twenty,
short_straddle/iron_fly position-aware tests) pass unchanged.

## 8. Files changed

- `internal/strategy/strategy.go` — **not modified**. `EvalContext.Candles` already existed
  (WP-6); no new field was needed.
- `internal/strategy/sniper.go` — mode-priority fix (Candles-first), refactored to use the
  new `CandleAggregator` instead of private maps/mutex, added `SniperParams`/`NewSniper`.
- `internal/strategy/candle_aggregator.go` — **new file.** Generic tick→candle aggregator,
  extracted from sniper's old private logic.
- `internal/strategy/registry.go` — added `ParamFactory`, `RegisterParams`, `GetWithParams`,
  `applyParams` (reflect-based field copier).
- `internal/strategy/ema_crossover.go`, `rsi_reversal.go`, `momentum.go`, `nine_twenty.go`,
  `iron_fly.go`, `short_straddle.go` — added `<Strategy>Params` + `New<Strategy>` per §5;
  `init()` updated to register both the plain and parameterized factories.
- `internal/strategy/sniper_test.go` — one existing test's internal-field access updated
  (`s.candles["NIFTY"]` → `s.agg.Completed("NIFTY")`); five new tests appended (§7).
- `internal/strategy/params_test.go` — **new file.** Five new tests (§7).
- `internal/backtest/engine.go` — **not modified** (§4: verified already correct).

## 9. Discrepancies from the plan / WP-10

- The plan doc (`PRODUCTION_GAPS_R2.md` §3, R2-2 task 1) hedges with "if it exists — check
  strategy.go; if it doesn't exist yet, you'll need to add it" for `EvalContext.Candles`. It
  already existed (WP-6). No field was added; only the consumption priority in `sniper.go`
  changed.
- Per the same hedge ("If it requires internal/backtest/engine.go's EvalContext-construction
  to also pass Candles, you own that section"): it did not require a change there — `Candles`
  was already being passed correctly. Flagged in §4 rather than making an unnecessary edit.
- WP-10's phrasing ("near-identical consecutive ticks... zero-range doji candles") is correct
  as an observed symptom but describes the mechanism loosely; the more precise mechanism
  (confirmed by reading `updateCandle`/`CandleAggregator` and the priority gate) is: the
  aggregation math was always correct given real sub-bucket ticks, but (a) it was
  unreachable in preference to the equally-broken Candles gate, and (b) even when reached,
  a caller that advances one full historical bar per `Evaluate` call feeds exactly one
  synthetic "tick" per bucket, which is mathematically a doji by definition (no second data
  point). This distinction mattered for deciding what to fix: the gate, not the math.
- The task's acceptance criteria mentions `go build ./internal/backtest/...` "may not pass
  depending on whether R2-3 has landed" — at the time of this work, `internal/backtest`
  compiled and tested clean against my changes (§7), since `internal/backtest/types.go`
  type-aliases directly to `strategy.EvalContext`/`Candle`/`Strategy` and I made no breaking
  changes to any of those types (only added new constructors/functions, which is additive).
