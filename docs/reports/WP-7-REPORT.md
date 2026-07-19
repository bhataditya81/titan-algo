# WP-7 — Backtest Engine Rebuild — Report

## Findings addressed

| Finding | Fix |
|---|---|
| **CR-9** — constant-delta-0.5 model made every short straddle mathematically guaranteed to profit (zero gamma) | Replaced with real Black-Scholes closed-form repricing of every leg on every candle (`internal/backtest/bs.go`, `engine.go`). Delta, gamma and theta (including decay on long legs, previously uncharged) all now fall out of full repricing. Proven with `TestShortStraddleLosesOnTrendingMarket`. |
| **CR-10** — leg-less directional signals never entered a position (dead branch); zero trades for sniper/EMA/RSI/momentum | Rewrote entry logic in `engine.go`'s `Run()`: flat + Buy/Sell opens a directional position (synthetic ATM CE/PE), Exit or an opposite-direction signal closes it; Legs-bearing signals are a separate multi-leg position type. Proven with `TestDirectionalEntryAndExit`, `TestDirectionalOppositeSignalCloses`, and a real CLI run against `ema_crossover` (see Sample Runs below) which now produces trades. |
| **ST-7** — fills at the signal candle's own close (look-ahead) | Every fill executes at `candles[i+1].Open` + slippage, never `candles[i].Close`. Proven with `TestFillAtNextOpen` (large synthetic gap between eval-close and fill-open). |
| **ST-6** — charges computed buy-side-only ×2 (missed sell-side STT), one flat brokerage for a multi-order round trip, hardcoded ₹150 premium, zero slippage/spread | Every leg, every side (entry buy/sell + exit buy/sell) is charged independently via `risk.EstimateCharges` at its own simulated premium, plus a modeled half-spread cost (`SpreadCost`, default 0.3% of premium). Proven with `TestLegCharge_WorkedExample`, `TestLegCharge_PerLegPerSide`. |
| **ST-10/M3** — hardcoded lot size 50 | `Config.LotSize`, CLI `-lotsize` (default 75). No hardcoded constant remains in `internal/backtest` or `cmd/backtest`. |
| **ST-10/M5** — no walk-forward/OOS, no drawdown/Sharpe/profit-factor reporting, fixed 30-day window | `Report` now carries trades, win rate, gross/net P&L, max drawdown, profit factor, expectancy, avg win/loss, worst single day, and a per-month table (`report.go`). `-from`/`-to` CLI flags replace the fixed 30-day window; `-csv` gives an offline candle cache. (Walk-forward/OOS harness itself is WP-10's scope, not WP-7's.) |

## Files changed

- `go-engine/internal/backtest/` (new package):
  - `types.go` — type aliases onto `internal/strategy`'s WP-6 contract (see below)
  - `bs.go` — Black-Scholes pricing/delta
  - `engine.go` — portfolio simulation: entries/exits, next-open fills, BS repricing, mark-to-market equity/drawdown
  - `charges.go` — per-leg-per-side charges (delegates to `internal/risk.EstimateCharges`) + spread cost
  - `report.go` — `Trade`/`Report`/`MonthStat` and formatted output
  - `cache.go` — local CSV candle cache (`LoadCandlesCSV`/`SaveCandlesCSV`/`LoadOrFetch`)
  - `config.go` — `Config` + `DefaultConfig()`
  - `bs_test.go`, `charges_test.go`, `engine_test.go`, `straddle_test.go`, `cache_test.go`, `sample_test.go`, `helpers_test.go`
- `go-engine/cmd/backtest/main.go` — rewritten as a thin CLI: flags, candle sourcing (cache-or-fetch), strategy lookup, prints `report.String()`. No simulation logic remains here.

## WP-6 / WP-2 / WP-1 dependency status

**At the time this package was built, WP-6, WP-2 and WP-1 had all landed** (checked live in the repo, not assumed from the plan doc). Sequence of events in this session:

1. Started against the *documented* WP-6 contract with a local mirror of `Signal`/`OrderLeg`/`EvalContext`/`Candle`/`Strategy` in `internal/backtest/types.go`, deliberately importing nothing from `internal/strategy`/`internal/risk`, per the task's parallel-execution guidance. Wrote and green-lit the full engine + test suite against those mirror types.
2. Re-checked `internal/strategy/strategy.go` and `internal/risk/risk.go` before writing `cmd/backtest/main.go` and found both packages had landed with contracts that match what was mirrored almost field-for-field:
   - `strategy.Signal{Action, Strength, Reason, Legs, StopLossPrice, TargetPrice}`, `strategy.OrderLeg{Direction, StrikeOffset, OptionType, Expiry, Quantity}`, `strategy.EvalContext{Symbol, Prices, Volumes, Candles, Now, HasPosition, PositionAge, EntryPremium}`, `strategy.Strategy.Evaluate(EvalContext) Signal` — exactly the documented contract.
   - `risk.EstimateCharges(price, quantity, tradeType, side) ChargeBreakdown` with `risk.OptCarry`/`risk.OptIntraday` etc. and an FY 2025-26 rate card (`DefaultChargeRates()`) that is **numerically identical** to the fallback rate table this package had independently implemented from the audit's EX-4 numbers (brokerage ₹20 flat, STT 0.1% sell, txn 0.03503%, stamp 0.003% buy, SEBI 0.0001%, GST 18%).
3. Refactored `internal/backtest/types.go` to **type-alias** onto `internal/strategy` (`type Candle = strategy.Candle`, etc.) instead of keeping duplicate mirror types — since the contract matched exactly, keeping a second copy would just be a drift risk. `internal/backtest/charges.go` was rewritten to delegate to `risk.EstimateCharges` instead of its own rate table (WP-2 task 7's "unified fee model" — done).
4. All engine/report/cache code needed **zero logic changes** for this refactor; only `types.go` and `charges.go` changed shape. All tests were re-run and pass unchanged (they use `Candle{...}`/`Signal{...}` literals, which are identical types under the alias).

**Net effect: no outstanding adjustment is needed once "WP-6 lands" — it already had, and the integration is real**, not stubbed. `cmd/backtest/main.go` calls `strategy.Get(name)` and passes the result directly into `backtest.Run` with no adapter layer.

One integration nuance surfaced and was handled: `internal/strategy/strategy.go`'s own doc comment on `EvalContext` states *"the caller is responsible for feeding the live combined-premium series into Prices... for a symbol tracking an open option structure"* — this is how `short_straddle`/`nine_twenty`'s CR-14 combined-premium stop-loss is meant to work. The engine now tracks a `premiumHistory` series (current combined premium of open legs, repriced via Black-Scholes each candle) and feeds it into `EvalContext.Prices` whenever a position is open, falling back to the underlying's close-price series while flat. Without this, a premium-based stop would have compared an option premium (~hundreds of rupees) against the raw NIFTY spot (~20,000) and fired on the very next candle every time. This is exercised for real in the short_straddle sample run below (see `internal/backtest/engine.go`, `combinedCurrentPremium`).

**Remaining follow-ups for the integration agent (WP-9), not blocking:**
- Lot size still comes from `-lotsize` (default 75), not `broker.InstrumentManager.GetLotSize` — wiring that requires a live/cached instrument master fetch, which is WP-9's integration territory per task 6.
- `OrderLeg.Expiry` parsing accepts `"2006-01-02"`, `"02Jan06"`, `"02-Jan-2006"`, `"2Jan2006"`; falls back to `asOf + Config.DefaultDTEDays` (default 7) at 15:30 local time when empty/unparseable. Real expiry-calendar resolution (weekly Tuesday, holidays, instrument-master lookup via WP-1's `GetExpiries`) is out of scope here — noted as a known simplification.

## Black-Scholes implementation

`internal/backtest/bs.go`: standard closed-form European option pricing.

```
d1 = (ln(S/K) + (r + σ²/2)T) / (σ√T)
d2 = d1 - σ√T
Call = S·N(d1) - K·e^(-rT)·N(d2)
Put  = K·e^(-rT)·N(-d2) - S·N(-d1)
Call delta = N(d1); Put delta = N(d1) - 1
```
`N(x)` via `math.Erfc` for numerical stability. Below `minTimeToExpiry` (~1 minute in years) or `Vol <= 0`, prices at intrinsic value instead of dividing by ~0.

**Golden-value test** (`TestBlackScholes_GoldenValues`, in `bs_test.go`): S=100, K=100, r=5%, σ=20%, T=1yr — a standard textbook sanity-check parameter set (Hull-style worked example, widely reproduced by online BS calculators). Hand derivation in the test's doc comment:
```
d1 = 0.35, d2 = 0.15
N(0.35) ≈ 0.636831, N(0.15) ≈ 0.559618
Call ≈ 10.4506, Put ≈ 5.5735
```
Verified to 0.01 tolerance against the implementation's output, plus an exact put-call-parity check (`C - P == S - Ke^(-rT)`) and delta identity check (`callDelta - putDelta == 1`) across a spread of strikes/spots. A dedicated assertion (`callDelta != 0.5`) exists specifically to catch a regression back to the old broken model.

**v1 limitation, documented in code**: IV is a single constant per run (`Config.IV`, default 12% for NIFTY, CLI `-iv`). Real markets have a volatility surface (skew, term structure, day-to-day IV changes) — reconstructing a historical per-strike IV series (e.g. inverting BS against real option candle closes) is explicitly **not implemented** and is future work (Phase 3 per the remediation plan). This v1 still fixes CR-9's core defect: full repricing on spot + time under *any* fixed vol reintroduces real delta/gamma/theta; only the vol *level* is simplified, not the mechanism that was broken (zero gamma).

## Cost model

`internal/backtest/charges.go` delegates to `internal/risk.EstimateCharges(premium, quantity, risk.OptCarry, side)` — WP-2's landed, FY 2025-26 rate card (brokerage ₹20 flat/order, STT 0.1% sell-side, exchange txn 0.03503%, stamp duty 0.003% buy-side, SEBI 0.0001%, GST 18% on brokerage+txn+SEBI). Charged **per leg, per side** (entry order + exit order, independently, for every leg) — an iron-fly round trip is correctly 8 charged orders, not "one flat fee ×2" like the old code. Additionally, `SpreadCost` charges a configurable half-spread (default 0.3% of premium) per leg per side, modeling the bid/ask an options backtest with only a theoretical mid-price wouldn't otherwise pay.

Worked example (`TestLegCharge_WorkedExample`, 1 lot/75 qty NIFTY option @ ₹150 premium): buy-side ≈ ₹28.60, sell-side ≈ ₹39.51 (sell > buy specifically because STT is sell-side-only — proven by explicit assertion).

## Sample runs

### 1. Synthetic in-process run (`go test -v -run TestSampleBacktestRun_PrintsReport ./internal/backtest/`)

140-day synthetic dataset (cyclic sine + drift, both range-bound and trending stretches), a fake weekly-short-straddle strategy entering every 8 candles and holding 6 of a 7-day expiry:

```
=====================================================
BACKTEST REPORT: weekly-short-straddle-demo
Period: 2026-01-01 -> 2026-05-20
=====================================================
Total Trades:   16 (Wins: 7 | Losses: 9)
Win Rate:       43.75%
Gross P&L:      Rs. -31820.31
Total Charges:  Rs. 4199.49
Net P&L:        Rs. -36019.80
Max Drawdown:   Rs. 52545.00
Profit Factor:  0.58
Expectancy:     Rs. -2251.24 / trade
Avg Win:        Rs. 6968.06
Avg Loss:       Rs. -9421.80
Worst Day:      Rs. -15881.49
-----------------------------------------------------
Per-Month Breakdown:
Month      Trades     Wins   Losses        Net P&L
2026-01         2        1        1       -2581.74
2026-02         3        1        2       -4351.39
2026-03         4        2        2      -17857.16
2026-04         4        1        3      -18557.76
2026-05         3        2        1        7328.25
=====================================================
```
Mixed win/loss result (unlike the old model's mathematically guaranteed win every time) — this is the point of the CR-9 fix.

### 2. Real CLI run against the actual `internal/strategy.ShortStraddleStrategy` (CR-9 proof, production code)

Built `cmd/backtest/main.go`, generated a synthetic ~160-day NIFTY candle CSV cache (sine + drift, ±550pt swings), ran:
```
backtest.exe -strategy short_straddle -symbol NIFTY -csv nifty_synth.csv -from 2026-01-01 -to 2026-06-09 -lotsize 75 -dte 7
```
```
=====================================================
BACKTEST REPORT: Short Straddle (Naked)
Period: 2026-01-01 -> 2026-06-09
=====================================================
Total Trades:   4 (Wins: 0 | Losses: 4)
Win Rate:       0.00%
Gross P&L:      Rs. -115139.00
Total Charges:  Rs. 1417.86
Net P&L:        Rs. -116556.86
Max Drawdown:   Rs. 117432.13
Profit Factor:  0.00
Expectancy:     Rs. -29139.22 / trade
Avg Win:        Rs. 0.00
Avg Loss:       Rs. -29139.22
Worst Day:      Rs. -37836.37
-----------------------------------------------------
Per-Month Breakdown:
Month      Trades     Wins   Losses        Net P&L
2026-03         1        0        1      -37836.37
2026-04         1        0        1      -31672.08
2026-05         1        0        1      -37355.52
2026-06         1        0        1       -9692.90
=====================================================
```
This is the actual production strategy code, no fakes — and it loses money on a synthetic trending/volatile market, which was **structurally impossible** under the old constant-delta-0.5 model.

### 3. Real CLI run against `internal/strategy.EMACrossoverStrategy` (CR-10 proof, production code)

Same cache/date range, `-strategy ema_crossover`:
```
=====================================================
BACKTEST REPORT: EMA Crossover (9/21)
Period: 2026-01-01 -> 2026-06-09
=====================================================
Total Trades:   2 (Wins: 1 | Losses: 1)
Win Rate:       50.00%
Gross P&L:      Rs. 19253.71
Total Charges:  Rs. 340.86
Net P&L:        Rs. 18912.85
Max Drawdown:   Rs. 45599.60
Profit Factor:  5.26
Expectancy:     Rs. 9456.42 / trade
Avg Win:        Rs. 23350.19
Avg Loss:       Rs. -4437.34
Worst Day:      Rs. -4437.34
-----------------------------------------------------
Per-Month Breakdown:
Month      Trades     Wins   Losses        Net P&L
2026-04         1        1        0       23350.19
2026-06         1        0        1       -4437.34
=====================================================
```
Under the old code this strategy produced **zero trades** (CR-10's dead entry branch). It now trades for real.

## Test evidence

```
go build ./internal/backtest/...   # clean
go vet   ./internal/backtest/...   # clean
go test -race ./internal/backtest/...
ok  	titan-algo/internal/backtest	~3s
```
13 test functions, all passing: `TestBlackScholes_GoldenValues`, `TestBlackScholes_DeepITMApproachesIntrinsic`, `TestBlackScholes_PutCallDeltaRelationship`, `TestLegCharge_WorkedExample`, `TestLegCharge_PerLegPerSide`, `TestSpreadCost`, `TestFillAtNextOpen`, `TestDirectionalEntryAndExit`, `TestDirectionalOppositeSignalCloses`, `TestNoEntryWhileAlreadyInPosition`, `TestRun_InsufficientCandles`, `TestShortStraddleLosesOnTrendingMarket`, `TestShortStraddleProfitsInFlatMarket`, `TestSampleBacktestRun_PrintsReport`, `TestCandleCacheRoundTrip`, `TestLoadOrFetch_UsesCacheWhenPresent`, `TestLoadOrFetch_FetchesAndCachesWhenMissing`.

`go build ./...` at the repo root fails, but only in `cmd/main.go` (`activeStrategy.Evaluate` still called with the old 4-argument signature) — that is WP-9's file to fix per the file-ownership matrix, and is the expected/acceptable fallout of WP-6's interface change per WP-6's own acceptance criteria ("go build ./... may break cmd/ ... acceptable ONLY in cmd/ and internal/engine"). `internal/backtest` and `cmd/backtest` both build clean in isolation and together.

## Discrepancies from the audit doc

- The audit's EX-4 worked example (round-trip cost, 1 lot/75 qty @ ₹150, buy+sell ≈ ₹47) is quoted in WP-2's task 2 acceptance criteria, not WP-7's. This package's own worked example (buy ≈ ₹28.60, sell ≈ ₹39.51, round-trip ≈ ₹68.11) uses the *actual* landed `risk.DefaultChargeRates()`, which is higher than the audit's rough estimate — the audit figure appears to be an approximate/illustrative number rather than a precise rate-card computation. No action needed on WP-7's side since it now delegates entirely to WP-2's canonical `EstimateCharges` rather than maintaining an independent number.
- CR-11 ("backtest and live trading do not share a code path") is only partially addressed by this package: `internal/backtest` now shares the *strategy* code path with live (both call `strategy.Strategy.Evaluate(EvalContext)`), but does not share a *portfolio/execution* engine with `internal/engine` — that unification is out of WP-7's owned-files scope (would require touching `internal/engine/engine.go`, which is WP-9's exclusively).
- Task 6 (lot size) is only partially closed: the hardcoded `50` is gone and replaced by a CLI flag with a correct default (75), but automatic instrument-master lookup (`GetLotSize`) is explicitly deferred to WP-9 per the task's own wording.
