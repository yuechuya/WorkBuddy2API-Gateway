@echo off
chcp 65001 >nul
echo Stopping wb2api.exe ...
taskkill /IM wb2api.exe /F >nul 2>&1
if errorlevel 1 (
  echo [INFO] wb2api.exe is not running.
) else (
  echo [OK] stopped.
)
timeout /t 2 >nul
