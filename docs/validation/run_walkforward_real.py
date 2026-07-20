#!/usr/bin/env python3
"""Real-data walk-forward driver.

Reuses run_walkforward.py's run_backtest/parse_report/month_windows machinery
against the REAL NIFTY/BANKNIFTY history fetched from Angel One this session
(go-engine/data/historical/{NIFTY,BANKNIFTY}.csv) instead of the synthetic
dataset used by WP-10/R2-3. Writes to a separate output file so the synthetic
baseline in docs/validation/out/ is preserved for comparison.

This still uses -iv 0.12 (constant IV) for both symbols -- no real option
chain has been fetched yet this session, so real per-bar IV (wired into
internal/backtest/config.go's Config.IVAt) isn't available for this run.
Every number below carries the same constant-IV caveat as the synthetic
pass; the only thing "real" about this run is the underlying price data.
"""
import csv
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import run_walkforward as wf

ROOT = wf.ROOT
OUTDIR = wf.OUTDIR

REAL_DATA = {
    "NIFTY": {
        "csv": os.path.join(ROOT, "go-engine", "data", "historical", "NIFTY.csv"),
        "lotsize": 75,
        "strikestep": 50,
    },
    "BANKNIFTY": {
        "csv": os.path.join(ROOT, "go-engine", "data", "historical", "BANKNIFTY.csv"),
        "lotsize": 30,
        "strikestep": 100,
    },
}

# Real data spans 2024-07-22 .. 2026-07-17. One month buffer at each end so
# every window is a full calendar month with real data on both sides.
WINDOWS = list(wf.month_windows(2024, 8, 2026, 6))


def run_backtest_real(strategy, frm, to, symbol):
    meta = REAL_DATA[symbol]
    cmd = [
        wf.EXE, "-strategy", strategy, "-symbol", symbol,
        "-csv", meta["csv"], "-from", frm, "-to", to,
        "-lotsize", str(meta["lotsize"]), "-iv", "0.12", "-dte", "7",
        "-strikestep", str(meta["strikestep"]),
    ]
    p = wf.subprocess.run(cmd, capture_output=True, text=True, cwd=ROOT)
    return p.stdout + p.stderr


def main():
    if not os.path.exists(wf.EXE):
        raise SystemExit(f"backtest binary not found at {wf.EXE} -- build it first")
    for symbol, meta in REAL_DATA.items():
        if not os.path.exists(meta["csv"]):
            raise SystemExit(f"real data not found at {meta['csv']} -- run cmd/fetchdata first")

    all_rows = []
    for symbol in REAL_DATA:
        for strat in wf.STRATEGIES:
            for frm, to in WINDOWS:
                text = run_backtest_real(strat, frm, to, symbol)
                parsed = wf.parse_report(text)
                if parsed is None:
                    continue
                parsed["symbol"] = symbol
                parsed["strategy"] = strat
                parsed["from"] = frm
                parsed["to"] = to
                all_rows.append(parsed)
                print(f"{symbol:9s} {strat:15s} {frm}..{to}  trades={parsed['trades']:3d} "
                      f"net={parsed['net_pnl']:>12.2f} pf={parsed['profit_factor']:.2f}")

    fields = ["symbol", "strategy", "from", "to", "trades", "wins", "losses", "win_rate",
              "gross_pnl", "total_charges", "net_pnl", "max_dd",
              "profit_factor", "expectancy", "avg_win", "avg_loss", "worst_day"]
    outpath = os.path.join(OUTDIR, "walkforward_real.csv")
    with open(outpath, "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=fields)
        w.writeheader()
        for r in all_rows:
            w.writerow(r)
    print(f"\nwrote {len(all_rows)} rows to {outpath}")


if __name__ == "__main__":
    main()
