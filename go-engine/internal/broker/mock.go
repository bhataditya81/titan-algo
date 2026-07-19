package broker

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// PaperFillConfig controls the realism of MockBroker's simulated order
// fills (docs/PRODUCTION_GAPS_R2.md gap G-6 / work package R2-4).
//
// This is a paper-trading APPROXIMATION meant to stop paper results from
// systematically flattering every strategy versus live trading — it is NOT
// real exchange matching or real SPAN margin. Once R2-1's broker margin API
// is wired end-to-end, live/paper margin decisions should come from there;
// ShortMarginPct here is only a stand-in until that integration lands.
//
// NewMockBroker(balance) uses DefaultPaperFillConfig(). Use
// NewMockBrokerWithConfig for a custom config, or LegacyPaperFillConfig()
// for the pre-R2-4 deterministic/noise-free behavior (useful for tests that
// only care about control flow, not fill realism).
type PaperFillConfig struct {
	// Spread (task 1): fills happen at LTP +/- a half-spread. Buys fill
	// worse (higher), sells fill worse (lower). Expressed as a percentage
	// of the reference price.
	OptionHalfSpreadPct float64 // default 0.3 — option symbols (suffix "CE"/"PE")
	EquityHalfSpreadPct float64 // default 0.02 — everything else (equity, futures)

	// Size-scaled slippage (task 2): the half-spread is multiplied by
	// sqrt(quantity / TypicalLiquidityQty) whenever quantity exceeds that
	// reference (an order at or below the reference size gets the plain
	// half-spread; larger orders get progressively worse fills). Set to 0
	// to disable size scaling entirely.
	TypicalLiquidityQty float64 // default 75 (one NIFTY lot)

	// Partial fills (task 3): PartialFillRate is the probability in [0,1]
	// that an order fills for less than requested. When it does, the
	// filled fraction is uniform in [PartialFillMinFrac, 1.0). Surfaces
	// through the exact FilledOrder.RequestedQty/Partial() contract WP-1
	// defined for the real broker.
	PartialFillRate    float64 // default 0.07
	PartialFillMinFrac float64 // default 0.4

	// Margin on shorts (task 4 — the "money printer" fix): when
	// MarginOnShorts is true, a SELL order that opens or adds to a short
	// position in an option or future symbol reserves
	// turnover*(1+ShortMarginPct) as locked margin instead of crediting the
	// premium as free cash. When false, the pre-R2-4 behavior is kept
	// (full turnover credited, nothing reserved) — only meant for
	// LegacyPaperFillConfig.
	MarginOnShorts bool    // default true
	ShortMarginPct float64 // default 0.13 (13% of notional, SPAN-ish proxy)

	// Rejections (task 5): probability in [0,1] that an order is rejected
	// outright (no fill, no balance/position change) with a realistic
	// broker-style error, to exercise the engine's multi-leg unwind path.
	RejectionRate float64 // default 0.03

	// Seed drives the broker's own RNG. Zero means "seed from wall-clock
	// time" (nondeterministic — appropriate for a live paper session).
	// Tests that need reproducible partial-fill/rejection behavior must set
	// a nonzero Seed.
	Seed int64
}

// DefaultPaperFillConfig returns the realistic defaults for R2-4.
func DefaultPaperFillConfig() PaperFillConfig {
	return PaperFillConfig{
		OptionHalfSpreadPct: 0.3,
		EquityHalfSpreadPct: 0.02,
		TypicalLiquidityQty: 75,
		PartialFillRate:     0.07,
		PartialFillMinFrac:  0.4,
		MarginOnShorts:      true,
		ShortMarginPct:      0.13,
		RejectionRate:       0.03,
	}
}

// LegacyPaperFillConfig reproduces the pre-R2-4 mock behavior: a small flat
// slippage band, no size scaling, no partial fills, no rejections, and
// full-turnover-credit shorts (the money-printer bug). It exists ONLY for
// tests elsewhere that construct a MockBroker directly and depend on pure,
// noise-free, deterministic fills to test unrelated logic.
func LegacyPaperFillConfig() PaperFillConfig {
	return PaperFillConfig{
		OptionHalfSpreadPct: 0.075, // midpoint of the old 0.05%-0.10% band
		EquityHalfSpreadPct: 0.075,
		TypicalLiquidityQty: 0, // disable size scaling
		PartialFillRate:     0,
		PartialFillMinFrac:  1,
		MarginOnShorts:      false,
		ShortMarginPct:      0,
		RejectionRate:       0,
		Seed:                1,
	}
}

// isOptionSymbol reports whether symbol looks like an option instrument
// (Angel/NSE convention: e.g. "NIFTY25JUL26000CE", "...PE" — a numeric
// strike always immediately precedes the CE/PE suffix). Requiring a digit
// before the suffix avoids misclassifying equity tickers that merely end in
// "CE"/"PE" (e.g. "RELIANCE", "APOLLOTYRE") as options.
func isOptionSymbol(symbol string) bool {
	for _, suffix := range [...]string{"CE", "PE"} {
		if !strings.HasSuffix(symbol, suffix) {
			continue
		}
		if prefix := len(symbol) - len(suffix); prefix > 0 {
			last := symbol[prefix-1]
			if last >= '0' && last <= '9' {
				return true
			}
		}
	}
	return false
}

// isDerivativeSymbol reports whether symbol is an option or a future
// (Angel/NSE convention: futures end in "FUT", e.g. "NIFTY25SEPFUT").
func isDerivativeSymbol(symbol string) bool {
	return isOptionSymbol(symbol) || strings.HasSuffix(symbol, "FUT")
}

// MockBroker simulates a real broker for paper trading
type MockBroker struct {
	mu             sync.RWMutex
	virtualBalance float64
	openPositions  map[string]*Position
	marketPrices   map[string]float64
	marketVolumes  map[string]float64
	marginReserved map[string]float64 // task 4: margin currently locked per symbol for short derivative positions
	orderCounter   int
	brokerageFee   float64
	connected      bool
	cfg            PaperFillConfig
	rng            *rand.Rand
}

// NewMockBroker creates a new mock broker with initial balance, using
// DefaultPaperFillConfig() (realistic paper fills — see PaperFillConfig).
func NewMockBroker(initialBalance float64) *MockBroker {
	return NewMockBrokerWithConfig(initialBalance, DefaultPaperFillConfig())
}

// NewMockBrokerWithConfig creates a mock broker with an explicit fill
// config. Use LegacyPaperFillConfig() for deterministic, noise-free fills,
// or set PaperFillConfig.Seed for a reproducible realistic simulation.
func NewMockBrokerWithConfig(initialBalance float64, cfg PaperFillConfig) *MockBroker {
	seed := cfg.Seed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	return &MockBroker{
		virtualBalance: initialBalance,
		openPositions:  make(map[string]*Position),
		marketPrices:   make(map[string]float64),
		marketVolumes:  make(map[string]float64),
		marginReserved: make(map[string]float64),
		brokerageFee:   20.0, // ₹20 flat fee per order
		connected:      false,
		cfg:            cfg,
		rng:            rand.New(rand.NewSource(seed)),
	}
}

// Connect simulates broker connection
func (m *MockBroker) Connect() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Simulate connection delay
	time.Sleep(100 * time.Millisecond)
	m.connected = true
	fmt.Println("MockBroker: Connected successfully")
	return nil
}

// Subscribe simulates subscribing to market data
func (m *MockBroker) Subscribe(tokens []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.connected {
		return fmt.Errorf("not connected to broker")
	}

	// Initialize mock prices for subscribed symbols
	for _, token := range tokens {
		// Generate random initial price between ₹100-₹500
		m.marketPrices[token] = 100 + m.rng.Float64()*400
	}

	fmt.Printf("MockBroker: Subscribed to %d symbols\n", len(tokens))
	return nil
}

// simulateFillPrice returns the fill price and the per-unit spread/slippage
// applied, given the reference (last-traded) price. Buys fill worse
// (higher), sells fill worse (lower) — tasks 1 and 2.
func (m *MockBroker) simulateFillPrice(symbol string, side OrderSide, quantity int, refPrice float64) (fillPrice, perUnitSlippage float64) {
	halfSpreadPct := m.cfg.EquityHalfSpreadPct
	if isOptionSymbol(symbol) {
		halfSpreadPct = m.cfg.OptionHalfSpreadPct
	}

	scale := 1.0
	if m.cfg.TypicalLiquidityQty > 0 && float64(quantity) > m.cfg.TypicalLiquidityQty {
		scale = math.Sqrt(float64(quantity) / m.cfg.TypicalLiquidityQty)
	}

	perUnitSlippage = refPrice * (halfSpreadPct / 100.0) * scale
	if side == Buy {
		fillPrice = refPrice + perUnitSlippage
	} else {
		fillPrice = refPrice - perUnitSlippage
		if fillPrice < 0 {
			fillPrice = 0
		}
	}
	return fillPrice, perUnitSlippage
}

// simulateFillQuantity rolls whether an order fills partially (task 3) and
// returns the actual filled quantity (always in [1, requested]).
func (m *MockBroker) simulateFillQuantity(requested int) int {
	if requested <= 1 || m.cfg.PartialFillRate <= 0 {
		return requested
	}
	if m.rng.Float64() >= m.cfg.PartialFillRate {
		return requested
	}
	minFrac := m.cfg.PartialFillMinFrac
	if minFrac <= 0 {
		minFrac = 0.1
	}
	if minFrac > 1 {
		minFrac = 1
	}
	frac := minFrac + m.rng.Float64()*(1.0-minFrac)
	qty := int(float64(requested) * frac)
	if qty >= requested {
		qty = requested - 1
	}
	if qty < 1 {
		qty = 1
	}
	return qty
}

// rollRejection decides whether an order is rejected outright (task 5).
func (m *MockBroker) rollRejection() bool {
	return m.cfg.RejectionRate > 0 && m.rng.Float64() < m.cfg.RejectionRate
}

// settleCash computes the balance delta for a fill (task 4: margin on
// shorts). existing is the position BEFORE this fill is applied — callers
// must compute this before calling updatePosition. It also updates
// m.marginReserved for the symbol as a side effect.
func (m *MockBroker) settleCash(symbol string, side OrderSide, fillQty int, fillPrice float64, existing *Position) float64 {
	turnover := fillPrice * float64(fillQty)

	if !m.cfg.MarginOnShorts || !isDerivativeSymbol(symbol) {
		// Legacy/non-derivative accounting: buys cost turnover, sells
		// credit turnover. Equity margin/short-selling rules are out of
		// scope for R2-4 (task 4 only covers option/future SELLs).
		if side == Buy {
			return -(turnover + m.brokerageFee)
		}
		return turnover - m.brokerageFee
	}

	switch side {
	case Sell:
		if existing == nil || existing.Side == Sell {
			// Opening or adding to a short: reserve margin, do NOT credit
			// the premium as free cash. This is the money-printer fix.
			marginNeeded := turnover * (1 + m.cfg.ShortMarginPct)
			m.marginReserved[symbol] += marginNeeded
			return -(marginNeeded + m.brokerageFee)
		}
		// Closing part of an existing long derivative position: an
		// ordinary sale: proceeds credited, nothing was reserved.
		return turnover - m.brokerageFee

	default: // Buy
		if existing != nil && existing.Side == Sell {
			// Covering part (or all) of an existing short: release the
			// proportional margin and realize P&L against the short's
			// average price. Never gated on available balance (a
			// position-reducing trade must always be allowed to proceed).
			covered := fillQty
			if covered > existing.Quantity {
				covered = existing.Quantity
			}
			reserved := m.marginReserved[symbol]
			var released float64
			if existing.Quantity > 0 {
				released = reserved * float64(covered) / float64(existing.Quantity)
			}
			m.marginReserved[symbol] -= released
			realizedPnL := (existing.AveragePrice - fillPrice) * float64(covered)
			delta := released + realizedPnL - m.brokerageFee

			if remainder := fillQty - covered; remainder > 0 {
				// Flips through zero into a new long: pay full price for
				// the excess (mirrors updatePosition's own crossover
				// handling).
				delta -= fillPrice * float64(remainder)
			}
			return delta
		}
		// Normal buy (opening/adding a long): pay full turnover + fee.
		return -(turnover + m.brokerageFee)
	}
}

// PlaceOrder simulates order placement with latency, spread/slippage,
// occasional partial fills, margin-aware shorts, and occasional rejections.
func (m *MockBroker) PlaceOrder(order Order) (*FilledOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.connected {
		return nil, fmt.Errorf("not connected to broker")
	}

	m.orderCounter++
	orderID := fmt.Sprintf("MOCK-%d", m.orderCounter)

	// 1. Simulate network latency (small — this is a paper broker, not a
	// realism target; kept only so callers don't assume synchronous zero-time
	// execution).
	latency := time.Duration(1+m.rng.Intn(4)) * time.Millisecond
	time.Sleep(latency)

	// 2. Occasional rejection (task 5) — realistic broker-style error, same
	// phrasing AngelBroker uses for a genuine exchange rejection. No state
	// mutated.
	if m.rollRejection() {
		return nil, fmt.Errorf("order %s REJECTED by exchange: margin insufficient (simulated paper rejection)", orderID)
	}

	// 3. Get reference (last-traded) price.
	refPrice, exists := m.marketPrices[order.Symbol]
	if !exists {
		refPrice = 100 + m.rng.Float64()*400
		m.marketPrices[order.Symbol] = refPrice
	}

	// 4. Partial fill (task 3).
	fillQty := m.simulateFillQuantity(order.Quantity)

	// 5. Spread + size-scaled slippage (tasks 1-2).
	fillPrice, perUnitSlippage := m.simulateFillPrice(order.Symbol, order.Side, order.Quantity, refPrice)

	// 6. Settle cash (task 4: margin on shorts). existing must be read
	// BEFORE updatePosition mutates it.
	existing := m.openPositions[order.Symbol]
	opening := existing == nil || existing.Side == order.Side
	balanceDelta := m.settleCash(order.Symbol, order.Side, fillQty, fillPrice, existing)

	// Only gate orders that open/increase exposure — a position-reducing
	// order must never be blocked for insufficient balance/margin (mirrors
	// how reduce-only orders bypass the real broker's circuit breaker).
	if opening && balanceDelta < 0 && -balanceDelta > m.virtualBalance {
		return nil, fmt.Errorf("insufficient balance/margin: need ₹%.2f, have ₹%.2f",
			-balanceDelta, m.virtualBalance)
	}
	m.virtualBalance += balanceDelta

	// 7. Update positions.
	m.updatePosition(order.Symbol, fillQty, fillPrice, order.Side)
	if _, stillOpen := m.openPositions[order.Symbol]; !stillOpen {
		delete(m.marginReserved, order.Symbol) // clears fully-closed/rounding dust
	}

	// 8. Generate filled order (WP-1 partial-fill contract).
	status := "complete"
	if fillQty < order.Quantity {
		status = "cancelled" // partial-then-cancelled, matching AngelBroker's terminology
	}
	filled := &FilledOrder{
		OrderID:        orderID,
		Symbol:         order.Symbol,
		Quantity:       fillQty,
		RequestedQty:   order.Quantity,
		Side:           order.Side,
		FillPrice:      fillPrice,
		Slippage:       perUnitSlippage,
		TransactionFee: m.brokerageFee,
		Timestamp:      time.Now(),
		Status:         status,
	}

	fmt.Printf("MockBroker: Order Filled - %s %s %d/%d @ ₹%.2f (Slippage: ₹%.2f/unit) | Balance: ₹%.2f\n",
		order.Side, order.Symbol, fillQty, order.Quantity, fillPrice, perUnitSlippage, m.virtualBalance)

	return filled, nil
}

// updatePosition updates the position tracking
func (m *MockBroker) updatePosition(symbol string, quantity int, price float64, side OrderSide) {
	pos, exists := m.openPositions[symbol]

	if !exists {
		// New position
		m.openPositions[symbol] = &Position{
			Symbol:       symbol,
			Quantity:     quantity,
			AveragePrice: price,
			Side:         side,
		}
		return
	}

	// Update existing position
	if side == pos.Side {
		// Adding to position
		totalValue := (pos.AveragePrice * float64(pos.Quantity)) + (price * float64(quantity))
		pos.Quantity += quantity
		pos.AveragePrice = totalValue / float64(pos.Quantity)
	} else {
		// Reducing/closing position
		pos.Quantity -= quantity
		if pos.Quantity <= 0 {
			delete(m.openPositions, symbol)
		}
	}
}

// GetBalance returns the current virtual balance
func (m *MockBroker) GetBalance() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.virtualBalance
}

// GetPositions returns all open positions
func (m *MockBroker) GetPositions() map[string]*Position {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Return a copy to prevent race conditions
	positions := make(map[string]*Position)
	for k, v := range m.openPositions {
		positions[k] = &Position{
			Symbol:       v.Symbol,
			Quantity:     v.Quantity,
			AveragePrice: v.AveragePrice,
			Side:         v.Side,
		}
	}
	return positions
}

// GetCurrentPrice returns the current market price for a symbol
func (m *MockBroker) GetCurrentPrice(symbol string) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	price, exists := m.marketPrices[symbol]
	if !exists {
		// Initialize with a base price if not set
		price = 1000.0
		m.marketPrices[symbol] = price
	}

	// Simulate realistic price movement (±2% with slight bias for trends)
	movement := (m.rng.Float64() - 0.5) * 0.04 // -2% to +2%

	// Force movement if static at 1000
	if price == 1000.0 {
		movement = 0.01 + m.rng.Float64()*0.02
	}

	newPrice := price * (1 + movement)

	// Keep price in realistic range (avoid going too low or too high)
	if newPrice < 100 {
		newPrice = 100 + m.rng.Float64()*50
	}
	if newPrice > 5000 {
		newPrice = 5000 - m.rng.Float64()*500
	}

	m.marketPrices[symbol] = newPrice
	return newPrice
}

// GetCurrentVolume returns the current market volume for a symbol
func (m *MockBroker) GetCurrentVolume(symbol string) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	vol, exists := m.marketVolumes[symbol]
	if !exists {
		// Initialize with base volume
		vol = 10000.0
		m.marketVolumes[symbol] = vol
	}

	// Simulate volume increase
	increment := m.rng.Float64() * 500
	newVol := vol + increment
	m.marketVolumes[symbol] = newVol

	return newVol
}

// FetchMarketDataBatch fetches price and volume for multiple symbols
// For MockBroker, this just uses the simulated prices
func (m *MockBroker) FetchMarketDataBatch(symbols []string) (map[string]float64, map[string]float64) {
	prices := make(map[string]float64)
	volumes := make(map[string]float64)
	for _, sym := range symbols {
		prices[sym] = m.GetCurrentPrice(sym)
		volumes[sym] = m.GetCurrentVolume(sym)
	}
	return prices, volumes
}

// Close simulates closing the broker connection
func (m *MockBroker) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.connected = false
	fmt.Println("MockBroker: Connection closed")
	return nil
}
