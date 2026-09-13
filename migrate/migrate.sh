#!/bin/bash
# m365-copilot2api 账号迁移工具（跨平台：fnOS / Linux / Windows-GitBash）
#
# 用途：把“正在运行的账号”从一处数据目录导出为可移植归档，
#       再导入到另一个 M365-Copilot2API 实例的数据目录。
#
# 迁移包含的核心文件：
#   accounts.json        —— M365 账号与令牌（accessToken / refreshToken）
#   api-keys.json        —— 网关对外发放的 OpenAI 兼容 API Key
#   admin-password.json  —— 管理后台密码哈希（迁移后保持同一密码）
# 可选（--with-logs）：
#   stats.json / usage.jsonl —— 用量统计与日志
#
# 用法：
#   migrate.sh export [--out FILE] [--with-logs] [--data-dir DIR]
#   migrate.sh import <ARCHIVE> [--force] [--data-dir DIR]
#   migrate.sh verify <ARCHIVE>
#   migrate.sh help
#
# 数据目录自动探测顺序：
#   1) $M365_DATA_DIR        （显式指定，优先）
#   2) $M365_CONFIG 的父目录
#   3) $TRIM_PKGVAR/data      （fnOS 生命周期环境变量）
#   4) 脚本所在位置的 ../data 或 ./data
#   5) $HOME/.config/m365-copilot2api
set -u

ARCHIVE_VERSION="1"
TOOL_NAME="m365-copilot2api-migrate"

log(){ echo "[migrate] $*"; }
err(){ echo "[migrate][ERROR] $*" >&2; }

# 是否在 Windows(msys/cygwin/mingw) 下运行
OS_KIND="$(uname -s 2>/dev/null)"
IS_WIN=0
if echo "$OS_KIND" | grep -qiE 'mingw|msys|cygwin'; then IS_WIN=1; fi

# 规范化路径参数（Windows 风格 -> POSIX）
normalize(){
  local p="$1"
  if [ "$IS_WIN" = "1" ] && echo "$p" | grep -qE '^[A-Za-z]:[/\\]'; then
    p="$(cygpath -u "$p" 2>/dev/null || printf '%s' "$p")"
  fi
  printf '%s' "$p"
}

detect_data_dir(){
  if [ -n "${M365_DATA_DIR:-}" ]; then normalize "$M365_DATA_DIR"; return; fi
  if [ -n "${M365_CONFIG:-}" ]; then dirname "$(normalize "$M365_CONFIG")"; return; fi
  if [ -n "${TRIM_PKGVAR:-}" ]; then printf '%s' "$TRIM_PKGVAR/data"; return; fi
  local here="$(cd "$(dirname "$0")" 2>/dev/null && pwd)"
  [ -z "$here" ] && here="."
  if [ -f "$here/../data/accounts.json" ]; then printf '%s' "$here/../data"; return; fi
  if [ -f "$here/data/accounts.json" ]; then printf '%s' "$here/data"; return; fi
  printf '%s' "$HOME/.config/m365-copilot2api"
}

estimate_accounts(){
  local f="$1"
  [ -f "$f" ] || { printf '0'; return; }
  grep -o '"id"' "$f" 2>/dev/null | wc -l | tr -d ' '
}

do_export(){
  local out="" withlogs=0 data_override=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --out) out="$2"; shift 2;;
      --with-logs) withlogs=1; shift;;
      --data-dir) data_override="$2"; shift 2;;
      *) shift;;
    esac
  done
  if [ -n "$data_override" ]; then export M365_DATA_DIR="$(normalize "$data_override")"; fi
  local data="$(detect_data_dir)"
  if [ ! -f "$data/accounts.json" ]; then
    err "未找到 accounts.json，无法确定数据目录：$data"
    err "请用 --data-dir 指定，或设置环境变量 M365_DATA_DIR。"
    exit 1
  fi
  [ -z "$out" ] && out="$data/m365-migration-$(date +%Y%m%d-%H%M%S).tar.gz"

  local tmp="$(mktemp -d)"
  local bundle="$tmp/m365-migration"
  mkdir -p "$bundle/data"
  cp -f "$data/accounts.json"    "$bundle/data/" 2>/dev/null
  cp -f "$data/api-keys.json"    "$bundle/data/" 2>/dev/null
  cp -f "$data/admin-password.json" "$bundle/data/" 2>/dev/null
  local files_list='["accounts.json","api-keys.json","admin-password.json"]'
  if [ "$withlogs" = "1" ]; then
    cp -f "$data/stats.json"  "$bundle/data/" 2>/dev/null
    cp -f "$data/usage.jsonl" "$bundle/data/" 2>/dev/null
    files_list='["accounts.json","api-keys.json","admin-password.json","stats.json","usage.jsonl"]'
  fi
  local accts="$(estimate_accounts "$data/accounts.json")"
  cat > "$bundle/manifest.json" <<EOF
{
  "tool": "$TOOL_NAME",
  "archive_version": "$ARCHIVE_VERSION",
  "created_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "source_data_dir": "$data",
  "account_count_estimate": $accts,
  "files": $files_list
}
EOF
  if ! tar -czf "$out" -C "$tmp" m365-migration 2>/dev/null; then
    rm -rf "$tmp"
    err "归档写入失败：$out"
    exit 1
  fi
  rm -rf "$tmp"
  log "已导出归档：$out"
  log "账号数(估算)：$accts    数据目录：$data"
  log "包含文件：$files_list"
  if [ "$IS_WIN" = "1" ]; then out="$(cygpath -w "$out" 2>/dev/null || printf '%s' "$out")"; fi
  log "Windows 路径：$out"
}

do_import(){
  local src="${1:-}"; local force=0 data_override=""
  shift 2>/dev/null || true
  while [ $# -gt 0 ]; do
    case "$1" in
      --force) force=1; shift;;
      --data-dir) data_override="$2"; shift 2;;
      *) shift;;
    esac
  done
  if [ -z "$src" ] || [ ! -f "$src" ]; then
    err "用法：migrate.sh import <ARCHIVE> [--force] [--data-dir DIR]"
    exit 1
  fi
  src="$(normalize "$src")"
  if ! tar -tzf "$src" >/dev/null 2>&1; then
    err "归档无效或已损坏：$src"
    exit 1
  fi
  if [ -n "$data_override" ]; then export M365_DATA_DIR="$(normalize "$data_override")"; fi
  local data="$(detect_data_dir)"
  mkdir -p "$data"

  # 备份现有数据
  local bk="$data/backup-$(date +%Y%m%d-%H%M%S)"
  mkdir -p "$bk"
  for f in accounts.json api-keys.json admin-password.json stats.json usage.jsonl; do
    [ -f "$data/$f" ] && cp -f "$data/$f" "$bk/" 2>/dev/null
  done
  log "现有数据已备份至：$bk"

  if ! tar -xzf "$src" -C "$data" --strip-components=2 m365-migration/data 2>/dev/null; then
    err "解包失败：$src"
    exit 1
  fi

  # fnOS 下修正属主
  if [ "$(id -u 2>/dev/null)" = "0" ] && [ -n "${TRIM_USERNAME:-}" ]; then
    chown -R "${TRIM_USERNAME}:${TRIM_GROUPNAME:-$TRIM_USERNAME}" "$data" 2>/dev/null
  fi
  if [ "$IS_WIN" = "1" ]; then data="$(cygpath -w "$data" 2>/dev/null || printf '%s' "$data")"; fi
  log "导入完成。数据目录：$data"
  log "请重启 M365-Copilot2API 服务使新账号生效。"
  [ "$force" = "1" ] || log "（本次未加 --force，亦未删除任何原有文件；如需清空旧账号请手动处理备份目录。）"
}

do_verify(){
  local src="${1:-}"
  if [ -z "$src" ] || [ ! -f "$src" ]; then
    err "用法：migrate.sh verify <ARCHIVE>"
    exit 1
  fi
  src="$(normalize "$src")"
  if ! tar -tzf "$src" >/dev/null 2>&1; then
    err "归档无效或已损坏：$src"
    exit 1
  fi
  log "归档有效。内容清单："
  tar -tzf "$src"
  # 尝试打印 manifest
  local mtmp="$(mktemp -d)"
  tar -xzf "$src" -C "$mtmp" m365-migration/manifest.json 2>/dev/null
  if [ -f "$mtmp/m365-migration/manifest.json" ]; then
    echo "----- manifest.json -----"
    cat "$mtmp/m365-migration/manifest.json"
    echo
  fi
  rm -rf "$mtmp"
}

usage(){
  cat <<'EOF'
m365-copilot2api 账号迁移工具

  export [--out FILE] [--with-logs] [--data-dir DIR]
      导出正在运行的账号为可移植归档（tar.gz）。
  import <ARCHIVE> [--force] [--data-dir DIR]
      将归档导入到目标实例的数据目录（导入前自动备份现有数据）。
  verify <ARCHIVE>
      校验归档完整性并打印清单。
  help
      显示本帮助。

数据目录自动探测：M365_DATA_DIR > M365_CONFIG父目录 > TRIM_PKGVAR/data > 脚本相邻 data/ > ~/.config/m365-copilot2api
EOF
}

case "${1:-help}" in
  export) shift; do_export "$@";;
  import) shift; do_import "$@";;
  verify) shift; do_verify "$@";;
  help|-h|--help) usage;;
  *) err "未知命令：$1"; usage; exit 1;;
esac
