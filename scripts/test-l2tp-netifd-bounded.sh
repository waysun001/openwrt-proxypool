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

export PROXYPOOL_L2TP_CONTROL_PIPE="$TMP/l2tp-control"
export PROXYPOOL_L2TP_RUNTIME_DIR="$TMP/runtime"
mkdir -p "$PROXYPOOL_L2TP_RUNTIME_DIR"
mkfifo "$PROXYPOOL_L2TP_CONTROL_PIPE"
printf '%s\n' stale >"$PROXYPOOL_L2TP_RUNTIME_DIR/status.ppv20001.log"
chmod 777 "$PROXYPOOL_L2TP_RUNTIME_DIR"
if [ "$(id -u)" -eq 0 ]; then
	chown 65534:65534 "$PROXYPOOL_L2TP_RUNTIME_DIR"
else
	export PROXYPOOL_L2TP_RUNTIME_UID="$(id -u)"
	export PROXYPOOL_L2TP_RUNTIME_GID="$(id -g)"
fi
export TEST_server='203.0.113.17:1701'
export TEST_username='alice'
export TEST_password='secret'
export TEST_keepalive='3 5'
export TEST_pppd_options='noauth'
export TEST_ipv6='0'
export TEST_mtu='1400'
umask 022
sh -c '
	resolveip() { printf "%s\n" 203.0.113.17; }
	pidof() { printf "%s\n" 999; }
	proto_add_host_dependency() { :; }
	json_get_var() { eval "$1=\${TEST_$2-}"; }
	json_get_vars() { for name in "$@"; do eval "$name=\${TEST_$name-}"; done; }
	. "$1"
	proto_l2tp_setup ppv20001
' sh "$SCRIPT"
OPTIONS="$PROXYPOOL_L2TP_RUNTIME_DIR/options.ppv20001"
STATUS="$PROXYPOOL_L2TP_RUNTIME_DIR/status.ppv20001.log"
[ -f "$OPTIONS" ] || fail "setup did not write the interface-scoped PPP options"
grep -F 'logfile "'"$PROXYPOOL_L2TP_RUNTIME_DIR"'/status.ppv20001.log"' "$OPTIONS" >/dev/null ||
	fail "PPP authentication log was not scoped to the owned interface"
[ -f "$STATUS" ] && [ ! -s "$STATUS" ] || fail "stale PPP authentication state survived setup"
[ "$(stat -c '%a' "$PROXYPOOL_L2TP_RUNTIME_DIR")" = 700 ] || fail "PPP runtime directory is not private"
[ "$(stat -c '%u:%g' "$PROXYPOOL_L2TP_RUNTIME_DIR")" = "${PROXYPOOL_L2TP_RUNTIME_UID:-0}:${PROXYPOOL_L2TP_RUNTIME_GID:-0}" ] ||
	fail "PPP runtime directory ownership was not repaired"
[ "$(stat -c '%a' "$OPTIONS")" = 600 ] || fail "PPP options are readable outside root"
[ "$(stat -c '%a' "$STATUS")" = 600 ] || fail "PPP status log is readable outside root"
[ "$(stat -c '%u:%g' "$OPTIONS")" = "${PROXYPOOL_L2TP_RUNTIME_UID:-0}:${PROXYPOOL_L2TP_RUNTIME_GID:-0}" ] ||
	fail "PPP options ownership is not private"
[ "$(stat -c '%u:%g' "$STATUS")" = "${PROXYPOOL_L2TP_RUNTIME_UID:-0}:${PROXYPOOL_L2TP_RUNTIME_GID:-0}" ] ||
	fail "PPP status ownership is not private"

mkdir -p "$TMP/fail-bin"
cat >"$TMP/fail-bin/chmod" <<'EOF'
#!/bin/sh
exit 1
EOF
chmod +x "$TMP/fail-bin/chmod"
if PATH="$TMP/fail-bin:$PATH" PROXYPOOL_L2TP_RUNTIME_DIR="$TMP/runtime-failure" sh -c '
	resolveip() { printf "%s\n" 203.0.113.17; }
	pidof() { printf "%s\n" 999; }
	proto_add_host_dependency() { :; }
	proto_notify_error() { :; }
	proto_setup_failed() { :; }
	json_get_var() { eval "$1=\${TEST_$2-}"; }
	json_get_vars() { for name in "$@"; do eval "$name=\${TEST_$name-}"; done; }
	. "$1"
	proto_l2tp_setup ppv20002
' sh "$SCRIPT"; then
	fail "runtime permission failure did not fail setup closed"
fi
[ ! -e "$TMP/runtime-failure/options.ppv20002" ] || fail "failed setup published PPP credentials"

echo "PASS: netifd L2TP setup and teardown helpers are bounded and interface-scoped"
