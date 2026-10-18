# start-all.ps1 — 一键启动 peerfold 全部组件
# 用法: .\start-all.ps1 [端口] [挂载点]
# 需要管理员权限（挂载需要）

param(
    [string]$Port = "7000",
    [string]$MountPoint = (Join-Path ([Environment]::GetFolderPath("UserProfile")) "peerfold")
)

$ProjectRoot = $PSScriptRoot
$BinPath = Join-Path $ProjectRoot "peerfold.exe"

# 如果 binary 不存在，先构建
if (-not (Test-Path $BinPath)) {
    Write-Host "peerfold.exe 不存在，正在构建..." -ForegroundColor Yellow
    $env:GOMODCACHE = Join-Path $env:TEMP "peerfold-gomodcache"
    $env:GOPROXY = "off"
    $env:GOFLAGS = "-mod=mod"
    if (-not (Test-Path $env:GOMODCACHE)) {
        $env:GOPROXY = "https://proxy.golang.org,direct"
        Remove-Item Env:GOFLAGS -ErrorAction SilentlyContinue
    }
    & go build -o $BinPath ./cmd/peerfold
    if ($LASTEXITCODE -ne 0) {
        Write-Host "构建失败，请先运行 .\setup.ps1" -ForegroundColor Red
        exit 1
    }
}

Write-Host "=== 启动 peerfold ===" -ForegroundColor Cyan
Write-Host "Server:   127.0.0.1:$Port"
Write-Host "Mount:    $MountPoint"
Write-Host ""

# 清理挂载点残留
if (Test-Path $MountPoint) {
    Write-Host "挂载点已存在，正在清理..." -ForegroundColor Yellow
    Remove-Item $MountPoint -Force -Recurse -ErrorAction SilentlyContinue
}

# 创建测试共享目录
$aliceDir = "C:\peerfold-demo\alice"
$bobDir = "C:\peerfold-demo\bob"
foreach ($d in @($aliceDir, $bobDir)) {
    if (-not (Test-Path $d)) { New-Item -ItemType Directory -Path $d -Force | Out-Null }
}

# 创建测试文件
if (-not (Test-Path "$aliceDir\hello.txt")) {
    Set-Content "$aliceDir\hello.txt" "hello from alice"
}
if (-not (Test-Path "$bobDir\hello.txt")) {
    Set-Content "$bobDir\hello.txt" "hello from bob"
}

Write-Host "启动 server..." -ForegroundColor Gray
Start-Process -FilePath $BinPath -ArgumentList "server","--listen","127.0.0.1:$Port" -WindowStyle Minimized
Start-Sleep -Seconds 1

Write-Host "启动 client alice..." -ForegroundColor Gray
Start-Process -FilePath $BinPath -ArgumentList "client","--server","127.0.0.1:$Port","--name","alice","--share","docs=C:\peerfold-demo\alice" -WindowStyle Minimized
Start-Sleep -Seconds 1

Write-Host "启动 client bob..." -ForegroundColor Gray
Start-Process -FilePath $BinPath -ArgumentList "client","--server","127.0.0.1:$Port","--name","bob","--share","docs=C:\peerfold-demo\bob" -WindowStyle Minimized
Start-Sleep -Seconds 1

Write-Host "启动 mount (挂载到 $MountPoint)..." -ForegroundColor Gray
Start-Process -FilePath $BinPath -ArgumentList "mount","--server","127.0.0.1:$Port","--mount",$MountPoint -WindowStyle Minimized
Start-Sleep -Seconds 3

Write-Host ""
Write-Host "=== 全部启动 ===" -ForegroundColor Green
Write-Host "  挂载点: $MountPoint"
Write-Host "    alice\docs\  <- C:\peerfold-demo\alice"
Write-Host "    bob\docs\    <- C:\peerfold-demo\bob"
Write-Host ""
Write-Host "  查看日志:" -ForegroundColor Gray
Write-Host "    Get-Content $env:USERPROFILE\peerfold-server\log.txt -Tail 20"
Write-Host "    Get-Content $env:USERPROFILE\peerfold-client-alice\log.txt -Tail 20"
Write-Host ""
Write-Host "  停止所有: .\stop-all.ps1" -ForegroundColor Yellow
