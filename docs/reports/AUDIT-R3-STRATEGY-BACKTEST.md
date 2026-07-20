# Round 3 Audit — Strategy Layer & Backtest Engine

**Date:** 2026-07-20
**Scope:** `go-engine/internal/strategy/` (all 7 strategies: iron_fly, short_straddle, ema_crossover, rsi_reversal, momentum, nine_twenty, sniper, plus indicators.go, candlestick.go, registry.go, candle_aggregator.go, price_history.go, strategy.go), `go-engine/internal/backtest/` (engine.go, bs.go, iv.go, config.go, params.go, report.go, cache.go, charges.go, types.go).
**Method:** Read the current code directly (not prior audit comments taken on faith), cross-checked every claim against `internal/engine/runner.go` and `cmd/backtest/main.go` where the strategy/backtest contract's actual consumer mattered, read every `*_test.go` in scope to see what's exercised vs. asserted-but-wrong, and traced concrete input->output scenarios for each finding rather than reporting code-smell. No files were modified except this report.

**Context:** This package has already been through two hardening rounds (R2-2 fixed the sniper zero-trades bug and added parameterized constructors; R2-3 added real Black-Scholes IV inversion, cost-multiplier stress testing, and a fetch-data tool). The code is visibly disciplined: fail-closed patterns, extensive doc comments citing the specific prior finding each fix addresses, and thorough golden-value/round-trip tests for the Black-Scholes and IV-inversion math. Most of what a first pass would flag has already been fixed and is confirmed correct below. The findings that follow are the gaps that survived that scrutiny — several of them contract violations that span both directories (a strategy emits something neither `internal/backtest` nor `internal/engine` ever reads), which is exactly the kind of thing a single-file review misses.

One correction to the prior round's own paper trail: `docs/reports/R2-3-REPORT.md` §2 says implied-vol wiring into `engine.go`'s `priceLeg` was blocked by file ownership and never landed, and `internal/backtest/report.go`'s `ConstantIVBanner` doc comment repeats that claim. Reading the current code shows this is now stale — `internal/backtest/config.go`'s `Config.IVAt` is wired into `engine.go:100`'s `priceLeg`, and `cmd/backtest/main.go:307` does set `cfg.RealIVSeries` when `-option-csv` is supplied. The wiring happened (evidently in a later, unreported round); see F5 for the one real consequence of the comment not being updated to match.

---

## Findings, ranked by severity

### F1 — CRITICAL: `nine_twenty` and `rsi_reversal` keep un-keyed per-instance state, but the live engine runs one shared strategy singleton across *all* configured symbols

**Files:** `go-engine/internal/strategy/nine_twenty.go:28-42` (struct fields), `:97-184` (`Evaluate`); `go-engine/internal/strategy/rsi_reversal.go:20-28` (struct fields), `:70-149` (`Evaluate`); `go-engine/internal/strategy/registry.go:36-52` (`Get` — singleton cache); `go-engine/internal/engine/runner.go:153-158` (`strat, err := strategy.Get(cfg.StrategyName)` — one instance for the whole `Runner`), `:468-470` (`for _, symbol := range r.cfg.Symbols { r.evaluateSymbol(...) }` — the same `r.strat` evaluated for every symbol).

`NineTwentyStrategy` holds `entered`, `pendingExit`, `signaledEntry`, `lastDate`, `entryPremium` as flat struct fields with no symbol key. `RSIReversalStrategy` holds `lastDirection` the same way. Neither strategy ever reads `EvalContext.Symbol` (confirmed by reading both files in full — `ctx.Symbol` doesn't appear once in either). `registry.Get(name)` deliberately caches and returns **one shared instance** per strategy name (the doc comment even explains why: "fetching the same name twice must return the SAME instance, or in-memory state... is silently lost"). `Runner` fetches that singleton once in `NewRunner` and reuses it for every symbol in `r.cfg.Symbols` inside the tick loop.

`RunnerConfig.Symbols` is not a single-element convenience — `cmd/main.go:280-319` builds it from live discovery (`discovery.ScanTopChains(cfg.Brokers.Trading.Discovery.TopChainsCount)`, default 10 in `internal/config/config.go:311-312`) or an interactive multi-select (`internal/cli/interactive.go`'s `SelectedChains`), and `config.example.yaml:24` ships `top_n_symbols: 3` as the example default. Multi-symbol trading through one `Runner` is the intended, documented feature, not an edge case.

**Concrete failure scenario (nine_twenty, 2 symbols, e.g. NIFTY + BANKNIFTY):**
1. Tick at 09:20: `evaluateSymbol("NIFTY", ...)` — flat, inside the entry window — returns `Sell` with legs, and sets `s.signaledEntry = true` on the shared instance.
2. Same tick, `evaluateSymbol("BANKNIFTY", ...)` — also flat, also inside the window — but `!s.signaledEntry` is now `false` (NIFTY just set it), so BANKNIFTY silently gets `Hold` this tick instead of its own entry signal. No log line distinguishes "no signal because conditions aren't met" from "no signal because another symbol's flag is latched."
3. NIFTY's order fills; the integration layer calls `ConfirmEntry()` on the shared instance → `s.entered = true`.
4. Next tick (still inside or shortly after the 5-minute entry window), `evaluateSymbol("BANKNIFTY", ...)`: `ctx.HasPosition` is correctly `false` for BANKNIFTY (that book is keyed by symbol in `Runner.open`), but `inPosition := ctx.HasPosition || s.entered` evaluates to `true` because `s.entered` belongs to NIFTY, not BANKNIFTY. BANKNIFTY now permanently skips the entry-window branch for the rest of the day (the window is only 09:20-09:24; by the time this is discoverable, it's closed).
5. Net effect: BANKNIFTY never enters at all that day. This repeats for a 3rd symbol under `top_n_symbols: 3` unless it happens to be evaluated before the others in the same tick.

**Concrete failure scenario (rsi_reversal, 2 symbols):** the flat branch (`if !ctx.HasPosition { s.lastDirection = "" ... }`) unconditionally resets the shared `lastDirection` on **every** flat evaluation, regardless of which symbol is flat. If NIFTY enters `Sell` (short) and sets `lastDirection = Sell`, then BANKNIFTY is evaluated next while still flat, `lastDirection` gets reset to `""` before BANKNIFTY's own entry check runs. If BANKNIFTY doesn't also enter that same tick, NIFTY's exit logic (`switch dir { case Sell: ...; default: /* Buy or unknown */ ... }`) now reads `dir == ""`, falls into the `default` (long-position) exit branch, and applies the **wrong-direction** mean-reversion exit condition to NIFTY's actual short position (exits on `price > SMA` instead of `price < SMA`) — a real, silent mis-timed exit on a live short option position.

Neither is a data race (the tick loop evaluates symbols sequentially, one goroutine) — it is a plain logical state-aliasing bug: one physical struct instance is being asked to represent N independent per-symbol state machines.

**Test coverage gap:** every test in `nine_twenty_test.go` and every position-aware test in `position_aware_test.go` constructs its own fresh `NewNineTwentyStrategy()`/`NewShortStraddleStrategy()`/`NewIronFlyStrategy()` and only ever evaluates one symbol (`"NIFTY"`) against it. No test anywhere in the repo evaluates the same strategy instance for two different symbols in the same run, so this is entirely unexercised.

**Why the other 5 strategies are safe:** `short_straddle`, `iron_fly`, `ema_crossover`, `momentum` are stateless (all decision-relevant data comes from `ctx`, not struct fields), so a shared singleton is harmless for them. `sniper`'s only mutable state (`agg *CandleAggregator`) is already keyed by symbol internally (`Add(symbol, ...)`, `Completed(symbol)`), so it is also safe for multi-symbol use. `nine_twenty` and `rsi_reversal` are the only two that mix "one shared instance" with "un-keyed state."

**Fix direction (not applied — audit only):** either (a) key every mutable field by symbol (`map[string]*nineTwentyState`) inside the one shared struct, or (b) have `Runner`/registry hand out one strategy instance per symbol instead of one per strategy name for strategies that carry state — (a) is the smaller diff and matches the existing `CandleAggregator` pattern sniper already uses.

---

### F2 — HIGH: `Signal.StopLossPrice`/`TargetPrice` are set by `sniper.go` but read by nothing — the documented contract in `strategy.go` is silently violated in both the backtest and the live engine, and the backtest has no stop-loss mechanism at all for the three strategies that never emit `Exit`

**Files:** `go-engine/internal/strategy/strategy.go:100-108` (Signal doc: "A consumer (engine/backtest) MUST: ... Honor StopLossPrice / TargetPrice when non-zero by placing broker-side protective orders (SL-M) alongside the entry"); `go-engine/internal/strategy/sniper.go:232-255` (`attachStops` — the only place these fields are ever set); `go-engine/internal/backtest/engine.go` (whole file — `grep StopLossPrice|TargetPrice` returns zero matches); `go-engine/internal/engine/runner.go` (same grep, zero matches; live risk-based stop-loss instead comes from an unrelated global `risk.StopLossConfig.Value` at `runner.go:960-1025`).

`sniper.go` computes a real per-signal stop-loss/target (1%/2% of the reference close price by default) and attaches it to the `Signal`. That is the *only* strategy of the seven that ever populates these fields. Nothing downstream consumes them:

- `internal/backtest/engine.go`'s `Run()` switch on `signal.Action`/`signal.Legs` never inspects `signal.StopLossPrice`/`TargetPrice` anywhere — a directional position, once opened, closes only on an explicit `Exit` signal, an opposite-direction `Buy`/`Sell`, or the end-of-data forced close.
- `internal/engine/runner.go`'s live path is no better: `enterDirectional`/`placeSingleLeg` never reference the field either. The live engine's actual stop-loss (`placeBrokerStop`, `softStopLossCheck`) is a **global percentage** (`risk.StopLossConfig.Value`) applied uniformly to every open leg of every strategy — a completely independent mechanism from what sniper computed, so sniper's designed 1%/2% risk/reward is never the thing actually enforced live either.

**Compounding backtest/live divergence (item 6):** `ema_crossover` and `momentum` never emit `Exit` at all (only rely on an opposite crossover to close), and `sniper` never emits `Exit` either (only relies on its now-dead StopLossPrice/TargetPrice). `internal/backtest` has **zero** stop-loss mechanism of any kind (confirmed: no case-insensitive match for "stop loss" anywhere in the package) beyond what a strategy itself emits as an `Exit` signal. So for these three strategies specifically, a backtested position can run for the entire remaining dataset without any risk control, while the *same* strategy running live is bounded by the global `risk.StopLossConfig` percentage. Any backtest metric that depends on trade duration, drawdown, or win/loss distribution for `ema_crossover`/`momentum`/`sniper` is measuring a risk profile live trading will never actually experience — the backtest is structurally more optimistic (or at least differently shaped) than live for exactly the reason CR-11 in the original audit called out, and that gap was not closed by this round's per-strategy stop attachment; it was only moved to a field nobody reads.

**Test coverage gap:** no test in `internal/backtest` or `internal/strategy` asserts that a `Signal.StopLossPrice`/`TargetPrice` ever causes a position to close. `sniper_test.go`'s `TestSniperAttachStops` only checks that the field gets a value, never that anything acts on it.

**Fix direction:** either wire `StopLossPrice`/`TargetPrice` into `internal/backtest/engine.go`'s mark-to-market loop (check the last leg's/underlying's current price against it each candle, close if breached) and into `internal/engine/runner.go`'s order-placement path (place a broker-side SL-M at that exact level instead of/in addition to the global percentage), or remove the fields and the doc-comment contract if the global-percentage model is the intended design going forward — the current state (documented contract, populated field, zero consumers) is the worst of both.

---

### F3 — MEDIUM: multi-leg option expiry is resolved once at entry and never re-anchored; a position held past the assumed DTE window is silently priced as already-expired instead of erroring

**Files:** `go-engine/internal/backtest/engine.go:43-57` (`resolveExpiry`), `:105-142` (`openMultiLeg` — calls `resolveExpiry` once, stores the result in `openLeg.Expiry`), `:65-73` (`timeToExpiryYears` — clamps negative time-to-expiry to 0), `bs.go:37-38` (`Price` — `T <= minTimeToExpiry` returns pure intrinsic value).

None of the three multi-leg strategies (`iron_fly`, `short_straddle`) ever set `OrderLeg.Expiry` (confirmed: neither file assigns anything but the zero value to that field), so `resolveExpiry` always falls back to `asOf.AddDate(0, 0, cfg.DefaultDTEDays)` — a synthetic expiry `cfg.DefaultDTEDays` (default 7) calendar days after the candle the position opened on. That synthetic expiry is computed **once**, cached in `openLeg.Expiry`, and used for every subsequent `priceLeg`/`markToMarket`/`closePosition` call for the life of the position — it is never re-resolved against an actual expiry calendar and never rolled.

`short_straddle` and `iron_fly` have no calendar-based exit at all — they hold until an RSI-band breakout or (for `short_straddle`) a combined-premium stop. In a genuinely range-bound market, RSI can sit inside `[45, 55]` for far longer than 7 calendar days. Once `asOf` passes the cached `Expiry`, `timeToExpiryYears` clamps to `0` and every subsequent `Price()` call for that leg returns pure intrinsic value (`bs.go:37`) — i.e. the model silently starts treating the option as expired-worthless-or-intrinsic while the strategy still believes it holds an open, live structure. For an ATM straddle sold near the money, intrinsic value on both legs is close to zero, so the backtest's mark-to-market and eventual close P&L for the tail of a long hold understate what a real position (which would need a manual or auto roll to a new expiry, and would carry a real, non-zero premium the whole time) would actually show. This directly matches the project's own standing rule ("if something can't be determined correctly, it must error, not guess") — here the code guesses a made-up expiry date at entry and then never revisits that guess, even once its own consequence (T=0 forcing intrinsic pricing) becomes internally detectable.

**Test coverage gap:** `straddle_test.go`'s trending/flat straddle tests both run 200 five-minute candles (≈16-17 hours, well under `DefaultDTEDays=7`) and `sample_test.go`'s straddle demo holds for at most 6 daily candles (6 days, still under the default 7). No test holds a multi-leg position past its assumed DTE window, so this failure mode is unexercised.

**Fix direction:** either (a) have `Run()` force-close (or roll, if that's ever modeled) a multi-leg position once `asOf` reaches within some buffer of the cached expiry, mirroring what a real trader/broker would be forced to do, or (b) make `priceLeg` return an error (surfaced as a forced exit with a clear reason, not a silent intrinsic price) once time-to-expiry hits zero for a position the strategy itself never told the engine to close.

---

### F4 — LOW/MEDIUM: no sanity validation of candle prices anywhere in `internal/backtest` — a zero/negative `Open` silently produces `NaN` P&L with no error

**Files:** `go-engine/internal/backtest/cache.go:13-54` (`LoadCandlesCSV` — validates parseability, not value sanity), `bs.go:36-46` (`Price` — `math.Log(p.Spot/p.Strike)` with `p.Spot < 0`), `engine.go` (whole file — no `Open <= 0`/`math.IsNaN` check anywhere).

`LoadCandlesCSV` only checks that each column parses as a valid float/int; it never checks `Open`/`High`/`Low`/`Close > 0` or `High >= Low` or that `Close` falls within `[Low, High]`. `Run()`'s main loop reads `fillCandle.Open` directly into `priceLeg`'s `Spot` parameter with no guard. For `Spot == 0`, `Price()` happens to converge to the correct mathematical limit (verified by hand: `d1/d2 -> -Inf`, `normCDF -> 0`, giving `Price -> 0` for calls and `Price -> discStrike` for puts — not a bug). But for `Spot < 0` — e.g. a corrupted historical-data row, a hand-edited cache CSV, or a genuine bad print from a data source — `math.Log(negative/positive)` returns `NaN` in Go, which propagates through `d1`, `normCDF`, and the final `Price()` return as `NaN`. That `NaN` then flows, uncaught, into `openLeg.EntryPremium`, every subsequent `legPnL`, `Trade.GrossPnL`/`NetPnL`, and `recomputeAggregates`'s running sums — silently poisoning `Report.NetPnL`/`ProfitFactor`/`Expectancy` for the *entire run* (once a sum contains `NaN`, it stays `NaN`), with no error, no log line, and no test that would catch it (`grep -i "IsNaN\|validate"` across the package returns nothing).

This is a lower-probability trigger than F1-F3 (it requires genuinely malformed input data, which the normal broker-fetch/cache-load path shouldn't produce), but it is a direct, traceable violation of "if something can't be determined correctly, it must error, not guess" — a `NaN` in a P&L report is worse than a wrong number, because it can silently poison an otherwise-plausible-looking report (e.g. `NetPnL: NaN` prints as literal `NaN` via `%.2f`, which an operator might notice, but any code that consumes `Report` programmatically rather than printing it — e.g. a future automated go/no-go gate — would not).

**Fix direction:** validate every loaded/fetched candle (`Open, High, Low, Close > 0`, `High >= Low`, `Low <= Open, Close <= High`) in `LoadCandlesCSV`/wherever candles first enter the package, and return an error rather than silently accepting the row; defensively, `Run()` could also refuse to start (or force-flatten and error) if any candle in the range fails that check.

---

### F5 — LOW: `ConstantIVBanner` is printed unconditionally, and its own claim ("every option leg... repriced at a single fixed IV") is now stale in the one case that matters — when real per-bar IV *was* successfully loaded

**Files:** `go-engine/internal/backtest/report.go:143-164` (`ConstantIVBanner` — doc comment and body), `go-engine/cmd/backtest/main.go:249-272` (unconditional `fmt.Println(backtest.ConstantIVBanner(cfg.IV))` regardless of `realIVLoaded`).

As noted in this report's intro, the IV series *is* now wired into `priceLeg` via `Config.IVAt` (contradicting both the R2-3 report and this function's own doc comment, which both still say the wiring "cannot be" done this round). The practical consequence: when an operator supplies `-option-csv` and it loads successfully, `cmd/backtest/main.go` prints a `[REAL IV]` block correctly explaining that *one* matching leg now uses real per-bar IV and every other leg still uses the constant fallback — and then, immediately below it, prints `ConstantIVBanner` unconditionally, which flatly states "every option leg below is repriced all run at a single fixed IV," directly contradicting the block just printed above it. For a single-leg strategy (sniper's synthetic directional leg, or any future single-leg strategy) with a correctly-loaded real IV series, the banner's core claim is simply false for that run.

This is cosmetic (it doesn't change any number in the report, only the surrounding text), but it's exactly the kind of stale, code-contradicting claim the task asked to be skeptical of, and it would mislead an operator trying to decide how much to trust a given backtest run's IV assumption.

**Fix direction:** make `ConstantIVBanner` (or its caller) aware of whether `cfg.RealIVSeries` is non-nil and adjust the wording (e.g. "every leg EXCEPT strike X/expiry Y/type Z below..." when real IV is loaded), or gate printing the pure constant-IV banner on `!realIVLoaded`.

---

## Confirmed correct (spot-checked, not re-flagged)

- **Sniper candle-priority fix (R2-2):** `sniper.go:120` genuinely prefers `ctx.Candles` unconditionally now; both `internal/backtest/engine.go:281` and (per R2-2's report) `internal/engine/runner.go` populate `Candles` alongside `Prices`, so the fix is load-bearing, not dead code again. `TestSniper_CandlesTakePriorityOverPrices` actually proves the regression this fixes.
- **Black-Scholes (`bs.go`) and IV inversion (`iv.go`):** golden-value tests match textbook Hull reference values and put-call parity to `1e-9`; `ImpliedVol`'s bisection round-trips correctly and fails closed (never guesses) on below-intrinsic or implausibly-high market prices. No issues found.
- **ST-5 (position-aware re-signaling) and CR-12 (hedge-leg ordering):** `iron_fly`/`short_straddle`/`nine_twenty` correctly gate entries on `!ctx.HasPosition` and exits on `ctx.HasPosition`; `iron_fly`'s leg slice genuinely places BUY hedges before SELL body legs. Backtest engine's own `switch` in `Run()` additionally suppresses same-direction re-entry churn for directional signals regardless of strategy statefulness (this is why `momentum`, which never itself consults `ctx.HasPosition`, doesn't churn in the backtest — the suppression lives in the engine, not the strategy, which is fine but worth knowing when reasoning about `momentum`'s behavior in isolation).
- **RSI edge cases (ST-1), engulfing pattern strictness (ST-10), session-anchored VWAP (ST-2):** all match their doc comments' stated fix and are exercised by tests that would fail under the old behavior.
- **Fill timing (ST-7):** `TestFillAtNextOpen` genuinely proves fills happen at the next candle's `Open`, not the signal candle's `Close`.
- **Charges (`charges.go`):** delegates to `internal/risk.EstimateCharges` with the correct `OptCarry`/side mapping; no unit (paise/rupee) or lot-size double-counting found — `Quantity` is consistently "shares = lots × lot size" from `OrderLeg.Quantity` (lots) through `openMultiLeg`'s `qty := qtyLots * cfg.LotSize` onward.

---

## Summary table

| # | Severity | Finding | Files |
|---|---|---|---|
| F1 | CRITICAL | `nine_twenty`/`rsi_reversal` share un-keyed state across a multi-symbol `Runner`, corrupting entries/exits between symbols | `nine_twenty.go`, `rsi_reversal.go`, `registry.go`, `runner.go` |
| F2 | HIGH | `Signal.StopLossPrice`/`TargetPrice` documented as mandatory, honored by nothing; backtest has no stop-loss for 3 of 7 strategies | `sniper.go`, `strategy.go`, `backtest/engine.go`, `engine/runner.go` |
| F3 | MEDIUM | Synthetic expiry fixed at entry, never rolled; long-held multi-leg positions silently reprice to intrinsic post-assumed-expiry | `backtest/engine.go`, `bs.go` |
| F4 | LOW/MEDIUM | No candle price sanity check; negative `Open` silently yields `NaN` P&L | `backtest/cache.go`, `bs.go`, `engine.go` |
| F5 | LOW | `ConstantIVBanner` printed unconditionally, contradicts the real-IV info block when `-option-csv` loads successfully | `backtest/report.go`, `cmd/backtest/main.go` |
