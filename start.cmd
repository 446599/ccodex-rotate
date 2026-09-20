@echo off
chcp 65001 >nul 2>&1
setlocal
cd /d "%~dp0"
set "DIR=%~dp0"
set "CFG=%USERPROFILE%\.ccodex-rotate\config.json"

set "BIN=%DIR%ccodex-rotate.exe"
if not exist "%BIN%" set "BIN=%DIR%dist\ccodex-rotate-windows-amd64.exe"
if not exist "%BIN%" set "BIN=%DIR%dist\ccodex-rotate-windows-arm64.exe"
if not exist "%BIN%" (
  echo [ccodex-rotate] ccodex-rotate.exe not found. Run build.bat or build.ps1 first.
  echo.
  pause
  exit /b 1
)

if not exist "%CFG%" (
  echo [ccodex-rotate] First run. Configure your subscription link first, for example:
  echo   "%BIN%" sub add "https://your-subscription-url"
  echo.
  "%BIN%" init --config "%CFG%"
  echo.
)

echo [ccodex-rotate] Starting. It does NOT change the system proxy and does NOT stop your Clash.
echo Press Ctrl+C to stop and restore the Codex config.
echo.
"%BIN%" serve --config "%CFG%"
echo.
echo [ccodex-rotate] exited with code %ERRORLEVEL%.
echo If the window closed too fast, run this file from a terminal to see the full output.
pause
