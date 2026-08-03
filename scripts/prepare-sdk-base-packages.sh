#!/usr/bin/env sh
set -eu

if [ "$#" -ne 2 ]; then
	echo "usage: $0 <sdk-root> <openwrt-source-root>" >&2
	exit 2
fi

SDK_ROOT=$1
OPENWRT_ROOT=$2

[ -d "$SDK_ROOT/package/kernel" ] || {
	echo "SDK package tree is missing: $SDK_ROOT/package/kernel" >&2
	exit 1
}
[ -f "$OPENWRT_ROOT/package/libs/libmnl/Makefile" ] || {
	echo "OpenWrt base package is missing: package/libs/libmnl" >&2
	exit 1
}

for source in "$OPENWRT_ROOT"/package/*; do
	name=${source##*/}
	case "$name" in
		Makefile|kernel|toolchain)
			continue
			;;
	esac
	cp -a "$source" "$SDK_ROOT/package/"
done

for source in "$OPENWRT_ROOT"/package/kernel/*; do
	name=${source##*/}
	[ "$name" = 'linux' ] && continue
	cp -a "$source" "$SDK_ROOT/package/kernel/"
done

[ -f "$SDK_ROOT/package/libs/libmnl/Makefile" ] || {
	echo 'failed to add libmnl sources to SDK' >&2
	exit 1
}
