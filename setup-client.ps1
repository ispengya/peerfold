# setup-client.ps1 - peerfold client one-click setup (Windows)
# Installs Go + WinFsp, builds the binary, creates config dirs, then prints launch commands.
# Run as Administrator.

param(
    [string]$Name = "",
    [string]$ShareDir = ""
)

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

# Use provided name or hostname
if (-not $Name) {
    $Name = $env:COMPUTERNAME
}

$clientDir = Join-Path $up "peerfold-client-$Name"
New-Item -ItemType Directory -Path $clientDir -Force | Out-Null
Ok $clientDir

# --- 5. Print commands ---
if (-not $ShareDir) {
    $ShareDir = "C:\peerfold-demo\$Name"
    New-Item -ItemType Directory -Path $ShareDir -Force | Out-Null
}

Step "Setup complete. Run this to start the client:"
Write-Host ""
Write-Host "    cd $ProjectRoot" -ForegroundColor White
Write-Host "    .\peerfold.exe client --server 127.0.0.1:7000 --name $Name --share docs=$ShareDir" -ForegroundColor White
Write-Host ""
Write-Host "  Change --server to the actual server address if remote." -ForegroundColor Gray
Write-Host "  Config: $clientDir\config.json" -ForegroundColor Gray
Write-Host "  Log:    $clientDir\log.txt" -ForegroundColor Gray
