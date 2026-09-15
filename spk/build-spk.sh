#!/usr/bin/env bash
#
# 构建群晖 DSM SPK。需在 Linux 或 WSL 下运行（Windows 的 tar 不保留权限位）。
#
#   ./spk/build-spk.sh [版本号]
#
# 版本号省略时取 spk/INFO 里 version= 的前半段。产物为 ./m365-copilot2api-<版本>.spk
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SPK="$ROOT/spk"
BUILD="$ROOT/.spkbuild"
PKG="$(grep -m1 '^package=' "$SPK/INFO" | cut -d'"' -f2)"
VER="${1:-$(grep -m1 '^version=' "$SPK/INFO" | cut -d'"' -f2 | cut -d- -f1)}"
OUT="$ROOT/${PKG}-${VER}.spk"

echo "==> 构建 ${PKG} ${VER}"

# go:embed 读的是 internal/web/web/，构建前同步前端，避免打进陈旧页面
cp -f "$ROOT"/web/*.html "$ROOT/internal/web/web/"

echo "==> 编译 linux/amd64"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -a -trimpath \
    -ldflags="-s -w -X m365-copilot2api/internal/web.Version=${VER}" \
    -o "$BUILD/payload/bin/m365-copilot2api" "$ROOT/cmd/server"

echo "==> 组装 package.tgz"
rm -rf "$BUILD/payload/ui"
cp -r "$SPK/ui" "$BUILD/payload/ui"
chmod 755 "$BUILD/payload/bin/m365-copilot2api"
chmod 644 "$BUILD/payload/ui/config" "$BUILD/payload/ui/images"/*
# Force the payload mode in the archive. This keeps the server executable when
# the staging tree lives on a filesystem without Unix mode bits (for example a
# Windows checkout used to prepare the package before final Linux validation).
tar czf "$BUILD/package.tgz" --owner=root --group=root --mode=755 -C "$BUILD/payload" .

echo "==> 打包 SPK"
rm -rf "$BUILD/spk" && mkdir -p "$BUILD/spk"
cp "$SPK/INFO" "$SPK/PACKAGE_ICON.PNG" "$SPK/PACKAGE_ICON_256.PNG" "$BUILD/spk/"
cp -r "$SPK/scripts" "$SPK/conf" "$SPK/WIZARD_UIFILES" "$BUILD/spk/"
cp "$BUILD/package.tgz" "$BUILD/spk/"
chmod 755 "$BUILD/spk/scripts"/*
chmod 644 "$BUILD/spk/INFO" "$BUILD/spk/conf"/* "$BUILD/spk/WIZARD_UIFILES"/* "$BUILD/spk"/*.PNG
# SPK 本体是不压缩的 tar，INFO 必须排在最前
( cd "$BUILD/spk" && tar cf "$OUT" --owner=root --group=root --format=gnu \
    INFO package.tgz scripts conf WIZARD_UIFILES PACKAGE_ICON.PNG PACKAGE_ICON_256.PNG )

# ---- 自检：声明与交付必须一致 ----
# 第一版 SPK 就是因为 INFO 声明了 dsmuidir 却没把 ui/ 放进 package.tgz，
# DSM 在安装末尾注册桌面应用时失败，并把套件置为无法修复的损坏状态。
echo "==> 一致性自检"
UIDIR="$(grep -m1 '^dsmuidir=' "$SPK/INFO" | cut -d'"' -f2 || true)"
APPNAME="$(grep -m1 '^dsmappname=' "$SPK/INFO" | cut -d'"' -f2 || true)"
# 先把清单落盘再 grep：`tar ... | grep -q` 在 set -o pipefail 下会误报——
# grep -q 命中即退出，tar 收到 SIGPIPE 返回非零，整条管道被判为失败。
LIST="$BUILD/package.list"
tar tzf "$BUILD/package.tgz" > "$LIST"
if [ -n "$UIDIR" ]; then
    grep -q "^\./${UIDIR}/config$" "$LIST" \
        || { echo "✗ INFO 声明了 dsmuidir=${UIDIR}，但 package.tgz 内没有 ${UIDIR}/config"; exit 1; }
    echo "  ✓ package.tgz 含 ./${UIDIR}/config"

    KEY="$(jq -r '.".url" | keys[0]' "$SPK/ui/config")"
    [ "$KEY" = "$APPNAME" ] \
        || { echo "✗ ui/config 主键 ($KEY) 与 dsmappname ($APPNAME) 不一致"; exit 1; }
    echo "  ✓ ui/config 主键 == dsmappname"

    for s in 16 24 32 48 64 72 256; do
        grep -q "^\./${UIDIR}/images/icon_${s}.png$" "$LIST" \
            || { echo "✗ 缺少图标 icon_${s}.png"; exit 1; }
    done
    echo "  ✓ 7 个尺寸图标齐全"
fi
grep -q "^\./bin/m365-copilot2api$" "$LIST" \
    || { echo "✗ package.tgz 内没有 bin/m365-copilot2api"; exit 1; }
echo "  ✓ 二进制已就位"
BINMODE="$(tar tvzf "$BUILD/package.tgz" ./bin/m365-copilot2api | awk '{print $1}')"
case "$BINMODE" in
    -rwx*) echo "  ✓ 二进制归档权限可执行 ($BINMODE)" ;;
    *) echo "✗ 二进制归档权限不可执行 ($BINMODE)"; exit 1 ;;
esac
for f in start-stop-status postinst preuninst preupgrade postupgrade; do
    sh -n "$SPK/scripts/$f" || { echo "✗ $f 语法错误"; exit 1; }
done
echo "  ✓ 脚本语法检查通过"

# INFO 的 version 必须高于已发布的最新版，否则套件中心会拒绝升级安装。
# 这道检查是因为真出过一次：版本号只在本地临时目录里递增，仓库里的 INFO
# 一直停在旧版本，照着仓库构建出来的包比线上还旧。
FEED="$ROOT/.gh-pages/index.json"
if [ -f "$FEED" ] && command -v jq >/dev/null 2>&1; then
    PUBLISHED="$(jq -r '.packages[0].version // empty' "$FEED")"
    INFOVER="$(grep -m1 '^version=' "$SPK/INFO" | cut -d'"' -f2)"
    if [ -n "$PUBLISHED" ]; then
        newest="$(printf '%s\n%s\n' "$PUBLISHED" "$INFOVER" | sort -V | tail -1)"
        if [ "$INFOVER" = "$PUBLISHED" ]; then
            echo "  ! INFO version ($INFOVER) 与已发布版本相同 —— 套件中心不会视为升级"
        elif [ "$newest" != "$INFOVER" ]; then
            echo "✗ INFO version ($INFOVER) 低于已发布的 $PUBLISHED，套件中心会拒绝安装"
            exit 1
        else
            echo "  ✓ INFO version $INFOVER 高于已发布的 $PUBLISHED"
        fi
    fi
fi

rm -rf "$BUILD"
echo
echo "==> 完成: $OUT"
sha256sum "$OUT"
