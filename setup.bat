@echo off
setlocal
title M365-Copilot2API Setup (one-click recovery)
set "ROOT=D:\work\M365-build"
set "EXE=%ROOT%\m365-copilot2api.exe"
set "START=%ROOT%\start-m365-copilot2api.bat"
set "SHORTCUT=%USERPROFILE%\Desktop\M365-Copilot2API.bat"

echo ============================================
echo  M365-Copilot2API - one-click recovery
echo ============================================

if not exist "%SHORTCUT%" (
    echo [INFO] Recreating desktop shortcut ...
    > "%SHORTCUT%" echo @echo off
    >> "%SHORTCUT%" echo call "%START%"
    echo [OK] Shortcut: %SHORTCUT%
) else (
    echo [INFO] Desktop shortcut already exists.
)

if not exist "%EXE%" (
    echo [WARN] m365-copilot2api.exe missing. Rebuilding ...
    call "%ROOT%\build.bat"
) else (
    echo [OK] Binary present: %EXE%
)

echo [INFO] Launching service ...
call "%START%"

endlocal
