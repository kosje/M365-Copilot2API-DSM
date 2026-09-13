#!/usr/bin/env bash
set -e
export GOPATH="D:/work/M365-build/gopath"
export GOCACHE="D:/work/M365-build/gocache"
export GOPROXY="https://goproxy.cn,direct"
export GOOS=linux
export GOARCH=amd64
export CGO_ENABLED=0
cd "D:/work/M365-build"
echo "go version:"
"C:/Users/pguoy/go/bin/go.exe" version
echo "=== sync web/ -> internal/web/web/ (go:embed reads internal/web/web/) ==="
# The //go:embed directive in internal/web/security_http.go resolves relative to
# that file's directory, so internal/web/web/ is what gets compiled in. Keep the
# two copies in sync automatically to avoid stale embedded assets.
cp -f web/index.html web/import.html web/login.html web/conversation.html web/debug.html internal/web/web/
echo "=== build linux/amd64 -> FPK app dir ==="
# -a forces full rebuild (incl. //go:embed web assets) so frontend changes are actually baked in.
"C:/Users/pguoy/go/bin/go.exe" build -a -trimpath -ldflags="-s -w" -o "D:/work/M365-fpk/m365-copilot2api/app/m365-copilot2api" ./cmd/server
echo "BUILD_EXIT=$?"
