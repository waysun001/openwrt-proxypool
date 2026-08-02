#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
MIGRATE="$ROOT/proxypool-core/files/proxypool-migrate.sh"
TMP=${TMPDIR:-/tmp}/proxypool-migration-test.$$
trap 'rm -rf "$TMP"' EXIT HUP INT TERM
mkdir -p "$TMP/bin" "$TMP/etc" "$TMP/state"

printf '%s\n' "config global 'global'" >"$TMP/etc/proxypool"
printf '%s\n' "config global 'global'" >"$TMP/etc/proxypool_v2"
cat >"$TMP/bin/proxypoolctl" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" >"$PROXYPOOL_TEST_ARGS"
printf '%s\n' '{"status":"migrated","revision":2,"nodes":1,"devices":0,"pending":1}'
EOF
chmod 755 "$TMP/bin/proxypoolctl"

PROXYPOOL_TEST_ARGS="$TMP/args" \
PROXYPOOL_CTL="$TMP/bin/proxypoolctl" \
PROXYPOOL_LEGACY_CONFIG="$TMP/etc/proxypool" \
PROXYPOOL_V2_CONFIG="$TMP/etc/proxypool_v2" \
PROXYPOOL_STATE_DIR="$TMP/state" \
PROXYPOOL_SOCKET="$TMP/state/proxypoold.sock" \
	sh "$MIGRATE" >"$TMP/output"

cat >"$TMP/expected" <<EOF
migrate-v1
--source
$TMP/etc/proxypool
--target
$TMP/etc/proxypool_v2
--backup-dir
$TMP/state/backups
--marker
$TMP/state/migration-v1.json
--socket
$TMP/state/proxypoold.sock
EOF
cmp -s "$TMP/expected" "$TMP/args" || { echo 'migration wrapper arguments are incorrect' >&2; exit 1; }
grep -Fq '"status":"migrated"' "$TMP/output" || { echo 'migration wrapper lost safe result' >&2; exit 1; }

rm -f "$TMP/etc/proxypool"
ln -s "$TMP/etc/proxypool_v2" "$TMP/etc/proxypool"
if [ -L "$TMP/etc/proxypool" ]; then
	rm -f "$TMP/args"
	if PROXYPOOL_TEST_ARGS="$TMP/args" PROXYPOOL_CTL="$TMP/bin/proxypoolctl" \
		PROXYPOOL_LEGACY_CONFIG="$TMP/etc/proxypool" PROXYPOOL_V2_CONFIG="$TMP/etc/proxypool_v2" \
		PROXYPOOL_STATE_DIR="$TMP/state" PROXYPOOL_SOCKET="$TMP/state/proxypoold.sock" sh "$MIGRATE" >/dev/null 2>&1; then
		echo 'migration wrapper accepted a symlink source' >&2
		exit 1
	fi
	[ ! -e "$TMP/args" ] || { echo 'migration wrapper called control tool for unsafe source' >&2; exit 1; }
fi

echo 'PASS: V1 migration wrapper is bounded and uses fixed private paths'
