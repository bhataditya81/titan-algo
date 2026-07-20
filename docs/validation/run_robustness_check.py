#!/usr/bin/env python3
"""Parameter robustness check for momentum and ema_crossover strategies.

Tests whether the promising results from RESULTS_REAL.md are robust to small
parameter variations, or whether they're a lucky/overfit pick of the defaults.

Runs each strategy over multiple parameter combinations using real BANKNIFTY
market data and writes results to docs/validation/out/robustness_check.csv.
"""
import csv
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))  # repo root (parent of docs/)
ROOT = os.path.dirname(ROOT)
EXE = os.path.join(ROOT, "docs", "validation", "bin", "backtest_robustness.exe")
DATA = os.path.join(ROOT, "go-engine", "data", "historical", "BANKNIFTY.csv")
OUTDIR = os.path.join(ROOT, "docs", "validation", "out")

# Ensure outdir exists
os.makedirs(OUTDIR, exist_ok=True)

FIELD_PATTERNS = {
    "trades": r"Total Trades:\s+(\d+)",
    "win_rate": r"Win Rate:\s+([\d.]+)%",
    "net_pnl": r"Net P&L:\s+Rs\. (-?[\d.]+)",
    "profit_factor": r"Profit Factor:\s+([\d.]+|\+?Inf)",
    "expectancy": r"Expectancy:\s+Rs\. (-?[\d.]+) / trade",
}

# Date range: full available data
FROM_DATE = "2024-07-22"
TO_DATE = "2026-07-17"

# EMA Crossover parameter combinations: FastPeriod, SlowPeriod
EMA_PARAMS = [
    {"FastPeriod": 9, "SlowPeriod": 21},      # default
    {"FastPeriod": 7, "SlowPeriod": 18},      # faster both
    {"FastPeriod": 12, "SlowPeriod": 21},     # slower fast
    {"FastPeriod": 9, "SlowPeriod": 25},      # slower slow
    {"FastPeriod": 5, "SlowPeriod": 15},      # much faster
    {"FastPeriod": 14, "SlowPeriod": 30},     # much slower
]

# Momentum parameter combinations
# Key parameters: RSIPeriod, RSIOversold, RSIOverbought, MinSignalStrength
MOMENTUM_PARAMS = [
    {
        "RSIPeriod": 14,
        "RSIOversold": 35,
        "RSIOverbought": 65,
        "MinSignalStrength": 0.6,
    },  # default
    {
        "RSIPeriod": 14,
        "RSIOversold": 30,
        "RSIOverbought": 70,
        "MinSignalStrength": 0.6,
    },  # tighter RSI (stricter)
    {
        "RSIPeriod": 14,
        "RSIOversold": 40,
        "RSIOverbought": 60,
        "MinSignalStrength": 0.6,
    },  # looser RSI
    {
        "RSIPeriod": 14,
        "RSIOversold": 35,
        "RSIOverbought": 65,
        "MinSignalStrength": 0.5,
    },  # lower threshold
    {
        "RSIPeriod": 14,
        "RSIOversold": 35,
        "RSIOverbought": 65,
        "MinSignalStrength": 0.7,
    },  # higher threshold
    {
        "RSIPeriod": 10,
        "RSIOversold": 35,
        "RSIOverbought": 65,
        "MinSignalStrength": 0.6,
    },  # faster RSI period
]


def run_backtest(strategy, params=None):
    """Run backtest with given params, return stdout+stderr."""
    cmd = [
        EXE,
        "-strategy",
        strategy,
        "-symbol",
        "BANKNIFTY",
        "-csv",
        DATA,
        "-from",
        FROM_DATE,
        "-to",
        TO_DATE,
        "-lotsize",
        "30",
        "-strikestep",
        "100",
    ]
    if params:
        param_str = ",".join(f"{k}={v}" for k, v in params.items())
        cmd += ["-params", param_str]
    p = subprocess.run(cmd, capture_output=True, text=True, cwd=ROOT)
    return p.stdout + p.stderr


def parse_report(text):
    """Parse backtest report output, return dict or None if parsing fails."""
    out = {}
    m = re.search(FIELD_PATTERNS["trades"], text)
    if not m:
        return None
    out["trades"] = int(m.group(1))

    for key in ["win_rate", "net_pnl", "expectancy"]:
        mm = re.search(FIELD_PATTERNS[key], text)
        out[key] = float(mm.group(1)) if mm else 0.0

    mm = re.search(FIELD_PATTERNS["profit_factor"], text)
    if mm:
        v = mm.group(1)
        out["profit_factor"] = float("inf") if "Inf" in v else float(v)
    else:
        out["profit_factor"] = 0.0
    return out


def params_to_string(params):
    """Convert params dict to readable string."""
    return ",".join(f"{k}={v}" for k, v in sorted(params.items()))


def main():
    rows = []

    print("\n" + "=" * 100)
    print("EMA Crossover Robustness Check")
    print("=" * 100)
    for params in EMA_PARAMS:
        text = run_backtest("ema_crossover", params)
        parsed = parse_report(text)
        if parsed is None:
            print(
                f"ERROR: Failed to parse report for ema_crossover {params_to_string(params)}"
            )
            print(f"Output:\n{text}")
            return 1
        parsed["strategy"] = "ema_crossover"
        parsed["params"] = params_to_string(params)
        rows.append(parsed)
        is_default = (
            params.get("FastPeriod") == 9 and params.get("SlowPeriod") == 21
        )
        marker = " [DEFAULT]" if is_default else ""
        print(
            f"  {parsed['params']:30s} trades={parsed['trades']:3d} "
            f"pf={parsed['profit_factor']:6.2f} pnl={parsed['net_pnl']:>12.2f} "
            f"expectancy={parsed['expectancy']:>10.2f}{marker}"
        )

    print("\n" + "=" * 100)
    print("Momentum Robustness Check")
    print("=" * 100)
    for params in MOMENTUM_PARAMS:
        text = run_backtest("momentum", params)
        parsed = parse_report(text)
        if parsed is None:
            print(
                f"ERROR: Failed to parse report for momentum {params_to_string(params)}"
            )
            print(f"Output:\n{text}")
            return 1
        parsed["strategy"] = "momentum"
        parsed["params"] = params_to_string(params)
        rows.append(parsed)
        is_default = (
            params.get("RSIPeriod") == 14
            and params.get("RSIOversold") == 35
            and params.get("RSIOverbought") == 65
            and params.get("MinSignalStrength") == 0.6
        )
        marker = " [DEFAULT]" if is_default else ""
        print(
            f"  {parsed['params']:60s} trades={parsed['trades']:3d} "
            f"pf={parsed['profit_factor']:6.2f} pnl={parsed['net_pnl']:>12.2f} "
            f"expectancy={parsed['expectancy']:>10.2f}{marker}"
        )

    # Write CSV
    outfile = os.path.join(OUTDIR, "robustness_check.csv")
    fields = ["strategy", "params", "trades", "win_rate", "net_pnl", "profit_factor", "expectancy"]
    with open(outfile, "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=fields)
        w.writeheader()
        for r in rows:
            w.writerow(r)
    print(f"\nwrote {len(rows)} rows to docs/validation/out/robustness_check.csv")
    return 0


if __name__ == "__main__":
    sys.exit(main())
