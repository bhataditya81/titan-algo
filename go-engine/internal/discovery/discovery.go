package discovery

import (
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"titan-algo/internal/broker"
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
		// 1. Fetch Spot Price for the index (to find ATM)
		spotPrice := sd.broker.GetCurrentPrice(index)
		if spotPrice <= 0 {
			// Try finding a fallback symbol for spot if the index name itself doesn't work
			// e.g., "NIFTY" might need to be "Nifty 50" depending on broker
			if index == "NIFTY" {
				spotPrice = sd.broker.GetCurrentPrice("Nifty 50")
			} else if index == "BANKNIFTY" {
				spotPrice = sd.broker.GetCurrentPrice("Nifty Bank")
			}
		}

		if spotPrice > 0 {
			log.Printf("🎯 Spot Price for %s: ₹%.2f", index, spotPrice)
		} else {
			log.Printf("⚠️  Could not fetch Spot Price for %s (using raw list)", index)
		}

		chains := sd.findOptionChains(index)

		// Assign lot size to each chain based on index
		lotSize := sd.getLotSize(index)
		for i := range chains {
			chains[i].LotSize = lotSize
		}

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

// getLotSize returns the lot size for an index
func (sd *SymbolDiscovery) getLotSize(index string) int {
	if sd.lotSizes != nil {
		if lotSize, ok := sd.lotSizes[index]; ok {
			return lotSize
		}
	}
	// Default lot sizes
	defaults := map[string]int{
		"NIFTY":      25,
		"BANKNIFTY":  15,
		"SENSEX":     10,
		"FINNIFTY":   40,
		"MIDCPNIFTY": 50,
	}
	if lotSize, ok := defaults[index]; ok {
		return lotSize
	}
	return 25 // Fallback
}

// getActualLotSize extracts the actual lot size from instrument master
func (sd *SymbolDiscovery) getActualLotSize(inst broker.Instrument) int {
	// Try to parse LotSizeInt first (if already parsed)
	if inst.LotSizeInt > 0 {
		return inst.LotSizeInt
	}

	// Parse from LotSize string
	var lotSize int
	fmt.Sscanf(inst.LotSize, "%d", &lotSize)
	if lotSize > 0 {
		return lotSize
	}

	// Fallback: Use index-based default from parent index
	// Extract index from symbol (e.g., "NIFTY27JAN..." -> "NIFTY")
	for index := range sd.lotSizes {
		if strings.HasPrefix(inst.Symbol, index) {
			return sd.getLotSize(index)
		}
	}

	return 25 // Ultimate fallback
}

// findOptionChains finds option chains for an index (filtered to current week expiry)
func (sd *SymbolDiscovery) findOptionChains(index string) []ChainInfo {
	var chains []ChainInfo

	// Get current date for expiry filtering
	now := time.Now()
	currentDay := now.Format("02")
	currentMonth := strings.ToUpper(now.Format("Jan"))
	currentYear := now.Format("06")

	// Calculate weekly expiry format (DDMMMYY)
	// Find next Thursday for NIFTY/FINNIFTY or Wednesday for BANKNIFTY
	weeklyExpiry := sd.getWeeklyExpiry(index, now)

	log.Printf("🗓️  %s: Looking for weekly expiry %s", index, weeklyExpiry)

	// Search for options matching the index
	searchQuery := index
	matches := sd.instruments.Search(searchQuery)

	for _, inst := range matches {
		// Filter: Only NFO options (CE/PE)
		if inst.ExchSeg != "NFO" {
			continue
		}
		if !strings.HasSuffix(inst.Symbol, "CE") && !strings.HasSuffix(inst.Symbol, "PE") {
			continue
		}

		// Parse the symbol to extract components
		chain := parseOptionSymbol(inst.Symbol, index)
		if chain == nil {
			continue
		}

		// STRICT FILTER: Only this week's expiry or next week's
		if !sd.isCurrentOrNextWeekExpiry(chain.Expiry, currentDay, currentMonth, currentYear) {
			continue
		}

		chain.Token = inst.Token

		// **CRITICAL FIX**: Get actual lot size from instrument master!
		chain.LotSize = sd.getActualLotSize(inst)

		chains = append(chains, *chain)
	}

	return chains
}

// getWeeklyExpiry returns the next weekly expiry date in DDMMMYY format
func (sd *SymbolDiscovery) getWeeklyExpiry(index string, now time.Time) string {
	// NIFTY/FINNIFTY expires Thursday, BANKNIFTY expires Wednesday
	targetWeekday := time.Thursday
	if index == "BANKNIFTY" {
		targetWeekday = time.Wednesday
	}

	// Find days until next expiry
	daysUntil := (int(targetWeekday) - int(now.Weekday()) + 7) % 7
	if daysUntil == 0 && now.Hour() >= 15 { // Past 3:30 PM on expiry day
		daysUntil = 7
	}

	expiryDate := now.AddDate(0, 0, daysUntil)
	return strings.ToUpper(expiryDate.Format("02Jan06"))
}

// isCurrentOrNextWeekExpiry checks if expiry is within 2 weeks
func (sd *SymbolDiscovery) isCurrentOrNextWeekExpiry(expiry, currentDay, currentMonth, currentYear string) bool {
	if len(expiry) < 7 {
		return false
	}

	expiryDay := expiry[:2]
	expiryMonth := expiry[2:5]
	expiryYear := expiry[5:7]

	// Must be current year
	if expiryYear != currentYear && expiryYear != fmt.Sprintf("%02d", (time.Now().Year()+1)%100) {
		return false
	}

	// Must be current or next month
	months := []string{"JAN", "FEB", "MAR", "APR", "MAY", "JUN", "JUL", "AUG", "SEP", "OCT", "NOV", "DEC"}
	currentMonthIdx := -1
	expiryMonthIdx := -1
	for i, m := range months {
		if m == currentMonth {
			currentMonthIdx = i
		}
		if m == expiryMonth {
			expiryMonthIdx = i
		}
	}

	// Same year logic
	if expiryYear == currentYear {
		if expiryMonthIdx < currentMonthIdx {
			return false // Past month
		}
		if expiryMonthIdx > currentMonthIdx+1 {
			return false // Too far in future
		}
		// If same month, check day
		if expiryMonthIdx == currentMonthIdx && expiryDay < currentDay {
			return false // Past day
		}
	}

	return true
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
