# cleanup.ps1 — Remove peerfold data, configs, and build artifacts
# Only removes peerfold's own files. Does NOT touch system dependencies
# (WinFsp, PATH, etc.) to avoid breaking other software.
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

Write-Host "  This will remove peerfold's own data:" -ForegroundColor Yellow
Write-Host ""
Write-Host "    [x] Server & client processes" -ForegroundColor DarkGray
Write-Host "    [x] Config directories and logs" -ForegroundColor DarkGray
Write-Host "    [x] Auto-start registry entries" -ForegroundColor DarkGray
Write-Host "    [x] Demo / test files" -ForegroundColor DarkGray
Write-Host "    [x] Build artifacts (peerfold.exe)" -ForegroundColor DarkGray
Write-Host ""
Write-Host "  Not touched: WinFsp, system PATH, source code" -ForegroundColor DarkGray
Write-Host ""

$confirm = Read-Host "  Continue? [y/N]"
if ($confirm -ne 'y' -and $confirm -ne 'Y') {
    Write-Host ""
    Write-Host "  Cancelled." -ForegroundColor DarkGray
    exit 0
}

$total = 5

# ── [1/5] Processes ──────────────────────────────────────────

Section 1 $total "Stop peerfold processes"

$procs = Get-Process peerfold -ErrorAction SilentlyContinue
if ($procs) {
    $procs | Stop-Process -Force
    Start-Sleep -Seconds 1
    Ok "Stopped $($procs.Count) process(es)"
} else {
    Ok "No running processes"
}

# ── [2/5] Config & logs ──────────────────────────────────────

Section 2 $total "Remove config and log directories"

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

# ── [3/5] Auto-start entries ─────────────────────────────────

Section 3 $total "Remove auto-start registrations"

$regBase = "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run"
$peerfoldKeys = Get-ItemProperty -Path $regBase -ErrorAction SilentlyContinue | Get-Member -MemberType NoteProperty | Where-Object { $_.Name -like "peerfold-*" }
if ($peerfoldKeys) {
    foreach ($key in $peerfoldKeys) {
        Remove-ItemProperty -Path $regBase -Name $key.Name -Force -ErrorAction SilentlyContinue
        Ok "Removed auto-start: $($key.Name)"
    }
} else {
    Ok "No auto-start entries found"
}

# ── [4/5] Demo files ─────────────────────────────────────────

Section 4 $total "Remove demo directories"

$demo = "C:\peerfold-demo"
if (Test-Path $demo) {
    Remove-Item $demo -Force -Recurse -ErrorAction SilentlyContinue
    Gone $demo
} else {
    NotFound $demo
}

# ── [5/5] Build artifacts ────────────────────────────────────

Section 5 $total "Remove build artifacts"

$bin = Join-Path $ProjectRoot "peerfold.exe"
if (Test-Path $bin) {
    Remove-Item $bin -Force -ErrorAction SilentlyContinue
    Gone $bin
} else {
    NotFound $bin
}

# ── Summary ──────────────────────────────────────────────────

Write-Host ""
Write-Host "  ------------------------------------" -ForegroundColor DarkGray
Write-Host "  Cleanup complete!" -ForegroundColor Green
Write-Host ""
Write-Host "  Removed: processes, configs, logs, auto-start," -ForegroundColor White
Write-Host "           demo files, build artifacts" -ForegroundColor White
Write-Host ""
Write-Host "  Untouched: WinFsp, system PATH, source code" -ForegroundColor DarkGray
Write-Host ""
