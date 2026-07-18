# Paper Trading Mode - Quick Start Guide

## 🎯 Overview
TitanAlgo now supports **Paper Trading Mode** with a realistic mock broker and real-time dashboard for monitoring your trading performance.

## 🚀 Quick Start

### 1. Run Paper Trading (Standalone)
```bash
cd go-engine
go run cmd/main.go -paper
```

This will:
- Initialize MockBroker with ₹10,00,000 virtual balance
- Execute 4 sample trades (2 buys, 2 sells)
- Log all trades to `logs/trades.csv`
- Simulate realistic latency (50-200ms) and slippage (0.05-0.10%)

### 2. Start the Dashboard
In a **separate terminal**:
```bash
cd py-brain/dashboard
pip install -r requirements.txt
streamlit run app.py
```

Open browser at: **http://localhost:8501**

The dashboard will:
- Auto-refresh every 2 seconds
- Display KPIs (Total P&L, Win Rate, Total Trades)
- Show balance chart over time
- List recent trades
- Show trade distribution by symbol and action

## 📊 Dashboard Features

### KPI Metrics
- **Total P&L**: Profit/Loss with percentage
- **Win Rate**: Percentage of profitable trades
- **Total Trades**: Number of executed trades
- **Current Balance**: Real-time balance

### Charts
- **Balance Over Time**: Line chart showing balance progression
- **Trades by Symbol**: Pie chart of trade distribution
- **Buy vs Sell**: Bar chart of trade actions

## 🧪 Mock Broker Features

### Realistic Simulation
- **Latency**: 50-200ms random delay per order
- **Slippage**: 
  - Buy orders: +0.05% to +0.10% (unfavorable)
  - Sell orders: -0.05% to -0.10% (unfavorable)
- **Brokerage**: ₹20 flat fee per order
- **Balance Tracking**: Real-time virtual balance updates
- **Position Management**: Tracks open positions

### CSV Trade Log
Location: `go-engine/logs/trades.csv`

Columns:
- Timestamp
- Symbol
- Action (BUY/SELL)
- Quantity
- FillPrice
- Slippage
- TransactionFee
- VirtualBalance

## 🐳 Docker Deployment

### Run with Docker Compose
```bash
docker-compose up --build
```

Services:
- **go-engine**: Paper trading engine
- **dashboard**: Streamlit dashboard on port 8501
- **timescaledb**: Database (port 5432)
- **redis**: Cache (port 6379)

Access dashboard: **http://localhost:8501**

## 🔧 Configuration

### Change Initial Balance
Edit `cmd/main.go`:
```go
tradeService = broker.NewMockBroker(2000000.0) // ₹20,00,000
```

### Modify Slippage Range
Edit `internal/broker/mock.go`:
```go
slippagePercent := 0.10 + rand.Float64()*0.10 // 0.10% to 0.20%
```

### Adjust Latency
Edit `internal/broker/mock.go`:
```go
latency := time.Duration(100+rand.Intn(200)) * time.Millisecond // 100-300ms
```

## 📝 Example Output

### Console
```
2026-01-14 11:45:00 Starting TitanAlgo in MODE_A mode
2026-01-14 11:45:00 🧪 PAPER TRADING MODE ENABLED
2026-01-14 11:45:00 CSV Logger initialized: logs/trades.csv
2026-01-14 11:45:00 MockBroker: Connected successfully
2026-01-14 11:45:00 MockBroker: Subscribed to 5 symbols

--- Trade 1: Buying RELIANCE ---
MockBroker: Order Filled - BUY RELIANCE 10 @ ₹245.12 (Slippage: ₹0.18) | Balance: ₹997,528.80

--- Trade 2: Buying TCS ---
MockBroker: Order Filled - BUY TCS 5 @ ₹3,456.78 (Slippage: ₹2.45) | Balance: ₹980,234.55

=== Paper Trading Summary ===
Final Balance: ₹1,002,345.67
Open Positions: 0

✅ Check logs/trades.csv for trade history
🚀 Start the dashboard: cd py-brain/dashboard && streamlit run app.py
```

### CSV Output
```csv
Timestamp,Symbol,Action,Quantity,FillPrice,Slippage,TransactionFee,VirtualBalance
2026-01-14 11:45:01,RELIANCE,BUY,10,245.12,0.18,20.00,997528.80
2026-01-14 11:45:03,TCS,BUY,5,3456.78,2.45,20.00,980234.55
2026-01-14 11:45:05,RELIANCE,SELL,10,248.34,0.15,20.00,982456.70
2026-01-14 11:45:07,TCS,SELL,5,3478.90,1.89,20.00,1002345.67
```

## 🎓 Next Steps
1. Integrate with Risk Manager for balance limits
2. Add more realistic market price simulation
3. Implement strategy backtesting
4. Connect to live broker APIs (Zerodha/AngelOne)

## ⚠️ Important Notes
- Paper trading uses simulated prices
- Slippage and latency are randomized
- Results may differ from live trading
- Use for testing strategies only
