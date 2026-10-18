# cleanup.ps1 — Remove all peerfold environment, configs, and dependencies
# Stops processes, removes all data, uninstalls WinFsp. Leaves source code intact.
# Run as Administrator.

$ErrorActionPreference = "Continue"
$ProjectRoot = $PSScriptRoot
$Up = [Environment]::GetFolderPath("UserProfile")

# ── Helpers ──────────────────────────────────────────────────

function Header {
    Write-Host ""
    Write-Host "  peerfold" -ForegroundColor White
    Write-Host "  Federated filesystem — cleanup" -ForegroundColor DarkGray
    Write-Host "  ------------------------------------" -ForegroundColor DarkGray
    Write-Host ""
}

function Section($num, $total, $title) {
    Write-Host ""
    Write-Host "  [$num/$total] $title" -ForegroundColor Cyan
}

function Ok($msg)    { Write-Host "       [OK] $msg" -ForegroundColor Green }
function Info($msg)  { Write-Host "       $msg" -ForegroundColor DarkGray }
function Gone($msg)  { Write-Host "       [OK] Removed: $msg" -ForegroundColor Green }
function NotFound($msg) { Write-Host "       ... Not found: $msg" -ForegroundColor DarkGray }

# ── Start ────────────────────────────────────────────────────

Header

Write-Host "  This will remove ALL peerfold data and dependencies:" -ForegroundColor Yellow
Write-Host ""
Write-Host "    [x] Server & client processes" -ForegroundColor DarkGray
Write-Host "    [x] Config directories and logs" -ForegroundColor DarkGray
Write-Host "    [x] Demo / test files" -ForegroundColor DarkGray
Write-Host "    [x] Build artifacts (peerfold.exe)" -ForegroundColor DarkGray
Write-Host "    [x] WinFsp (FUSE driver)" -ForegroundColor DarkGray
Write-Host ""

$confirm = Read-Host "  Continue? [y/N]"
if ($confirm -ne 'y' -and $confirm -ne 'Y') {
    Write-Host ""
    Write-Host "  Cancelled." -ForegroundColor DarkGray
    exit 0
}

$total = 6

# ── [1/6] Processes ──────────────────────────────────────────

Section 1 $total "Stop peerfold processes"

$procs = Get-Process peerfold -ErrorAction SilentlyContinue
if ($procs) {
    $procs | Stop-Process -Force
    Start-Sleep -Seconds 1
    Ok "Stopped $($procs.Count) process(es)"
} else {
    Ok "No running processes"
}

# ── [2/6] Mount point ────────────────────────────────────────

Section 2 $total "Remove mount point"

$mountPoint = Join-Path $Up "peerfold"
if (Test-Path $mountPoint) {
    Remove-Item $mountPoint -Force -Recurse -ErrorAction SilentlyContinue
    Gone $mountPoint
} else {
    NotFound $mountPoint
}

# ── [3/6] Config & logs ──────────────────────────────────────

Section 3 $total "Remove config and log directories"

$dirs = @()
$dirs += Join-Path $Up "peerfold-server"
Get-ChildItem -Path $Up -Directory -Filter "peerfold-client-*" -ErrorAction SilentlyContinue | ForEach-Object {
    $dirs += $_.FullName
}
foreach ($dir in $dirs) {
    if (Test-Path $dir) {
        Remove-Item $dir -Force -Recurse -ErrorAction SilentlyContinue
        Gone $dir
    } else {
        NotFound $dir
    }
}

$flatFiles = Get-ChildItem -Path $Up -File -Filter "peerfold-*.json" -ErrorAction SilentlyContinue
foreach ($f in $flatFiles) {
    Remove-Item $f.FullName -Force -ErrorAction SilentlyContinue
    Gone $f.FullName
}

# ── [4/6] Demo files ─────────────────────────────────────────

Section 4 $total "Remove demo directories"

$demo = "C:\peerfold-demo"
if (Test-Path $demo) {
    Remove-Item $demo -Force -Recurse -ErrorAction SilentlyContinue
    Gone $demo
} else {
    NotFound $demo
}

# ── [5/6] Build artifacts ────────────────────────────────────

Section 5 $total "Remove build artifacts"

$bin = Join-Path $ProjectRoot "peerfold.exe"
if (Test-Path $bin) {
    Remove-Item $bin -Force -ErrorAction SilentlyContinue
    Gone $bin
} else {
    NotFound $bin
}

# ── [6/6] WinFsp ─────────────────────────────────────────────

Section 6 $total "Uninstall WinFsp"

$svc = Get-Service "WinFsp.Launcher" -ErrorAction SilentlyContinue
if ($svc) {
    if ($svc.Status -eq "Running") {
        Write-Host "       Stopping WinFsp service..." -ForegroundColor DarkGray
        Stop-Service "WinFsp.Launcher" -Force -ErrorAction SilentlyContinue
    }
    sc.exe delete "WinFsp.Launcher" 2>&1 | Out-Null
    Ok "Service removed"
} else {
    Ok "Service not found"
}

$winget = Get-Command winget -ErrorAction SilentlyContinue
if ($winget) {
    $installed = & winget list --id WinFsp.WinFsp 2>&1 | Select-String "WinFsp"
    if ($installed) {
        Write-Progress -Activity "Uninstalling WinFsp" -Status "Please wait..." -PercentComplete 50
        & winget uninstall --id WinFsp.WinFsp --accept-source-agreements 2>&1 | Out-Null
        Write-Progress -Activity "Uninstalling WinFsp" -Completed
        Ok "WinFsp package uninstalled"
    } else {
        Ok "WinFsp not in package manager"
    }
}

$winfspDir = "C:\Program Files (x86)\WinFsp"
if (Test-Path $winfspDir) {
    Remove-Item $winfspDir -Force -Recurse -ErrorAction SilentlyContinue
    Gone $winfspDir
} else {
    NotFound $winfspDir
}

$regPaths = @("HKLM:\SOFTWARE\WOW6432Node\WinFsp", "HKLM:\SOFTWARE\WinFsp")
foreach ($reg in $regPaths) {
    if (Test-Path $reg) {
        Remove-Item $reg -Force -Recurse -ErrorAction SilentlyContinue
        Gone "registry: $reg"
    }
}

# ── Summary ──────────────────────────────────────────────────

Write-Host ""
Write-Host "  ------------------------------------" -ForegroundColor DarkGray
Write-Host "  Cleanup complete!" -ForegroundColor Green
Write-Host ""
Write-Host "  Removed: processes, mount point, configs, logs," -ForegroundColor White
Write-Host "           demo files, build artifacts, WinFsp" -ForegroundColor White
Write-Host ""
Write-Host "  Source code in $ProjectRoot is untouched." -ForegroundColor DarkGray
Write-Host "  To use peerfold again, run setup-server.ps1 or setup-client.ps1." -ForegroundColor DarkGray
Write-Host ""
