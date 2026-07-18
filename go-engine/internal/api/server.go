package api

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Server holds the API server state
type Server struct {
	mu            sync.RWMutex
	port          int
	apiKey        string
	running       bool
	mode          string
	strategy      string
	balance       float64
	unrealizedPnL float64
	realizedPnL   float64
	positions     []PositionInfo
	startTime     time.Time
	wsClients     map[*websocket.Conn]bool
	wsUpgrader    websocket.Upgrader
	tradesLogPath string
	configPath    string
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

// TradeRecord from CSV
type TradeRecord struct {
	Timestamp string  `json:"timestamp"`
	Symbol    string  `json:"symbol"`
	Action    string  `json:"action"`
	Quantity  int     `json:"quantity"`
	FillPrice float64 `json:"fill_price"`
	NetPnL    float64 `json:"net_pnl"`
}

// NewServer creates a new API server
func NewServer(port int, apiKey string) *Server {
	return &Server{
		port:          port,
		apiKey:        apiKey,
		running:       false,
		mode:          "paper",
		strategy:      "sniper",
		balance:       1000.0,
		positions:     []PositionInfo{},
		wsClients:     make(map[*websocket.Conn]bool),
		tradesLogPath: "logs/trades.csv",
		configPath:    "config.yaml",
		wsUpgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true // Allow all origins for mobile app
			},
		},
	}
}

// Start runs the API server
func (s *Server) Start() error {
	mux := http.NewServeMux()

	// Register endpoints with auth middleware
	mux.HandleFunc("/api/status", s.authMiddleware(s.handleStatus))
	mux.HandleFunc("/api/positions", s.authMiddleware(s.handlePositions))
	mux.HandleFunc("/api/trades", s.authMiddleware(s.handleTrades))
	mux.HandleFunc("/api/config", s.authMiddleware(s.handleConfig))
	mux.HandleFunc("/api/start", s.authMiddleware(s.handleStart))
	mux.HandleFunc("/api/stop", s.authMiddleware(s.handleStop))
	mux.HandleFunc("/ws/live", s.handleWebSocket)

	// Health check (no auth required)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	addr := fmt.Sprintf(":%d", s.port)
	log.Printf("📱 Mobile API Server starting on http://localhost%s", addr)
	log.Printf("🔑 API Key: %s", s.apiKey[:8]+"...")

	return http.ListenAndServe(addr, s.corsMiddleware(mux))
}

// authMiddleware checks for valid API key
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiKey := r.Header.Get("X-API-Key")
		if apiKey != s.apiKey {
			http.Error(w, `{"error": "Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// corsMiddleware adds CORS headers
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// handleStatus returns engine status
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var uptime int64 = 0
	if s.running && !s.startTime.IsZero() {
		uptime = int64(time.Since(s.startTime).Seconds())
	}

	resp := StatusResponse{
		Running:        s.running,
		Mode:           s.mode,
		Strategy:       s.strategy,
		Balance:        s.balance,
		UnrealizedPnL:  s.unrealizedPnL,
		RealizedPnL:    s.realizedPnL,
		PositionsCount: len(s.positions),
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

// handleTrades returns trade history from CSV
func (s *Server) handleTrades(w http.ResponseWriter, r *http.Request) {
	trades := s.loadTradesFromCSV()

	resp := map[string]interface{}{
		"trades": trades,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// loadTradesFromCSV reads the trades log
func (s *Server) loadTradesFromCSV() []TradeRecord {
	var trades []TradeRecord

	file, err := os.Open(s.tradesLogPath)
	if err != nil {
		return trades
	}
	defer file.Close()

	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		return trades
	}

	// Skip header, parse records
	for i, record := range records {
		if i == 0 || len(record) < 6 {
			continue
		}

		var qty int
		var fillPrice, pnl float64
		fmt.Sscanf(record[3], "%d", &qty)
		fmt.Sscanf(record[4], "%f", &fillPrice)
		if len(record) > 9 {
			fmt.Sscanf(record[9], "%f", &pnl)
		}

		trades = append(trades, TradeRecord{
			Timestamp: record[0],
			Symbol:    record[1],
			Action:    record[2],
			Quantity:  qty,
			FillPrice: fillPrice,
			NetPnL:    pnl,
		})
	}

	return trades
}

// handleConfig returns/updates configuration
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		s.mu.RLock()
		defer s.mu.RUnlock()

		resp := ConfigResponse{
			Strategy:         s.strategy,
			SessionBalance:   s.balance,
			StopLossEnabled:  true,
			StopLossPercent:  5.0,
			DiscoveryEnabled: true,
			Indices:          []string{"NIFTY", "BANKNIFTY"},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if r.Method == "POST" {
		var updates map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
			http.Error(w, `{"error": "Invalid JSON"}`, http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		if balance, ok := updates["session_balance"].(float64); ok {
			s.balance = balance
		}
		if strategy, ok := updates["strategy"].(string); ok {
			s.strategy = strategy
		}
		s.mu.Unlock()

		log.Printf("📱 Config updated via mobile app: %v", updates)

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success": true}`))
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// handleStart starts the trading engine
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req map[string]string
	json.NewDecoder(r.Body).Decode(&req)

	s.mu.Lock()
	s.running = true
	s.startTime = time.Now()
	if mode, ok := req["mode"]; ok {
		s.mode = mode
	}
	if strategy, ok := req["strategy"]; ok {
		s.strategy = strategy
	}
	s.mu.Unlock()

	log.Printf("📱 Trading STARTED via mobile app (Mode: %s, Strategy: %s)", s.mode, s.strategy)

	// Broadcast to WebSocket clients
	s.broadcast(map[string]interface{}{
		"type":    "status",
		"running": true,
		"mode":    s.mode,
	})

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"success": true, "message": "Trading started"}`))
}

// handleStop stops the trading engine
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	s.running = false
	s.mu.Unlock()

	log.Printf("📱 Trading STOPPED via mobile app")

	// Broadcast to WebSocket clients
	s.broadcast(map[string]interface{}{
		"type":    "status",
		"running": false,
	})

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"success": true, "message": "Trading stopped"}`))
}

// handleWebSocket handles WebSocket connections for live updates
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	defer conn.Close()

	// Register client
	s.mu.Lock()
	s.wsClients[conn] = true
	s.mu.Unlock()

	log.Printf("📱 Mobile app connected via WebSocket")

	// Keep connection alive and send heartbeats
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		s.mu.RLock()
		running := s.running
		s.mu.RUnlock()

		msg := map[string]interface{}{
			"type":      "heartbeat",
			"timestamp": time.Now().Format(time.RFC3339),
			"running":   running,
		}

		if err := conn.WriteJSON(msg); err != nil {
			s.mu.Lock()
			delete(s.wsClients, conn)
			s.mu.Unlock()
			log.Printf("📱 Mobile app disconnected")
			return
		}
	}
}

// broadcast sends a message to all WebSocket clients
func (s *Server) broadcast(msg interface{}) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for conn := range s.wsClients {
		conn.WriteJSON(msg)
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
