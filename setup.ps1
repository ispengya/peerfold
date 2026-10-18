# setup.ps1 — peerfold 一键环境配置（Windows）
#
# 用法（以管理员身份运行 PowerShell）：
#   .\setup.ps1              检查并安装所有依赖，构建 binary
#   .\setup.ps1 -Quick       跳过安装检查，只构建
#   .\setup.ps1 -Start       配置完成后直接启动 server + mount
#
# 做的事：
#   1. 检查/安装 Go
#   2. 检查/安装 WinFsp
#   3. 构建 peerfold.exe
#   4. 创建配置和日志目录
#   5. 生成启动脚本

param(
    [switch]$Quick,
    [switch]$Start
)

$ErrorActionPreference = "Stop"
$ProjectRoot = $PSScriptRoot
$BinPath = Join-Path $ProjectRoot "peerfold.exe"
$GoModCache = Join-Path $env:TEMP "peerfold-gomodcache"

function Write-Step($msg) {
    Write-Host ""
    Write-Host "==> $msg" -ForegroundColor Cyan
}

function Write-Ok($msg) {
    Write-Host "    [OK] $msg" -ForegroundColor Green
}

function Write-Warn($msg) {
    Write-Host "    [!]  $msg" -ForegroundColor Yellow
}

function Write-Fail($msg) {
    Write-Host "    [X]  $msg" -ForegroundColor Red
}

# ─── 1. 检查 Go ───────────────────────────────────────────────
Write-Step "检查 Go 编译器"

$goCmd = Get-Command go -ErrorAction SilentlyContinue
if ($goCmd) {
    $goVersion = & go version 2>&1
    Write-Ok "Go 已安装: $goVersion"
} else {
    Write-Warn "Go 未安装"
    if ($Quick) {
        Write-Fail "Quick 模式跳过安装，请先手动安装 Go: https://go.dev/dl/"
        exit 1
    }

    # 尝试用 winget 安装
    $winget = Get-Command winget -ErrorAction SilentlyContinue
    if ($winget) {
        Write-Host "    尝试通过 winget 安装 Go..." -ForegroundColor Gray
        try {
            winget install --id GoLang.Go --accept-source-agreements --accept-package-agreements
            Write-Ok "Go 安装完成"
            Write-Warn "请关闭并重新打开终端，然后重新运行此脚本"
            exit 0
        } catch {
            Write-Fail "winget 安装失败: $_"
            Write-Host "    请手动下载安装: https://go.dev/dl/" -ForegroundColor Yellow
            exit 1
        }
    } else {
        Write-Fail "未找到 winget，请手动安装 Go: https://go.dev/dl/"
        exit 1
    }
}

# ─── 2. 检查 WinFsp ───────────────────────────────────────────
Write-Step "检查 WinFsp"

$winfspService = Get-Service "WinFsp.Launcher" -ErrorAction SilentlyContinue
$winfspInstall = Test-Path "C:\Program Files (x86)\WinFsp"
$winfspReg = Test-Path "HKLM:\SOFTWARE\WOW6432Node\WinFsp"

if ($winfspService) {
    Write-Ok "WinFsp 服务已注册: $($winfspService.Status)"
    if ($winfspService.Status -ne "Running") {
        Write-Host "    正在启动 WinFsp 服务..." -ForegroundColor Gray
        Start-Service "WinFsp.Launcher" -ErrorAction SilentlyContinue
        Write-Ok "WinFsp 服务已启动"
    }
} elseif ($winfspInstall -or $winfspReg) {
    Write-Ok "WinFsp 已安装（服务未注册，可能需要重启）"
} else {
    Write-Warn "WinFsp 未安装"
    if ($Quick) {
        Write-Fail "Quick 模式跳过安装，请先手动安装 WinFsp: https://winfsp.dev/rel/"
        exit 1
    }

    $winget = Get-Command winget -ErrorAction SilentlyContinue
    if ($winget) {
        Write-Host "    尝试通过 winget 安装 WinFsp..." -ForegroundColor Gray
        try {
            winget install --id WinFsp.WinFsp --accept-source-agreements --accept-package-agreements
            Write-Ok "WinFsp 安装完成"
        } catch {
            Write-Fail "winget 安装失败: $_"
            Write-Host "    请手动下载安装: https://winfsp.dev/rel/" -ForegroundColor Yellow
            Write-Host "    下载 .msi 安装包，运行安装向导即可" -ForegroundColor Yellow
            exit 1
        }
    } else {
        Write-Fail "未找到 winget，请手动安装 WinFsp: https://winfsp.dev/rel/"
        Write-Host "    下载 .msi 安装包，运行安装向导即可" -ForegroundColor Yellow
        exit 1
    }
}

# ─── 3. 构建 binary ───────────────────────────────────────────
Write-Step "构建 peerfold.exe"

# 离线构建支持：如果默认 GOMODCACHE 为空且无网络，使用备用缓存
$env:GOMODCACHE = $GoModCache
$env:GOPROXY = "off"
$env:GOFLAGS = "-mod=mod"

if (-not (Test-Path $GoModCache)) {
    Write-Host "    备用模块缓存不存在，尝试在线下载依赖..." -ForegroundColor Gray
    $env:GOPROXY = "https://proxy.golang.org,direct"
    Remove-Item Env:GOFLAGS -ErrorAction SilentlyContinue
}

try {
    & go build -o $BinPath ./cmd/peerfold
    if ($LASTEXITCODE -eq 0) {
        Write-Ok "构建成功: $BinPath"
    } else {
        Write-Fail "构建失败 (exit $LASTEXITCODE)"
        exit 1
    }
} catch {
    Write-Fail "构建出错: $_"
    exit 1
}

# ─── 4. 创建配置和日志目录 ────────────────────────────────────
Write-Step "创建配置目录"

$Home = [Environment]::GetFolderPath("UserProfile")

$dirs = @(
    (Join-Path $Home "peerfold-server"),
    (Join-Path $Home "peerfold-client-alice"),
    (Join-Path $Home "peerfold-client-bob")
)

foreach ($dir in $dirs) {
    if (-not (Test-Path $dir)) {
        New-Item -ItemType Directory -Path $dir -Force | Out-Null
    }
}

Write-Ok "配置目录已就绪:"
foreach ($dir in $dirs) {
    Write-Host "    $dir" -ForegroundColor Gray
}

# ─── 5. 生成启动脚本 ──────────────────────────────────────────
Write-Step "生成启动脚本"

# start-all.ps1 — 一键启动所有组件
$startAll = @"
# start-all.ps1 — 启动 peerfold 全部组件
# 用法: .\start-all.ps1 [端口] [挂载点]
# 需要管理员权限（挂载需要）

param(
    [string]`$Port = "7000",
    [string]`$MountPoint = (Join-Path ([Environment]::GetFolderPath("UserProfile")) "peerfold")
)

`$ProjectRoot = `$PSScriptRoot
`$BinPath = Join-Path `$ProjectRoot "peerfold.exe"

Write-Host "=== 启动 peerfold ===" -ForegroundColor Cyan
Write-Host "Server:   127.0.0.1:`$Port"
Write-Host "Mount:    `$MountPoint"
Write-Host ""

# 检查挂载点残留
if (Test-Path `$MountPoint) {
    Write-Host "挂载点已存在，正在清理..." -ForegroundColor Yellow
    Remove-Item `$MountPoint -Force -Recurse -ErrorAction SilentlyContinue
}

# 创建测试目录（如果不存在）
`$aliceDir = "C:\peerfold-demo\alice"
`$bobDir = "C:\peerfold-demo\bob"
if (-not (Test-Path `$aliceDir)) { New-Item -ItemType Directory -Path `$aliceDir -Force | Out-Null }
if (-not (Test-Path `$bobDir)) { New-Item -ItemType Directory -Path `$bobDir -Force | Out-Null }

Write-Host "启动 server..." -ForegroundColor Gray
Start-Process -FilePath `$BinPath -ArgumentList "server","--listen","127.0.0.1:`$Port" -WindowStyle Minimized
Start-Sleep -Seconds 1

Write-Host "启动 client alice..." -ForegroundColor Gray
Start-Process -FilePath `$BinPath -ArgumentList "client","--server","127.0.0.1:`$Port","--name","alice","--share","docs=C:\peerfold-demo\alice" -WindowStyle Minimized
Start-Sleep -Seconds 1

Write-Host "启动 client bob..." -ForegroundColor Gray
Start-Process -FilePath `$BinPath -ArgumentList "client","--server","127.0.0.1:`$Port","--name","bob","--share","docs=C:\peerfold-demo\bob" -WindowStyle Minimized
Start-Sleep -Seconds 1

Write-Host "启动 mount (挂载到 `$MountPoint)..." -ForegroundColor Gray
Start-Process -FilePath `$BinPath -ArgumentList "mount","--server","127.0.0.1:`$Port","--mount",`$MountPoint -WindowStyle Minimized
Start-Sleep -Seconds 3

Write-Host ""
Write-Host "=== 全部启动 ===" -ForegroundColor Green
Write-Host "挂载点: `$MountPoint"
Write-Host "  alice\docs\  — 来自 C:\peerfold-demo\alice"
Write-Host "  bob\docs\    — 来自 C:\peerfold-demo\bob"
Write-Host ""
Write-Host "停止所有: .\stop-all.ps1" -ForegroundColor Yellow
"@

# stop-all.ps1 — 停止所有 peerfold 进程
$stopAll = @"
# stop-all.ps1 — 停止 peerfold 所有组件
Write-Host "停止所有 peerfold 进程..." -ForegroundColor Cyan
Get-Process peerfold -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Seconds 1

`$MountPoint = Join-Path ([Environment]::GetFolderPath("UserProfile")) "peerfold"
if (Test-Path `$MountPoint) {
    Write-Host "清理挂载点..." -ForegroundColor Gray
    Remove-Item `$MountPoint -Force -Recurse -ErrorAction SilentlyContinue
}

Write-Host "已停止" -ForegroundColor Green
"@

Set-Content -Path (Join-Path $ProjectRoot "start-all.ps1") -Value $startAll -Encoding UTF8
Set-Content -Path (Join-Path $ProjectRoot "stop-all.ps1") -Value $stopAll -Encoding UTF8

Write-Ok "已生成 start-all.ps1 / stop-all.ps1"

# ─── 6. 完成 ──────────────────────────────────────────────────
Write-Step "环境配置完成"

Write-Host ""
Write-Host "  可用命令:" -ForegroundColor Cyan
Write-Host "    .\start-all.ps1       一键启动 server + 2 client + mount"
Write-Host "    .\stop-all.ps1        一键停止所有"
Write-Host ""
Write-Host "  或手动启动四个终端:" -ForegroundColor Cyan
Write-Host "    .\peerfold.exe server --listen 127.0.0.1:7000"
Write-Host "    .\peerfold.exe client --server 127.0.0.1:7000 --name alice --share docs=C:\peerfold-demo\alice"
Write-Host "    .\peerfold.exe client --server 127.0.0.1:7000 --name bob --share docs=C:\peerfold-demo\bob"
Write-Host "    .\peerfold.exe mount --server 127.0.0.1:7000 --mount C:\Users\你\peerfold"
Write-Host ""

if ($Start) {
    Write-Step "启动所有组件..."
    & (Join-Path $ProjectRoot "start-all.ps1")
}
