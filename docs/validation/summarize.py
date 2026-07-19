#!/usr/bin/env python3
"""Aggregates docs/validation/out/walkforward_windows.csv into one row per
strategy for RESULTS.md: pooled trades/win-rate/PF/expectancy across all 24
monthly OOS windows, worst single window, and the 2x-cost expectancy stress
test.

2x-cost method: the backtest's cost model is purely subtractive
(NetPnL = GrossPnL - Charges, verified against internal/backtest/report.go
and engine.go -- charges never feed back into fill price, stop triggers, or
trade count). Doubling every charge is therefore algebraically exact as
NetPnL' = GrossPnL - 2*Charges = NetPnL - Charges, computed straight from the
already-reported Net P&L and Total Charges columns -- no simulator re-run
needed, and no approximation. This does NOT reclassify individual trades
from win to loss (that needs the per-trade ledger, which report.String()
does not expose), so the "2x-cost profit factor" below is NOT recomputed --
only 2x-cost aggregate expectancy is (WP-10 gate only requires the latter).
"""
import csv
import os

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
IN = os.path.join(ROOT, "docs", "validation", "out", "walkforward_windows.csv")
OUT = os.path.join(ROOT, "docs", "validation", "out", "strategy_summary.csv")

def main():
    with open(IN) as f:
        rows = list(csv.DictReader(f))

    by_strat = {}
    for r in rows:
        s = r["strategy"]
        by_strat.setdefault(s, []).append(r)

    summary = []
    for strat, wrows in by_strat.items():
        trades = sum(int(r["trades"]) for r in wrows)
        wins = sum(int(r["wins"]) for r in wrows)
        losses = sum(int(r["losses"]) for r in wrows)
        gross = sum(float(r["gross_pnl"]) for r in wrows)
        charges = sum(float(r["total_charges"]) for r in wrows)
        net = sum(float(r["net_pnl"]) for r in wrows)
        total_win = sum(float(r["avg_win"]) * int(r["wins"]) for r in wrows)
        total_loss = sum(abs(float(r["avg_loss"])) * int(r["losses"]) for r in wrows)
        pf = (total_win / total_loss) if total_loss > 0 else (float("inf") if total_win > 0 else 0.0)
        expectancy = net / trades if trades else 0.0
        expectancy_2x_cost = (net - charges) / trades if trades else 0.0
        worst_window = min(wrows, key=lambda r: float(r["net_pnl"]))
        worst_day = min(float(r["worst_day"]) for r in wrows)
        max_dd = max(float(r["max_dd"]) for r in wrows)  # largest single-window drawdown observed
        losing_windows = sum(1 for r in wrows if float(r["net_pnl"]) < 0)
        summary.append({
            "strategy": strat,
            "windows": len(wrows),
            "losing_windows": losing_windows,
            "trades": trades,
            "win_rate_pct": round(100 * wins / (wins + losses), 2) if (wins + losses) else 0.0,
            "gross_pnl": round(gross, 2),
            "total_charges": round(charges, 2),
            "net_pnl": round(net, 2),
            "profit_factor_pooled": round(pf, 3) if pf != float("inf") else "inf",
            "expectancy_per_trade": round(expectancy, 2),
            "expectancy_per_trade_2x_cost": round(expectancy_2x_cost, 2),
            "max_single_window_dd": round(max_dd, 2),
            "worst_single_day": round(worst_day, 2),
            "worst_window": f"{worst_window['from']}..{worst_window['to']} ({float(worst_window['net_pnl']):.2f})",
        })

    fields = list(summary[0].keys())
    with open(OUT, "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=fields)
        w.writeheader()
        for r in summary:
            w.writerow(r)

    for r in summary:
        print(r)

if __name__ == "__main__":
    main()
