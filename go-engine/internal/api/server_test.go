package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const testToken = "test-token-for-unit-tests-only"

// newTestServer builds a Server with a known token and returns it plus an
// httptest.Server wrapping the same mux/middleware stack as Start().
func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := NewServer(0, testToken)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", s.authMiddleware(s.handleStatus))
	mux.HandleFunc("/api/positions", s.authMiddleware(s.handlePositions))
	mux.HandleFunc("/api/trades", s.authMiddleware(s.handleTrades))
	mux.HandleFunc("/api/config", s.authMiddleware(s.handleConfig))
	mux.HandleFunc("/api/start", s.authMiddleware(s.handleStart))
	mux.HandleFunc("/api/stop", s.authMiddleware(s.handleStop))
	mux.HandleFunc("/api/kill", s.authMiddleware(s.handleKill))
	mux.HandleFunc("/ws/live", s.handleWebSocket)

	ts := httptest.NewServer(s.corsMiddleware(mux))
	t.Cleanup(ts.Close)
	return s, ts
}

// --- Token handling ---

func TestNewServerGeneratesTokenWhenEmpty(t *testing.T) {
	s := NewServer(0, "")
	if s.token == "" {
		t.Fatal("expected generated token, got empty")
	}
	if len(s.token) != 64 { // 32 bytes hex-encoded
		t.Fatalf("expected 64-char hex token, got %d chars", len(s.token))
	}
	s2 := NewServer(0, "")
	if s.token == s2.token {
		t.Fatal("two generated tokens are identical — not random")
	}
}

func TestNewServerUsesProvidedToken(t *testing.T) {
	s := NewServer(0, testToken)
	if s.token != testToken {
		t.Fatalf("expected provided token to be used")
	}
}

func TestDefaultBindAddrIsLoopback(t *testing.T) {
	s := NewServer(8080, testToken)
	if s.bindAddr != "127.0.0.1:8080" {
		t.Fatalf("default bind addr = %q, want 127.0.0.1:8080", s.bindAddr)
	}
}

// --- REST auth ---

func TestRESTEndpointsRejectMissingOrBadToken(t *testing.T) {
	_, ts := newTestServer(t)

	endpoints := []struct {
		method, path string
	}{
		{"GET", "/api/status"},
		{"GET", "/api/positions"},
		{"GET", "/api/trades"},
		{"GET", "/api/config"},
		{"POST", "/api/start"},
		{"POST", "/api/stop"},
		{"POST", "/api/kill"},
	}

	for _, ep := range endpoints {
		for _, token := range []string{"", "wrong-token"} {
			req, _ := http.NewRequest(ep.method, ts.URL+ep.path, nil)
			if token != "" {
				req.Header.Set("X-API-Key", token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", ep.method, ep.path, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with token %q: got %d, want 401",
					ep.method, ep.path, token, resp.StatusCode)
			}
		}
	}
}

func TestRESTEndpointAcceptsValidToken(t *testing.T) {
	_, ts := newTestServer(t)

	req, _ := http.NewRequest("GET", ts.URL+"/api/status", nil)
	req.Header.Set("X-API-Key", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid token: got %d, want 200", resp.StatusCode)
	}
}

// --- Control hooks: 503 when unwired, correct hook when wired ---

func TestControlEndpointsReturn503WhenHooksUnset(t *testing.T) {
	_, ts := newTestServer(t)

	for _, path := range []string{"/api/start", "/api/stop", "/api/kill"} {
		req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader("{}"))
		req.Header.Set("X-API-Key", testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]string
		json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("POST %s without hooks: got %d, want 503", path, resp.StatusCode)
		}
		if body["error"] != "not wired" {
			t.Errorf("POST %s without hooks: body %v, want error=not wired", path, body)
		}
	}
}

func TestControlEndpointsCallCorrectHooks(t *testing.T) {
	s, ts := newTestServer(t)

	var pauseCalls, resumeCalls, killCalls int32
	s.SetControlHooks(ControlHooks{
		Pause:          func() error { atomic.AddInt32(&pauseCalls, 1); return nil },
		Resume:         func() error { atomic.AddInt32(&resumeCalls, 1); return nil },
		KillAndFlatten: func() error { atomic.AddInt32(&killCalls, 1); return nil },
		Status:         func() EngineStatus { return EngineStatus{Running: true, Mode: "paper"} },
	})

	post := func(path string) *http.Response {
		req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader("{}"))
		req.Header.Set("X-API-Key", testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	if resp := post("/api/stop"); resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/stop with hooks: got %d, want 200", resp.StatusCode)
	}
	if resp := post("/api/start"); resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/start with hooks: got %d, want 200", resp.StatusCode)
	}
	if resp := post("/api/kill"); resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/kill with hooks: got %d, want 200", resp.StatusCode)
	}

	if n := atomic.LoadInt32(&pauseCalls); n != 1 {
		t.Errorf("Pause called %d times, want 1", n)
	}
	if n := atomic.LoadInt32(&resumeCalls); n != 1 {
		t.Errorf("Resume called %d times, want 1", n)
	}
	if n := atomic.LoadInt32(&killCalls); n != 1 {
		t.Errorf("KillAndFlatten called %d times, want 1", n)
	}
}

func TestControlEndpointHookErrorReturns500(t *testing.T) {
	s, ts := newTestServer(t)
	s.SetControlHooks(ControlHooks{
		Pause: func() error { return errors.New("engine jammed") },
	})

	req, _ := http.NewRequest("POST", ts.URL+"/api/stop", strings.NewReader("{}"))
	req.Header.Set("X-API-Key", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("hook error: got %d, want 500", resp.StatusCode)
	}
}

// --- Config endpoint validation ---

func TestConfigPostValidation(t *testing.T) {
	s, ts := newTestServer(t)
	s.SetConfigHooks(ConfigHooks{
		AllowedStrategies: []string{"sniper", "nine_twenty"},
		MaxSessionBalance: 50000,
	})

	post := func(body string) *http.Response {
		req, _ := http.NewRequest("POST", ts.URL+"/api/config", strings.NewReader(body))
		req.Header.Set("X-API-Key", testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	cases := []struct {
		name string
		body string
		want int
	}{
		{"valid balance", `{"session_balance": 10000}`, 200},
		{"zero balance", `{"session_balance": 0}`, 400},
		{"negative balance", `{"session_balance": -500}`, 400},
		{"over max balance", `{"session_balance": 50001}`, 400},
		{"balance wrong type", `{"session_balance": "lots"}`, 400},
		{"valid strategy", `{"strategy": "nine_twenty"}`, 200},
		{"unknown strategy", `{"strategy": "yolo_naked_calls"}`, 400},
		{"empty strategy", `{"strategy": ""}`, 400},
		{"invalid json", `{nope`, 400},
	}

	for _, tc := range cases {
		if resp := post(tc.body); resp.StatusCode != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
	}
}

func TestConfigPostRejectsStrategyWithoutAllowlist(t *testing.T) {
	_, ts := newTestServer(t) // no SetConfigHooks

	req, _ := http.NewRequest("POST", ts.URL+"/api/config",
		strings.NewReader(`{"strategy": "sniper"}`))
	req.Header.Set("X-API-Key", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("strategy change without allowlist: got %d, want 400", resp.StatusCode)
	}
}

func TestConfigGetReturnsHookValues(t *testing.T) {
	s, ts := newTestServer(t)
	s.SetControlHooks(ControlHooks{
		Status: func() EngineStatus {
			return EngineStatus{
				Strategy:         "nine_twenty",
				Balance:          42000,
				StopLossEnabled:  true,
				StopLossPercent:  3.5,
				DiscoveryEnabled: true,
				Indices:          []string{"NIFTY"},
			}
		},
	})

	req, _ := http.NewRequest("GET", ts.URL+"/api/config", nil)
	req.Header.Set("X-API-Key", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var cfg ConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Strategy != "nine_twenty" || cfg.SessionBalance != 42000 ||
		!cfg.StopLossEnabled || cfg.StopLossPercent != 3.5 {
		t.Fatalf("GET /api/config did not return hook-sourced values: %+v", cfg)
	}
}

// --- WebSocket auth ---

func wsURL(ts *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + path
}

func TestWebSocketRejectsMissingToken(t *testing.T) {
	_, ts := newTestServer(t)

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(ts, "/ws/live"), nil)
	if err == nil {
		t.Fatal("WS dial without token succeeded, want rejection")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("WS without token: got status %d, want 401", code)
	}
}

func TestWebSocketRejectsBadToken(t *testing.T) {
	_, ts := newTestServer(t)

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(ts, "/ws/live?token=wrong"), nil)
	if err == nil {
		t.Fatal("WS dial with bad token succeeded, want rejection")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("WS with bad token: want 401")
	}
}

func TestWebSocketAcceptsValidTokenQueryParam(t *testing.T) {
	_, ts := newTestServer(t)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts, "/ws/live?token="+testToken), nil)
	if err != nil {
		t.Fatalf("WS dial with valid token failed: %v", err)
	}
	conn.Close()
}

func TestWebSocketAcceptsValidTokenHeader(t *testing.T) {
	_, ts := newTestServer(t)

	hdr := http.Header{}
	hdr.Set("X-API-Key", testToken)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts, "/ws/live"), hdr)
	if err != nil {
		t.Fatalf("WS dial with valid token header failed: %v", err)
	}
	conn.Close()
}

func TestWebSocketRejectsDisallowedOrigin(t *testing.T) {
	_, ts := newTestServer(t) // empty allowlist

	hdr := http.Header{}
	hdr.Set("Origin", "http://evil.example.com")
	_, _, err := websocket.DefaultDialer.Dial(wsURL(ts, "/ws/live?token="+testToken), hdr)
	if err == nil {
		t.Fatal("WS dial from disallowed origin succeeded, want rejection")
	}
}

func TestWebSocketAcceptsAllowlistedOrigin(t *testing.T) {
	s, ts := newTestServer(t)
	s.SetAllowedOrigins([]string{"http://app.example.com"})

	hdr := http.Header{}
	hdr.Set("Origin", "http://app.example.com")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts, "/ws/live?token="+testToken), hdr)
	if err != nil {
		t.Fatalf("WS dial from allowlisted origin failed: %v", err)
	}
	conn.Close()
}

// --- WebSocket concurrent-write stress ---

// TestWebSocketConcurrentBroadcastAndHeartbeat hammers broadcast/UpdateStatus
// from many goroutines while a real connected client (whose server side runs
// the 5s heartbeat loop and writer pump) reads everything. Run with -race:
// the old code called conn.WriteJSON from both the heartbeat loop and
// broadcast concurrently, which panics gorilla/websocket. The writer-pump
// design must survive this with no race and no panic.
func TestWebSocketConcurrentBroadcastAndHeartbeat(t *testing.T) {
	s, ts := newTestServer(t)

	const numClients = 4
	conns := make([]*websocket.Conn, 0, numClients)
	for i := 0; i < numClients; i++ {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts, "/ws/live?token="+testToken), nil)
		if err != nil {
			t.Fatalf("client %d dial: %v", i, err)
		}
		conns = append(conns, conn)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	// Readers: drain everything the server sends so buffers don't fill.
	var readWG sync.WaitGroup
	stopRead := make(chan struct{})
	for _, conn := range conns {
		readWG.Add(1)
		go func(c *websocket.Conn) {
			defer readWG.Done()
			for {
				c.SetReadDeadline(time.Now().Add(5 * time.Second))
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
				select {
				case <-stopRead:
					return
				default:
				}
			}
		}(conn)
	}

	// Writers: hammer broadcast and UpdateStatus concurrently.
	var writeWG sync.WaitGroup
	for g := 0; g < 8; g++ {
		writeWG.Add(1)
		go func(id int) {
			defer writeWG.Done()
			for i := 0; i < 200; i++ {
				if id%2 == 0 {
					s.broadcast(map[string]interface{}{"type": "stress", "goroutine": id, "i": i})
				} else {
					s.UpdateStatus(true, 1000+float64(i), 1.5, -2.5, nil)
				}
			}
		}(g)
	}

	writeWG.Wait()
	close(stopRead)
	readWG.Wait()
	// Reaching here without a panic (and with -race clean) is the pass
	// condition. The old implementation panicked with
	// "concurrent write to websocket connection".
}

// TestWebSocketSlowConsumerIsDropped verifies a client that never reads gets
// disconnected (buffer overflow → drop) instead of blocking broadcast.
func TestWebSocketSlowConsumerIsDropped(t *testing.T) {
	s, ts := newTestServer(t)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts, "/ws/live?token="+testToken), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Never read from conn. Flood well past the buffer size.
	done := make(chan struct{})
	go func() {
		for i := 0; i < wsSendBufferSize*10; i++ {
			s.broadcast(map[string]interface{}{"type": "flood", "i": i})
		}
		close(done)
	}()

	select {
	case <-done:
		// broadcast never blocked — good.
	case <-time.After(10 * time.Second):
		t.Fatal("broadcast blocked on a slow consumer")
	}

	// Client should have been dropped from the registry.
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.RLock()
		n := len(s.wsClients)
		s.mu.RUnlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slow consumer still registered after flood (clients=%d)", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- Rate limiting (G-9) ---

func TestRateLimitReturns429WithRetryAfter(t *testing.T) {
	s, ts := newTestServer(t)
	s.SetRateLimit(2, 2) // 2 rps, burst 2 — tiny so the test is fast and deterministic

	get := func() *http.Response {
		req, _ := http.NewRequest("GET", ts.URL+"/api/status", nil)
		req.Header.Set("X-API-Key", testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	var sawTooMany bool
	var retryAfter string
	for i := 0; i < 10; i++ {
		resp := get()
		if resp.StatusCode == http.StatusTooManyRequests {
			sawTooMany = true
			retryAfter = resp.Header.Get("Retry-After")
			resp.Body.Close()
			break
		}
		resp.Body.Close()
	}
	if !sawTooMany {
		t.Fatal("expected a 429 within burst+a few requests, got none")
	}
	if retryAfter == "" {
		t.Error("429 response missing Retry-After header")
	}
}

func TestRateLimitRejectsBadTokenTooAfterBurst(t *testing.T) {
	// Rate limiting must apply before the token check, so a client hammering
	// the endpoint with a WRONG token also gets throttled (not just valid
	// clients) — otherwise the limiter is useless against brute force.
	s, ts := newTestServer(t)
	s.SetRateLimit(1, 1)

	req, _ := http.NewRequest("GET", ts.URL+"/api/status", nil)
	req.Header.Set("X-API-Key", "wrong-token")
	resp1, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusUnauthorized {
		t.Fatalf("first bad-token request: got %d, want 401", resp1.StatusCode)
	}

	req2, _ := http.NewRequest("GET", ts.URL+"/api/status", nil)
	req2.Header.Set("X-API-Key", "wrong-token")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second bad-token request past burst=1: got %d, want 429", resp2.StatusCode)
	}
}

func TestRateLimitIsPerIP(t *testing.T) {
	s, _ := newTestServer(t)
	s.SetRateLimit(1, 1)

	limA := s.limiterFor("1.2.3.4")
	limB := s.limiterFor("5.6.7.8")
	if !limA.Allow() {
		t.Fatal("fresh limiter for IP A should allow first request")
	}
	if !limB.Allow() {
		t.Fatal("fresh limiter for IP B should allow first request independent of IP A")
	}
	if limA.Allow() {
		t.Fatal("IP A burst=1 should be exhausted by its second request")
	}
}

// --- WebSocket connection cap (G-9) ---

func TestWebSocketConnectionCapRejectsBeyondLimit(t *testing.T) {
	s, ts := newTestServer(t)
	s.SetWSMaxConns(2)

	var conns []*websocket.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	for i := 0; i < 2; i++ {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts, "/ws/live?token="+testToken), nil)
		if err != nil {
			t.Fatalf("connection %d within cap failed: %v", i, err)
		}
		conns = append(conns, conn)
	}

	// Give the server a moment to register both clients (registration
	// happens synchronously in handleWebSocket before this dial returns, so
	// this is a belt-and-suspenders wait, not strictly required).
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.RLock()
		n := len(s.wsClients)
		s.mu.RUnlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(ts, "/ws/live?token="+testToken), nil)
	if err == nil {
		t.Fatal("3rd connection beyond cap=2 succeeded, want rejection")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("3rd connection beyond cap: got status %d, want 503", code)
	}
}

// --- CORS ---

func TestNoCORSHeadersByDefault(t *testing.T) {
	_, ts := newTestServer(t)

	req, _ := http.NewRequest("GET", ts.URL+"/api/status", nil)
	req.Header.Set("X-API-Key", testToken)
	req.Header.Set("Origin", "http://anything.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if v := resp.Header.Get("Access-Control-Allow-Origin"); v != "" {
		t.Fatalf("CORS header sent with empty allowlist: %q", v)
	}
}

func TestCORSAllowlist(t *testing.T) {
	s, ts := newTestServer(t)
	s.SetCORSAllowedOrigins([]string{"http://app.example.com"})

	// Allowlisted origin gets the header echoed.
	req, _ := http.NewRequest("GET", ts.URL+"/api/status", nil)
	req.Header.Set("X-API-Key", testToken)
	req.Header.Set("Origin", "http://app.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if v := resp.Header.Get("Access-Control-Allow-Origin"); v != "http://app.example.com" {
		t.Fatalf("allowlisted origin: header %q, want echo of origin", v)
	}

	// Non-allowlisted origin gets nothing.
	req2, _ := http.NewRequest("GET", ts.URL+"/api/status", nil)
	req2.Header.Set("X-API-Key", testToken)
	req2.Header.Set("Origin", "http://evil.example.com")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if v := resp2.Header.Get("Access-Control-Allow-Origin"); v != "" {
		t.Fatalf("non-allowlisted origin got CORS header: %q", v)
	}
	// Wildcard must never appear.
	if v := resp2.Header.Get("Access-Control-Allow-Origin"); v == "*" {
		t.Fatal("wildcard CORS header present")
	}
}
