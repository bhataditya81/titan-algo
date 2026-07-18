# Stop all TitanAlgo processes

Write-Host "Stopping TitanAlgo processes..." -ForegroundColor Yellow

# Stop Go processes
$GoProcesses = Get-Process | Where-Object { $_.ProcessName -like "*go*" -and $_.MainWindowTitle -like "*titan*" }
if ($GoProcesses) {
    $GoProcesses | Stop-Process -Force
    Write-Host "✓ Stopped Go engine" -ForegroundColor Green
}

# Stop Streamlit processes
$StreamlitProcesses = Get-Process | Where-Object { $_.ProcessName -eq "streamlit" }
if ($StreamlitProcesses) {
    $StreamlitProcesses | Stop-Process -Force
    Write-Host "✓ Stopped Streamlit dashboard" -ForegroundColor Green
}

Write-Host ""
Write-Host "All TitanAlgo processes stopped" -ForegroundColor Green
