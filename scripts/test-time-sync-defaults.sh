#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
DEFAULTS="$ROOT/files/etc/uci-defaults/97-zeanlink-time"
HOTPLUG="$ROOT/files/etc/hotplug.d/iface/95-zeanlink-time"
TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM
LOG="$TEST_TMP/calls"

[ -x "$DEFAULTS" ] || {
	echo 'time defaults script is missing or not executable' >&2
	exit 1
}
[ -x "$HOTPLUG" ] || {
	echo 'time WAN hotplug script is missing or not executable' >&2
	exit 1
}

cat >"$TEST_TMP/uci" <<'EOF'
#!/bin/sh
printf 'uci %s\n' "$*" >>"$PROXYPOOL_TEST_LOG"
exit 0
EOF
cat >"$TEST_TMP/sysntpd" <<'EOF'
#!/bin/sh
printf 'sysntpd %s\n' "$*" >>"$PROXYPOOL_TEST_LOG"
exit 0
EOF
chmod 755 "$TEST_TMP/uci" "$TEST_TMP/sysntpd"

PROXYPOOL_TEST_LOG="$LOG" \
PROXYPOOL_UCI="$TEST_TMP/uci" \
PROXYPOOL_SYSNTPD="$TEST_TMP/sysntpd" \
	sh "$DEFAULTS"

grep -Fxq 'uci -q set system.ntp=timeserver' "$LOG"
grep -Fxq 'uci -q set system.ntp.enabled=1' "$LOG"
grep -Fxq 'uci -q set system.ntp.enable_server=0' "$LOG"
grep -Fxq 'uci -q set system.ntp.use_dhcp=0' "$LOG"
grep -Fxq 'uci -q delete system.ntp.server' "$LOG"
grep -Fxq 'uci -q add_list system.ntp.server=162.159.200.1' "$LOG"
grep -Fxq 'uci -q add_list system.ntp.server=162.159.200.123' "$LOG"
grep -Fxq 'uci -q commit system' "$LOG"
grep -Fxq 'sysntpd enable' "$LOG"

: >"$LOG"
PROXYPOOL_TEST_LOG="$LOG" \
PROXYPOOL_SYSNTPD="$TEST_TMP/sysntpd" \
	ACTION=ifup INTERFACE=wan sh "$HOTPLUG"
grep -Fxq 'sysntpd reload' "$LOG"

: >"$LOG"
PROXYPOOL_TEST_LOG="$LOG" \
PROXYPOOL_SYSNTPD="$TEST_TMP/sysntpd" \
	ACTION=ifup INTERFACE=lan sh "$HOTPLUG"
[ ! -s "$LOG" ] || {
	echo 'non-WAN event restarted the time client' >&2
	exit 1
}

echo 'Numeric NTP defaults and WAN retry: PASS'
