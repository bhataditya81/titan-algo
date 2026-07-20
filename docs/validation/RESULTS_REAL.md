# Walk-Forward Validation — REAL Data (NIFTY / BANKNIFTY, 2024-08 → 2026-06)

**Date:** 2026-07-20
**Data:** real 5-minute candles fetched from Angel One this session (`go-engine/data/historical/{NIFTY,BANKNIFTY}.csv`, 2024-07-22 → 2026-07-17), NOT synthetic. Prior validation (`docs/validation/RESULTS.md`) used synthetic data only — this supersedes it as the first real-data evidence this project has ever produced.
**Method:** `docs/validation/run_walkforward_real.py` — 23 rolling calendar-month OOS windows × 7 strategies × 2 symbols = 322 backtest runs, driving the current `cmd/backtest` binary (rebuilt from HEAD, includes the FY26 charge fix, the sniper fix, and all fail-closed changes from this session).
**Limitation carried forward:** constant IV = 12% for every run. No real option chain has been fetched yet this session (only underlying index candles), so the real-IV wiring landed earlier today (`Config.IVAt`) isn't exercised here — every number below has the same IV-assumption caveat the synthetic pass did. Treat especially the option-selling strategies' results as provisional until rerun with real per-bar IV.

## Gates (unchanged from the synthetic pass)
1. Profit factor > 1.3 out-of-sample (pooled across all 23 windows)
2. Max drawdown < 15% of deployed capital/margin
3. Positive expectancy after doubling modeled transaction costs

**Gate 2 could not be rigorously evaluated this run** — each backtest window resets its own equity curve from zero (an artifact of running `Run()` fresh per calendar-month window rather than one continuous multi-year simulation), so "max drawdown" below is per-window peak-to-trough, not drawdown against a real deployed-capital base. Treat gate 2 as **not evaluated** for now; a continuous (non-windowed) run would be needed to assess it properly.

## Pooled Results (all 23 windows summed/reconstructed per strategy per symbol)

| Symbol | Strategy | Trades | Win% | Pooled PF | Net P&L (₹) | Expectancy/trade (₹) | Worst single day (₹) | Losing months |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| BANKNIFTY | **momentum** | 86 | 36.1% | **1.80** | +230,484.70 | +2,680.05 | -12,485.86 | 9/23 |
| BANKNIFTY | **ema_crossover** | 419 | 44.4% | **1.33** | +268,154.81 | +639.99 | -15,213.77 | 10/23 |
| BANKNIFTY | sniper | 584 | 26.2% | 1.20 | +212,166.12 | +363.30 | -19,630.32 | 12/23 |
| NIFTY | momentum | 91 | 29.7% | 1.26 | +89,397.16 | +982.39 | -15,353.63 | 15/23 |
| NIFTY | sniper | 568 | 26.9% | 1.20 | +222,370.23 | +391.50 | -17,893.70 | 13/23 |
| NIFTY | ema_crossover | 407 | 44.0% | 1.11 | +107,138.34 | +263.24 | -14,699.83 | 12/23 |
| BANKNIFTY | rsi_reversal | 3,465 | 43.5% | 0.90 | -203,300.25 | -58.67 | -17,381.54 | 19/23 |
| NIFTY | rsi_reversal | 3,526 | 40.8% | 0.77 | -468,346.42 | -132.83 | -13,723.77 | 18/23 |
| NIFTY | short_straddle | 1,328 | 17.0% | 0.41 | -486,978.34 | -366.70 | -46,506.57 | 19/23 |
| BANKNIFTY | short_straddle | 1,357 | 16.4% | 0.28 | -731,175.39 | -538.82 | -63,090.23 | 20/23 |
| NIFTY | nine_twenty | 448 | 29.9% | 0.03 | -611,847.95 | -1,365.73 | -12,277.84 | **23/23** |
| BANKNIFTY | nine_twenty | 448 | 28.4% | 0.02 | -700,013.31 | -1,562.53 | -10,888.58 | **23/23** |
| NIFTY | iron_fly | 1,329 | 2.5% | 0.01 | -699,323.84 | -526.20 | -4,544.48 | **23/23** |
| BANKNIFTY | iron_fly | 1,357 | 0.0% | 0.00 | -727,899.09 | -536.40 | -2,811.13 | **23/23** |

Pooled PF reconstructed from `wins × avg_win` / `|losses × avg_loss|` summed across all 23 windows (a straight average of per-window PF values would be mathematically wrong — PF is a ratio of sums, not additive).

## Gate 1 + Gate 3 check for the two candidates that cleared PF > 1.3

| Strategy (symbol) | PF | Gross P&L | Charges | Net @ 1x cost | Net @ 2x cost (gross − 2×charges) | Survives gate 3? |
|---|---:|---:|---:|---:|---:|:--:|
| BANKNIFTY momentum | 1.80 | +242,899.20 | 12,414.50 | +230,484.70 | +218,070.20 | **Yes** |
| BANKNIFTY ema_crossover | 1.33 | +326,327.73 | 58,172.95 | +268,154.81 | +209,981.83 | **Yes** |

Both survive doubled transaction costs with wide margin — this is a materially different outcome than the synthetic pass, where nothing survived all three gates.

## Verdict per strategy

- **BANKNIFTY momentum — PASSES gates 1 and 3.** Best pooled PF (1.80) and highest per-trade expectancy (₹2,680) of anything tested. **Caveat: only 86 trades across 23 months (~3.7/month) — a small sample for a real go/no-go decision.** A handful of large winning months could be doing most of the work; needs the parameter-neighborhood/regime-breakdown robustness checks R2-7 originally called for before trusting this number.
- **BANKNIFTY ema_crossover — PASSES gates 1 and 3, more data behind it.** 419 trades, PF 1.33 (right at the gate line), positive in 13/23 months. More trades than momentum so somewhat more statistically credible, but PF this close to the 1.3 cutoff means it could fall either side on a slightly different window split or real-IV rerun.
- **NIFTY momentum (PF 1.26) and NIFTY sniper / BANKNIFTY sniper (PF 1.20 both)** — close but do not clear gate 1. Sniper is worth noting separately: this strategy produced **zero trades in every prior synthetic run** due to a real bug fixed earlier this session (R2-2, the `Prices`-always-populated mode-selection bug). This is the first evidence that the fix produced a strategy that actually trades and is close to profitable — worth a second look after more data/a parameter sweep, not worth discarding.
- **rsi_reversal — fails clearly, and the failure mode is informative, not just "no edge".** 3,465-3,526 trades over 23 months (~150/month, multiple per day) with a *positive* gross P&L on NIFTY (+₹33,869 pooled, not shown in this table — see raw CSV) that gets completely erased by ₹502,215 in charges. This is the overtrading/churn problem the original audit (ST-5) warned about: whatever small directional edge this strategy has is being traded away by transaction costs. A lower-frequency variant (larger period params, fewer signals) might fare very differently — this is a parameter problem, not necessarily a dead strategy.
- **short_straddle, nine_twenty, iron_fly — fail decisively and consistently.** All three lose in the overwhelming majority of months (short_straddle 19-20/23, nine_twenty and iron_fly a perfect **23/23** on both symbols). The 100%-losing-months pattern for nine_twenty and iron_fly, with almost no variance in the loss magnitude month to month, is a stronger signal than "no edge in this regime" — it looks like these strategies may be structurally unable to profit under the current cost/wing-width/premium-stop assumptions regardless of market conditions. **Recommend a targeted code-level review of iron_fly's wing economics and nine_twenty's premium-stop logic before spending further backtest cycles on parameter tuning** — if the charge model or wing width makes the strategy lose by construction, no amount of walk-forward validation will find a working parameter set.

## What changed vs. the synthetic-data pass

The synthetic pass (`docs/validation/RESULTS.md`) found **0 of 7 strategies** clearing all three gates, and its own top caveat was that short-vol strategies' results were artifacts of the assumed constant IV rather than a real edge (short_straddle's PF flipped 0.29→1.66 purely from changing assumed vol). This real-data pass:
- Confirms short_straddle/iron_fly/nine_twenty are **not** viable as currently built, on real price data, independent of the synthetic-data IV-sensitivity artifact — a second, independent line of evidence against the naked/hedged short-vol strategies as they stand.
- Surfaces two directional strategies (momentum, ema_crossover) with real, if preliminary, positive evidence — something the synthetic pass never surfaced clearly enough to highlight, because the earlier report's synthetic dataset and gate framing centered on the option-selling strategies' IV sensitivity.

## Honest gaps in this pass

1. **Constant IV throughout** — no real option-chain data was fetched this session. The next real step for validation quality is fetching a real option chain (`cmd/fetchdata -option-underlying ...`) and rerunning at least the two passing candidates and the option-selling strategies with real per-bar IV via the now-wired `Config.IVAt`.
2. **Per-window drawdown, not continuous equity** — gate 2 (max DD vs. deployed capital) genuinely could not be evaluated correctly this run; needs a continuous multi-year simulation, not 23 independent resets.
3. **No parameter sweep performed for this pass** — every strategy ran at its compiled-in default parameters (`registry.GetWithParams` exists and is wired, per R2-2, but this run didn't use it). The rsi_reversal churn problem and the borderline PF=1.2-1.3 strategies are exactly the kind of result a parameter sweep (lower trade frequency, wider stops) could meaningfully change.
4. **No walk-forward train/test split in the statistical sense** — these are OOS *windows* rolled forward, but no strategy has any fitted/trained parameters to begin with (all compiled-in constants), so "OOS" here means "out of the window used to compute the pooled statistic," not "out of a fitting sample." This matches the original WP-10 methodology note.
5. **Only 2 symbols, ~23 months** — real regime coverage (crash, extreme-vol events) beyond what happened to occur in this particular 2024-07→2026-07 window is unknown.

## Recommendation

Do not go live with iron_fly, nine_twenty, or short_straddle as currently built — two independent lines of evidence (synthetic IV-sensitivity + real-data consistent losses) now argue against them. BANKNIFTY momentum and BANKNIFTY ema_crossover are the first strategies in this project's history to show real, gate-clearing evidence — investigate further (larger sample via parameter/regime robustness checks, a real-IV rerun) before treating either as validated enough for even a 1-lot live pilot.
