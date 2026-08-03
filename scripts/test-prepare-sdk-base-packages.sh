#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
TMP_ROOT=$(mktemp -d)
trap 'rm -rf "$TMP_ROOT"' EXIT HUP INT TERM

SDK_ROOT="$TMP_ROOT/sdk"
OPENWRT_ROOT="$TMP_ROOT/openwrt"

mkdir -p \
	"$SDK_ROOT/package/kernel" \
	"$SDK_ROOT/package/toolchain" \
	"$OPENWRT_ROOT/package/libs/libmnl" \
	"$OPENWRT_ROOT/package/network/config/firewall4" \
	"$OPENWRT_ROOT/package/kernel" \
	"$OPENWRT_ROOT/package/toolchain"

printf '%s\n' 'sdk kernel marker' >"$SDK_ROOT/package/kernel/marker"
printf '%s\n' 'sdk toolchain marker' >"$SDK_ROOT/package/toolchain/marker"
printf '%s\n' 'base libmnl marker' >"$OPENWRT_ROOT/package/libs/libmnl/Makefile"
printf '%s\n' 'base firewall4 marker' >"$OPENWRT_ROOT/package/network/config/firewall4/Makefile"
printf '%s\n' 'source kernel marker' >"$OPENWRT_ROOT/package/kernel/marker"
printf '%s\n' 'source toolchain marker' >"$OPENWRT_ROOT/package/toolchain/marker"

sh "$ROOT/scripts/prepare-sdk-base-packages.sh" "$SDK_ROOT" "$OPENWRT_ROOT"

grep -Fqx 'base libmnl marker' "$SDK_ROOT/package/libs/libmnl/Makefile"
grep -Fqx 'base firewall4 marker' "$SDK_ROOT/package/network/config/firewall4/Makefile"
grep -Fqx 'sdk kernel marker' "$SDK_ROOT/package/kernel/marker"
grep -Fqx 'sdk toolchain marker' "$SDK_ROOT/package/toolchain/marker"

echo 'SDK base package preparation: PASS'
