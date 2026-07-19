package discovery

import (
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"titan-algo/internal/broker"
	"titan-algo/internal/strategy"
)

// ChainInfo holds information about an option chain
type ChainInfo struct {
	Symbol     string  // Full symbol e.g., "NIFTY20JAN2625600PE"
	Index      string  // Base index e.g., "NIFTY"
	Expiry     string  // Expiry date e.g., "20JAN26"
	Strike     float64 // Strike price
	OptionType string  // "CE" or "PE"
	Volume     int64   // Trading volume
	OI         int64   // Open Interest
	LTP        float64 // Last Traded Price
	Change     float64 // Price change percentage
	Token      string  // Instrument token
	LotSize    int     // Lot size for this symbol
	TradeCost  float64 // LTP × LotSize = cost to trade 1 lot
}

// SymbolDiscovery handles dynamic symbol detection
type SymbolDiscovery struct {
	broker        broker.TradeService
	instruments   *broker.InstrumentManager
	indices       []string
	lotSizes      map[string]int // Lot size per index
	filterBalance float64        // Session balance for affordability filter
	cacheDir      string
}

// NewSymbolDiscovery creates a new discovery instance
func NewSymbolDiscovery(brokerService broker.TradeService, indices []string, lotSizes map[string]int, sessionBalance float64) *SymbolDiscovery {
	return &SymbolDiscovery{
		broker:        brokerService,
		instruments:   broker.NewInstrumentManager(),
		indices:       indices,
		lotSizes:      lotSizes,
		filterBalance: sessionBalance,
		cacheDir:      "",
	}
}

// ScanTopChains returns top N chains by volume for configured indices
func (sd *SymbolDiscovery) ScanTopChains(topN int) ([]ChainInfo, error) {
	log.Println("🔍 Scanning F&O option chains (weekly expiry only)...")

	// Load instrument master
	if err := sd.instruments.LoadInstruments(); err != nil {
		return nil, fmt.Errorf("failed to load instruments: %w", err)
	}

	// Find option chains for configured indices (filtered to current/next week)
	var allChains []ChainInfo

	for _, index := range sd.indices {
		// 1. Fetch Spot Price for the index (to find ATM). The bare index
		// name (e.g. "NIFTY") is not itself the quotable instrument in
		// Angel's master -- resolve the real one (e.g. "Nifty 50") via the
		// instrument master rather than a hardcoded per-index guess (this
		// used to hardcode only NIFTY/BANKNIFTY; FindIndexSymbol works for
		// any index the master actually has).
		spotPrice := sd.broker.GetCurrentPrice(index)
		if spotPrice <= 0 && sd.instruments != nil {
			if realSym, err := sd.instruments.FindIndexSymbol(index); err == nil {
				spotPrice = sd.broker.GetCurrentPrice(realSym)
			}
		}

		if spotPrice > 0 {
			log.Printf("🎯 Spot Price for %s: ₹%.2f", index, spotPrice)
		} else {
			log.Printf("⚠️  Could not fetch Spot Price for %s (using raw list)", index)
		}

		// findOptionChains already resolves each chain's real lot size from
		// the instrument master (or an explicit per-underlying config
		// override), skipping any chain it can't resolve — do not overwrite
		// that with a cruder per-index guess here (that used to clobber the
		// accurate per-instrument value with a stale hardcoded number).
		chains := sd.findOptionChains(index)

		// 2. Sort by distance from ATM (if spot price available)
		if spotPrice > 0 {
			sort.Slice(chains, func(i, j int) bool {
				distI := math.Abs(chains[i].Strike - spotPrice)
				distJ := math.Abs(chains[j].Strike - spotPrice)
				return distI < distJ
			})
			// Keep only top 50 closest to ATM per index to ensure high quality candidates
			limitPerIndex := 50
			if len(chains) > limitPerIndex {
				chains = chains[:limitPerIndex]
			}
		}

		allChains = append(allChains, chains...)
	}

	if len(allChains) == 0 {
		return nil, fmt.Errorf("no option chains found for indices: %v", sd.indices)
	}

	log.Printf("📊 Found %d potential option chains (ATM prioritized)", len(allChains))

	// LIMIT: Only fetch market data for top 50 total (to avoid rate limits)
	maxFetch := 50
	if len(allChains) > maxFetch {
		log.Printf("⚡ Limiting market data fetch to %d chains (rate limit protection)", maxFetch)
		allChains = allChains[:maxFetch]
	}

	// Fetch volume data with rate limiting
	sd.enrichWithMarketDataRateLimited(allChains)

	// Calculate TradeCost (LTP × LotSize) for each chain
	for i := range allChains {
		allChains[i].TradeCost = allChains[i].LTP * float64(allChains[i].LotSize)
	}

	// Filter by affordability if balance is set
	if sd.filterBalance > 0 {
		var affordableChains []ChainInfo
		for _, c := range allChains {
			if c.TradeCost <= sd.filterBalance && c.TradeCost > 0 {
				affordableChains = append(affordableChains, c)
			}
		}
		if len(affordableChains) > 0 {
			log.Printf("💰 Filtered to %d affordable chains (Balance: ₹%.2f)", len(affordableChains), sd.filterBalance)
			allChains = affordableChains
		} else {
			log.Printf("⚠️ No affordable chains found for balance ₹%.2f - showing all chains", sd.filterBalance)
		}
	}

	// Filter out chains with zero volume (no market data available)
	var activeChains []ChainInfo
	for _, c := range allChains {
		if c.Volume > 0 {
			activeChains = append(activeChains, c)
		}
	}
	if len(activeChains) > 0 {
		log.Printf("📊 Filtered to %d chains with active volume (removed %d zero-volume chains)",
			len(activeChains), len(allChains)-len(activeChains))
		allChains = activeChains
	} else {
		log.Printf("⚠️ No chains with volume data found - showing ATM chains (best guess)")
	}

	// Sort by volume (descending)
	sort.Slice(allChains, func(i, j int) bool {
		return allChains[i].Volume > allChains[j].Volume
	})

	// Return top N
	if topN > len(allChains) {
		topN = len(allChains)
	}

	result := allChains[:topN]
	log.Printf("🔥 Top %d chains by volume selected", topN)

	return result, nil
}

// resolveLotSize returns the real lot size for symbol from the instrument
// master, falling back only to an explicit operator-configured per-index
// override (sd.lotSizes, set from config.yaml). It NEVER guesses: if neither
// source resolves a lot size, it returns an error and the caller must skip
// the instrument rather than trade it with a made-up quantity multiplier
// (a wrong lot size silently corrupts every downstream margin/P&L figure).
func (sd *SymbolDiscovery) resolveLotSize(index, symbol string) (int, error) {
	if sd.instruments != nil {
		if ls, err := sd.instruments.GetLotSize(symbol); err == nil && ls > 0 {
			return ls, nil
		}
	}
	if sd.lotSizes != nil {
		if lotSize, ok := sd.lotSizes[index]; ok && lotSize > 0 {
			return lotSize, nil
		}
	}
	return 0, fmt.Errorf("no lot size available for %s (%s) in instrument master or config", symbol, index)
}

// targetExpiries returns the nearest N (default 2: current + next) real,
// still-open expiry dates for index from the instrument master's Expiry
// field -- replacing the old weekday-arithmetic guess (NIFTY=Thursday,
// BANKNIFTY=Wednesday), which went stale the moment NSE's own expiry-day
// rules changed and could silently select an already-passed or wrong-week
// contract. Errors (rather than guesses) if the instrument master has no
// resolvable future expiry for this underlying.
func (sd *SymbolDiscovery) targetExpiries(index string, asOf time.Time) ([]time.Time, error) {
	all, err := sd.instruments.GetExpiries(index)
	if err != nil {
		return nil, fmt.Errorf("resolve expiries for %s: %w", index, err)
	}
	today := time.Date(asOf.Year(), asOf.Month(), asOf.Day(), 0, 0, 0, 0, asOf.Location())
	var future []time.Time
	for _, t := range all {
		if !t.Before(today) {
			future = append(future, t)
		}
	}
	if len(future) == 0 {
		return nil, fmt.Errorf("no non-expired expiry found for %s in instrument master", index)
	}
	const maxExpiries = 2
	if len(future) > maxExpiries {
		future = future[:maxExpiries]
	}
	return future, nil
}

// findOptionChains finds option chains for an index, filtered to the
// nearest real (instrument-master-sourced) expiry or two.
func (sd *SymbolDiscovery) findOptionChains(index string) []ChainInfo {
	var chains []ChainInfo

	now := time.Now().In(strategy.IST)

	targets, err := sd.targetExpiries(index, now)
	if err != nil {
		log.Printf("⚠️  %s: %v — skipping this index entirely rather than guessing an expiry", index, err)
		return nil
	}
	targetSet := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		targetSet[t.Format("2006-01-02")] = struct{}{}
		log.Printf("🗓️  %s: including expiry %s (from instrument master)", index, t.Format("02-Jan-2006"))
	}

	// Search for options matching the index
	matches := sd.instruments.Search(index)

	for _, inst := range matches {
		// Filter: Only NFO options (CE/PE)
		if inst.ExchSeg != "NFO" {
			continue
		}
		if !strings.HasSuffix(inst.Symbol, "CE") && !strings.HasSuffix(inst.Symbol, "PE") {
			continue
		}

		instExpiry, err := broker.ParseExpiry(inst.Expiry)
		if err != nil {
			continue // unparseable row, skip rather than guess its date
		}
		if _, ok := targetSet[instExpiry.Format("2006-01-02")]; !ok {
			continue // not one of the target (current/next real) expiries
		}

		// Parse the symbol for Strike/OptionType only -- Expiry comes from
		// the authoritative instrument-master field above, not re-derived
		// from the symbol string.
		chain := parseOptionSymbol(inst.Symbol, index)
		if chain == nil {
			continue
		}
		chain.Expiry = strings.ToUpper(inst.Expiry)
		chain.Token = inst.Token

		lotSize, err := sd.resolveLotSize(index, inst.Symbol)
		if err != nil {
			log.Printf("⚠️  %s: %v — excluding this chain from discovery (never trading on a guessed lot size)", inst.Symbol, err)
			continue
		}
		chain.LotSize = lotSize

		chains = append(chains, *chain)
	}

	return chains
}

// parseOptionSymbol extracts components from an option symbol
// Format: NIFTY20JAN2625600PE
func parseOptionSymbol(symbol, index string) *ChainInfo {
	if !strings.HasPrefix(symbol, index) {
		return nil
	}

	remaining := symbol[len(index):]
	if len(remaining) < 10 { // Minimum: DDMMMYYSTRIKECP
		return nil
	}

	// Extract option type (last 2 chars)
	optionType := remaining[len(remaining)-2:]
	if optionType != "CE" && optionType != "PE" {
		return nil
	}

	// Extract expiry (first 7 chars after index: DDMMMYY)
	if len(remaining) < 9 {
		return nil
	}
	expiry := remaining[:7]

	// Extract strike (between expiry and option type)
	strikeStr := remaining[7 : len(remaining)-2]
	var strike float64
	fmt.Sscanf(strikeStr, "%f", &strike)

	return &ChainInfo{
		Symbol:     symbol,
		Index:      index,
		Expiry:     expiry,
		Strike:     strike,
		OptionType: optionType,
	}
}

// enrichWithMarketDataRateLimited fetches volume/LTP using batch API (single call!)
func (sd *SymbolDiscovery) enrichWithMarketDataRateLimited(chains []ChainInfo) {
	log.Printf("📈 Fetching market data for %d chains (batch mode)...", len(chains))

	// Collect all symbols
	symbols := make([]string, len(chains))
	for i, c := range chains {
		symbols[i] = c.Symbol
	}

	// Use batch API call (single request for all symbols!)
	prices, volumes := sd.broker.FetchMarketDataBatch(symbols)

	// Apply fetched data to chains
	successCount := 0
	for i := range chains {
		sym := chains[i].Symbol
		if ltp, ok := prices[sym]; ok && ltp > 0 {
			chains[i].LTP = ltp
		}
		if vol, ok := volumes[sym]; ok && vol > 0 {
			chains[i].Volume = int64(vol)
		}
		if chains[i].Volume > 0 || chains[i].LTP > 0 {
			successCount++
		}
	}

	log.Printf("✅ Market data fetched for %d/%d chains", successCount, len(chains))
}

// GetDefaultIndices returns the default F&O indices to scan
func GetDefaultIndices() []string {
	return []string{"NIFTY", "BANKNIFTY"}
}
