@echo off
setlocal enabledelayedexpansion
title M365-Copilot2API Builder

rem Default to this script's own directory so a fresh clone builds without editing
rem paths. Override with:  set M365_BUILD_ROOT=D:\somewhere
if defined M365_BUILD_ROOT (set "ROOT=%M365_BUILD_ROOT%") else (set "ROOT=%~dp0")
if "%ROOT:~-1%"=="\" set "ROOT=%ROOT:~0,-1%"

set "GO_CACHE=%ROOT%\.build\go"
set "GO_DIR=%ROOT%\.build\toolchain\go"
set "EXE=%ROOT%\m365-copilot2api.exe"
set "GOPATH=%ROOT%\.build\gopath"
set "GOCACHE=%ROOT%\.build\gocache"

rem Take the Go version from go.mod rather than hardcoding it: the pinned value
rem had already drifted (1.27.1 against a go.mod that says 1.25), which is how a
rem bootstrap ends up downloading a toolchain the module refuses.
set "GO_VER="
for /f "tokens=2" %%v in ('findstr /b /c:"go " "%ROOT%\go.mod"') do set "GO_VER=%%v"
if not defined GO_VER (
    echo [ERROR] cannot read the go directive from go.mod
    goto :done
)
echo [INFO] go.mod requires go %GO_VER%

if not exist "%GO_DIR%\bin\go.exe" (
    echo [INFO] Go toolchain not found. Downloading go%GO_VER% ...
    if not exist "%GO_CACHE%" mkdir "%GO_CACHE%"
    set "GO_ZIP=%GO_CACHE%\go%GO_VER%.windows-amd64.zip"
    if not exist "!GO_ZIP!" (
        curl -fL -o "!GO_ZIP!" "https://go.dev/dl/go%GO_VER%.windows-amd64.zip"
        if errorlevel 1 echo [ERROR] Go download failed ^(need internet^).
    )
    if exist "!GO_ZIP!" (
        echo [INFO] Extracting Go ...
        if exist "%GO_DIR%" rmdir /s /q "%GO_DIR%"
        mkdir "%GO_DIR%"
        tar -xf "!GO_ZIP!" -C "%GO_DIR%" --strip-components=1
    )
)

if not exist "%GO_DIR%\bin\go.exe" (
    echo [ERROR] Go still missing. Cannot build.
    goto :done
)

set "PATH=%GO_DIR%\bin;%PATH%"
set "GOPROXY=https://goproxy.cn,direct"
set "GOOS=windows"
set "GOARCH=amd64"
set "CGO_ENABLED=0"

rem go:embed reads internal/web/web/, so sync before building. Without this step
rem an edit under web\ is silently ignored on Windows and the console serves the
rem stale page - build-spk.sh has always done this copy, build.bat never did.
echo [INFO] Syncing web\*.html -^> internal\web\web\ ...
copy /y "%ROOT%\web\*.html" "%ROOT%\internal\web\web\" >nul
if errorlevel 1 (
    echo [ERROR] front-end sync failed
    pause
    exit /b 1
)

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

rem -a forces a full rebuild so //go:embed assets are re-baked rather than cached.
go build -a -trimpath -ldflags="-s -w -X m365-copilot2api/internal/web.Version=%APP_VERSION%" -o "%EXE%" ./cmd/server
if errorlevel 1 (
    echo [ERROR] Build failed. See output above.
    pause
    exit /b 1
)
echo [OK] Built: %EXE%

:done
endlocal
