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

echo 'ImageBuilder FILES staging: PASS'
