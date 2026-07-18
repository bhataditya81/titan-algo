# TitanAlgo - Quick Start Guide

## 🚀 Quick Start

### Option 1: Default Start (₹1,000 session balance)
```powershell
.\quick-start.ps1
```

### Option 2: Custom Session Balance
```powershell
# Start with ₹2,000 session balance
.\start.ps1 -Balance 2000

# Or use the custom script
.\start-custom.ps1 -Balance 5000
```

### Option 3: Start Components Separately
```powershell
# Start only the trading engine
.\start.ps1 -EngineOnly -Balance 1000

# Start only the dashboard (if engine is already running)
.\start.ps1 -DashboardOnly
```

## 📊 Access Dashboard
After starting, open your browser to:
**http://localhost:8501**

## 🛑 Stop TitanAlgo
```powershell
.\stop.ps1
```

## 📁 File Locations

### Trade Logs
- **CSV File**: `go-engine/logs/trades.csv`
- Contains all trade history with dual balances

### Configuration
- **Go Config**: `go-engine/config.yaml`
- **Dashboard**: `py-brain/dashboard/app.py`

## 🔧 Troubleshooting

### Go Engine Won't Start
```powershell
# Check Go installation
go version

# Navigate to go-engine and run manually
cd go-engine
go mod tidy
go run cmd/main.go -paper -balance 1000
```

### Dashboard Won't Start
```powershell
# Install Python dependencies manually
cd py-brain/dashboard
pip install -r requirements.txt
streamlit run app.py
```

### Port Already in Use
```powershell
# Kill process on port 8501
netstat -ano | findstr :8501
taskkill /PID <PID> /F
```

## 📝 Script Parameters

### start.ps1 Parameters
- `-Balance <amount>`: Set session balance (default: 1000)
- `-EngineOnly`: Start only the Go trading engine
- `-DashboardOnly`: Start only the Streamlit dashboard

### Examples
```powershell
# High balance for testing
.\start.ps1 -Balance 10000

# Low balance to test limits
.\start.ps1 -Balance 500

# Restart just the dashboard
.\stop.ps1
.\start.ps1 -DashboardOnly
```

## 🎯 What Happens When You Start

1. **Prerequisites Check**: Verifies Go and Python are installed
2. **Logs Directory**: Creates `go-engine/logs` if needed
3. **Go Engine**: Starts in new PowerShell window
   - Initializes Mock Broker (₹10L)
   - Initializes Risk Manager (your -Balance amount)
   - Executes demo trades
   - Logs to CSV
4. **Dashboard**: Starts in new PowerShell window
   - Installs dependencies if needed
   - Opens on port 8501
   - Auto-refreshes every 2 seconds

## 📈 Expected Console Output

### Go Engine Window
```
Starting TitanAlgo in MODE_A mode
🧪 PAPER TRADING MODE ENABLED
💰 Session Balance Limit: ₹1000.00
CSV Logger initialized: logs/trades.csv
MockBroker: Connected successfully

=== Order Request: BUY RELIANCE 2 ===
✅ Order APPROVED by Risk Manager
MockBroker: Order Filled - BUY RELIANCE 2 @ ₹245.18
```

### Dashboard Window
```
You can now view your Streamlit app in your browser.

  Local URL: http://localhost:8501
  Network URL: http://192.168.x.x:8501
```

## 🔄 Workflow

1. Run `.\quick-start.ps1`
2. Wait for both windows to open
3. Open browser to http://localhost:8501
4. Watch trades execute in Go window
5. See real-time updates in dashboard
6. When done, run `.\stop.ps1`

## ⚠️ Important Notes

- **First Run**: May take longer as Go downloads dependencies
- **Python Deps**: First run installs Streamlit, Pandas, Plotly
- **CSV File**: Created automatically on first trade
- **Windows**: Keep both PowerShell windows open while running
- **Browser**: Dashboard auto-refreshes, no manual refresh needed

## 🎓 Next Steps

After running the system:
1. Check `logs/trades.csv` for trade history
2. Modify `-Balance` to test different scenarios
3. Edit `go-engine/cmd/main.go` to change trading logic
4. Customize `py-brain/dashboard/app.py` for different visualizations

## 📞 Support

If you encounter issues:
1. Check Go and Python versions (Go 1.22+, Python 3.11+)
2. Ensure no other services are using port 8501
3. Check `logs/trades.csv` exists and has data
4. Verify both windows stay open (don't close them)
