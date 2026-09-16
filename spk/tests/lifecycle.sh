#!/usr/bin/env bash
# Functional harness for the DSM lifecycle hooks.
#
# The hooks decide whether M365 accounts survive an upgrade or an uninstall, so
# syntax checking is not enough: this runs the real scripts against a simulated
# package tree and asserts on the resulting files.
#
# It simulates the pessimistic case throughout - DSM wiping the package
# directory, which is the failure the snapshot exists to survive.
#
#   bash spk/tests/lifecycle.sh
#
# Exit code 0 when every case passes.

set -uo pipefail

# Some development shells export function wrappers for rm/cp/mkdir; drop them
# so the scripts under test see the real coreutils binaries.
unset -f rm 2>/dev/null || true
unset -f cp 2>/dev/null || true
unset -f mkdir 2>/dev/null || true

# Derive the repository root from this script's location so the harness works
# from any checkout.
REPO="${REPO:-$(cd "$(dirname "$0")/../.." && pwd)}"
SCRIPTS="$REPO/spk/scripts"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0
ok()   { echo "  PASS  $1"; PASS=$((PASS + 1)); }
bad()  { echo "  FAIL  $1"; FAIL=$((FAIL + 1)); }
check() { # check <desc> <expected> <actual>
    if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (expected '$2', got '$3')"; fi
}
check_file() { # check_file <desc> <path>
    if [ -f "$2" ]; then ok "$1"; else bad "$1 (missing $2)"; fi
}
check_absent() { # check_absent <desc> <path>
    if [ -e "$2" ]; then bad "$1 (still present: $2)"; else ok "$1"; fi
}

# Build a fresh simulated install. Sets PKGVAR, VOL, DATA.
new_env() {
    ROOT="$TMP/case-$1"
    VOL="$ROOT/volume1"
    PKGVAR="$ROOT/var"
    DATA="$PKGVAR/data"
    mkdir -p "$VOL/@appdata" "$DATA"
    PACKAGE="m365-copilot2api"
    export SYNOPKG_PKGNAME="$PACKAGE"
    export SYNOPKG_PKGVAR="$PKGVAR"
    export SYNOPKG_PKGDEST_VOL="$VOL"
    export SYNOPKG_TEMP_LOGFILE="$ROOT/hook.log"
    unset SYNOPKG_PKG_STATUS pkgwizard_admin_password pkgwizard_delete_data 2>/dev/null || true
    RETAIN="$VOL/@appdata/m365-copilot2api"
}
seed_accounts() {
    printf '{"accounts":[{"id":"a@example.com","email":"a@example.com","refreshToken":"secret"}]}\n' > "$DATA/accounts.json"
}

echo "=== 1. preupgrade snapshots OUTSIDE the package directory ==="
new_env 1
seed_accounts
sh "$SCRIPTS/preupgrade"; rc=$?
check "exit status" 0 "$rc"
check_file "snapshot holds accounts.json" "$RETAIN/preupgrade-snapshot/accounts.json"
check_absent "no snapshot left inside the package var dir" "$PKGVAR/data-preupgrade"
if grep -q "refreshToken" "$RETAIN/preupgrade-snapshot/accounts.json"; then
    ok "snapshot content is complete"
else
    bad "snapshot content is incomplete"
fi

echo
echo "=== 2. postupgrade restores after DSM wipes the package directory ==="
rm -rf "$PKGVAR"                      # the pessimistic case: var/ is gone
sh "$SCRIPTS/postupgrade"; rc=$?
check "exit status" 0 "$rc"
check_file "accounts.json restored" "$DATA/accounts.json"
check_absent "snapshot removed once the restore is verified" "$RETAIN/preupgrade-snapshot"

echo
echo "=== 3. postupgrade keeps the snapshot when the restore cannot be trusted ==="
new_env 3
seed_accounts
sh "$SCRIPTS/preupgrade" >/dev/null
rm -rf "$PKGVAR"
# Replace the good snapshot with an unusable one: files present, but no
# accounts.json in it.
rm -rf "$RETAIN/preupgrade-snapshot"
mkdir -p "$RETAIN/preupgrade-snapshot"
printf 'leftover\n' > "$RETAIN/preupgrade-snapshot/settings.json"
# A snapshot with files but no accounts.json is unusable: the hook must keep
# it rather than delete the only copy.
sh "$SCRIPTS/postupgrade"; rc=$?
check "exit status" 0 "$rc"
if [ -d "$RETAIN/preupgrade-snapshot" ]; then ok "unusable snapshot kept for recovery"; else bad "unusable snapshot was deleted"; fi
if grep -q "has no accounts.json" "$SYNOPKG_TEMP_LOGFILE" 2>/dev/null; then
    ok "the reason was logged"
else
    bad "no explanation logged"
fi

echo
echo "=== 4. a failed snapshot copy aborts the upgrade ==="
new_env 4
seed_accounts
mkdir -p "$RETAIN"
# Occupy the snapshot path with a regular file: a portable way to make `cp -a`
# fail, unlike permission bits which Windows ignores on directories.
printf 'occupied\n' > "$RETAIN/preupgrade-snapshot"
sh "$SCRIPTS/preupgrade" >/dev/null 2>&1; rc=$?
check "preupgrade fails loudly" 1 "$rc"
check_file "the original data is untouched" "$DATA/accounts.json"
if grep -q "snapshot copy failed" "$SYNOPKG_TEMP_LOGFILE" 2>/dev/null; then
    ok "the reason was logged"
else
    bad "no explanation logged"
fi
if [ -s "$RETAIN/preupgrade-snapshot" ] && [ ! -d "$RETAIN/preupgrade-snapshot" ]; then
    ok "the blocking file was left alone"
else
    bad "the blocking file was replaced"
fi

echo "=== 5. postinst does NOT resurrect data over a live install ==="
new_env 5
mkdir -p "$RETAIN/keep"
printf '{"accounts":[{"id":"retained@example.com"}]}\n' > "$RETAIN/keep/accounts.json"
printf '{"keys":[{"id":"revoked-key"}]}\n' > "$RETAIN/keep/api-keys.json"
# The live install already has accounts (this is an upgrade/repair), and the
# operator deliberately deleted api-keys.json.
seed_accounts
sh "$SCRIPTS/postinst"; rc=$?
check "exit status" 0 "$rc"
check_absent "deleted api-keys.json was not resurrected" "$DATA/api-keys.json"
if grep -q "retained@example.com" "$DATA/accounts.json"; then
    bad "retained accounts overwrote live accounts"
else
    ok "live accounts untouched"
fi

echo
echo "=== 6. postinst DOES restore into a genuinely empty data dir ==="
new_env 6
mkdir -p "$RETAIN/keep"
printf '{"accounts":[{"id":"retained@example.com"}]}\n' > "$RETAIN/keep/accounts.json"
sh "$SCRIPTS/postinst"; rc=$?
check "exit status" 0 "$rc"
check_file "retained data restored" "$DATA/accounts.json"
if grep -q "retained@example.com" "$DATA/accounts.json"; then
    ok "restored the retained accounts"
else
    bad "restored the wrong content"
fi

echo
echo "=== 7. postinst stages the wizard password instead of exporting it ==="
new_env 7
export pkgwizard_admin_password="Strong!Wizard#2026A"
sh "$SCRIPTS/postinst"; rc=$?
check "exit status" 0 "$rc"
check_file "reset file staged" "$PKGVAR/admin-password-reset"
check_absent "the old plaintext wizard file is gone" "$PKGVAR/admin-password-wizard"
if [ "$(cat "$PKGVAR/admin-password-reset")" = "Strong!Wizard#2026A" ]; then
    ok "staged value is the wizard password"
else
    bad "staged value is wrong"
fi
case "$(uname -s)" in
    MINGW*|MSYS*|CYGWIN*)
        echo "  SKIP  POSIX file modes are not enforced on Windows - verify 0600 on the NAS" ;;
    *)
        check "reset file mode is 600" "600" "$(stat -c '%a' "$PKGVAR/admin-password-reset" 2>/dev/null || echo '?')" ;;
esac

echo
echo "=== 8. postinst refuses a too-short wizard password ==="
new_env 8
export pkgwizard_admin_password="short123"
sh "$SCRIPTS/postinst" >/dev/null; rc=$?
check "exit status" 0 "$rc"
check_absent "weak password was not staged" "$PKGVAR/admin-password-reset"

echo
echo "=== 9. preuninst retains outside the package, and honours delete-data ==="
new_env 9
seed_accounts
sh "$SCRIPTS/preuninst"; rc=$?
check "exit status" 0 "$rc"
check_file "retained copy written outside the package" "$RETAIN/keep/accounts.json"
check_absent "nothing retained inside the package var dir" "/var/packages/m365-copilot2api/keep"

new_env 9b
seed_accounts
mkdir -p "$RETAIN/keep"
printf '{"accounts":[{"id":"stale@example.com"}]}\n' > "$RETAIN/keep/accounts.json"
export SYNOPKG_PKG_STATUS="UNINSTALL_DELDATA"
sh "$SCRIPTS/preuninst" >/dev/null; rc=$?
check "exit status" 0 "$rc"
check_absent "delete-data removed the retained copy" "$RETAIN/keep"

echo
echo "=== 10. preuninst never discards data during an upgrade ==="
new_env 10
seed_accounts
export SYNOPKG_PKG_STATUS="UPGRADE"
sh "$SCRIPTS/preuninst" >/dev/null
check "no retention performed on upgrade" "absent" "$([ -d "$RETAIN/keep" ] && echo present || echo absent)"

echo
echo "=== 11. start-stop-status never exports the password ==="
new_env 11
if grep -q 'M365_ADMIN_PASSWORD=' "$SCRIPTS/start-stop-status"; then
    bad "start-stop-status still assigns M365_ADMIN_PASSWORD"
else
    ok "no M365_ADMIN_PASSWORD assignment"
fi
if grep -q 'M365_ADMIN_PASSWORD_RESET_FILE' "$SCRIPTS/start-stop-status"; then
    ok "uses the reset-file variable"
else
    bad "reset-file variable not wired up"
fi

echo
echo "================================================"
echo "  passed: $PASS   failed: $FAIL"
echo "================================================"
[ "$FAIL" -eq 0 ]
