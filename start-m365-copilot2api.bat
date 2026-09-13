@echo off
title M365-Copilot2API
setlocal
set "EXE=D:\work\M365-build\m365-copilot2api.exe"
set "DATA_DIR=D:\work\M365-build\data"
netstat -ano 2>nul | findstr ":4141" | findstr "LISTENING" >nul
if %errorlevel%==0 (
    echo [INFO] m365-copilot2api is already running on 127.0.0.1:4141.
    echo Close this window. No new instance started.
    pause >nul
    exit /b 0
)
echo [INFO] Starting m365-copilot2api (data dir: %DATA_DIR%) ...
echo Keep this window open to keep the service running. To stop: close window or Ctrl+C.
echo.
set "M365_DATA_DIR=D:\work\M365-build\data"
set "M365_CONFIG=D:\work\M365-build\data\accounts.json"
set "M365_API_KEYS=D:\work\M365-build\data\api-keys.json"
"%EXE%"
echo.
echo [INFO] m365-copilot2api exited. Close this window.
pause >nul
