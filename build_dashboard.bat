@echo off
cd /d "%~dp0cmd\dashboard"
echo Building dashboard with Wails...
wails build
if %ERRORLEVEL% NEQ 0 (
    echo FAILED
    exit /b %ERRORLEVEL%
)
copy /y "build\bin\whale-dashboard.exe" "..\..\bin\whale-dashboard.exe" >nul
echo OK: bin/whale-dashboard.exe
