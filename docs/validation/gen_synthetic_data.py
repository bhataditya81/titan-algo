#!/usr/bin/env python3
"""Synthetic NIFTY-like 5-min OHLCV generator for WP-10 validation.

WHY THIS EXISTS: WP-10 needs >=2 years of 5-min candle data to walk-forward
test the backtest engine (internal/backtest). No real cached NSE data exists
anywhere in this repo (checked: no *.csv under go-engine, no data/ dir with
candles -- only internal/backtest/*_test.go embed tiny inline fixtures), and
fetching real data requires a live Angel One broker session, which the hard
constraints for this task forbid (no credentials, no -live). So this script
fabricates a plausible NIFTY-like index path and writes it in the exact CSV
cache format internal/backtest/cache.go expects (header
"time,open,high,low,close,volume", RFC3339 timestamps, IST offset).

ALL RESULTS DERIVED FROM THIS DATA ARE METHODOLOGY DEMONSTRATIONS, NOT
EVIDENCE OF REAL MARKET EDGE. See docs/validation/RESULTS.md for the caveat
restated in the actual findings.

Regime schedule (deliberately varied per the WP-10 brief -- trending months,
range-bound months, one sharp-gap/high-vol event):

  2024-01 .. 2024-04   range-bound, low vol, mean-reverting
  2024-05 .. 2024-09   steady uptrend
  2024-10              sharp gap-down shock (single-day ~-6% gap) + elevated
                        vol for the rest of the month
  2024-11 .. 2025-02   volatile recovery / choppy bottoming
  2025-03 .. 2025-08   range-bound, low vol
  2025-09 .. 2026-02   steady uptrend
  2026-03 .. 2026-06   mixed/volatile range

Index path: geometric random walk, one step per 5-min bar, with a
regime-dependent per-bar drift and vol. Session 09:15-15:30 IST, Mon-Fri only
(Indian market holidays are NOT modeled -- a known simplification, noted in
the report). Base level ~22000 (rough NIFTY spot order of magnitude).
"""
import csv
import datetime as dt
import math
import random

random.seed(20260719)  # reproducible

IST = dt.timezone(dt.timedelta(hours=5, minutes=30))
BASE = 22000.0
OUT = "docs/validation/data/nifty_synthetic_5min.csv"

# (year, month) -> (drift_per_bar, vol_per_bar) as fraction of price.
# ~75 bars/trading day. Drift chosen so a "trend" month moves several
# thousand points; vol chosen so range-bound months chop sideways.
REGIME = {}
def set_regime(y0, m0, y1, m1, drift, vol):
    y, m = y0, m0
    while (y, m) <= (y1, m1):
        REGIME[(y, m)] = (drift, vol)
        m += 1
        if m > 12:
            m = 1
            y += 1

set_regime(2024, 1, 2024, 4, 0.0000, 0.0006)     # range-bound
set_regime(2024, 5, 2024, 9, 0.000025, 0.0007)   # uptrend (~+20% over 5mo)
set_regime(2024, 10, 2024, 10, -0.00001, 0.0022) # shock month, high vol
set_regime(2024, 11, 2025, 2, 0.000008, 0.0016)  # volatile choppy recovery
set_regime(2025, 3, 2025, 8, 0.0000, 0.0006)     # range-bound
set_regime(2025, 9, 2026, 2, 0.000025, 0.0007)   # uptrend
set_regime(2026, 3, 2026, 6, 0.000003, 0.0015)   # mixed/volatile

def session_bars(day):
    """5-min bar start times for one trading day, 09:15..15:25 IST (75 bars)."""
    start = dt.datetime(day.year, day.month, day.day, 9, 15, tzinfo=IST)
    for i in range(75):
        yield start + dt.timedelta(minutes=5 * i)

def gen():
    rows = []
    price = BASE
    d = dt.date(2024, 1, 1)
    end = dt.date(2026, 6, 30)
    shock_done = False
    while d <= end:
        if d.weekday() < 5:  # Mon-Fri only; holidays not modeled (simplification)
            drift, vol = REGIME.get((d.year, d.month), (0.0, 0.0008))
            # the single sharp-gap event: first trading day of Oct 2024
            if d.year == 2024 and d.month == 10 and not shock_done:
                price *= 0.94  # ~6% overnight gap down
                shock_done = True
            day_open = price
            for bar_start in session_bars(d):
                o = price
                step = random.gauss(drift, vol)
                price = price * (1 + step)
                # intrabar high/low: small extra noise around the O->C move
                hi = max(o, price) * (1 + abs(random.gauss(0, vol * 0.4)))
                lo = min(o, price) * (1 - abs(random.gauss(0, vol * 0.4)))
                vol_bar = int(abs(random.gauss(150000, 60000))) + 10000
                rows.append((bar_start, o, hi, lo, price, vol_bar))
        d += dt.timedelta(days=1)
    return rows

def main():
    rows = gen()
    with open(OUT, "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["time", "open", "high", "low", "close", "volume"])
        for t, o, h, l, c, v in rows:
            w.writerow([
                t.isoformat(),
                f"{o:.4f}", f"{h:.4f}", f"{l:.4f}", f"{c:.4f}", v,
            ])
    print(f"wrote {len(rows)} bars to {OUT}, {rows[0][0]} -> {rows[-1][0]}")
    print(f"price range: {min(r[4] for r in rows):.1f} .. {max(r[4] for r in rows):.1f}")

if __name__ == "__main__":
    main()
