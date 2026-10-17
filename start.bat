@echo off
chcp 65001 >nul
cd /d "%~dp0"

if not exist "bin\wb2api.exe" (
  echo [ERROR] bin\wb2api.exe not found.
  echo         Get a prebuilt binary from GitHub Releases, or build from source:
  echo         go build -trimpath -ldflags="-s -w" -o bin\wb2api.exe ./cmd/server
  pause
  exit /b 1
)

rem Idempotent: if :7863 is already listening, skip (a second instance would
rem just fail to bind and exit -- that is not an error).
netstat -ano | findstr ":7863" | findstr "LISTENING" >nul
if not errorlevel 1 (
  echo [SKIP] An instance is already listening on :7863
  echo        Health: curl http://127.0.0.1:7863/healthz
  timeout /t 4 /nobreak >nul
  exit /b 0
)

if not exist "auths" mkdir auths
if not exist "data"  mkdir data

echo ============================================================
echo   WorkBuddy2API-Gateway
echo   Panel  : http://127.0.0.1:7863/panel/
echo   Health : http://127.0.0.1:7863/healthz
echo   Stop   : close this window, or run stop.bat
echo ============================================================
echo.

bin\wb2api.exe -config config.json
echo.
echo [Server exited] press any key to close ...
pause >nul
