package broker

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"

	"titan-algo/internal/strategy"
)

// Historical Data Request
type historicalRequest struct {
	Exchange    string `json:"exchange"`
	SymbolToken string `json:"symboltoken"`
	Interval    string `json:"interval"`
	FromDate    string `json:"fromdate"`
	ToDate      string `json:"todate"`
}

// Historical Response
type historicalResponse struct {
	Status  bool            `json:"status"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"` // Can be [][]interface{}
}

// angelDateFormat is the wall-clock format Angel's historical API expects
// (no timezone offset in the string itself — the API interprets it as IST).
const angelDateFormat = "2006-01-02 15:04"

// intervalDurations maps Angel's interval strings to their bar duration,
// used to truncate the fetch window to the last fully-completed candle.
var intervalDurations = map[string]time.Duration{
	"ONE_MINUTE":     time.Minute,
	"THREE_MINUTE":   3 * time.Minute,
	"FIVE_MINUTE":    5 * time.Minute,
	"TEN_MINUTE":     10 * time.Minute,
	"FIFTEEN_MINUTE": 15 * time.Minute,
	"THIRTY_MINUTE":  30 * time.Minute,
	"ONE_HOUR":       time.Hour,
	"ONE_DAY":        24 * time.Hour,
}

const (
	marketOpenHour    = 9
	marketOpenMinute  = 15
	marketCloseHour   = 15
	marketCloseMinute = 30
)

// truncateToLastCompletedCandle returns `now` (converted to IST) truncated
// down to the boundary of the last FULLY COMPLETED candle for the given
// interval, so a fetch never includes the currently-forming (in-progress)
// bar (ST-7: fixes "running intraday includes the currently-forming candle
// as the last bar", which repaints indicator values on refetch).
//
// This does not account for exchange holidays/weekends — that calendar
// lives with WP-9/discovery, which is out of scope for this package.
func truncateToLastCompletedCandle(now time.Time, interval string) time.Time {
	nowIST := now.In(strategy.IST)

	dur, ok := intervalDurations[interval]
	if !ok {
		// Unknown interval string: best effort, just normalize to IST.
		return nowIST
	}

	if dur >= 24*time.Hour {
		closeToday := time.Date(nowIST.Year(), nowIST.Month(), nowIST.Day(), marketCloseHour, marketCloseMinute, 0, 0, strategy.IST)
		if nowIST.Before(closeToday) {
			// Today's daily candle hasn't closed yet.
			return closeToday.AddDate(0, 0, -1)
		}
		return closeToday
	}

	openToday := time.Date(nowIST.Year(), nowIST.Month(), nowIST.Day(), marketOpenHour, marketOpenMinute, 0, 0, strategy.IST)
	if nowIST.Before(openToday) {
		// Before today's open: no candles have formed yet today, so the
		// last completed candle is yesterday's close (15:30), not today's
		// open.
		closeYesterday := time.Date(nowIST.Year(), nowIST.Month(), nowIST.Day(), marketCloseHour, marketCloseMinute, 0, 0, strategy.IST).AddDate(0, 0, -1)
		return closeYesterday
	}

	elapsed := nowIST.Sub(openToday)
	completed := elapsed / dur
	boundary := openToday.Add(completed * dur)

	closeToday := time.Date(nowIST.Year(), nowIST.Month(), nowIST.Day(), marketCloseHour, marketCloseMinute, 0, 0, strategy.IST)
	if boundary.After(closeToday) {
		boundary = closeToday
	}
	return boundary
}

// maxChunkDays is the per-request calendar-day window used when paging a
// FetchHistory call across Angel's historical API. Angel enforces two
// separate limits per request: a per-interval max date range (documented on
// the SmartAPI forum, e.g. 30 days for ONE_MINUTE, 100 days for
// FIVE_MINUTE), AND an undocumented-in-code but forum-confirmed cap of
// ~500 returned records regardless of interval. For intraday intervals the
// 500-record cap binds first (e.g. FIVE_MINUTE has ~75 candles/trading day,
// so 100 days would silently return far fewer than requested, or nothing,
// well before the day-limit is reached) -- Angel's API does NOT error on an
// oversized request, it just silently returns fewer/zero rows, which is
// exactly the kind of silent-wrong-answer this system must never accept.
// These values are deliberately conservative (well under both limits); if
// Angel changes their limits, widen this table rather than assume it's
// still correct.
var maxChunkDays = map[string]int{
	"ONE_MINUTE":     1,
	"THREE_MINUTE":   3,
	"FIVE_MINUTE":    5,
	"TEN_MINUTE":     10,
	"FIFTEEN_MINUTE": 15,
	"THIRTY_MINUTE":  30,
	"ONE_HOUR":       60,
	"ONE_DAY":        400,
}

// interChunkDelay paces successive chunk requests within one FetchHistory
// call. This is independent of (and in addition to) any rate limiting the
// caller applies between different symbols/targets (e.g. cmd/fetchdata's
// -rps flag) -- FetchHistory has no visibility into a caller-level limiter,
// so it must not assume the caller paces requests within a single symbol's
// multi-chunk fetch. Angel's getCandleData is documented at 3 req/s; 400ms
// (2.5 req/s) leaves headroom rather than riding the limit exactly.
const interChunkDelay = 400 * time.Millisecond

// FetchHistory fetches historical candle data from Angel One, paging
// internally across Angel's per-request day/record limits (see
// maxChunkDays) so a multi-year request returns the FULL range rather than
// silently truncating to whatever fit in one request.
// interval: "ONE_MINUTE", "FIVE_MINUTE", "ONE_DAY", etc.
// days: total number of days of history to fetch, ending at the last
// fully-completed candle.
func (a *AngelBroker) FetchHistory(symbol, interval string, days int) ([]strategy.Candle, error) {
	inst, err := a.instruments.GetInstrument(symbol)
	if err != nil {
		return nil, fmt.Errorf("unknown symbol %s: %v", symbol, err)
	}

	chunkDays, ok := maxChunkDays[interval]
	if !ok {
		return nil, fmt.Errorf("unknown interval %q: no known safe per-request chunk size (add it to maxChunkDays rather than guessing)", interval)
	}

	// Prepare dates in IST (ST-3 fix: was server-local time.Now(), which
	// silently fetches against the wrong window on a non-IST host). Also
	// truncate `to` to the last fully-completed candle (ST-7 fix).
	windowEnd := truncateToLastCompletedCandle(time.Now(), interval)
	windowStart := windowEnd.AddDate(0, 0, -days)

	log.Printf("Fetching %s history (%s) for %s [%s] from %s to %s IST (chunked at %d day(s)/request)",
		interval, inst.ExchSeg, symbol, inst.Token, windowStart.Format(angelDateFormat), windowEnd.Format(angelDateFormat), chunkDays)

	var all []strategy.Candle
	totalSkipped := 0
	chunkFrom := windowStart
	for chunkFrom.Before(windowEnd) {
		chunkTo := chunkFrom.AddDate(0, 0, chunkDays)
		if chunkTo.After(windowEnd) {
			chunkTo = windowEnd
		}

		candles, skipped, err := a.fetchHistoryChunk(inst, interval, chunkFrom, chunkTo)
		if err != nil {
			return nil, fmt.Errorf("chunk %s..%s: %w", chunkFrom.Format(angelDateFormat), chunkTo.Format(angelDateFormat), err)
		}
		all = append(all, candles...)
		totalSkipped += skipped

		chunkFrom = chunkTo
		if chunkFrom.Before(windowEnd) {
			time.Sleep(interChunkDelay)
		}
	}

	log.Printf("Received %d candles total (%d skipped as invalid) across the full requested range", len(all), totalSkipped)
	return all, nil
}

const historicalPath = "/rest/secure/angelbroking/historical/v1/getCandleData"

// fetchHistoryChunk performs exactly one getCandleData request for the
// given [from, to) window -- callers are responsible for keeping that
// window within Angel's per-request limits (see maxChunkDays). Routed
// through doAPIRequest (the same choke point every other authenticated
// Angel call uses) rather than a hand-rolled http.Client call, so a
// multi-chunk fetch that spans a session/token refresh boundary gets the
// same transparent 401-refresh-and-retry and WAF detection as every other
// endpoint, instead of silently failing partway through.
func (a *AngelBroker) fetchHistoryChunk(inst Instrument, interval string, fromIST, toIST time.Time) ([]strategy.Candle, int, error) {
	payload := historicalRequest{
		Exchange:    inst.ExchSeg,
		SymbolToken: inst.Token,
		Interval:    interval,
		FromDate:    fromIST.Format(angelDateFormat),
		ToDate:      toIST.Format(angelDateFormat),
	}

	reqBody, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}

	body, err := a.doAPIRequest("POST", historicalPath, reqBody)
	if err != nil {
		return nil, 0, err
	}

	var histResp historicalResponse
	if err := json.Unmarshal(body, &histResp); err != nil {
		return nil, 0, fmt.Errorf("json parse error: %v | Body: %s", err, string(body))
	}
	if !histResp.Status {
		return nil, 0, fmt.Errorf("API error: %s", histResp.Message)
	}

	var rows [][]interface{}
	if err := json.Unmarshal(histResp.Data, &rows); err != nil {
		return nil, 0, fmt.Errorf("failed to parse data rows: %v", err)
	}

	candles, skipped := parseCandleRows(rows)
	return candles, skipped, nil
}

// parseCandleRows converts raw Angel candle rows ([timestamp, O, H, L, C, V])
// into validated strategy.Candle values, in the same order as the input.
// Rows that fail parsing or sanity checks are SKIPPED (not zero-filled) —
// the caller's downstream indicators would otherwise be silently poisoned
// by fabricated zero-value candles (ST-10 fix). Returns the accepted
// candles plus a count of skipped rows.
func parseCandleRows(rows [][]interface{}) ([]strategy.Candle, int) {
	var candles []strategy.Candle
	skipped := 0
	var lastTs time.Time
	haveLastTs := false

	for _, row := range rows {
		c, err := parseCandleRow(row)
		if err != nil {
			log.Printf("Skipping invalid candle row: %v", err)
			skipped++
			continue
		}

		// Reject non-positive prices or an inverted High/Low.
		if c.Open <= 0 || c.High <= 0 || c.Low <= 0 || c.Close <= 0 {
			log.Printf("Skipping candle with non-positive price: %+v", c)
			skipped++
			continue
		}
		if c.High < c.Low {
			log.Printf("Skipping candle with High < Low: %+v", c)
			skipped++
			continue
		}

		// Assert non-decreasing timestamps (relative to the last ACCEPTED
		// row) — a single out-of-order row is dropped rather than failing
		// the whole fetch.
		if haveLastTs && c.Time.Before(lastTs) {
			log.Printf("Skipping out-of-order candle: %s before %s", c.Time, lastTs)
			skipped++
			continue
		}

		candles = append(candles, c)
		lastTs = c.Time
		haveLastTs = true
	}

	return candles, skipped
}

// parseCandleRow parses a single [Timestamp, Open, High, Low, Close, Volume]
// row. Numeric parse failures are now propagated as errors instead of being
// silently swallowed into a 0.0 value (the old behavior poisoned every
// downstream indicator with fabricated zero candles).
func parseCandleRow(row []interface{}) (strategy.Candle, error) {
	if len(row) < 6 {
		return strategy.Candle{}, fmt.Errorf("row has %d fields, need 6", len(row))
	}

	tsStr, ok := row[0].(string)
	if !ok {
		return strategy.Candle{}, fmt.Errorf("timestamp field is not a string: %v", row[0])
	}
	// Angel Hist format: "2021-02-08T09:15:00+05:30" (RFC3339)
	ts, err := time.Parse(time.RFC3339, tsStr)
	if err != nil {
		return strategy.Candle{}, fmt.Errorf("timestamp %q: %w", tsStr, err)
	}

	open, err := getFloat(row[1])
	if err != nil {
		return strategy.Candle{}, fmt.Errorf("open: %w", err)
	}
	high, err := getFloat(row[2])
	if err != nil {
		return strategy.Candle{}, fmt.Errorf("high: %w", err)
	}
	low, err := getFloat(row[3])
	if err != nil {
		return strategy.Candle{}, fmt.Errorf("low: %w", err)
	}
	closePrice, err := getFloat(row[4])
	if err != nil {
		return strategy.Candle{}, fmt.Errorf("close: %w", err)
	}
	vol, err := getFloat(row[5])
	if err != nil {
		return strategy.Candle{}, fmt.Errorf("volume: %w", err)
	}

	return strategy.Candle{
		Time:   ts,
		Open:   open,
		High:   high,
		Low:    low,
		Close:  closePrice,
		Volume: int64(vol),
	}, nil
}

// getFloat parses a JSON-decoded numeric field (float64 or string) into a
// float64, propagating parse errors instead of silently returning 0.
func getFloat(v interface{}) (float64, error) {
	switch val := v.(type) {
	case float64:
		return val, nil
	case string:
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid numeric value %q: %w", val, err)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("unexpected type %T for numeric field", v)
	}
}
