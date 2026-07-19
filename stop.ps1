# TitanAlgo - Stop
#
# WP-8 fix (audit CR-15/§process): the previous version matched process
# window titles against "*titan*" (which almost never matches a
# `go run`-launched process) and force-killed unconditionally, bypassing
# graceful shutdown / open-position flattening.
#
# New behavior: try the running engine's HTTP control API first —
# POST /api/stop (pause entries) then POST /api/kill (flatten + halt) — using
# TITAN_API_TOKEN from the environment (never printed). Only if that fails
# do we fall back to killing the process, and only by the PID recorded in
# go-engine\titan.pid by start*.ps1 (not by guessing from window titles),
# with a clear warning that this bypasses graceful shutdown.

param(
    [string]$ApiBindAddr = "127.0.0.1:8080"
)

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$PidFile = Join-Path $ScriptDir "go-engine\titan.pid"

Write-Host "Stopping TitanAlgo..." -ForegroundColor Yellow

$token = $env:TITAN_API_TOKEN
if (-not $token) {
    Write-Host "WARNING: TITAN_API_TOKEN is not set - graceful HTTP stop will likely be rejected (401)." -ForegroundColor Yellow
}

function Invoke-ControlEndpoint {
    param([string]$Path)
    try {
        $headers = @{}
        if ($token) { $headers["X-API-Key"] = $token }
        $uri = "http://$ApiBindAddr$Path"
        Invoke-RestMethod -Uri $uri -Method Post -Headers $headers -TimeoutSec 5 | Out-Null
        Write-Host "  -> $Path OK" -ForegroundColor Green
        return $true
    } catch {
        Write-Host "  -> $Path failed: $($_.Exception.Message)" -ForegroundColor DarkYellow
        return $false
    }
}

Write-Host "Attempting graceful shutdown via HTTP control API..." -ForegroundColor Yellow
$stopOk = Invoke-ControlEndpoint -Path "/api/stop"
$killOk = Invoke-ControlEndpoint -Path "/api/kill"

if ($stopOk -or $killOk) {
    Write-Host "Graceful shutdown signal sent." -ForegroundColor Green
    Remove-Item $PidFile -ErrorAction SilentlyContinue
} else {
    Write-Host ""
    Write-Host "WARNING: Graceful HTTP shutdown failed (server unreachable or auth rejected)." -ForegroundColor Red
    Write-Host "Falling back to force-killing the process by its recorded PID." -ForegroundColor Red
    Write-Host "This bypasses graceful shutdown - open positions will NOT be flattened by this path." -ForegroundColor Red
    Write-Host ""

    if (Test-Path $PidFile) {
        $enginePid = (Get-Content $PidFile | Select-Object -First 1).Trim()
        $proc = Get-Process -Id $enginePid -ErrorAction SilentlyContinue
        if ($proc) {
            Write-Host "Force-killing PID $enginePid ..." -ForegroundColor Red
            Stop-Process -Id $enginePid -Force
            Write-Host "OK - Stopped (force-killed)." -ForegroundColor Green
        } else {
            Write-Host "PID file references $enginePid but no such process is running." -ForegroundColor Yellow
        }
        Remove-Item $PidFile -ErrorAction SilentlyContinue
    } else {
        Write-Host "No PID file found at $PidFile - nothing to force-kill." -ForegroundColor Yellow
        Write-Host "If the engine is still running, stop it manually." -ForegroundColor Yellow
    }
}

# Best-effort: also stop the Streamlit dashboard. It holds no trading state,
# so an unconditional Stop-Process here is not a safety concern.
$StreamlitProcesses = Get-Process -Name streamlit -ErrorAction SilentlyContinue
if ($StreamlitProcesses) {
    $StreamlitProcesses | Stop-Process -Force
    Write-Host "OK - Stopped Streamlit dashboard" -ForegroundColor Green
}

Write-Host ""
Write-Host "Done." -ForegroundColor Green
