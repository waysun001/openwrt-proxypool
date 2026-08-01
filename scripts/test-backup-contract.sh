#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
BACKUP="$ROOT/proxypool-core/files/backup.sh"

fail() {
	echo "backup contract: $*" >&2
	exit 1
}

[ -f "$BACKUP" ] || fail 'missing backup helper'

TEST_TMP=$(mktemp -d "${TMPDIR:-/tmp}/proxypool-backup-test.XXXXXX")
TEST_TMP=$(CDPATH= cd -- "$TEST_TMP" && pwd -P)
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM
CONFIG_DIR="$TEST_TMP/config"
STATE_DIR="$TEST_TMP/state"
mkdir -p "$CONFIG_DIR" "$STATE_DIR"

printf '%s\n' \
	"config global 'global'" \
	"\toption enabled '0'" \
	>"$CONFIG_DIR/proxypool"

run_backup() {
	env \
		PROXYPOOL_BACKUP_LEGACY_CONFIG="$CONFIG_DIR/proxypool" \
		PROXYPOOL_BACKUP_V2_CONFIG="$CONFIG_DIR/proxypool_v2" \
		PROXYPOOL_BACKUP_SELECTOR_CONFIG="$CONFIG_DIR/proxypool_runtime" \
		PROXYPOOL_BACKUP_STATE_DIR="$STATE_DIR" \
		PROXYPOOL_BACKUP_TMPDIR="$TEST_TMP" \
		sh "$BACKUP" "$@"
}

LEGACY_ARCHIVE="$TEST_TMP/legacy.tar.gz"
legacy_output=$(run_backup create "$LEGACY_ARCHIVE") || fail 'legacy-only backup creation failed'
[ "$legacy_output" = "$LEGACY_ARCHIVE" ] || fail 'backup create returned an unexpected path'
[ -f "$LEGACY_ARCHIVE" ] && [ ! -L "$LEGACY_ARCHIVE" ] || fail 'legacy backup is not a regular file'
case "$(uname -s 2>/dev/null || printf unknown)" in
	MINGW*|MSYS*|CYGWIN*|Windows_NT*) : ;;
	*) [ "$(stat -c '%a' "$LEGACY_ARCHIVE")" = 600 ] || fail 'backup archive mode is not 0600' ;;
esac
legacy_verify=$(run_backup verify "$LEGACY_ARCHIVE") || fail 'legacy-only backup verification failed'
printf '%s\n' "$legacy_verify" | grep -Fq '"valid":true' || fail 'legacy verify did not report valid'
printf '%s\n' "$legacy_verify" | grep -Fq '"kind":"legacy-only"' || fail 'legacy kind is not explicit'
printf '%s\n' "$legacy_verify" | grep -Fq '"schema_version":1' || fail 'backup schema is not explicit'

# A dual bundle is accepted only when V1, V2, and the selector all exist.
printf '%s\n' \
	"config global 'global'" \
	"\toption schema_version '2'" \
	>"$CONFIG_DIR/proxypool_v2"
printf '%s\n' \
	"config runtime 'runtime'" \
	"\toption backend 'v2_shadow'" \
	>"$CONFIG_DIR/proxypool_runtime"
FULL_ARCHIVE="$TEST_TMP/full.tar.gz"
run_backup create "$FULL_ARCHIVE" >/dev/null || fail 'full-dual backup creation failed'
full_verify=$(run_backup verify "$FULL_ARCHIVE") || fail 'full-dual backup verification failed'
printf '%s\n' "$full_verify" | grep -Fq '"kind":"full-dual"' || fail 'full-dual kind is not explicit'
for member in config/proxypool config/proxypool_v2 config/proxypool_runtime manifest; do
	[ "$(tar -tzf "$FULL_ARCHIVE" | grep -Fxc "$member" || true)" -eq 1 ] ||
		fail "full-dual archive does not contain exactly one $member"
done

# Any partial-dual state is rejected before the output path appears.
for partial in missing_v2 missing_selector missing_v1; do
	case "$partial" in
		missing_v2) mv "$CONFIG_DIR/proxypool_v2" "$CONFIG_DIR/proxypool_v2.off" ;;
		missing_selector) mv "$CONFIG_DIR/proxypool_runtime" "$CONFIG_DIR/proxypool_runtime.off" ;;
		missing_v1) mv "$CONFIG_DIR/proxypool" "$CONFIG_DIR/proxypool.off" ;;
	esac
	partial_archive="$TEST_TMP/$partial.tar.gz"
	if run_backup create "$partial_archive" >/dev/null 2>&1; then
		fail "$partial partial-dual backup was accepted"
	fi
	[ ! -e "$partial_archive" ] && [ ! -L "$partial_archive" ] ||
		fail "$partial failure published an archive"
	case "$partial" in
		missing_v2) mv "$CONFIG_DIR/proxypool_v2.off" "$CONFIG_DIR/proxypool_v2" ;;
		missing_selector) mv "$CONFIG_DIR/proxypool_runtime.off" "$CONFIG_DIR/proxypool_runtime" ;;
		missing_v1) mv "$CONFIG_DIR/proxypool.off" "$CONFIG_DIR/proxypool" ;;
	esac
done

# Refuse overwrite and symlink targets instead of following them.
if run_backup create "$FULL_ARCHIVE" >/dev/null 2>&1; then
	fail 'backup create overwrote an existing archive'
fi
SYMLINK_TARGET="$TEST_TMP/symlink-target"
printf 'preserved\n' >"$SYMLINK_TARGET"
if ln -s "$SYMLINK_TARGET" "$TEST_TMP/output-link" 2>/dev/null; then
	if run_backup create "$TEST_TMP/output-link" >/dev/null 2>&1; then
		fail 'backup create followed an output symlink'
	fi
	[ "$(cat "$SYMLINK_TARGET")" = preserved ] || fail 'backup create modified a symlink target'
else
	echo 'SKIP: host cannot create backup output symlink fixture'
fi

# Integrity covers every config payload. Repack a changed file under the old
# manifest and require verification to reject it.
TAMPER_DIR="$TEST_TMP/tamper"
mkdir "$TAMPER_DIR"
tar -xzf "$FULL_ARCHIVE" -C "$TAMPER_DIR"
printf '%s\n' "config global 'tampered'" >"$TAMPER_DIR/config/proxypool_v2"
tar -czf "$TEST_TMP/tampered.tar.gz" -C "$TAMPER_DIR" manifest config
if run_backup verify "$TEST_TMP/tampered.tar.gz" >/dev/null 2>&1; then
	fail 'backup verification accepted a payload/hash mismatch'
fi
printf 'extra\n' >"$TAMPER_DIR/extra"
tar -czf "$TEST_TMP/extra.tar.gz" -C "$TAMPER_DIR" manifest config extra
if run_backup verify "$TEST_TMP/extra.tar.gz" >/dev/null 2>&1; then
	fail 'backup verification accepted an unmanifested archive member'
fi

# Phase 1 restore is deliberately unavailable. It must reject even a missing
# input before invoking tar, service control, cp, or any other mutation.
RESTORE_BIN="$TEST_TMP/restore-bin"
RESTORE_TRACE="$TEST_TMP/restore.trace"
mkdir "$RESTORE_BIN"
for command_name in tar cp mv; do
	printf '%s\n' '#!/bin/sh' 'printf "%s\\n" "$0 $*" >>"$PROXYPOOL_TEST_RESTORE_TRACE"' 'exit 99' \
		>"$RESTORE_BIN/$command_name"
	chmod 755 "$RESTORE_BIN/$command_name"
done
if PATH="$RESTORE_BIN:$PATH" PROXYPOOL_TEST_RESTORE_TRACE="$RESTORE_TRACE" \
	run_backup restore "$TEST_TMP/does-not-exist.tar.gz" >"$TEST_TMP/restore.out" 2>&1; then
	fail 'unsupported restore returned success'
fi
grep -Fq 'restore_unsupported' "$TEST_TMP/restore.out" || fail 'restore rejection is not explicit'
[ ! -e "$RESTORE_TRACE" ] || fail 'unsupported restore invoked a mutation helper'

echo 'backup create/verify and restore-unavailable contracts: PASS'
