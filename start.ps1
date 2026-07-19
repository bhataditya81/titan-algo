# TitanAlgo - Paper Trading Launcher
# Builds the Go engine to a real binary (not `go run`) and starts it
# alongside the Streamlit dashboard.
#
# WP-8 fix (audit CR-15/§process): `go run` recompiles on every launch and
# runs the binary under a randomly-named temp process, which stop.ps1 can
# never reliably target. This script does `go build -o titan.exe ./cmd`
# once, launches that binary directly, and records its real PID to a PID
# file so stop.ps1 can find it if graceful HTTP shutdown fails.
#
# Credentials (ANGEL_*, TITAN_API_TOKEN) are read from the environment by
# internal/config — this script never reads, prints, or forwards secret
# values. Set them in your shell/session before running this script.

param(
    [Parameter(Mandatory = $false)]
    [double]$Balance = 10000.0,

    [Parameter(Mandatory = $false)]
    [switch]$DashboardOnly,

    [Parameter(Mandatory = $false)]
    [switch]$EngineOnly
)

$ErrorActionPreference = "Stop"

Write-Host "========================================" -ForegroundColor Cyan
Write-Host "  TitanAlgo - Paper Trading System" -ForegroundColor Cyan
Write-Host "========================================" -ForegroundColor Cyan
Write-Host ""

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RootDir = $ScriptDir
$GoDir = Join-Path $RootDir "go-engine"
$PidFile = Join-Path $GoDir "titan.pid"

function Test-Command {
    param($Command)
    $null = Get-Command $Command -ErrorAction SilentlyContinue
    return $?
}

Write-Host "Checking prerequisites..." -ForegroundColor Yellow

if (-not (Test-Command "go")) {
    Write-Host "ERROR: Go is not installed or not in PATH" -ForegroundColor Red
    Write-Host "Please install Go 1.22+ from https://go.dev/dl/" -ForegroundColor Red
    exit 1
}

if (-not (Test-Command "python")) {
    Write-Host "ERROR: Python is not installed or not in PATH" -ForegroundColor Red
    Write-Host "Please install Python 3.11+ from https://www.python.org/downloads/" -ForegroundColor Red
    exit 1
}

Write-Host "OK - Go installed: $(go version)" -ForegroundColor Green
Write-Host "OK - Python installed: $(python --version)" -ForegroundColor Green
Write-Host ""

$LogsDir = Join-Path $GoDir "logs"
if (-not (Test-Path $LogsDir)) {
    New-Item -ItemType Directory -Path $LogsDir -Force | Out-Null
    Write-Host "OK - Created logs directory" -ForegroundColor Green
}

# Build + start the Go engine, recording its real PID.
function Start-GoEngine {
    param([double]$SessionBalance)

    Write-Host "Building Go Trading Engine..." -ForegroundColor Yellow
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
    Write-Host "OK - Built go-engine\titan.exe" -ForegroundColor Green

    Write-Host "Starting Go Trading Engine..." -ForegroundColor Yellow
    Write-Host "Session Balance: Rs.$SessionBalance" -ForegroundColor Cyan
    Write-Host ""

    $proc = Start-Process -FilePath (Join-Path $GoDir "titan.exe") `
        -ArgumentList "-paper", "-balance", "$SessionBalance" `
        -WorkingDirectory $GoDir `
        -WindowStyle Normal `
        -PassThru

    Set-Content -Path $PidFile -Value $proc.Id
    Write-Host "OK - Go engine started (PID $($proc.Id), recorded to $PidFile)" -ForegroundColor Green
    Start-Sleep -Seconds 2
}

function Start-Dashboard {
    Write-Host "Starting Streamlit Dashboard..." -ForegroundColor Yellow

    $DashboardDir = Join-Path $RootDir "py-brain\dashboard"
    $RequirementsFile = Join-Path $DashboardDir "requirements.txt"

    Write-Host "Checking Python dependencies..." -ForegroundColor Yellow

    $null = python -c "import streamlit" 2>$null
    if ($LASTEXITCODE -ne 0) {
        Write-Host "Installing Python dependencies..." -ForegroundColor Yellow
        python -m pip install -r $RequirementsFile

        if ($LASTEXITCODE -ne 0) {
            Write-Host "ERROR: Failed to install Python dependencies" -ForegroundColor Red
            exit 1
        }
    }

    Write-Host "OK - Python dependencies installed" -ForegroundColor Green
    Write-Host ""

    # Keep the dashboard's displayed balance consistent with the balance the
    # engine was actually launched with (audit §5 — was hardcoded to 1000).
    $env:TITAN_SESSION_BALANCE = "$Balance"

    $DashboardCommand = "cd '$DashboardDir'; streamlit run app.py"
    Start-Process powershell -ArgumentList "-NoExit", "-Command", $DashboardCommand -WindowStyle Normal

    Write-Host "OK - Dashboard started in new window" -ForegroundColor Green
    Write-Host ""
    Write-Host "Dashboard URL: http://localhost:8501" -ForegroundColor Cyan
}

if ($DashboardOnly) {
    Start-Dashboard
}
elseif ($EngineOnly) {
    Start-GoEngine -SessionBalance $Balance
}
else {
    Start-GoEngine -SessionBalance $Balance
    Start-Sleep -Seconds 3
    Start-Dashboard
}

Write-Host ""
Write-Host "========================================" -ForegroundColor Cyan
Write-Host "  TitanAlgo is running!" -ForegroundColor Green
Write-Host "========================================" -ForegroundColor Cyan
Write-Host ""
Write-Host "Go Engine: Running in separate window (PID recorded in $PidFile)" -ForegroundColor Yellow
Write-Host "Dashboard: http://localhost:8501" -ForegroundColor Yellow
Write-Host ""
Write-Host "Run .\stop.ps1 to shut down gracefully." -ForegroundColor Gray
Write-Host ""
