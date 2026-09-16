#!/usr/bin/env bash
#
# 一条命令走完发版：构建 SPK → 发 GitHub Release → 更新套件来源 feed。
#
#   ./spk/release.sh <版本号>
#
# 例：./spk/release.sh 1.5.7
#
# 版本号即 Go 内部版本与 git tag（tag 会加 v 前缀）。SPK 的 INFO version
# 需要事先改好（DSM 只认 X.Y.Z-BBBB 格式，与这里的版本号是两套编号）。
#
# 需要：bash、go、git、gh（已登录）、jq、md5sum。Windows 下在 Git Bash 里跑，
# 但 tar 不保留权限位，所以最终打包建议在 Linux/WSL 执行 build-spk.sh。
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VER="${1:?用法: release.sh <版本号>，如 1.5.7}"
TAG="v$VER"
REPO="kosje/M365-Copilot2API-DSM"
PKG="m365-copilot2api"
SPK="$ROOT/$PKG-$VER.spk"
PAGES="$ROOT/.gh-pages"

step() { printf '\n\033[1m==> %s\033[0m\n' "$1"; }

step "检查工作区"
[ -z "$(git -C "$ROOT" status --porcelain)" ] || { echo "有未提交改动，先提交再发版"; exit 1; }

step "把版本号写入 spk/INFO"
# INFO 的 version 是 DSM 唯一认的编号（X.Y.Z-BBBB），此前靠人工维护，出过
# 套件中心显示 1.6.5-0004 而控制台显示 1.5.7 的事故。这里以命令行传入的版本号
# 为唯一来源自动改写：同版本递增 build 号，换版本从 0001 起。
INFO="$ROOT/spk/INFO"
CUR="$(grep -m1 '^version=' "$INFO" | cut -d'"' -f2)"
CUR_VER="${CUR%%-*}"
CUR_BUILD="${CUR##*-}"
# A malformed INFO (no -BBBB suffix) must not abort the release in
# arithmetic expansion.
case "$CUR_BUILD" in
    ''|*[!0-9]*) CUR_BUILD=0 ;;
esac
if [ "$CUR_VER" = "$VER" ]; then
    BUILD="$(printf '%04d' $((10#${CUR_BUILD} + 1)))"
else
    BUILD="0001"
fi
NEWVER="${VER}-${BUILD}"
if [ "$NEWVER" != "$CUR" ]; then
    sed -i.bak "s/^version=\"[^\"]*\"/version=\"${NEWVER}\"/" "$INFO" && rm -f "$INFO.bak"
    echo "  INFO version: $CUR -> $NEWVER"
    git -C "$ROOT" add spk/INFO
    git -C "$ROOT" commit -q -m "chore(spk): INFO version $NEWVER"
else
    echo "  INFO version 已是 $NEWVER"
fi
git -C "$ROOT" diff --quiet "@{upstream}" 2>/dev/null || echo "  提醒：本地与远程 main 不一致，记得 push"

step "构建 SPK $VER"
"$ROOT/spk/build-spk.sh" "$VER"
[ -f "$SPK" ] || { echo "构建产物不存在: $SPK"; exit 1; }

step "生成校验和"
BIN="$ROOT/.spkbin/$PKG-linux-amd64"
mkdir -p "$(dirname "$BIN")"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -a -trimpath \
    -ldflags="-s -w -X m365-copilot2api/internal/web.Version=$VER" -o "$BIN" "$ROOT/cmd/server"
( cd "$ROOT" && sha256sum "$(basename "$SPK")" > checksums.txt && sha256sum -b "$BIN" | sed "s|.*\*.*/|$PKG-linux-amd64|" >> checksums.txt ) || true
sha256sum "$SPK" "$BIN"

step "发布 Release $TAG"
if gh release view "$TAG" --repo "$REPO" >/dev/null 2>&1; then
    echo "  $TAG 已存在，改为覆盖上传资产"
    gh release upload "$TAG" --repo "$REPO" --clobber "$SPK" "$BIN" "$ROOT/checksums.txt"
else
    echo "  创建 $TAG（发布说明请随后在网页端补充）"
    gh release create "$TAG" --repo "$REPO" --title "$TAG" --notes "见提交记录。" "$SPK" "$BIN" "$ROOT/checksums.txt"
fi
# README 让用户比对 Release 页的 SHA256，而 checksums.txt 此前只生成不上传。
echo "  已附带 checksums.txt"

step "更新套件来源 feed"
[ -d "$PAGES" ] || { echo "缺少 gh-pages 工作树，先执行：git worktree add .gh-pages origin/gh-pages"; exit 1; }
INFOVER="$(tar xOf "$SPK" INFO | grep -m1 '^version=' | cut -d'"' -f2)"
LINK="https://github.com/$REPO/releases/download/$TAG/$(basename "$SPK")"
MD5="$(md5sum "$SPK" | cut -d' ' -f1)"
SIZE="$(wc -c < "$SPK" | tr -d ' ')"
jq --arg v "$INFOVER" --arg l "$LINK" --arg m "$MD5" --argjson s "$SIZE" \
   '.packages[0].version=$v | .packages[0].link=$l | .packages[0].md5=$m | .packages[0].size=$s' \
   "$PAGES/index.json" > "$PAGES/index.json.tmp" && mv "$PAGES/index.json.tmp" "$PAGES/index.json"
git -C "$PAGES" add -A
git -C "$PAGES" commit -q -m "feed: $INFOVER ($TAG)"
git -C "$PAGES" push -q origin gh-pages

step "完成"
echo "  Release : https://github.com/$REPO/releases/tag/$TAG"
echo "  Feed    : https://kosje.github.io/M365-Copilot2API-DSM/index.json  ($INFOVER)"
echo "  套件中心会在下次检查时提示更新。"
echo
echo "  发布后请核对 feed 里的 md5 与实际下载一致："
echo "    curl -sL -o /tmp/x.spk \"$LINK\" && md5sum /tmp/x.spk   # 应为 $MD5"
