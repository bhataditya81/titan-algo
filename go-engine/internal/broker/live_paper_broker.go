package broker

import (
	"fmt"
)

// LivePaperBroker combines a live data source with a paper trading execution engine.
// It allows users to trade virtual money against real market moves.
type LivePaperBroker struct {
	liveBroker  TradeService // For Data (Angel One)
	paperBroker *MockBroker  // For Execution (Virtual Money)
}

// NewLivePaperBroker creates a hybrid broker: real market data, simulated
// execution via MockBroker's DefaultPaperFillConfig() (realistic fills).
func NewLivePaperBroker(live TradeService, initialBalance float64) *LivePaperBroker {
	return &LivePaperBroker{
		liveBroker:  live,
		paperBroker: NewMockBroker(initialBalance),
	}
}

// NewLivePaperBrokerWithConfig is NewLivePaperBroker with an explicit
// PaperFillConfig for the simulated execution side (e.g. LegacyPaperFillConfig()
// or a seeded config for deterministic tests).
func NewLivePaperBrokerWithConfig(live TradeService, initialBalance float64, cfg PaperFillConfig) *LivePaperBroker {
	return &LivePaperBroker{
		liveBroker:  live,
		paperBroker: NewMockBrokerWithConfig(initialBalance, cfg),
	}
}

func (b *LivePaperBroker) Connect() error {
	// Connect to live broker for data
	if err := b.liveBroker.Connect(); err != nil {
		return err
	}
	// Connect to paper broker for state management
	return b.paperBroker.Connect()
}

func (b *LivePaperBroker) Subscribe(symbols []string) error {
	// Subscribe to LIVE data
	return b.liveBroker.Subscribe(symbols)
}

func (b *LivePaperBroker) PlaceOrder(order Order) (*FilledOrder, error) {
	// 1. Get REAL LIVE PRICE from the live broker
	livePrice := b.liveBroker.GetCurrentPrice(order.Symbol)
	if livePrice <= 0 {
		return nil, fmt.Errorf("waiting for live data for %s", order.Symbol)
	}

	// 2. Override MockBroker's internal price with Real Price
	b.paperBroker.mu.Lock()
	b.paperBroker.marketPrices[order.Symbol] = livePrice
	b.paperBroker.mu.Unlock()

	// 3. Execute 'Paper Trade' at Real Price
	return b.paperBroker.PlaceOrder(order)
}

func (b *LivePaperBroker) GetBalance() float64 {
	return b.paperBroker.GetBalance()
}

func (b *LivePaperBroker) GetPositions() map[string]*Position {
	return b.paperBroker.GetPositions()
}

func (b *LivePaperBroker) GetCurrentPrice(symbol string) float64 {
	// Helper: Direct pass-through to live data
	return b.liveBroker.GetCurrentPrice(symbol)
}

func (b *LivePaperBroker) GetCurrentVolume(symbol string) float64 {
	// Helper: Direct pass-through to live data
	return b.liveBroker.GetCurrentVolume(symbol)
}

// FetchMarketDataBatch fetches price and volume for multiple symbols
// Delegates to the live broker for actual API calls
func (b *LivePaperBroker) FetchMarketDataBatch(symbols []string) (map[string]float64, map[string]float64) {
	return b.liveBroker.FetchMarketDataBatch(symbols)
}

func (b *LivePaperBroker) Close() error {
	b.liveBroker.Close()
	return b.paperBroker.Close()
}
