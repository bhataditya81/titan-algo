# Start TitanAlgo in LIVE TRADING Mode with Dashboard

$ErrorActionPreference = "Stop"

Write-Host "⚠️  WARNING: STARTING LIVE TRADING MODE ⚠️" -ForegroundColor Red
Write-Host "==========================================" -ForegroundColor Red
Write-Host "Real Money will be used. Losses are possible." -ForegroundColor Red
Write-Host "Ensure your config.yaml has correct Angel One credentials." -ForegroundColor Yellow
Write-Host ""
Start-Sleep -Seconds 2

# 1. Start the Streamlit Dashboard (Background)
Write-Host "Launching Dashboard..." -ForegroundColor Yellow
Start-Process -FilePath "python" -ArgumentList "-m streamlit run py-brain/dashboard/app.py" -WindowStyle Minimized
Write-Host "Dashboard started (Check your browser)" -ForegroundColor Green

# 2. Start the Trading Engine (Foreground)
Write-Host "Starting Trading Engine in LIVE MODE..." -ForegroundColor Red
Push-Location "go-engine"

try {
    # Run in LIVE Mode
    # The engine will ask for explicit confirmation (type 'I UNDERSTAND THE RISKS')
    & go run cmd/main.go -live
    
    # Keep window open if it stops/crashes
    if ($LASTEXITCODE -ne 0) {
        Write-Host 'Engine crashed or stopped.' -ForegroundColor Red
        Read-Host 'Press Enter to exit...'
    }
}
finally {
    Pop-Location
}
