<#
.SYNOPSIS
    一键发布双版本（Windows + fnOS/Linux）到 GitHub Release，两个版本均携带可自更新的原生二进制。

.DESCRIPTION
    1. 读取版本号（version.txt，单一来源）。
    2. 构建 Linux 二进制（build_linux.sh，自动注入版本号）。
    3. 交叉编译 Windows 二进制，产出「规范名 exe」（供 run.bat 双击）
       与「平台资产名 exe」（m365-copilot2api-windows-amd64.exe，供在线更新下载）。
    4. 打包 Windows zip（含 run.bat / build.bat / version.txt / 说明）。
    5. fnpack 打包 fnOS fpk。
    6. 在 GitHub 创建 Release v<ver>，上传 4 个资产：
       - m365-copilot2api-windows-<ver>.zip   （Windows 手动安装包）
       - m365-copilot2api-windows-amd64.exe    （Windows 在线更新原生二进制）
       - m365-copilot2api-linux-amd64          （Linux/fnOS 在线更新原生二进制）
       - m365-copilot2api.fpk                  （fnOS 应用中心手动安装包）

    要求：本机已安装 Go 工具链、Git（含 bash/curl）、fnpack.exe。
    鉴权：GitHub PAT 取自环境变量 $env:GITHUB_TOKEN；
          出网代理默认 http://127.0.0.1:3067（与 git push 同一代理）。
#>

[CmdletBinding()]
param(
    [string]$Version = "",
    [switch]$Draft = $false,
    # ---- 路径（默认贴合本机布局，可用参数覆盖） ----
    [string]$BuildDir   = "D:/work/M365-build",
    [string]$FpkSrcDir  = "D:/work/M365-fpk/m365-copilot2api",
    [string]$FpkOutDir  = "D:/work/M365-fpk",
    [string]$GoExe      = "C:/Users/pguoy/go/bin/go.exe",
    [string]$FnpackExe  = "D:/work/M365-fpk/fnpack.exe",
    [string]$Repo       = "my788525/M365-Copilot2API-FNOS",
    [string]$Proxy      = "http://127.0.0.1:3067",
    [string]$GithubToken = $env:GITHUB_TOKEN
)

$ErrorActionPreference = "Stop"

# ---------- 版本号 ----------
if (-not $Version) {
    $Version = (Get-Content (Join-Path $BuildDir "version.txt") -Raw).Trim()
}
if ($Version -notmatch '^\d+\.\d+\.\d+$') {
    throw "版本号格式非法: '$Version'（应为 x.y.z）"
}
$Tag = "v$Version"
Write-Host "[release] 版本 $Tag" -ForegroundColor Cyan

# ---------- 代理 + 鉴权 ----------
$env:HTTPS_PROXY  = $Proxy
$env:HTTP_PROXY   = $Proxy
$env:https_proxy  = $Proxy
$env:http_proxy   = $Proxy
if (-not $GithubToken) {
    throw "未提供 GitHub PAT：请设置环境变量 GITHUB_TOKEN 或在参数 -GithubToken 传入。"
}
$env:GH_TOKEN = $GithubToken

$DistDir = Join-Path $BuildDir "dist-win"
New-Item -ItemType Directory -Force -Path $DistDir | Out-Null

# ---------- 1) Linux 二进制 ----------
Write-Host "[release] 构建 Linux 二进制 ..." -ForegroundColor Cyan
$bash = Get-Command bash -ErrorAction SilentlyContinue
if (-not $bash) { $bash = Get-Command "C:/Program Files/Git/bin/bash.exe" -ErrorAction SilentlyContinue }
if (-not $bash) { throw "找不到 bash（需安装 Git）" }
& $bash.Source "build_linux.sh"
if ($LASTEXITCODE -ne 0) { throw "build_linux.sh 失败" }

$LinuxBin = Join-Path $FpkSrcDir "app/m365-copilot2api"
$LinuxAsset = Join-Path $FpkOutDir "m365-copilot2api-linux-amd64"
Copy-Item $LinuxBin $LinuxAsset -Force
Write-Host "[release]   Linux 资产 -> $LinuxAsset" -ForegroundColor DarkGray

# ---------- 2) Windows 二进制 ----------
Write-Host "[release] 交叉编译 Windows 二进制 ..." -ForegroundColor Cyan
$WinNamed = Join-Path $DistDir "m365-copilot2api-windows-amd64.exe"
$env:GOPATH    = Join-Path $BuildDir "gopath"
$env:GOCACHE   = Join-Path $BuildDir "gocache"
$env:GOPROXY   = "https://goproxy.cn,direct"
$env:GOOS      = "windows"
$env:GOARCH    = "amd64"
$env:CGO_ENABLED = "0"
& $GoExe build -trimpath "-ldflags=-s -w -X m365-copilot2api/internal/web.Version=$Version" -o $WinNamed ./cmd/server
if ($LASTEXITCODE -ne 0) { throw "Windows 交叉编译失败" }
# 规范名 exe（run.bat 双击用）
Copy-Item $WinNamed (Join-Path $DistDir "m365-copilot2api.exe") -Force
Write-Host "[release]   Windows 资产 -> $WinNamed" -ForegroundColor DarkGray

# ---------- 3) Windows zip ----------
Write-Host "[release] 打包 Windows zip ..." -ForegroundColor Cyan
$WinZip = Join-Path $FpkOutDir "m365-copilot2api-windows-$Version.zip"
$winFiles = @(
    (Join-Path $DistDir "run.bat"),
    (Join-Path $DistDir "build.bat"),
    (Join-Path $DistDir "version.txt"),
    (Join-Path $DistDir "m365.env.example"),
    (Join-Path $DistDir "README.txt"),
    (Join-Path $DistDir "m365-copilot2api.exe")
)
Compress-Archive -Force -Path $winFiles -DestinationPath $WinZip
Write-Host "[release]   $WinZip" -ForegroundColor DarkGray

# ---------- 4) fnOS fpk ----------
Write-Host "[release] fnpack 打包 fpk ..." -ForegroundColor Cyan
& $FnpackExe build -d $FpkSrcDir
if ($LASTEXITCODE -ne 0) { throw "fnpack 失败" }
$Fpk = Join-Path $FpkOutDir "m365-copilot2api.fpk"
Write-Host "[release]   $Fpk" -ForegroundColor DarkGray

# ---------- 5) GitHub Release ----------
$assets = @($WinZip, $WinNamed, $LinuxAsset, $Fpk)
Write-Host "[release] 创建 GitHub Release $Tag ..." -ForegroundColor Cyan

# 发布说明：取 manifest changelog 字段（去掉 "changelog=" 前缀与本版号前缀）
$changelogRaw = (Get-Content (Join-Path $FpkSrcDir "manifest") | Where-Object { $_ -like "changelog=*" } | Select-Object -First 1)
$notes = ""
if ($changelogRaw) {
    $notes = $changelogRaw.Substring("changelog=".Length)
    $notes = $notes -replace "^$Version\s*", ""
}

$draftFlag = if ($Draft) { "--draft" } else { "" }

$gh = Get-Command gh -ErrorAction SilentlyContinue
if ($gh) {
    Write-Host "[release] 使用 gh 创建 Release ..." -ForegroundColor DarkGray
    $args = @("release","create",$Tag,"-R",$Repo,"-t",$Tag,"-n",$notes) + $draftFlag
    $args += $assets
    & $gh.Source @args
    if ($LASTEXITCODE -ne 0) { throw "gh release create 失败" }
    Write-Host "[release] 完成：https://github.com/$Repo/releases/tag/$Tag" -ForegroundColor Green
    exit 0
}

# 回退：curl REST API
Write-Host "[release] 未找到 gh，使用 curl REST API ..." -ForegroundColor DarkGray
$headers = @("-H", "Authorization: Bearer $GithubToken", "-H", "Accept: application/vnd.github+json", "-H", "Content-Type: application/json")
$createBody = @{ tag_name = $Tag; name = $Tag; body = $notes; draft = [bool]$Draft; prerelease = $false } | ConvertTo-Json -Compress
$createOut = & curl.exe -s -X POST $headers -d $createBody "https://api.github.com/repos/$Repo/releases"
if ($LASTEXITCODE -ne 0) { throw "创建 Release 失败 (curl)" }
$rel = $createOut | ConvertFrom-Json
if (-not $rel.id) { throw "创建 Release 未返回 id：$createOut" }
foreach ($a in $assets) {
    $name = Split-Path $a -Leaf
    $ctype = if ($name -like "*.zip") { "application/zip" } else { "application/octet-stream" }
    Write-Host "[release]   上传 $name ..." -ForegroundColor DarkGray
    & curl.exe -s -X POST $headers[0],$headers[1] "-H","Content-Type: $ctype" --data-binary "@$a" "https://uploads.github.com/repos/$Repo/releases/$($rel.id)/assets?name=$name"
    if ($LASTEXITCODE -ne 0) { throw "上传资产 $name 失败" }
}
Write-Host "[release] 完成：https://github.com/$Repo/releases/tag/$Tag" -ForegroundColor Green
