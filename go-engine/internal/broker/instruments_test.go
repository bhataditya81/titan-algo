package broker

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestInstrumentManager_HasHTTPTimeout(t *testing.T) {
	im := NewInstrumentManager()
	if im.httpClient == nil || im.httpClient.Timeout <= 0 {
		t.Fatal("expected a bounded http.Client timeout, not a bare http.Get (audit EX-9)")
	}
}

func TestLoadInstruments_DownloadsThenUsesDiskCacheOnSecondCall(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"token":"99926000","symbol":"NIFTY","name":"NIFTY","exch_seg":"NSE","lot_size":"75"}]`))
	}))
	defer srv.Close()

	origURL := instrumentURL
	instrumentURL = srv.URL
	t.Cleanup(func() { instrumentURL = origURL })

	im := NewInstrumentManager()
	im.SetCacheDir(t.TempDir())

	if err := im.LoadInstruments(); err != nil {
		t.Fatalf("first load failed: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected 1 network hit on first load, got %d", got)
	}
	if _, err := im.GetInstrument("NIFTY"); err != nil {
		t.Fatalf("expected NIFTY to be indexed: %v", err)
	}

	// Second manager, same cache dir, same day -> must read from disk, not network.
	im2 := NewInstrumentManager()
	im2.SetCacheDir(im.cacheDir)
	if err := im2.LoadInstruments(); err != nil {
		t.Fatalf("second (cached) load failed: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected still 1 network hit (second load should use disk cache), got %d", got)
	}
	if _, err := im2.GetInstrument("NIFTY"); err != nil {
		t.Fatalf("expected NIFTY to be indexed from cache: %v", err)
	}
}

func TestLoadInstruments_NonOKStatus_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	origURL := instrumentURL
	instrumentURL = srv.URL
	t.Cleanup(func() { instrumentURL = origURL })

	im := NewInstrumentManager()
	im.SetCacheDir(t.TempDir())

	if err := im.LoadInstruments(); err == nil {
		t.Fatal("expected error on non-200 instrument master response")
	}
}

// TestFindIndexSymbol_ResolvesRealQuotableInstrument reproduces the exact
// dual-row shape Angel's real instrument master has for index underlyings
// (confirmed against the live account: a bare-named, data-less row alongside
// the real AMXIDX-typed quotable one) and proves FindIndexSymbol picks the
// real one, not the bare name.
func TestFindIndexSymbol_ResolvesRealQuotableInstrument(t *testing.T) {
	im := NewInstrumentManager()
	im.instruments = map[string]Instrument{
		// The real, quotable index instrument.
		"Nifty 50": {Token: "99926000", Symbol: "Nifty 50", Name: "NIFTY", ExchSeg: "NSE", InstrumentType: "AMXIDX"},
		// The data-less placeholder row sharing the same Name -- this is
		// what a naive symbol-by-bare-name lookup would wrongly resolve to.
		"NIFTY": {Token: "26000", Symbol: "NIFTY", Name: "NIFTY", ExchSeg: "NSE", InstrumentType: ""},
	}

	got, err := im.FindIndexSymbol("NIFTY")
	if err != nil {
		t.Fatalf("FindIndexSymbol: %v", err)
	}
	if got != "Nifty 50" {
		t.Fatalf("want the real AMXIDX symbol %q, got %q (would silently fetch 0 candles forever)", "Nifty 50", got)
	}
}

// TestFindIndexSymbol_UnknownUnderlying_ErrorsRatherThanGuessing proves a
// non-index or unrecognized underlying is rejected, not defaulted to the
// bare name.
func TestFindIndexSymbol_UnknownUnderlying_ErrorsRatherThanGuessing(t *testing.T) {
	im := NewInstrumentManager()
	im.instruments = map[string]Instrument{
		"RELIANCE-EQ": {Token: "2885", Symbol: "RELIANCE-EQ", Name: "RELIANCE", ExchSeg: "NSE", InstrumentType: ""},
	}

	if _, err := im.FindIndexSymbol("NIFTY"); err == nil {
		t.Fatal("expected an error when no AMXIDX/NSE instrument matches, got nil")
	}
}

// TestFindIndexSymbol_AmbiguousMatch_ErrorsRatherThanGuessing proves that if
// the instrument master ever carries two DIFFERENT AMXIDX symbols for the
// same Name (a scenario this code has no way to disambiguate correctly),
// it refuses rather than picking one at random.
func TestFindIndexSymbol_AmbiguousMatch_ErrorsRatherThanGuessing(t *testing.T) {
	im := NewInstrumentManager()
	im.instruments = map[string]Instrument{
		"Nifty 50":  {Token: "99926000", Symbol: "Nifty 50", Name: "NIFTY", ExchSeg: "NSE", InstrumentType: "AMXIDX"},
		"NIFTY 50X": {Token: "99999999", Symbol: "NIFTY 50X", Name: "NIFTY", ExchSeg: "NSE", InstrumentType: "AMXIDX"},
	}

	if _, err := im.FindIndexSymbol("NIFTY"); err == nil {
		t.Fatal("expected an error for an ambiguous (two AMXIDX rows, same Name) match, got nil")
	}
}

// TestFindOption_FindsExactContractAmongManySharingAName reproduces the real
// bug hit against the live account: findOptionSymbol used to scan via
// Search, which caps at 50 results with random map iteration order --
// with 300+ instruments sharing one underlying's Name (many strikes x two
// option types x several expiries), the exact contract wanted could be
// missing from any given call's capped result set. FindOption must find it
// reliably regardless of how many other same-Name instruments exist.
func TestFindOption_FindsExactContractAmongManySharingAName(t *testing.T) {
	im := NewInstrumentManager()
	instruments := map[string]Instrument{}
	expiry := time.Date(2026, 7, 28, 0, 0, 0, 0, istLocation)
	expiryStr := "28JUL2026"

	// Simulate a realistic-sized chain: 200 decoy strikes across two
	// expiries and two option types, none of which is the one we want
	// (58500 is deliberately excluded from this range so it stays unique
	// to the "want" entry below).
	for i := 0; i < 80; i++ {
		strike := float64(50000 + i*100) // 50000..57900, never 58500
		sym := "BANKNIFTY" + expiryStr + itoaStrike(strike) + "CE"
		instruments[sym] = Instrument{Token: sym, Symbol: sym, Name: "BANKNIFTY", ExchSeg: "NFO", Expiry: expiryStr, StrikeFloat: strike * 100}
		symPE := "BANKNIFTY" + expiryStr + itoaStrike(strike) + "PE"
		instruments[symPE] = Instrument{Token: symPE, Symbol: symPE, Name: "BANKNIFTY", ExchSeg: "NFO", Expiry: expiryStr, StrikeFloat: strike * 100}
	}
	// The exact contract under test, buried among the 200 decoys.
	wantSym := "BANKNIFTY28JUL2658500CE"
	instruments[wantSym] = Instrument{Token: wantSym, Symbol: wantSym, Name: "BANKNIFTY", ExchSeg: "NFO", Expiry: expiryStr, StrikeFloat: 5850000}
	im.instruments = instruments

	// Run many times -- if this were still Search-based (map iteration,
	// 50-item cap), a flaky implementation would intermittently fail here.
	for i := 0; i < 20; i++ {
		got, err := im.FindOption("BANKNIFTY", expiry, 58500, "CE")
		if err != nil {
			t.Fatalf("iteration %d: FindOption: %v", i, err)
		}
		if got != wantSym {
			t.Fatalf("iteration %d: want %q, got %q", i, wantSym, got)
		}
	}
}

// TestFindOption_MatchesRawAndPaiseStrikeConventions proves both strike
// conventions the instrument master has been observed to use are handled.
func TestFindOption_MatchesRawAndPaiseStrikeConventions(t *testing.T) {
	expiry := time.Date(2026, 7, 28, 0, 0, 0, 0, istLocation)
	rawIM := NewInstrumentManager()
	rawIM.instruments = map[string]Instrument{
		"X": {Symbol: "BANKNIFTY28JUL2658500CE", Name: "BANKNIFTY", ExchSeg: "NFO", Expiry: "28JUL2026", StrikeFloat: 58500},
	}
	if got, err := rawIM.FindOption("BANKNIFTY", expiry, 58500, "CE"); err != nil || got != "BANKNIFTY28JUL2658500CE" {
		t.Fatalf("raw-rupee convention: got %q, err %v", got, err)
	}

	paiseIM := NewInstrumentManager()
	paiseIM.instruments = map[string]Instrument{
		"X": {Symbol: "BANKNIFTY28JUL2658500CE", Name: "BANKNIFTY", ExchSeg: "NFO", Expiry: "28JUL2026", StrikeFloat: 5850000},
	}
	if got, err := paiseIM.FindOption("BANKNIFTY", expiry, 58500, "CE"); err != nil || got != "BANKNIFTY28JUL2658500CE" {
		t.Fatalf("paise convention: got %q, err %v", got, err)
	}
}

// TestFindOption_NotFound_ErrorsRatherThanGuessing proves a genuinely
// missing contract (e.g. a strike NSE never listed for this expiry) errors
// instead of returning the nearest available symbol.
func TestFindOption_NotFound_ErrorsRatherThanGuessing(t *testing.T) {
	im := NewInstrumentManager()
	im.instruments = map[string]Instrument{
		"X": {Symbol: "BANKNIFTY28JUL2658500CE", Name: "BANKNIFTY", ExchSeg: "NFO", Expiry: "28JUL2026", StrikeFloat: 5850000},
	}
	expiry := time.Date(2026, 7, 28, 0, 0, 0, 0, istLocation)
	if _, err := im.FindOption("BANKNIFTY", expiry, 58600, "CE"); err == nil {
		t.Fatal("expected an error for a strike that isn't listed, got nil")
	}
	if _, err := im.FindOption("BANKNIFTY", expiry, 58500, "PE"); err == nil {
		t.Fatal("expected an error when only CE is listed at this strike, got nil")
	}
}

// itoaStrike formats a strike as an integer string (helper for building
// synthetic option symbols in tests).
func itoaStrike(strike float64) string {
	return strconv.Itoa(int(strike))
}
