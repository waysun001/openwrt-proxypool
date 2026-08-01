#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
INSPECTOR="$ROOT/scripts/inspect-ipk.sh"
TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM

mkdir "$TEST_TMP/bin"
printf '%s\n' \
	'#!/usr/bin/env sh' \
	"printf '%s\\n' 'ELF 64-bit LSB executable, ARM aarch64'" \
	>"$TEST_TMP/bin/file"
chmod 755 "$TEST_TMP/bin/file"
printf '%s\n' \
	'#!/usr/bin/env sh' \
	'target=' \
	'for argument in "$@"; do target=$argument; done' \
	'case "$target" in' \
	'  */etc/config/proxypool) printf "600\\n" ;;' \
	'  */lib/upgrade/keep.d/proxypool) printf "644\\n" ;;' \
	'  *) printf "755\\n" ;;' \
	'esac' \
	>"$TEST_TMP/bin/stat"
chmod 755 "$TEST_TMP/bin/stat"

make_ipk() {
	name=$1
	conffiles_kind=$2
	fixture="$TEST_TMP/$name"
	outer="$fixture/outer"
	control="$fixture/control"
	data="$fixture/data"
	mkdir -p "$outer" "$control" \
		"$data/etc/config" "$data/etc/init.d" \
		"$data/usr/sbin" "$data/usr/bin" "$data/usr/lib/proxypool" \
		"$data/lib/upgrade/keep.d"
	printf '%s\n' \
		'Package: proxypool-core' \
		'Version: 2.0.0-1' \
		'Architecture: aarch64_cortex-a53' >"$control/control"
	case "$conffiles_kind" in
		valid|payload_v2|payload_state|bad_keep) printf '/etc/config/proxypool\n' >"$control/conffiles" ;;
		extra) printf '/etc/config/proxypool\n/etc/config/unrelated\n' >"$control/conffiles" ;;
		crlf) printf '/etc/config/proxypool\r\n' >"$control/conffiles" ;;
		no_newline) printf '/etc/config/proxypool' >"$control/conffiles" ;;
		missing) : ;;
	esac
	cp "$ROOT/proxypool-core/files/proxypool.config" "$data/etc/config/proxypool"
	printf '/etc/config/proxypool_v2\n/etc/config/proxypool_runtime\n/etc/proxypool/activated-backend\n/etc/proxypool/cleanup-required\n' >"$data/lib/upgrade/keep.d/proxypool"
	case "$conffiles_kind" in
		payload_v2) printf "config global 'global'\n" >"$data/etc/config/proxypool_v2" ;;
		payload_state) mkdir -p "$data/etc/proxypool"; printf 'v1\n' >"$data/etc/proxypool/activated-backend" ;;
		bad_keep) printf '/etc/config/proxypool_v2\n/etc/config/proxypool_runtime\n/etc/proxypool/activated-backend\n' >"$data/lib/upgrade/keep.d/proxypool" ;;
	esac
	printf '%s\n' '#!/bin/sh' 'exit 0' >"$data/etc/init.d/proxypool"
	for binary in \
		"$data/usr/sbin/proxypoold" \
		"$data/usr/bin/proxypoolctl" \
		"$data/usr/lib/proxypool/ip2region_searcher" \
		"$data/usr/bin/slp-client"; do
		printf '%s\n' '#!/bin/sh' 'exit 0' >"$binary"
		chmod 755 "$binary"
	done
	chmod 600 "$data/etc/config/proxypool"
	chmod 755 "$data/etc/init.d/proxypool"
	printf '2.0\n' >"$outer/debian-binary"
	tar -czf "$outer/control.tar.gz" -C "$control" .
	tar -czf "$outer/data.tar.gz" -C "$data" .
	tar -czf "$TEST_TMP/$name.ipk" -C "$outer" .
}

make_ipk valid valid
if ! PATH="$TEST_TMP/bin:$PATH" sh "$INSPECTOR" "$TEST_TMP/valid.ipk" aarch64_cortex-a53 \
	>"$TEST_TMP/valid.log" 2>&1; then
	cat "$TEST_TMP/valid.log" >&2
	exit 1
fi
grep -Fq 'IPK contents: PASS' "$TEST_TMP/valid.log"

for invalid_kind in missing extra crlf no_newline payload_v2 payload_state bad_keep; do
	make_ipk "$invalid_kind" "$invalid_kind"
	if PATH="$TEST_TMP/bin:$PATH" sh "$INSPECTOR" "$TEST_TMP/$invalid_kind.ipk" aarch64_cortex-a53 \
		>"$TEST_TMP/$invalid_kind.log" 2>&1; then
		printf 'invalid control/conffiles variant passed: %s\n' "$invalid_kind" >&2
		exit 1
	fi
done
grep -Fq 'package payload unexpectedly owns /etc/config/proxypool_v2' "$TEST_TMP/payload_v2.log"
grep -Fq 'package payload unexpectedly owns /etc/proxypool/activated-backend' "$TEST_TMP/payload_state.log"
grep -Fq 'missing or invalid sysupgrade keep list' "$TEST_TMP/bad_keep.log"

echo 'IPK conffiles inspection: PASS'
