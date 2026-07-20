package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"titan-algo/internal/ledger"
)

// newUITestServer builds a Server wired with the same mux/middleware stack
// Start() uses, including the four additions in this file (server.go):
// /api/strategies, /api/candles, the ledger-backed /api/trades, and the
// unauthenticated static file handler at "/". candlesDir/webUIDir are
// applied via the real setters when non-empty, mirroring how newTestServer
// (server_test.go) exercises the real middleware chain rather than calling
// handlers directly.
func newUITestServer(t *testing.T, candlesDir, webUIDir string) (*Server, *httptest.Server) {
	t.Helper()
	s := NewServer(0, testToken)
	if candlesDir != "" {
		s.SetCandlesDir(candlesDir)
	}
	if webUIDir != "" {
		s.SetWebUIDir(webUIDir)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", s.authMiddleware(s.handleStatus))
	mux.HandleFunc("/api/positions", s.authMiddleware(s.handlePositions))
	mux.HandleFunc("/api/trades", s.authMiddleware(s.handleTrades))
	mux.HandleFunc("/api/config", s.authMiddleware(s.handleConfig))
	mux.HandleFunc("/api/start", s.authMiddleware(s.handleStart))
	mux.HandleFunc("/api/stop", s.authMiddleware(s.handleStop))
	mux.HandleFunc("/api/kill", s.authMiddleware(s.handleKill))
	mux.HandleFunc("/api/strategies", s.authMiddleware(s.handleStrategies))
	mux.HandleFunc("/api/candles", s.authMiddleware(s.handleCandles))
	mux.HandleFunc("/ws/live", s.handleWebSocket)
	mux.Handle("/", http.FileServer(http.Dir(s.webUIDir)))

	ts := httptest.NewServer(s.corsMiddleware(mux))
	t.Cleanup(ts.Close)
	return s, ts
}

func getWithToken(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("X-API-Key", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// --- /api/strategies ---

func TestStrategiesEndpointReturnsRealSortedList(t *testing.T) {
	_, ts := newUITestServer(t, "", "")

	resp := getWithToken(t, ts.URL+"/api/strategies", testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	var body struct {
		Strategies []string `json:"strategies"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Strategies) == 0 {
		t.Fatal("expected at least one registered strategy, got none")
	}
	if !sort.StringsAreSorted(body.Strategies) {
		t.Errorf("strategies not sorted: %v", body.Strategies)
	}
}

// --- /api/candles ---

func TestCandlesEndpointReturnsParsedData(t *testing.T) {
	dir := t.TempDir()
	csvContent := "time,open,high,low,close,volume\n" +
		"2026-07-17T15:20:00+05:30,24330.00,24340.00,24325.00,24335.00,0\n" +
		"2026-07-17T15:25:00+05:30,24341.55,24352.65,24339.20,24346.70,0\n"
	if err := os.WriteFile(filepath.Join(dir, "NIFTY.csv"), []byte(csvContent), 0644); err != nil {
		t.Fatal(err)
	}

	_, ts := newUITestServer(t, dir, "")

	resp := getWithToken(t, ts.URL+"/api/candles?symbol=NIFTY&limit=10", testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	var body struct {
		Symbol  string      `json:"symbol"`
		Candles []CandleOut `json:"candles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Symbol != "NIFTY" {
		t.Errorf("symbol = %q, want NIFTY", body.Symbol)
	}
	if len(body.Candles) != 2 {
		t.Fatalf("got %d candles, want 2", len(body.Candles))
	}
	if body.Candles[1].Close != 24346.70 {
		t.Errorf("last candle close = %v, want 24346.70", body.Candles[1].Close)
	}
	if body.Candles[1].Time != "2026-07-17T15:25:00+05:30" {
		t.Errorf("last candle time = %q, want 2026-07-17T15:25:00+05:30", body.Candles[1].Time)
	}
}

func TestCandlesEndpointLimitsAndCaps(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	sb.WriteString("time,open,high,low,close,volume\n")
	base := time.Date(2026, 7, 1, 9, 15, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		ts := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		sb.WriteString(fmt.Sprintf("%s,%d,%d,%d,%d,0\n", ts, i, i, i, i))
	}
	if err := os.WriteFile(filepath.Join(dir, "BANKNIFTY.csv"), []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}

	_, ts := newUITestServer(t, dir, "")

	resp := getWithToken(t, ts.URL+"/api/candles?symbol=BANKNIFTY&limit=3", testToken)
	defer resp.Body.Close()
	var body struct {
		Candles []CandleOut `json:"candles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Candles) != 3 {
		t.Fatalf("got %d candles, want 3 (limit)", len(body.Candles))
	}
	// Must be the LAST 3 rows (i=7,8,9), not the first 3.
	if body.Candles[2].Close != 9 {
		t.Errorf("last candle close = %v, want 9 (tail of file)", body.Candles[2].Close)
	}
}

func TestCandlesEndpoint404ForUnknownSymbol(t *testing.T) {
	dir := t.TempDir()
	_, ts := newUITestServer(t, dir, "")

	resp := getWithToken(t, ts.URL+"/api/candles?symbol=UNKNOWN", testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d, want 404", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if !strings.Contains(body["error"], "UNKNOWN") {
		t.Errorf("error body = %v, want it to mention UNKNOWN", body)
	}
}

func TestCandlesEndpointRejectsPathTraversalSymbol(t *testing.T) {
	dir := t.TempDir()
	// A file one directory above candlesDir that a traversal attempt might
	// otherwise be able to reach if the symbol were used unvalidated.
	secretPath := filepath.Join(filepath.Dir(dir), "secret.csv")
	if err := os.WriteFile(secretPath, []byte("time,open,high,low,close,volume\n2026-01-01T00:00:00Z,1,1,1,1,0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(secretPath) })

	_, ts := newUITestServer(t, dir, "")

	badSymbols := []string{"../secret", "..\\secret", "a/b", "a\\b", "..", "NIFTY.csv", ""}
	for _, symbol := range badSymbols {
		q := "symbol=" + strings.ReplaceAll(strings.ReplaceAll(symbol, "\\", "%5C"), "/", "%2F")
		resp := getWithToken(t, ts.URL+"/api/candles?"+q, testToken)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("symbol %q: got %d, want 400", symbol, resp.StatusCode)
		}
	}
}

// --- /api/trades (ledger-backed) ---

func TestTradesEndpointReturnsLedgerRowsMostRecentFirst(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	led, err := ledger.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { led.Close() })

	base := time.Date(2026, 7, 18, 9, 15, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		err := led.Append(ledger.Trade{
			Timestamp:     base.Add(time.Duration(i) * time.Minute),
			ClientOrderID: fmt.Sprintf("ORD-%d", i),
			Symbol:        "NIFTY",
			Side:          "BUY",
			Quantity:      75,
			Price:         100 + float64(i),
			Status:        ledger.StatusFilled,
			Mode:          ledger.ModePaper,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	s, ts := newUITestServer(t, "", "")
	s.SetLedger(led)

	resp := getWithToken(t, ts.URL+"/api/trades?limit=2", testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	var body struct {
		Trades []ledger.Trade `json:"trades"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Trades) != 2 {
		t.Fatalf("got %d trades, want 2 (limit)", len(body.Trades))
	}
	if body.Trades[0].ClientOrderID != "ORD-2" {
		t.Errorf("most recent trade = %q, want ORD-2 (most-recent-first)", body.Trades[0].ClientOrderID)
	}
	if body.Trades[1].ClientOrderID != "ORD-1" {
		t.Errorf("second trade = %q, want ORD-1", body.Trades[1].ClientOrderID)
	}
}

func TestTradesEndpointNotConnectedShapeWhenNoLedger(t *testing.T) {
	_, ts := newUITestServer(t, "", "")

	resp := getWithToken(t, ts.URL+"/api/trades", testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	trades, ok := body["trades"].([]interface{})
	if !ok || len(trades) != 0 {
		t.Errorf("trades = %v, want empty array", body["trades"])
	}
	if body["note"] != "ledger not connected" {
		t.Errorf("note = %v, want %q", body["note"], "ledger not connected")
	}
}

// --- Auth: new endpoints must go through the exact same middleware ---

func TestNewEndpointsRejectMissingOrBadToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "NIFTY.csv"),
		[]byte("time,open,high,low,close,volume\n2026-07-17T15:20:00+05:30,1,1,1,1,0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, ts := newUITestServer(t, dir, "")

	endpoints := []string{
		"/api/strategies",
		"/api/candles?symbol=NIFTY",
		"/api/trades",
	}

	for _, path := range endpoints {
		for _, token := range []string{"", "wrong-token"} {
			resp := getWithToken(t, ts.URL+path, token)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s with token %q: got %d, want 401", path, token, resp.StatusCode)
			}
		}
	}
}

// --- Static UI files: no token required ---

func TestStaticFileHandlerServesWithoutToken(t *testing.T) {
	dir := t.TempDir()
	const page = "<html><body>Titan Control Panel</body></html>"
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(page), 0644); err != nil {
		t.Fatal(err)
	}

	_, ts := newUITestServer(t, "", dir)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200 (no token supplied)", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "Titan Control Panel") {
		t.Errorf("body = %q, want it to contain the fixture page content", body)
	}
}
