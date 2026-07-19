package broker

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var testUpgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

// makeTick builds a binary tick frame matching this package's own wire
// layout (tickOff* constants in ws_feed.go) — self-consistent with the
// client parser under test, per the verification flag in ws_feed.go.
func makeTick(token string, ltpRupees float64, volume int64) []byte {
	buf := make([]byte, tickMinLenQuote)
	buf[tickOffMode] = byte(wsModeQuote)
	buf[tickOffExchange] = 2 // NFO
	copy(buf[tickOffToken:tickOffToken+tickTokenLen], []byte(token))
	binary.LittleEndian.PutUint64(buf[tickOffSeq:tickOffSeq+8], 1)
	binary.LittleEndian.PutUint64(buf[tickOffTimestamp:tickOffTimestamp+8], uint64(time.Now().UnixMilli()))
	binary.LittleEndian.PutUint64(buf[tickOffLTP:tickOffLTP+8], uint64(int64(ltpRupees*100)))
	binary.LittleEndian.PutUint64(buf[tickOffVolume:tickOffVolume+8], uint64(volume))
	return buf
}

func wsURLFromHTTP(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func newLiveTestBroker(t *testing.T) *AngelBroker {
	t.Helper()
	b := NewAngelBroker("CLIENT", "1234", "apikey", "")
	b.connected = true
	b.accessToken = "tok"
	b.feedToken = "feed"
	b.instruments.mu.Lock()
	b.instruments.instruments["NIFTY"] = Instrument{Token: "99926000", Symbol: "NIFTY", Name: "NIFTY", ExchSeg: "NFO"}
	b.instruments.mu.Unlock()
	return b
}

func shrinkWSTimings(t *testing.T) {
	t.Helper()
	origInitial, origMax, origHB := wsReconnectInitialDelay, wsReconnectMaxDelay, wsHeartbeatInterval
	wsReconnectInitialDelay = 10 * time.Millisecond
	wsReconnectMaxDelay = 50 * time.Millisecond
	wsHeartbeatInterval = 50 * time.Millisecond
	t.Cleanup(func() {
		wsReconnectInitialDelay, wsReconnectMaxDelay, wsHeartbeatInterval = origInitial, origMax, origHB
	})
}

// ---------------------------------------------------------------------------
// Subscribe + tick updates the SAME price cache GetCurrentPriceWithAge reads
// ---------------------------------------------------------------------------

func TestSubscribeLive_TickUpdatesPriceAge(t *testing.T) {
	shrinkWSTimings(t)

	gotSubscribe := make(chan wsSubscribeRequest, 1)
	tickSent := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var req wsSubscribeRequest
		if json.Unmarshal(msg, &req) == nil {
			gotSubscribe <- req
		}

		_ = conn.WriteMessage(websocket.BinaryMessage, makeTick("99926000", 25000.50, 12345))
		close(tickSent)

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	angelWSURL = wsURLFromHTTP(srv.URL)
	defer func() { angelWSURL = "wss://smartapisocket.angelone.in/smart-stream" }()

	b := newLiveTestBroker(t)

	if err := b.SubscribeLive([]string{"NIFTY"}); err != nil {
		t.Fatalf("SubscribeLive failed: %v", err)
	}
	defer b.Close()

	select {
	case req := <-gotSubscribe:
		if req.Action != wsActionSubscribe {
			t.Errorf("expected subscribe action %d, got %d", wsActionSubscribe, req.Action)
		}
		if req.Params.Mode != wsModeQuote {
			t.Errorf("expected mode %d (quote, for volume), got %d", wsModeQuote, req.Params.Mode)
		}
		if len(req.Params.TokenList) != 1 || len(req.Params.TokenList[0].Tokens) != 1 || req.Params.TokenList[0].Tokens[0] != "99926000" {
			t.Errorf("unexpected token list: %+v", req.Params.TokenList)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never received a subscribe message")
	}

	select {
	case <-tickSent:
	case <-time.After(2 * time.Second):
		t.Fatal("tick was never sent")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		price, age, err := b.GetCurrentPriceWithAge("NIFTY")
		if err == nil && price == 25000.50 {
			if age > time.Second {
				t.Fatalf("expected fresh price age right after a WS tick, got %v", age)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("WS tick never updated the shared price cache (GetCurrentPriceWithAge)")
}

// ---------------------------------------------------------------------------
// Forced disconnect triggers reconnect with backoff
// ---------------------------------------------------------------------------

func TestWSFeed_ForcedDisconnect_Reconnects(t *testing.T) {
	shrinkWSTimings(t)

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			conn.Close() // force an immediate drop -> must trigger a reconnect
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	angelWSURL = wsURLFromHTTP(srv.URL)
	defer func() { angelWSURL = "wss://smartapisocket.angelone.in/smart-stream" }()

	b := newLiveTestBroker(t)
	if err := b.SubscribeLive([]string{"NIFTY"}); err != nil {
		t.Fatalf("SubscribeLive failed: %v", err)
	}
	defer b.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&attempts) >= 2 {
			return // reconnected after the forced drop
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected >=2 connection attempts (reconnect after forced disconnect), got %d", atomic.LoadInt32(&attempts))
}

// ---------------------------------------------------------------------------
// WS never connects -> REST polling still returns prices (fallback proof)
// ---------------------------------------------------------------------------

func TestWSFeed_NeverConnects_RESTFallbackStillWorks(t *testing.T) {
	shrinkWSTimings(t)

	restMux := http.NewServeMux()
	restMux.HandleFunc(angelLTPPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"status":true,"message":"SUCCESS","data":{"fetched":[{"tradingSymbol":"NIFTY","symbolToken":"99926000","ltp":25000.75,"tradeVolume":"1000"}],"unfetched":[]}}`)
	})
	restSrv := httptest.NewServer(restMux)
	defer restSrv.Close()

	// Plain HTTP 404 handler: the WS handshake (upgrade) always fails here,
	// simulating "feed down" / never connects.
	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer deadSrv.Close()

	angelWSURL = wsURLFromHTTP(deadSrv.URL)
	defer func() { angelWSURL = "wss://smartapisocket.angelone.in/smart-stream" }()

	b := newLiveTestBroker(t)
	b.baseURL = restSrv.URL
	b.httpClient = restSrv.Client()

	if err := b.SubscribeLive([]string{"NIFTY"}); err != nil {
		t.Fatalf("SubscribeLive failed: %v", err)
	}
	defer b.Close()

	// Let the feed fail-and-retry a few times in the background; it must not
	// block or crash the broker while doing so.
	time.Sleep(150 * time.Millisecond)

	prices, _ := b.FetchMarketDataBatch([]string{"NIFTY"})
	if prices["NIFTY"] != 25000.75 {
		t.Fatalf("expected REST polling to still return the price while the WS feed is down, got %+v", prices)
	}
}

// ---------------------------------------------------------------------------
// UnsubscribeLive is a safe no-op when the feed was never started
// ---------------------------------------------------------------------------

func TestUnsubscribeLive_NeverStarted_NoOp(t *testing.T) {
	b := NewAngelBroker("C", "P", "K", "")
	if err := b.UnsubscribeLive([]string{"NIFTY"}); err != nil {
		t.Fatalf("expected nil error when feed was never started, got %v", err)
	}
}

func TestSubscribeLive_NotConnected_ReturnsError(t *testing.T) {
	b := NewAngelBroker("C", "P", "K", "")
	if err := b.SubscribeLive([]string{"NIFTY"}); err == nil {
		t.Fatal("expected error when broker is not connected")
	}
}
