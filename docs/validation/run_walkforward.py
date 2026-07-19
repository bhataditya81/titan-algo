#!/usr/bin/env python3
"""WP-10 walk-forward driver.

Repeatedly invokes the built backtest CLI (docs/validation/bin/backtest.exe)
over rolling monthly OOS windows across the synthetic 2.5-year dataset
(docs/validation/data/nifty_synthetic_5min.csv), for every registered
strategy, and a small grid of the backtest-level knobs the CLI actually
exposes (-iv, -dte). Parses each printed report into a row and writes:

  docs/validation/out/walkforward_windows.csv   -- one row per (strategy, window)
  docs/validation/out/sensitivity_grid.csv      -- one row per (strategy, iv, dte) on a fixed window

IMPORTANT LIMITATION (see WP-10-REPORT.md): the CLI has no flags for
strategy-INTERNAL parameters (RSI thresholds, EMA periods, straddle wing
width, stop multipliers -- see internal/strategy/*.go constructors). Per the
task's hard constraint, this harness may only add files under
docs/validation/ and must not edit cmd/backtest/main.go or any strategy file
to add such flags. So the "parameter sensitivity" sweep here varies the
backtest-level knobs that ARE exposed (IV, days-to-expiry) instead, which is
a real and meaningful sensitivity check for the option-selling strategies
(nine_twenty, short_straddle, iron_fly) since it directly perturbs the
Black-Scholes repricing, but it does NOT substitute for sweeping e.g.
RSI-2's oversold/overbought thresholds. Flagged plainly as a blocker in the
report, not hidden here.

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

def run_backtest(strategy, frm, to, iv=0.12, dte=7, lotsize=75, strikestep=50):
    cmd = [
        EXE, "-strategy", strategy, "-symbol", "NIFTY",
        "-csv", DATA, "-from", frm, "-to", to,
        "-lotsize", str(lotsize), "-iv", str(iv), "-dte", str(dte),
        "-strikestep", str(strikestep),
    ]
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

if __name__ == "__main__":
    main()
