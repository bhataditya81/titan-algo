package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pquerna/otp/totp"
	"golang.org/x/time/rate"
)

// AngelBroker implements TradeService (and ExtendedTradeService) for Angel One SmartAPI.
//
// Lock discipline (EX-5): a.mu guards ONLY in-memory state (tokens, positions,
// price cache, health, circuit-breaker counters). It is never held across an
// HTTP call, a sleep, or the fill-poll loop. Read the state you need into
// local variables under RLock/Lock, release, then do I/O.
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
	priceUpdated  map[string]time.Time // CR-14/EX-6: staleness tracking per symbol
	liveFeed      *wsFeed              // G-7: optional WS 2.0 feed, additive to REST polling
	instruments   *InstrumentManager

	// Session health (EX-6)
	healthy     bool
	healthErr   error

	// Order variety, remembered per broker order ID so CancelOrder can send
	// the correct variety (NORMAL vs STOPLOSS) without the caller tracking it.
	orderVariety map[string]string

	// Rate limiting (EX-5): token bucket, 10 orders/sec per Angel One limits.
	// Waited on OUTSIDE a.mu — never sleeps while holding the lock.
	orderLimiter *rate.Limiter

	// Circuit Breaker — entry orders only; reduce-only orders always bypass.
	consecutiveFailures int
	circuitOpenUntil    time.Time
}

// Angel One API endpoints (remediation plan Appendix A — do not add others).
const (
	angelBaseURL         = "https://apiconnect.angelone.in"
	angelLoginPath       = "/rest/auth/angelbroking/user/v1/loginByPassword" // A-1
	angelOrderPath       = "/rest/secure/angelbroking/order/v1/placeOrder"   // A-2
	angelCancelPath      = "/rest/secure/angelbroking/order/v1/cancelOrder"  // A-3
	angelRefreshPath     = "/rest/auth/angelbroking/jwt/v1/generateTokens"   // A-4
	angelRMSPath         = "/rest/secure/angelbroking/user/v1/getRMS"       // A-5
	angelOrderBookPath   = "/rest/secure/angelbroking/order/v1/getOrderBook" // A-7
	angelPositionsPath   = "/rest/secure/angelbroking/order/v1/getPosition"  // A-8
	angelLTPPath         = "/rest/secure/angelbroking/market/v1/quote/"      // A-10
	// angelMarginPath is the margin-calculator batch endpoint (Appendix A-6).
	// VERIFICATION FLAG (G-1, task instruction): confirmed plausible via two
	// independent web lookups against the public SmartAPI forum/docs during
	// this work package (see R2-1-REPORT.md), but NOT hit against a live
	// sandbox call. Verify request/response shape against a real account
	// before trusting this in production.
	angelMarginPath = "/rest/secure/angelbroking/margin/v1/batch" // A-6
)

// maxMarginBasketLegs is Angel's documented per-request cap on the margin
// batch endpoint (50 positions).
const maxMarginBasketLegs = 50

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

type angelRefreshRequest struct {
	RefreshToken string `json:"refreshToken"`
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
	TriggerPrice    string `json:"triggerprice"`
	Quantity        string `json:"quantity"`
}

type angelOrderResponse struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
	ErrCode string `json:"errorcode"`
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
		Status        string      `json:"status"` // "complete", "rejected", "open", "cancelled", "partially filled", ...
		AveragePrice  json.Number `json:"averageprice"`
		FilledShares  json.Number `json:"filledshares"`
		TradingSymbol string      `json:"tradingsymbol"`
	} `json:"data"`
}

type angelRMSResponse struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
	Data    struct {
		AvailableCash string `json:"availablecash"`
	} `json:"data"`
}

// Margin batch request/response structures (endpoint A-6, G-1). Shape
// confirmed against public SmartAPI margin-calculator documentation (see the
// verification flag on angelMarginPath): request is a flat list of legs
// ("positions"), response follows the same {status,message,errorcode,data}
// envelope every other Angel endpoint in this file uses. `Data` is a pointer
// so a `"data":null` response (Angel's own documented error example) is
// distinguishable from a present-but-zero value — both are treated as
// fail-closed errors.
type angelMarginPositionReq struct {
	Exchange    string  `json:"exchange"`
	Qty         int     `json:"qty"`
	Price       float64 `json:"price"`
	ProductType string  `json:"productType"`
	Token       string  `json:"token"`
	TradeType   string  `json:"tradeType"`
}

type angelMarginRequest struct {
	Positions []angelMarginPositionReq `json:"positions"`
}

type angelMarginResponse struct {
	Status    bool   `json:"status"`
	Message   string `json:"message"`
	ErrorCode string `json:"errorcode"`
	Data      *struct {
		TotalMarginRequired float64 `json:"totalMarginRequired"`
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
		priceUpdated:  make(map[string]time.Time),
		orderVariety:  make(map[string]string),
		orderLimiter:  rate.NewLimiter(rate.Limit(10), 10), // 10 orders/sec, burst 10 (EX-5)
		instruments:   NewInstrumentManager(),
	}
}

// Connect authenticates with Angel One API.
func (a *AngelBroker) Connect() error {
	fmt.Println("🔐 Connecting to Angel One...")

	loginResp, err := a.login()
	if err != nil {
		return err
	}

	a.mu.Lock()
	a.accessToken = loginResp.Data.JWTToken
	a.refreshToken = loginResp.Data.RefreshToken
	a.feedToken = loginResp.Data.FeedToken
	a.connected = true
	a.healthy = true
	a.healthErr = nil
	a.mu.Unlock()

	// No secrets in logs (audit: remove token/key logging).
	fmt.Println("✅ Connected to Angel One successfully")

	if _, err := a.RefreshBalance(); err != nil {
		fmt.Printf("⚠️ Failed to fetch balance: %v\n", err)
	}

	// Instruments load OUTSIDE the broker lock — this can be a slow download
	// (EX-9). InstrumentManager guards its own state.
	if a.instruments == nil {
		a.instruments = NewInstrumentManager()
	}
	if err := a.instruments.LoadInstruments(); err != nil {
		fmt.Printf("⚠️ Failed to load instruments: %v\n", err)
	}

	if err := a.fetchRemotePositions(); err != nil {
		log.Printf("⚠️ Failed to sync remote positions: %v", err)
	} else {
		a.mu.RLock()
		n := len(a.positions)
		a.mu.RUnlock()
		log.Printf("✅ Synced %d existing positions from Angel One", n)
	}

	return nil
}

// login performs the TOTP + password login (endpoint A-1). Does not touch a.mu
// except implicitly via authHeaders (accessToken is empty pre-login, harmless).
func (a *AngelBroker) login() (angelLoginResponse, error) {
	var out angelLoginResponse

	totpCode, err := a.generateTOTP()
	if err != nil {
		return out, fmt.Errorf("failed to generate TOTP: %w", err)
	}

	payload, err := json.Marshal(angelLoginRequest{ClientCode: a.clientCode, Password: a.pin, TOTP: totpCode})
	if err != nil {
		return out, fmt.Errorf("failed to marshal login request: %w", err)
	}

	body, status, _, err := a.rawRequest(http.MethodPost, angelLoginPath, payload)
	if err != nil {
		return out, fmt.Errorf("login request failed: %w", err)
	}
	if isWAFBlocked(body) {
		return out, fmt.Errorf("login blocked (WAF/edge)")
	}
	if status != http.StatusOK {
		return out, &HTTPStatusError{StatusCode: status, Path: angelLoginPath, Body: truncateForLog(body)}
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("failed to parse login response: %w", err)
	}
	if !out.Status {
		return out, fmt.Errorf("login failed: %s", out.Message)
	}
	return out, nil
}

// Position response structure
type angelPosition struct {
	TradingSymbol string  `json:"tradingsymbol"`
	NetQty        float64 `json:"netqty,string"`
	AvgPrice      float64 `json:"avgnetprice,string"`
}

type angelPositionResponse struct {
	Status  bool            `json:"status"`
	Message string          `json:"message"`
	Data    []angelPosition `json:"data"`
}

// fetchRemotePositions gets all open positions from Angel One (endpoint A-8).
//
// EX-9: any transport failure, non-200, or WAF block is now a returned error —
// never silently reported as "zero positions" (that would poison emergency
// liquidation logic upstream).
func (a *AngelBroker) fetchRemotePositions() error {
	body, err := a.doAPIRequest(http.MethodGet, angelPositionsPath, nil)
	if err != nil {
		return fmt.Errorf("position fetch failed: %w", err)
	}

	if len(body) == 0 {
		// A genuinely empty (200 OK) body means "no positions" — not an error.
		a.mu.Lock()
		a.positions = make(map[string]*Position)
		a.mu.Unlock()
		return nil
	}

	var posResp angelPositionResponse
	if err := json.Unmarshal(body, &posResp); err != nil {
		return fmt.Errorf("failed to unmarshal positions (body: %s): %w", truncateForLog(body), err)
	}
	if !posResp.Status {
		return fmt.Errorf("positions API error: %s", posResp.Message)
	}

	positions := make(map[string]*Position)
	for _, p := range posResp.Data {
		qty := int(p.NetQty)
		if qty == 0 {
			continue
		}
		side := Buy
		if qty < 0 {
			side = Sell
			qty = -qty
		}
		positions[p.TradingSymbol] = &Position{Symbol: p.TradingSymbol, Quantity: qty, AveragePrice: p.AvgPrice, Side: side}
		log.Printf("  • Found Open Position: %s %s x %d", side, p.TradingSymbol, qty)
	}

	a.mu.Lock()
	a.positions = positions
	a.mu.Unlock()

	log.Printf("📥 Fetched %d remote positions", len(positions))
	return nil
}

// LTP request/response structures (Quote API)
type angelQuoteRequest struct {
	Mode           string              `json:"mode"`
	ExchangeTokens map[string][]string `json:"exchangeTokens"`
}

type angelQuoteResponse struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
	Data    struct {
		Fetched []struct {
			TradingSymbol string      `json:"tradingSymbol"`
			SymbolToken   string      `json:"symbolToken"`
			LTP           float64     `json:"ltp"`
			TradeVolume   json.Number `json:"tradeVolume"`
		} `json:"fetched"`
		UnFetched []struct {
			SymbolToken string `json:"symbolToken"`
			Message     string `json:"message"`
		} `json:"unfetched"`
	} `json:"data"`
}

// Subscribe subscribes to market data and starts background refresh.
func (a *AngelBroker) Subscribe(symbols []string) error {
	a.mu.RLock()
	connected := a.connected
	a.mu.RUnlock()
	if !connected {
		return fmt.Errorf("not connected to broker")
	}

	fmt.Printf("📡 Subscribed to %d symbols: %v\n", len(symbols), symbols)

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			a.mu.RLock()
			stillConnected := a.connected
			a.mu.RUnlock()
			if !stillConnected {
				return
			}
			a.FetchMarketDataBatch(symbols)
		}
	}()

	return nil
}

// fetchMarketData gets price and volume for a single symbol (Quote API, A-10).
func (a *AngelBroker) fetchMarketData(symbol string) (float64, float64, error) {
	inst, err := a.instruments.GetInstrument(symbol)
	if err != nil {
		return 0, 0, err
	}

	payload, err := json.Marshal(angelQuoteRequest{Mode: "FULL", ExchangeTokens: map[string][]string{inst.ExchSeg: {inst.Token}}})
	if err != nil {
		return 0, 0, err
	}

	body, err := a.doAPIRequest(http.MethodPost, angelLTPPath, payload)
	if err != nil {
		return 0, 0, err
	}

	var quoteResp angelQuoteResponse
	if err := json.Unmarshal(body, &quoteResp); err != nil {
		return 0, 0, fmt.Errorf("%w (body: %s)", err, truncateForLog(body))
	}
	if !quoteResp.Status {
		return 0, 0, fmt.Errorf("quote API error: %s", quoteResp.Message)
	}

	if len(quoteResp.Data.Fetched) > 0 {
		d := quoteResp.Data.Fetched[0]
		return d.LTP, parseTradeVolume(d.TradeVolume), nil
	}
	if len(quoteResp.Data.UnFetched) > 0 {
		return 0, 0, fmt.Errorf("data unavailable: %s", quoteResp.Data.UnFetched[0].Message)
	}
	return 0, 0, fmt.Errorf("no data returned for %s", symbol)
}

// FetchMarketDataBatch fetches price and volume for multiple symbols in a single API call.
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

	exchangeTokens := make(map[string][]string)
	tokenToSymbol := make(map[string]string)
	for _, symbol := range symbols {
		inst, err := a.instruments.GetInstrument(symbol)
		if err != nil {
			continue
		}
		exchangeTokens[inst.ExchSeg] = append(exchangeTokens[inst.ExchSeg], inst.Token)
		tokenToSymbol[inst.Token] = symbol
	}
	if len(exchangeTokens) == 0 {
		return prices, volumes
	}

	payload, err := json.Marshal(angelQuoteRequest{Mode: "FULL", ExchangeTokens: exchangeTokens})
	if err != nil {
		log.Printf("⚠️ Failed to marshal batch request: %v", err)
		return prices, volumes
	}

	body, err := a.doAPIRequest(http.MethodPost, angelLTPPath, payload)
	if err != nil {
		log.Printf("⚠️ Batch quote request failed: %v", err)
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

	now := time.Now()
	a.mu.Lock()
	for _, d := range quoteResp.Data.Fetched {
		symbol := tokenToSymbol[d.SymbolToken]
		if symbol == "" {
			symbol = d.TradingSymbol
		}
		if d.LTP > 0 {
			prices[symbol] = d.LTP
			a.marketPrices[symbol] = d.LTP
			a.priceUpdated[symbol] = now
		}
		if vol := parseTradeVolume(d.TradeVolume); vol > 0 {
			volumes[symbol] = vol
			a.marketVolumes[symbol] = vol
		}
	}
	a.mu.Unlock()

	log.Printf("📊 Batch fetched: %d prices, %d volumes from %d symbols", len(prices), len(volumes), len(symbols))
	return prices, volumes
}

// ---------------------------------------------------------------------------
// PlaceOrder (CR-5, CR-6, EX-2, EX-5)
// ---------------------------------------------------------------------------

// PlaceOrder places a MARKET order and waits (outside any lock) for the order
// book to report a terminal state.
//
//   - order.Intent == IntentReduceOnly bypasses the circuit breaker entirely.
//   - FilledOrder.Quantity is the broker-reported filled quantity, never the
//     requested quantity (CR-6).
//   - If the poll budget is exhausted before a terminal status is seen, this
//     returns an error satisfying errors.Is(err, ErrOrderIndeterminate) and
//     NEVER fabricates a fill (CR-5) — no LTP fallback, no 1.0 fallback.
func (a *AngelBroker) PlaceOrder(order Order) (*FilledOrder, error) {
	a.mu.RLock()
	connected := a.connected
	circuitOpen := time.Now().Before(a.circuitOpenUntil)
	circuitUntil := a.circuitOpenUntil
	failures := a.consecutiveFailures
	a.mu.RUnlock()

	if !connected {
		return nil, fmt.Errorf("not connected to broker")
	}
	if circuitOpen && !order.ReduceOnly() {
		return nil, fmt.Errorf("%w: paused until %s (%d consecutive failures)", ErrCircuitOpen,
			circuitUntil.Format("15:04:05"), failures)
	}

	// Rate limit OUTSIDE the lock (EX-5).
	if err := a.orderLimiter.Wait(context.Background()); err != nil {
		return nil, fmt.Errorf("rate limiter: %w", err)
	}

	inst, err := a.instruments.GetInstrument(order.Symbol)
	if err != nil {
		return nil, fmt.Errorf("invalid symbol %s: %w", order.Symbol, err)
	}

	exchange := inst.ExchSeg
	productType := "INTRADAY"
	if exchange == "MCX" || exchange == "NFO" {
		productType = "CARRYFORWARD"
	}

	angelOrder := angelOrderRequest{
		Variety:         "NORMAL",
		TradingSymbol:   inst.Symbol,
		SymbolToken:     inst.Token,
		TransactionType: string(order.Side),
		Exchange:        exchange,
		OrderType:       string(Market),
		ProductType:     productType,
		Duration:        "DAY",
		Price:           "0",
		SquareOff:       "0",
		StopLoss:        "0",
		TriggerPrice:    "0",
		Quantity:        fmt.Sprintf("%d", order.Quantity),
	}

	log.Printf("🔄 Placing order: %s %s %d (intent=%s, token=%s, exch=%s)",
		order.Side, order.Symbol, order.Quantity, order.Intent, inst.Token, exchange)

	orderID, err := a.submitAngelOrder(angelOrder)
	if err != nil {
		a.recordOrderFailure()
		return nil, fmt.Errorf("order placement failed: %w", err)
	}
	a.recordOrderSuccess()
	log.Printf("✅ Order accepted: OrderID=%s", orderID)

	// Poll for terminal state OUTSIDE the lock.
	status, fillPrice, fillQty, terminal := a.pollOrderFill(orderID)

	switch {
	case status == "rejected":
		return nil, fmt.Errorf("order %s REJECTED by exchange", orderID)

	case status == "cancelled" && fillQty <= 0:
		return nil, fmt.Errorf("order %s CANCELLED with no fill", orderID)

	case terminal && fillQty > 0 && fillPrice > 0:
		// status is "complete", or "cancelled" with a partial fill before cancel.
		if fillQty < order.Quantity {
			log.Printf("⚠️ Partial fill on %s: requested %d, filled %d", orderID, order.Quantity, fillQty)
		}

		a.mu.RLock()
		ltp := a.marketPrices[order.Symbol]
		a.mu.RUnlock()
		slippage := 0.0
		if ltp > 0 {
			if order.Side == Buy {
				slippage = fillPrice - ltp
			} else {
				slippage = ltp - fillPrice
			}
		}

		filled := &FilledOrder{
			OrderID:        orderID,
			Symbol:         order.Symbol,
			Quantity:       fillQty, // CR-6: actual filled qty
			RequestedQty:   order.Quantity,
			Side:           order.Side,
			FillPrice:      fillPrice,
			Slippage:       slippage,
			TransactionFee: min(fillPrice*float64(fillQty)*0.001, 20.0),
			Timestamp:      time.Now(),
			Status:         status,
		}

		a.mu.Lock()
		a.updatePosition(order.Symbol, fillQty, fillPrice, order.Side)
		a.mu.Unlock()

		log.Printf("📋 Order Summary: %s %s %d/%d @ ₹%.2f | Slippage: ₹%.2f | Status: %s",
			order.Side, order.Symbol, fillQty, order.Quantity, fillPrice, slippage, status)
		return filled, nil

	default:
		// CR-5: budget exhausted (or a terminal status arrived with unusable
		// price/qty data) — never fabricate a fill.
		log.Printf("🟡 Order %s INDETERMINATE: last status=%q fillQty=%d", orderID, status, fillQty)
		return nil, &IndeterminateOrderError{OrderID: orderID, Symbol: order.Symbol, LastStatus: status}
	}
}

// terminalOrderStatuses are the statuses at which polling stops.
var terminalOrderStatuses = map[string]bool{
	"complete":  true,
	"rejected":  true,
	"cancelled": true,
}

// Poll budget — vars (not consts) so tests can shrink them.
var (
	fillPollAttempts    = 10
	fillPollInitialWait = 500 * time.Millisecond
	fillPollMaxWait     = 5 * time.Second
)

// pollOrderFill polls the order book until a terminal status or budget
// exhaustion. Never called with a.mu held.
func (a *AngelBroker) pollOrderFill(orderID string) (status string, fillPrice float64, fillQty int, terminal bool) {
	wait := fillPollInitialWait
	for attempt := 1; attempt <= fillPollAttempts; attempt++ {
		time.Sleep(wait)

		s, price, qty, err := a.fetchOrderStatus(orderID)
		if err != nil {
			log.Printf("⚠️ Order status check %d/%d failed: %v", attempt, fillPollAttempts, err)
			wait = min(wait*2, fillPollMaxWait)
			continue
		}
		status, fillPrice, fillQty = s, price, qty

		if terminalOrderStatuses[s] {
			return status, fillPrice, fillQty, true
		}
		log.Printf("⏳ Order %s pending (status=%q, attempt %d/%d)", orderID, s, attempt, fillPollAttempts)
		wait = min(wait*2, fillPollMaxWait)
	}
	return status, fillPrice, fillQty, false
}

func (a *AngelBroker) recordOrderFailure() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.consecutiveFailures++
	if a.consecutiveFailures >= 5 {
		a.circuitOpenUntil = time.Now().Add(30 * time.Second)
		log.Printf("🔴 Circuit breaker TRIPPED: %d consecutive failures. Entries paused 30s (reduce-only orders unaffected).", a.consecutiveFailures)
	}
}

func (a *AngelBroker) recordOrderSuccess() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.consecutiveFailures = 0
}

// submitAngelOrder posts an order and returns the broker order ID.
func (a *AngelBroker) submitAngelOrder(angelOrder angelOrderRequest) (string, error) {
	payload, err := json.Marshal(angelOrder)
	if err != nil {
		return "", fmt.Errorf("failed to marshal order: %w", err)
	}
	body, err := a.doAPIRequest(http.MethodPost, angelOrderPath, payload)
	if err != nil {
		return "", err
	}
	var orderResp angelOrderResponse
	if err := json.Unmarshal(body, &orderResp); err != nil {
		return "", fmt.Errorf("failed to parse order response: %w", err)
	}
	if !orderResp.Status {
		return "", fmt.Errorf("%s", orderResp.Message)
	}
	a.mu.Lock()
	a.orderVariety[orderResp.Data.OrderID] = angelOrder.Variety
	a.mu.Unlock()
	return orderResp.Data.OrderID, nil
}

// fetchOrderStatus polls the order book for one order's current state (A-7).
func (a *AngelBroker) fetchOrderStatus(orderID string) (status string, avgPrice float64, filledQty int, err error) {
	body, err := a.doAPIRequest(http.MethodGet, angelOrderBookPath, nil)
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

	for _, o := range orderBookResp.Data {
		if o.OrderID != orderID {
			continue
		}
		price, _ := o.AveragePrice.Float64()
		qty, _ := o.FilledShares.Int64()
		return strings.ToLower(strings.TrimSpace(o.Status)), price, int(qty), nil
	}
	return "", 0, 0, fmt.Errorf("order %s not found in order book", orderID)
}

// ---------------------------------------------------------------------------
// Stop-loss orders & cancellation (task 11)
// ---------------------------------------------------------------------------

// PlaceStopLossOrder places a broker-side STOPLOSS_MARKET order (variety
// STOPLOSS). It does not poll for a fill — SL orders rest at the exchange
// until triggered. Always bypasses the circuit breaker: a protective stop
// must never be blocked by unrelated entry-side failures (EX-2).
func (a *AngelBroker) PlaceStopLossOrder(symbol string, qty int, triggerPrice float64, side OrderSide) (string, error) {
	a.mu.RLock()
	connected := a.connected
	a.mu.RUnlock()
	if !connected {
		return "", fmt.Errorf("not connected to broker")
	}
	if qty <= 0 {
		return "", fmt.Errorf("invalid quantity %d", qty)
	}
	if triggerPrice <= 0 {
		return "", fmt.Errorf("invalid trigger price %.2f", triggerPrice)
	}

	if err := a.orderLimiter.Wait(context.Background()); err != nil {
		return "", fmt.Errorf("rate limiter: %w", err)
	}

	inst, err := a.instruments.GetInstrument(symbol)
	if err != nil {
		return "", fmt.Errorf("invalid symbol %s: %w", symbol, err)
	}

	exchange := inst.ExchSeg
	productType := "INTRADAY"
	if exchange == "MCX" || exchange == "NFO" {
		productType = "CARRYFORWARD"
	}

	angelOrder := angelOrderRequest{
		Variety:         "STOPLOSS",
		TradingSymbol:   inst.Symbol,
		SymbolToken:     inst.Token,
		TransactionType: string(side),
		Exchange:        exchange,
		OrderType:       string(StopLossMarket),
		ProductType:     productType,
		Duration:        "DAY",
		Price:           "0",
		SquareOff:       "0",
		StopLoss:        "0",
		TriggerPrice:    fmt.Sprintf("%.2f", triggerPrice),
		Quantity:        fmt.Sprintf("%d", qty),
	}

	orderID, err := a.submitAngelOrder(angelOrder)
	if err != nil {
		return "", fmt.Errorf("stop-loss order failed: %w", err)
	}
	log.Printf("🛡️ Stop-loss placed: %s %s %d @ trigger ₹%.2f (OrderID=%s)", side, symbol, qty, triggerPrice, orderID)
	return orderID, nil
}

// CancelOrder cancels an open order by broker order ID (A-3). Looks up the
// variety recorded when the order was placed; defaults to NORMAL if unknown.
func (a *AngelBroker) CancelOrder(orderID string) error {
	if orderID == "" {
		return fmt.Errorf("orderID required")
	}
	a.mu.RLock()
	variety := a.orderVariety[orderID]
	a.mu.RUnlock()
	if variety == "" {
		variety = "NORMAL"
	}

	payload, err := json.Marshal(struct {
		Variety string `json:"variety"`
		OrderID string `json:"orderid"`
	}{Variety: variety, OrderID: orderID})
	if err != nil {
		return fmt.Errorf("failed to marshal cancel request: %w", err)
	}

	body, err := a.doAPIRequest(http.MethodPost, angelCancelPath, payload)
	if err != nil {
		return fmt.Errorf("cancel order failed: %w", err)
	}
	var cancelResp angelOrderResponse
	if err := json.Unmarshal(body, &cancelResp); err != nil {
		return fmt.Errorf("failed to parse cancel response: %w", err)
	}
	if !cancelResp.Status {
		return fmt.Errorf("cancel rejected: %s", cancelResp.Message)
	}
	log.Printf("🛑 Order %s cancelled", orderID)
	return nil
}

// ---------------------------------------------------------------------------
// Balance, positions, prices
// ---------------------------------------------------------------------------

// GetBalance returns the cached account balance.
func (a *AngelBroker) GetBalance() float64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.balance
}

// RefreshBalance fetches real available cash from the RMS funds endpoint (A-5)
// and updates the cache (EX-7 — replaces the hardcoded 10000.0 placeholder).
func (a *AngelBroker) RefreshBalance() (float64, error) {
	body, err := a.doAPIRequest(http.MethodGet, angelRMSPath, nil)
	if err != nil {
		return 0, fmt.Errorf("RMS fetch failed: %w", err)
	}
	var rmsResp angelRMSResponse
	if err := json.Unmarshal(body, &rmsResp); err != nil {
		return 0, fmt.Errorf("failed to parse RMS response: %w", err)
	}
	if !rmsResp.Status {
		return 0, fmt.Errorf("RMS API error: %s", rmsResp.Message)
	}
	cash, err := strconv.ParseFloat(strings.TrimSpace(rmsResp.Data.AvailableCash), 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse available cash %q: %w", rmsResp.Data.AvailableCash, err)
	}
	a.mu.Lock()
	a.balance = cash
	a.mu.Unlock()
	log.Printf("💰 Account Balance: ₹%.2f", cash)
	return cash, nil
}

// GetRequiredMargin returns the total margin (in rupees) required for a
// basket of orders via Angel's margin-calculator batch endpoint (A-6, G-1).
// All legs are sent in a single request so hedged-margin benefit across a
// multi-leg basket (e.g. iron_fly's 4 legs) is computed together, not
// per-leg. Fail-closed: any resolution failure, HTTP error, non-200 status,
// malformed JSON, status:false, or a missing/non-positive total margin
// returns an error. Never returns a guessed number.
func (a *AngelBroker) GetRequiredMargin(orders []MarginOrderInput) (float64, error) {
	if len(orders) == 0 {
		return 0, fmt.Errorf("GetRequiredMargin: no orders supplied")
	}
	if len(orders) > maxMarginBasketLegs {
		return 0, fmt.Errorf("GetRequiredMargin: %d legs exceeds Angel's %d-position batch limit", len(orders), maxMarginBasketLegs)
	}

	a.mu.RLock()
	connected := a.connected
	a.mu.RUnlock()
	if !connected {
		return 0, fmt.Errorf("not connected to broker")
	}

	positions := make([]angelMarginPositionReq, 0, len(orders))
	for i, o := range orders {
		if o.Quantity <= 0 {
			return 0, fmt.Errorf("GetRequiredMargin: leg %d (%s) has non-positive quantity %d", i, o.Symbol, o.Quantity)
		}
		if o.TransactionType != Buy && o.TransactionType != Sell {
			return 0, fmt.Errorf("GetRequiredMargin: leg %d (%s) has invalid transaction type %q", i, o.Symbol, o.TransactionType)
		}

		token := o.Token
		exchange := o.Exchange
		if token == "" || exchange == "" {
			inst, err := a.instruments.GetInstrument(o.Symbol)
			if err != nil {
				return 0, fmt.Errorf("GetRequiredMargin: leg %d: %w", i, err)
			}
			if token == "" {
				token = inst.Token
			}
			if exchange == "" {
				exchange = inst.ExchSeg
			}
		}
		if token == "" || exchange == "" {
			return 0, fmt.Errorf("GetRequiredMargin: leg %d (%s): could not resolve token/exchange", i, o.Symbol)
		}

		productType := o.ProductType
		if productType == "" {
			productType = "INTRADAY"
			if exchange == "MCX" || exchange == "NFO" {
				productType = "CARRYFORWARD"
			}
		}

		positions = append(positions, angelMarginPositionReq{
			Exchange:    exchange,
			Qty:         o.Quantity,
			Price:       o.Price,
			ProductType: productType,
			Token:       token,
			TradeType:   string(o.TransactionType),
		})
	}

	payload, err := json.Marshal(angelMarginRequest{Positions: positions})
	if err != nil {
		return 0, fmt.Errorf("GetRequiredMargin: failed to marshal request: %w", err)
	}

	body, err := a.doAPIRequest(http.MethodPost, angelMarginPath, payload)
	if err != nil {
		return 0, fmt.Errorf("GetRequiredMargin: request failed: %w", err)
	}

	var marginResp angelMarginResponse
	if err := json.Unmarshal(body, &marginResp); err != nil {
		return 0, fmt.Errorf("GetRequiredMargin: malformed response (body: %s): %w", truncateForLog(body), err)
	}
	if !marginResp.Status {
		return 0, fmt.Errorf("GetRequiredMargin: margin API error: %s (%s)", marginResp.Message, marginResp.ErrorCode)
	}
	if marginResp.Data == nil {
		return 0, fmt.Errorf("GetRequiredMargin: margin API returned no data (message=%q)", marginResp.Message)
	}
	if marginResp.Data.TotalMarginRequired <= 0 {
		return 0, fmt.Errorf("GetRequiredMargin: ambiguous margin response: non-positive total margin %.2f", marginResp.Data.TotalMarginRequired)
	}

	return marginResp.Data.TotalMarginRequired, nil
}

// applyTick updates the shared price cache from a WS tick, through the SAME
// priceUpdated staleness mechanism REST polling uses (FetchMarketDataBatch,
// GetCurrentPrice), so GetCurrentPriceWithAge reflects a live WS tick's age
// exactly as it would a REST refresh (G-7). Never called with a.mu held by
// the caller; takes the lock itself for the minimal map-write only.
func (a *AngelBroker) applyTick(symbol string, ltp float64, volume float64, hasVolume bool) {
	if ltp <= 0 {
		return
	}
	now := time.Now()
	a.mu.Lock()
	a.marketPrices[symbol] = ltp
	a.priceUpdated[symbol] = now
	if hasVolume && volume > 0 {
		a.marketVolumes[symbol] = volume
	}
	a.mu.Unlock()
}

// GetPositions returns all open positions.
func (a *AngelBroker) GetPositions() map[string]*Position {
	a.mu.RLock()
	defer a.mu.RUnlock()
	positions := make(map[string]*Position, len(a.positions))
	for k, v := range a.positions {
		cp := *v
		positions[k] = &cp
	}
	return positions
}

// GetCurrentPrice returns current market price (cache, or on-demand fetch).
func (a *AngelBroker) GetCurrentPrice(symbol string) float64 {
	a.mu.RLock()
	price, ok := a.marketPrices[symbol]
	connected := a.connected
	a.mu.RUnlock()
	if ok && price > 0 {
		return price
	}
	if connected {
		if p, _, err := a.fetchMarketData(symbol); err == nil && p > 0 {
			a.mu.Lock()
			a.marketPrices[symbol] = p
			a.priceUpdated[symbol] = time.Now()
			a.mu.Unlock()
			return p
		}
	}
	return 0.0
}

// GetCurrentPriceWithAge returns the cached price and its age (CR-14/EX-6
// staleness support). Returns ErrNoPrice if never priced.
func (a *AngelBroker) GetCurrentPriceWithAge(symbol string) (float64, time.Duration, error) {
	a.mu.RLock()
	price, ok := a.marketPrices[symbol]
	ts, tsOk := a.priceUpdated[symbol]
	connected := a.connected
	a.mu.RUnlock()

	if ok && price > 0 && tsOk {
		return price, time.Since(ts), nil
	}

	if connected {
		if p, _, err := a.fetchMarketData(symbol); err == nil && p > 0 {
			now := time.Now()
			a.mu.Lock()
			a.marketPrices[symbol] = p
			a.priceUpdated[symbol] = now
			a.mu.Unlock()
			return p, 0, nil
		}
	}
	return 0, 0, fmt.Errorf("%w: %s", ErrNoPrice, symbol)
}

// GetCurrentVolume returns current trading volume (cache, or on-demand fetch).
func (a *AngelBroker) GetCurrentVolume(symbol string) float64 {
	a.mu.RLock()
	vol, ok := a.marketVolumes[symbol]
	connected := a.connected
	a.mu.RUnlock()
	if ok && vol > 0 {
		return vol
	}
	if connected {
		if p, v, err := a.fetchMarketData(symbol); err == nil {
			a.mu.Lock()
			if p > 0 {
				a.marketPrices[symbol] = p
				a.priceUpdated[symbol] = time.Now()
			}
			if v > 0 {
				a.marketVolumes[symbol] = v
			}
			a.mu.Unlock()
			return v
		}
	}
	return 0.0
}

// Healthy reports whether the session is usable.
func (a *AngelBroker) Healthy() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.connected && a.healthy
}

// HealthError returns the last error that made the broker unhealthy, if any.
func (a *AngelBroker) HealthError() error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.healthErr
}

// Close closes the connection, including stopping the WS live feed (if any).
func (a *AngelBroker) Close() error {
	a.mu.Lock()
	feed := a.liveFeed
	a.connected = false
	a.mu.Unlock()

	if feed != nil {
		feed.stop()
	}

	fmt.Println("🔌 Disconnected from Angel One")
	return nil
}

// ---------------------------------------------------------------------------
// Session refresh (EX-6)
// ---------------------------------------------------------------------------

// refreshSession tries the A-4 token-refresh endpoint first, then falls back
// to one full TOTP re-login (A-1). On total failure, caller marks unhealthy.
func (a *AngelBroker) refreshSession() error {
	if err := a.refreshTokens(); err == nil {
		return nil
	} else {
		log.Printf("⚠️ Token refresh failed, attempting TOTP re-login: %v", err)
	}

	loginResp, err := a.login()
	if err != nil {
		return fmt.Errorf("refresh and TOTP re-login both failed: %w", err)
	}

	a.mu.Lock()
	a.accessToken = loginResp.Data.JWTToken
	a.refreshToken = loginResp.Data.RefreshToken
	a.feedToken = loginResp.Data.FeedToken
	a.healthy = true
	a.healthErr = nil
	a.mu.Unlock()

	log.Printf("🔄 Session recovered via TOTP re-login")
	return nil
}

// refreshTokens calls endpoint A-4 with the stored refresh token.
func (a *AngelBroker) refreshTokens() error {
	a.mu.RLock()
	refreshToken := a.refreshToken
	a.mu.RUnlock()
	if refreshToken == "" {
		return fmt.Errorf("no refresh token available")
	}

	payload, err := json.Marshal(angelRefreshRequest{RefreshToken: refreshToken})
	if err != nil {
		return err
	}

	body, status, env, err := a.rawRequest(http.MethodPost, angelRefreshPath, payload)
	if err != nil {
		return fmt.Errorf("refresh request failed: %w", err)
	}
	if status != http.StatusOK || !env.Status {
		return fmt.Errorf("refresh rejected (status=%d, ok=%v, msg=%s)", status, env.Status, env.Message)
	}

	var refreshResp struct {
		Data struct {
			JWTToken     string `json:"jwtToken"`
			RefreshToken string `json:"refreshToken"`
			FeedToken    string `json:"feedToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &refreshResp); err != nil {
		return fmt.Errorf("failed to parse refresh response: %w", err)
	}
	if refreshResp.Data.JWTToken == "" {
		return fmt.Errorf("refresh response missing token")
	}

	a.mu.Lock()
	a.accessToken = refreshResp.Data.JWTToken
	if refreshResp.Data.RefreshToken != "" {
		a.refreshToken = refreshResp.Data.RefreshToken
	}
	if refreshResp.Data.FeedToken != "" {
		a.feedToken = refreshResp.Data.FeedToken
	}
	a.healthy = true
	a.healthErr = nil
	a.mu.Unlock()

	log.Printf("🔄 Session token refreshed")
	return nil
}

// ---------------------------------------------------------------------------
// Low-level HTTP plumbing: status checks, WAF detection, auth-failure retry
// (EX-6, EX-9)
// ---------------------------------------------------------------------------

type angelEnvelope struct {
	Status    bool   `json:"status"`
	Message   string `json:"message"`
	ErrorCode string `json:"errorcode"`
}

// isAuthErrorCode matches Angel's documented auth-failure error code prefixes
// (remediation plan Appendix A: "AG"/"AB").
func isAuthErrorCode(code string) bool {
	code = strings.ToUpper(strings.TrimSpace(code))
	return code != "" && (strings.HasPrefix(code, "AG") || strings.HasPrefix(code, "AB"))
}

func authFailure(status int, env angelEnvelope) bool {
	return status == http.StatusUnauthorized || (!env.Status && isAuthErrorCode(env.ErrorCode))
}

// isWAFBlocked heuristically detects an HTML block page in place of JSON.
func isWAFBlocked(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	sample := body
	if len(sample) > 500 {
		sample = sample[:500]
	}
	s := string(sample)
	upper := strings.ToUpper(s)
	return strings.Contains(upper, "<!DOCTYPE") ||
		strings.Contains(upper, "<HTML") ||
		strings.Contains(s, "Request Rejected") ||
		strings.Contains(upper, "ACCESS DENIED") ||
		strings.Contains(s, "exceeding access rate")
}

func truncateForLog(body []byte) string {
	s := string(body)
	if len(s) > 300 {
		return s[:300] + "...(truncated)"
	}
	return s
}

func parseTradeVolume(n json.Number) float64 {
	if v, err := n.Int64(); err == nil {
		return float64(v)
	}
	if v, err := n.Float64(); err == nil {
		return v
	}
	return 0
}

// authHeaders sets the standard Angel One headers, reading credentials under
// RLock (they are otherwise immutable after Connect except token refresh).
func (a *AngelBroker) authHeaders(req *http.Request) {
	a.mu.RLock()
	apiKey := a.apiKey
	token := a.accessToken
	a.mu.RUnlock()

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-UserType", "USER")
	req.Header.Set("X-SourceID", "WEB")
	req.Header.Set("X-ClientLocalIP", "127.0.0.1")
	req.Header.Set("X-ClientPublicIP", "127.0.0.1")
	req.Header.Set("X-MACAddress", "00:00:00:00:00:00")
	req.Header.Set("X-PrivateKey", apiKey)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// rawRequest performs one HTTP round-trip. No retry, no lock held during I/O.
func (a *AngelBroker) rawRequest(method, path string, payload []byte) ([]byte, int, angelEnvelope, error) {
	var env angelEnvelope
	var reqBody io.Reader
	if payload != nil {
		reqBody = bytes.NewReader(payload)
	}

	req, err := http.NewRequest(method, a.baseURL+path, reqBody)
	if err != nil {
		return nil, 0, env, err
	}
	a.authHeaders(req)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, 0, env, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, env, err
	}

	_ = json.Unmarshal(body, &env) // best-effort; body may be HTML/empty
	return body, resp.StatusCode, env, nil
}

// doAPIRequest wraps rawRequest with: (1) HTTP status checks, (2) WAF/HTML
// block detection, (3) one transparent session-refresh-and-retry on 401 /
// Angel auth-error codes. This is the single choke point every authenticated
// Angel call goes through (task 9: status checks; task 6: auth refresh).
func (a *AngelBroker) doAPIRequest(method, path string, payload []byte) ([]byte, error) {
	body, status, env, err := a.rawRequest(method, path, payload)
	if err != nil {
		return nil, fmt.Errorf("request to %s failed: %w", path, err)
	}

	if authFailure(status, env) {
		if refreshErr := a.refreshSession(); refreshErr != nil {
			a.mu.Lock()
			a.healthy = false
			a.healthErr = fmt.Errorf("%w: %v (TOTP re-login required)", ErrAuthFailed, refreshErr)
			a.mu.Unlock()
			return nil, ErrAuthFailed
		}
		body, status, env, err = a.rawRequest(method, path, payload)
		if err != nil {
			return nil, fmt.Errorf("retry after session refresh failed: %w", err)
		}
		if authFailure(status, env) {
			a.mu.Lock()
			a.healthy = false
			a.healthErr = ErrAuthFailed
			a.mu.Unlock()
			return nil, ErrAuthFailed
		}
	}

	if isWAFBlocked(body) {
		return nil, fmt.Errorf("%w: %s", ErrWAFBlocked, truncateForLog(body))
	}
	if status != http.StatusOK {
		return nil, &HTTPStatusError{StatusCode: status, Path: path, Body: truncateForLog(body)}
	}

	a.mu.Lock()
	a.healthy = true
	a.healthErr = nil
	a.mu.Unlock()

	return body, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (a *AngelBroker) generateTOTP() (string, error) {
	if a.totpSecret == "" {
		return "", fmt.Errorf("TOTP secret not provided in config")
	}
	return totp.GenerateCode(a.totpSecret, time.Now())
}

// updatePosition applies a fill to the position book. Caller MUST hold a.mu
// (write lock).
//
// EX-9 sign-crossover fix: an opposite-side fill larger than the existing
// position flips through zero into a NEW position on the opposite side with
// the remainder quantity, instead of merely deleting the old one.
func (a *AngelBroker) updatePosition(symbol string, quantity int, price float64, side OrderSide) {
	pos, exists := a.positions[symbol]

	if !exists {
		if quantity <= 0 {
			return
		}
		a.positions[symbol] = &Position{Symbol: symbol, Quantity: quantity, AveragePrice: price, Side: side}
		return
	}

	if side == pos.Side {
		totalValue := (pos.AveragePrice * float64(pos.Quantity)) + (price * float64(quantity))
		pos.Quantity += quantity
		if pos.Quantity > 0 {
			pos.AveragePrice = totalValue / float64(pos.Quantity)
		}
		return
	}

	switch {
	case quantity < pos.Quantity:
		pos.Quantity -= quantity
	case quantity == pos.Quantity:
		delete(a.positions, symbol)
	default: // quantity > pos.Quantity: flips through zero
		remainder := quantity - pos.Quantity
		a.positions[symbol] = &Position{Symbol: symbol, Quantity: remainder, AveragePrice: price, Side: side}
	}
}
