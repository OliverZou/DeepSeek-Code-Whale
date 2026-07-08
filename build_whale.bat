@echo off
cd /d "%~dp0"
echo Building whale.exe...
go build -tags teamlog -o bin\whale.exe .\cmd\whale
if %ERRORLEVEL% NEQ 0 (
    echo FAILED
    exit /b %ERRORLEVEL%
)
echo OK: bin\whale.exe
