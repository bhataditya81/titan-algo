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
