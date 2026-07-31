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

config="$inspect_tmp/data/etc/config/proxypool"
[ -f "$config" ] || { echo 'missing /etc/config/proxypool' >&2; exit 1; }
[ "$(stat -c '%a' "$config")" = 600 ] || { echo 'unexpected mode for /etc/config/proxypool' >&2; exit 1; }

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
