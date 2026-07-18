# Start TitanAlgo in Paper Trading Mode with Dashboard

$ErrorActionPreference = "Stop"

Write-Host "Starting TitanAlgo: Risk-Free Paper Trading Mode" -ForegroundColor Cyan
Write-Host "===================================================" -ForegroundColor Cyan

# 1. Start the Streamlit Dashboard (Background)
Write-Host "Launching Dashboard..." -ForegroundColor Yellow
Start-Process -FilePath "python" -ArgumentList "-m streamlit run py-brain/dashboard/app.py" -WindowStyle Minimized
Write-Host "Dashboard started (Check your browser)" -ForegroundColor Green

# 2. Start the Trading Engine (Foreground)
Write-Host "Starting Trading Engine..." -ForegroundColor Yellow
Push-Location "go-engine"

try {
    # Run in Paper Mode using configured balance
    # Using & operator to properly invoke go and forward stdin for interactive input
    # This will use Live Data if credentials exist in config.yaml, or Mock Data otherwise.
    & go run cmd/main.go -paper -balance 10000
    
    # Keep window open if it stops/crashes
    if ($LASTEXITCODE -ne 0) {
        Write-Host 'Engine crashed or stopped.' -ForegroundColor Red
        Read-Host 'Press Enter to exit...'
    }
}
finally {
    Pop-Location
}
