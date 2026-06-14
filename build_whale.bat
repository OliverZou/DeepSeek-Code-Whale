@echo off
cd /d "%~dp0"
echo Building whale.exe...
go build -tags teamlog -o bin\whale.exe .\cmd\whale
if %ERRORLEVEL% EQU 0 (
    echo OK: bin/whale.exe
) else (
    echo FAILED
    exit /b %ERRORLEVEL%
)
