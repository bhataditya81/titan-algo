package broker

import (
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// MockBroker simulates a real broker for paper trading
type MockBroker struct {
	mu             sync.RWMutex
	virtualBalance float64
	openPositions  map[string]*Position
	marketPrices   map[string]float64
	marketVolumes  map[string]float64
	orderCounter   int
	brokerageFee   float64
	connected      bool
}

// NewMockBroker creates a new mock broker with initial balance
func NewMockBroker(initialBalance float64) *MockBroker {
	return &MockBroker{
		virtualBalance: initialBalance,
		openPositions:  make(map[string]*Position),
		marketPrices:   make(map[string]float64),
		marketVolumes:  make(map[string]float64),
		brokerageFee:   20.0, // ₹20 flat fee per order
		connected:      false,
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
		m.marketPrices[token] = 100 + rand.Float64()*400
	}

	fmt.Printf("MockBroker: Subscribed to %d symbols\n", len(tokens))
	return nil
}

// PlaceOrder simulates order placement with latency and slippage
func (m *MockBroker) PlaceOrder(order Order) (*FilledOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.connected {
		return nil, fmt.Errorf("not connected to broker")
	}

	// 1. Simulate network latency (50-200ms)
	latency := time.Duration(50+rand.Intn(150)) * time.Millisecond
	time.Sleep(latency)

	// 2. Get current market price
	currentPrice, exists := m.marketPrices[order.Symbol]
	if !exists {
		// If price not set, generate one
		currentPrice = 100 + rand.Float64()*400
		m.marketPrices[order.Symbol] = currentPrice
	}

	// 3. Calculate slippage (0.05% - 0.10%)
	slippagePercent := 0.05 + rand.Float64()*0.05 // 0.05% to 0.10%
	var fillPrice float64
	var slippage float64

	if order.Side == Buy {
		// Buy orders fill higher (unfavorable)
		slippage = currentPrice * slippagePercent / 100.0
		fillPrice = currentPrice + slippage
	} else {
		// Sell orders fill lower (unfavorable)
		slippage = currentPrice * slippagePercent / 100.0
		fillPrice = currentPrice - slippage
	}

	// 4. Calculate total cost
	turnover := fillPrice * float64(order.Quantity)
	totalCost := turnover + m.brokerageFee

	// 5. Check if sufficient balance
	if order.Side == Buy && totalCost > m.virtualBalance {
		return nil, fmt.Errorf("insufficient balance: need ₹%.2f, have ₹%.2f",
			totalCost, m.virtualBalance)
	}

	// 6. Update balance
	if order.Side == Buy {
		m.virtualBalance -= totalCost
	} else {
		m.virtualBalance += (turnover - m.brokerageFee)
	}

	// 7. Update positions
	m.updatePosition(order.Symbol, order.Quantity, fillPrice, order.Side)

	// 8. Generate filled order
	m.orderCounter++
	filled := &FilledOrder{
		OrderID:        fmt.Sprintf("MOCK-%d", m.orderCounter),
		Symbol:         order.Symbol,
		Quantity:       order.Quantity,
		Side:           order.Side,
		FillPrice:      fillPrice,
		Slippage:       slippage,
		TransactionFee: m.brokerageFee,
		Timestamp:      time.Now(),
	}

	fmt.Printf("MockBroker: Order Filled - %s %s %d @ ₹%.2f (Slippage: ₹%.2f) | Balance: ₹%.2f\n",
		order.Side, order.Symbol, order.Quantity, fillPrice, slippage, m.virtualBalance)

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
	movement := (rand.Float64() - 0.5) * 0.04 // -2% to +2%

	// Force movement if static at 1000
	if price == 1000.0 {
		movement = 0.01 + rand.Float64()*0.02
	}

	newPrice := price * (1 + movement)

	// Keep price in realistic range (avoid going too low or too high)
	if newPrice < 100 {
		newPrice = 100 + rand.Float64()*50
	}
	if newPrice > 5000 {
		newPrice = 5000 - rand.Float64()*500
	}

	m.marketPrices[symbol] = newPrice
	// fmt.Printf("DEBUG: %s Old: %.2f New: %.2f (Move: %.4f)\n", symbol, price, newPrice, movement)
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
	increment := rand.Float64() * 500
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
