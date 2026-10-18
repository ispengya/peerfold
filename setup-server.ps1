# setup-server.ps1 - peerfold server one-click setup (Windows)
# Installs Go + WinFsp, builds the binary, creates config dirs, then starts the server.
# Run as Administrator.

$ErrorActionPreference = "Stop"
$ProjectRoot = $PSScriptRoot
$BinPath = Join-Path $ProjectRoot "peerfold.exe"
$GoModCache = Join-Path $env:TEMP "peerfold-gomodcache"

function Step($msg)  { Write-Host ""; Write-Host "==> $msg" -ForegroundColor Cyan }
function Ok($msg)    { Write-Host "    [OK] $msg" -ForegroundColor Green }
function Warn($msg)  { Write-Host "    [!]  $msg" -ForegroundColor Yellow }
function Fail($msg)  { Write-Host "    [X]  $msg" -ForegroundColor Red }

# --- 1. Go ---
Step "Check Go"
if (Get-Command go -ErrorAction SilentlyContinue) {
    Ok ("Go " + (& go version))
} else {
    Warn "Go not found. Installing..."
    winget install --id GoLang.Go --accept-source-agreements --accept-package-agreements
    Ok "Go installed. Reopen terminal and run this script again."
    exit 0
}

# --- 2. WinFsp ---
Step "Check WinFsp"
$svc = Get-Service "WinFsp.Launcher" -ErrorAction SilentlyContinue
if ($svc) {
    if ($svc.Status -ne "Running") {
        Start-Service "WinFsp.Launcher" -ErrorAction SilentlyContinue
    }
    Ok "WinFsp service: Running"
} else {
    Warn "WinFsp not found. Installing..."
    winget install --id WinFsp.WinFsp --accept-source-agreements --accept-package-agreements
    Ok "WinFsp installed. Reopen terminal and run this script again."
    exit 0
}

# --- 3. Build ---
Step "Build peerfold.exe"
$env:GOMODCACHE = $GoModCache
$env:GOPROXY = "off"
$env:GOFLAGS = "-mod=mod"
if (-not (Test-Path $GoModCache)) {
    $env:GOPROXY = "https://proxy.golang.org,direct"
    Remove-Item Env:GOFLAGS -ErrorAction SilentlyContinue
}
Push-Location $ProjectRoot
& go build -o $BinPath ./cmd/peerfold
Pop-Location
if ($LASTEXITCODE -ne 0) { Fail "Build failed"; exit 1 }
Ok "Build succeeded"

# --- 4. Config dirs ---
Step "Create config directories"
$up = [Environment]::GetFolderPath("UserProfile")
New-Item -ItemType Directory -Path (Join-Path $up "peerfold-server") -Force | Out-Null
Ok (Join-Path $up "peerfold-server")

# --- 5. Start server ---
Step "Start server"
Start-Process -FilePath $BinPath -ArgumentList "server","--listen","127.0.0.1:7000" -WindowStyle Minimized
Start-Sleep -Seconds 1

Write-Host ""
Write-Host "=== Server running ===" -ForegroundColor Green
Write-Host "  Address: 127.0.0.1:7000"
Write-Host "  Config:  $up\peerfold-server\config.json"
Write-Host "  Log:     $up\peerfold-server\log.txt"
Write-Host ""
Write-Host "  Next: run setup-client.ps1 on the same or another machine" -ForegroundColor Yellow
