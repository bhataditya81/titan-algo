# TitanAlgo - Paper Trading Launcher
# This script starts both the Go trading engine and the Streamlit dashboard

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

# Get script directory
$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RootDir = $ScriptDir

# Function to check if a command exists
function Test-Command {
    param($Command)
    $null = Get-Command $Command -ErrorAction SilentlyContinue
    return $?
}

# Check prerequisites
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

Write-Host "✓ Go installed: $(go version)" -ForegroundColor Green
Write-Host "✓ Python installed: $(python --version)" -ForegroundColor Green
Write-Host ""

# Create logs directory if it doesn't exist
$LogsDir = Join-Path $RootDir "go-engine\logs"
if (-not (Test-Path $LogsDir)) {
    New-Item -ItemType Directory -Path $LogsDir -Force | Out-Null
    Write-Host "✓ Created logs directory" -ForegroundColor Green
}

# Function to start the Go engine
function Start-GoEngine {
    param([double]$SessionBalance)
    
    Write-Host "Starting Go Trading Engine..." -ForegroundColor Yellow
    Write-Host "Session Balance: ₹$SessionBalance" -ForegroundColor Cyan
    Write-Host ""
    
    $GoDir = Join-Path $RootDir "go-engine"
    
    # Start Go engine in a new window
    $GoCommand = "cd '$GoDir'; go run cmd/main.go -paper -balance $SessionBalance"
    
    Start-Process powershell -ArgumentList "-NoExit", "-Command", $GoCommand -WindowStyle Normal
    
    Write-Host "✓ Go engine started in new window" -ForegroundColor Green
    Start-Sleep -Seconds 2
}

# Function to start the dashboard
function Start-Dashboard {
    Write-Host "Starting Streamlit Dashboard..." -ForegroundColor Yellow
    
    $DashboardDir = Join-Path $RootDir "py-brain\dashboard"
    $RequirementsFile = Join-Path $DashboardDir "requirements.txt"
    
    # Check if requirements are installed
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
    
    Write-Host "✓ Python dependencies installed" -ForegroundColor Green
    Write-Host ""
    
    # Start dashboard in a new window
    $DashboardCommand = "cd '$DashboardDir'; streamlit run app.py"
    
    Start-Process powershell -ArgumentList "-NoExit", "-Command", $DashboardCommand -WindowStyle Normal
    
    Write-Host "✓ Dashboard started in new window" -ForegroundColor Green
    Write-Host ""
    Write-Host "Dashboard URL: http://localhost:8501" -ForegroundColor Cyan
}

# Main execution
if ($DashboardOnly) {
    Start-Dashboard
}
elseif ($EngineOnly) {
    Start-GoEngine -SessionBalance $Balance
}
else {
    # Start both
    Start-GoEngine -SessionBalance $Balance
    Start-Sleep -Seconds 3
    Start-Dashboard
}

Write-Host ""
Write-Host "========================================" -ForegroundColor Cyan
Write-Host "  TitanAlgo is running!" -ForegroundColor Green
Write-Host "========================================" -ForegroundColor Cyan
Write-Host ""
Write-Host "Go Engine: Running in separate window" -ForegroundColor Yellow
Write-Host "Dashboard: http://localhost:8501" -ForegroundColor Yellow
Write-Host ""
Write-Host "Press Ctrl+C in each window to stop" -ForegroundColor Gray
Write-Host ""
