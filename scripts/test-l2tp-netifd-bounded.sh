#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
SCRIPT="$ROOT/files/lib/netifd/proto/l2tp.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

[ -f "$SCRIPT" ] || fail "bounded netifd L2TP overlay is missing"

mkdir -p "$TMP/sys/class/net/l2tp-ppv20001" "$TMP/proc/123" "$TMP/proc/456" "$TMP/proc/789"
printf 'pppd\000plugin\000pppol2tp.so\000ifname\000l2tp-ppv20001\000' >"$TMP/proc/123/cmdline"
printf 'pppd\000plugin\000pppol2tp.so\000ifname\000l2tp-other\000' >"$TMP/proc/456/cmdline"
printf 'pppd\000plugin\000not-pppol2tp.so\000ifname\000l2tp-ppv20001\000' >"$TMP/proc/789/cmdline"

cat >"$TMP/sleep" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$PROXYPOOL_TEST_SLEEP_LOG"
EOF
cat >"$TMP/kill" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$PROXYPOOL_TEST_KILL_LOG"
[ "$1" != -TERM ] || rm -rf "$PROXYPOOL_L2TP_PROC/$2"
EOF
cat >"$TMP/control" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$PROXYPOOL_TEST_CONTROL_LOG"
EOF
cat >"$TMP/timeout" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$PROXYPOOL_TEST_TIMEOUT_LOG"
shift 3
exec "$@"
EOF
chmod +x "$TMP/sleep" "$TMP/kill" "$TMP/control" "$TMP/timeout"

export PROXYPOOL_TEST_SLEEP_LOG="$TMP/sleep.log"
export PROXYPOOL_TEST_KILL_LOG="$TMP/kill.log"
export PROXYPOOL_TEST_CONTROL_LOG="$TMP/control.log"
export PROXYPOOL_TEST_TIMEOUT_LOG="$TMP/timeout.log"
export PROXYPOOL_L2TP_SYS_CLASS_NET="$TMP/sys/class/net"
export PROXYPOOL_L2TP_PROC="$TMP/proc"
export PROXYPOOL_L2TP_SLEEP="$TMP/sleep"
export PROXYPOOL_L2TP_KILL="$TMP/kill"
export PROXYPOOL_L2TP_CONTROL="$TMP/control"
export PROXYPOOL_L2TP_TIMEOUT="$TMP/timeout"
export INCLUDE_ONLY=1

if sh -c '. "$1"; proxypool_l2tp_wait_device_removed l2tp-ppv20001 3' sh "$SCRIPT"; then
	fail "persistent PPP device did not reach the bounded timeout"
fi
[ "$(wc -l <"$TMP/sleep.log" | tr -d ' ')" -eq 3 ] || fail "device removal wait was not bounded to three polls"

sh -c '. "$1"; proxypool_l2tp_stop_owned_ppp l2tp-ppv20001' sh "$SCRIPT"
[ "$(cat "$TMP/kill.log")" = "-TERM 123" ] || fail "PPP cleanup did not target only the exact interface owner"

sh -c '. "$1"; proxypool_l2tp_control disconnect-lac ppv20001' sh "$SCRIPT"
[ "$(cat "$TMP/timeout.log")" = "-s KILL 5 $TMP/control disconnect-lac ppv20001" ] ||
	fail "xl2tpd-control was not wrapped in a hard five-second timeout"
[ "$(cat "$TMP/control.log")" = "disconnect-lac ppv20001" ] || fail "bounded control wrapper changed its arguments"

echo "PASS: netifd L2TP setup and teardown helpers are bounded and interface-scoped"
