# Start TitanAlgo in LIVE TRADING Mode with Dashboard
#
# WP-8 fix (audit CR-15/§process): builds a real binary
# (go build -o titan.exe ./cmd) instead of `go run`, and records its PID to
# go-engine\titan.pid so stop.ps1 can target it if graceful HTTP shutdown
# fails. Credentials (ANGEL_*, TITAN_API_TOKEN) MUST be set as environment
# variables — internal/config's live-mode gate refuses to start if any
# credential came from config.yaml instead. This script never echoes them.

$ErrorActionPreference = "Stop"

Write-Host "WARNING: STARTING LIVE TRADING MODE" -ForegroundColor Red
Write-Host "==========================================" -ForegroundColor Red
Write-Host "Real Money will be used. Losses are possible." -ForegroundColor Red
Write-Host "Ensure ANGEL_CLIENT_CODE, ANGEL_PIN, ANGEL_API_KEY, ANGEL_API_SECRET" -ForegroundColor Yellow
Write-Host "and ANGEL_TOTP_SECRET are set as environment variables (not in config.yaml)." -ForegroundColor Yellow
Write-Host ""
Start-Sleep -Seconds 2

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$GoDir = Join-Path $ScriptDir "go-engine"
$PidFile = Join-Path $GoDir "titan.pid"

# 1. Start the Streamlit Dashboard (Background)
Write-Host "Launching Dashboard..." -ForegroundColor Yellow
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

# 3. Start the built binary in LIVE mode (foreground, same console —
#    preserves interactive stdin for the "I UNDERSTAND THE RISKS" prompt).
Write-Host "Starting Trading Engine in LIVE MODE..." -ForegroundColor Red

$proc = Start-Process -FilePath (Join-Path $GoDir "titan.exe") `
    -ArgumentList "-live" `
    -WorkingDirectory $GoDir `
    -NoNewWindow -PassThru
Set-Content -Path $PidFile -Value $proc.Id

$proc.WaitForExit()
if ($proc.ExitCode -ne 0) {
    Write-Host 'Engine crashed or stopped.' -ForegroundColor Red
    Read-Host 'Press Enter to exit...'
}
Remove-Item $PidFile -ErrorAction SilentlyContinue
