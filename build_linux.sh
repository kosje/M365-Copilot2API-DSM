#!/usr/bin/env bash
# 在 Linux / WSL 下构建 linux/amd64 二进制（SPK 与 Docker 的载荷）。
#
# 此前这个脚本把构建目录、Go 路径和版本来源都写死在一台机器的目录布局上
# （D:/work/M365-build、C:/Users/pguoy/go/bin/go.exe，版本取自仓库外的 FPK
# manifest），换台机器必然失败，升版本也容易漏改。现在全部改为相对脚本自身推导，
# Go 直接用 PATH 里的那个，版本优先取命令行参数、其次取 spk/INFO。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="${1:-${ROOT}/m365-copilot2api-linux-amd64}"
VER="${2:-$(grep -m1 '^version=' "${ROOT}/spk/INFO" | cut -d'"' -f2 | cut -d- -f1)}"

if ! command -v go >/dev/null 2>&1; then
    echo "error: go not found on PATH" >&2
    exit 1
fi

export GOOS=linux
export GOARCH=amd64
export CGO_ENABLED=0
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

echo "go version: $(go version)"
echo "version:    ${VER}"

# go:embed 读 internal/web/web/，构建前先同步前端，否则打进的是陈旧页面。
echo "=== sync web/ -> internal/web/web/ ==="
cp -f "${ROOT}"/web/*.html "${ROOT}/internal/web/web/"

echo "=== build linux/amd64 -> ${OUT} ==="
cd "${ROOT}"
# -a 强制全量重建，确保 //go:embed 的前端资源被重新打包而不是命中缓存。
go build -a -trimpath \
    -ldflags="-s -w -X m365-copilot2api/internal/web.Version=${VER}" \
    -o "${OUT}" ./cmd/server

ls -l "${OUT}"
