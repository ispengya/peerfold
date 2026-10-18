# setup-server.ps1 — peerfold server one-click setup
# Installs Go + WinFsp, builds the binary, starts the server.
# Run as Administrator.

$ErrorActionPreference = "Stop"
$ProjectRoot = $PSScriptRoot
$BinPath = Join-Path $ProjectRoot "peerfold.exe"
$GoModCache = Join-Path $env:TEMP "peerfold-gomodcache"
$Up = [Environment]::GetFolderPath("UserProfile")

# ── Helpers ──────────────────────────────────────────────────

function Header {
    Write-Host ""
    Write-Host "  peerfold" -ForegroundColor White
    Write-Host "  Federated filesystem — server setup" -ForegroundColor DarkGray
    Write-Host "  ------------------------------------" -ForegroundColor DarkGray
    Write-Host ""
}

function Section($num, $total, $title) {
    Write-Host ""
    Write-Host "  [$num/$total] $title" -ForegroundColor Cyan
}

function Ok($msg)   { Write-Host "       [OK] $msg" -ForegroundColor Green }
function Info($msg) { Write-Host "       $msg" -ForegroundColor DarkGray }
function Warn($msg) { Write-Host "       [!] $msg" -ForegroundColor Yellow }
function Fail($msg) { Write-Host "       [X] $msg" -ForegroundColor Red }

function ShowProgress($label, $estimatedSec) {
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $job = Start-Job -ScriptBlock $Action
    while ($job.State -eq 'Running') {
        $pct = [math]::Min(95, [int](($sw.Elapsed.TotalSeconds / $estimatedSec) * 100))
        Write-Progress -Activity $label -Status "$pct% complete" -PercentComplete $pct
        Start-Sleep -Milliseconds 300
    }
    Write-Progress -Activity $label -Completed
    $result = Receive-Job $job 2>&1
    Remove-Job $job
    return $result
}

function InstallPackage($id, $name, $estSec) {
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $job = Start-Job -ScriptBlock {
        param($pkgId)
        & winget install --id $pkgId --accept-source-agreements --accept-package-agreements 2>&1
    } -ArgumentList $id

    while ($job.State -eq 'Running') {
        $pct = [math]::Min(95, [int](($sw.Elapsed.TotalSeconds / $estSec) * 100))
        Write-Progress -Activity "Installing $name" -Status "Downloading and installing... $pct%" -PercentComplete $pct
        Start-Sleep -Milliseconds 500
    }
    Write-Progress -Activity "Installing $name" -Completed

    $out = Receive-Job $job 2>&1
    Remove-Job $job
    return $LASTEXITCODE -eq 0 -or $out -match "Successfully installed"
}

function BuildBinary {
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $job = Start-Job -ScriptBlock {
        param($root, $bin, $gmc)
        $env:GOMODCACHE = $gmc
        $env:GOPROXY = "off"
        $env:GOFLAGS = "-mod=mod"
        if (-not (Test-Path $gmc)) {
            $env:GOPROXY = "https://proxy.golang.org,direct"
            Remove-Item Env:GOFLAGS -ErrorAction SilentlyContinue
        }
        Push-Location $root
        & go build -o $bin ./cmd/peerfold 2>&1
        Pop-Location
    } -ArgumentList $ProjectRoot, $BinPath, $GoModCache

    $est = 20
    while ($job.State -eq 'Running') {
        $pct = [math]::Min(95, [int](($sw.Elapsed.TotalSeconds / $est) * 100))
        Write-Progress -Activity "Building peerfold.exe" -Status "Compiling Go source... $pct%" -PercentComplete $pct
        Start-Sleep -Milliseconds 400
    }
    Write-Progress -Activity "Building peerfold.exe" -Completed

    $out = Receive-Job $job 2>&1
    Remove-Job $job
    return (Test-Path $BinPath)
}

# ── Start ────────────────────────────────────────────────────

Header

$total = 5

# ── [1/5] Go ─────────────────────────────────────────────────

Section 1 $total "Go compiler"

$go = Get-Command go -ErrorAction SilentlyContinue
if ($go) {
    $ver = (& go version) -replace 'go version ', ''
    Ok "Go $ver already installed"
} else {
    Info "Go not found. Installing..."
    if (InstallPackage "GoLang.Go" "Go" 60) {
        Ok "Go installed successfully"
        Warn "Close and reopen this terminal, then run the script again."
        exit 0
    } else {
        Fail "Installation failed. Install manually: https://go.dev/dl/"
        exit 1
    }
}

# ── [2/5] WinFsp ─────────────────────────────────────────────

Section 2 $total "WinFsp (FUSE for Windows)"

$svc = Get-Service "WinFsp.Launcher" -ErrorAction SilentlyContinue
if ($svc) {
    if ($svc.Status -ne "Running") {
        Start-Service "WinFsp.Launcher" -ErrorAction SilentlyContinue
    }
    Ok "WinFsp service running"
} else {
    Info "WinFsp not found. Installing..."
    if (InstallPackage "WinFsp.WinFsp" "WinFsp" 45) {
        Ok "WinFsp installed successfully"
        Warn "Close and reopen this terminal, then run the script again."
        exit 0
    } else {
        Fail "Installation failed. Install manually: https://winfsp.dev/rel/"
        exit 1
    }
}

# ── [3/5] Build ──────────────────────────────────────────────

Section 3 $total "Build peerfold.exe"

if (Test-Path $BinPath) {
    $existing = Get-Item $BinPath
    Info "Binary exists (built $($existing.LastWriteTime.ToString('yyyy-MM-dd HH:mm')))"
    $rebuild = Read-Host "       Rebuild? [y/N]"
    if ($rebuild -ne 'y' -and $rebuild -ne 'Y') {
        Ok "Keeping existing binary"
    } else {
        Info "Compiling..."
        if (BuildBinary) {
            Ok "Build succeeded"
        } else {
            Fail "Build failed"
            exit 1
        }
    }
} else {
    Info "Compiling source code (first build downloads modules)..."
    if (BuildBinary) {
        $size = [math]::Round((Get-Item $BinPath).Length / 1MB, 1)
        Ok "Build succeeded — peerfold.exe ($size MB)"
    } else {
        Fail "Build failed"
        exit 1
    }
}

# ── [4/5] Config directories ─────────────────────────────────

Section 4 $total "Configuration directories"

$serverDir = Join-Path $Up "peerfold-server"
if (Test-Path $serverDir) {
    Ok "Already exists: $serverDir"
} else {
    New-Item -ItemType Directory -Path $serverDir -Force | Out-Null
    Ok "Created: $serverDir"
}

# ── [5/5] Start server ───────────────────────────────────────

Section 5 $total "Start server"

$running = Get-Process peerfold -ErrorAction SilentlyContinue
if ($running) {
    Warn "Server already running (PID $($running.Id))"
} else {
    Info "Launching server on 127.0.0.1:7000..."
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    Start-Process -FilePath $BinPath -ArgumentList "server","--listen","127.0.0.1:7000" -WindowStyle Minimized
    while ($sw.Elapsed.TotalSeconds -lt 3) {
        $pct = [int](($sw.Elapsed.TotalSeconds / 3) * 100)
        Write-Progress -Activity "Starting server" -Status "Waiting for startup... $pct%" -PercentComplete $pct
        Start-Sleep -Milliseconds 200
    }
    Write-Progress -Activity "Starting server" -Completed

    $check = Get-Process peerfold -ErrorAction SilentlyContinue
    if ($check) {
        Ok "Server started (PID $($check.Id))"
    } else {
        Fail "Server failed to start. Check $serverDir\log.txt"
        exit 1
    }
}

# ── Summary ──────────────────────────────────────────────────

Write-Host ""
Write-Host "  ------------------------------------" -ForegroundColor DarkGray
Write-Host "  Setup complete!" -ForegroundColor Green
Write-Host ""
Write-Host "  Server:    127.0.0.1:7000" -ForegroundColor White
Write-Host "  Config:    $serverDir\config.json" -ForegroundColor DarkGray
Write-Host "  Log:       $serverDir\log.txt" -ForegroundColor DarkGray
Write-Host ""
Write-Host "  Next: run setup-client.ps1 on a client machine" -ForegroundColor Yellow
Write-Host ""
