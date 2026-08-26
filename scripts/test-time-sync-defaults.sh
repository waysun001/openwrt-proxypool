#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
DEFAULTS="$ROOT/files/etc/uci-defaults/97-zeanlink-time"
HOTPLUG="$ROOT/files/etc/hotplug.d/iface/95-zeanlink-time"
BOOTSTRAP="$ROOT/files/usr/lib/zeanlink/time-bootstrap.sh"
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
[ -f "$BOOTSTRAP" ] || {
	echo 'numeric HTTP time bootstrap is missing' >&2
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
cat >"$TEST_TMP/bootstrap" <<'EOF'
#!/bin/sh
printf 'bootstrap\n' >>"$PROXYPOOL_TEST_LOG"
exit 0
EOF
chmod 755 "$TEST_TMP/uci" "$TEST_TMP/sysntpd" "$TEST_TMP/bootstrap"

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
	PROXYPOOL_TIME_BOOTSTRAP="$TEST_TMP/bootstrap" \
	ACTION=ifup INTERFACE=wan sh "$HOTPLUG"
grep -Fxq 'sysntpd reload' "$LOG"
grep -Fxq 'bootstrap' "$LOG"

: >"$LOG"
PROXYPOOL_TEST_LOG="$LOG" \
PROXYPOOL_SYSNTPD="$TEST_TMP/sysntpd" \
	PROXYPOOL_TIME_BOOTSTRAP="$TEST_TMP/bootstrap" \
	ACTION=ifup INTERFACE=lan sh "$HOTPLUG"
[ ! -s "$LOG" ] || {
	echo 'non-WAN event restarted the time client' >&2
	exit 1
}

cat >"$TEST_TMP/curl" <<'EOF'
#!/bin/sh
headers=
url=
while [ "$#" -gt 0 ]; do
	case "$1" in
		--dump-header) headers=$2; shift 2 ;;
		http://*) url=$1; shift ;;
		*) shift ;;
	esac
done
case "$url" in
	http://1.1.1.1/)
		date_value='Wed, 26 Aug 2026 10:40:55 GMT'
		;;
	http://208.67.222.222/)
		date_value='Wed, 26 Aug 2026 10:41:21 GMT'
		;;
	*) exit 28 ;;
esac
printf 'HTTP/1.1 301 Moved Permanently\r\nDate: %s\r\n\r\n' "$date_value" >"$headers"
EOF
cat >"$TEST_TMP/date" <<'EOF'
#!/bin/sh
case "$*" in
	'-u +%Y') printf '2024\n' ;;
	*'2026-08-26 10:40:55'*'+%s') printf '1787740855\n' ;;
	*'2026-08-26 10:41:21'*'+%s') printf '1787740881\n' ;;
	*'-s 2026-08-26 10:40:55') printf 'date-set %s\n' "$*" >>"$PROXYPOOL_TEST_LOG" ;;
	*) exit 1 ;;
esac
EOF
chmod 755 "$TEST_TMP/curl" "$TEST_TMP/date"

: >"$LOG"
PROXYPOOL_TEST_LOG="$LOG" \
	ZEANLINK_CURL="$TEST_TMP/curl" \
	ZEANLINK_DATE="$TEST_TMP/date" \
	ZEANLINK_LOGGER=/bin/true \
	sh "$BOOTSTRAP"
grep -Fxq 'date-set -u -s 2026-08-26 10:40:55' "$LOG"

echo 'Numeric NTP defaults, WAN retry and HTTP consensus bootstrap: PASS'
