@echo off
setlocal
title M365-Copilot2API Builder
set "ROOT=D:\work\M365-build"
set "GO_DIR=%ROOT%\go"
set "GO_ZIP=%ROOT%\go1.27.1.windows-amd64.zip"
set "EXE=%ROOT%\m365-copilot2api.exe"
set "GO_URL=https://go.dev/dl/go1.27.1.windows-amd64.zip"

if not exist "%GO_DIR%\bin\go.exe" (
    echo [INFO] Go toolchain not found. Downloading ...
    if not exist "%GO_ZIP%" (
        curl -L -o "%GO_ZIP%" "%GO_URL%" || echo [ERROR] Go download failed (need internet).
    )
    if exist "%GO_ZIP%" (
        echo [INFO] Extracting Go ...
        if exist "%GO_DIR%" rmdir /s /q "%GO_DIR%"
        tar -xf "%GO_ZIP%" -C "%ROOT%"
    )
)

if not exist "%GO_DIR%\bin\go.exe" (
    echo [ERROR] Go still missing. Cannot build. Run existing exe instead.
    goto :done
)

set "PATH=%GO_DIR%\bin;%PATH%"
set "GOPATH=%ROOT%\gopath"
set "GOPROXY=https://goproxy.cn,direct"
set "GOOS=windows"
set "GOARCH=amd64"
set "CGO_ENABLED=0"

echo [INFO] Building m365-copilot2api.exe ...
cd /d "%ROOT%"

rem --- shared kernel version (same source as the Linux fpk build) ---
set "APP_VERSION=dev"
if exist "%ROOT%\version.txt" (
    set /p APP_VERSION=<"%ROOT%\version.txt"
)
if not defined APP_VERSION set "APP_VERSION=dev"
if "%APP_VERSION%"=="" set "APP_VERSION=dev"
rem Fallback: read version= from the fpk manifest if version.txt is missing.
if "%APP_VERSION%"=="dev" (
    for /f "tokens=2 delims==" %%v in ('findstr /b "version=" "%ROOT%\..\M365-fpk\m365-copilot2api\manifest" 2^>nul') do (
        if not "%%v"=="" set "APP_VERSION=%%v"
    )
)
echo [INFO] Version: %APP_VERSION%

go build -trimpath -ldflags="-s -w -X m365-copilot2api/internal/web.Version=%APP_VERSION%" -o "%EXE%" ./cmd/server
if errorlevel 1 (
    echo [ERROR] Build failed. See output above.
    pause
    exit /b 1
)
echo [OK] Built: %EXE%

:done
endlocal
