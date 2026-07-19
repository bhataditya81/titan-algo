package strategy

import (
	"sync"
	"time"
)

// CandleAggregator builds real-range OHLC candles from a raw tick stream
// (price + cumulative volume + timestamp), bucketed to wall-clock IST
// interval boundaries.
//
// This is the shared, generic form of the per-symbol tick-aggregation
// SniperStrategy has always done internally (updateCandle) — factored out
// here (R2-2 / G-2 fix) so any caller with only a raw tick feed can build a
// real EvalContext.Candles buffer instead of relying on strategies to
// degrade to tick-mode themselves. Notably: internal/engine/runner.go
// currently feeds ONLY EvalContext.Prices (raw ticks) to every strategy and
// never populates Candles — see docs/reports/R2-2-REPORT.md for the exact
// change needed there to wire this in. Until that lands, SniperStrategy's
// own fallback path (this same aggregator, held per-instance) is the only
// consumer.
//
// Thread-safe; one instance tracks many symbols independently.
type CandleAggregator struct {
	interval time.Duration
	maxKeep  int // 0 = unbounded

	mu       sync.Mutex
	done     map[string][]Candle
	building map[string]*aggBucket
}

type aggBucket struct {
	bucketStart time.Time
	candle      Candle
	lastCumVol  float64
	haveLastVol bool
}

// NewCandleAggregator creates an aggregator that buckets ticks into candles
// of the given interval (e.g. 5*time.Minute; <=0 defaults to 5 minutes).
// maxKeep caps how many completed candles are retained per symbol (0 means
// unbounded, matching the pre-R2-2 sniper.go behavior).
func NewCandleAggregator(interval time.Duration, maxKeep int) *CandleAggregator {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &CandleAggregator{
		interval: interval,
		maxKeep:  maxKeep,
		done:     make(map[string][]Candle),
		building: make(map[string]*aggBucket),
	}
}

// Add feeds one tick (price + CUMULATIVE volume, e.g. a broker
// GetCurrentPrice/GetCurrentVolume poll reading) for symbol at time now
// (converted to IST internally). Returns true exactly on the tick that
// completes a prior bucket (crosses an interval boundary) — false for every
// tick that merely updates the in-progress candle.
//
// Real-range guarantee: Open is the first tick's price in the bucket; High/
// Low track the actual min/max seen; Close is the latest tick's price;
// Volume is the delta of cumulative volume across the bucket. A bucket fed
// only one tick is legitimately a zero-range doji (there is no second data
// point to form a range from) — that is correct, not a bug; feed ticks at a
// frequency finer than the interval to get real ranges.
func (a *CandleAggregator) Add(symbol string, price, cumVolume float64, now time.Time) bool {
	if now.IsZero() {
		now = time.Now()
	}
	bucketStart := floorToInterval(now.In(IST), a.interval)

	a.mu.Lock()
	defer a.mu.Unlock()

	st, exists := a.building[symbol]
	if !exists || !st.bucketStart.Equal(bucketStart) {
		completed := exists
		if exists {
			a.done[symbol] = append(a.done[symbol], st.candle)
			if a.maxKeep > 0 && len(a.done[symbol]) > a.maxKeep {
				a.done[symbol] = a.done[symbol][len(a.done[symbol])-a.maxKeep:]
			}
		}
		a.building[symbol] = &aggBucket{
			bucketStart: bucketStart,
			candle:      Candle{Time: bucketStart, Open: price, High: price, Low: price, Close: price},
			lastCumVol:  cumVolume,
			haveLastVol: true,
		}
		return completed
	}

	// Same bucket: extend the in-progress candle's real range.
	st.candle.Close = price
	if price > st.candle.High {
		st.candle.High = price
	}
	if price < st.candle.Low {
		st.candle.Low = price
	}
	if st.haveLastVol && cumVolume >= st.lastCumVol {
		st.candle.Volume += int64(cumVolume - st.lastCumVol)
	}
	st.lastCumVol = cumVolume
	st.haveLastVol = true
	return false
}

// Completed returns a copy of the completed-candle history for symbol
// (oldest to newest). The in-progress candle is not included — it is only
// appended once a later tick crosses into the next bucket.
func (a *CandleAggregator) Completed(symbol string) []Candle {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Candle(nil), a.done[symbol]...)
}

// floorToInterval floors t (already in the desired location) down to the
// nearest multiple of interval since local midnight.
func floorToInterval(t time.Time, interval time.Duration) time.Time {
	if interval <= 0 {
		return t
	}
	y, mo, d := t.Date()
	dayStart := time.Date(y, mo, d, 0, 0, 0, 0, t.Location())
	elapsed := t.Sub(dayStart)
	floored := (elapsed / interval) * interval
	return dayStart.Add(floored)
}
