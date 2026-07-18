Write-Host "🚀 Starting Titan Algo Backtest..." -ForegroundColor Cyan
Write-Host "Strategy Configuration: config.yaml" -ForegroundColor Yellow

# Ensure we are in the correct directory
Set-Location $PSScriptRoot

# Run the backtest runner
# We pipe to Out-String to handle potential encoding issues if any remain
go run cmd/backtest/main.go | Out-String

Write-Host "`n✅ Backtest Complete." -ForegroundColor Green
Read-Host "Press Enter to exit..."
