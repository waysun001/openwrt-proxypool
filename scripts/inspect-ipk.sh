#!/usr/bin/env sh
set -eu

if [ "$#" -ne 2 ]; then
	echo 'usage: inspect-ipk.sh PACKAGE.ipk EXPECTED_PACKAGE_ARCH' >&2
	exit 2
fi

package=$1
expected_arch=$2
[ -f "$package" ] || { echo "package not found: $package" >&2; exit 1; }

inspect_tmp=$(mktemp -d)
trap 'rm -rf "$inspect_tmp"' EXIT HUP INT TERM
mkdir "$inspect_tmp/outer" "$inspect_tmp/data" "$inspect_tmp/control"
tar -xzf "$package" -C "$inspect_tmp/outer"
[ "$(tr -d '\r\n' <"$inspect_tmp/outer/debian-binary")" = 2.0 ] || { echo 'invalid IPK format version' >&2; exit 1; }
tar -xzf "$inspect_tmp/outer/data.tar.gz" -C "$inspect_tmp/data"
tar -xzf "$inspect_tmp/outer/control.tar.gz" -C "$inspect_tmp/control"
grep -Fqx "Architecture: $expected_arch" "$inspect_tmp/control/control" || { echo 'unexpected package architecture metadata' >&2; exit 1; }
conffiles="$inspect_tmp/control/conffiles"
expected_conffiles="$inspect_tmp/expected-conffiles"
printf '/etc/config/proxypool\n' >"$expected_conffiles"
[ -f "$conffiles" ] && cmp -s "$expected_conffiles" "$conffiles" || {
	echo 'control/conffiles must contain exactly /etc/config/proxypool followed by LF' >&2
	exit 1
}

config="$inspect_tmp/data/etc/config/proxypool"
[ -f "$config" ] || { echo 'missing /etc/config/proxypool' >&2; exit 1; }
[ "$(stat -c '%a' "$config")" = 600 ] || { echo 'unexpected mode for /etc/config/proxypool' >&2; exit 1; }
[ "$(sha256sum "$config" | awk '{ print $1 }')" = '00f37918933d1e7a66fc0b83b7791c164e15ea835a7fa6bee5761701f9291958' ] || {
	echo 'packaged /etc/config/proxypool is not the exact V1 rollback baseline' >&2
	exit 1
}
for overlay_only in proxypool_v2 proxypool_runtime; do
	[ ! -e "$inspect_tmp/data/etc/config/$overlay_only" ] || {
		echo "package payload unexpectedly owns /etc/config/$overlay_only" >&2
		exit 1
	}
done

keep="$inspect_tmp/data/lib/upgrade/keep.d/proxypool"
expected_keep="$inspect_tmp/expected-keep"
printf '/etc/config/proxypool_v2\n/etc/config/proxypool_runtime\n' >"$expected_keep"
[ -f "$keep" ] && cmp -s "$expected_keep" "$keep" || { echo 'missing or invalid sysupgrade keep list' >&2; exit 1; }
[ "$(stat -c '%a' "$keep")" = 644 ] || { echo 'unexpected mode for sysupgrade keep list' >&2; exit 1; }

init="$inspect_tmp/data/etc/init.d/proxypool"
[ -f "$init" ] && [ -x "$init" ] || { echo 'missing executable /etc/init.d/proxypool' >&2; exit 1; }
[ "$(stat -c '%a' "$init")" = 755 ] || { echo 'unexpected mode for /etc/init.d/proxypool' >&2; exit 1; }

for relative in usr/sbin/proxypoold usr/bin/proxypoolctl usr/lib/proxypool/ip2region_searcher usr/bin/slp-client; do
	binary="$inspect_tmp/data/$relative"
	[ -f "$binary" ] && [ -x "$binary" ] || { echo "missing executable /$relative" >&2; exit 1; }
	[ "$(stat -c '%a' "$binary")" = 755 ] || { echo "unexpected mode for /$relative" >&2; exit 1; }
	description=$(file -b "$binary")
	printf '/%s: %s\n' "$relative" "$description"
	printf '%s\n' "$description" | grep -Eq 'ELF 64-bit LSB.*ARM aarch64' || { echo "unexpected target binary architecture for /$relative" >&2; exit 1; }
done

echo 'IPK contents: PASS'
