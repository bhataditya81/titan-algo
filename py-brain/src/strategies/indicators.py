"""Technical indicators for py-brain.

WP-8 task 7 / audit §3 (py-brain stubs): this module used to hard-import
`cudf` at module scope with an entirely commented-out body — importing it
crashed on any machine without RAPIDS/cuDF installed, for zero actual
benefit (nothing was implemented). GPU acceleration is not implemented yet;
this is a working pandas/numpy fallback so the module is at least usable
(and importable) on a normal CPU machine. Swap in a cuDF-backed path behind
a feature flag if/when GPU support is built for real.
"""

import pandas as pd


def calculate_indicators(df: pd.DataFrame) -> pd.DataFrame:
    """Calculates basic technical indicators on a DataFrame with a 'close'
    column. Returns a copy with new columns added; does not mutate the input.
    """
    out = df.copy()
    out["sma_20"] = out["close"].rolling(window=20, min_periods=1).mean()
    out["ema_9"] = out["close"].ewm(span=9, adjust=False).mean()
    out["ema_21"] = out["close"].ewm(span=21, adjust=False).mean()

    delta = out["close"].diff()
    gain = delta.clip(lower=0)
    loss = -delta.clip(upper=0)
    avg_gain = gain.rolling(window=14, min_periods=1).mean()
    avg_loss = loss.rolling(window=14, min_periods=1).mean()
    rs = avg_gain / avg_loss.replace(0, float("nan"))
    out["rsi_14"] = (100 - (100 / (1 + rs))).fillna(50.0)

    return out


if __name__ == "__main__":
    # ponytail: smallest runnable check, not a test framework.
    sample = pd.DataFrame({"close": [1.0, 2.0, 3.0, 2.0, 1.0, 2.0, 3.0, 4.0]})
    result = calculate_indicators(sample)
    assert set(sample.columns).issubset(result.columns)
    assert "sma_20" in result.columns and "rsi_14" in result.columns
    assert result["rsi_14"].between(0, 100).all()
    print("indicators self-check OK")
