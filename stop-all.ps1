# stop-all.ps1 — 停止 peerfold 所有组件
Write-Host "停止所有 peerfold 进程..." -ForegroundColor Cyan

Get-Process peerfold -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Seconds 1

$MountPoint = Join-Path ([Environment]::GetFolderPath("UserProfile")) "peerfold"
if (Test-Path $MountPoint) {
    Write-Host "清理挂载点 $MountPoint ..." -ForegroundColor Gray
    Remove-Item $MountPoint -Force -Recurse -ErrorAction SilentlyContinue
}

Write-Host "已停止" -ForegroundColor Green
