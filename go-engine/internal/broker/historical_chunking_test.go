package broker

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestFetchHistory_ChunksAcrossMultipleRequests proves the fix for a real
// production bug: Angel's historical API silently returns fewer/zero rows
// when a single request's date range exceeds its per-interval limit (it
// does not error), so a naive single-shot request for a multi-year range
// returns 0 candles with no indication why. FetchHistory must page the
// request internally and never send a single request wider than
// maxChunkDays[interval].
func TestFetchHistory_ChunksAcrossMultipleRequests(t *testing.T) {
	const interval = "FIVE_MINUTE" // maxChunkDays["FIVE_MINUTE"] == 5
	var mu sync.Mutex
	var gotRanges []struct{ From, To string }

	b, _ := newTestBroker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req historicalRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		mu.Lock()
		gotRanges = append(gotRanges, struct{ From, To string }{req.FromDate, req.ToDate})
		mu.Unlock()

		// One candle per request is enough to prove concatenation works;
		// the exact OHLCV values don't matter for this test.
		from, err := time.ParseInLocation(angelDateFormat, req.FromDate, istLocation)
		if err != nil {
			t.Fatalf("server: parse fromdate %q: %v", req.FromDate, err)
		}
		row := []interface{}{from.Format(time.RFC3339), "100", "110", "90", "105", "1000"}
		resp := historicalResponse{Status: true, Message: "SUCCESS"}
		resp.Data, _ = json.Marshal([][]interface{}{row})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))

	// 12 days at a 5-day chunk size must produce 3 chunks (5 + 5 + 2), not
	// one oversized request that Angel would silently truncate.
	const requestedDays = 12
	candles, err := b.FetchHistory("NIFTY", interval, requestedDays)
	if err != nil {
		t.Fatalf("FetchHistory: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	wantChunks := 3
	if len(gotRanges) != wantChunks {
		t.Fatalf("want %d chunked requests for a %d-day fetch at %d-day chunks, got %d: %+v",
			wantChunks, requestedDays, maxChunkDays[interval], len(gotRanges), gotRanges)
	}
	if len(candles) != wantChunks {
		t.Fatalf("want %d concatenated candles (one per chunk in this fixture), got %d", wantChunks, len(candles))
	}

	// Verify chunk windows are contiguous and none exceeds maxChunkDays.
	for i, rng := range gotRanges {
		from, err := time.ParseInLocation(angelDateFormat, rng.From, istLocation)
		if err != nil {
			t.Fatalf("chunk %d: parse From %q: %v", i, rng.From, err)
		}
		to, err := time.ParseInLocation(angelDateFormat, rng.To, istLocation)
		if err != nil {
			t.Fatalf("chunk %d: parse To %q: %v", i, rng.To, err)
		}
		if span := to.Sub(from); span > time.Duration(maxChunkDays[interval])*24*time.Hour {
			t.Fatalf("chunk %d spans %v, exceeds the %d-day safe limit for %s", i, span, maxChunkDays[interval], interval)
		}
		if i > 0 {
			prevTo, _ := time.ParseInLocation(angelDateFormat, gotRanges[i-1].To, istLocation)
			if !from.Equal(prevTo) {
				t.Fatalf("chunk %d starts at %v, expected it to continue exactly from the previous chunk's end %v (gap or overlap)", i, from, prevTo)
			}
		}
	}
}

// TestFetchHistory_UnknownInterval_ErrorsRatherThanGuessing proves that an
// interval string with no known safe chunk size is rejected outright,
// instead of silently falling through to an unsafe or arbitrary window.
func TestFetchHistory_UnknownInterval_ErrorsRatherThanGuessing(t *testing.T) {
	b, _ := newTestBroker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no HTTP request should be made for an unknown interval")
	}))

	if _, err := b.FetchHistory("NIFTY", "SEVEN_MINUTE", 10); err == nil {
		t.Fatal("expected an error for an interval with no known chunk size, got nil")
	}
}

// TestFetchHistory_SingleChunk_NoUnnecessaryPaging proves a request that
// already fits within one chunk makes exactly one HTTP call (no wasted
// requests or artificial delay for the common case).
func TestFetchHistory_SingleChunk_NoUnnecessaryPaging(t *testing.T) {
	var reqCount int
	var mu sync.Mutex

	b, _ := newTestBroker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqCount++
		mu.Unlock()
		resp := historicalResponse{Status: true, Message: "SUCCESS"}
		row := []interface{}{time.Now().Format(time.RFC3339), "100", "110", "90", "105", "1000"}
		resp.Data, _ = json.Marshal([][]interface{}{row})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))

	if _, err := b.FetchHistory("NIFTY", "FIVE_MINUTE", 2); err != nil { // 2 days < 5-day chunk size
		t.Fatalf("FetchHistory: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if reqCount != 1 {
		t.Fatalf("want exactly 1 request for a range within one chunk, got %d", reqCount)
	}
}
