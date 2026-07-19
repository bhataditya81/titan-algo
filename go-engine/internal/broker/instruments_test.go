package broker

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
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
