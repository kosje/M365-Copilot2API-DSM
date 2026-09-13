@echo off
title M365-Copilot2API 一键导出账号
setlocal
set "HERE=%~dp0"
set "BASH=C:\Program Files\Git\bin\bash.exe"
if not exist "%BASH%" set "BASH=%ProgramFiles%\Git\bin\bash.exe"
if not exist "%BASH%" set "BASH=bash"
echo [INFO] 正在导出当前运行实例的账号（accounts.json / api-keys.json / admin-password.json）...
"%BASH%" "%HERE%migrate.sh" export
echo.
echo [INFO] 归档已生成在 data 目录下（m365-migration-*.tar.gz）。
echo 将其复制到目标机器后，用 migrate.bat import 该文件即可完成迁移。
pause >nul
endlocal
