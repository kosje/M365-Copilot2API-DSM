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
go build -trimpath -ldflags="-s -w" -o "%EXE%" ./cmd/server
if errorlevel 1 (
    echo [ERROR] Build failed. See output above.
    pause
    exit /b 1
)
echo [OK] Built: %EXE%

:done
endlocal
