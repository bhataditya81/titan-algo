# Start TitanAlgo in Paper Trading Mode with Dashboard
#
# WP-8 fix (audit CR-15/§process): builds a real binary
# (go build -o titan.exe ./cmd) instead of `go run`, and records its PID to
# go-engine\titan.pid so stop.ps1 can target it if graceful HTTP shutdown
# fails. Credentials (ANGEL_*, TITAN_API_TOKEN) are read from the
# environment by internal/config — this script never echoes them.

$ErrorActionPreference = "Stop"

Write-Host "Starting TitanAlgo: Risk-Free Paper Trading Mode" -ForegroundColor Cyan
Write-Host "===================================================" -ForegroundColor Cyan

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$GoDir = Join-Path $ScriptDir "go-engine"
$PidFile = Join-Path $GoDir "titan.pid"

# 1. Start the Streamlit Dashboard (Background)
Write-Host "Launching Dashboard..." -ForegroundColor Yellow
$env:TITAN_SESSION_BALANCE = "10000"
Start-Process -FilePath "python" -ArgumentList "-m streamlit run py-brain/dashboard/app.py" -WindowStyle Minimized
Write-Host "Dashboard started (Check your browser)" -ForegroundColor Green

# 2. Build the Trading Engine
Write-Host "Building Trading Engine..." -ForegroundColor Yellow
Push-Location $GoDir
try {
    & go build -o titan.exe ./cmd
    if ($LASTEXITCODE -ne 0) {
        Write-Host "ERROR: go build failed" -ForegroundColor Red
        exit 1
    }
} finally {
    Pop-Location
}

# 3. Start the built binary (foreground, same console — preserves
#    interactive stdin for any confirmation prompts).
Write-Host "Starting Trading Engine..." -ForegroundColor Yellow
Write-Host "This will use Live Data if Angel credentials are set in the environment, or Mock Data otherwise." -ForegroundColor Gray

$proc = Start-Process -FilePath (Join-Path $GoDir "titan.exe") `
    -ArgumentList "-paper", "-balance", "10000" `
    -WorkingDirectory $GoDir `
    -NoNewWindow -PassThru
Set-Content -Path $PidFile -Value $proc.Id

$proc.WaitForExit()
if ($proc.ExitCode -ne 0) {
    Write-Host 'Engine crashed or stopped.' -ForegroundColor Red
    Read-Host 'Press Enter to exit...'
}
Remove-Item $PidFile -ErrorAction SilentlyContinue
