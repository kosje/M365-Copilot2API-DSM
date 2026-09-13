@echo off
title M365-Copilot2API 账号迁移工具
setlocal
set "HERE=%~dp0"
set "BASH=C:\Program Files\Git\bin\bash.exe"
if not exist "%BASH%" set "BASH=%ProgramFiles%\Git\bin\bash.exe"
if not exist "%BASH%" set "BASH=bash"
"%BASH%" "%HERE%migrate.sh" %*
endlocal
