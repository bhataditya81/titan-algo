package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------

// newTestBroker builds an AngelBroker wired to a local httptest server, with
// a fake NIFTY instrument pre-registered and an already-established session
// (so tests exercise PlaceOrder/etc. without going through login).
func newTestBroker(t *testing.T, handler http.Handler) (*AngelBroker, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	b := NewAngelBroker("CLIENT", "1234", "apikey", "")
	b.baseURL = srv.URL
	b.httpClient = srv.Client()
	b.connected = true
	b.healthy = true
	b.accessToken = "initial-token"
	b.refreshToken = "initial-refresh"

	b.instruments.mu.Lock()
	b.instruments.instruments["NIFTY"] = Instrument{
		Token: "99926000", Symbol: "NIFTY", Name: "NIFTY", ExchSeg: "NSE", LotSize: "75", LotSizeInt: 75,
	}
	b.instruments.mu.Unlock()

	// Speed up fill polling for tests.
	fillPollAttempts = 3
	fillPollInitialWait = 5 * time.Millisecond
	fillPollMaxWait = 20 * time.Millisecond
	t.Cleanup(func() {
		fillPollAttempts = 10
		fillPollInitialWait = 500 * time.Millisecond
		fillPollMaxWait = 5 * time.Second
	})

	return b, srv
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func orderPlaceOK(orderID string) string {
	return fmt.Sprintf(`{"status":true,"message":"SUCCESS","errorcode":"","data":{"orderid":%q}}`, orderID)
}

func orderBookOK(orderID, status, avgPrice, filledShares string) string {
	return fmt.Sprintf(`{"status":true,"message":"SUCCESS","data":[{"orderid":%q,"status":%q,"averageprice":%q,"filledshares":%q,"tradingsymbol":"NIFTY"}]}`,
		orderID, status, avgPrice, filledShares)
}

// ---------------------------------------------------------------------------
// CR-5: indeterminate orders
// ---------------------------------------------------------------------------

func TestPlaceOrder_PendingTimesOut_ReturnsIndeterminate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(angelOrderPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, orderPlaceOK("OID-PENDING"))
	})
	mux.HandleFunc(angelOrderBookPath, func(w http.ResponseWriter, r *http.Request) {
		// Never reaches a terminal status within the poll budget.
		writeJSON(w, http.StatusOK, orderBookOK("OID-PENDING", "open", "0", "0"))
	})

	b, _ := newTestBroker(t, mux)

	filled, err := b.PlaceOrder(Order{Symbol: "NIFTY", Quantity: 75, Side: Buy, OrderType: Market})
	if filled != nil {
		t.Fatalf("expected no FilledOrder on timeout, got %+v", filled)
	}
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrOrderIndeterminate) {
		t.Fatalf("expected errors.Is(err, ErrOrderIndeterminate), got: %v", err)
	}
	var ioe *IndeterminateOrderError
	if !errors.As(err, &ioe) {
		t.Fatalf("expected *IndeterminateOrderError, got %T: %v", err, err)
	}
	if ioe.OrderID != "OID-PENDING" {
		t.Fatalf("expected order ID attached, got %q", ioe.OrderID)
	}

	// Fail-closed: no phantom position was created.
	if pos := b.GetPositions(); len(pos) != 0 {
		t.Fatalf("expected no position created on indeterminate order, got %+v", pos)
	}
}

func TestPlaceOrder_CancelledNoFill_ReturnsErrorNotFill(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(angelOrderPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, orderPlaceOK("OID-CANCEL"))
	})
	mux.HandleFunc(angelOrderBookPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, orderBookOK("OID-CANCEL", "cancelled", "0", "0"))
	})

	b, _ := newTestBroker(t, mux)

	filled, err := b.PlaceOrder(Order{Symbol: "NIFTY", Quantity: 75, Side: Buy, OrderType: Market})
	if filled != nil {
		t.Fatalf("expected no fill for a pure cancel, got %+v", filled)
	}
	if err == nil {
		t.Fatal("expected an error")
	}
}

// ---------------------------------------------------------------------------
// CR-6: partial fills
// ---------------------------------------------------------------------------

func TestPlaceOrder_PartialFill_RecordsActualQty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(angelOrderPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, orderPlaceOK("OID-PARTIAL"))
	})
	mux.HandleFunc(angelOrderBookPath, func(w http.ResponseWriter, r *http.Request) {
		// Broker marks it complete but only 40 of the requested 75 filled.
		writeJSON(w, http.StatusOK, orderBookOK("OID-PARTIAL", "complete", "150.50", "40"))
	})

	b, _ := newTestBroker(t, mux)

	filled, err := b.PlaceOrder(Order{Symbol: "NIFTY", Quantity: 75, Side: Buy, OrderType: Market})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if filled == nil {
		t.Fatal("expected a FilledOrder")
	}
	if filled.Quantity != 40 {
		t.Fatalf("expected FilledOrder.Quantity=40 (actual filled), got %d", filled.Quantity)
	}
	if filled.RequestedQty != 75 {
		t.Fatalf("expected RequestedQty=75, got %d", filled.RequestedQty)
	}
	if !filled.Partial() {
		t.Fatal("expected Partial() == true")
	}

	pos := b.GetPositions()
	p, ok := pos["NIFTY"]
	if !ok {
		t.Fatal("expected a NIFTY position")
	}
	if p.Quantity != 40 {
		t.Fatalf("expected position qty=40 (actual filled, not requested 75), got %d", p.Quantity)
	}
}

// ---------------------------------------------------------------------------
// EX-9: position sign crossover
// ---------------------------------------------------------------------------

func TestUpdatePosition_SignCrossover_FlipsToOppositeSideWithRemainder(t *testing.T) {
	b := NewAngelBroker("C", "P", "K", "")
	b.positions["NIFTY"] = &Position{Symbol: "NIFTY", Quantity: 25, AveragePrice: 100, Side: Buy}

	// Sell 75 against a long 25 -> should NOT just delete; must flip to short 50.
	b.updatePosition("NIFTY", 75, 120, Sell)

	pos, ok := b.positions["NIFTY"]
	if !ok {
		t.Fatal("expected a resulting position, got none (bug: crossover deleted instead of flipping)")
	}
	if pos.Side != Sell {
		t.Fatalf("expected resulting side SELL, got %s", pos.Side)
	}
	if pos.Quantity != 50 {
		t.Fatalf("expected remainder quantity 50 (75-25), got %d", pos.Quantity)
	}
	if pos.AveragePrice != 120 {
		t.Fatalf("expected new position avg price 120 (fill price), got %.2f", pos.AveragePrice)
	}
}

func TestUpdatePosition_ExactClose_Deletes(t *testing.T) {
	b := NewAngelBroker("C", "P", "K", "")
	b.positions["NIFTY"] = &Position{Symbol: "NIFTY", Quantity: 75, AveragePrice: 100, Side: Buy}
	b.updatePosition("NIFTY", 75, 110, Sell)
	if _, ok := b.positions["NIFTY"]; ok {
		t.Fatal("expected position to be closed (deleted) on exact-quantity opposite fill")
	}
}

func TestUpdatePosition_PartialReduce_KeepsSameSide(t *testing.T) {
	b := NewAngelBroker("C", "P", "K", "")
	b.positions["NIFTY"] = &Position{Symbol: "NIFTY", Quantity: 75, AveragePrice: 100, Side: Buy}
	b.updatePosition("NIFTY", 25, 110, Sell)
	pos, ok := b.positions["NIFTY"]
	if !ok {
		t.Fatal("expected position to still exist")
	}
	if pos.Side != Buy || pos.Quantity != 50 {
		t.Fatalf("expected BUY 50 remaining, got %s %d", pos.Side, pos.Quantity)
	}
}

// ---------------------------------------------------------------------------
// EX-9: WAF-blocked position fetch must error, not silently report zero
// ---------------------------------------------------------------------------

func TestFetchRemotePositions_WAFBlocked_ReturnsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(angelPositionsPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html><body>Request Rejected</body></html>"))
	})

	b, _ := newTestBroker(t, mux)

	err := b.fetchRemotePositions()
	if err == nil {
		t.Fatal("expected an error on WAF-blocked position fetch, got nil (would silently report zero positions)")
	}
	if !errors.Is(err, ErrWAFBlocked) {
		t.Fatalf("expected errors.Is(err, ErrWAFBlocked), got: %v", err)
	}
}

func TestFetchRemotePositions_NonOKStatus_ReturnsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(angelPositionsPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, `{"status":false,"message":"server error"}`)
	})

	b, _ := newTestBroker(t, mux)

	err := b.fetchRemotePositions()
	if err == nil {
		t.Fatal("expected an error on HTTP 500 position fetch")
	}
	var hse *HTTPStatusError
	if !errors.As(err, &hse) {
		t.Fatalf("expected *HTTPStatusError in chain, got %T: %v", err, err)
	}
}

// ---------------------------------------------------------------------------
// EX-6: 401 triggers the refresh path
// ---------------------------------------------------------------------------

func TestDoAPIRequest_401_TriggersRefreshThenSucceeds(t *testing.T) {
	var rmsCalls int
	var refreshCalls int
	var mu sync.Mutex

	mux := http.NewServeMux()
	mux.HandleFunc(angelRMSPath, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		rmsCalls++
		call := rmsCalls
		mu.Unlock()

		if call == 1 {
			// First call: session expired.
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"status":false,"message":"expired","errorcode":"AG8001"}`))
			return
		}
		// Second call (post-refresh) succeeds.
		writeJSON(w, http.StatusOK, `{"status":true,"message":"SUCCESS","data":{"availablecash":"54321.50"}}`)
	})
	mux.HandleFunc(angelRefreshPath, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		refreshCalls++
		mu.Unlock()
		writeJSON(w, http.StatusOK, `{"status":true,"message":"SUCCESS","data":{"jwtToken":"new-token","refreshToken":"new-refresh","feedToken":"new-feed"}}`)
	})

	b, _ := newTestBroker(t, mux)

	bal, err := b.RefreshBalance()
	if err != nil {
		t.Fatalf("expected success after transparent refresh, got error: %v", err)
	}
	if bal != 54321.50 {
		t.Fatalf("expected balance 54321.50, got %.2f", bal)
	}

	mu.Lock()
	defer mu.Unlock()
	if refreshCalls != 1 {
		t.Fatalf("expected exactly 1 refresh call, got %d", refreshCalls)
	}
	if rmsCalls != 2 {
		t.Fatalf("expected 2 RMS calls (fail then retry), got %d", rmsCalls)
	}

	b.mu.RLock()
	token := b.accessToken
	b.mu.RUnlock()
	if token != "new-token" {
		t.Fatalf("expected access token updated to new-token, got %q", token)
	}
	if !b.Healthy() {
		t.Fatal("expected broker to be healthy after successful refresh")
	}
}

func TestDoAPIRequest_401_RefreshAndReloginBothFail_MarksUnhealthy(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(angelRMSPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"status":false,"message":"expired","errorcode":"AG8001"}`))
	})
	mux.HandleFunc(angelRefreshPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"status":false,"message":"refresh token invalid","errorcode":"AB1234"}`)
	})
	// No TOTP secret configured -> login() fails fast without hitting the network.

	b, _ := newTestBroker(t, mux)

	_, err := b.RefreshBalance()
	if err == nil {
		t.Fatal("expected error when both refresh and re-login fail")
	}
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("expected errors.Is(err, ErrAuthFailed), got: %v", err)
	}
	if b.Healthy() {
		t.Fatal("expected broker to be unhealthy after failed refresh+relogin")
	}
	if b.HealthError() == nil {
		t.Fatal("expected HealthError() to be set")
	}
}

// ---------------------------------------------------------------------------
// EX-5: lock hygiene — PlaceOrder must not hold the mutex across HTTP calls
// ---------------------------------------------------------------------------

func TestPlaceOrder_DoesNotBlockConcurrentReadsDuringHTTP(t *testing.T) {
	orderStarted := make(chan struct{})
	release := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc(angelOrderPath, func(w http.ResponseWriter, r *http.Request) {
		close(orderStarted)
		<-release // held open until the test says go
		writeJSON(w, http.StatusOK, orderPlaceOK("OID-SLOW"))
	})
	mux.HandleFunc(angelOrderBookPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, orderBookOK("OID-SLOW", "complete", "100", "75"))
	})

	b, _ := newTestBroker(t, mux)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = b.PlaceOrder(Order{Symbol: "NIFTY", Quantity: 75, Side: Buy, OrderType: Market})
	}()

	select {
	case <-orderStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("order HTTP call never started")
	}

	// While the order's HTTP call is blocked in-flight, a read that only
	// needs a.mu.RLock() must NOT be blocked — proves the write lock isn't
	// held across httpClient.Do.
	done := make(chan struct{})
	go func() {
		b.GetBalance()
		b.GetCurrentPrice("NIFTY")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		close(release)
		t.Fatal("GetBalance/GetCurrentPrice blocked while order HTTP call was in flight — lock held across I/O")
	}

	close(release)
	wg.Wait()
}

// ---------------------------------------------------------------------------
// EX-2: reduce-only orders bypass the circuit breaker
// ---------------------------------------------------------------------------

func TestPlaceOrder_ReduceOnly_BypassesOpenCircuitBreaker(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(angelOrderPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, orderPlaceOK("OID-EXIT"))
	})
	mux.HandleFunc(angelOrderBookPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, orderBookOK("OID-EXIT", "complete", "100", "75"))
	})

	b, _ := newTestBroker(t, mux)
	b.mu.Lock()
	b.circuitOpenUntil = time.Now().Add(time.Hour) // wide open
	b.consecutiveFailures = 5
	b.mu.Unlock()

	// A plain entry order must be rejected locally (never reaches the server).
	_, err := b.PlaceOrder(Order{Symbol: "NIFTY", Quantity: 75, Side: Buy, OrderType: Market})
	if err == nil || !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected entry order to be rejected by open circuit breaker, got: %v", err)
	}

	// A reduce-only (exit) order must go through despite the open circuit.
	filled, err := b.PlaceOrder(Order{Symbol: "NIFTY", Quantity: 75, Side: Sell, OrderType: Market, Intent: IntentReduceOnly})
	if err != nil {
		t.Fatalf("expected reduce-only order to bypass circuit breaker, got error: %v", err)
	}
	if filled == nil {
		t.Fatal("expected a fill for the reduce-only order")
	}
}

// ---------------------------------------------------------------------------
// Instrument master helpers (GetLotSize / GetExpiries)
// ---------------------------------------------------------------------------

func TestGetLotSize(t *testing.T) {
	im := NewInstrumentManager()
	im.instruments["NIFTY"] = Instrument{Symbol: "NIFTY", LotSize: "75", LotSizeInt: 75}
	im.instruments["BADLOT"] = Instrument{Symbol: "BADLOT", LotSize: "0"}

	got, err := im.GetLotSize("NIFTY")
	if err != nil || got != 75 {
		t.Fatalf("expected lot size 75, got %d, err=%v", got, err)
	}

	if _, err := im.GetLotSize("BADLOT"); err == nil {
		t.Fatal("expected error for invalid lot size, got nil")
	}
	if _, err := im.GetLotSize("NOPE"); err == nil {
		t.Fatal("expected error for unknown symbol")
	}
}

func TestGetExpiries(t *testing.T) {
	im := NewInstrumentManager()
	im.instruments["NIFTY25JUL2025CE18000"] = Instrument{Symbol: "NIFTY25JUL2025CE18000", Name: "NIFTY", ExchSeg: "NFO", Expiry: "25JUL2025"}
	im.instruments["NIFTY31JUL2025CE18000"] = Instrument{Symbol: "NIFTY31JUL2025CE18000", Name: "NIFTY", ExchSeg: "NFO", Expiry: "31JUL2025"}
	im.instruments["NIFTY25JUL2025PE18000"] = Instrument{Symbol: "NIFTY25JUL2025PE18000", Name: "NIFTY", ExchSeg: "NFO", Expiry: "25JUL2025"} // dup date
	im.instruments["BANKNIFTY07AUG2025CE"] = Instrument{Symbol: "BANKNIFTY07AUG2025CE", Name: "BANKNIFTY", ExchSeg: "NFO", Expiry: "07AUG2025"}

	expiries, err := im.GetExpiries("NIFTY")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(expiries) != 2 {
		t.Fatalf("expected 2 distinct expiries, got %d: %v", len(expiries), expiries)
	}
	if !expiries[0].Before(expiries[1]) {
		t.Fatalf("expected sorted ascending, got %v", expiries)
	}
	if expiries[0].Day() != 25 || expiries[0].Month().String() != "July" {
		t.Fatalf("unexpected first expiry: %v", expiries[0])
	}

	if _, err := im.GetExpiries("NOTREAL"); err == nil {
		t.Fatal("expected error for unknown underlying")
	}
}

// ---------------------------------------------------------------------------
// GetCurrentPriceWithAge
// ---------------------------------------------------------------------------

func TestGetCurrentPriceWithAge(t *testing.T) {
	b := NewAngelBroker("C", "P", "K", "")
	b.connected = false // force cache-only path

	if _, _, err := b.GetCurrentPriceWithAge("NIFTY"); err == nil {
		t.Fatal("expected ErrNoPrice for an unpriced symbol")
	}

	b.mu.Lock()
	b.marketPrices["NIFTY"] = 25000
	b.priceUpdated["NIFTY"] = time.Now().Add(-10 * time.Second)
	b.mu.Unlock()

	price, age, err := b.GetCurrentPriceWithAge("NIFTY")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if price != 25000 {
		t.Fatalf("expected price 25000, got %.2f", price)
	}
	if age < 9*time.Second {
		t.Fatalf("expected age >= ~10s, got %v", age)
	}
}

// ---------------------------------------------------------------------------
// G-1: GetRequiredMargin (margin batch endpoint, A-6)
// ---------------------------------------------------------------------------

func registerIronFlyLegs(b *AngelBroker) {
	b.instruments.mu.Lock()
	defer b.instruments.mu.Unlock()
	b.instruments.instruments["NIFTY24000CE"] = Instrument{Token: "1", Symbol: "NIFTY24000CE", ExchSeg: "NFO"}
	b.instruments.instruments["NIFTY24000PE"] = Instrument{Token: "2", Symbol: "NIFTY24000PE", ExchSeg: "NFO"}
	b.instruments.instruments["NIFTY24500CE"] = Instrument{Token: "3", Symbol: "NIFTY24500CE", ExchSeg: "NFO"}
	b.instruments.instruments["NIFTY23500PE"] = Instrument{Token: "4", Symbol: "NIFTY23500PE", ExchSeg: "NFO"}
}

func TestGetRequiredMargin_HappyPath_MultiLegBasket(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(angelMarginPath, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req angelMarginRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("failed to parse margin request: %v", err)
		}
		if len(req.Positions) != 4 {
			t.Errorf("expected 4 legs (iron_fly basket) in one request, got %d", len(req.Positions))
		}
		for _, p := range req.Positions {
			if p.Token == "" || p.Exchange == "" || p.TradeType == "" || p.ProductType == "" {
				t.Errorf("incomplete leg in margin request: %+v", p)
			}
		}
		writeJSON(w, http.StatusOK, `{"status":true,"message":"SUCCESS","errorcode":"","data":{"totalMarginRequired":18500.25}}`)
	})

	b, _ := newTestBroker(t, mux)
	registerIronFlyLegs(b)

	legs := []MarginOrderInput{
		{Symbol: "NIFTY24000CE", TransactionType: Sell, Quantity: 75},
		{Symbol: "NIFTY24000PE", TransactionType: Sell, Quantity: 75},
		{Symbol: "NIFTY24500CE", TransactionType: Buy, Quantity: 75},
		{Symbol: "NIFTY23500PE", TransactionType: Buy, Quantity: 75},
	}
	margin, err := b.GetRequiredMargin(legs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if margin != 18500.25 {
		t.Fatalf("expected margin 18500.25, got %.2f", margin)
	}
}

func TestGetRequiredMargin_HTTPError_FailsClosed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(angelMarginPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, `{"status":false,"message":"server error"}`)
	})

	b, _ := newTestBroker(t, mux)
	registerIronFlyLegs(b)

	if _, err := b.GetRequiredMargin([]MarginOrderInput{{Symbol: "NIFTY24000CE", TransactionType: Sell, Quantity: 75}}); err == nil {
		t.Fatal("expected error on HTTP 500 — must fail closed, never guess a margin number")
	}
}

func TestGetRequiredMargin_MalformedResponse_FailsClosed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(angelMarginPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `this is not json`)
	})

	b, _ := newTestBroker(t, mux)
	registerIronFlyLegs(b)

	if _, err := b.GetRequiredMargin([]MarginOrderInput{{Symbol: "NIFTY24000CE", TransactionType: Sell, Quantity: 75}}); err == nil {
		t.Fatal("expected error on malformed JSON response")
	}
}

func TestGetRequiredMargin_NullData_FailsClosed(t *testing.T) {
	// Matches Angel's own documented error example: status:false + data:null.
	mux := http.NewServeMux()
	mux.HandleFunc(angelMarginPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"status":false,"message":"Null or Empty Margin Data","errorcode":"AB4022","data":null}`)
	})

	b, _ := newTestBroker(t, mux)
	registerIronFlyLegs(b)

	if _, err := b.GetRequiredMargin([]MarginOrderInput{{Symbol: "NIFTY24000CE", TransactionType: Sell, Quantity: 75}}); err == nil {
		t.Fatal("expected error for status:false/data:null response")
	}
}

func TestGetRequiredMargin_NonPositiveMargin_FailsClosed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(angelMarginPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"status":true,"message":"SUCCESS","errorcode":"","data":{"totalMarginRequired":0}}`)
	})

	b, _ := newTestBroker(t, mux)
	registerIronFlyLegs(b)

	if _, err := b.GetRequiredMargin([]MarginOrderInput{{Symbol: "NIFTY24000CE", TransactionType: Sell, Quantity: 75}}); err == nil {
		t.Fatal("expected error for a non-positive/ambiguous total margin")
	}
}

func TestGetRequiredMargin_NoOrders_ReturnsError(t *testing.T) {
	b := NewAngelBroker("C", "P", "K", "")
	b.connected = true
	if _, err := b.GetRequiredMargin(nil); err == nil {
		t.Fatal("expected error for an empty order basket")
	}
}

func TestGetRequiredMargin_UnresolvableSymbol_ReturnsError(t *testing.T) {
	b, _ := newTestBroker(t, http.NewServeMux())
	if _, err := b.GetRequiredMargin([]MarginOrderInput{{Symbol: "NOT-A-REAL-SYMBOL", TransactionType: Sell, Quantity: 75}}); err == nil {
		t.Fatal("expected error when the symbol can't be resolved to a token/exchange")
	}
}
