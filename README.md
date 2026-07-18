# TitanAlgo - Indian Markets HFT System

A proprietary High-Frequency Trading (HFT) system designed for NSE/BSE markets with a hybrid architecture combining **Golang** for low-latency execution and **Python** for GPU-accelerated analytics.

## 🏗️ Architecture Overview

TitanAlgo operates in two distinct modes controlled by the `TITAN_MODE` environment variable:

### MODE A: "F&O Predator" (Full Auto-Trade)
- **Purpose**: Ultra-low latency execution for NIFTY/BANKNIFTY Futures & Options
- **Flow**: WebSocket Ticks → Go Indicators (VWAP, SuperTrend) → Risk Check → Direct Broker Execution
- **Latency**: Minimized by bypassing Python entirely
- **Features**: Circuit breaker logic, max orders/min throttling

### MODE B: "Stock Vision" (Analysis Only)
- **Purpose**: Scan 500+ NSE Equity stocks for profit potential with timeline predictions
- **Flow**: Go Ingestion → Shared Memory (Arrow IPC) → Python GPU (cuDF) → LSTM/Transformer → Prediction Cards
- **Output**: Analysis only, NO auto-execution

## 📁 Project Structure

```
/titan-algo
├── /proto                      # Shared Protocol Buffer definitions
│   ├── market_data.proto       # Tick, Bar, Batch messages
│   └── prediction.proto        # Signal, PredictionResponse
├── /go-engine                  # Execution & Ingestion Service
│   ├── /cmd                    # Entry point (main.go)
│   ├── /internal
│   │   ├── /broker             # Zerodha/AngelOne interfaces
│   │   ├── /feed               # WebSocket Manager
│   │   ├── /risk               # Kill Switch, Max Drawdown
│   │   └── /ipc                # Arrow Flight Client
│   ├── /models                 # GORM definitions (Trade, Order)
│   ├── go.mod
│   └── config.yaml
├── /py-brain                   # GPU Analytics Service
│   ├── /src
│   │   ├── /strategies         # RAPIDS cuDF indicators
│   │   ├── /models             # PyTorch Inference
│   │   └── server.py           # gRPC Server
│   ├── requirements.txt
│   └── Dockerfile.gpu
└── docker-compose.yml
```

## 🚀 Quick Start

### Prerequisites
- **Go**: 1.22+
- **Python**: 3.11+
- **Docker**: With NVIDIA Container Toolkit (for GPU support)
- **NVIDIA GPU**: Required for Python Brain (RAPIDS.ai)

### 1. Configure Broker Credentials
Edit `go-engine/config.yaml` and add your API keys:
```yaml
broker:
  zerodha:
    api_key: "YOUR_API_KEY"
    api_secret: "YOUR_API_SECRET"
  angel:
    client_code: "YOUR_CLIENT_CODE"
    password: "YOUR_PASSWORD"
    api_key: "YOUR_API_KEY"
```

### 2. Generate Protocol Buffers
```bash
# Install protoc compiler
# For Go
cd proto
protoc --go_out=../go-engine --go_opt=paths=source_relative \
       --go-grpc_out=../go-engine --go-grpc_opt=paths=source_relative \
       *.proto

# For Python
protoc --python_out=../py-brain/src --grpc_python_out=../py-brain/src *.proto
```

### 3. Run with Docker Compose
```bash
# Start all services (TimescaleDB, Redis, Go Engine, Python Brain)
docker-compose up --build

# Run in detached mode
docker-compose up -d
```

### 4. Run Standalone (Development)

#### Go Engine (Mode A)
```bash
cd go-engine
go mod tidy
export TITAN_MODE=MODE_A
go run cmd/main.go
```

#### Python Brain (Mode B)
```bash
cd py-brain
pip install -r requirements.txt
export TITAN_MODE=MODE_B
python src/server.py
```

## 🔧 Technology Stack

### Core
- **Go 1.22+**: Goroutines, Channels for concurrency
- **Python 3.11+**: GPU acceleration with RAPIDS.ai

### Communication
- **gRPC**: Inter-service communication (Protobuf)
- **Apache Arrow Flight**: Zero-copy shared memory transfer

### Databases
- **TimescaleDB**: Tick data (time-series)
- **PostgreSQL**: User data, trade logs (via GORM)
- **Redis**: Hot state, job queues (via Asynq)

### Analytics
- **RAPIDS.ai cuDF**: GPU-accelerated DataFrames
- **PyTorch**: LSTM/Transformer models for predictions

### Broker APIs
- Zerodha Kite Connect
- Angel One SmartAPI

## ⚙️ Configuration

### Environment Variables
- `TITAN_MODE`: Set to `MODE_A` (Auto-Trade) or `MODE_B` (Analysis)

### Risk Parameters (`config.yaml`)
```yaml
risk:
  max_drawdown_percent: 5.0
  max_orders_per_min: 100
  kill_switch_enabled: false
  session_balance_limit: 1000.0  # Session trading limit in INR
```

### Session Balance Limit
TitanAlgo includes a **dynamic session balance** feature that adjusts based on your trading performance:

- **Initial Balance**: Start with a set amount (e.g., ₹1,000)
- **Dynamic Adjustment**: Balance increases with profits, decreases with losses
- **Example**: 
  - Start: ₹1,000
  - Trade 1 Profit: +₹50 → New Balance: ₹1,050
  - Trade 2 Loss: -₹30 → New Balance: ₹1,020
- **Includes All Charges**: Automatically calculates and deducts brokerage, STT, transaction charges, GST, SEBI fees, and stamp duty
- **Pre-Trade Validation**: Rejects orders that would exceed available balance
- **Real-time Tracking**: Continuous monitoring of P&L and available capital

#### How It Works
1. **Open Position**: Capital is locked (turnover + charges)
2. **Close Position**: P&L is calculated and added to current balance
3. **Available Balance**: Current Balance - Locked Capital
4. **Max Drawdown**: Triggers if loss exceeds configured % (default 5%)

#### Indian Market Charges Calculated
The system automatically computes all trading charges based on Indian market regulations:

| Charge Type | Rate | Applied On |
|------------|------|------------|
| **Brokerage** | ₹20 or 0.03% (whichever lower) | All trades |
| **STT** | 0.025% - 0.1% | Varies by trade type |
| **Transaction Charges** | 0.00190% - 0.00375% | NSE/BSE trades |
| **GST** | 18% | Brokerage + Txn charges |
| **SEBI Fee** | ₹10 per crore | All trades |
| **Stamp Duty** | 0.015% | Buy side only |

#### Usage Example
```go
// Open a position
valid, reason := riskMgr.ValidateOrder(100.0, 5, risk.EquityIntraday, risk.Buy)
if !valid {
    log.Printf("Order REJECTED: %s", reason)
    return
}

riskMgr.OpenPosition("RELIANCE", 100.0, 5, risk.EquityIntraday, risk.Buy)
// Position OPENED - BUY RELIANCE 5 @ ₹100.00 | Locked: ₹523.87 | Available: ₹476.13/₹1000.00

// Close position with profit
pnl, _ := riskMgr.ClosePosition("RELIANCE", 110.0) // Sold at ₹110
// Position CLOSED - RELIANCE | Entry: ₹100.00, Exit: ₹110.00 | Net P&L: ₹26.13
// Balance Update - Current: ₹1026.13 (Initial: ₹1000.00, Realized P&L: ₹26.13)

// Check session stats
stats := riskMgr.GetSessionStats()
log.Printf("Current Balance: ₹%.2f (P&L: %.2f%%)", 
    stats["current_balance"], stats["pnl_percentage"])
```

## 📊 Database Schema

### Trade Model (GORM)
```go
type Trade struct {
    ID        uint
    Symbol    string
    Price     float64
    Quantity  int
    Side      string    // BUY/SELL
    Timestamp time.Time
}
```

## 🛡️ Risk Management

TitanAlgo implements multiple layers of risk protection:

### Session Balance Limit
- **Capital Control**: Set a maximum trading amount per session (e.g., ₹1,000)
- **Charge-Aware**: Automatically accounts for all brokerage and regulatory charges
- **Pre-Trade Validation**: Prevents orders that would exceed the limit
- **Real-time Tracking**: Continuous monitoring of used vs. available balance

### Other Risk Controls
- **Kill Switch**: Emergency stop for all trading activity
- **Max Drawdown**: Automatic halt at configured loss threshold (default 5%)
- **Circuit Breaker**: Rate limiting for order placement (100 orders/min)
- **Margin Validation**: Pre-trade balance checks

### Charge Breakdown Example
For a ₹500 equity intraday buy order:
```
Turnover:     ₹500.00
Brokerage:    ₹20.00
STT:          ₹0.00 (only on sell)
Txn Charges:  ₹0.16
GST:          ₹3.63
SEBI Fee:     ₹0.00
Stamp Duty:   ₹0.08
─────────────────────
Total Cost:   ₹523.87
```

## 🔮 Roadmap

- [ ] Implement Zerodha/AngelOne broker connectors
- [ ] Add technical indicators (VWAP, SuperTrend, RSI, MACD)
- [ ] Build LSTM/Transformer prediction models
- [ ] Implement Arrow Flight shared memory
- [ ] Add backtesting framework
- [ ] Create monitoring dashboard

## ⚠️ Disclaimer

This is a proprietary trading system. Use at your own risk. The authors are not responsible for any financial losses incurred through the use of this software.

## 📝 License

Proprietary - All Rights Reserved

---

**Built with ⚡ in Pune, India**
