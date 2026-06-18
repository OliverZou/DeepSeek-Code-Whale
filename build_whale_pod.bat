@echo off
setlocal

cd /d "%~dp0cmd\whale-pod"

set GOPROXY=https://goproxy.cn,direct

echo [1/4] Building frontend...
cd frontend
call npm run build 2>&1
if %ERRORLEVEL% neq 0 ( echo FRONTEND BUILD FAILED & exit /b %ERRORLEVEL% )
cd ..

echo [2/4] Staging production files...
copy /Y frontend\dist\index.html frontend\index.html >nul 2>&1
if exist frontend\assets rmdir /s /q frontend\assets >nul 2>&1
xcopy /E /I /Y frontend\dist\assets frontend\assets >nul 2>&1

echo [3/4] Building Wails app...
wails build -tags teamlog 2>&1
if %ERRORLEVEL% neq 0 ( echo WAILS BUILD FAILED & exit /b %ERRORLEVEL% )

echo [4/4] Restoring dev index.html...
echo ^<^^!DOCTYPE html^>^<html lang="zh"^>^<head^>^<meta charset="UTF-8" /^>^<meta name="viewport" content="width=device-width, initial-scale=1.0" /^>^<title^>Whale Pod^</title^>^</head^>^<body^>^<div id="root"^>^</div^>^<script type="module" src="/src/main.tsx"^>^</script^>^</body^>^</html^> > frontend\index.html

cd /d "%~dp0"
if not exist bin mkdir bin
copy /Y cmd\whale-pod\build\bin\whale-pod.exe bin\whale-pod.exe >nul 2>&1

echo BUILD OK - bin\whale-pod.exe
