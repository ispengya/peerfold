# install.ps1 — download the peerfold binary from GitHub Releases
# Usage:
#   irm https://raw.githubusercontent.com/ispengya/peerfold/main/install.ps1 | iex
#   .\install.ps1                # install the latest release
#   .\install.ps1 -Version v1.0.0
#   .\install.ps1 -Uninstall

param(
    [string]$Version = "latest",
    [string]$InstallDir = "",
    [switch]$Uninstall
)

$ErrorActionPreference = "Stop"
$Repo = if ($env:PEERFOLD_REPO) { $env:PEERFOLD_REPO } else { "ispengya/peerfold" }
if (-not $InstallDir) {
    $InstallDir = Join-Path ([Environment]::GetFolderPath("LocalApplicationData")) "peerfold"
}
$Binary = Join-Path $InstallDir "peerfold.exe"

function Ok($msg)   { Write-Host "       [OK] $msg" -ForegroundColor Green }
function Info($msg) { Write-Host "       $msg" -ForegroundColor DarkGray }
function Fail($msg) { Write-Host "       [X] $msg" -ForegroundColor Red }

if ($Uninstall) {
    if (Test-Path $Binary) {
        Remove-Item $Binary -Force
        Ok "Removed $Binary"
    } else {
        Info "Not found: $Binary"
    }
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if ($userPath -like "*$InstallDir*") {
        $newPath = ($userPath -split ';' | Where-Object { $_ -and $_ -ne $InstallDir }) -join ';'
        [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
        Ok "Removed from user PATH"
    }
    exit 0
}

# ── Platform ─────────────────────────────────────────────────

$arch = $env:PROCESSOR_ARCHITECTURE
switch ($arch) {
    "AMD64" { $goarch = "amd64" }
    "ARM64" { $goarch = "arm64" }
    default { Fail "Unsupported architecture: $arch"; exit 1 }
}
$asset = "peerfold-windows-$goarch.exe"

# ── URL ──────────────────────────────────────────────────────

if ($Version -eq "latest") {
    $url = "https://github.com/$Repo/releases/latest/download/$asset"
} else {
    $url = "https://github.com/$Repo/releases/download/$Version/$asset"
}

Write-Host ""
Write-Host "  peerfold installer"
Write-Host "  ------------------------------------"
Write-Host ""
Info "Repository: $Repo"
Info "Version:    $Version"
Info "Asset:      $asset"
Info "Install to: $InstallDir"
Write-Host ""

# ── Download ─────────────────────────────────────────────────

New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
Write-Host "  Downloading..."
try {
    Invoke-WebRequest -Uri $url -OutFile $Binary -UseBasicParsing
} catch {
    Fail "Download failed: $url"
    Fail $_.Exception.Message
    exit 1
}
Ok "Installed $Binary"

# ── PATH ─────────────────────────────────────────────────────

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -like "*$InstallDir*") {
    Ok "Already in user PATH: $InstallDir"
} else {
    if ($userPath) {
        $newPath = "$userPath;$InstallDir"
    } else {
        $newPath = $InstallDir
    }
    [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
    Ok "Added to user PATH: $InstallDir"
    Info "Open a new terminal for 'peerfold' to be on PATH."
}

# ── WinFsp hint ──────────────────────────────────────────────

$winfsp = Get-Service "WinFsp.Launcher" -ErrorAction SilentlyContinue
if (-not $winfsp) {
    Write-Host ""
    Info "WinFsp is not installed. 'peerfold mount' needs it:"
    Info "  winget install WinFsp.WinFsp"
    Info "  or https://winfsp.dev/rel/"
}

Write-Host ""
Write-Host "  ------------------------------------"
Write-Host "  Done! Next:"
Write-Host ""
Write-Host "    peerfold server     # start the aggregation server"
Write-Host "    peerfold client     # share local directories"
Write-Host "    peerfold mount      # mount the federated tree"
Write-Host ""
