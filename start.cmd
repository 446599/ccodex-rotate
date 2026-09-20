@echo off
setlocal
rem Windows launcher for ccodex-rotate. Double-click start.cmd.
set "DIR=%~dp0"
set "CFG=%USERPROFILE%\.ccodex-rotate\config.json"

set "BIN=%DIR%ccodex-rotate.exe"
if not exist "%BIN%" set "BIN=%DIR%dist\ccodex-rotate-windows-amd64.exe"
if not exist "%BIN%" set "BIN=%DIR%dist\ccodex-rotate-windows-arm64.exe"
if not exist "%BIN%" (
  echo 找不到 ccodex-rotate.exe，请先运行 build.ps1 或 build.bat
  pause
  exit /b 1
)

if not exist "%CFG%" (
  echo 首次运行：请先配置订阅链接，例如：
  echo   "%BIN%" sub add "https://你的订阅链接"
  "%BIN%" init --config "%CFG%"
)

echo 启动 ccodex-rotate（不修改系统代理，也不影响本地 Clash）。按 Ctrl+C 停止并还原 Codex 配置。
"%BIN%" serve --config "%CFG%"
pause
