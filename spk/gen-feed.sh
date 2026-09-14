#!/usr/bin/env bash
#
# 生成群晖「套件来源」的静态 feed（docs/index.json），供 GitHub Pages 托管。
#
#   ./spk/gen-feed.sh <spk 文件> <下载直链>
#
# 例：
#   ./spk/gen-feed.sh m365-copilot2api-1.5.2.1.spk \
#     https://github.com/kosje/M365-Copilot2API-DSM/releases/download/v1.5.2.1/m365-copilot2api-1.5.2.1.spk
#
# 套件中心会对该 URL 发请求并读取 {"packages":[...]}。GitHub Pages 只能提供
# 静态 GET，所以无法按请求的 arch/build 过滤——本项目只发一个 x86-64 包，
# 由 DSM 依据条目里的 arch / os_min_ver 自行判断即可。
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SPKFILE="${1:?用法: gen-feed.sh <spk 文件> <下载直链>}"
LINK="${2:?用法: gen-feed.sh <spk 文件> <下载直链>}"
DOCS="$ROOT/docs"
PAGES_BASE="${PAGES_BASE:-https://kosje.github.io/M365-Copilot2API-DSM}"

[ -f "$SPKFILE" ] || { echo "找不到 $SPKFILE"; exit 1; }

# 直接从 SPK 里取 INFO，保证 feed 与包内元数据永远一致
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
tar xf "$SPKFILE" -C "$TMP" INFO

info() { grep -m1 "^$1=" "$TMP/INFO" | cut -d'"' -f2 || true; }

PKG="$(info package)"
VER="$(info version)"
DNAME="$(info displayname)"
DESC="$(info description)"
MAINT="$(info maintainer)"
MAINT_URL="$(info maintainer_url)"
ARCH="$(info arch)"
OSMIN="$(info os_min_ver)"

SIZE="$(wc -c < "$SPKFILE" | tr -d ' ')"
MD5="$(md5sum "$SPKFILE" | cut -d' ' -f1)"

# qinst/qstart 表示"无需向导即可安装/启动"。本套件带安装向导（要设管理员
# 密码），所以必须为 false，否则套件中心会跳过向导、密码无从设置。
HAS_WIZARD=true

mkdir -p "$DOCS"
cp -f "$ROOT/spk/ui/images/icon_72.png"  "$DOCS/icon_72.png"
cp -f "$ROOT/spk/ui/images/icon_256.png" "$DOCS/icon_256.png"

CHANGELOG="${CHANGELOG:-详见 $MAINT_URL/releases}"

jq -n \
  --arg package "$PKG" --arg version "$VER" --arg dname "$DNAME" \
  --arg desc "$DESC" --arg link "$LINK" --arg md5 "$MD5" \
  --argjson size "$SIZE" \
  --arg maintainer "$MAINT" --arg maintainer_url "$MAINT_URL" \
  --arg changelog "$CHANGELOG" \
  --arg thumb "$PAGES_BASE/icon_72.png" \
  --arg thumb2x "$PAGES_BASE/icon_256.png" \
  --argjson qinst "$([ "$HAS_WIZARD" = true ] && echo false || echo true)" \
  '{
     packages: [{
       package: $package,
       version: $version,
       dname: $dname,
       desc: $desc,
       link: $link,
       md5: $md5,
       size: $size,
       thumbnail: [$thumb],
       thumbnail_retina: [$thumb2x],
       qinst: $qinst,
       qupgrade: true,
       qstart: false,
       snapshot: [],
       maintainer: $maintainer,
       maintainer_url: $maintainer_url,
       distributor: $maintainer,
       distributor_url: $maintainer_url,
       changelog: $changelog
     }]
   }' > "$DOCS/index.json"

jq empty "$DOCS/index.json"
echo "==> 已生成 $DOCS/index.json"
echo "    套件: $PKG $VER   arch=$ARCH   os_min_ver=$OSMIN"
echo "    大小: $SIZE 字节   md5: $MD5"
echo "    下载: $LINK"
echo
echo "套件来源地址（添加到 套件中心 → 设置 → 套件来源）:"
echo "    $PAGES_BASE/index.json"
