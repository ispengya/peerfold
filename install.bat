@echo off
setlocal
rem install.bat — Windows entry point for the peerfold installer.
rem Works both from a cloned repo (uses the local install.ps1) and as a
rem standalone download (fetches install.ps1 first).
rem
rem   curl -fsSL -o install.bat https://raw.githubusercontent.com/ispengya/peerfold/main/install.bat && install.bat

set "SCRIPT=%~dp0install.ps1"
set "REMOTE=https://raw.githubusercontent.com/ispengya/peerfold/main/install.ps1"

if not exist "%SCRIPT%" (
    set "SCRIPT=%TEMP%\peerfold-install.ps1"
    echo        Downloading install.ps1...
    powershell.exe -NoProfile -ExecutionPolicy Bypass -Command ^
        "try { Invoke-WebRequest -Uri '%REMOTE%' -OutFile '%SCRIPT%' -UseBasicParsing } catch { Write-Host $_.Exception.Message; exit 1 }"
    if errorlevel 1 (
        echo        [X] Failed to download install.ps1
        exit /b 1
    )
)

powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%SCRIPT%" %*
exit /b %ERRORLEVEL%
