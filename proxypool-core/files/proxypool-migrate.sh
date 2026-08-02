#!/bin/sh
set -eu

umask 077

[ "$#" -eq 0 ] || {
	echo 'usage: proxypool-migrate.sh' >&2
	exit 2
}

CTL="${PROXYPOOL_CTL:-/usr/bin/proxypoolctl}"
LEGACY="${PROXYPOOL_LEGACY_CONFIG:-/etc/config/proxypool}"
TARGET="${PROXYPOOL_V2_CONFIG:-/etc/config/proxypool_v2}"
STATE_DIR="${PROXYPOOL_STATE_DIR:-/etc/proxypool}"
SOCKET="${PROXYPOOL_SOCKET:-/var/run/proxypoold.sock}"

[ -x "$CTL" ] || { echo 'ProxyPool migration tool is unavailable' >&2; exit 1; }
[ -f "$LEGACY" ] && [ ! -L "$LEGACY" ] || { echo 'legacy ProxyPool configuration is unsafe' >&2; exit 1; }
[ -f "$TARGET" ] && [ ! -L "$TARGET" ] || { echo 'V2 ProxyPool configuration is unsafe' >&2; exit 1; }

exec "$CTL" migrate-v1 \
	--source "$LEGACY" \
	--target "$TARGET" \
	--backup-dir "$STATE_DIR/backups" \
	--marker "$STATE_DIR/migration-v1.json" \
	--socket "$SOCKET"
