package strategy

import (
	"sync"
)

// PricePoint represents a single price/volume data point
type PricePoint struct {
	Price  float64
	Volume float64
}

// PriceHistory maintains rolling price/volume history for symbols
type PriceHistory struct {
	mu      sync.RWMutex
	data    map[string][]PricePoint
	maxSize int
}

// NewPriceHistory creates a new price history tracker
// maxSize: maximum number of data points to keep per symbol
func NewPriceHistory(maxSize int) *PriceHistory {
	return &PriceHistory{
		data:    make(map[string][]PricePoint),
		maxSize: maxSize,
	}
}

// Add adds a new price/volume data point for a symbol
func (ph *PriceHistory) Add(symbol string, price, volume float64) {
	ph.mu.Lock()
	defer ph.mu.Unlock()

	if _, exists := ph.data[symbol]; !exists {
		ph.data[symbol] = make([]PricePoint, 0, ph.maxSize)
	}

	ph.data[symbol] = append(ph.data[symbol], PricePoint{
		Price:  price,
		Volume: volume,
	})

	// Trim to max size if needed
	if len(ph.data[symbol]) > ph.maxSize {
		ph.data[symbol] = ph.data[symbol][len(ph.data[symbol])-ph.maxSize:]
	}
}

// GetPrices returns the price history for a symbol
func (ph *PriceHistory) GetPrices(symbol string) []float64 {
	ph.mu.RLock()
	defer ph.mu.RUnlock()

	points, exists := ph.data[symbol]
	if !exists {
		return nil
	}

	prices := make([]float64, len(points))
	for i, p := range points {
		prices[i] = p.Price
	}
	return prices
}

// GetVolumes returns the volume history for a symbol
func (ph *PriceHistory) GetVolumes(symbol string) []float64 {
	ph.mu.RLock()
	defer ph.mu.RUnlock()

	points, exists := ph.data[symbol]
	if !exists {
		return nil
	}

	volumes := make([]float64, len(points))
	for i, p := range points {
		volumes[i] = p.Volume
	}
	return volumes
}

// GetDataPoints returns the number of data points for a symbol
func (ph *PriceHistory) GetDataPoints(symbol string) int {
	ph.mu.RLock()
	defer ph.mu.RUnlock()

	points, exists := ph.data[symbol]
	if !exists {
		return 0
	}
	return len(points)
}

// Clear removes all data for a symbol
func (ph *PriceHistory) Clear(symbol string) {
	ph.mu.Lock()
	defer ph.mu.Unlock()
	delete(ph.data, symbol)
}

// ClearAll removes all data
func (ph *PriceHistory) ClearAll() {
	ph.mu.Lock()
	defer ph.mu.Unlock()
	ph.data = make(map[string][]PricePoint)
}
