#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
HELPER="$ROOT/proxypool-core/files/lan-isolation.sh"
HOTPLUG="$ROOT/proxypool-core/files/proxypool-lan-isolation.hotplug"
WORKER="$ROOT/proxypool-core/files/lan-isolation-worker.sh"
WIRELESS_DEFAULT="$ROOT/files/etc/uci-defaults/98-proxypool-wireless"

fail() {
	echo "lan isolation test: $*" >&2
	exit 1
}

[ -f "$HELPER" ] || fail 'missing LAN isolation reconciler'
[ -f "$HOTPLUG" ] || fail 'missing LAN isolation hotplug wrapper'
[ -f "$WIRELESS_DEFAULT" ] || fail 'missing image wireless default'

if grep -Eq "disabled=['\"]?0|encryption=['\"]?none|delete[[:space:]]+wireless\..*\.key" "$WIRELESS_DEFAULT"; then
	fail 'image overlay enables or weakens an unauthenticated wireless network'
fi
grep -Fq '/usr/lib/proxypool/lan-isolation.sh' "$WIRELESS_DEFAULT" ||
	fail 'image wireless default does not delegate to the audited isolation helper'

TEST_TMP=${TMPDIR:-/tmp}/proxypool-l2-test.$$
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM
BIN="$TEST_TMP/bin"
DEFAULT_DELTA="$TEST_TMP/uci-default-delta"
CONFIG_DIR="$TEST_TMP/config"
STAGE_ROOT="$TEST_TMP/persistent"
RUN_DIR="$TEST_TMP/run"
REBOOT_MARKER="$RUN_DIR/wireless-reboot-required"
WIRELESS_QUARANTINE_ROOT="$STAGE_ROOT/wireless-quarantine"
LAN_STATE_ROOT="$RUN_DIR/lan-isolation"
LAN_PENDING_MARKER="$LAN_STATE_ROOT/pending"
LAN_INFLIGHT_MARKER="$LAN_STATE_ROOT/inflight"
SYS_CLASS_NET="$TEST_TMP/sys/class/net"
CPU_ONLY_MARKER="$TEST_TMP/proxypool_cpu_only_bridge"
NETWORK_PENDING_FILE="$TEST_TMP/network.pending"
STAGE_LINK_STATE="$TEST_TMP/stage-hardlink.state"
TRACE="$TEST_TMP/trace"
DEFAULT_CONFIG="$TEST_TMP/default-wireless"
DEFAULT_MODE_STATE="$TEST_TMP/default-wireless.mode"
mkdir -p "$BIN" "$DEFAULT_DELTA" "$CONFIG_DIR" "$STAGE_ROOT" "$RUN_DIR" "$SYS_CLASS_NET/br-lan/brif"
chmod 700 "$STAGE_ROOT" "$RUN_DIR"
printf '%s\n' \
	'radio0.disabled=0' \
	'ap_lan.disabled=0' \
	'anonymous0.disabled=0' \
	'ap_lan.isolate=0' \
	'ap_lan.bridge_isolate=0' \
	'anonymous0.isolate=0' \
	'anonymous0.bridge_isolate=0' >"$CONFIG_DIR/wireless"
chmod 600 "$CONFIG_DIR/wireless"
: >"$TRACE"

wireless_value() {
	sed -n "s/^$1=//p" "$CONFIG_DIR/wireless"
}

reset_wireless_config() {
	printf '%s\n' \
		'radio0.disabled=0' \
		'ap_lan.disabled=0' \
		'anonymous0.disabled=0' \
		'ap_lan.isolate=0' \
		'ap_lan.bridge_isolate=0' \
		'anonymous0.isolate=0' \
		'anonymous0.bridge_isolate=0' >"$CONFIG_DIR/wireless"
	chmod 600 "$CONFIG_DIR/wireless"
}

cat >"$BIN/uci" <<'EOF_UCI'
#!/bin/sh
set -eu
while [ "$#" -gt 0 ]; do
	case "$1" in
		-q) shift ;;
		-t) printf '%s\n' 'uci:unsafe-private-cli' >>"$PROXYPOOL_TEST_TRACE"; exit 90 ;;
		*) break ;;
	esac
done
command=${1:-}
[ "$#" -gt 0 ] && shift

inject_network_pending() {
	[ "${PROXYPOOL_TEST_NETWORK_INJECT_PENDING_AT:-}" = "$1" ] || return 0
	printf 'user-change\n' >"$PROXYPOOL_TEST_NETWORK_PENDING_FILE"
}

case "$command" in
	show)
	case "${1:-}" in
		network)
			cat <<'EOF_NETWORK_SHOW'
network.@device[0]=device
network.wan_device=device
EOF_NETWORK_SHOW
			[ "${PROXYPOOL_TEST_NETWORK_VARIANT:-normal}" != duplicate ] ||
				printf '%s\n' 'network.duplicate_lan=device'
			;;
		*) exit 2 ;;
		esac
		;;
	changes)
	case "${1:-}" in
		wireless)
			[ "${PROXYPOOL_TEST_PENDING:-0}" = 0 ] || printf '%s\n' 'wireless.user.setting=keep-me'
			for file in "$PROXYPOOL_TEST_DEFAULT_DELTA"/*; do
				[ -f "$file" ] || continue
				printf 'wireless.pending.%s=1\n' "${file##*/}"
			done
			;;
		network)
			[ "${PROXYPOOL_TEST_NETWORK_PENDING:-0}" = 0 ] &&
				[ ! -f "$PROXYPOOL_TEST_NETWORK_PENDING_FILE" ] ||
				printf '%s\n' 'network.@device[0].ports=lan1'
			;;
		*) exit 2 ;;
		esac
		;;
	get)
		key=${1:-}
		inject_network_pending "$key"
		case "$key" in
			network.@device\[0\].type) printf '%s\n' bridge ;;
			network.@device\[0\].name) printf '%s\n' br-lan ;;
			network.@device\[0\].ports)
				[ "${PROXYPOOL_TEST_NETWORK_VARIANT:-normal}" != empty ] || exit 1
				printf '%s\n' "${PROXYPOOL_TEST_EXPECTED_PORTS:-lan1 lan2 lan3 lan4 lan5}"
				;;
			network.wan_device.type) printf '%s\n' 8021q ;;
			network.wan_device.name) printf '%s\n' eth1.2 ;;
			network.duplicate_lan.type) printf '%s\n' bridge ;;
			network.duplicate_lan.name) printf '%s\n' br-lan ;;
			network.duplicate_lan.ports) printf '%s\n' lan99 ;;
			*) exit 1 ;;
		esac
		;;
	revert)
		printf 'uci:revert:%s\n' "${1:-}" >>"$PROXYPOOL_TEST_TRACE"
		exit 91
		;;
	*) exit 2 ;;
esac
EOF_UCI

cat >"$BIN/stat" <<'EOF_STAT'
#!/bin/sh
set -eu
[ "$#" -eq 3 ] || exit 2
if [ "$1" = -c ] && [ "$2" = '%a %u %h' ] &&
	[ "$3" = "$PROXYPOOL_TEST_DEFAULT_CONFIG" ]; then
	[ -f "$3" ] && [ ! -L "$3" ] || exit 1
	mode=$(cat "$PROXYPOOL_TEST_DEFAULT_MODE_STATE")
	printf '%s %s %s\n' "$mode" "${PROXYPOOL_TEST_DEFAULT_OWNER:-0}" \
		"${PROXYPOOL_TEST_DEFAULT_LINKS:-1}"
	exit 0
fi
[ "$1" = -c ] && [ "$2" = '%a %u %h %d' ] || exit 2
path=$3
if [ "${PROXYPOOL_TEST_STAT_WIDE_PATH:-}" = "$path" ]; then
	mode=777
elif [ -d "$path" ]; then
	mode=700
elif [ -f "$path" ]; then
	mode=600
else
	exit 1
fi
links=1
[ "${PROXYPOOL_TEST_STAT_LINK_PATH:-}" != "$path" ] || links=2
if [ -f "${PROXYPOOL_TEST_STAGE_LINK_STATE:-/nonexistent}" ]; then
	case "$path" in
		"$PROXYPOOL_TEST_STAGE_ROOT"/.wireless-stage.*/wireless) links=2 ;;
	esac
fi
printf '%s 0 %s 4242\n' "$mode" "$links"
EOF_STAT

cat >"$BIN/id" <<'EOF_ID'
#!/bin/sh
[ "$#" -eq 1 ] && [ "$1" = -u ] || exit 2
printf '%s\n' "${PROXYPOOL_TEST_UID:-0}"
EOF_ID

cat >"$BIN/staged-apply" <<'EOF_STAGED_APPLY'
#!/bin/sh
set -eu

[ "$#" -eq 3 ] || exit 2
action=$1
case "$action" in
	apply-wireless-isolation|disable-all-wireless|verify-all-wireless-disabled) : ;;
	*) exit 2 ;;
esac
case "$2" in /*) : ;; *) exit 2 ;; esac
case "$3" in /*) : ;; *) exit 2 ;; esac
case "$2" in
	"$PROXYPOOL_TEST_STAGE_ROOT"/.wireless-stage.*|\
	"$PROXYPOOL_TEST_QUARANTINE_ROOT"/.transaction.*|\
	"$PROXYPOOL_TEST_QUARANTINE_ROOT"/.verify.*) : ;;
	*) exit 2 ;;
esac
[ -d "$3" ] && [ ! -L "$3" ] || exit 2
[ -f "$2/wireless" ] && [ ! -L "$2/wireless" ] || exit 2

if [ "$action" = verify-all-wireless-disabled ]; then
	[ ! -s "$2/wireless" ] ||
		! grep -Eq '^[A-Za-z0-9_]+\.disabled=(0|false|no|off)$' "$2/wireless" || exit 74
	printf '%s\n' disabled
	exit 0
fi

if [ "$action" = disable-all-wireless ]; then
	[ "${PROXYPOOL_TEST_DISABLE_PARSE_FAIL:-0}" -eq 0 ] || exit 78
	if grep -Eq '^[A-Za-z0-9_]+\.disabled=(0|false|no|off)$' "$2/wireless"; then
		sed '/^[A-Za-z0-9_]*\.disabled=/s/=.*/=1/' "$2/wireless" >"$2/wireless.next"
		chmod 600 "$2/wireless.next"
		mv -f "$2/wireless.next" "$2/wireless"
		printf '%s\n' changed
	else
		printf '%s\n' unchanged
	fi
	exit 0
fi

[ "${PROXYPOOL_TEST_FORCE_ISOLATION_FAIL:-0}" -eq 0 ] || exit 77

inject_user_delta() {
	printf 'keep-me\n' >"$PROXYPOOL_TEST_DEFAULT_DELTA/user.setting"
}

maybe_fail_get() {
	[ "${PROXYPOOL_TEST_GET_FAIL:-}" != "$1" ] || {
		[ "${PROXYPOOL_TEST_INJECT_USER_DELTA:-0}" = 0 ] || inject_user_delta
		exit 75
	}
}

get_fixture() {
	key=$1
	maybe_fail_get "$key"
	case "$key" in
		wireless.ap_lan.mode) printf '%s\n' ap ;;
		wireless.ap_lan.network) printf '%s\n' 'lan guest' ;;
		wireless.ap_guest.mode) printf '%s\n' ap ;;
		wireless.ap_guest.network) printf '%s\n' guest ;;
		wireless.sta_lan.mode) printf '%s\n' sta ;;
		wireless.@wifi-iface\[0\].mode) printf '%s\n' ap ;;
		wireless.@wifi-iface\[0\].network) printf '%s\n' lan ;;
		*) exit 1 ;;
	esac
}

option_file() {
	case "$1" in
		wireless.ap_lan.isolate) printf '%s\n' ap_lan.isolate ;;
		wireless.ap_lan.bridge_isolate) printf '%s\n' ap_lan.bridge_isolate ;;
		wireless.@wifi-iface\[0\].isolate) printf '%s\n' anonymous0.isolate ;;
		wireless.@wifi-iface\[0\].bridge_isolate) printf '%s\n' anonymous0.bridge_isolate ;;
		*) return 1 ;;
	esac
}

read_option() {
	key=$1
	maybe_fail_get "$key"
	file=$(option_file "$key") || exit 1
	value=$(sed -n "s/^$file=//p" "$stage/wireless")
	[ -n "$value" ] || exit 1
	printf '%s\n' "$value"
}

append_change() {
	if [ -n "${changes:-}" ]; then
		changes="$changes
$1"
	else
		changes=$1
	fi
}

# This mirrors the ucode helper's all-read classification pass.  Nothing is
# written to either savedir while an unclassified late section can still fail.
stage=$2
targets=
for section in ap_lan ap_guest sta_lan '@wifi-iface[0]'; do
	mode=$(get_fixture "wireless.$section.mode") || exit $?
	[ "$mode" = ap ] || continue
	networks=$(get_fixture "wireless.$section.network") || exit $?
	case " $networks " in
		*' lan '*)
			if [ -n "$targets" ]; then targets="$targets $section"; else targets=$section; fi
			;;
	esac
done

changes=
for section in $targets; do
	for option in isolate bridge_isolate; do
		key="wireless.$section.$option"
		current=$(read_option "$key") || exit $?
		[ "$current" = 1 ] || append_change "$key=1"
	done
done

if [ -z "$changes" ]; then
	printf '%s\n' "${PROXYPOOL_TEST_STAGE_OUTPUT:-unchanged}"
	exit 0
fi

old_ifs=$IFS
IFS='
'
for assignment in $changes; do
	printf 'uci:set:%s\n' "$assignment" >>"$PROXYPOOL_TEST_TRACE"
	if [ "${PROXYPOOL_TEST_SET_FAIL:-}" = "$assignment" ]; then
		[ "${PROXYPOOL_TEST_INJECT_USER_DELTA:-0}" = 0 ] || inject_user_delta
		exit 71
	fi
done
IFS=$old_ifs

printf 'uci:commit:wireless\n' >>"$PROXYPOOL_TEST_TRACE"
if [ "${PROXYPOOL_TEST_COMMIT_RC:-0}" -ne 0 ]; then
	[ "${PROXYPOOL_TEST_INJECT_USER_DELTA:-0}" = 0 ] || inject_user_delta
	exit "$PROXYPOOL_TEST_COMMIT_RC"
fi
[ "${PROXYPOOL_TEST_INJECT_USER_DELTA_AT_COMMIT:-0}" = 0 ] || inject_user_delta

IFS='
'
sed \
	-e 's/^ap_lan\.isolate=.*/ap_lan.isolate=1/' \
	-e 's/^ap_lan\.bridge_isolate=.*/ap_lan.bridge_isolate=1/' \
	-e 's/^anonymous0\.isolate=.*/anonymous0.isolate=1/' \
	-e 's/^anonymous0\.bridge_isolate=.*/anonymous0.bridge_isolate=1/' \
	"$stage/wireless" >"$stage/wireless.next"
chmod 600 "$stage/wireless.next"
mv -f "$stage/wireless.next" "$stage/wireless"
IFS=$old_ifs
[ "${PROXYPOOL_TEST_STAGE_LINK:-0}" -eq 0 ] ||
	printf 'linked\n' >"$PROXYPOOL_TEST_STAGE_LINK_STATE"

printf '%s\n' "${PROXYPOOL_TEST_STAGE_OUTPUT:-changed}"
EOF_STAGED_APPLY

cat >"$BIN/bridge" <<'EOF_BRIDGE'
#!/bin/sh
set -eu
[ "$#" -eq 6 ] && [ "$1" = link ] && [ "$2" = set ] && [ "$3" = dev ] &&
	[ "$5" = isolated ] && [ "$6" = on ] || exit 2
port=$4
printf 'bridge:isolate:%s\n' "$port" >>"$PROXYPOOL_TEST_TRACE"
[ "${PROXYPOOL_TEST_BRIDGE_FAIL:-}" != "$port" ] || exit 73
[ "${PROXYPOOL_TEST_BRIDGE_NO_EFFECT:-}" != "$port" ] || exit 0
printf '1\n' >"$PROXYPOOL_TEST_SYS_CLASS_NET/$port/brport/isolated"
if [ "${PROXYPOOL_TEST_REQUEST_DURING_BRIDGE:-}" = "$port" ]; then
	sh "$PROXYPOOL_TEST_HELPER" request
fi
EOF_BRIDGE

cat >"$BIN/wifi" <<'EOF_WIFI'
#!/bin/sh
[ "$#" -eq 1 ] || exit 2
printf 'wifi:%s\n' "$1" >>"$PROXYPOOL_TEST_TRACE"
[ "$1" = down ] || exit 92
exit "${PROXYPOOL_TEST_WIFI_DOWN_RC:-0}"
EOF_WIFI

cat >"$BIN/wireless-down-probe" <<'EOF_WIRELESS_DOWN_PROBE'
#!/bin/sh
set -eu
[ "$#" -eq 1 ] && [ "$1" = "${PROXYPOOL_TEST_MODE:-apply}" ] || exit 2
printf 'wifi:probe\n' >>"$PROXYPOOL_TEST_TRACE"
grep -Fxq 'wifi:down' "$PROXYPOOL_TEST_TRACE" || exit 96
probe_count=$(grep -c '^wifi:probe$' "$PROXYPOOL_TEST_TRACE")
[ "$probe_count" -gt "${PROXYPOOL_TEST_WIFI_PROBE_FAILS:-0}" ] || exit 97
exit "${PROXYPOOL_TEST_WIFI_PROBE_RC:-0}"
EOF_WIRELESS_DOWN_PROBE

cat >"$BIN/sleep" <<'EOF_SLEEP'
#!/bin/sh
[ "$#" -eq 1 ] && [ "$1" = 1 ] || exit 2
printf 'wifi:sleep\n' >>"$PROXYPOOL_TEST_TRACE"
EOF_SLEEP

cat >"$BIN/ubus" <<'EOF_UBUS'
#!/bin/sh
set -eu
case "$*" in
	'-S list')
		printf 'ubus:list:all\n' >>"$PROXYPOOL_TEST_TRACE"
		[ "${PROXYPOOL_TEST_UBUS_LIST_RC:-0}" -eq 0 ] ||
			exit "$PROXYPOOL_TEST_UBUS_LIST_RC"
		printf '%s\n' system
		[ -z "${PROXYPOOL_TEST_HOSTAPD_OBJECTS:-}" ] ||
			printf '%s\n' "$PROXYPOOL_TEST_HOSTAPD_OBJECTS"
		;;
	'-S list hostapd.*')
		printf 'ubus:list:hostapd-pattern\n' >>"$PROXYPOOL_TEST_TRACE"
		[ "${PROXYPOOL_TEST_UBUS_LIST_RC:-0}" -eq 0 ] ||
			exit "$PROXYPOOL_TEST_UBUS_LIST_RC"
		[ -n "${PROXYPOOL_TEST_HOSTAPD_OBJECTS:-}" ] || exit 4
		printf '%s\n' "$PROXYPOOL_TEST_HOSTAPD_OBJECTS"
		;;
	'-S call network.wireless status')
		printf 'ubus:wireless:status\n' >>"$PROXYPOOL_TEST_TRACE"
		[ "${PROXYPOOL_TEST_UBUS_STATUS_RC:-0}" -eq 0 ] ||
			exit "$PROXYPOOL_TEST_UBUS_STATUS_RC"
		printf '%s\n' '{"fixture":"wireless-status"}'
		;;
	*) exit 2 ;;
esac
EOF_UBUS

cat >"$BIN/jshn.sh" <<'EOF_JSHN'
json_init() { PROXYPOOL_TEST_JSON_RADIO=; }
json_load() {
	[ "$#" -eq 1 ] && [ "$1" = '{"fixture":"wireless-status"}' ] || return 1
	[ "${PROXYPOOL_TEST_JSON_LOAD_RC:-0}" -eq 0 ]
}
json_get_keys() {
	[ "$#" -eq 1 ] || return 1
	eval "$1=\${PROXYPOOL_TEST_RADIOS:-radio0}"
}
json_select() {
	[ "$#" -eq 1 ] || return 1
	if [ "$1" = .. ]; then
		PROXYPOOL_TEST_JSON_RADIO=
	else
		PROXYPOOL_TEST_JSON_RADIO=$1
	fi
}
json_get_type() {
	[ "$#" -eq 2 ] && [ -n "$PROXYPOOL_TEST_JSON_RADIO" ] || return 1
	destination=$1
	key=$2
	[ "${PROXYPOOL_TEST_RADIO_MISSING:-}" != "$key" ] || return 1
	value=boolean
	[ "${PROXYPOOL_TEST_RADIO_STRING_FIELD:-}" != "$key" ] || value=string
	eval "$destination=\$value"
}
json_get_var() {
	[ "$#" -eq 2 ] && [ -n "$PROXYPOOL_TEST_JSON_RADIO" ] || return 1
	destination=$1
	key=$2
	[ "${PROXYPOOL_TEST_RADIO_MISSING:-}" != "$key" ] || return 1
	case "$key" in
		up) value=${PROXYPOOL_TEST_RADIO_UP:-0} ;;
		pending) value=${PROXYPOOL_TEST_RADIO_PENDING:-0} ;;
		autostart) value=${PROXYPOOL_TEST_RADIO_AUTOSTART:-0} ;;
		*) return 1 ;;
	esac
	eval "$destination=\$value"
}
EOF_JSHN

cat >"$BIN/default-wifi" <<'EOF_DEFAULT_WIFI'
#!/bin/sh
set -eu
[ "$#" -eq 1 ] && [ "$1" = config ] || exit 2
printf 'default:wifi:config\n' >>"$PROXYPOOL_TEST_TRACE"
printf 'stock-wireless\n' >"$PROXYPOOL_TEST_DEFAULT_CONFIG"
printf '644\n' >"$PROXYPOOL_TEST_DEFAULT_MODE_STATE"
EOF_DEFAULT_WIFI

cat >"$BIN/default-chmod" <<'EOF_DEFAULT_CHMOD'
#!/bin/sh
set -eu
[ "$#" -eq 2 ] && [ "$1" = 600 ] &&
	[ "$2" = "$PROXYPOOL_TEST_DEFAULT_CONFIG" ] || exit 2
printf 'default:chmod:600\n' >>"$PROXYPOOL_TEST_TRACE"
printf '600\n' >"$PROXYPOOL_TEST_DEFAULT_MODE_STATE"
EOF_DEFAULT_CHMOD

cat >"$BIN/default-helper" <<'EOF_DEFAULT_HELPER'
#!/bin/sh
set -eu
[ "$#" -eq 1 ] && [ "$1" = boot ] || exit 2
[ "$(cat "$PROXYPOOL_TEST_DEFAULT_MODE_STATE")" = 600 ] || exit 95
printf 'default:helper:boot\n' >>"$PROXYPOOL_TEST_TRACE"
EOF_DEFAULT_HELPER

cat >"$BIN/install-wireless" <<'EOF_INSTALL_WIRELESS'
#!/bin/sh
set -eu
[ "$#" -eq 2 ] || exit 2
printf 'wireless:install\n' >>"$PROXYPOOL_TEST_TRACE"
[ "${PROXYPOOL_TEST_INSTALL_RC:-0}" -eq 0 ] || exit "$PROXYPOOL_TEST_INSTALL_RC"
mv -f "$1" "$2"
EOF_INSTALL_WIRELESS
chmod 755 "$BIN/uci" "$BIN/stat" "$BIN/id" "$BIN/staged-apply" "$BIN/bridge" "$BIN/wifi" \
	"$BIN/wireless-down-probe" "$BIN/sleep" "$BIN/default-wifi" "$BIN/default-chmod" \
	"$BIN/ubus" "$BIN/default-helper" "$BIN/install-wireless"

run_wireless_default() {
	env \
		PROXYPOOL_WIRELESS_CONFIG="$DEFAULT_CONFIG" \
		PROXYPOOL_WIFI="$BIN/default-wifi" \
		PROXYPOOL_LAN_ISOLATION="$BIN/default-helper" \
		PROXYPOOL_STAT="$BIN/stat" \
		PROXYPOOL_CHMOD="$BIN/default-chmod" \
		PROXYPOOL_TEST_DEFAULT_CONFIG="$DEFAULT_CONFIG" \
		PROXYPOOL_TEST_DEFAULT_MODE_STATE="$DEFAULT_MODE_STATE" \
		PROXYPOOL_TEST_TRACE="$TRACE" \
		"$@" \
		sh "$WIRELESS_DEFAULT"
}

# OpenWrt's stock `wifi config` creates /etc/config/wireless with the caller's
# normal 022 umask.  The image default must converge both a fresh 0644 file and
# an upgrade's pre-existing 0644 file to root:0600 before the isolation helper
# is allowed to read or stage credentials.
rm -f "$DEFAULT_CONFIG" "$DEFAULT_MODE_STATE"
: >"$TRACE"
run_wireless_default || fail 'fresh stock wireless config was not safely converged'
[ "$(cat "$DEFAULT_MODE_STATE")" = 600 ] || fail 'fresh wireless config remained world-readable'
expected_default_trace='default:wifi:config
default:chmod:600
default:helper:boot'
[ "$(cat "$TRACE")" = "$expected_default_trace" ] ||
	fail 'fresh wireless default used the wrong permission/helper order'

printf 'existing-wireless\n' >"$DEFAULT_CONFIG"
printf '644\n' >"$DEFAULT_MODE_STATE"
: >"$TRACE"
run_wireless_default || fail 'existing 0644 wireless config was not safely converged'
[ "$(cat "$TRACE")" = 'default:chmod:600
default:helper:boot' ] || fail 'existing wireless config was regenerated or staged before chmod'

printf '644\n' >"$DEFAULT_MODE_STATE"
: >"$TRACE"
if run_wireless_default PROXYPOOL_TEST_DEFAULT_OWNER=1000 >/dev/null 2>&1; then
	fail 'non-root-owned wireless config was accepted by the image default'
fi
[ ! -s "$TRACE" ] || fail 'unsafe wireless ownership caused a mutation'

printf '644\n' >"$DEFAULT_MODE_STATE"
: >"$TRACE"
if run_wireless_default PROXYPOOL_TEST_DEFAULT_LINKS=2 >/dev/null 2>&1; then
	fail 'hard-linked wireless config was accepted by the image default'
fi
[ ! -s "$TRACE" ] || fail 'hard-linked wireless config was chmodded or staged'

for port in lan1 lan2 lan3 lan4 lan5 phy0-ap0; do
	mkdir -p "$SYS_CLASS_NET/$port/brport"
	printf '0\n' >"$SYS_CLASS_NET/$port/brport/isolated"
	# A real sysfs brif entry is a symlink.  Directory fixtures keep this test
	# portable on Windows while exercising the same basename enumeration.
	mkdir "$SYS_CLASS_NET/br-lan/brif/$port"
done

run_helper() {
	env \
		PROXYPOOL_UCI="$BIN/uci" \
		PROXYPOOL_UCI_STAGED_APPLY="$BIN/staged-apply" \
		PROXYPOOL_CONFIG_DIR="$CONFIG_DIR" \
		PROXYPOOL_WIRELESS_STAGE_ROOT="$STAGE_ROOT" \
		PROXYPOOL_WIRELESS_REBOOT_MARKER="$REBOOT_MARKER" \
		PROXYPOOL_WIRELESS_QUARANTINE_ROOT="$WIRELESS_QUARANTINE_ROOT" \
		PROXYPOOL_LAN_STATE_ROOT="$LAN_STATE_ROOT" \
		PROXYPOOL_WIRELESS_INSTALL="$BIN/install-wireless" \
		PROXYPOOL_STAT="$BIN/stat" \
		PROXYPOOL_ID="$BIN/id" \
		PROXYPOOL_BRIDGE="$BIN/bridge" \
		PROXYPOOL_WIFI="$BIN/wifi" \
		PROXYPOOL_WIRELESS_DOWN_PROBE="$BIN/wireless-down-probe" \
		PROXYPOOL_UBUS="$BIN/ubus" \
		PROXYPOOL_JSHN="$BIN/jshn.sh" \
		PROXYPOOL_SLEEP="$BIN/sleep" \
		PROXYPOOL_SYNC=/bin/true \
		PROXYPOOL_SYS_CLASS_NET="$SYS_CLASS_NET" \
		PROXYPOOL_CPU_ONLY_MARKER="$CPU_ONLY_MARKER" \
		PROXYPOOL_TEST_DEFAULT_DELTA="$DEFAULT_DELTA" \
		PROXYPOOL_TEST_STAGE_ROOT="$STAGE_ROOT" \
		PROXYPOOL_TEST_QUARANTINE_ROOT="$WIRELESS_QUARANTINE_ROOT" \
		PROXYPOOL_TEST_NETWORK_PENDING_FILE="$NETWORK_PENDING_FILE" \
		PROXYPOOL_TEST_STAGE_LINK_STATE="$STAGE_LINK_STATE" \
		PROXYPOOL_TEST_SYS_CLASS_NET="$SYS_CLASS_NET" \
		PROXYPOOL_TEST_HELPER="$HELPER" \
		PROXYPOOL_TEST_TRACE="$TRACE" \
		PROXYPOOL_TMPDIR="$TEST_TMP" \
		"$@" \
		sh "$HELPER" "${PROXYPOOL_TEST_MODE:-apply}"
}

# The custom-kernel CPU-only bridge proof is a prerequisite for every helper
# mode.  Missing, false, malformed, or symlinked markers fail before mutation.
CPU_ONLY_MARKER_TARGET="$TEST_TMP/proxypool_cpu_only_bridge.target"
printf 'Y\n' >"$CPU_ONLY_MARKER_TARGET"
for marker_failure in missing false malformed trailing symlink; do
	rm -f "$CPU_ONLY_MARKER"
	case "$marker_failure" in
		missing) : ;;
		false) printf 'N\n' >"$CPU_ONLY_MARKER" ;;
		malformed) printf 'Y\nY\n' >"$CPU_ONLY_MARKER" ;;
		trailing) printf 'Y\njunk' >"$CPU_ONLY_MARKER" ;;
		symlink)
			MSYS=winsymlinks:nativestrict \
				ln -s "$CPU_ONLY_MARKER_TARGET" "$CPU_ONLY_MARKER" 2>/dev/null ||
				fail 'host cannot create CPU-only marker symlink fixture'
			[ -L "$CPU_ONLY_MARKER" ] || fail 'host did not create a real marker symlink fixture'
			;;
	esac
	: >"$TRACE"
	if PROXYPOOL_TEST_MODE=configure run_helper >/dev/null 2>&1; then
		fail "$marker_failure CPU-only bridge marker was accepted"
	fi
	[ ! -s "$TRACE" ] || fail "$marker_failure CPU-only bridge marker caused a mutation"
done
rm -f "$CPU_ONLY_MARKER"
printf 'Y\n' >"$CPU_ONLY_MARKER"

# A semantic or parse failure before netifd starts must replace the active
# wireless package with a persistent, explicitly disabled fallback.  Merely
# calling `wifi down` is insufficient because S20 can otherwise start the
# original guest/mesh AP after this helper returns.
reset_wireless_config
cp "$CONFIG_DIR/wireless" "$TEST_TMP/wireless-quarantine.before"
: >"$TRACE"
if PROXYPOOL_TEST_MODE=boot run_helper PROXYPOOL_TEST_FORCE_ISOLATION_FAIL=1 \
	>/dev/null 2>&1; then
	fail 'cold-start wireless validation failure was accepted'
fi
[ "$(cat "$WIRELESS_QUARANTINE_ROOT/state")" = DISABLED ] ||
	fail 'cold-start wireless failure did not publish persistent DISABLED state'
cmp -s "$TEST_TMP/wireless-quarantine.before" "$WIRELESS_QUARANTINE_ROOT/recovery" ||
	fail 'wireless quarantine did not preserve the original bytes for recovery'
for section in radio0 ap_lan anonymous0; do
	[ "$(wireless_value "$section.disabled")" = 1 ] ||
		fail "wireless quarantine left $section enabled"
done
grep -Fxq 'wifi:down' "$TRACE" || fail 'wireless quarantine did not stop runtime Wi-Fi'
grep -Fxq 'wireless:install' "$TRACE" || fail 'wireless quarantine did not atomically install its fallback'
rm -rf "$LAN_STATE_ROOT" "$REBOOT_MARKER"
if PROXYPOOL_TEST_MODE=readiness run_helper >/dev/null 2>&1; then
	fail 'persistent wireless quarantine was reported LAN-ready'
fi

# A later cold invocation must recognize the exact installed fallback and
# keep it disabled without repeatedly replacing the active file.  It may let
# S20 start wired management, while readiness continues to deny ProxyPool.
: >"$TRACE"
PROXYPOOL_TEST_MODE=boot run_helper ||
	fail 'verified disabled fallback did not release the wired-management boot'
if grep -Fxq 'wireless:install' "$TRACE"; then
	fail 'wireless quarantine re-entry replaced an already verified fallback'
fi
grep -Fxq 'wifi:down' "$TRACE" || fail 'wireless quarantine re-entry did not re-prove shutdown'

# Wired administration may replace the disabled fallback with a fully valid,
# already-isolated topology.  Only a byte-different file plus the complete
# normal validator may retire the persistent quarantine.
reset_wireless_config
sed -i 's/\.isolate=0$/.isolate=1/; s/\.bridge_isolate=0$/.bridge_isolate=1/' \
	"$CONFIG_DIR/wireless"
rm -rf "$REBOOT_MARKER" "$LAN_STATE_ROOT"
: >"$TRACE"
PROXYPOOL_TEST_MODE=boot run_helper ||
	fail 'fully validated wired repair could not retire wireless quarantine'
[ ! -e "$WIRELESS_QUARANTINE_ROOT" ] && [ ! -L "$WIRELESS_QUARANTINE_ROOT" ] ||
	fail 'fully validated wired repair retained persistent wireless quarantine'

rm -rf "$REBOOT_MARKER" "$LAN_STATE_ROOT"
printf '%s\n' 'not valid UCI {' >"$CONFIG_DIR/wireless"
chmod 600 "$CONFIG_DIR/wireless"
cp "$CONFIG_DIR/wireless" "$TEST_TMP/wireless-malformed.before"
: >"$TRACE"
if PROXYPOOL_TEST_MODE=boot run_helper PROXYPOOL_TEST_FORCE_ISOLATION_FAIL=1 \
	PROXYPOOL_TEST_DISABLE_PARSE_FAIL=1 >/dev/null 2>&1; then
	fail 'malformed cold-start wireless package was accepted'
fi
[ "$(cat "$WIRELESS_QUARANTINE_ROOT/state")" = DISABLED ] ||
	fail 'malformed wireless package did not reach persistent DISABLED state'
cmp -s "$TEST_TMP/wireless-malformed.before" "$WIRELESS_QUARANTINE_ROOT/recovery" ||
	fail 'malformed wireless bytes were not preserved for recovery'
[ ! -s "$CONFIG_DIR/wireless" ] ||
	fail 'malformed wireless package was not replaced by the inert empty fallback'

# Saved UCI deltas are user transaction state and must never be committed or
# deleted.  The committed fallback is installed, but PREPARING remains a boot
# hold until a real cold reboot clears /tmp/.uci and a second proof completes.
rm -rf "$WIRELESS_QUARANTINE_ROOT" "$REBOOT_MARKER" "$LAN_STATE_ROOT"
reset_wireless_config
: >"$TRACE"
if PROXYPOOL_TEST_MODE=boot run_helper PROXYPOOL_TEST_PENDING=1 >/dev/null 2>&1; then
	fail 'pending wireless delta crossed the cold-start quarantine boundary'
fi
[ "$(cat "$WIRELESS_QUARANTINE_ROOT/state")" = PREPARING ] ||
	fail 'pending wireless delta did not retain PREPARING boot-hold state'
[ "$(wireless_value 'radio0.disabled')" = 1 ] ||
	fail 'pending wireless delta did not install the committed disabled fallback'
rm -rf "$REBOOT_MARKER" "$LAN_STATE_ROOT"
: >"$TRACE"
PROXYPOOL_TEST_MODE=boot run_helper ||
	fail 'post-tmpfs quarantine recovery did not release the disabled wired-management boot'
[ "$(cat "$WIRELESS_QUARANTINE_ROOT/state")" = DISABLED ] ||
	fail 'cold retry did not finish PREPARING wireless quarantine'

rm -rf "$WIRELESS_QUARANTINE_ROOT" "$REBOOT_MARKER" "$LAN_STATE_ROOT"
reset_wireless_config

# Existing quarantine evidence is never guessed away.  A wrong-mode or
# multiply-linked state file must survive byte-for-byte and force the boot hold
# before any replacement of the active wireless package.
for quarantine_state_fault in mode links; do
	mkdir "$WIRELESS_QUARANTINE_ROOT"
	chmod 700 "$WIRELESS_QUARANTINE_ROOT"
	printf 'attacker-state\n' >"$WIRELESS_QUARANTINE_ROOT/state"
	cp "$CONFIG_DIR/wireless" "$WIRELESS_QUARANTINE_ROOT/disabled"
	cp "$CONFIG_DIR/wireless" "$WIRELESS_QUARANTINE_ROOT/recovery"
	chmod 600 "$WIRELESS_QUARANTINE_ROOT/state" \
		"$WIRELESS_QUARANTINE_ROOT/disabled" "$WIRELESS_QUARANTINE_ROOT/recovery"
	: >"$TRACE"
	quarantine_state_knob=
	case "$quarantine_state_fault" in
		mode) quarantine_state_knob="PROXYPOOL_TEST_STAT_WIDE_PATH=$WIRELESS_QUARANTINE_ROOT/state" ;;
		links) quarantine_state_knob="PROXYPOOL_TEST_STAT_LINK_PATH=$WIRELESS_QUARANTINE_ROOT/state" ;;
	esac
	if PROXYPOOL_TEST_MODE=boot run_helper $quarantine_state_knob >/dev/null 2>&1; then
		fail "unsafe quarantine state $quarantine_state_fault was accepted"
	fi
	[ "$(cat "$WIRELESS_QUARANTINE_ROOT/state")" = attacker-state ] ||
		fail "unsafe quarantine state $quarantine_state_fault was replaced"
	if grep -Fxq 'wireless:install' "$TRACE"; then
		fail "unsafe quarantine state $quarantine_state_fault changed active wireless config"
	fi
	rm -rf "$WIRELESS_QUARANTINE_ROOT" "$REBOOT_MARKER" "$LAN_STATE_ROOT"
done

: >"$TRACE"
if [ "${PROXYPOOL_TEST_FOCUS_WIRELESS_QUARANTINE:-0}" = 1 ]; then
	echo 'PASS: focused persistent wireless quarantine'
	exit 0
fi

# A bridge event is durable state, not a bounded hotplug attempt.  Before the
# serialized worker exists, readiness must fail.  Requesting work creates a
# root-only pending marker; only a complete reconciliation may retire it.
[ ! -e "$LAN_PENDING_MARKER" ] || rmdir "$LAN_PENDING_MARKER"
[ ! -e "$LAN_INFLIGHT_MARKER" ] || rmdir "$LAN_INFLIGHT_MARKER"
[ ! -e "$LAN_STATE_ROOT" ] || rmdir "$LAN_STATE_ROOT"
if PROXYPOOL_TEST_MODE=readiness run_helper >/dev/null 2>&1; then
	fail 'an uninitialized LAN convergence state was reported ready'
fi
PROXYPOOL_TEST_MODE=request run_helper || fail 'LAN convergence request could not be published'
[ -d "$LAN_STATE_ROOT" ] && [ ! -L "$LAN_STATE_ROOT" ] ||
	fail 'LAN convergence request did not create a private state root'
[ -d "$LAN_PENDING_MARKER" ] && [ ! -L "$LAN_PENDING_MARKER" ] ||
	fail 'LAN convergence request did not create a pending marker'
if PROXYPOOL_TEST_MODE=readiness run_helper >/dev/null 2>&1; then
	fail 'pending LAN convergence was reported ready'
fi
PROXYPOOL_TEST_MODE=reconcile run_helper || fail 'pending LAN convergence did not reconcile'
[ ! -e "$LAN_PENDING_MARKER" ] && [ ! -L "$LAN_PENDING_MARKER" ] ||
	fail 'successful LAN convergence retained its pending marker'
[ ! -e "$LAN_INFLIGHT_MARKER" ] && [ ! -L "$LAN_INFLIGHT_MARKER" ] ||
	fail 'successful LAN convergence retained its in-flight marker'
PROXYPOOL_TEST_MODE=readiness run_helper ||
	fail 'fully reconciled LAN convergence was not reported ready'

# Verification is a read-only periodic audit.  It detects drift without
# publishing or clearing work; the worker owns that state transition.
printf '0\n' >"$SYS_CLASS_NET/lan2/brport/isolated"
: >"$TRACE"
if PROXYPOOL_TEST_MODE=verify run_helper >/dev/null 2>&1; then
	fail 'read-only LAN audit accepted an unisolated bridge member'
fi
[ ! -s "$TRACE" ] || fail 'read-only LAN audit mutated an unisolated bridge member'
[ ! -e "$LAN_PENDING_MARKER" ] && [ ! -e "$LAN_INFLIGHT_MARKER" ] ||
	fail 'read-only LAN audit changed convergence markers'
printf '1\n' >"$SYS_CLASS_NET/lan2/brport/isolated"
PROXYPOOL_TEST_MODE=verify run_helper || fail 'read-only LAN audit rejected fully isolated members'

# An early event can be claimed before netifd enslaves every configured port.
# Failure retains in-flight ownership; one later successful full proof resumes
# that same claim and retires it without manufacturing another pending event.
PROXYPOOL_TEST_MODE=request run_helper || fail 'could not request early bridge reconciliation'
rmdir "$SYS_CLASS_NET/br-lan/brif/lan2"
if PROXYPOOL_TEST_MODE=reconcile run_helper >/dev/null 2>&1; then
	fail 'early bridge event was accepted before its configured member appeared'
fi
[ -d "$LAN_INFLIGHT_MARKER" ] && [ ! -L "$LAN_INFLIGHT_MARKER" ] ||
	fail 'failed early bridge reconciliation did not retain in-flight state'
[ ! -e "$LAN_PENDING_MARKER" ] && [ ! -L "$LAN_PENDING_MARKER" ] ||
	fail 'claiming an early bridge event retained duplicate pending state'
mkdir "$SYS_CLASS_NET/br-lan/brif/lan2"
PROXYPOOL_TEST_MODE=reconcile run_helper || fail 'in-flight bridge work did not resume when topology became ready'
[ ! -e "$LAN_PENDING_MARKER" ] && [ ! -e "$LAN_INFLIGHT_MARKER" ] ||
	fail 'resumed bridge reconciliation did not retire its only claim'

# A new event that arrives while a pass owns inflight must create a fresh
# pending marker.  Completing the old pass may not erase that newer request.
for port in lan1 lan2 lan3 lan4 lan5 phy0-ap0; do
	printf '0\n' >"$SYS_CLASS_NET/$port/brport/isolated"
done
PROXYPOOL_TEST_MODE=request run_helper || fail 'could not request concurrent-event fixture'
PROXYPOOL_TEST_MODE=reconcile run_helper PROXYPOOL_TEST_REQUEST_DURING_BRIDGE=lan1 ||
	fail 'first pass with a concurrent bridge event did not complete'
[ ! -e "$LAN_INFLIGHT_MARKER" ] || fail 'completed first pass retained stale in-flight state'
[ -d "$LAN_PENDING_MARKER" ] && [ ! -L "$LAN_PENDING_MARKER" ] ||
	fail 'event arriving during reconciliation was lost'
if PROXYPOOL_TEST_MODE=readiness run_helper >/dev/null 2>&1; then
	fail 'new event arriving during reconciliation was reported ready'
fi
PROXYPOOL_TEST_MODE=reconcile run_helper || fail 'second pass did not consume the concurrent event'
PROXYPOOL_TEST_MODE=readiness run_helper || fail 'two complete convergence passes did not become ready'

# Unsafe marker layouts are evidence, never objects the helper may replace or
# delete.  A nonempty marker and wrong-mode marker both remain fail closed.
mkdir "$LAN_PENDING_MARKER"
printf 'foreign\n' >"$LAN_PENDING_MARKER/payload"
if PROXYPOOL_TEST_MODE=request run_helper >/dev/null 2>&1; then
	fail 'nonempty pending LAN marker was accepted'
fi
[ "$(cat "$LAN_PENDING_MARKER/payload")" = foreign ] ||
	fail 'nonempty pending LAN marker payload was modified'
rm -f "$LAN_PENDING_MARKER/payload"
rmdir "$LAN_PENDING_MARKER"
mkdir "$LAN_PENDING_MARKER"
if PROXYPOOL_TEST_MODE=readiness run_helper \
	PROXYPOOL_TEST_STAT_WIDE_PATH="$LAN_PENDING_MARKER" >/dev/null 2>&1; then
	fail 'wrong-mode pending LAN marker was accepted'
fi
[ -d "$LAN_PENDING_MARKER" ] || fail 'wrong-mode pending LAN marker was deleted'
rmdir "$LAN_PENDING_MARKER"

# A runtime wireless transaction that requires a cold reboot is also an
# unresolved L2 boundary even if bridge convergence markers are clear.
mkdir "$REBOOT_MARKER"
if PROXYPOOL_TEST_MODE=readiness run_helper >/dev/null 2>&1; then
	fail 'wireless cold-reboot boundary was reported LAN-ready'
fi
rmdir "$REBOOT_MARKER"
PROXYPOOL_TEST_MODE=readiness run_helper || fail 'cleared wireless reboot boundary did not restore LAN readiness'

# The procd child is the only component that repeatedly mutates bridge state.
# One execution covers both failure backoff after an early net event and the
# idle periodic audit that repairs a completely missed event.
[ -f "$WORKER" ] || fail 'missing serialized LAN convergence worker'
WORKER_HELPER="$BIN/lan-isolation-worker-helper"
cp "$HELPER" "$WORKER_HELPER"
chmod 755 "$WORKER_HELPER"
WORKER_SLEEP="$BIN/worker-sleep"
WORKER_SLEEP_COUNT="$TEST_TMP/worker-sleep.count"
cat >"$WORKER_SLEEP" <<'EOF_WORKER_SLEEP'
#!/bin/sh
set -eu
[ "$#" -eq 1 ] || exit 2
count=0
[ ! -f "$PROXYPOOL_TEST_WORKER_SLEEP_COUNT" ] ||
	count=$(cat "$PROXYPOOL_TEST_WORKER_SLEEP_COUNT")
count=$((count + 1))
printf '%s\n' "$count" >"$PROXYPOOL_TEST_WORKER_SLEEP_COUNT"
printf 'worker:sleep:%s\n' "$1" >>"$PROXYPOOL_TEST_TRACE"
case "$count:$1" in
	1:1)
		mkdir "$PROXYPOOL_TEST_SYS_CLASS_NET/br-lan/brif/lan2"
		;;
	2:30)
		printf '0\n' >"$PROXYPOOL_TEST_SYS_CLASS_NET/phy0-ap0/brport/isolated"
		;;
	3:30)
		exit 91
		;;
	*) exit 92 ;;
esac
EOF_WORKER_SLEEP
chmod 755 "$WORKER_SLEEP"

run_worker() {
	env \
		PROXYPOOL_LAN_ISOLATION="$WORKER_HELPER" \
		PROXYPOOL_WORKER_SLEEP="$WORKER_SLEEP" \
		PROXYPOOL_DEFERRED_START_MARKER="$TEST_TMP/worker-start-deferred" \
		PROXYPOOL_UCI="$BIN/uci" \
		PROXYPOOL_STAT="$BIN/stat" \
		PROXYPOOL_ID="$BIN/id" \
		PROXYPOOL_BRIDGE="$BIN/bridge" \
		PROXYPOOL_WIRELESS_REBOOT_MARKER="$REBOOT_MARKER" \
		PROXYPOOL_LAN_STATE_ROOT="$LAN_STATE_ROOT" \
		PROXYPOOL_SYS_CLASS_NET="$SYS_CLASS_NET" \
		PROXYPOOL_CPU_ONLY_MARKER="$CPU_ONLY_MARKER" \
		PROXYPOOL_TEST_DEFAULT_DELTA="$DEFAULT_DELTA" \
		PROXYPOOL_TEST_NETWORK_PENDING_FILE="$NETWORK_PENDING_FILE" \
		PROXYPOOL_TEST_SYS_CLASS_NET="$SYS_CLASS_NET" \
		PROXYPOOL_TEST_HELPER="$HELPER" \
		PROXYPOOL_TEST_TRACE="$TRACE" \
		PROXYPOOL_TEST_WORKER_SLEEP_COUNT="$WORKER_SLEEP_COUNT" \
		sh "$WORKER"
}

for marker in "$LAN_PENDING_MARKER" "$LAN_INFLIGHT_MARKER"; do
	[ ! -e "$marker" ] || rmdir "$marker"
done
for port in lan1 lan2 lan3 lan4 lan5 phy0-ap0; do
	printf '1\n' >"$SYS_CLASS_NET/$port/brport/isolated"
done
rmdir "$SYS_CLASS_NET/br-lan/brif/lan2"
printf '0\n' >"$SYS_CLASS_NET/lan2/brport/isolated"
rm -f "$WORKER_SLEEP_COUNT"
: >"$TRACE"
if run_worker >"$TEST_TMP/worker.log" 2>&1; then
	fail 'LAN convergence worker ignored a failed sleep primitive'
fi
[ -f "$WORKER_SLEEP_COUNT" ] || {
	cat "$TEST_TMP/worker.log" >&2
	fail 'LAN convergence worker exited before its first retry sleep'
}
[ "$(cat "$WORKER_SLEEP_COUNT")" -eq 3 ] || fail 'worker did not execute the bounded retry and periodic audit sequence'
expected_worker_sleeps='worker:sleep:1
worker:sleep:30
worker:sleep:30'
[ "$(grep '^worker:sleep:' "$TRACE")" = "$expected_worker_sleeps" ] ||
	fail 'worker used the wrong backoff or audit delays'
for port in lan2 phy0-ap0; do
	[ "$(cat "$SYS_CLASS_NET/$port/brport/isolated")" = 1 ] ||
		fail "worker did not converge $port after an early or missed event"
done
[ -d "$LAN_PENDING_MARKER" ] && [ ! -L "$LAN_PENDING_MARKER" ] ||
	fail 'exiting worker did not leave a durable pending request'
[ ! -e "$LAN_INFLIGHT_MARKER" ] || fail 'exiting worker retained a completed in-flight claim'
rmdir "$LAN_PENDING_MARKER"

# USR1 is a wakeup, not a worker failure.  Interrupting the idle sleep must
# begin another audit immediately without waiting for procd to respawn.
WAKE_HELPER="$BIN/worker-wake-helper"
WAKE_SLEEP="$BIN/worker-wake-sleep"
WAKE_SLEEP_COUNT="$TEST_TMP/worker-wake-sleep.count"
cat >"$WAKE_HELPER" <<'EOF_WAKE_HELPER'
#!/bin/sh
set -eu
case "${1:-}" in
	request|readiness|verify) exit 0 ;;
	*) exit 2 ;;
esac
EOF_WAKE_HELPER
cat >"$WAKE_SLEEP" <<'EOF_WAKE_SLEEP'
#!/bin/sh
set -eu
[ "$#" -eq 1 ] && [ "$1" = 30 ] || exit 2
count=0
[ ! -f "$PROXYPOOL_TEST_WAKE_SLEEP_COUNT" ] || count=$(cat "$PROXYPOOL_TEST_WAKE_SLEEP_COUNT")
count=$((count + 1))
printf '%s\n' "$count" >"$PROXYPOOL_TEST_WAKE_SLEEP_COUNT"
if [ "$count" -eq 1 ]; then
	kill -USR1 "$PPID"
	while :; do :; done
fi
exit 93
EOF_WAKE_SLEEP
chmod 755 "$WAKE_HELPER" "$WAKE_SLEEP"
rm -f "$WAKE_SLEEP_COUNT"
if PROXYPOOL_LAN_ISOLATION="$WAKE_HELPER" PROXYPOOL_WORKER_SLEEP="$WAKE_SLEEP" \
	PROXYPOOL_DEFERRED_START_MARKER="$TEST_TMP/worker-wake-start-deferred" \
	PROXYPOOL_TEST_WAKE_SLEEP_COUNT="$WAKE_SLEEP_COUNT" sh "$WORKER" \
	>"$TEST_TMP/worker-wake.log" 2>&1; then
	fail 'LAN worker accepted the terminal wake-sleep fixture failure'
fi
[ "$(cat "$WAKE_SLEEP_COUNT")" -eq 2 ] ||
	fail 'USR1 wakeup terminated the LAN worker instead of starting another audit'

# S99 can observe the fail-closed boundary while the singleton worker is still
# converging.  Its durable request must be retried by that same worker after
# readiness becomes true; otherwise the daemon stays absent until a manual
# reconnect/restart even though every safety proof has recovered.
DEFERRED_HELPER="$BIN/worker-deferred-helper"
DEFERRED_INIT="$BIN/worker-deferred-init"
DEFERRED_SLEEP="$BIN/worker-deferred-sleep"
DEFERRED_MARKER="$TEST_TMP/start-deferred"
DEFERRED_TRACE="$TEST_TMP/start-deferred.trace"
cat >"$DEFERRED_HELPER" <<'EOF_DEFERRED_HELPER'
#!/bin/sh
set -eu
case "${1:-}" in
	request|readiness|verify) exit 0 ;;
	*) exit 2 ;;
esac
EOF_DEFERRED_HELPER
cat >"$DEFERRED_INIT" <<'EOF_DEFERRED_INIT'
#!/bin/sh
set -eu
[ "$#" -eq 1 ] && [ "$1" = start ] || exit 2
printf 'init:start\n' >>"$PROXYPOOL_TEST_DEFERRED_TRACE"
rm -f "$PROXYPOOL_DEFERRED_START_MARKER"
EOF_DEFERRED_INIT
cat >"$DEFERRED_SLEEP" <<'EOF_DEFERRED_SLEEP'
#!/bin/sh
set -eu
[ "$#" -eq 1 ] && [ "$1" = 30 ] || exit 2
exit 93
EOF_DEFERRED_SLEEP
chmod 755 "$DEFERRED_HELPER" "$DEFERRED_INIT" "$DEFERRED_SLEEP"
: >"$DEFERRED_MARKER"
: >"$DEFERRED_TRACE"
if PROXYPOOL_LAN_ISOLATION="$DEFERRED_HELPER" \
	PROXYPOOL_WORKER_SLEEP="$DEFERRED_SLEEP" \
	PROXYPOOL_INIT="$DEFERRED_INIT" \
	PROXYPOOL_DEFERRED_START_MARKER="$DEFERRED_MARKER" \
	PROXYPOOL_TEST_DEFERRED_TRACE="$DEFERRED_TRACE" \
	sh "$WORKER" >"$TEST_TMP/worker-deferred.log" 2>&1; then
	fail 'deferred-start worker ignored the terminal sleep fixture failure'
fi
[ "$(cat "$DEFERRED_TRACE")" = 'init:start' ] ||
	fail 'ready LAN worker did not retry the deferred ProxyPool start'
[ ! -e "$DEFERRED_MARKER" ] && [ ! -L "$DEFERRED_MARKER" ] ||
	fail 'deferred-start retry did not consume its request marker'

# Hotplug only publishes durable work and makes a one-second best-effort wake
# call.  It never waits for topology, retries bridge operations, or treats a
# failed wakeup as loss of the already-published event.
HOTPLUG_HELPER="$BIN/hotplug-request-helper"
HOTPLUG_UBUS="$BIN/hotplug-ubus"
HOTPLUG_TRACE="$TEST_TMP/hotplug.trace"
cat >"$HOTPLUG_HELPER" <<'EOF_HOTPLUG_REQUEST'
#!/bin/sh
set -eu
[ "$#" -eq 1 ] && [ "$1" = request ] || exit 2
printf 'helper:request\n' >>"$PROXYPOOL_TEST_HOTPLUG_TRACE"
exit "${PROXYPOOL_TEST_HOTPLUG_REQUEST_RC:-0}"
EOF_HOTPLUG_REQUEST
cat >"$HOTPLUG_UBUS" <<'EOF_HOTPLUG_UBUS'
#!/bin/sh
set -eu
printf 'ubus:%s\n' "$*" >>"$PROXYPOOL_TEST_HOTPLUG_TRACE"
exit "${PROXYPOOL_TEST_HOTPLUG_UBUS_RC:-0}"
EOF_HOTPLUG_UBUS
chmod 755 "$HOTPLUG_HELPER" "$HOTPLUG_UBUS"

run_hotplug_request() {
	env \
		PROXYPOOL_LAN_ISOLATION="$HOTPLUG_HELPER" \
		PROXYPOOL_HOTPLUG_UBUS="$HOTPLUG_UBUS" \
		PROXYPOOL_TEST_HOTPLUG_TRACE="$HOTPLUG_TRACE" \
		PROXYPOOL_TEST_HOTPLUG_REQUEST_RC="${PROXYPOOL_TEST_HOTPLUG_REQUEST_RC:-0}" \
		PROXYPOOL_TEST_HOTPLUG_UBUS_RC="${PROXYPOOL_TEST_HOTPLUG_UBUS_RC:-0}" \
		"$@" sh "$HOTPLUG"
}

: >"$HOTPLUG_TRACE"
ACTION=add run_hotplug_request PROXYPOOL_TEST_HOTPLUG_UBUS_RC=9 ||
	fail 'hotplug discarded a durable request when best-effort wakeup failed'
[ "$(grep -c '^helper:request$' "$HOTPLUG_TRACE")" -eq 1 ] ||
	fail 'relevant hotplug event did not publish exactly one request'
grep -Eq '^ubus:-t 1 call service signal \{.*"instance":"lan-reconciler".*\}$' "$HOTPLUG_TRACE" ||
	fail 'hotplug did not issue the bounded lan-reconciler wakeup'
: >"$HOTPLUG_TRACE"
if ACTION=change run_hotplug_request PROXYPOOL_TEST_HOTPLUG_REQUEST_RC=74 >/dev/null 2>&1; then
	fail 'hotplug accepted a failed durable request publication'
fi
[ "$(cat "$HOTPLUG_TRACE")" = 'helper:request' ] ||
	fail 'hotplug attempted a wakeup after request publication failed'
: >"$HOTPLUG_TRACE"
ACTION=ifup INTERFACE=wan run_hotplug_request
[ ! -s "$HOTPLUG_TRACE" ] || fail 'unrelated interface hotplug published LAN work'
: >"$HOTPLUG_TRACE"
ACTION=remove run_hotplug_request
ACTION=update run_hotplug_request
[ ! -s "$HOTPLUG_TRACE" ] || fail 'irrelevant net hotplug action published LAN work'
: >"$HOTPLUG_TRACE"
ACTION=ifupdate INTERFACE=lan run_hotplug_request || fail 'LAN ifupdate did not publish durable work'
[ "$(grep -c '^helper:request$' "$HOTPLUG_TRACE")" -eq 1 ] ||
	fail 'LAN ifupdate did not publish exactly one request'

# Restore the original fixture for the remainder of the broad legacy matrix.
for port in lan1 lan2 lan3 lan4 lan5 phy0-ap0; do
	printf '0\n' >"$SYS_CLASS_NET/$port/brport/isolated"
done
: >"$TRACE"

if [ "${PROXYPOOL_TEST_FOCUS_DURABLE_L2:-0}" = 1 ]; then
	echo 'PASS: focused durable LAN convergence state'
	exit 0
fi

: >"$TRACE"
if PROXYPOOL_TEST_MODE=configure run_helper PROXYPOOL_TEST_UID=1000 >/dev/null 2>&1; then
	fail 'non-root LAN isolation helper invocation was accepted'
fi
[ ! -s "$TRACE" ] || fail 'non-root LAN isolation helper invocation caused a mutation'

: >"$TRACE"
if PROXYPOOL_TEST_MODE=configure run_helper \
	PROXYPOOL_TEST_STAT_LINK_PATH="$CONFIG_DIR/wireless" >/dev/null 2>&1; then
	fail 'hard-linked live wireless config was accepted'
fi
grep -Fxq 'wifi:down' "$TRACE" || fail 'hard-linked live wireless config left Wi-Fi running'
grep -Fxq 'wifi:probe' "$TRACE" || fail 'hard-linked live wireless config did not prove Wi-Fi down'
[ -d "$REBOOT_MARKER" ] || fail 'hard-linked live wireless config lost its reboot marker'
rm -rf "$REBOOT_MARKER"
: >"$TRACE"

# Even during early boot, a changed persistent wireless policy never continues
# into the same boot.  It installs with Wi-Fi down and retains a marker until a
# second cold boot proves the now-unchanged file before services can continue.
if PROXYPOOL_TEST_MODE=boot run_helper >/dev/null 2>&1; then
	fail 'changed cold-boot Wi-Fi policy continued without a reboot boundary'
fi
for section in ap_lan anonymous0; do
	[ "$(wireless_value "$section.isolate")" = 1 ] || fail "$section lacks station isolation"
	[ "$(wireless_value "$section.bridge_isolate")" = 1 ] || fail "$section lacks bridge-port isolation"
done
if grep -Eq 'ap_guest|sta_lan' "$TRACE"; then
	fail 'non-LAN AP or station section was modified'
fi
[ "$(grep -c '^uci:commit:wireless$' "$TRACE")" -eq 1 ] || fail 'wireless changes were not committed exactly once'
[ "$(grep -c '^wifi:down$' "$TRACE")" -eq 1 ] || fail 'cold-boot wireless install did not stop Wi-Fi exactly once'
[ "$(grep -c '^wifi:probe$' "$TRACE")" -eq 1 ] || fail 'cold-boot wireless install did not prove Wi-Fi down'
[ "$(grep -c '^wireless:install$' "$TRACE")" -eq 1 ] || fail 'staged wireless config was not installed exactly once'
[ -d "$REBOOT_MARKER" ] && [ ! -L "$REBOOT_MARKER" ] || fail 'changed cold boot lost its reboot marker'
if grep -Eq '^wifi:(reload|up)$' "$TRACE"; then
	fail 'wireless isolation activated Wi-Fi in the staging transaction'
fi

# Simulate the tmpfs reset produced only by a true cold boot.  With the
# persistent config already converged, this pass is read-only and may proceed.
rmdir "$REBOOT_MARKER"
: >"$TRACE"
PROXYPOOL_TEST_MODE=boot run_helper || fail 'unchanged second cold boot did not pass isolation validation'
[ ! -s "$TRACE" ] || fail 'unchanged second cold boot emitted a wireless mutation'

: >"$TRACE"
PROXYPOOL_TEST_MODE=reconcile run_helper || fail 'valid LAN bridge isolation reconciliation failed'
for port in lan1 lan2 lan3 lan4 lan5 phy0-ap0; do
	[ "$(cat "$SYS_CLASS_NET/$port/brport/isolated")" = 1 ] || fail "$port was not isolated"
	grep -Fxq "bridge:isolate:$port" "$TRACE" || fail "$port was not reconciled through bridge(8)"
done

: >"$TRACE"
run_helper || fail 'idempotent reconciliation failed'
[ ! -s "$TRACE" ] || fail 'idempotent reconciliation emitted mutations'

# Pending UCI deltas are not safe to absorb into an automated commit.
: >"$TRACE"
printf 'keep-me\n' >"$DEFAULT_DELTA/user.setting"
if run_helper >/dev/null 2>&1; then
	fail 'pending wireless delta was accepted'
fi
grep -Fxq 'wifi:down' "$TRACE" || fail 'pending wireless delta did not fail closed with Wi-Fi down'
grep -Fxq 'wifi:probe' "$TRACE" || fail 'pending wireless delta did not prove Wi-Fi down'
[ -d "$REBOOT_MARKER" ] || fail 'pending wireless delta did not publish a reboot boundary'
[ "$(cat "$DEFAULT_DELTA/user.setting")" = keep-me ] || fail 'pre-existing wireless delta was damaged'
rm -f "$DEFAULT_DELTA/user.setting"
rm -rf "$REBOOT_MARKER"

# A late classification failure must occur before the first write, even when a
# concurrent user's pending delta appears at the same time.  Both mode and
# network are mandatory inputs for proving that an AP is outside LAN scope.
for getter in mode network; do
	reset_wireless_config
	rm -f "$DEFAULT_DELTA"/*
	: >"$TRACE"
	if [ "$getter" = mode ]; then
		getter_knobs="PROXYPOOL_TEST_GET_FAIL=wireless.@wifi-iface[0].$getter PROXYPOOL_TEST_INJECT_USER_DELTA=1"
	else
		getter_knobs="PROXYPOOL_TEST_GET_FAIL=wireless.@wifi-iface[0].$getter"
	fi
	# shellcheck disable=SC2086 -- split the controlled key=value fixture list.
	if run_helper $getter_knobs >/dev/null 2>&1; then
		fail "late wireless $getter classification failure was accepted"
	fi
	if grep -Fq 'uci:set:' "$TRACE"; then
		fail "wireless was mutated before every section $getter was classified"
	fi
	if [ "$getter" = mode ]; then
		[ "$(cat "$DEFAULT_DELTA/user.setting")" = keep-me ] ||
			fail 'concurrent user delta was damaged after mode getter failure'
	fi
	grep -Fxq 'wifi:down' "$TRACE" || fail "wireless $getter classification failure left Wi-Fi running"
	[ -d "$REBOOT_MARKER" ] || fail "wireless $getter classification failure lost its reboot marker"
	rm -rf "$REBOOT_MARKER"
done
rm -f "$DEFAULT_DELTA/user.setting"

assert_failed_transaction_clean() {
	label=$1
	for option_file in ap_lan.isolate ap_lan.bridge_isolate anonymous0.isolate anonymous0.bridge_isolate; do
		[ ! -f "$DEFAULT_DELTA/$option_file" ] || fail "$label left a helper delta in the default UCI savedir"
	done
	[ "$(cat "$DEFAULT_DELTA/user.setting" 2>/dev/null)" = keep-me ] ||
		fail "$label removed a concurrent user delta"
	if find "$TEST_TMP" -maxdepth 1 -type d -name 'proxypool-wireless.*' -print | grep -q .; then
		fail "$label left a private UCI savedir"
	fi
	if find "$STAGE_ROOT" -mindepth 1 -maxdepth 1 -type d -name '.wireless-stage.*' -print | grep -q .; then
		fail "$label left a private wireless config stage"
	fi
	for option_file in ap_lan.isolate ap_lan.bridge_isolate anonymous0.isolate anonymous0.bridge_isolate; do
		[ "$(wireless_value "$option_file")" = 0 ] || fail "$label changed the live wireless config"
	done
}

# Setter and commit failures must discard only the helper's private transaction.
for failure in set commit; do
	reset_wireless_config
	rm -f "$DEFAULT_DELTA"/*
	: >"$TRACE"
	case "$failure" in
		set) knobs='PROXYPOOL_TEST_SET_FAIL=wireless.ap_lan.bridge_isolate=1 PROXYPOOL_TEST_INJECT_USER_DELTA=1' ;;
		commit) knobs='PROXYPOOL_TEST_COMMIT_RC=74 PROXYPOOL_TEST_INJECT_USER_DELTA=1' ;;
	esac
	# shellcheck disable=SC2086 -- split the controlled key=value fixture list.
	if run_helper $knobs >/dev/null 2>&1; then
		fail "wireless $failure failure was accepted"
	fi
	assert_failed_transaction_clean "wireless $failure failure"
	if grep -Fxq 'uci:revert:wireless' "$TRACE"; then
		fail "wireless $failure failure broadly reverted the package"
	fi
	grep -Fxq 'wifi:down' "$TRACE" || fail "wireless $failure failure left Wi-Fi running"
	grep -Fxq 'wifi:probe' "$TRACE" || fail "wireless $failure failure did not prove Wi-Fi down"
	[ -d "$REBOOT_MARKER" ] || fail "wireless $failure failure lost its reboot marker"
	if grep -Eq '^wifi:(reload|up)$' "$TRACE"; then
		fail "wireless $failure failure reactivated Wi-Fi"
	fi
	rm -rf "$REBOOT_MARKER"
done
rm -f "$DEFAULT_DELTA/user.setting"

# The staged helper is untrusted transaction machinery.  Replacing its output
# with a hard-link alias must be detected before the live UCI file is renamed.
reset_wireless_config
rm -f "$STAGE_LINK_STATE"
rm -rf "$REBOOT_MARKER"
: >"$TRACE"
if run_helper PROXYPOOL_TEST_STAGE_LINK=1 >/dev/null 2>&1; then
	fail 'hard-linked staged wireless output was accepted'
fi
if grep -Fq 'wireless:install' "$TRACE"; then
	fail 'hard-linked staged wireless output reached atomic install'
fi
grep -Fxq 'wifi:down' "$TRACE" || fail 'hard-linked staged output left Wi-Fi running'
[ -d "$REBOOT_MARKER" ] || fail 'hard-linked staged output lost its reboot marker'
for option_file in ap_lan.isolate ap_lan.bridge_isolate anonymous0.isolate anonymous0.bridge_isolate; do
	[ "$(wireless_value "$option_file")" = 0 ] ||
		fail 'hard-linked staged wireless output changed the live config'
done
rm -f "$STAGE_LINK_STATE"
rm -rf "$REBOOT_MARKER"

# Marker creation/validation is itself a safety boundary.  A writable parent,
# a regular-file sentinel, or a linked sentinel fails before staging or Wi-Fi.
reset_wireless_config
for reboot_layout in wide_parent regular symlink; do
	rm -rf "$REBOOT_MARKER"
	chmod 700 "$RUN_DIR"
	reboot_layout_knob=
	case "$reboot_layout" in
		wide_parent) reboot_layout_knob="PROXYPOOL_TEST_STAT_WIDE_PATH=$RUN_DIR" ;;
		regular) printf 'unsafe\n' >"$REBOOT_MARKER" ;;
		symlink)
			mkdir "$RUN_DIR/reboot-target"
			MSYS=winsymlinks:nativestrict \
				ln -s "$RUN_DIR/reboot-target" "$REBOOT_MARKER" 2>/dev/null ||
				fail 'host cannot create reboot-marker symlink fixture'
			[ -L "$REBOOT_MARKER" ] || fail 'host did not create a real reboot-marker symlink'
			;;
	esac
	: >"$TRACE"
	# shellcheck disable=SC2086 -- split the controlled optional key=value fixture.
	if run_helper $reboot_layout_knob >/dev/null 2>&1; then
		fail "$reboot_layout reboot-marker layout was accepted"
	fi
	[ ! -s "$TRACE" ] || fail "$reboot_layout reboot-marker failure caused a mutation"
	for option_file in ap_lan.isolate ap_lan.bridge_isolate anonymous0.isolate anonymous0.bridge_isolate; do
		[ "$(wireless_value "$option_file")" = 0 ] ||
			fail "$reboot_layout reboot-marker failure changed live wireless config"
	done
	if [ "$reboot_layout" = symlink ]; then
		rm -f "$REBOOT_MARKER"
		rmdir "$RUN_DIR/reboot-target"
	else
		rm -f "$REBOOT_MARKER"
	fi
done
chmod 700 "$RUN_DIR"

# A user delta that appears inside the staged helper's commit window is not
# absorbed and is not installed.  Runtime Wi-Fi is stopped first, the live
# config stays byte-for-byte unisolated, and the reboot marker remains.
reset_wireless_config
rm -rf "$REBOOT_MARKER"
: >"$TRACE"
if run_helper PROXYPOOL_TEST_INJECT_USER_DELTA_AT_COMMIT=1 >/dev/null 2>&1; then
	fail 'post-stage concurrent wireless delta was accepted'
fi
[ "$(cat "$DEFAULT_DELTA/user.setting")" = keep-me ] ||
	fail 'post-stage concurrent wireless delta was not preserved'
for option_file in ap_lan.isolate ap_lan.bridge_isolate anonymous0.isolate anonymous0.bridge_isolate; do
	[ "$(wireless_value "$option_file")" = 0 ] ||
		fail 'post-stage concurrent delta leaked the staged config into live UCI'
done
grep -Fxq 'wifi:down' "$TRACE" || fail 'concurrent delta failure did not leave Wi-Fi down'
if grep -Fq 'wireless:install' "$TRACE"; then
	fail 'concurrent delta was detected only after installing the staged config'
fi
[ -d "$REBOOT_MARKER" ] && [ ! -L "$REBOOT_MARKER" ] ||
	fail 'concurrent delta failure did not retain the cold-reboot marker'
if grep -Eq '^wifi:(reload|up)$' "$TRACE"; then
	fail 'concurrent delta failure reactivated Wi-Fi'
fi
rm -rf "$REBOOT_MARKER"
rm -f "$DEFAULT_DELTA/user.setting"

# Failure to prove Wi-Fi down or atomically install the staged file is fail-closed:
# live config is unchanged, no reload/up occurs, and the marker survives.
for runtime_failure in probe install; do
	reset_wireless_config
	rm -rf "$REBOOT_MARKER"
	: >"$TRACE"
	case "$runtime_failure" in
		probe) runtime_knob=PROXYPOOL_TEST_WIFI_PROBE_RC=76 ;;
		install) runtime_knob=PROXYPOOL_TEST_INSTALL_RC=77 ;;
	esac
	if run_helper "$runtime_knob" >/dev/null 2>&1; then
		fail "wireless $runtime_failure failure was accepted"
	fi
	for option_file in ap_lan.isolate ap_lan.bridge_isolate anonymous0.isolate anonymous0.bridge_isolate; do
		[ "$(wireless_value "$option_file")" = 0 ] ||
			fail "wireless $runtime_failure failure changed live wireless config"
	done
	[ -d "$REBOOT_MARKER" ] && [ ! -L "$REBOOT_MARKER" ] ||
		fail "wireless $runtime_failure failure lost the cold-reboot marker"
	if grep -Eq '^wifi:(reload|up)$' "$TRACE"; then
		fail "wireless $runtime_failure failure reactivated Wi-Fi"
	fi
	if [ "$runtime_failure" = probe ] && grep -Fq 'wireless:install' "$TRACE"; then
		fail 'staged wireless config was installed without proving Wi-Fi down'
	fi
	if [ "$runtime_failure" = probe ]; then
		[ "$(grep -c '^wifi:down$' "$TRACE")" -eq 5 ] ||
			fail 'unproven Wi-Fi down did not exhaust the bounded stop attempts'
		[ "$(grep -c '^wifi:probe$' "$TRACE")" -eq 5 ] ||
			fail 'unproven Wi-Fi down did not exhaust the bounded status probes'
	fi
	rm -rf "$REBOOT_MARKER"
done

# Command status is advisory: a non-zero `wifi down` is accepted when the
# independent state proof is already safe.  Conversely, transient unknown
# probes retry both down and status without ever installing early.
reset_wireless_config
: >"$TRACE"
if run_helper PROXYPOOL_TEST_WIFI_DOWN_RC=76 >/dev/null 2>&1; then
	fail 'verified wireless install crossed its deliberate reboot boundary'
fi
[ "$(grep -c '^wifi:down$' "$TRACE")" -eq 1 ] ||
	fail 'non-zero wifi status caused unnecessary shutdown retries after proof'
grep -Fxq 'wireless:install' "$TRACE" ||
	fail 'verified down state was rejected only because wifi returned non-zero'
rm -rf "$REBOOT_MARKER"

reset_wireless_config
: >"$TRACE"
if run_helper PROXYPOOL_TEST_WIFI_PROBE_FAILS=2 >/dev/null 2>&1; then
	fail 'transiently verified wireless install crossed its reboot boundary'
fi
[ "$(grep -c '^wifi:down$' "$TRACE")" -eq 3 ] || fail 'wireless down was not retried with its probe'
[ "$(grep -c '^wifi:probe$' "$TRACE")" -eq 3 ] || fail 'wireless status used the wrong retry count'
[ "$(grep -c '^wifi:sleep$' "$TRACE")" -eq 2 ] || fail 'wireless proof retry backoff was not bounded'
grep -Fxq 'wireless:install' "$TRACE" || fail 'wireless stage was not installed after a later safe proof'
rm -rf "$REBOOT_MARKER"

# Exercise the production ubus+jshn proof, not only the injectable test seam.
# Every radio must be fully down, not pending, and have autostart revoked; any
# hostapd object or unknown/missing field blocks the persistent transaction.
reset_wireless_config
: >"$TRACE"
if run_helper PROXYPOOL_WIRELESS_DOWN_PROBE= >/dev/null 2>&1; then
	fail 'production-probed wireless install crossed its reboot boundary'
fi
grep -Fxq 'ubus:list:all' "$TRACE" || fail 'production probe did not enumerate ubus before checking hostapd objects'
if grep -Fxq 'ubus:list:hostapd-pattern' "$TRACE"; then
	fail 'production probe used a wildcard lookup that reports no hostapd objects as Not found'
fi
grep -Fxq 'ubus:wireless:status' "$TRACE" || fail 'production probe skipped netifd radio status'
grep -Fxq 'wireless:install' "$TRACE" || fail 'safe production wireless proof was rejected'
rm -rf "$REBOOT_MARKER"

for unsafe_probe in hostapd list_failure up pending autostart missing_up string_up object_absent_runtime; do
	reset_wireless_config
	: >"$TRACE"
	case "$unsafe_probe" in
		hostapd) probe_knob='PROXYPOOL_TEST_HOSTAPD_OBJECTS=hostapd.phy0-ap0' ;;
		list_failure) probe_knob=PROXYPOOL_TEST_UBUS_LIST_RC=7 ;;
		up) probe_knob=PROXYPOOL_TEST_RADIO_UP=1 ;;
		pending) probe_knob=PROXYPOOL_TEST_RADIO_PENDING=1 ;;
		autostart) probe_knob=PROXYPOOL_TEST_RADIO_AUTOSTART=1 ;;
		missing_up) probe_knob=PROXYPOOL_TEST_RADIO_MISSING=up ;;
		string_up) probe_knob=PROXYPOOL_TEST_RADIO_STRING_FIELD=up ;;
		object_absent_runtime) probe_knob=PROXYPOOL_TEST_UBUS_STATUS_RC=4 ;;
	esac
	# shellcheck disable=SC2086 -- controlled key=value fixture.
	if run_helper PROXYPOOL_WIRELESS_DOWN_PROBE= $probe_knob >/dev/null 2>&1; then
		fail "$unsafe_probe production wireless state was accepted"
	fi
	if grep -Fq 'wireless:install' "$TRACE"; then
		fail "$unsafe_probe production wireless state reached persistent install"
	fi
	[ "$(grep -c '^wifi:down$' "$TRACE")" -eq 5 ] ||
		fail "$unsafe_probe production wireless state skipped bounded shutdown retries"
	[ -d "$REBOOT_MARKER" ] || fail "$unsafe_probe production wireless state lost its marker"
	rm -rf "$REBOOT_MARKER"
done

# At S10/S18 only, ubusd can prove no hostapd objects while netifd's
# network.wireless object is not registered yet.  This narrow cold-start case
# must converge the file; the identical runtime configure case above is denied.
reset_wireless_config
: >"$TRACE"
if PROXYPOOL_TEST_MODE=boot run_helper PROXYPOOL_WIRELESS_DOWN_PROBE= \
	PROXYPOOL_TEST_UBUS_STATUS_RC=4 >/dev/null 2>&1; then
	fail 'cold object-absent wireless install crossed its reboot boundary'
fi
grep -Fxq 'wireless:install' "$TRACE" || fail 'cold object-absent proof could not converge wireless policy'
rm -rf "$REBOOT_MARKER"

# A successful runtime install deliberately returns failure until a cold boot.
# Re-entry with the marker cannot stage, stop, install, or reconcile anything.
reset_wireless_config
: >"$TRACE"
if run_helper >/dev/null 2>&1; then
	fail 'runtime wireless policy change did not require a cold reboot'
fi
grep -Fxq 'wifi:down' "$TRACE" || fail 'runtime wireless install did not stop Wi-Fi'
grep -Fxq 'wireless:install' "$TRACE" || fail 'runtime wireless policy was not atomically installed'
[ -d "$REBOOT_MARKER" ] && [ ! -L "$REBOOT_MARKER" ] ||
	fail 'runtime wireless install did not retain its reboot marker'
for option_file in ap_lan.isolate ap_lan.bridge_isolate anonymous0.isolate anonymous0.bridge_isolate; do
	[ "$(wireless_value "$option_file")" = 1 ] ||
		fail 'runtime wireless install did not persist the staged isolation policy'
done
if grep -Eq '^wifi:(reload|up)$|^bridge:isolate:' "$TRACE"; then
	fail 'runtime wireless install activated Wi-Fi or continued past its reboot boundary'
fi
: >"$TRACE"
if run_helper >/dev/null 2>&1; then
	fail 'reboot-required marker allowed a second runtime apply'
fi
grep -Fxq 'wifi:down' "$TRACE" || fail 'reboot-required re-entry did not force Wi-Fi down again'
grep -Fxq 'wifi:probe' "$TRACE" || fail 'reboot-required re-entry did not independently prove Wi-Fi down'
if grep -Eq '^wireless:install$|^bridge:isolate:' "$TRACE"; then
	fail 'reboot-required re-entry continued after re-proving Wi-Fi down'
fi

# Simulate the /var/run reset of a real cold boot.  The already-installed
# wireless file is unchanged, so boot performs only read-only validation.
rm -rf "$REBOOT_MARKER"
: >"$TRACE"
PROXYPOOL_TEST_MODE=boot run_helper || fail 'cold boot did not clear the runtime install boundary'
[ ! -s "$TRACE" ] || fail 'unchanged cold boot emitted a wireless mutation'

# A missing br-lan runtime topology is "not ready", never successful.
rm -rf "$SYS_CLASS_NET/br-lan/brif"
: >"$TRACE"
if PROXYPOOL_TEST_MODE=reconcile run_helper >/dev/null 2>&1; then
	fail 'missing br-lan topology was reported as reconciled'
fi
if grep -Fq 'bridge:isolate:' "$TRACE"; then
	fail 'missing br-lan topology caused a bridge mutation'
fi

# S18 runs before netifd creates br-lan.  Boot persists wireless intent and a
# pending marker, while strict runtime proof is deferred to the procd worker.
: >"$TRACE"
PROXYPOOL_TEST_MODE=boot run_helper ||
	fail 'pre-netifd boot could not persist wireless isolation intent'
if grep -Fq 'bridge:isolate:' "$TRACE"; then
	fail 'pre-netifd boot attempted a nonexistent bridge mutation'
fi
for section in ap_lan anonymous0; do
	[ "$(wireless_value "$section.isolate")" = 1 ] || fail "$section boot isolate intent did not persist"
	[ "$(wireless_value "$section.bridge_isolate")" = 1 ] || fail "$section boot bridge-isolate intent did not persist"
done
mkdir -p "$SYS_CLASS_NET/br-lan/brif"
for port in lan1 lan2 lan3 lan4 lan5 phy0-ap0; do
	mkdir "$SYS_CLASS_NET/br-lan/brif/$port"
done

# Expected static members come from network's br-lan device and must all be
# present before any actual member is modified.
rm -rf "$SYS_CLASS_NET/br-lan/brif/lan2"
for port in lan1 lan2 lan3 lan4 lan5 phy0-ap0; do
	printf '0\n' >"$SYS_CLASS_NET/$port/brport/isolated"
done
: >"$TRACE"
if PROXYPOOL_TEST_MODE=reconcile run_helper >/dev/null 2>&1; then
	fail 'incomplete configured br-lan membership was accepted'
fi
if grep -Fq 'bridge:isolate:' "$TRACE"; then
	fail 'incomplete br-lan membership was mutated before topology validation'
fi
mkdir "$SYS_CLASS_NET/br-lan/brif/lan2"
for port in lan1 lan2 lan3 lan4 lan5 phy0-ap0; do
	printf '1\n' >"$SYS_CLASS_NET/$port/brport/isolated"
done

# Ambiguous, empty, or pending network truth is never a usable expected-member
# source and must fail before touching a bridge port.
for network_failure in pending duplicate empty; do
	: >"$TRACE"
	case "$network_failure" in
		pending) network_knob=PROXYPOOL_TEST_NETWORK_PENDING=1 ;;
		duplicate) network_knob=PROXYPOOL_TEST_NETWORK_VARIANT=duplicate ;;
		empty) network_knob=PROXYPOOL_TEST_NETWORK_VARIANT=empty ;;
	esac
	if PROXYPOOL_TEST_MODE=reconcile run_helper "$network_knob" >/dev/null 2>&1; then
		fail "$network_failure network bridge truth was accepted"
	fi
	if grep -Fq 'bridge:isolate:' "$TRACE"; then
		fail "$network_failure network bridge truth caused a bridge mutation"
	fi
done

# This image targets one exact board topology.  Dynamic enumeration must not
# turn a missing LAN5 or a sixth physical client port into an accepted safety
# baseline, even if every currently visible bridge member is isolated.
for topology_failure in missing_lan5 extra_lan6; do
	: >"$TRACE"
	case "$topology_failure" in
		missing_lan5)
			expected_ports='lan1 lan2 lan3 lan4'
			;;
		extra_lan6)
			expected_ports='lan1 lan2 lan3 lan4 lan5 lan6'
			mkdir -p "$SYS_CLASS_NET/lan6/brport" "$SYS_CLASS_NET/br-lan/brif/lan6"
			printf '1\n' >"$SYS_CLASS_NET/lan6/brport/isolated"
			;;
	esac
	if PROXYPOOL_TEST_MODE=reconcile run_helper \
		PROXYPOOL_TEST_EXPECTED_PORTS="$expected_ports" >/dev/null 2>&1; then
		fail "$topology_failure configured physical-port set was accepted"
	fi
	if grep -Fq 'bridge:isolate:' "$TRACE"; then
		fail "$topology_failure physical-port set caused a bridge mutation"
	fi
	if [ "$topology_failure" = extra_lan6 ]; then
		rm -rf "$SYS_CLASS_NET/br-lan/brif/lan6" "$SYS_CLASS_NET/lan6"
	fi
done

# A delta created after the first pending check but during the multi-read UCI
# discovery must be caught by the second check before sysfs is mutated.
rm -f "$NETWORK_PENDING_FILE"
printf '0\n' >"$SYS_CLASS_NET/lan1/brport/isolated"
: >"$TRACE"
if PROXYPOOL_TEST_MODE=reconcile run_helper \
	PROXYPOOL_TEST_NETWORK_INJECT_PENDING_AT='network.@device[0].ports' >/dev/null 2>&1; then
	fail 'mid-discovery network delta was accepted'
fi
[ "$(cat "$SYS_CLASS_NET/lan1/brport/isolated")" = 0 ] ||
	fail 'mid-discovery network delta allowed a bridge mutation'
if grep -Fq 'bridge:isolate:' "$TRACE"; then
	fail 'mid-discovery network delta reached bridge(8)'
fi
[ -f "$NETWORK_PENDING_FILE" ] || fail 'network race fixture did not inject its delta'
rm -f "$NETWORK_PENDING_FILE"
printf '1\n' >"$SYS_CLASS_NET/lan1/brport/isolated"

# Validate every actual member's sysfs proof before mutating the first member.
for port in lan1 lan2 lan3 lan4 lan5 phy0-ap0; do
	printf '0\n' >"$SYS_CLASS_NET/$port/brport/isolated"
done
rm -f "$SYS_CLASS_NET/phy0-ap0/brport/isolated"
: >"$TRACE"
if PROXYPOOL_TEST_MODE=reconcile run_helper >/dev/null 2>&1; then
	fail 'unreadable actual br-lan member state was accepted'
fi
if grep -Fq 'bridge:isolate:' "$TRACE"; then
	fail 'bridge member was mutated before all actual members were validated'
fi
[ "$(cat "$SYS_CLASS_NET/lan1/brport/isolated")" = 0 ] ||
	fail 'preflight failure partially isolated an earlier member'
printf '0\n' >"$SYS_CLASS_NET/phy0-ap0/brport/isolated"
for port in lan1 lan2 lan3 lan4 lan5 phy0-ap0; do
	printf '1\n' >"$SYS_CLASS_NET/$port/brport/isolated"
done

# Sysfs attestation is exact bytes, not merely a valid first line.  A forged
# or malformed value with an unterminated suffix must never pass reconciliation.
printf '1\njunk' >"$SYS_CLASS_NET/phy0-ap0/brport/isolated"
: >"$TRACE"
if PROXYPOOL_TEST_MODE=reconcile run_helper >/dev/null 2>&1; then
	fail 'bridge isolation state with trailing bytes was accepted'
fi
if grep -Fq 'bridge:isolate:' "$TRACE"; then
	fail 'malformed bridge isolation proof caused a mutation'
fi
printf '1\n' >"$SYS_CLASS_NET/phy0-ap0/brport/isolated"

# Runtime mutation and postcondition failures must be visible to the caller.
for failure in command no_effect; do
	printf '0\n' >"$SYS_CLASS_NET/lan2/brport/isolated"
	case "$failure" in
		command) knob=PROXYPOOL_TEST_BRIDGE_FAIL=lan2 ;;
		no_effect) knob=PROXYPOOL_TEST_BRIDGE_NO_EFFECT=lan2 ;;
	esac
	if PROXYPOOL_TEST_MODE=reconcile run_helper "$knob" >/dev/null 2>&1; then
		fail "bridge $failure failure was accepted"
	fi
done
printf '1\n' >"$SYS_CLASS_NET/lan2/brport/isolated"

echo 'PASS: LAN/Wi-Fi software isolation defaults and reconciliation'
