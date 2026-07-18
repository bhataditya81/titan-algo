package broker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pquerna/otp/totp"
)

// AngelBroker implements TradeService for Angel One SmartAPI
type AngelBroker struct {
	mu            sync.RWMutex
	clientCode    string
	pin           string
	apiKey        string
	totpSecret    string
	accessToken   string
	refreshToken  string
	feedToken     string
	httpClient    *http.Client
	baseURL       string
	connected     bool
	positions     map[string]*Position
	balance       float64
	marketPrices  map[string]float64
	marketVolumes map[string]float64
	wsConn        *websocket.Conn
	instruments   *InstrumentManager

	// Rate Limiting (10 orders/second per Angel One API limits)
	lastOrderTime     time.Time
	orderCount        int
	orderCountResetAt time.Time

	// Circuit Breaker
	consecutiveFailures int
	circuitOpenUntil    time.Time
}

// Angel One API endpoints
const (
	angelBaseURL   = "https://apiconnect.angelone.in"
	angelLoginPath = "/rest/auth/angelbroking/user/v1/loginByPassword"
	angelOrderPath = "/rest/secure/angelbroking/order/v1/placeOrder"
	angelLTPPath   = "/rest/secure/angelbroking/market/v1/quote/"
)

// Login request/response structures
type angelLoginRequest struct {
	ClientCode string `json:"clientcode"`
	Password   string `json:"password"`
	TOTP       string `json:"totp"`
}

type angelLoginResponse struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
	Data    struct {
		JWTToken     string `json:"jwtToken"`
		RefreshToken string `json:"refreshToken"`
		FeedToken    string `json:"feedToken"`
	} `json:"data"`
}

// Order request structure
type angelOrderRequest struct {
	Variety         string `json:"variety"`
	TradingSymbol   string `json:"tradingsymbol"`
	SymbolToken     string `json:"symboltoken"`
	TransactionType string `json:"transactiontype"`
	Exchange        string `json:"exchange"`
	OrderType       string `json:"ordertype"`
	ProductType     string `json:"producttype"`
	Duration        string `json:"duration"`
	Price           string `json:"price"`
	SquareOff       string `json:"squareoff"`
	StopLoss        string `json:"stoploss"`
	Quantity        string `json:"quantity"`
}

type angelOrderResponse struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
	Data    struct {
		OrderID string `json:"orderid"`
	} `json:"data"`
}

// Order book response for fetching actual fill details
type angelOrderBookResponse struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
	Data    []struct {
		OrderID       string      `json:"orderid"`
		Status        string      `json:"status"`       // "complete", "rejected", "pending", "cancelled"
		AveragePrice  json.Number `json:"averageprice"` // Actual fill price (can be string OR number)
		FilledShares  json.Number `json:"filledshares"` // Quantity filled (can be string OR number)
		TradingSymbol string      `json:"tradingsymbol"`
		Exchange      string      `json:"exchange"`
		OrderType     string      `json:"ordertype"`
		ProductType   string      `json:"producttype"`
		Quantity      string      `json:"quantity"`
		Text          string      `json:"text"` // Rejection reason if any
	} `json:"data"`
}

// NewAngelBroker creates a new Angel One broker instance
func NewAngelBroker(clientCode, pin, apiKey, totpSecret string) *AngelBroker {
	return &AngelBroker{
		clientCode:    clientCode,
		pin:           pin,
		apiKey:        apiKey,
		totpSecret:    totpSecret,
		httpClient:    &http.Client{Timeout: 30 * time.Second},
		baseURL:       angelBaseURL,
		connected:     false,
		positions:     make(map[string]*Position),
		marketPrices:  make(map[string]float64),
		marketVolumes: make(map[string]float64),
		balance:       0,
		instruments:   NewInstrumentManager(),
	}
}

// Connect authenticates with Angel One API
func (a *AngelBroker) Connect() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	fmt.Println("🔐 Connecting to Angel One...")

	// 1. Generate TOTP
	totpCode, err := a.generateTOTP()
	if err != nil {
		return fmt.Errorf("failed to generate TOTP: %w", err)
	}
	fmt.Println("🔑 Generated TOTP code")

	// 2. Login
	loginReq := angelLoginRequest{
		ClientCode: a.clientCode,
		Password:   a.pin,
		TOTP:       totpCode,
	}

	body, err := json.Marshal(loginReq)
	if err != nil {
		return fmt.Errorf("failed to marshal login request: %w", err)
	}

	req, err := http.NewRequest("POST", a.baseURL+angelLoginPath, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-UserType", "USER")
	req.Header.Set("X-SourceID", "WEB")
	req.Header.Set("X-ClientLocalIP", "127.0.0.1")
	req.Header.Set("X-ClientPublicIP", "127.0.0.1")
	req.Header.Set("X-MACAddress", "00:00:00:00:00:00")
	req.Header.Set("X-PrivateKey", a.apiKey)

	// Execute request
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("login request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	// Parse response
	var loginResp angelLoginResponse
	if err := json.Unmarshal(body, &loginResp); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	if !loginResp.Status {
		return fmt.Errorf("login failed: %s", loginResp.Message)
	}

	// Store tokens
	a.accessToken = loginResp.Data.JWTToken
	a.refreshToken = loginResp.Data.RefreshToken
	a.feedToken = loginResp.Data.FeedToken
	a.connected = true
	fmt.Println("✅ Connected to Angel One successfully")
	fmt.Printf("📊 Access Token: %s...\n", a.accessToken[:20])

	// Fetch Balance (Optional)
	_ = a.fetchBalance()

	// Load Instruments (always, since they need to be fetched fresh each session)
	if a.instruments == nil {
		a.instruments = NewInstrumentManager()
	}
	if err := a.instruments.LoadInstruments(); err != nil {
		fmt.Printf("⚠️ Failed to load instruments: %v\n", err)
	}

	// Sync existing positions from Angel One (important for restart recovery!)
	if err := a.fetchRemotePositions(); err != nil {
		log.Printf("⚠️ Failed to sync remote positions: %v", err)
	} else {
		log.Printf("✅ Synced %d existing positions from Angel One", len(a.positions))
	}

	// Start WebSocket
	// go a.connectWebSocket() // WebSocket implementation missing, using Polling in Subscribe instead

	return nil
}

// Position response structure
type angelPosition struct {
	SymbolToken    string  `json:"symboltoken"`
	TradingSymbol  string  `json:"tradingsymbol"`
	InstrumentType string  `json:"instrumenttype"`
	ProductType    string  `json:"producttype"`
	Pnl            float64 `json:"pnl,string"` // Angel sends this as string sometimes? Check response.
	NetQty         float64 `json:"netqty,string"`
	AvgPrice       float64 `json:"avgnetprice,string"` // Check actual field name
	LTP            float64 `json:"ltp,string"`
}

type angelPositionResponse struct {
	Status  bool            `json:"status"`
	Message string          `json:"message"`
	Data    []angelPosition `json:"data"`
}

// fetchRemotePositions gets all open positions from Angel One
func (a *AngelBroker) fetchRemotePositions() error {
	req, err := http.NewRequest("GET", a.baseURL+"/rest/secure/angelbroking/portfolio/v1/getPosition", nil)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-PrivateKey", a.apiKey)
	req.Header.Set("Authorization", "Bearer "+a.accessToken)
	req.Header.Set("X-ClientLocalIP", "127.0.0.1")
	req.Header.Set("X-ClientPublicIP", "127.0.0.1")
	req.Header.Set("X-MACAddress", "00:00:00:00:00:00")
	req.Header.Set("X-UserType", "USER")
	req.Header.Set("X-SourceID", "WEB")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if len(body) == 0 {
		// Empty body likely means no positions or 204 No Content behavior
		return nil
	}

	// Check for WAF block / rate limit response (HTML response instead of JSON)
	bodyStr := string(body)
	if strings.Contains(bodyStr, "Request Rejected") || strings.Contains(bodyStr, "<!DOCTYPE") || strings.Contains(bodyStr, "<html>") {
		log.Printf("⚠️ WAF Block Detected - API temporarily blocked. Wait 5-10 minutes before retrying.")
		log.Printf("   (This is normal after heavy API usage. Positions sync skipped.)")
		return nil // Treat as non-fatal - continue without remote positions
	}

	var posResp angelPositionResponse
	if err := json.Unmarshal(body, &posResp); err != nil {
		return fmt.Errorf("failed to unmarshal positions (Status: %d, Body: '%s'): %w", resp.StatusCode, string(body), err)
	}

	if !posResp.Status {
		// If no positions, it might return success with empty data or logic depending on API
		// But usually status is true.
		return fmt.Errorf("API error: %s", posResp.Message)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// Clear existing positions map (assuming we are refreshing)
	a.positions = make(map[string]*Position)

	log.Printf("📥 Fetched %d remote positions", len(posResp.Data))

	for _, p := range posResp.Data {
		qty := int(p.NetQty)

		if qty != 0 {
			side := Buy
			if qty < 0 {
				side = Sell
				qty = -qty
			}

			a.positions[p.TradingSymbol] = &Position{
				Symbol:       p.TradingSymbol,
				Quantity:     qty,
				AveragePrice: p.AvgPrice, // Direct assignment now
				Side:         side,
			}
			log.Printf("  • Found Open Position: %s %s x %d", side, p.TradingSymbol, qty)
		}
	}

	return nil
}

// LTP request/response structures (New Quote API format)
type angelQuoteRequest struct {
	Mode           string              `json:"mode"`
	ExchangeTokens map[string][]string `json:"exchangeTokens"` // e.g., {"NSE": ["3045"]}
}

type angelQuoteResponse struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
	Data    struct {
		Fetched []struct {
			Exchange      string      `json:"exchange"`
			TradingSymbol string      `json:"tradingSymbol"`
			SymbolToken   string      `json:"symbolToken"`
			LTP           float64     `json:"ltp"`
			Open          float64     `json:"open"`
			High          float64     `json:"high"`
			Low           float64     `json:"low"`
			Close         float64     `json:"close"`
			TradeVolume   json.Number `json:"tradeVolume"` // Handle both string and number
		} `json:"fetched"`
		UnFetched []struct {
			Exchange    string `json:"exchange"`
			SymbolToken string `json:"symbolToken"`
			Message     string `json:"message"`
		} `json:"unfetched"`
	} `json:"data"`
}

// Subscribe subscribes to market data and starts background refresh
func (a *AngelBroker) Subscribe(symbols []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.connected {
		return fmt.Errorf("not connected to broker")
	}

	fmt.Printf("📡 Subscribed to %d symbols: %v\n", len(symbols), symbols)

	// Background refresh loop - fetches live prices periodically
	go func() {
		ticker := time.NewTicker(5 * time.Second) // Refresh every 5 seconds
		defer ticker.Stop()

		for range ticker.C {
			a.mu.RLock()
			if !a.connected {
				a.mu.RUnlock()
				break
			}
			a.mu.RUnlock()

			// Batch fetch all symbols (1 API call)
			prices, _ := a.FetchMarketDataBatch(symbols)
			if len(prices) > 0 {
				// Silent update - no spam logging
			}
		}
	}()

	return nil
}

// fetchMarketData gets the Price and Volume from Angel One using the Quote API
func (a *AngelBroker) fetchMarketData(symbol string) (float64, float64, error) {
	// Get instrument details
	inst, err := a.instruments.GetInstrument(symbol)
	if err != nil {
		return 0, 0, err
	}

	// Build request using Quote API - FULL mode to get Volume
	reqPayload := angelQuoteRequest{
		Mode: "FULL",
		ExchangeTokens: map[string][]string{
			inst.ExchSeg: {inst.Token},
		},
	}

	reqBody, err := json.Marshal(reqPayload)
	if err != nil {
		return 0, 0, err
	}

	req, err := http.NewRequest("POST", a.baseURL+angelLTPPath, bytes.NewBuffer(reqBody))
	if err != nil {
		return 0, 0, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-PrivateKey", a.apiKey)
	req.Header.Set("Authorization", "Bearer "+a.accessToken)
	req.Header.Set("X-ClientLocalIP", "127.0.0.1")
	req.Header.Set("X-ClientPublicIP", "127.0.0.1")
	req.Header.Set("X-MACAddress", "00:00:00:00:00:00")
	req.Header.Set("X-UserType", "USER")
	req.Header.Set("X-SourceID", "WEB")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, 0, err
	}

	// Handle empty body
	if len(body) == 0 {
		return 0, 0, fmt.Errorf("empty response from API")
	}

	var quoteResp angelQuoteResponse
	if err := json.Unmarshal(body, &quoteResp); err != nil {
		return 0, 0, fmt.Errorf("%w (Body: '%s')", err, string(body))
	}

	if !quoteResp.Status {
		return 0, 0, fmt.Errorf("API error: %s", quoteResp.Message)
	}

	// Extract Data
	if len(quoteResp.Data.Fetched) > 0 {
		data := quoteResp.Data.Fetched[0]

		// Parse Volume
		var vol float64
		// Try Int64 first
		if v, err := data.TradeVolume.Int64(); err == nil {
			vol = float64(v)
		} else {
			// Try Float64
			if v, err := data.TradeVolume.Float64(); err == nil {
				vol = v
			} else {
				// Log warning but continue with 0 volume
				fmt.Printf("⚠️ Failed to parse volume '%v': %v\n", data.TradeVolume, err)
			}
		}

		return data.LTP, vol, nil
	}

	// Check if unfetched
	if len(quoteResp.Data.UnFetched) > 0 {
		return 0, 0, fmt.Errorf("data unavailable: %s", quoteResp.Data.UnFetched[0].Message)
	}

	return 0, 0, fmt.Errorf("no data returned for %s", symbol)
}

// FetchMarketDataBatch fetches price and volume for multiple symbols in a single API call
// This is much more efficient than individual calls and avoids rate limits
func (a *AngelBroker) FetchMarketDataBatch(symbols []string) (map[string]float64, map[string]float64) {
	prices := make(map[string]float64)
	volumes := make(map[string]float64)

	if len(symbols) == 0 {
		return prices, volumes
	}

	a.mu.RLock()
	connected := a.connected
	a.mu.RUnlock()

	if !connected {
		return prices, volumes
	}

	// Group symbols by exchange for the API request
	exchangeTokens := make(map[string][]string)
	symbolToToken := make(map[string]string) // token -> symbol mapping for response

	for _, symbol := range symbols {
		inst, err := a.instruments.GetInstrument(symbol)
		if err != nil {
			continue
		}
		exchangeTokens[inst.ExchSeg] = append(exchangeTokens[inst.ExchSeg], inst.Token)
		symbolToToken[inst.Token] = symbol
	}

	if len(exchangeTokens) == 0 {
		return prices, volumes
	}

	// Build request using Quote API - FULL mode to get Volume
	reqPayload := angelQuoteRequest{
		Mode:           "FULL",
		ExchangeTokens: exchangeTokens,
	}

	reqBody, err := json.Marshal(reqPayload)
	if err != nil {
		log.Printf("⚠️ Failed to marshal batch request: %v", err)
		return prices, volumes
	}

	req, err := http.NewRequest("POST", a.baseURL+angelLTPPath, bytes.NewBuffer(reqBody))
	if err != nil {
		log.Printf("⚠️ Failed to create batch request: %v", err)
		return prices, volumes
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-PrivateKey", a.apiKey)
	req.Header.Set("Authorization", "Bearer "+a.accessToken)
	req.Header.Set("X-ClientLocalIP", "127.0.0.1")
	req.Header.Set("X-ClientPublicIP", "127.0.0.1")
	req.Header.Set("X-MACAddress", "00:00:00:00:00:00")
	req.Header.Set("X-UserType", "USER")
	req.Header.Set("X-SourceID", "WEB")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		log.Printf("⚠️ Batch API request failed: %v", err)
		return prices, volumes
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("⚠️ Failed to read batch response: %v", err)
		return prices, volumes
	}

	if len(body) == 0 {
		log.Printf("⚠️ Empty batch response")
		return prices, volumes
	}

	// Check for rate limit response
	if strings.Contains(string(body), "exceeding access rate") {
		log.Printf("⚠️ Rate limit hit on batch request, waiting...")
		return prices, volumes
	}

	var quoteResp angelQuoteResponse
	if err := json.Unmarshal(body, &quoteResp); err != nil {
		log.Printf("⚠️ Failed to parse batch response: %v", err)
		return prices, volumes
	}

	if !quoteResp.Status {
		log.Printf("⚠️ Batch API error: %s", quoteResp.Message)
		return prices, volumes
	}

	// Process fetched data
	for _, data := range quoteResp.Data.Fetched {
		symbol := symbolToToken[data.SymbolToken]
		if symbol == "" {
			symbol = data.TradingSymbol
		}

		if data.LTP > 0 {
			prices[symbol] = data.LTP
			// Cache it
			a.mu.Lock()
			a.marketPrices[symbol] = data.LTP
			a.mu.Unlock()
		}

		// Parse Volume
		var vol float64
		if v, err := data.TradeVolume.Int64(); err == nil {
			vol = float64(v)
		} else if v, err := data.TradeVolume.Float64(); err == nil {
			vol = v
		}
		if vol > 0 {
			volumes[symbol] = vol
			a.mu.Lock()
			a.marketVolumes[symbol] = vol
			a.mu.Unlock()
		}
	}

	log.Printf("📊 Batch fetched: %d prices, %d volumes from %d symbols", len(prices), len(volumes), len(symbols))
	return prices, volumes
}
func (a *AngelBroker) PlaceOrder(order Order) (*FilledOrder, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.connected {
		return nil, fmt.Errorf("not connected to broker")
	}

	// 🛡️ CIRCUIT BREAKER CHECK
	// If circuit is open, reject orders until cooldown expires
	if time.Now().Before(a.circuitOpenUntil) {
		return nil, fmt.Errorf("circuit breaker OPEN: API paused until %s (had %d consecutive failures)",
			a.circuitOpenUntil.Format("15:04:05"), a.consecutiveFailures)
	}

	// 🛡️ RATE LIMITER (10 orders/second per Angel One API limits)
	now := time.Now()
	// Reset counter every second
	if now.After(a.orderCountResetAt) {
		a.orderCount = 0
		a.orderCountResetAt = now.Add(1 * time.Second)
	}
	// Check if we've hit the limit
	if a.orderCount >= 10 {
		waitTime := a.orderCountResetAt.Sub(now)
		log.Printf("⏳ Rate limit: waiting %v before placing order...", waitTime)
		// Release lock while waiting
		a.mu.Unlock()
		time.Sleep(waitTime + 10*time.Millisecond) // Small buffer
		a.mu.Lock()
		a.orderCount = 0
		a.orderCountResetAt = time.Now().Add(1 * time.Second)
	}
	a.orderCount++
	a.lastOrderTime = now

	// Get instrument details
	inst, err := a.instruments.GetInstrument(order.Symbol)
	if err != nil {
		return nil, fmt.Errorf("invalid symbol %s: %v", order.Symbol, err)
	}

	fmt.Printf("\n🔄 Placing order with Angel One: %s %s %d (Token: %s, Exch: %s)\n",
		order.Side, order.Symbol, order.Quantity, inst.Token, inst.ExchSeg)

	// Determine Product Type & Exchange
	exchange := inst.ExchSeg
	productType := "INTRADAY" // Default
	if exchange == "MCX" || exchange == "NFO" {
		productType = "CARRYFORWARD" // Usually for F&O/Commodity we might want carryforward or check config
	}

	// Map order to Angel One format
	angelOrder := angelOrderRequest{
		Variety:         "NORMAL",
		TradingSymbol:   inst.Symbol,
		SymbolToken:     inst.Token,
		TransactionType: string(order.Side),
		Exchange:        exchange,
		OrderType:       string(order.OrderType),
		ProductType:     productType,
		Duration:        "DAY",
		Price:           "0", // Market order
		SquareOff:       "0",
		StopLoss:        "0",
		Quantity:        fmt.Sprintf("%d", order.Quantity),
	}

	reqBody, err := json.Marshal(angelOrder)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal order: %w", err)
	}

	// Create HTTP request
	req, err := http.NewRequest("POST", a.baseURL+angelOrderPath, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-UserType", "USER")
	req.Header.Set("X-SourceID", "WEB")
	req.Header.Set("X-ClientLocalIP", "127.0.0.1")
	req.Header.Set("X-ClientPublicIP", "127.0.0.1")
	req.Header.Set("X-MACAddress", "00:00:00:00:00:00")
	req.Header.Set("X-PrivateKey", a.apiKey)
	req.Header.Set("Authorization", "Bearer "+a.accessToken)

	// Execute request
	resp, err := a.httpClient.Do(req)
	if err != nil {
		// Trip circuit breaker on network failure
		a.consecutiveFailures++
		if a.consecutiveFailures >= 5 {
			a.circuitOpenUntil = time.Now().Add(30 * time.Second)
			log.Printf("🔴 Circuit breaker TRIPPED: %d consecutive failures. Pausing for 30s.", a.consecutiveFailures)
		}
		return nil, fmt.Errorf("order request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Parse response
	var orderResp angelOrderResponse
	if err := json.Unmarshal(body, &orderResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if !orderResp.Status {
		// Increment failure counter (but don't trip immediately on business logic failures)
		a.consecutiveFailures++
		if a.consecutiveFailures >= 5 {
			a.circuitOpenUntil = time.Now().Add(30 * time.Second)
			log.Printf("🔴 Circuit breaker TRIPPED after order rejection. Pausing for 30s.")
		}
		return nil, fmt.Errorf("order placement failed: %s", orderResp.Message)
	}

	// Reset circuit breaker on success
	a.consecutiveFailures = 0

	fmt.Printf("✅ Order placed successfully: OrderID=%s\n", orderResp.Data.OrderID)

	// Poll for actual fill status and price (critical for accurate P&L!)
	// Retry up to 10 times with exponential backoff
	var fillPrice float64
	var fillQty int
	var orderStatus string
	backoffMs := 500 // Start with 500ms

	for attempt := 1; attempt <= 10; attempt++ {
		time.Sleep(time.Duration(backoffMs) * time.Millisecond)

		status, price, qty, err := a.fetchOrderStatus(orderResp.Data.OrderID)
		if err != nil {
			log.Printf("⚠️ Order status check attempt %d/10 failed: %v", attempt, err)
			backoffMs = min(backoffMs*2, 5000) // Cap at 5 seconds
			continue
		}

		orderStatus = status
		fillPrice = price
		fillQty = qty

		if status == "complete" {
			log.Printf("✅ Order FILLED: %s @ ₹%.2f (Qty: %d)", order.Symbol, fillPrice, fillQty)
			break
		} else if status == "rejected" {
			return nil, fmt.Errorf("order REJECTED by exchange")
		} else if status == "cancelled" {
			return nil, fmt.Errorf("order was CANCELLED")
		}
		// Still pending, continue polling with backoff
		log.Printf("⏳ Order pending (attempt %d/10, next wait: %dms)...", attempt, backoffMs)
		backoffMs = min(backoffMs*2, 5000) // Exponential backoff, cap at 5s
	}

	// If we couldn't get fill price, fallback to LTP (with warning)
	// NOTE: Access cached price directly to avoid deadlock (we already hold the write lock)
	if fillPrice <= 0 {
		fillPrice = a.marketPrices[order.Symbol]
		if fillPrice <= 0 {
			// Fallback to a reasonable estimate
			fillPrice = 1.0
		}
		log.Printf("⚠️ Could not fetch actual fill price, using cached LTP: ₹%.2f", fillPrice)
	}

	// Calculate actual slippage
	// NOTE: Access cached price directly to avoid deadlock (we already hold the write lock)
	ltpPrice := a.marketPrices[order.Symbol]
	slippage := 0.0
	if ltpPrice > 0 {
		if order.Side == Buy {
			slippage = fillPrice - ltpPrice
		} else {
			slippage = ltpPrice - fillPrice
		}
	} else {
		log.Printf("⚠️ Could not calculate slippage: LTP not cached for %s", order.Symbol)
	}

	filled := &FilledOrder{
		OrderID:   orderResp.Data.OrderID,
		Symbol:    order.Symbol,
		Quantity:  order.Quantity,
		Side:      order.Side,
		FillPrice: fillPrice,
		Slippage:  slippage,
		// Dynamic fee estimate: ~0.1% of turnover, capped at ₹20 per leg (approximate)
		TransactionFee: min(fillPrice*float64(order.Quantity)*0.001, 20.0),
		Timestamp:      time.Now(),
	}

	// Update positions with actual fill price
	a.updatePosition(order.Symbol, order.Quantity, fillPrice, order.Side)

	log.Printf("📋 Order Summary: %s %s %d @ ₹%.2f | Slippage: ₹%.2f | Status: %s",
		order.Side, order.Symbol, order.Quantity, fillPrice, slippage, orderStatus)

	return filled, nil
}

// fetchOrderStatus polls the order book to get actual fill status and price
// This is critical for accurate P&L tracking - never assume fill price!
func (a *AngelBroker) fetchOrderStatus(orderID string) (status string, avgPrice float64, filledQty int, err error) {
	req, err := http.NewRequest("GET", a.baseURL+"/rest/secure/angelbroking/order/v1/getOrderBook", nil)
	if err != nil {
		return "", 0, 0, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-PrivateKey", a.apiKey)
	req.Header.Set("Authorization", "Bearer "+a.accessToken)
	req.Header.Set("X-ClientLocalIP", "127.0.0.1")
	req.Header.Set("X-ClientPublicIP", "127.0.0.1")
	req.Header.Set("X-MACAddress", "00:00:00:00:00:00")
	req.Header.Set("X-UserType", "USER")
	req.Header.Set("X-SourceID", "WEB")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, 0, err
	}

	var orderBookResp angelOrderBookResponse
	if err := json.Unmarshal(body, &orderBookResp); err != nil {
		return "", 0, 0, fmt.Errorf("failed to parse order book: %w", err)
	}

	if !orderBookResp.Status {
		return "", 0, 0, fmt.Errorf("order book API error: %s", orderBookResp.Message)
	}

	// Find our order in the book
	for _, order := range orderBookResp.Data {
		if order.OrderID == orderID {
			// Parse average price (json.Number handles both string and number)
			price, _ := order.AveragePrice.Float64()

			// Parse filled quantity
			qtyInt64, _ := order.FilledShares.Int64()
			qty := int(qtyInt64)

			log.Printf("📊 Order %s Status: %s | Fill Price: ₹%.2f | Filled: %d",
				orderID, order.Status, price, qty)

			return order.Status, price, qty, nil
		}
	}

	return "", 0, 0, fmt.Errorf("order %s not found in order book", orderID)
}

// GetBalance returns current account balance
func (a *AngelBroker) GetBalance() float64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.balance
}

// GetPositions returns all open positions
func (a *AngelBroker) GetPositions() map[string]*Position {
	a.mu.RLock()
	defer a.mu.RUnlock()

	positions := make(map[string]*Position)
	for k, v := range a.positions {
		positions[k] = &Position{
			Symbol:       v.Symbol,
			Quantity:     v.Quantity,
			AveragePrice: v.AveragePrice,
			Side:         v.Side,
		}
	}
	return positions
}

// GetCurrentPrice returns current market price (from cache, or fetches on-demand if connected)
func (a *AngelBroker) GetCurrentPrice(symbol string) float64 {
	a.mu.RLock()
	price, ok := a.marketPrices[symbol]
	connected := a.connected
	a.mu.RUnlock()

	if ok && price > 0 {
		return price
	}

	// Lazy fetch: If connected but no cached price, try fetching directly
	if connected {
		fetchedPrice, _, err := a.fetchMarketData(symbol)
		if err == nil && fetchedPrice > 0 {
			// Cache the fetched price
			a.mu.Lock()
			a.marketPrices[symbol] = fetchedPrice
			a.mu.Unlock()
			return fetchedPrice
		}
	}

	return 0.0
}

// GetCurrentVolume returns current trading volume (from cache, or fetches on-demand if connected)
func (a *AngelBroker) GetCurrentVolume(symbol string) float64 {
	a.mu.RLock()
	vol, ok := a.marketVolumes[symbol]
	connected := a.connected
	a.mu.RUnlock()

	if ok && vol > 0 {
		return vol
	}

	// Lazy fetch: If connected but no cached volume, try fetching directly
	if connected {
		fetchedPrice, fetchedVol, err := a.fetchMarketData(symbol)
		if err == nil {
			// Cache both price and volume
			a.mu.Lock()
			if fetchedPrice > 0 {
				a.marketPrices[symbol] = fetchedPrice
			}
			if fetchedVol > 0 {
				a.marketVolumes[symbol] = fetchedVol
			}
			a.mu.Unlock()
			return fetchedVol
		}
	}

	return 0.0
}

// Close closes the connection
func (a *AngelBroker) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.wsConn != nil {
		_ = a.wsConn.Close()
	}

	a.connected = false
	fmt.Println("🔌 Disconnected from Angel One")
	return nil
}

// Helper methods

func (a *AngelBroker) generateTOTP() (string, error) {
	if a.totpSecret == "" {
		return "", fmt.Errorf("TOTP secret not provided in config")
	}

	// Used pquerna/otp/totp library
	code, err := totp.GenerateCode(a.totpSecret, time.Now())
	if err != nil {
		return "", err
	}
	return code, nil
}

func (a *AngelBroker) fetchBalance() error {
	// TODO: Implement profile API call to fetch balance
	a.balance = 10000.0 // Placeholder
	fmt.Printf("💰 Account Balance: ₹%.2f\n", a.balance)
	return nil
}

func (a *AngelBroker) updatePosition(symbol string, quantity int, price float64, side OrderSide) {
	pos, exists := a.positions[symbol]

	if !exists {
		a.positions[symbol] = &Position{
			Symbol:       symbol,
			Quantity:     quantity,
			AveragePrice: price,
			Side:         side,
		}
		return
	}

	if side == pos.Side {
		totalValue := (pos.AveragePrice * float64(pos.Quantity)) + (price * float64(quantity))
		pos.Quantity += quantity
		pos.AveragePrice = totalValue / float64(pos.Quantity)
	} else {
		pos.Quantity -= quantity
		if pos.Quantity <= 0 {
			delete(a.positions, symbol)
		}
	}
}
