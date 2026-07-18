package broker

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// InstrumentManager handles looking up tokens for symbols
type InstrumentManager struct {
	mu          sync.RWMutex
	instruments map[string]Instrument // Map "SYMBOL" -> Instrument (e.g., "RELIANCE", "NIFTY")
	lastUpdated time.Time
}

const instrumentURL = "https://margincalculator.angelbroking.com/OpenAPI_File/files/OpenAPIScripMaster.json"

// NewInstrumentManager creates and initializes the manager
func NewInstrumentManager() *InstrumentManager {
	return &InstrumentManager{
		instruments: make(map[string]Instrument),
	}
}

// LoadInstruments downloads and parses the instrument master file
func (im *InstrumentManager) LoadInstruments() error {
	fmt.Println("📥 Downloading Instrument Master from Angel One...")

	resp, err := http.Get(instrumentURL)
	if err != nil {
		return fmt.Errorf("failed to download instruments: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read body: %w", err)
	}

	var instruments []Instrument
	if err := json.Unmarshal(body, &instruments); err != nil {
		return fmt.Errorf("failed to decode JSON: %w", err)
	}

	im.mu.Lock()
	defer im.mu.Unlock()

	count := 0
	for _, inst := range instruments {
		// 1. NSE Equity & Indices
		if inst.ExchSeg == "NSE" {
			if strings.HasSuffix(inst.Symbol, "-EQ") {
				cleanSymbol := strings.TrimSuffix(inst.Symbol, "-EQ")
				im.instruments[cleanSymbol+"-EQ"] = inst // Store as "RELIANCE-EQ"
				// Also map bare symbol for convenience if unique
				if _, exists := im.instruments[cleanSymbol]; !exists {
					im.instruments[cleanSymbol] = inst
				}
			} else {
				// Indices (e.g., "Nifty 50")
				im.instruments[inst.Symbol] = inst
			}
			count++
		} else if inst.ExchSeg == "NFO" {
			// 2. NSE Futures & Options (e.g., NIFTY25SEPFUT)
			// Store by specific symbol name
			im.instruments[inst.Symbol] = inst

			// For generic "NIFTY", we usually want the near-month future or spot.
			// This logic is complex; for now, we just index by the raw symbol name provided in config.
			// E.g. Config: "NIFTY28AUGFUT"
			count++
		} else if inst.ExchSeg == "MCX" {
			// 3. Multi Commodity Exchange (e.g., SILVERMIC29AUGFUT)
			im.instruments[inst.Symbol] = inst

			// Handle "SILVER", "GOLD" generic mapping if possible?
			// Usually commodities have specific expiries like "SILVERMIC29AUGFUT"
			// For simplified matching, we might need a more sophisticated lookup.
			// For now, user must provide EXACT symbol from Angel One master.
			// Exception: If symbol is just "SILVER", find the nearest future? -> Too risky for MVP.
			// Let's stick to exact symbol mapping.
			count++
		}
	}

	im.lastUpdated = time.Now()
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
