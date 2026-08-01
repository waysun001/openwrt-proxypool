#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
PREPARE="$ROOT/scripts/prepare-image-files.sh"
TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM
SOURCE="$TEST_TMP/source"
DESTINATION="$TEST_TMP/staged"

mkdir -p "$SOURCE/etc/config" "$SOURCE/etc/uci-defaults"
printf '%s\n' "config global 'global'" >"$SOURCE/etc/config/proxypool"
printf '%s\n' "config global 'global'" >"$SOURCE/etc/config/proxypool_v2"
printf '%s\n' "config global 'global'" >"$SOURCE/etc/config/proxypool_runtime"
printf '%s\n' '#!/bin/sh' 'exit 0' >"$SOURCE/etc/uci-defaults/keep"
chmod 644 "$SOURCE/etc/config/proxypool"
chmod 644 "$SOURCE/etc/config/proxypool_v2"
chmod 644 "$SOURCE/etc/config/proxypool_runtime"
chmod 755 "$SOURCE/etc/uci-defaults/keep"

sh "$PREPARE" "$SOURCE" "$DESTINATION"

cmp -s "$SOURCE/etc/config/proxypool" "$DESTINATION/etc/config/proxypool"
cmp -s "$SOURCE/etc/config/proxypool_v2" "$DESTINATION/etc/config/proxypool_v2"
cmp -s "$SOURCE/etc/config/proxypool_runtime" "$DESTINATION/etc/config/proxypool_runtime"
cmp -s "$SOURCE/etc/uci-defaults/keep" "$DESTINATION/etc/uci-defaults/keep"
[ "$(stat -c '%a' "$SOURCE/etc/config/proxypool")" = 644 ]
[ "$(stat -c '%a' "$SOURCE/etc/config/proxypool_v2")" = 644 ]
[ "$(stat -c '%a' "$SOURCE/etc/config/proxypool_runtime")" = 644 ]
[ "$(stat -c '%a' "$DESTINATION/etc/uci-defaults/keep")" = 755 ]
case "$(uname -s)" in
	Linux*)
		for config_name in proxypool proxypool_v2 proxypool_runtime; do
			[ "$(stat -c '%a' "$DESTINATION/etc/config/$config_name")" = 600 ]
		done
		;;
	*) echo 'SKIP: staged config mode assertion requires a POSIX filesystem' ;;
esac

for required_config in proxypool proxypool_v2 proxypool_runtime; do
	missing_source="$TEST_TMP/missing-$required_config"
	missing_destination="$TEST_TMP/staged-missing-$required_config"
	cp -a "$SOURCE" "$missing_source"
	rm "$missing_source/etc/config/$required_config"
	if sh "$PREPARE" "$missing_source" "$missing_destination" >/dev/null 2>&1; then
		printf 'staging accepted a missing required config: %s\n' "$required_config" >&2
		exit 1
	fi
	[ ! -e "$missing_destination" ] || {
		printf 'failed staging left a partial destination: %s\n' "$required_config" >&2
		exit 1
	}
done

# Model a settings-preserving sysupgrade from a legacy-only V1 image. The old
# backup has no selector, so the new ROM default remains visible after restore
# and must not silently select V2. The separate V2 shadow config still ships so
# a later explicit migration has a validated target.
ROM="$TEST_TMP/rom"
OLD_V1_BACKUP="$TEST_TMP/old-v1-backup"
UPGRADED_V1="$TEST_TMP/upgraded-v1"
sh "$PREPARE" "$ROOT/files" "$ROM"
mkdir -p "$OLD_V1_BACKUP/etc/config" "$UPGRADED_V1"
cp "$ROOT/proxypool-core/files/proxypool.config" "$OLD_V1_BACKUP/etc/config/proxypool"
cp -a "$ROM/." "$UPGRADED_V1/"
cp -a "$OLD_V1_BACKUP/." "$UPGRADED_V1/"
grep -Fq "option runtime_backend 'v1'" "$UPGRADED_V1/etc/config/proxypool_runtime" || {
	echo 'legacy V1 sysupgrade inherited a non-V1 ROM selector' >&2
	exit 1
}
if grep -Fq "option runtime_backend 'v2_shadow'" "$UPGRADED_V1/etc/config/proxypool_runtime"; then
	echo 'legacy V1 sysupgrade silently selected V2 shadow' >&2
	exit 1
fi
grep -Fq "option schema_version '2'" "$UPGRADED_V1/etc/config/proxypool_v2"
grep -Fq "option runtime_backend 'v2_shadow'" "$UPGRADED_V1/etc/config/proxypool_v2"

# An existing V2 selector is an explicitly preserved sysupgrade file and must
# override the safer phase-1 ROM default when restored from the keep archive.
V2_KEEP_BACKUP="$TEST_TMP/v2-keep-backup"
UPGRADED_V2="$TEST_TMP/upgraded-v2"
mkdir -p "$V2_KEEP_BACKUP/etc/config" "$V2_KEEP_BACKUP/etc/proxypool" "$UPGRADED_V2"
printf "config global 'global'\n\toption runtime_backend 'v2_shadow'\n" >"$V2_KEEP_BACKUP/etc/config/proxypool_runtime"
printf 'v2_shadow\n' >"$V2_KEEP_BACKUP/etc/proxypool/activated-backend"
cp -a "$ROM/." "$UPGRADED_V2/"
cp -a "$V2_KEEP_BACKUP/." "$UPGRADED_V2/"
grep -Fq "option runtime_backend 'v2_shadow'" "$UPGRADED_V2/etc/config/proxypool_runtime" || {
	echo 'preserved V2 selector did not override the ROM default' >&2
	exit 1
}
[ "$(cat "$UPGRADED_V2/etc/proxypool/activated-backend")" = v2_shadow ] || {
	echo 'preserved V2 selector lost its activated backend ownership' >&2
	exit 1
}

echo 'ImageBuilder FILES staging: PASS'
