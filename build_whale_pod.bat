@echo off
setlocal

cd /d "%~dp0"

set GOPROXY=https://goproxy.cn,direct

echo Building whale-pod with teamlog ...
cd cmd\whale-pod
wails build -s -skipbindings -tags teamlog 2>&1

if %ERRORLEVEL% neq 0 (
    cd /d "%~dp0"
    echo BUILD FAILED
    exit /b %ERRORLEVEL%
)

cd /d "%~dp0"
if not exist bin mkdir bin
copy /Y cmd\whale-pod\build\bin\whale-pod.exe bin\whale-pod.exe >nul 2>&1

echo BUILD OK - bin\whale-pod.exe
