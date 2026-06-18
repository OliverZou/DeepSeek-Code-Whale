@echo off
setlocal

REM === Step 1: Build frontend ===
echo [1/2] Building frontend...
cd /d "%~dp0cmd\whale-pod\frontend"

where npm >nul 2>&1
if %ERRORLEVEL% neq 0 (
    echo ERROR: npm not found in PATH
    exit /b 1
)

echo   Running npm install...
call npm install
if %ERRORLEVEL% neq 0 (
    echo ERROR: npm install failed
    exit /b %ERRORLEVEL%
)

echo   Running npm run build...
call npm run build
if %ERRORLEVEL% neq 0 (
    echo ERROR: npm run build failed
    exit /b %ERRORLEVEL%
)

if not exist "dist\index.html" (
    echo ERROR: dist\index.html not found - frontend build may have failed
    exit /b 1
)
echo   Frontend build OK.

REM === Step 2: Build Go binary with Wails ===
echo [2/2] Building whale-pod.exe...
cd /d "%~dp0cmd\whale-pod"

set GOPROXY=https://goproxy.cn,direct

REM -s skips Wails own frontend build (we already built it)
REM -clean removes stale build artifacts
wails build -s -clean -tags teamlog
if %ERRORLEVEL% neq 0 (
    echo BUILD FAILED
    exit /b %ERRORLEVEL%
)

cd /d "%~dp0"
if not exist bin mkdir bin
copy /Y "cmd\whale-pod\build\bin\whale-pod.exe" "bin\whale-pod.exe" >nul 2>&1
echo BUILD OK - bin\whale-pod.exe
