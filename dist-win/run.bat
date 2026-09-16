@echo off
setlocal
cd /d "%~dp0"

rem ============================================================
rem  M365-Copilot2API 本机转发服务 - 双击启动
rem  可选：在同目录放置 m365.env 覆盖以下默认值
rem    M365_LISTEN=127.0.0.1:4141   监听地址（仅本机）
rem    M365_DATA_DIR=<本目录>\data   数据/配置目录
rem    HTTPS_PROXY=...               上游出网代理（如需）
rem ============================================================

rem --- 可选本地覆盖 ---
if exist "%~dp0m365.env" call "%~dp0m365.env"

if not defined M365_LISTEN set "M365_LISTEN=127.0.0.1:4141"
if not defined M365_DATA_DIR set "M365_DATA_DIR=%~dp0data"

if not exist "%~dp0m365-copilot2api.exe" (
    echo [ERROR] 未找到 m365-copilot2api.exe
    echo         请先双击 build.bat 编译（首次需联网下载 Go 工具链），
    echo         或把已编译的 m365-copilot2api.exe 放到本目录。
    pause
    exit /b 1
)

if not exist "%M365_DATA_DIR%" mkdir "%M365_DATA_DIR%" >nul 2>&1

echo ============================================================
echo  M365-Copilot2API 转发服务（本机）
echo  监听地址 : http://%M365_LISTEN%
echo  管理/聊天 : http://%M365_LISTEN%/
echo  数据目录 : %M365_DATA_DIR%
echo  自定义模型接入 base_url : http://%M365_LISTEN%/v1
echo  按 Ctrl+C 停止服务
echo ============================================================

rem 延迟 2 秒后打开默认浏览器（服务就绪后再访问，失败可手动刷新）
start "" /min cmd /c "timeout /t 2 /nobreak >nul & start http://%M365_LISTEN%/"

"%~dp0m365-copilot2api.exe"
