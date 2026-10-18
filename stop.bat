@echo off
echo Stopping wb2api.exe ...
taskkill /IM wb2api.exe /F >nul 2>&1
if errorlevel 1 (
  echo [INFO] wb2api.exe is not running.
) else (
  echo [OK] stopped.
)
ping -n 3 127.0.0.1 >nul
