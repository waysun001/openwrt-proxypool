#!/bin/sh
set -eu

if [ "$#" -lt 1 ]; then
	echo 'usage: test-ucode-l2tp-bridge.sh UCODE_COMMAND [ARG ...]' >&2
	exit 2
fi

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
HELPER="$REPO_ROOT/proxypool-core/files/proxypool-ubus-call-stdin.uc"
BYTECODE=$(mktemp)
trap 'rm -f "$BYTECODE"' EXIT HUP INT TERM

[ -f "$HELPER" ] && [ ! -L "$HELPER" ] || {
	echo "L2TP ubus bridge is missing or linked: $HELPER" >&2
	exit 1
}

"$@" -c -o "$BYTECODE" "$HELPER"
[ -s "$BYTECODE" ] || {
	echo 'ucode produced no L2TP bridge bytecode' >&2
	exit 1
}

echo 'L2TP ucode bridge syntax: PASS'
