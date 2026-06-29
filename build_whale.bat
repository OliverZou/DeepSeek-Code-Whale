@echo off
cd /d "%~dp0"
echo Building whale.exe...
go build -tags teamlog -o bin\whale.exe .\cmd\whale
if %ERRORLEVEL% NEQ 0 (
    echo FAILED
    exit /b %ERRORLEVEL%
)
echo OK: bin\whale.exe

REM Copy to whale-pod build dir if it exists
set POD_BUILD=D:\src\whale-pod\build\bin
if exist "%POD_BUILD%" (
    copy /Y bin\whale.exe "%POD_BUILD%\whale.exe" >nul
    echo Copied to %POD_BUILD%
)
