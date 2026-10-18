# cleanup.ps1 - Remove all peerfold environment, config, and dependencies (Windows)
# Stops all processes, removes config/logs/demos/build artifacts, and uninstalls WinFsp.
# Run as Administrator.

$ErrorActionPreference = "Continue"
$ProjectRoot = $PSScriptRoot
$up = [Environment]::GetFolderPath("UserProfile")

function Step($msg)  { Write-Host ""; Write-Host "==> $msg" -ForegroundColor Cyan }
function Ok($msg)    { Write-Host "    [OK] $msg" -ForegroundColor Green }
function Info($msg)  { Write-Host "    $msg" -ForegroundColor Gray }

# --- 1. Kill processes ---
Step "Stop all peerfold processes"
Get-Process peerfold -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Seconds 1
Ok "Done"

# --- 2. Remove mount point ---
Step "Remove mount point"
$mountPoint = Join-Path $up "peerfold"
if (Test-Path $mountPoint) {
    Remove-Item $mountPoint -Force -Recurse -ErrorAction SilentlyContinue
    Ok "Removed: $mountPoint"
} else {
    Info "Not found: $mountPoint"
}

# --- 3. Remove config and log directories ---
Step "Remove config and log directories"
$dirs = @(
    (Join-Path $up "peerfold-server"),
    (Join-Path $up "peerfold-client-alice"),
    (Join-Path $up "peerfold-client-bob")
)
Get-ChildItem -Path $up -Directory -Filter "peerfold-client-*" -ErrorAction SilentlyContinue | ForEach-Object {
    if ($dirs -notcontains $_.FullName) {
        $dirs += $_.FullName
    }
}
foreach ($dir in $dirs) {
    if (Test-Path $dir) {
        Remove-Item $dir -Force -Recurse -ErrorAction SilentlyContinue
        Ok "Removed: $dir"
    } else {
        Info "Not found: $dir"
    }
}

# Also remove old flat config files (peerfold-client-*.json)
Get-ChildItem -Path $up -File -Filter "peerfold-client-*.json" -ErrorAction SilentlyContinue | ForEach-Object {
    Remove-Item $_.FullName -Force -ErrorAction SilentlyContinue
    Ok "Removed: $($_.FullName)"
}
Get-ChildItem -Path $up -File -Filter "peerfold-server.json" -ErrorAction SilentlyContinue | ForEach-Object {
    Remove-Item $_.FullName -Force -ErrorAction SilentlyContinue
    Ok "Removed: $($_.FullName)"
}

# --- 4. Remove demo directories ---
Step "Remove demo directories"
$demo = "C:\peerfold-demo"
if (Test-Path $demo) {
    Remove-Item $demo -Force -Recurse -ErrorAction SilentlyContinue
    Ok "Removed: $demo"
} else {
    Info "Not found: $demo"
}

# --- 5. Remove build artifacts ---
Step "Remove build artifacts"
$bin = Join-Path $ProjectRoot "peerfold.exe"
if (Test-Path $bin) {
    Remove-Item $bin -Force -ErrorAction SilentlyContinue
    Ok "Removed: $bin"
} else {
    Info "Not found: $bin"
}

# --- 6. Uninstall WinFsp ---
Step "Uninstall WinFsp"

# Stop the service
$svc = Get-Service "WinFsp.Launcher" -ErrorAction SilentlyContinue
if ($svc) {
    if ($svc.Status -eq "Running") {
        Stop-Service "WinFsp.Launcher" -Force -ErrorAction SilentlyContinue
    }
    sc.exe delete "WinFsp.Launcher" 2>&1 | Out-Null
    Ok "Service removed"
}

# Uninstall via winget
$winget = Get-Command winget -ErrorAction SilentlyContinue
if ($winget) {
    $installed = winget list --id WinFsp.WinFsp 2>&1 | Select-String "WinFsp"
    if ($installed) {
        Write-Host "    Uninstalling WinFsp..." -ForegroundColor Gray
        winget uninstall --id WinFsp.WinFsp --accept-source-agreements 2>&1 | Out-Null
        Ok "WinFsp uninstalled"
    } else {
        Info "WinFsp not in winget list"
    }
}

# Remove installation directory
$winfspDir = "C:\Program Files (x86)\WinFsp"
if (Test-Path $winfspDir) {
    Remove-Item $winfspDir -Force -Recurse -ErrorAction SilentlyContinue
    Ok "Removed: $winfspDir"
}

# Remove registry entries
$regPaths = @(
    "HKLM:\SOFTWARE\WOW6432Node\WinFsp",
    "HKLM:\SOFTWARE\WinFsp"
)
foreach ($reg in $regPaths) {
    if (Test-Path $reg) {
        Remove-Item $reg -Force -Recurse -ErrorAction SilentlyContinue
        Ok "Removed registry: $reg"
    }
}

# --- 7. Done ---
Step "Cleanup complete"
Write-Host ""
Write-Host "  All peerfold files, configs, logs, and WinFsp have been removed." -ForegroundColor Green
Write-Host "  The project source code in $ProjectRoot is untouched." -ForegroundColor Gray
