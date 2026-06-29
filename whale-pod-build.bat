@echo off
setlocal

echo ========================================
echo  Whale Pod Build
echo ========================================

set POD_DIR=D:\src\whale-pod

if not exist "%POD_DIR%" (
    echo ERROR: whale-pod project not found at %POD_DIR%
    exit /b 1
)

REM Step 1: Build whale daemon first (needed alongside pod)
echo [1/3] Building whale.exe...
cd /d "%~dp0"
go build -o bin\whale.exe ./cmd/whale/
if %ERRORLEVEL% neq 0 (
    echo ERROR: whale build failed
    exit /b %ERRORLEVEL%
)

REM Step 2: Build whale-pod
echo [2/3] Building whale-pod.exe...
cd /d "%POD_DIR%"
call build.bat
if %ERRORLEVEL% neq 0 (
    echo ERROR: whale-pod build failed
    exit /b %ERRORLEVEL%
)

REM Step 3: Copy outputs to common bin
echo [3/3] Copying outputs...
cd /d "%~dp0"
if not exist bin mkdir bin
copy /Y "%POD_DIR%\build\bin\whale-pod.exe" bin\whale-pod.exe >nul 2>&1

echo ========================================
echo  BUILD OK
echo  bin\whale.exe       - daemon
echo  bin\whale-pod.exe   - desktop app
echo ========================================
