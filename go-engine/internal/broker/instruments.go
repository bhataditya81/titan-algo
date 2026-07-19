package broker

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Instrument represents a single scrip/contract from Angel One
type Instrument struct {
	Token          string `json:"token"`
	Symbol         string `json:"symbol"`
	Name           string `json:"name"`
	Expiry         string `json:"expiry"`
	Strike         string `json:"strike"`
	TickSize       string `json:"tick_size"`
	LotSize        string `json:"lot_size"`
	InstrumentType string `json:"instrumenttype"`
	ExchSeg        string `json:"exch_seg"`
	TickSizeFloat  float64
	StrikeFloat    float64
	LotSizeInt     int
}

// InstrumentManager handles looking up tokens for symbols.
//
// Concurrency note (EX-9): LoadInstruments performs the (large, slow) download
// and parse WITHOUT holding im.mu; the lock is only taken to swap the built
// index in. Callers (AngelBroker.Connect) must likewise never hold the broker
// mutex across LoadInstruments.
type InstrumentManager struct {
	mu          sync.RWMutex
	instruments map[string]Instrument // Map "SYMBOL" -> Instrument (e.g., "RELIANCE", "NIFTY")
	lastUpdated time.Time

	httpClient *http.Client
	cacheDir   string // disk cache for the instrument master, keyed by date
}

// Instrument master URL — remediation plan Appendix A, endpoint A-11.
// var (not const) so tests can point it at a local httptest server.
var instrumentURL = "https://margincalculator.angelone.in/OpenAPI_File/files/OpenAPIScripMaster.json"

// istLocation is the market timezone; used to key the daily instrument cache.
var istLocation = mustLoadIST()

func mustLoadIST() *time.Location {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		// Fixed fallback: IST is UTC+5:30 with no DST.
		return time.FixedZone("IST", 5*3600+1800)
	}
	return loc
}

// NewInstrumentManager creates and initializes the manager
func NewInstrumentManager() *InstrumentManager {
	return &InstrumentManager{
		instruments: make(map[string]Instrument),
		// Instrument master is ~100MB; allow a generous but bounded timeout
		// instead of the previous bare http.Get (which could hang forever
		// while the broker lock was held — audit EX-9).
		httpClient: &http.Client{Timeout: 180 * time.Second},
		cacheDir:   filepath.Join("data", "instruments"),
	}
}

// SetCacheDir overrides the on-disk cache directory (used by tests).
func (im *InstrumentManager) SetCacheDir(dir string) {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.cacheDir = dir
}

// cachePathForToday returns the disk-cache file path keyed by IST date.
func (im *InstrumentManager) cachePathForToday() string {
	im.mu.RLock()
	dir := im.cacheDir
	im.mu.RUnlock()
	day := time.Now().In(istLocation).Format("2006-01-02")
	return filepath.Join(dir, "scripmaster_"+day+".json")
}

// LoadInstruments loads the instrument master: from today's disk cache if
// present, otherwise via download (then writes the cache). The download and
// JSON parse happen without any lock held.
func (im *InstrumentManager) LoadInstruments() error {
	cachePath := im.cachePathForToday()

	raw, err := os.ReadFile(cachePath)
	if err == nil && len(raw) > 0 {
		fmt.Printf("📂 Loading instrument master from cache: %s\n", cachePath)
		if err := im.buildIndex(raw); err == nil {
			return nil
		}
		// Corrupt cache — fall through to a fresh download.
		fmt.Printf("⚠️ Instrument cache unreadable, re-downloading\n")
	}

	fmt.Println("📥 Downloading Instrument Master from Angel One...")
	raw, err = im.download()
	if err != nil {
		return err
	}

	if err := im.buildIndex(raw); err != nil {
		return err
	}

	// Best-effort cache write; failure to cache is not fatal.
	if dir := filepath.Dir(cachePath); dir != "" {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr == nil {
			if wErr := os.WriteFile(cachePath, raw, 0o644); wErr != nil {
				fmt.Printf("⚠️ Failed to cache instrument master: %v\n", wErr)
			}
		}
	}
	return nil
}

// download fetches the raw instrument master JSON with a bounded timeout.
func (im *InstrumentManager) download() ([]byte, error) {
	resp, err := im.httpClient.Get(instrumentURL)
	if err != nil {
		return nil, fmt.Errorf("failed to download instruments: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPStatusError{StatusCode: resp.StatusCode, Path: instrumentURL, Body: resp.Status}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read body: %w", err)
	}
	return body, nil
}

// buildIndex parses raw JSON and swaps in the new symbol index. Parsing is
// done outside the lock; only the final map swap is locked.
func (im *InstrumentManager) buildIndex(raw []byte) error {
	var instruments []Instrument
	if err := json.Unmarshal(raw, &instruments); err != nil {
		return fmt.Errorf("failed to decode instrument JSON: %w", err)
	}
	if len(instruments) == 0 {
		return fmt.Errorf("instrument master is empty")
	}

	index := make(map[string]Instrument, len(instruments))
	count := 0
	for _, inst := range instruments {
		// Populate numeric fields once here (previously declared but never
		// parsed, forcing every caller — e.g. discovery.go — to re-parse the
		// raw strings itself on every lookup).
		if f, err := strconv.ParseFloat(strings.TrimSpace(inst.LotSize), 64); err == nil && f > 0 {
			inst.LotSizeInt = int(f)
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(inst.Strike), 64); err == nil {
			inst.StrikeFloat = f
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(inst.TickSize), 64); err == nil {
			inst.TickSizeFloat = f
		}

		// 1. NSE Equity & Indices
		if inst.ExchSeg == "NSE" {
			if strings.HasSuffix(inst.Symbol, "-EQ") {
				cleanSymbol := strings.TrimSuffix(inst.Symbol, "-EQ")
				index[cleanSymbol+"-EQ"] = inst // Store as "RELIANCE-EQ"
				// Also map bare symbol for convenience if unique
				if _, exists := index[cleanSymbol]; !exists {
					index[cleanSymbol] = inst
				}
			} else {
				// Indices (e.g., "Nifty 50")
				index[inst.Symbol] = inst
			}
			count++
		} else if inst.ExchSeg == "NFO" {
			// 2. NSE Futures & Options (e.g., NIFTY25SEPFUT)
			index[inst.Symbol] = inst
			count++
		} else if inst.ExchSeg == "MCX" {
			// 3. Multi Commodity Exchange (e.g., SILVERMIC29AUGFUT)
			index[inst.Symbol] = inst
			count++
		}
	}

	im.mu.Lock()
	im.instruments = index
	im.lastUpdated = time.Now()
	im.mu.Unlock()

	fmt.Printf("✅ Loaded %d instruments (NSE/NFO/MCX)\n", count)
	return nil
}

// GetToken returns the token for a given symbol
func (im *InstrumentManager) GetToken(symbol string) (string, error) {
	im.mu.RLock()
	defer im.mu.RUnlock()

	if inst, ok := im.instruments[symbol]; ok {
		return inst.Token, nil
	}

	return "", fmt.Errorf("symbol not found: %s", symbol)
}

// GetInstrument returns the full instrument details
func (im *InstrumentManager) GetInstrument(symbol string) (Instrument, error) {
	im.mu.RLock()
	defer im.mu.RUnlock()

	if inst, ok := im.instruments[symbol]; ok {
		return inst, nil
	}

	return Instrument{}, fmt.Errorf("symbol not found: %s", symbol)
}

// GetLotSize returns the contract lot size for a symbol from the instrument
// master. Fails (never guesses) when the symbol is unknown or the master's
// lot size is missing/invalid — callers must not fall back to hardcoded lots.
func (im *InstrumentManager) GetLotSize(symbol string) (int, error) {
	inst, err := im.GetInstrument(symbol)
	if err != nil {
		return 0, err
	}
	if inst.LotSizeInt > 0 {
		return inst.LotSizeInt, nil
	}
	ls := strings.TrimSpace(inst.LotSize)
	if ls == "" {
		return 0, fmt.Errorf("no lot size in instrument master for %s", symbol)
	}
	// Master sometimes carries decimals like "75.0"
	f, err := strconv.ParseFloat(ls, 64)
	if err != nil || f <= 0 || f != float64(int(f)) {
		return 0, fmt.Errorf("invalid lot size %q in instrument master for %s", inst.LotSize, symbol)
	}
	return int(f), nil
}

// GetExpiries returns the sorted, de-duplicated list of derivative expiry
// dates (IST midnight) for an underlying (e.g. "NIFTY", "BANKNIFTY") from the
// NFO segment of the instrument master. Fails if none found.
func (im *InstrumentManager) GetExpiries(underlying string) ([]time.Time, error) {
	underlying = strings.ToUpper(strings.TrimSpace(underlying))
	if underlying == "" {
		return nil, fmt.Errorf("empty underlying")
	}

	im.mu.RLock()
	seen := make(map[time.Time]struct{})
	for _, inst := range im.instruments {
		if inst.ExchSeg != "NFO" || !strings.EqualFold(inst.Name, underlying) {
			continue
		}
		t, err := parseAngelExpiry(inst.Expiry)
		if err != nil {
			continue // skip unparseable rows, do not fail the whole query
		}
		seen[t] = struct{}{}
	}
	im.mu.RUnlock()

	if len(seen) == 0 {
		return nil, fmt.Errorf("no expiries found for underlying %s in instrument master", underlying)
	}

	out := make([]time.Time, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out, nil
}

// parseAngelExpiry parses Angel's expiry format, e.g. "30JAN2025" or
// "30JAN25", into an IST-midnight time.
func parseAngelExpiry(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty expiry")
	}
	// Go month parsing wants "Jan"; Angel sends "JAN".
	norm := strings.ToUpper(s)
	if len(norm) >= 5 {
		mon := norm[2:5]
		norm = norm[:2] + mon[:1] + strings.ToLower(mon[1:]) + norm[5:]
	}
	for _, layout := range []string{"02Jan2006", "02Jan06"} {
		if t, err := time.ParseInLocation(layout, norm, istLocation); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable expiry %q", s)
}

// GetSymbol returns the symbol for a given token (useful for reverse lookup from WS)
func (im *InstrumentManager) GetSymbol(token string) (string, error) {
	im.mu.RLock()
	defer im.mu.RUnlock()

	for sym, inst := range im.instruments {
		if inst.Token == token {
			return sym, nil
		}
	}

	return "", fmt.Errorf("token not found: %s", token)
}

// Search finds instruments matching the query string (case-insensitive)
func (im *InstrumentManager) Search(query string) []Instrument {
	im.mu.RLock()
	defer im.mu.RUnlock()

	var matches []Instrument
	query = strings.ToUpper(query)

	for _, inst := range im.instruments {
		if strings.Contains(inst.Symbol, query) || strings.Contains(inst.Name, query) {
			matches = append(matches, inst)
			if len(matches) >= 50 { // Limit results
				break
			}
		}
	}
	return matches
}
