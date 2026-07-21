package broker

import (
	"fmt"
	"time"
)

// LivePaperBroker combines a live data source with a paper trading execution engine.
// It allows users to trade virtual money against real market moves.
//
// liveExt caches a one-time type assertion of liveBroker to
// ExtendedTradeService (audit R3): without this, LivePaperBroker itself
// didn't implement ExtendedTradeService at all, so the software stop-loss
// loop's staleness check (GetCurrentPriceWithAge) and broker health check
// (Healthy/HealthError) silently always reported "fresh"/"healthy" — during
// exactly the paper-trading run meant to validate safety before going live.
// Execution-side methods (PlaceStopLossOrder/CancelOrder) are NOT delegated
// to the live broker — paper trading has no real resting broker-side order
// to place, so they fail closed with a clear error instead of silently
// placing something against the real account.
type LivePaperBroker struct {
	liveBroker  TradeService // For Data (Angel One)
	paperBroker *MockBroker  // For Execution (Virtual Money)
	liveExt     ExtendedTradeService
}

// NewLivePaperBroker creates a hybrid broker: real market data, simulated
// execution via MockBroker's DefaultPaperFillConfig() (realistic fills).
func NewLivePaperBroker(live TradeService, initialBalance float64) *LivePaperBroker {
	liveExt, _ := live.(ExtendedTradeService)
	return &LivePaperBroker{
		liveBroker:  live,
		paperBroker: NewMockBroker(initialBalance),
		liveExt:     liveExt,
	}
}

// NewLivePaperBrokerWithConfig is NewLivePaperBroker with an explicit
// PaperFillConfig for the simulated execution side (e.g. LegacyPaperFillConfig()
// or a seeded config for deterministic tests).
func NewLivePaperBrokerWithConfig(live TradeService, initialBalance float64, cfg PaperFillConfig) *LivePaperBroker {
	liveExt, _ := live.(ExtendedTradeService)
	return &LivePaperBroker{
		liveBroker:  live,
		paperBroker: NewMockBrokerWithConfig(initialBalance, cfg),
		liveExt:     liveExt,
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

// PlaceStopLossOrder fails closed: paper trading executes virtually, so
// there is no real broker-side resting order to place. The caller
// (runner.placeBrokerStop) already degrades to the software stop-loss loop
// on this error, exactly as it did before this broker implemented
// ExtendedTradeService at all.
func (b *LivePaperBroker) PlaceStopLossOrder(symbol string, qty int, triggerPrice float64, side OrderSide) (string, error) {
	return "", fmt.Errorf("LivePaperBroker: broker-side stop-loss orders are not applicable to paper trading (software SL loop applies)")
}

// CancelOrder is a no-op: PlaceStopLossOrder above never returns a real
// order ID, so there is never anything to cancel.
func (b *LivePaperBroker) CancelOrder(orderID string) error { return nil }

// GetCurrentPriceWithAge delegates to the live data broker so the software
// stop-loss loop's staleness check reflects the REAL feed's age, not an
// always-fresh default. Errors if the underlying live broker doesn't
// support staleness tracking either (never guesses "fresh").
func (b *LivePaperBroker) GetCurrentPriceWithAge(symbol string) (float64, time.Duration, error) {
	if b.liveExt == nil {
		return 0, 0, fmt.Errorf("LivePaperBroker: underlying live data broker does not support price-age tracking")
	}
	return b.liveExt.GetCurrentPriceWithAge(symbol)
}

// Healthy/HealthError delegate to the live data broker: paper execution
// itself can't be "unhealthy" (it's local simulation), but the REAL data
// feed backing it can be, and that's what actually matters for deciding
// whether to trust prices during the paper-trading validation run.
func (b *LivePaperBroker) Healthy() bool {
	if b.liveExt == nil {
		return true
	}
	return b.liveExt.Healthy()
}

func (b *LivePaperBroker) HealthError() error {
	if b.liveExt == nil {
		return nil
	}
	return b.liveExt.HealthError()
}

// RefreshBalance reports the PAPER (virtual) balance — refreshing from the
// real broker's RMS endpoint would be meaningless here, since execution is
// simulated against virtual money, not the live account's real funds.
func (b *LivePaperBroker) RefreshBalance() (float64, error) {
	return b.paperBroker.GetBalance(), nil
}

// GetRequiredMargin delegates to the live broker's real margin-calculator
// endpoint. This is informational/sizing data with no money movement, so
// using real broker-quoted margin makes paper-mode SELL-derivative entries
// (short_straddle/iron_fly) realistically sized instead of fail-closed for
// lack of a margin source (README's previously-documented paper-mode gap).
func (b *LivePaperBroker) GetRequiredMargin(orders []MarginOrderInput) (float64, error) {
	if b.liveExt == nil {
		return 0, fmt.Errorf("LivePaperBroker: underlying live data broker does not support margin calculation")
	}
	return b.liveExt.GetRequiredMargin(orders)
}

// SubscribeLive/UnsubscribeLive delegate to the live data broker's
// WebSocket feed — this is exactly the same "data" concern Subscribe/
// FetchMarketDataBatch already delegate for.
func (b *LivePaperBroker) SubscribeLive(symbols []string) error {
	if b.liveExt == nil {
		return fmt.Errorf("LivePaperBroker: underlying live data broker does not support a live feed")
	}
	return b.liveExt.SubscribeLive(symbols)
}

func (b *LivePaperBroker) UnsubscribeLive(symbols []string) error {
	if b.liveExt == nil {
		return nil
	}
	return b.liveExt.UnsubscribeLive(symbols)
}

var _ ExtendedTradeService = (*LivePaperBroker)(nil)
