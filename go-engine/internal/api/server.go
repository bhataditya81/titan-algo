package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"

	"titan-algo/internal/backtest"
	"titan-algo/internal/ledger"
	"titan-algo/internal/strategy"
)

// wsSendBufferSize is the per-connection outbound message buffer. If a client
// can't keep up and the buffer fills, the connection is dropped rather than
// blocking the writer or the sender (heartbeat/broadcast).
const wsSendBufferSize = 32

// DefaultMaxSessionBalance is the built-in sane upper bound accepted by
// POST /api/config for session_balance when no ConfigHooks.MaxSessionBalance
// override has been supplied. 10,00,000 (10 lakh) INR.
const DefaultMaxSessionBalance = 1000000.0

// Rate-limit defaults (G-9). This is a personal control API (one operator,
// one mobile client), not a public service — these are deliberately small.
const (
	DefaultRateLimitRPS   = 10.0 // sustained requests/sec per client IP
	DefaultRateLimitBurst = 20   // short burst allowance on top of the above
	DefaultWSMaxConns     = 5    // max concurrent /ws/live connections
)

// Server holds the API server state
type Server struct {
	mu   sync.RWMutex
	port int
	// token is this server's OWN authentication credential for the mobile
	// REST/WS API. It is never logged after the one-time startup print (only
	// when generated) — see NewServer.
	token              string
	bindAddr           string
	tlsCertFile        string
	tlsKeyFile         string
	allowedOrigins     []string // WS Origin allowlist
	corsAllowedOrigins []string // REST CORS allowlist (empty = no CORS headers)
	running            bool
	mode               string
	strategy           string
	balance            float64
	unrealizedPnL      float64
	realizedPnL        float64
	positions          []PositionInfo
	startTime          time.Time
	wsClients          map[*wsClient]bool
	wsUpgrader         websocket.Upgrader
	configPath         string
	hooks              *ControlHooks
	configHooks        *ConfigHooks

	// ledger is the durable trade record backing GET /api/trades. nil (the
	// default, until SetLedger is called) is a supported state — the
	// endpoint returns an empty "not connected" shape rather than erroring,
	// so this server also works standalone/before the integration layer
	// wires a ledger.
	ledger *ledger.Ledger

	// candlesDir is where GET /api/candles looks for "{symbol}.csv" cache
	// files (same format/convention as cmd/fetchdata and cmd/backtest's -csv
	// flag; see internal/backtest.LoadCandlesCSV). Default "data/historical".
	candlesDir string

	// webUIDir is the directory served at "/" for the web control panel's
	// static HTML/CSS/JS (unauthenticated -- the page itself has no
	// secrets; only the /api/* and /ws/live calls it makes need the token).
	// Default "../web-ui", i.e. the sibling repo-root web-ui/ directory
	// relative to go-engine's working directory (matching how cmd/main.go
	// is run and how every other relative path in this struct, e.g.
	// candlesDir/tradesLogPath/configPath, is already anchored).
	webUIDir string

	// Rate limiting (G-9): one token-bucket per client IP, applied to every
	// REST endpoint via authMiddleware. Chosen over per-token limiting
	// because this server currently issues a single shared token (see
	// NewServer) — per-token would collapse to the same single bucket
	// anyway, while per-IP also throttles an unauthenticated client hammering
	// the endpoint with bad tokens. Document this choice if a per-token
	// scheme (multiple issued tokens) is ever added.
	rateLimitRPS   float64
	rateLimitBurst int
	rlMu           sync.Mutex
	rateLimiters   map[string]*rate.Limiter // ponytail: unbounded map keyed by IP; fine for a handful of LAN/phone clients, add LRU/eviction if this ever faces many distinct IPs

	// wsMaxConns caps concurrent /ws/live connections (G-9). Small default:
	// this is a personal control channel, not a public WS service.
	wsMaxConns int
}

// PositionInfo represents an open position
type PositionInfo struct {
	Symbol       string  `json:"symbol"`
	Side         string  `json:"side"`
	Quantity     int     `json:"quantity"`
	EntryPrice   float64 `json:"entry_price"`
	CurrentPrice float64 `json:"current_price"`
	PnL          float64 `json:"pnl"`
}

// EngineStatus is the real, current state of the trading engine as sourced
// from ControlHooks.Status. It backs both /api/status and /api/config (GET).
type EngineStatus struct {
	Running          bool     `json:"running"`
	Mode             string   `json:"mode"`
	Strategy         string   `json:"strategy"`
	Balance          float64  `json:"balance"`
	UnrealizedPnL    float64  `json:"unrealized_pnl"`
	RealizedPnL      float64  `json:"realized_pnl"`
	PositionsCount   int      `json:"positions_count"`
	StopLossEnabled  bool     `json:"stop_loss_enabled"`
	StopLossPercent  float64  `json:"stop_loss_percent"`
	DiscoveryEnabled bool     `json:"discovery_enabled"`
	Indices          []string `json:"indices"`
}

// ControlHooks wires this server to the real engine control surface. Until
// SetControlHooks is called, /api/start, /api/stop and /api/kill return
// HTTP 503 "not wired" — they never fake success.
//
// Pause must stop new entries while leaving exits/management alone (soft
// stop). Resume must undo Pause. KillAndFlatten must stop entries AND
// flatten/square-off everything (hard stop / emergency kill). Status must
// return the real, current engine state.
type ControlHooks struct {
	Pause          func() error
	Resume         func() error
	KillAndFlatten func() error
	Status         func() EngineStatus
}

// ConfigHooks lets the integration layer (WP-9) supply the real validation
// limits and a callback to push a validated /api/config change into the live
// engine/risk manager. Optional — if unset (or AllowedStrategies is empty),
// strategy changes via POST /api/config are rejected (fail closed); the
// built-in DefaultMaxSessionBalance still applies to session_balance.
type ConfigHooks struct {
	// AllowedStrategies is the whitelist of strategy names POST /api/config
	// may switch to. Empty means no strategy change is accepted.
	AllowedStrategies []string
	// MaxSessionBalance caps the session_balance a client may set. <= 0 means
	// DefaultMaxSessionBalance applies.
	MaxSessionBalance float64
	// Apply receives the already-validated (sessionBalance, strategy) pair so
	// the integration layer can push it into the real engine. Optional.
	Apply func(sessionBalance float64, strategy string) error
}

// StatusResponse for /api/status
type StatusResponse struct {
	Running        bool    `json:"running"`
	Mode           string  `json:"mode"`
	Strategy       string  `json:"strategy"`
	Balance        float64 `json:"balance"`
	UnrealizedPnL  float64 `json:"unrealized_pnl"`
	RealizedPnL    float64 `json:"realized_pnl"`
	PositionsCount int     `json:"positions_count"`
	UptimeSeconds  int64   `json:"uptime_seconds"`
	LastHeartbeat  string  `json:"last_heartbeat"`
}

// ConfigResponse for /api/config
type ConfigResponse struct {
	Strategy         string   `json:"strategy"`
	SessionBalance   float64  `json:"session_balance"`
	StopLossEnabled  bool     `json:"stop_loss_enabled"`
	StopLossPercent  float64  `json:"stop_loss_percent"`
	DiscoveryEnabled bool     `json:"discovery_enabled"`
	Indices          []string `json:"indices"`
}

// CandleOut is one OHLCV bar as returned by GET /api/candles.
type CandleOut struct {
	Time   string  `json:"time"`
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume int64   `json:"volume"`
}

// defaultTradesLimit / maxTradesLimit / defaultCandlesLimit / maxCandlesLimit
// bound the ?limit= query params on /api/trades and /api/candles so a
// client can't force an unbounded response.
const (
	defaultTradesLimit  = 100
	maxTradesLimit      = 5000
	defaultCandlesLimit = 500
	maxCandlesLimit     = 5000
)

// wsClient wraps one WebSocket connection with a dedicated writer goroutine
// (writePump) and a buffered outbound channel. Both the periodic heartbeat
// and broadcast() enqueue onto send instead of ever calling conn.WriteJSON /
// conn.WriteMessage directly — this is the fix for the concurrent-write
// panic: gorilla/websocket connections are not safe for concurrent writers.
type wsClient struct {
	conn      *websocket.Conn
	send      chan []byte
	closeOnce sync.Once
	closed    chan struct{}
}

func newWSClient(conn *websocket.Conn) *wsClient {
	return &wsClient{
		conn:   conn,
		send:   make(chan []byte, wsSendBufferSize),
		closed: make(chan struct{}),
	}
}

// enqueue marshals v and hands it to the writer goroutine. If the client's
// buffer is full (slow consumer) the connection is dropped instead of
// blocking the caller (heartbeat ticker or broadcast).
func (c *wsClient) enqueue(v interface{}) bool {
	data, err := json.Marshal(v)
	if err != nil {
		return false
	}
	select {
	case <-c.closed:
		return false
	default:
	}
	select {
	case c.send <- data:
		return true
	case <-c.closed:
		return false
	default:
		// Buffer full: slow/stuck consumer. Drop the connection rather than
		// block the sender (heartbeat/broadcast must never stall on a bad
		// client).
		c.stop()
		return false
	}
}

func (c *wsClient) stop() {
	c.closeOnce.Do(func() {
		close(c.closed)
	})
}

// writePump is the ONLY goroutine allowed to write to conn. It exits (and
// closes conn) when the client is stopped or a write fails.
func (c *wsClient) writePump() {
	defer c.conn.Close()
	for {
		select {
		case msg := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				c.stop()
				return
			}
		case <-c.closed:
			return
		}
	}
}

// NewServer creates a new API server.
//
// token is this server's OWN authentication credential for the mobile
// REST/WS API. It MUST be generated independently of any broker credential
// (Angel One API key, secret, PIN, or TOTP seed) and MUST NEVER be set to
// one. Reusing a broker credential here means every phone/network hop that
// can read this server's token can also authenticate directly to the
// broker — this was the CR-1 finding this rewrite fixes. This constructor
// cannot detect "is this string a broker key" from inside the package, so
// the caller (WP-9, in internal/app/titan.go) is responsible for sourcing
// token from a dedicated config value (e.g. env var TITAN_API_TOKEN),
// never from cfg.Brokers.Angel.APIKey.
//
// If token == "", a random 32-byte token is generated with crypto/rand,
// hex-encoded, and printed ONCE to stdout at startup with a clear label.
// It is not written to any log after that, and callers must not log the
// resulting token either (do not log s.token, do not log any prefix of it).
func NewServer(port int, token string) *Server {
	generated := false
	if token == "" {
		token = generateToken()
		generated = true
	}

	s := &Server{
		port:           port,
		token:          token,
		bindAddr:       fmt.Sprintf("127.0.0.1:%d", port),
		running:        false,
		mode:           "paper",
		strategy:       "sniper",
		balance:        1000.0,
		positions:      []PositionInfo{},
		wsClients:      make(map[*wsClient]bool),
		configPath:     "config.yaml",
		rateLimitRPS:   DefaultRateLimitRPS,
		rateLimitBurst: DefaultRateLimitBurst,
		rateLimiters:   make(map[string]*rate.Limiter),
		wsMaxConns:     DefaultWSMaxConns,
		candlesDir:     "data/historical",
		webUIDir:       "../web-ui",
	}
	s.wsUpgrader = websocket.Upgrader{CheckOrigin: s.checkOrigin}

	if generated {
		fmt.Println("================================================================")
		fmt.Println("TITAN API AUTH TOKEN (shown once — save it now)")
		fmt.Println(token)
		fmt.Println("This token is required for every REST call (X-API-Key header)")
		fmt.Println("and WebSocket connection (?token= query param). It will not be")
		fmt.Println("printed or logged again.")
		fmt.Println("================================================================")
	}

	return s
}

// generateToken returns a cryptographically random 32-byte token, hex-encoded.
func generateToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failure is a fatal environment problem; falling back to
		// a weak token would silently defeat auth, so refuse to start.
		log.Fatalf("api: failed to generate secure auth token: %v", err)
	}
	return hex.EncodeToString(buf)
}

// Token returns the server's current auth token (the value passed to
// NewServer, or the crypto/rand-generated one when that was empty).
// R2-INT/G-14 wiring: internal/app/titan.go logs this through the `log`
// package (in addition to NewServer's one-time stdout banner) so the mobile
// build — which redirects `log` output to a file it can read but never sees
// stdout — has a channel to actually surface the token from. Callers must
// not write it anywhere less trusted than that (same rule NewServer's doc
// comment already states).
func (s *Server) Token() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.token
}

// SetBindAddr overrides the listen address (default "127.0.0.1:<port>" set
// by NewServer). Call before Start(). Intended for wiring from config
// (e.g. api.bind_addr) by WP-9/WP-8.
func (s *Server) SetBindAddr(addr string) {
	s.mu.Lock()
	s.bindAddr = addr
	s.mu.Unlock()
}

// SetTLS configures optional TLS. If both certFile and keyFile are non-empty,
// Start() serves via ListenAndServeTLS instead of plaintext HTTP. Call before
// Start().
func (s *Server) SetTLS(certFile, keyFile string) {
	s.mu.Lock()
	s.tlsCertFile = certFile
	s.tlsKeyFile = keyFile
	s.mu.Unlock()
}

// SetAllowedOrigins configures the Origin allowlist for /ws/live upgrades.
// Requests that send an Origin header (i.e. browser-originated) not present
// in this list are rejected. Requests with no Origin header (native mobile
// clients typically don't send one) are always allowed through this check
// (they still need a valid token). An empty/unset allowlist rejects ALL
// browser-originated (Origin-header-bearing) cross-origin WS connections.
func (s *Server) SetAllowedOrigins(origins []string) {
	s.mu.Lock()
	s.allowedOrigins = origins
	s.mu.Unlock()
}

// SetCORSAllowedOrigins configures the REST CORS allowlist. Empty (the
// default) means no CORS headers are ever sent — correct for a native app
// that never runs inside a browser origin. Only set this if a browser-based
// client needs to call the API directly.
func (s *Server) SetCORSAllowedOrigins(origins []string) {
	s.mu.Lock()
	s.corsAllowedOrigins = origins
	s.mu.Unlock()
}

// SetRateLimit configures the per-client-IP token-bucket rate limit applied
// to every REST endpoint (see authMiddleware). rps <= 0 or burst <= 0 leaves
// the corresponding default (DefaultRateLimitRPS / DefaultRateLimitBurst)
// unchanged. Call before Start(). Intended for wiring from config (e.g.
// api.rate_limit_rps / api.rate_limit_burst) by R2-INT.
func (s *Server) SetRateLimit(rps float64, burst int) {
	s.mu.Lock()
	if rps > 0 {
		s.rateLimitRPS = rps
	}
	if burst > 0 {
		s.rateLimitBurst = burst
	}
	s.mu.Unlock()
}

// SetWSMaxConns caps concurrent /ws/live connections (default
// DefaultWSMaxConns). n <= 0 is ignored. Call before Start(). Intended for
// wiring from config (e.g. api.ws_max_conns) by R2-INT.
func (s *Server) SetWSMaxConns(n int) {
	s.mu.Lock()
	if n > 0 {
		s.wsMaxConns = n
	}
	s.mu.Unlock()
}

// SetControlHooks wires the server to the real engine control surface. Until
// this is called, /api/start, /api/stop, /api/kill return 503 "not wired".
func (s *Server) SetControlHooks(hooks ControlHooks) {
	s.mu.Lock()
	s.hooks = &hooks
	s.mu.Unlock()
}

// SetConfigHooks wires the server to real config validation/apply logic for
// POST /api/config. See ConfigHooks for defaults when unset.
func (s *Server) SetConfigHooks(hooks ConfigHooks) {
	s.mu.Lock()
	s.configHooks = &hooks
	s.mu.Unlock()
}

// SetLedger wires GET /api/trades to the real, durable trade ledger (see
// internal/ledger). Call before Start(). Until called (or if passed nil),
// the endpoint returns {"trades": [], "note": "ledger not connected"}
// rather than erroring.
func (s *Server) SetLedger(l *ledger.Ledger) {
	s.mu.Lock()
	s.ledger = l
	s.mu.Unlock()
}

// SetCandlesDir overrides the directory GET /api/candles reads
// "{symbol}.csv" cache files from (default "data/historical"). Call before
// Start().
func (s *Server) SetCandlesDir(dir string) {
	if dir == "" {
		return
	}
	s.mu.Lock()
	s.candlesDir = dir
	s.mu.Unlock()
}

// SetWebUIDir overrides the directory served at "/" for the web control
// panel's static files (default "../web-ui"). Call before Start().
func (s *Server) SetWebUIDir(dir string) {
	if dir == "" {
		return
	}
	s.mu.Lock()
	s.webUIDir = dir
	s.mu.Unlock()
}

// checkOrigin implements websocket.Upgrader.CheckOrigin against the
// configured allowlist (see SetAllowedOrigins).
func (s *Server) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Non-browser clients (native mobile apps) typically don't send an
		// Origin header at all; there is no cross-origin browser risk here.
		return true
	}
	s.mu.RLock()
	allowed := s.allowedOrigins
	s.mu.RUnlock()
	for _, o := range allowed {
		if o == origin {
			return true
		}
	}
	return false
}

// Start runs the API server. Blocks until the listener returns an error.
func (s *Server) Start() error {
	mux := http.NewServeMux()

	// Register endpoints with auth middleware
	mux.HandleFunc("/api/status", s.authMiddleware(s.handleStatus))
	mux.HandleFunc("/api/positions", s.authMiddleware(s.handlePositions))
	mux.HandleFunc("/api/trades", s.authMiddleware(s.handleTrades))
	mux.HandleFunc("/api/config", s.authMiddleware(s.handleConfig))
	mux.HandleFunc("/api/start", s.authMiddleware(s.handleStart))
	mux.HandleFunc("/api/stop", s.authMiddleware(s.handleStop))
	mux.HandleFunc("/api/kill", s.authMiddleware(s.handleKill))
	mux.HandleFunc("/api/strategies", s.authMiddleware(s.handleStrategies))
	mux.HandleFunc("/api/candles", s.authMiddleware(s.handleCandles))
	// R3 fix: /ws/live previously bypassed the per-IP rate limiter entirely
	// (every REST endpoint goes through authMiddleware, which applies it) —
	// a client hammering upgrade attempts, valid token or not, went
	// completely unthrottled. rateLimitMiddleware alone (not the full
	// authMiddleware) is correct here: handleWebSocket already does its own
	// token check accepting BOTH ?token= and X-API-Key, which authMiddleware
	// doesn't (REST-only, header form only).
	mux.HandleFunc("/ws/live", s.rateLimitMiddleware(s.handleWebSocket))

	// Health check (no auth required)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Static web control-panel files (task 4). Deliberately NOT behind
	// authMiddleware -- the page itself has no secrets, only the /api/* and
	// /ws/live calls it makes need the token, which the page collects from
	// the user and attaches itself. Registered as the mux's catch-all "/"
	// pattern; every other route above is a more specific pattern so
	// ServeMux matches those first regardless of registration order.
	s.mu.RLock()
	webUIDir := s.webUIDir
	s.mu.RUnlock()
	mux.Handle("/", http.FileServer(http.Dir(webUIDir)))

	s.mu.RLock()
	addr := s.bindAddr
	certFile := s.tlsCertFile
	keyFile := s.tlsKeyFile
	s.mu.RUnlock()

	scheme := "http"
	if certFile != "" && keyFile != "" {
		scheme = "https"
	}
	log.Printf("Titan API server starting on %s://%s", scheme, addr)

	handler := s.corsMiddleware(mux)

	if certFile != "" && keyFile != "" {
		return http.ListenAndServeTLS(addr, certFile, keyFile, handler)
	}
	return http.ListenAndServe(addr, handler)
}

// validToken reports whether candidate matches the server's token using a
// constant-time comparison. Never logs either value.
func (s *Server) validToken(candidate string) bool {
	if candidate == "" {
		return false
	}
	s.mu.RLock()
	token := s.token
	s.mu.RUnlock()
	if token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(token)) == 1
}

// clientIP extracts the request's peer IP (without port) for rate-limit
// bucketing. Falls back to the raw RemoteAddr if it isn't a host:port pair.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limiterFor returns (creating if needed) the token-bucket limiter for ip.
func (s *Server) limiterFor(ip string) *rate.Limiter {
	s.rlMu.Lock()
	defer s.rlMu.Unlock()
	if lim, ok := s.rateLimiters[ip]; ok {
		return lim
	}
	s.mu.RLock()
	rps, burst := s.rateLimitRPS, s.rateLimitBurst
	s.mu.RUnlock()
	lim := rate.NewLimiter(rate.Limit(rps), burst)
	s.rateLimiters[ip] = lim
	return lim
}

// rateLimitMiddleware enforces the per-client-IP token bucket (G-9). On
// breach it returns 429 with a Retry-After hint rather than silently
// dropping the request.
func (s *Server) rateLimitMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lim := s.limiterFor(clientIP(r))
		if !lim.Allow() {
			w.Header().Set("Retry-After", strconv.Itoa(int(1/float64(lim.Limit())+1)))
			writeJSONError(w, http.StatusTooManyRequests, "rate limit exceeded, slow down")
			return
		}
		next(w, r)
	}
}

// authMiddleware enforces the per-IP rate limit first (so a client hammering
// the endpoint with bad tokens gets throttled too, not just valid clients),
// then checks for a valid token.
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	checked := func(w http.ResponseWriter, r *http.Request) {
		if !s.validToken(r.Header.Get("X-API-Key")) {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
	return s.rateLimitMiddleware(checked)
}

// corsMiddleware adds CORS headers only for origins on the configured
// allowlist. With no allowlist configured (the default, appropriate for a
// native app that never runs inside a browser origin), no CORS headers are
// sent at all.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		allowed := s.corsAllowedOrigins
		s.mu.RUnlock()

		if len(allowed) > 0 {
			origin := r.Header.Get("Origin")
			for _, o := range allowed {
				if o == origin {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Vary", "Origin")
					w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
					w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key")
					break
				}
			}
		}

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// writeJSONError writes a JSON {"error": msg} body with the given status.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// handleStatus returns engine status
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	hooks := s.hooks
	running := s.running
	mode := s.mode
	strategy := s.strategy
	balance := s.balance
	unrealizedPnL := s.unrealizedPnL
	realizedPnL := s.realizedPnL
	positionsCount := len(s.positions)
	startTime := s.startTime
	s.mu.RUnlock()

	if hooks != nil && hooks.Status != nil {
		st := hooks.Status()
		running = st.Running
		mode = st.Mode
		strategy = st.Strategy
		balance = st.Balance
		unrealizedPnL = st.UnrealizedPnL
		realizedPnL = st.RealizedPnL
		positionsCount = st.PositionsCount
	}

	var uptime int64 = 0
	if running && !startTime.IsZero() {
		uptime = int64(time.Since(startTime).Seconds())
	}

	resp := StatusResponse{
		Running:        running,
		Mode:           mode,
		Strategy:       strategy,
		Balance:        balance,
		UnrealizedPnL:  unrealizedPnL,
		RealizedPnL:    realizedPnL,
		PositionsCount: positionsCount,
		UptimeSeconds:  uptime,
		LastHeartbeat:  time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handlePositions returns open positions
func (s *Server) handlePositions(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	resp := map[string]interface{}{
		"positions": s.positions,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// parseLimitParam reads the "limit" query param, defaulting to def and
// capping at max. A present-but-invalid (non-integer or <= 0) value is
// rejected with a 400 by the caller (ok == false).
func parseLimitParam(r *http.Request, def, max int) (limit int, ok bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, false
	}
	if n > max {
		n = max
	}
	return n, true
}

// handleTrades returns recent trade history from the durable ledger (see
// SetLedger), most-recent-first. If no ledger is wired, this returns an
// empty list with an explanatory note rather than erroring -- this server
// must keep working standalone/before the integration layer wires a ledger.
func (s *Server) handleTrades(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimitParam(r, defaultTradesLimit, maxTradesLimit)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "limit must be a positive integer")
		return
	}

	s.mu.RLock()
	led := s.ledger
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")

	if led == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"trades": []ledger.Trade{},
			"note":   "ledger not connected",
		})
		return
	}

	trades, err := led.Query(ledger.DateRange{})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to query ledger: "+err.Error())
		return
	}

	if len(trades) > limit {
		trades = trades[len(trades)-limit:]
	}
	// Query returns ascending (oldest first); reverse for most-recent-first.
	for i, j := 0, len(trades)-1; i < j; i, j = i+1, j-1 {
		trades[i], trades[j] = trades[j], trades[i]
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"trades": trades})
}

// handleStrategies returns the sorted list of registered strategy names
// (the same registry cmd/backtest's -list-strategies flag reads).
func (s *Server) handleStrategies(w http.ResponseWriter, r *http.Request) {
	names := strategy.GetAvailableStrategies()
	sort.Strings(names)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"strategies": names})
}

// isAlphanumericSymbol reports whether s is non-empty and contains only
// ASCII letters/digits. Used to validate the ?symbol= query param on
// GET /api/candles before it's used to build a file path -- rejects "/",
// "\", ".." and anything else that isn't a plain instrument symbol.
func isAlphanumericSymbol(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// handleCandles serves cached historical OHLCV candles for charting, read
// from "{candlesDir}/{symbol}.csv" (see SetCandlesDir; same CSV format as
// internal/backtest.LoadCandlesCSV / cmd/backtest's -csv cache).
func (s *Server) handleCandles(w http.ResponseWriter, r *http.Request) {
	symbol := r.URL.Query().Get("symbol")
	if !isAlphanumericSymbol(symbol) {
		writeJSONError(w, http.StatusBadRequest, "symbol must be a non-empty alphanumeric string")
		return
	}

	limit, ok := parseLimitParam(r, defaultCandlesLimit, maxCandlesLimit)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "limit must be a positive integer")
		return
	}

	s.mu.RLock()
	dir := s.candlesDir
	s.mu.RUnlock()

	path := filepath.Join(dir, symbol+".csv")
	candles, err := backtest.LoadCandlesCSV(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("no cached candle data for symbol %s", symbol))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "failed to read candle data: "+err.Error())
		return
	}

	if len(candles) > limit {
		candles = candles[len(candles)-limit:]
	}

	out := make([]CandleOut, 0, len(candles))
	for _, c := range candles {
		out = append(out, CandleOut{
			Time:   c.Time.Format(time.RFC3339),
			Open:   c.Open,
			High:   c.High,
			Low:    c.Low,
			Close:  c.Close,
			Volume: c.Volume,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"symbol": symbol, "candles": out})
}

// handleConfig returns/updates configuration
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		hooks := s.hooks
		strategy := s.strategy
		balance := s.balance
		s.mu.RUnlock()

		resp := ConfigResponse{
			Strategy:       strategy,
			SessionBalance: balance,
		}

		if hooks != nil && hooks.Status != nil {
			st := hooks.Status()
			resp.Strategy = st.Strategy
			resp.SessionBalance = st.Balance
			resp.StopLossEnabled = st.StopLossEnabled
			resp.StopLossPercent = st.StopLossPercent
			resp.DiscoveryEnabled = st.DiscoveryEnabled
			resp.Indices = st.Indices
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return

	case http.MethodPost:
		s.handleConfigPost(w, r)
		return

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleConfigPost(w http.ResponseWriter, r *http.Request) {
	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	s.mu.RLock()
	cfgHooks := s.configHooks
	s.mu.RUnlock()

	maxBalance := DefaultMaxSessionBalance
	var allowedStrategies []string
	if cfgHooks != nil {
		if cfgHooks.MaxSessionBalance > 0 {
			maxBalance = cfgHooks.MaxSessionBalance
		}
		allowedStrategies = cfgHooks.AllowedStrategies
	}

	var (
		newBalance   float64
		haveBalance  bool
		newStrategy  string
		haveStrategy bool
	)

	if raw, ok := updates["session_balance"]; ok {
		// R3 fix: this used to update only s.balance (a display field read
		// back by GET /api/config) and return {"success":true} even when no
		// Apply hook was wired — the real risk.Manager cap never changed,
		// silently misleading whoever made the request into believing it
		// had. Fail closed instead, same as the strategy-change guard below.
		if cfgHooks == nil || cfgHooks.Apply == nil {
			writeJSONError(w, http.StatusBadRequest,
				"session_balance changes are not accepted: no Apply hook configured (see SetConfigHooks) — the real risk cap would not change")
			return
		}
		balance, ok := raw.(float64)
		if !ok || balance <= 0 || balance > maxBalance {
			writeJSONError(w, http.StatusBadRequest,
				fmt.Sprintf("session_balance must be a number > 0 and <= %.2f", maxBalance))
			return
		}
		newBalance = balance
		haveBalance = true
	}

	if raw, ok := updates["strategy"]; ok {
		strategy, ok := raw.(string)
		if !ok || strategy == "" {
			writeJSONError(w, http.StatusBadRequest, "strategy must be a non-empty string")
			return
		}
		if len(allowedStrategies) == 0 {
			writeJSONError(w, http.StatusBadRequest, "strategy changes are not accepted: no allowlist configured (see SetConfigHooks)")
			return
		}
		found := false
		for _, a := range allowedStrategies {
			if a == strategy {
				found = true
				break
			}
		}
		if !found {
			writeJSONError(w, http.StatusBadRequest, "strategy not in allowed list")
			return
		}
		newStrategy = strategy
		haveStrategy = true
	}

	s.mu.Lock()
	if haveBalance {
		s.balance = newBalance
	}
	if haveStrategy {
		s.strategy = newStrategy
	}
	balanceOut := s.balance
	strategyOut := s.strategy
	s.mu.Unlock()

	if cfgHooks != nil && cfgHooks.Apply != nil {
		if err := cfgHooks.Apply(balanceOut, strategyOut); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to apply config: "+err.Error())
			return
		}
	}

	log.Printf("Config updated via mobile app (strategy=%s session_balance=%.2f)", strategyOut, balanceOut)

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"success": true}`))
}

// handleStart resumes trading (Resume hook). Returns 503 if hooks aren't wired.
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.RLock()
	hooks := s.hooks
	s.mu.RUnlock()

	if hooks == nil || hooks.Resume == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "not wired")
		return
	}

	var req map[string]string
	json.NewDecoder(r.Body).Decode(&req)

	if err := hooks.Resume(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.mu.Lock()
	s.running = true
	s.startTime = time.Now()
	if mode, ok := req["mode"]; ok {
		s.mode = mode
	}
	s.mu.Unlock()

	log.Printf("Trading RESUME requested via mobile app")

	s.broadcast(map[string]interface{}{
		"type":    "status",
		"running": true,
	})

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"success": true, "message": "Trading resumed"}`))
}

// handleStop pauses trading (Pause hook). Returns 503 if hooks aren't wired.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.RLock()
	hooks := s.hooks
	s.mu.RUnlock()

	if hooks == nil || hooks.Pause == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "not wired")
		return
	}

	if err := hooks.Pause(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.mu.Lock()
	s.running = false
	s.mu.Unlock()

	log.Printf("Trading PAUSE requested via mobile app")

	s.broadcast(map[string]interface{}{
		"type":    "status",
		"running": false,
	})

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"success": true, "message": "Trading paused"}`))
}

// handleKill triggers an emergency stop-and-flatten (KillAndFlatten hook).
// Returns 503 if hooks aren't wired.
func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.RLock()
	hooks := s.hooks
	s.mu.RUnlock()

	if hooks == nil || hooks.KillAndFlatten == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "not wired")
		return
	}

	if err := hooks.KillAndFlatten(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.mu.Lock()
	s.running = false
	s.mu.Unlock()

	log.Printf("KILL AND FLATTEN requested via mobile app")

	s.broadcast(map[string]interface{}{
		"type":    "status",
		"running": false,
		"killed":  true,
	})

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"success": true, "message": "Kill switch activated, flattening positions"}`))
}

// handleWebSocket handles WebSocket connections for live updates. Requires a
// valid token (query param ?token=... or header X-API-Key) before the
// upgrade, and validates Origin against the configured allowlist (see
// SetAllowedOrigins / checkOrigin) as part of the upgrade itself.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		token = r.Header.Get("X-API-Key")
	}
	if !s.validToken(token) {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// Connection cap (G-9): this is a personal control channel, not a public
	// WS service — reject new connections once at capacity rather than
	// upgrading and immediately dropping them. Checked before Upgrade() so
	// the rejection is a plain HTTP response (no half-open WS handshake).
	s.mu.RLock()
	atCap := len(s.wsClients) >= s.wsMaxConns
	maxConns := s.wsMaxConns
	s.mu.RUnlock()
	if atCap {
		writeJSONError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("too many concurrent /ws/live connections (max %d)", maxConns))
		return
	}

	conn, err := s.wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	client := newWSClient(conn)

	s.mu.Lock()
	s.wsClients[client] = true
	s.mu.Unlock()

	log.Printf("Mobile app connected via WebSocket")

	go client.writePump()
	go s.wsReadPump(client)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.mu.RLock()
			running := s.running
			s.mu.RUnlock()

			msg := map[string]interface{}{
				"type":      "heartbeat",
				"timestamp": time.Now().Format(time.RFC3339),
				"running":   running,
			}

			if !client.enqueue(msg) {
				s.removeClient(client)
				return
			}
		case <-client.closed:
			s.removeClient(client)
			return
		}
	}
}

// wsReadPump drains inbound frames (control frames / client disconnect
// detection). The mobile client is not expected to send app messages, but a
// connection must be read from for gorilla/websocket to process pings/close
// frames and to detect the peer closing the socket.
func (s *Server) wsReadPump(client *wsClient) {
	defer client.stop()
	for {
		if _, _, err := client.conn.ReadMessage(); err != nil {
			return
		}
	}
}

// removeClient unregisters and stops a client exactly once.
func (s *Server) removeClient(client *wsClient) {
	s.mu.Lock()
	_, existed := s.wsClients[client]
	delete(s.wsClients, client)
	s.mu.Unlock()
	client.stop()
	if existed {
		log.Printf("Mobile app disconnected")
	}
}

// broadcast sends a message to all WebSocket clients via each client's
// buffered channel (never writes to a connection directly — see wsClient).
func (s *Server) broadcast(msg interface{}) {
	s.mu.RLock()
	clients := make([]*wsClient, 0, len(s.wsClients))
	for c := range s.wsClients {
		clients = append(clients, c)
	}
	s.mu.RUnlock()

	for _, c := range clients {
		if !c.enqueue(msg) {
			s.removeClient(c)
		}
	}
}

// UpdateStatus updates the server state (called by trading engine)
func (s *Server) UpdateStatus(running bool, balance, unrealizedPnL, realizedPnL float64, positions []PositionInfo) {
	s.mu.Lock()
	s.running = running
	s.balance = balance
	s.unrealizedPnL = unrealizedPnL
	s.realizedPnL = realizedPnL
	s.positions = positions
	s.mu.Unlock()

	// Broadcast update to mobile clients
	s.broadcast(map[string]interface{}{
		"type":           "update",
		"running":        running,
		"balance":        balance,
		"unrealized_pnl": unrealizedPnL,
		"realized_pnl":   realizedPnL,
		"positions":      len(positions),
	})
}
