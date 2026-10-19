@echo off
rem install.bat - Windows entry point for the peerfold installer.
rem Standalone: fetches install.ps1 from the GitHub release, then runs it.
rem   curl -fsSL -o install.bat https://github.com/ispengya/peerfold/releases/latest/download/install.bat && install.bat

set "SCRIPT=%~dp0install.ps1"
set "REMOTE=https://github.com/ispengya/peerfold/releases/latest/download/install.ps1"

if not exist "%SCRIPT%" (
    set "SCRIPT=%TEMP%\peerfold-install.ps1"
    echo        Downloading install.ps1...
    curl -fsSL -o "%SCRIPT%" "%REMOTE%"
    if errorlevel 1 (
        echo        [X] Failed to download install.ps1 from %REMOTE%
        exit /b 1
    )
)

powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%SCRIPT%" %*
exit /b %ERRORLEVEL%
