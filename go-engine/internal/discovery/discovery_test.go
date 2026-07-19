package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"titan-algo/internal/broker"
	"titan-algo/internal/strategy"
)

// seedInstruments writes a scripmaster JSON straight to the disk-cache path
// InstrumentManager.LoadInstruments looks for, so these tests never touch
// the network (same pattern as internal/broker/instruments_test.go).
func seedInstruments(t *testing.T, rows []map[string]string) *broker.InstrumentManager {
	t.Helper()
	dir := t.TempDir()
	im := broker.NewInstrumentManager()
	im.SetCacheDir(dir)

	today := time.Now().In(strategy.IST).Format("2006-01-02")
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	path := filepath.Join(dir, "scripmaster_"+today+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture cache: %v", err)
	}
	if err := im.LoadInstruments(); err != nil {
		t.Fatalf("LoadInstruments from seeded cache: %v", err)
	}
	return im
}

func angelExpiry(t time.Time) string {
	return strings.ToUpper(t.Format("02Jan2006"))
}

func newDiscoveryWithInstruments(t *testing.T, im *broker.InstrumentManager, lotSizes map[string]int) *SymbolDiscovery {
	t.Helper()
	sd := NewSymbolDiscovery(broker.NewMockBroker(10000), []string{"NIFTY"}, lotSizes, 0)
	sd.instruments = im
	return sd
}

func TestResolveLotSize_UsesInstrumentMasterFirst(t *testing.T) {
	now := time.Now().In(strategy.IST)
	nextWeek := angelExpiry(now.AddDate(0, 0, 7))
	im := seedInstruments(t, []map[string]string{
		{"token": "1", "symbol": "NIFTY" + nextWeek + "25000CE", "name": "NIFTY", "exch_seg": "NFO", "expiry": nextWeek, "lot_size": "75"},
	})
	sd := newDiscoveryWithInstruments(t, im, nil)

	got, err := sd.resolveLotSize("NIFTY", "NIFTY"+nextWeek+"25000CE")
	if err != nil {
		t.Fatalf("resolveLotSize: %v", err)
	}
	if got != 75 {
		t.Fatalf("want lot size 75 from instrument master, got %d", got)
	}
}

func TestResolveLotSize_FallsBackToConfigOverrideOnly(t *testing.T) {
	// buildIndex deliberately rejects a truly empty instrument master (fail
	// closed against a corrupt/truncated download), so seed one unrelated
	// row to keep the list non-empty while the target symbol stays absent.
	im := seedInstruments(t, []map[string]string{
		{"token": "999", "symbol": "UNRELATED-EQ", "name": "UNRELATED", "exch_seg": "NSE"},
	}) // empty master: symbol unresolvable
	sd := newDiscoveryWithInstruments(t, im, map[string]int{"NIFTY": 75})

	got, err := sd.resolveLotSize("NIFTY", "NIFTY_UNKNOWN_SYMBOL")
	if err != nil {
		t.Fatalf("resolveLotSize with config override: %v", err)
	}
	if got != 75 {
		t.Fatalf("want config-override lot size 75, got %d", got)
	}
}

func TestResolveLotSize_ErrorsRatherThanGuessing(t *testing.T) {
	// buildIndex deliberately rejects a truly empty instrument master (fail
	// closed against a corrupt/truncated download), so seed one unrelated
	// row to keep the list non-empty while the target symbol stays absent.
	im := seedInstruments(t, []map[string]string{
		{"token": "999", "symbol": "UNRELATED-EQ", "name": "UNRELATED", "exch_seg": "NSE"},
	})
	sd := newDiscoveryWithInstruments(t, im, nil) // no instrument data, no config override

	_, err := sd.resolveLotSize("NIFTY", "NIFTY_UNKNOWN_SYMBOL")
	if err == nil {
		t.Fatal("expected an error when lot size cannot be resolved from any source, got nil (would have silently guessed)")
	}
}

func TestTargetExpiries_PicksNearestFutureOnly(t *testing.T) {
	now := time.Now().In(strategy.IST)
	past := angelExpiry(now.AddDate(0, 0, -7))
	near := angelExpiry(now.AddDate(0, 0, 7))
	next := angelExpiry(now.AddDate(0, 0, 14))
	far := angelExpiry(now.AddDate(0, 0, 60))

	im := seedInstruments(t, []map[string]string{
		{"token": "1", "symbol": "NIFTY" + past + "25000CE", "name": "NIFTY", "exch_seg": "NFO", "expiry": past, "lot_size": "75"},
		{"token": "2", "symbol": "NIFTY" + near + "25000CE", "name": "NIFTY", "exch_seg": "NFO", "expiry": near, "lot_size": "75"},
		{"token": "3", "symbol": "NIFTY" + next + "25000CE", "name": "NIFTY", "exch_seg": "NFO", "expiry": next, "lot_size": "75"},
		{"token": "4", "symbol": "NIFTY" + far + "25000CE", "name": "NIFTY", "exch_seg": "NFO", "expiry": far, "lot_size": "75"},
	})
	sd := newDiscoveryWithInstruments(t, im, nil)

	targets, err := sd.targetExpiries("NIFTY", now)
	if err != nil {
		t.Fatalf("targetExpiries: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("want nearest 2 future expiries, got %d: %v", len(targets), targets)
	}
	wantNear := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, 7)
	if !targets[0].Equal(wantNear) {
		t.Fatalf("want first target %v, got %v (past expiry must never be selected)", wantNear, targets[0])
	}
}

func TestTargetExpiries_ErrorsWhenNoFutureExpiryExists(t *testing.T) {
	now := time.Now().In(strategy.IST)
	past := angelExpiry(now.AddDate(0, 0, -7))
	im := seedInstruments(t, []map[string]string{
		{"token": "1", "symbol": "NIFTY" + past + "25000CE", "name": "NIFTY", "exch_seg": "NFO", "expiry": past, "lot_size": "75"},
	})
	sd := newDiscoveryWithInstruments(t, im, nil)

	if _, err := sd.targetExpiries("NIFTY", now); err == nil {
		t.Fatal("expected an error when every known expiry is in the past, got nil")
	}
}

func TestFindOptionChains_SkipsChainWithUnresolvableLotSize(t *testing.T) {
	now := time.Now().In(strategy.IST)
	near := angelExpiry(now.AddDate(0, 0, 7))
	im := seedInstruments(t, []map[string]string{
		// No lot_size at all -- must be skipped, never assigned a guessed value.
		{"token": "1", "symbol": "NIFTY" + near + "25000CE", "name": "NIFTY", "exch_seg": "NFO", "expiry": near},
	})
	sd := newDiscoveryWithInstruments(t, im, nil) // no config override either

	chains := sd.findOptionChains("NIFTY")
	if len(chains) != 0 {
		t.Fatalf("expected the unresolvable-lot-size chain to be skipped, got %d chains: %+v", len(chains), chains)
	}
}

func TestFindOptionChains_IncludesResolvableChainWithRealExpiryAndLotSize(t *testing.T) {
	now := time.Now().In(strategy.IST)
	near := angelExpiry(now.AddDate(0, 0, 7))
	stale := angelExpiry(now.AddDate(0, 0, -30))
	im := seedInstruments(t, []map[string]string{
		{"token": "1", "symbol": "NIFTY" + near + "25000CE", "name": "NIFTY", "exch_seg": "NFO", "expiry": near, "lot_size": "75"},
		{"token": "2", "symbol": "NIFTY" + stale + "25000CE", "name": "NIFTY", "exch_seg": "NFO", "expiry": stale, "lot_size": "75"},
	})
	sd := newDiscoveryWithInstruments(t, im, nil)

	chains := sd.findOptionChains("NIFTY")
	if len(chains) != 1 {
		t.Fatalf("want exactly 1 chain (the non-expired one), got %d: %+v", len(chains), chains)
	}
	if chains[0].LotSize != 75 {
		t.Fatalf("want real lot size 75 from instrument master, got %d", chains[0].LotSize)
	}
	if chains[0].Expiry != strings.ToUpper(near) {
		t.Fatalf("want authoritative expiry %s, got %s", strings.ToUpper(near), chains[0].Expiry)
	}
}
