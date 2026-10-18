@echo off
cd /d "%~dp0"

rem 兼容两种布局：
rem   - GitHub Release 解压后 wb2api.exe 在脚本同级根目录（zip 五件套）
rem   - 源码树 / 本地构建时 wb2api.exe 在 bin\ 子目录
rem 编码约定：本文件必须保持 GBK，禁止另存为 UTF-8
rem （cmd 按控制台代码页解析批处理，UTF-8 中文会乱码串行并被误判为命令）
set "WB2API_EXE="
if exist "wb2api.exe" set "WB2API_EXE=wb2api.exe"
if not defined WB2API_EXE (
  if exist "bin\wb2api.exe" set "WB2API_EXE=bin\wb2api.exe"
)

if not defined WB2API_EXE (
  if exist "go.mod" if exist "cmd" (
    echo [提示] 当前目录是源码树，不含预编译好的 wb2api.exe。
    echo         推荐直接下载 Release 里的 Windows 预编译包，解压后双击 start.bat：
    echo         https://github.com/yuechuya/WorkBuddy2API-Gateway/releases/latest
    echo         文件名形如 wb2api-gateway-v*-windows-amd64.zip
    echo         也可以在本地构建：
    echo         go build -trimpath -ldflags="-s -w" -o wb2api.exe ./cmd/server
    pause
    exit /b 1
  )
  echo [ERROR] wb2api.exe not found.
  echo         Expected either:
  echo           %CD%\wb2api.exe
  echo           %CD%\bin\wb2api.exe
  echo         Get a prebuilt binary from GitHub Releases, or build from source:
  echo         go build -trimpath -ldflags="-s -w" -o bin\wb2api.exe ./cmd/server
  pause
  exit /b 1
)

rem 幂等：:7863 已在监听就跳过（再起一个实例也只会绑定失败退出，不算错误）
netstat -ano | findstr ":7863" | findstr "LISTENING" >nul
if not errorlevel 1 (
  echo [SKIP] An instance is already listening on :7863
  echo        Health: curl http://127.0.0.1:7863/healthz
  ping -n 4 127.0.0.1 >nul
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

"%WB2API_EXE%" -config config.json
echo.
echo [Server exited] press any key to close ...
pause >nul
