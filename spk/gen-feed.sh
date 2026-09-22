#!/usr/bin/env bash
#
# 生成群晖「套件来源」的静态 feed（docs/index.json），供 GitHub Pages 托管。
#
#   ./spk/gen-feed.sh <spk 文件> <下载直链> [输出路径]
#
# 例：
#   ./spk/gen-feed.sh m365-copilot2api-1.6.6.spk \
#     https://github.com/kosje/M365-Copilot2API-DSM/releases/download/v1.6.6/m365-copilot2api-1.6.6.spk \
#     .gh-pages/index.json
#
# 输出路径缺省是 docs/index.json。**注意 GitHub Pages 实际服务的是 gh-pages
# 分支根目录**，所以真正生效的那份是 `.gh-pages/index.json`；release.sh 与
# release.yml 都显式传这个路径。此前两条路径各写各的，Pages 读的那份和脚本
# 生成的那份可能不一致。
#
# 套件中心会对该 URL 发请求并读取 {"packages":[...]}。GitHub Pages 只能提供
# 静态 GET，所以无法按请求的 arch/build 过滤。项目只发一个 x86-64 包，
# SPK 自身的 INFO.arch 已经足以让套件中心在 ARM 机型上拒绝安装（那是安装期
# 校验）；feed 条目里的 arch 只影响"是否提示更新"，且字段类型必须与 DSM 期望
# 一致，否则可能导致整个 feed 不可读。所以默认不写，需要时用
# FEED_EMIT_ARCH=1 打开，并在真机上确认套件中心仍能读到更新提示。
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SPKFILE="${1:?用法: gen-feed.sh <spk 文件> <下载直链> [输出路径]}"
LINK="${2:?用法: gen-feed.sh <spk 文件> <下载直链> [输出路径]}"
# 输出路径可以指定，因为 Pages 服务的是 gh-pages 分支，不是 docs/。
OUTFILE="${3:-$ROOT/docs/index.json}"
OUTDIR="$(dirname "$OUTFILE")"
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

# 图标必须与 feed 放在同一目录：thumbnail 用的是 Pages 上的绝对地址。
mkdir -p "$OUTDIR"
cp -f "$ROOT/spk/ui/images/icon_72.png"  "$OUTDIR/icon_72.png"
cp -f "$ROOT/spk/ui/images/icon_256.png" "$OUTDIR/icon_256.png"

CHANGELOG="${CHANGELOG:-详见 $MAINT_URL/releases}"

command -v jq >/dev/null 2>&1 || {
    echo "jq is required to generate the feed; no file was written" >&2
    exit 1
}

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
   }' > "$OUTFILE"

# arch / os_min_ver 默认不写：SPK 的 INFO.arch 已经在安装期做校验，而 feed 里这两
# 个字段的期望类型没有把握，写错会让套件中心整个读不到本 feed。需要时用
# FEED_EMIT_ARCH=1 打开（单独一趟 jq，保持本脚本不依赖 bash 数组，这样 CI 里用
# sh -n 也能检查它）。
if [ "${FEED_EMIT_ARCH:-0}" = "1" ]; then
    jq --arg arch "$ARCH" --arg osmin "$OSMIN" \
       '.packages[0].arch = $arch | .packages[0].os_min_ver = $osmin' \
       "$OUTFILE" > "$OUTFILE.tmp" && mv "$OUTFILE.tmp" "$OUTFILE"
fi

jq empty "$OUTFILE"
echo "==> 已生成 $OUTFILE"
echo "    套件: $PKG $VER   arch=$ARCH   os_min_ver=$OSMIN"
echo "    大小: $SIZE 字节   md5: $MD5"
echo "    下载: $LINK"
echo
echo "套件来源地址（添加到 套件中心 → 设置 → 套件来源）:"
echo "    $PAGES_BASE/index.json"
if [ "${FEED_EMIT_ARCH:-0}" != "1" ]; then
    echo
    echo "提示：feed 未包含 arch/os_min_ver（INFO 里是 arch=$ARCH  os_min_ver=$OSMIN）。"
    echo "      这是有意的，见文件头的说明；确认无误后可用 FEED_EMIT_ARCH=1 打开。"
fi
