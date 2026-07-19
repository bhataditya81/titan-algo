#!/usr/bin/env python3
"""WP-10 walk-forward driver.

Repeatedly invokes the built backtest CLI (docs/validation/bin/backtest.exe)
over rolling monthly OOS windows across the synthetic 2.5-year dataset
(docs/validation/data/nifty_synthetic_5min.csv), for every registered
strategy, and a small grid of the backtest-level knobs the CLI actually
exposes (-iv, -dte). Parses each printed report into a row and writes:

  docs/validation/out/walkforward_windows.csv   -- one row per (strategy, window)
  docs/validation/out/sensitivity_grid.csv      -- one row per (strategy, iv, dte) on a fixed window

R2-3 UPDATE: cmd/backtest now has real -params and -cost-multiplier flags
(G-5), replacing the two workarounds below:

  - -params key=val,... is passed straight to strategy.GetWithParams once
    R2-2's parameterized constructors land (internal/backtest.ResolveStrategy
    falls back to the strategy's compiled-in defaults with a loud WARNING if
    that hook isn't wired yet -- see docs/reports/R2-3-REPORT.md). The old
    IV/DTE-only sensitivity grid below is UNCHANGED (it's still a real,
    separate sensitivity axis for the option-selling strategies), but a new
    real_params_sweep() demonstrates the flag working end-to-end against
    THIS SAME synthetic dataset: it runs a few strategies with -params set
    and confirms the CLI degrades gracefully (still produces a valid report)
    rather than crashing, so it's ready to sweep real parameter grids the
    moment R2-2's registry.GetWithParams lands -- no harness changes needed
    then, just real values in PARAM_GRID below.
  - -cost-multiplier replaces summarize.py's "recompute 2x-cost expectancy
    by hand outside the tool" workaround. real_cost_multiplier_check() below
    reruns a few (strategy, window) pairs at -cost-multiplier=2.0 and checks
    the result exactly matches the old manual-arithmetic prediction
    (NetPnL' = NetPnL - Charges), proving ScaleCosts is correct.

No code outside docs/validation/ is modified or created by this script --
it only shells out to the pre-built exe and writes into docs/validation/out.
"""
import csv
import os
import re
import subprocess
import sys
from datetime import date

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))  # repo root (parent of docs/)
ROOT = os.path.dirname(ROOT)
EXE = os.path.join(ROOT, "docs", "validation", "bin", "backtest.exe")
DATA = os.path.join(ROOT, "docs", "validation", "data", "nifty_synthetic_5min.csv")
OUTDIR = os.path.join(ROOT, "docs", "validation", "out")

STRATEGIES = [
    "ema_crossover", "rsi_reversal", "momentum", "sniper",
    "nine_twenty", "short_straddle", "iron_fly",
]

FIELD_PATTERNS = {
    "trades": r"Total Trades:\s+(\d+) \(Wins: (\d+) \| Losses: (\d+)\)",
    "win_rate": r"Win Rate:\s+([\d.]+)%",
    "gross_pnl": r"Gross P&L:\s+Rs\. (-?[\d.]+)",
    "total_charges": r"Total Charges:\s+Rs\. (-?[\d.]+)",
    "net_pnl": r"Net P&L:\s+Rs\. (-?[\d.]+)",
    "max_dd": r"Max Drawdown:\s+Rs\. (-?[\d.]+)",
    "profit_factor": r"Profit Factor:\s+([\d.]+|\+?Inf)",
    "expectancy": r"Expectancy:\s+Rs\. (-?[\d.]+) / trade",
    "avg_win": r"Avg Win:\s+Rs\. (-?[\d.]+)",
    "avg_loss": r"Avg Loss:\s+Rs\. (-?[\d.]+)",
    "worst_day": r"Worst Day:\s+Rs\. (-?[\d.]+)",
}

def run_backtest(strategy, frm, to, iv=0.12, dte=7, lotsize=75, strikestep=50,
                  cost_multiplier=1.0, params=None):
    cmd = [
        EXE, "-strategy", strategy, "-symbol", "NIFTY",
        "-csv", DATA, "-from", frm, "-to", to,
        "-lotsize", str(lotsize), "-iv", str(iv), "-dte", str(dte),
        "-strikestep", str(strikestep),
    ]
    if cost_multiplier != 1.0:
        cmd += ["-cost-multiplier", str(cost_multiplier)]
    if params:
        cmd += ["-params", ",".join(f"{k}={v}" for k, v in params.items())]
    p = subprocess.run(cmd, capture_output=True, text=True, cwd=ROOT)
    return p.stdout + p.stderr

def parse_report(text):
    out = {}
    m = re.search(FIELD_PATTERNS["trades"], text)
    if not m:
        return None  # e.g. "no candles in range" fatal log -- treat as no-data window
    out["trades"] = int(m.group(1))
    out["wins"] = int(m.group(2))
    out["losses"] = int(m.group(3))
    for key in ["win_rate", "gross_pnl", "total_charges", "net_pnl", "max_dd",
                "expectancy", "avg_win", "avg_loss", "worst_day"]:
        mm = re.search(FIELD_PATTERNS[key], text)
        out[key] = float(mm.group(1)) if mm else 0.0
    mm = re.search(FIELD_PATTERNS["profit_factor"], text)
    if mm:
        v = mm.group(1)
        out["profit_factor"] = float("inf") if "Inf" in v else float(v)
    else:
        out["profit_factor"] = 0.0
    return out

def month_windows(start_y, start_m, end_y, end_m):
    y, m = start_y, start_m
    while (y, m) <= (end_y, end_m):
        frm = date(y, m, 1)
        nm, ny = (m % 12) + 1, y + (1 if m == 12 else 0)
        to = date(ny, nm, 1)
        to = date.fromordinal(to.toordinal() - 1)  # last day of month m
        yield frm.isoformat(), to.isoformat()
        m, y = nm, ny

def main():
    # Walk-forward: first 6 months (2024-01..2024-06) are the "train" burn-in
    # (unused -- these strategies have no fitting step; fixed compiled-in
    # params only, see WP-10-REPORT.md). OOS test windows: one calendar month
    # each, rolling forward across the remaining ~24 months, covering every
    # regime in gen_synthetic_data.py (range-bound, uptrend, shock, volatile
    # recovery, range-bound, uptrend, mixed).
    windows = list(month_windows(2024, 7, 2026, 6))

    rows = []
    for strat in STRATEGIES:
        for frm, to in windows:
            text = run_backtest(strat, frm, to)
            parsed = parse_report(text)
            if parsed is None:
                continue
            parsed["strategy"] = strat
            parsed["from"] = frm
            parsed["to"] = to
            rows.append(parsed)
            print(f"{strat:15s} {frm}..{to}  trades={parsed['trades']:3d} "
                  f"net={parsed['net_pnl']:>12.2f} pf={parsed['profit_factor']:.2f}")

    fields = ["strategy", "from", "to", "trades", "wins", "losses", "win_rate",
              "gross_pnl", "total_charges", "net_pnl", "max_dd",
              "profit_factor", "expectancy", "avg_win", "avg_loss", "worst_day"]
    with open(os.path.join(OUTDIR, "walkforward_windows.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=fields)
        w.writeheader()
        for r in rows:
            w.writerow(r)
    print(f"\nwrote {len(rows)} rows to docs/validation/out/walkforward_windows.csv")

    # --- sensitivity grid: IV x DTE, on a single representative window that
    # spans a full regime (2025-09..2025-11, uptrend) for the two strategies
    # whose economics are most IV/DTE-sensitive (naked/hedged option sellers)
    # plus one directional strategy as a control (IV/DTE affect its synthetic
    # option P&L too, since backtest prices its directional bet as an ATM
    # option, not the raw index).
    sens_rows = []
    ivs = [0.08, 0.12, 0.18, 0.25]
    dtes = [3, 7, 14]
    sens_window = ("2025-09-01", "2025-11-30")
    for strat in ["short_straddle", "iron_fly", "nine_twenty", "ema_crossover"]:
        for iv in ivs:
            for dte in dtes:
                text = run_backtest(strat, *sens_window, iv=iv, dte=dte)
                parsed = parse_report(text)
                if parsed is None:
                    continue
                parsed.update(strategy=strat, iv=iv, dte=dte)
                sens_rows.append(parsed)
                print(f"[sens] {strat:15s} iv={iv:.2f} dte={dte:2d}  "
                      f"trades={parsed['trades']:3d} net={parsed['net_pnl']:>12.2f} pf={parsed['profit_factor']:.2f}")

    sfields = ["strategy", "iv", "dte", "trades", "wins", "losses", "win_rate",
               "gross_pnl", "total_charges", "net_pnl", "max_dd",
               "profit_factor", "expectancy", "avg_win", "avg_loss", "worst_day"]
    with open(os.path.join(OUTDIR, "sensitivity_grid.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=sfields)
        w.writeheader()
        for r in sens_rows:
            w.writerow(r)
    print(f"wrote {len(sens_rows)} rows to docs/validation/out/sensitivity_grid.csv")

    real_cost_multiplier_check(rows)
    real_params_sweep()


# PARAM_GRID: real strategy-internal parameter grids, using R2-2's actual
# <Strategy>Params field names (registry.GetWithParams landed during this
# WP -- see docs/reports/R2-2-REPORT.md section 5/6). Field names are
# exact Go struct field names (case-sensitive, reflect-based field mapping
# in internal/strategy/registry.go's applyParams).
PARAM_GRID = {
    "rsi_reversal": [{"Period": 2}, {"Period": 5}, {"Period": 14}],
    # A near-default multiplier (1.4) barely differs from 1.2/1.8 on a calm
    # window -- the stop just doesn't bind either way. Use a genuinely tight
    # stop (0.1) as one grid point so the sweep has real signal to detect.
    "short_straddle": [{"StopMultiplier": 0.1}, {"StopMultiplier": 1.4}, {"StopMultiplier": 5.0}],
    "ema_crossover": [{"FastPeriod": 9, "SlowPeriod": 21}, {"FastPeriod": 5, "SlowPeriod": 40}],
}


def real_params_sweep():
    """Demonstrates -params end-to-end against the existing synthetic
    dataset (G-5 acceptance: "showing it works"). R2-2's
    registry.GetWithParams landed during this WP and is wired in via
    cmd/backtest/main.go's init() (backtest.StrategyWithParams =
    strategy.GetWithParams), so this is REAL parameterization, not just
    flag plumbing: it asserts that varying Period (2 vs 14) for
    rsi_reversal changes the trade count -- if -params were being silently
    ignored, every row in a strategy's grid would produce an identical
    report.
    """
    # The Oct-2024 shock month (see gen_synthetic_data.py) has real
    # intraday swings, so a stop-loss multiplier actually gets tested
    # against something -- a dead-calm window can make legitimately
    # different params produce identical output (the stop never binds
    # either way), which looks like a wiring bug but isn't.
    window = ("2024-10-01", "2024-10-31")
    rows = []
    for strat, grid in PARAM_GRID.items():
        trade_counts = set()
        for params in grid:
            text = run_backtest(strat, *window, params=params)
            parsed = parse_report(text)
            ok = parsed is not None
            if not ok:
                raise SystemExit(f"-params run FAILED for {strat} {params}:\n{text}")
            trade_counts.add(parsed["trades"])
            rows.append({"strategy": strat, "params": params, "trades": parsed["trades"], "net_pnl": parsed["net_pnl"]})
            print(f"[params-demo] {strat:15s} {params}  trades={parsed['trades']:4d} net={parsed['net_pnl']:>12.2f}")
        if len(trade_counts) < 2:
            raise SystemExit(f"-params sweep for {strat} produced IDENTICAL trade counts across all params "
                              f"{grid} -- looks like parameterization isn't actually taking effect")

    with open(os.path.join(OUTDIR, "params_flag_demo.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=["strategy", "params", "trades", "net_pnl"])
        w.writeheader()
        for r in rows:
            w.writerow(r)
    print(f"wrote {len(rows)} rows to docs/validation/out/params_flag_demo.csv -- "
          f"-params flag confirmed to REALLY change strategy behavior (trade counts differ across the grid)")


def real_cost_multiplier_check(base_rows):
    """Demonstrates -cost-multiplier end-to-end (G-5 acceptance: "showing it
    works") by rerunning a handful of (strategy, window) pairs already in
    base_rows at -cost-multiplier=2.0 and checking the result matches the
    old WP-10 manual-arithmetic prediction (NetPnL' = NetPnL - Charges)
    EXACTLY -- this is a real assertion, not just "it didn't crash": if
    internal/backtest.ScaleCosts had a bug, the numbers would disagree.
    """
    sample = [r for r in base_rows if r["strategy"] in ("short_straddle", "ema_crossover")][:6]
    rows = []
    mismatches = 0
    for base in sample:
        text = run_backtest(base["strategy"], base["from"], base["to"], cost_multiplier=2.0)
        parsed = parse_report(text)
        if parsed is None:
            continue
        predicted = base["net_pnl"] - base["total_charges"]
        actual = parsed["net_pnl"]
        match = abs(predicted - actual) < 0.01
        if not match:
            mismatches += 1
        rows.append({
            "strategy": base["strategy"], "from": base["from"], "to": base["to"],
            "net_pnl_1x": base["net_pnl"], "charges_1x": base["total_charges"],
            "predicted_net_pnl_2x": round(predicted, 2), "actual_net_pnl_2x": round(actual, 2),
            "match": match,
        })
        print(f"[cost-mult-demo] {base['strategy']:15s} {base['from']}..{base['to']}  "
              f"predicted_2x={predicted:>12.2f} actual_2x={actual:>12.2f} match={match}")

    with open(os.path.join(OUTDIR, "cost_multiplier_check.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=["strategy", "from", "to", "net_pnl_1x", "charges_1x",
                                          "predicted_net_pnl_2x", "actual_net_pnl_2x", "match"])
        w.writeheader()
        for r in rows:
            w.writerow(r)
    if mismatches:
        raise SystemExit(f"-cost-multiplier check FAILED: {mismatches} window(s) didn't match the predicted 2x-cost P&L")
    print(f"wrote {len(rows)} rows to docs/validation/out/cost_multiplier_check.csv -- "
          f"-cost-multiplier=2.0 matches the WP-10 manual-arithmetic prediction exactly on every sampled window")


if __name__ == "__main__":
    main()
