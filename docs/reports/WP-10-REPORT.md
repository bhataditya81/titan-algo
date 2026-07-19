# WP-10 — Validation Harness — Report

## Scope discipline

Per the task's hard constraints, **no existing source file was edited**. Every file
created lives under `docs/validation/`. The one build step performed
(`go build -o docs/validation/bin/backtest.exe ./cmd/backtest`) does not modify any
tracked source; it produces a binary artifact, written directly into
`docs/validation/bin/`, from the already-landed, unmodified WP-7 CLI. Re-ran
`go build ./... && go vet ./...` from `go-engine/` at the end of this WP: still clean.

## What was built

| File | Purpose |
|---|---|
| `docs/validation/gen_synthetic_data.py` | Generates the synthetic candle dataset (see Data below). Deterministic (seeded), reproducible. |
| `docs/validation/data/nifty_synthetic_5min.csv` | Its output: 48,900 five-minute bars, 2024-01-01 → 2026-06-30, in the exact `internal/backtest/cache.go` CSV cache format (`time,open,high,low,close,volume`, RFC3339, header row). |
| `docs/validation/run_walkforward.py` | Drives the walk-forward protocol: shells out to `docs/validation/bin/backtest.exe` once per (strategy, window) and per (strategy, IV, DTE) sensitivity combo, parses the printed report text, writes `docs/validation/out/walkforward_windows.csv` and `docs/validation/out/sensitivity_grid.csv`. |
| `docs/validation/summarize.py` | Pools the 24 per-strategy windows into `docs/validation/out/strategy_summary.csv` (aggregate trades/PF/expectancy/worst-window/worst-day, plus the 2x-cost expectancy stress test computed arithmetically — see RESULTS.md's method note). |
| `docs/validation/out/*.csv` | Raw and aggregated results; every number in RESULTS.md traces back to a row in one of these. |
| `docs/validation/bin/backtest.exe` | Built, unmodified WP-7 CLI binary, used as the actual engine for every run. |
| `docs/validation/RESULTS.md` | The deliverable: per-strategy walk-forward table, gate verdicts, sensitivity findings, and honest discussion of what's wrong/inconclusive. |
| `docs/validation/LIVE_GATE_CHECKLIST.md` | The deliverable: operator checklist before any `-live` run. |

## Exact commands run

Build:
```
cd go-engine
go build -o ../docs/validation/bin/backtest.exe ./cmd/backtest
```

Data generation:
```
python docs/validation/gen_synthetic_data.py
```

Walk-forward + sensitivity sweep (repo root):
```
python docs/validation/run_walkforward.py
python docs/validation/summarize.py
```

`run_walkforward.py` itself issues 168 + 48 = 216 subprocess calls to `backtest.exe`, one
representative example (every strategy/window pair used the same flag shape):
```
docs/validation/bin/backtest.exe -strategy short_straddle -symbol NIFTY \
  -csv docs/validation/data/nifty_synthetic_5min.csv \
  -from 2024-07-01 -to 2024-07-31 -lotsize 75 -iv 0.12 -dte 7 -strikestep 50
```
Verified manually (`-list-strategies`) that all 7 registered strategies
(`ema_crossover, iron_fly, momentum, nine_twenty, rsi_reversal, short_straddle, sniper`)
were covered.

## Data: real vs. synthetic (read this first)

**No real cached NIFTY candle data exists anywhere in this repo.** Checked: no `*.csv`
candle files under `go-engine/` outside test fixtures, `internal/backtest/cache.go`'s
`LoadOrFetch` only knows how to read its own cache format or fall through to a live broker
fetch (`cmd/backtest/main.go`'s `fetch()` closure calls `broker.NewAngelBroker(...).Connect()`
using `ANGEL_*` env vars or `config.yaml`). Per the task's constraints, no credentials were
used and nothing was run with `-live` or any live broker connection. This is expected and
matches the task's own stated expectation.

**So every number in `docs/validation/RESULTS.md` comes from fabricated data**
(`docs/validation/gen_synthetic_data.py`): a geometric-random-walk NIFTY-like index path,
5-minute bars, IST session hours, weekdays only (holidays not modeled), with a
deliberately varied regime schedule — range-bound (2024 H1), uptrend (mid-2024),
a sharp ~6% overnight gap-down shock (Oct 2024) followed by elevated-vol choppy recovery,
range-bound again (2025 H1), another uptrend (late 2025/early 2026), and a mixed/volatile
tail (2026 H1). This satisfies the brief's requirement to cover trending, ranging, and a
sharp shock event, but **it is not real market data and does not demonstrate anything
about real NIFTY behavior.** RESULTS.md restates this caveat at the top and again wherever
a specific number could be misread as a real finding.

## Walk-forward protocol actually run

24 rolling one-calendar-month out-of-sample windows (2024-07 through 2026-06), each
strategy run independently per window at its compiled-in default parameters. The first six
months (2024-01..06) were generated but deliberately **not scored** as a "train" window —
see RESULTS.md's "Why 'train' does nothing here" section: these strategies have no fitting
step (all parameters are hardcoded Go struct literals in each `New*Strategy()`
constructor), so there is nothing to train; the walk-forward here validates regime
robustness, not parameter generalization. This discrepancy from the plan's literal
"6-month train / 1-month test" wording is called out rather than papered over.

## Parameter sensitivity: partial, with a stated blocker

The brief asked for a sweep across strategy-internal parameters (RSI thresholds, EMA
periods, straddle wing width). **`cmd/backtest/main.go` exposes no flags for these** — they
are hardcoded per strategy (e.g. `iron_fly.go`'s `WingWidth: 200`, `short_straddle.go`'s
`StopMultiplier: 1.4`). Adding such flags requires editing `cmd/backtest/main.go` and/or
strategy files, both forbidden by this WP's file-ownership constraint. This is reported as
a blocker, not silently worked around.

What *was* swept, since the CLI does expose it: **IV (4 levels: 8/12/18/25%) × DTE
(3/7/14 days)**, on the four strategies whose backtest economics run through the
Black-Scholes leg pricing (short_straddle, iron_fly, nine_twenty, ema_crossover), on a
fixed representative window. This produced the single most load-bearing finding in
RESULTS.md: **short_straddle's aggregate result flips from a clear loser (PF 0.29 at
IV=8%) to an apparent winner (PF 1.66 at IV=25%) purely by changing the assumed constant
IV, with no change to market data or strategy logic** — a direct, reproduced consequence of
WP-7's documented v1 limitation (constant IV, no vega/IV-crush risk modeled). This is
flagged explicitly in RESULTS.md as a warning against taking any option-seller's
"apparently positive" number at face value.

## Headline result

**Zero of the seven registered strategies clear all three WP-10 gates (PF > 1.3 OOS, max
DD < 15% of deployed margin, positive expectancy at 2x modeled costs) on this synthetic
dataset.** `ema_crossover` is the only one clearing PF and 2x-cost-expectancy, and even
that is fragile under the IV/DTE sensitivity grid. `rsi_reversal`, `short_straddle`,
`nine_twenty`, and `iron_fly` all fail decisively. `sniper` could not be evaluated at all
(see below). Full numbers, per-strategy tables, and the reasoning behind each verdict are
in `docs/validation/RESULTS.md`. This is consistent with, and does not overturn,
`docs/PRODUCTION_READINESS_AUDIT.md` §8's finding that the system has no demonstrated edge.

## Findings beyond the raw numbers (full detail in RESULTS.md)

1. **`sniper` produced zero trades across all 24 windows/every regime.** Traced this to a
   real integration gap between `internal/strategy/sniper.go`'s documented "candle-mode"
   fast path (gated on `len(ctx.Prices) == 0`) and `internal/backtest/engine.go`, which
   always populates `ctx.Prices` when flat — so sniper always runs its live
   tick-aggregation path instead, which (fed one price per call in this driving pattern)
   builds zero-range doji candles that its pattern detectors (Hammer/Engulfing/Shooting
   Star) can structurally never trigger on. This is a defect for whoever owns
   `internal/backtest/engine.go` + `internal/strategy/sniper.go` to reconcile — not
   something this WP's constraints allow fixing, and not evidence about sniper's real edge
   either way.
2. **`iron_fly` and `short_straddle` lose in the vast majority of windows even in calm,
   range-bound months** — traced to their RSI(14)-on-5-min-bars entry/exit gating
   producing very short holding periods (several round trips a day) relative to the 7-day
   DTE the options are priced against, so per-round-trip transaction costs (8 charged
   orders for a 4-leg fly) dominate whatever theta was captured almost every time. This
   reproduces the audit's §8.2 warning ("any strategy trading every 5 minutes is
   structurally handicapped") on synthetic data rather than surfacing a new bug.
3. **`nine_twenty` loses in 24/24 windows**, not just the modeled shock month — its WP-6
   CR-14 premium stop (`StopMultiplier: 1.4`, i.e. 40% adverse move) is evidently too loose
   to prevent steady bleed even without a trend day.

## Caveats to carry forward (do not lose these)

- **Synthetic data only.** Restated in RESULTS.md's first paragraph and its own header
  comment in `gen_synthetic_data.py`. Nothing here is a real edge-discovery result.
- **Constant-IV Black-Scholes**, inherited unmodified from WP-7 — no smile, skew,
  term-structure, or day-to-day IV changes; zero vega/IV-crush risk modeled. Directly
  responsible for the sign-flipping IV sensitivity finding above.
- **No margin model** anywhere in `internal/backtest` — the max-drawdown gate could only
  be graded against a rough outside estimate of typical NIFTY option margin, not real
  broker margin data (endpoint A-6 is unused by the backtest).
- **No CLI/config support for strategy-internal parameter sweeps** — a real gap for
  whoever tunes these strategies next; this WP could not add it without violating its own
  file-ownership constraint.
- **Indian market holidays not modeled** in the synthetic data generator (weekdays-only
  calendar) — a known simplification, immaterial to the findings above but worth fixing if
  this generator is reused.
- **`sniper` is untested**, not "no edge" — see Finding 1.

## Acceptance criteria checklist

- [x] Every number in RESULTS.md traces to an actual backtest run (`docs/validation/out/*.csv`), with representative commands given per strategy.
- [x] Synthetic-vs-real-data status stated explicitly, repeatedly, not just once and forgotten.
- [x] Honest/skeptical framing — the IV-sensitivity finding and the "zero strategies clear all three gates" headline are stated plainly, not softened.
- [x] Blockers reported rather than worked around by editing forbidden files (strategy-parameter sweep flags, margin model, 2x-cost CLI flag — the last one solved arithmetically instead, method documented).
- [x] `go build ./... && go vet ./...` clean at the end, no source file touched.
