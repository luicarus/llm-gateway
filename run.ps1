# Builds and starts the gateway.
#
# Proxy handling: the gateway itself needs no proxy configuration. Go's HTTP
# client honours HTTP_PROXY / HTTPS_PROXY / NO_PROXY if your environment sets
# them, and connects directly if it does not, so in most cases you can ignore
# the -Proxy parameter entirely.
#
# Set -Proxy only when you are behind a corporate/local proxy AND it is not
# already exported in your shell (Go does not read the Windows system proxy
# settings, only these environment variables).
#
# Examples:
#   .\run.ps1                                        # config file if present, else env/single upstream
#   .\run.ps1 -Config gateway.yaml                   # explicit config
#   .\run.ps1 -Upstream https://api.deepseek.com     # single-upstream shorthand
#   .\run.ps1 -Proxy http://127.0.0.1:7890           # behind a proxy not in the env
#   .\run.ps1 -Proxy ""                              # force direct connections
param(
    [string]$Config   = "",
    [string]$Upstream = "",
    [string]$Listen   = "",
    [string]$Proxy    = ""
)

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

if ($Proxy) {
    Write-Host "using proxy $Proxy for network access" -ForegroundColor Cyan
    $env:HTTPS_PROXY = $Proxy
    $env:HTTP_PROXY  = $Proxy
} elseif ($env:HTTPS_PROXY -or $env:HTTP_PROXY) {
    $fromEnv = if ($env:HTTPS_PROXY) { $env:HTTPS_PROXY } else { $env:HTTP_PROXY }
    Write-Host "using proxy from environment: $fromEnv" -ForegroundColor Cyan
} else {
    Write-Host "no proxy configured; connecting directly" -ForegroundColor Cyan
}

# Loopback traffic must never go through a proxy: a local upstream (vLLM,
# Ollama, or the test mock) is reached directly. This is safe to set even when
# no proxy is in use, and it prevents a proxy from breaking local upstreams.
# Existing NO_PROXY entries are preserved.
# Note: the loop variable is not named $host, which is a read-only PowerShell
# automatic variable.
$noProxy = @($env:NO_PROXY -split ',' | ForEach-Object { $_.Trim() } | Where-Object { $_ })
foreach ($loopback in @("127.0.0.1", "localhost", "::1")) {
    if ($noProxy -notcontains $loopback) { $noProxy += $loopback }
}
$env:NO_PROXY = ($noProxy -join ',')

$env:CGO_ENABLED = "0"

Write-Host "building..." -ForegroundColor Cyan
go build -o llmgateway.exe .
if ($LASTEXITCODE -ne 0) { throw "build failed" }

# Prefer an explicit config; otherwise fall back to the first config file found
# in the working directory, so a deployment can just drop gateway.yaml here.
if (-not $Config) {
    foreach ($candidate in @("gateway.yaml", "gateway.yml", "gateway.json")) {
        if (Test-Path $candidate) { $Config = $candidate; break }
    }
}

$gatewayArgs = @()
if ($Config)   { $gatewayArgs += @("-config", $Config) }
if ($Upstream) { $gatewayArgs += @("-upstream", $Upstream) }
if ($Listen)   { $gatewayArgs += @("-listen", $Listen) }

if ($Config) {
    Write-Host "starting gateway with config: $Config" -ForegroundColor Green
} else {
    Write-Host "starting gateway (no config file found; using defaults/env)" -ForegroundColor Yellow
    Write-Host "  tip: copy gateway.example.yaml to gateway.yaml to configure upstreams and keys" -ForegroundColor Yellow
}

& ".\llmgateway.exe" @gatewayArgs
