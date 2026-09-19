# Builds and starts the gateway.
#
# Go does not read the Windows system proxy, so the proxy is set explicitly here.
# It is only needed to download modules; once built, the gateway also uses it to
# reach a remote upstream (loopback upstreams are excluded via NO_PROXY).
#
# Examples:
#   .\run.ps1                                        # config file if present, else env/single upstream
#   .\run.ps1 -Config gateway.yaml                   # explicit config
#   .\run.ps1 -Upstream https://api.deepseek.com     # single-upstream shorthand
param(
    [string]$Config   = "",
    [string]$Upstream = "",
    [string]$Listen   = "",
    [string]$Proxy    = "http://127.0.0.1:7897"
)

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

if ($Proxy) {
    $env:HTTPS_PROXY = $Proxy
    $env:HTTP_PROXY  = $Proxy
}
# Never route loopback traffic through the proxy: a local upstream (vLLM, Ollama,
# or the mock used in tests) must be reached directly.
$env:NO_PROXY = "127.0.0.1,localhost"
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

$args = @()
if ($Config)   { $args += @("-config", $Config) }
if ($Upstream) { $args += @("-upstream", $Upstream) }
if ($Listen)   { $args += @("-listen", $Listen) }

if ($Config) {
    Write-Host "starting gateway with config: $Config" -ForegroundColor Green
} else {
    Write-Host "starting gateway (no config file found; using defaults/env)" -ForegroundColor Yellow
    Write-Host "  tip: copy gateway.example.yaml to gateway.yaml to configure upstreams and keys" -ForegroundColor Yellow
}

& ".\llmgateway.exe" @args
