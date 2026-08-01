#!/bin/sh
set -eu

UCI="${PROXYPOOL_UCI:-/sbin/uci}"
UCODE="${PROXYPOOL_UCODE:-/usr/bin/ucode}"
UCI_STAGED_HELPER="${PROXYPOOL_UCI_STAGED_HELPER:-/usr/lib/proxypool/proxypool-uci-staged.uc}"
UCI_STAGED_APPLY="${PROXYPOOL_UCI_STAGED_APPLY:-}"
CONFIG_DIR="${PROXYPOOL_CONFIG_DIR:-/etc/config}"
WIRELESS_STAGE_ROOT="${PROXYPOOL_WIRELESS_STAGE_ROOT:-/etc/proxypool}"
REBOOT_REQUIRED_MARKER="${PROXYPOOL_WIRELESS_REBOOT_MARKER:-/var/run/proxypool-wireless-reboot-required}"
WIRELESS_QUARANTINE_ROOT="${PROXYPOOL_WIRELESS_QUARANTINE_ROOT:-$WIRELESS_STAGE_ROOT/wireless-quarantine}"
LAN_STATE_ROOT="${PROXYPOOL_LAN_STATE_ROOT:-/var/run/proxypool-lan-isolation}"
WIRELESS_INSTALL="${PROXYPOOL_WIRELESS_INSTALL:-}"
STAT="${PROXYPOOL_STAT:-/bin/stat}"
ID="${PROXYPOOL_ID:-/usr/bin/id}"
CMP="${PROXYPOOL_CMP:-/usr/bin/cmp}"
BRIDGE="${PROXYPOOL_BRIDGE:-/usr/sbin/bridge}"
WIFI="${PROXYPOOL_WIFI:-/sbin/wifi}"
WIRELESS_DOWN_PROBE="${PROXYPOOL_WIRELESS_DOWN_PROBE:-}"
UBUS="${PROXYPOOL_UBUS:-/bin/ubus}"
JSHN="${PROXYPOOL_JSHN:-/usr/share/libubox/jshn.sh}"
SLEEP="${PROXYPOOL_SLEEP:-/bin/sleep}"
SYNC="${PROXYPOOL_SYNC:-/bin/sync}"
SYS_CLASS_NET="${PROXYPOOL_SYS_CLASS_NET:-/sys/class/net}"
CPU_ONLY_MARKER="${PROXYPOOL_CPU_ONLY_MARKER:-/sys/module/mt7530/parameters/proxypool_cpu_only_bridge}"
TMP_ROOT="${PROXYPOOL_TMPDIR:-/tmp}"
MODE=${1:-apply}
WIRELESS_CHANGED=0
WIRELESS_DELTA_DIR=
WIRELESS_STAGE_DIR=
WIRELESS_QUARANTINE_REPAIR=0
WIRELESS_QUARANTINE_SAFE_DISABLED=0
PHASE1_LAN_PORTS='lan1 lan2 lan3 lan4 lan5'

fail() {
	echo "ProxyPool LAN isolation: $*" >&2
	exit 1
}

case "$MODE" in
	boot|configure|request|readiness|verify|reconcile|apply) : ;;
	*) fail "unsupported mode: $MODE" ;;
esac

case "$SYS_CLASS_NET" in
	/*) : ;;
	*) fail "unsafe sysfs root: $SYS_CLASS_NET" ;;
esac
[ "$SYS_CLASS_NET" != / ] || fail 'refusing filesystem-root sysfs path'
[ -x "$ID" ] || fail "id is not executable: $ID"
HELPER_UID=$("$ID" -u 2>/dev/null) || fail 'cannot determine LAN isolation helper uid'
[ "$HELPER_UID" = 0 ] || fail 'LAN isolation helper must run as root'

validate_cpu_only_bridge() {
	case "$CPU_ONLY_MARKER" in
		/*) : ;;
		*) fail "unsafe CPU-only bridge marker path: $CPU_ONLY_MARKER" ;;
	esac
	[ "$CPU_ONLY_MARKER" != / ] || fail 'refusing filesystem-root CPU-only marker path'
	[ -f "$CPU_ONLY_MARKER" ] && [ ! -L "$CPU_ONLY_MARKER" ] ||
		fail 'cannot prove the custom-kernel CPU-only bridge marker'
	[ "$(wc -c <"$CPU_ONLY_MARKER" | tr -d '[:space:]')" -eq 2 ] ||
		fail 'malformed custom-kernel CPU-only bridge marker'
	IFS= read -r marker_value <"$CPU_ONLY_MARKER" ||
		fail 'cannot read the custom-kernel CPU-only bridge marker'
	[ "$marker_value" = Y ] || fail 'custom kernel did not force the LAN bridge through the CPU'
}

validate_cpu_only_bridge

cleanup_wireless_delta() {
	[ -n "$WIRELESS_DELTA_DIR" ] || return 0
	case "$WIRELESS_DELTA_DIR" in
		"$TMP_ROOT"/proxypool-wireless.*) : ;;
		*) return 1 ;;
	esac
	[ -d "$WIRELESS_DELTA_DIR" ] && [ ! -L "$WIRELESS_DELTA_DIR" ] || return 1
	for delta_entry in "$WIRELESS_DELTA_DIR"/*; do
		[ -e "$delta_entry" ] || [ -L "$delta_entry" ] || continue
		[ -f "$delta_entry" ] && [ ! -L "$delta_entry" ] || return 1
		rm -f "$delta_entry" || return 1
	done
	rmdir "$WIRELESS_DELTA_DIR" || return 1
	WIRELESS_DELTA_DIR=
}

cleanup_wireless_stage() {
	[ -n "$WIRELESS_STAGE_DIR" ] || return 0
	case "$WIRELESS_STAGE_DIR" in
		"$WIRELESS_STAGE_ROOT"/.wireless-stage.*) : ;;
		*) return 1 ;;
	esac
	[ -d "$WIRELESS_STAGE_DIR" ] && [ ! -L "$WIRELESS_STAGE_DIR" ] || return 1
	for stage_entry in "$WIRELESS_STAGE_DIR"/* "$WIRELESS_STAGE_DIR"/.[!.]* "$WIRELESS_STAGE_DIR"/..?*; do
		[ -e "$stage_entry" ] || [ -L "$stage_entry" ] || continue
		[ -f "$stage_entry" ] && [ ! -L "$stage_entry" ] || return 1
		rm -f "$stage_entry" || return 1
	done
	rmdir "$WIRELESS_STAGE_DIR" || return 1
	WIRELESS_STAGE_DIR=
}

trap 'cleanup_wireless_stage || :; cleanup_wireless_delta || :' EXIT

path_metadata() {
	[ -x "$STAT" ] || return 1
	"$STAT" -c '%a %u %h %d' "$1" 2>/dev/null
}

current_uid() {
	printf '%s\n' "$HELPER_UID"
}

secure_private_directory() {
	path=$1
	[ -d "$path" ] && [ ! -L "$path" ] || return 1
	metadata=$(path_metadata "$path") || return 1
	mode=${metadata%% *}
	rest=${metadata#* }
	owner=${rest%% *}
	uid=$(current_uid) || return 1
	[ "$mode" = 700 ] && [ "$owner" = "$uid" ]
}

validate_lan_state_root_path() {
	case "$LAN_STATE_ROOT" in
		/*) : ;;
		*) fail "unsafe LAN convergence state root: $LAN_STATE_ROOT" ;;
	esac
	[ "$LAN_STATE_ROOT" != / ] || fail 'refusing filesystem-root LAN convergence state path'
	state_parent=${LAN_STATE_ROOT%/*}
	[ -n "$state_parent" ] || state_parent=/
	secure_parent_directory "$state_parent" ||
		fail "LAN convergence state parent is not secure: $state_parent"
}

marker_path_present() {
	[ -e "$1" ] || [ -L "$1" ]
}

secure_empty_marker_directory() {
	marker=$1
	secure_private_directory "$marker" || return 1
	for marker_entry in "$marker"/* "$marker"/.[!.]* "$marker"/..?*; do
		[ ! -e "$marker_entry" ] && [ ! -L "$marker_entry" ] || return 1
	done
	return 0
}

validate_lan_state_root() {
	validate_lan_state_root_path
	secure_private_directory "$LAN_STATE_ROOT" ||
		fail 'LAN convergence state root is linked, malformed, or not root-only'
}

ensure_lan_state_root() {
	validate_lan_state_root_path
	if ! marker_path_present "$LAN_STATE_ROOT"; then
		umask 077
		mkdir -m 700 "$LAN_STATE_ROOT" 2>/dev/null ||
			marker_path_present "$LAN_STATE_ROOT" ||
			fail 'cannot create the LAN convergence state root'
	fi
	validate_lan_state_root
}

pending_marker_path() {
	printf '%s\n' "$LAN_STATE_ROOT/pending"
}

inflight_marker_path() {
	printf '%s\n' "$LAN_STATE_ROOT/inflight"
}

validate_lan_marker_if_present() {
	marker=$1
	marker_name=$2
	marker_path_present "$marker" || return 0
	secure_empty_marker_directory "$marker" ||
		fail "$marker_name LAN convergence marker is linked, malformed, nonempty, or not root-only"
}

request_lan_reconciliation() {
	ensure_lan_state_root
	pending=$(pending_marker_path)
	inflight=$(inflight_marker_path)
	validate_lan_marker_if_present "$inflight" in-flight
	if marker_path_present "$pending"; then
		validate_lan_marker_if_present "$pending" pending
		return 0
	fi
	umask 077
	mkdir -m 700 "$pending" 2>/dev/null ||
		marker_path_present "$pending" ||
		fail 'cannot publish the pending LAN convergence marker'
	validate_lan_marker_if_present "$pending" pending
}

lan_reconciliation_ready() {
	validate_lan_state_root
	pending=$(pending_marker_path)
	inflight=$(inflight_marker_path)
	validate_lan_marker_if_present "$pending" pending
	validate_lan_marker_if_present "$inflight" in-flight
	marker_path_present "$pending" && return 1
	marker_path_present "$inflight" && return 1
	validate_reboot_marker_path
	if marker_exists; then
		validate_existing_reboot_marker
		return 1
	fi
	if quarantine_path_present "$WIRELESS_QUARANTINE_ROOT"; then
		validate_wireless_quarantine_root ||
			fail 'wireless quarantine root is linked, malformed, or not root-only'
		read_wireless_quarantine_state >/dev/null ||
			fail 'wireless quarantine state is missing or malformed'
		return 1
	fi
	return 0
}

claim_lan_reconciliation() {
	ensure_lan_state_root
	pending=$(pending_marker_path)
	inflight=$(inflight_marker_path)
	if marker_path_present "$inflight"; then
		validate_lan_marker_if_present "$inflight" in-flight
		validate_lan_marker_if_present "$pending" pending
		return 0
	fi
	request_lan_reconciliation
	validate_lan_marker_if_present "$pending" pending
	mv -T "$pending" "$inflight" || fail 'cannot claim pending LAN convergence work'
	validate_lan_marker_if_present "$inflight" in-flight
}

complete_lan_reconciliation() {
	inflight=$(inflight_marker_path)
	pending=$(pending_marker_path)
	validate_lan_state_root
	validate_lan_marker_if_present "$inflight" in-flight
	marker_path_present "$inflight" || fail 'LAN convergence lost its in-flight marker'
	rmdir "$inflight" || fail 'cannot retire the completed LAN convergence marker'
	validate_lan_marker_if_present "$pending" pending
}

secure_parent_directory() {
	path=$1
	[ -d "$path" ] && [ ! -L "$path" ] || return 1
	metadata=$(path_metadata "$path") || return 1
	mode=${metadata%% *}
	rest=${metadata#* }
	owner=${rest%% *}
	uid=$(current_uid) || return 1
	[ "$owner" = "$uid" ] || return 1
	printf '%s\n' "$mode" | grep -Eq '^[0-7]?7[0145][0145]$'
}

secure_wireless_file() {
	path=$1
	[ -f "$path" ] && [ ! -L "$path" ] || return 1
	metadata=$(path_metadata "$path") || return 1
	mode=${metadata%% *}
	rest=${metadata#* }
	owner=${rest%% *}
	rest=${rest#* }
	links=${rest%% *}
	uid=$(current_uid) || return 1
	[ "$mode" = 600 ] && [ "$owner" = "$uid" ] && [ "$links" = 1 ]
}

quarantine_path_present() {
	[ -e "$1" ] || [ -L "$1" ]
}

validate_wireless_quarantine_root_path() {
	case "$WIRELESS_QUARANTINE_ROOT" in
		/*) : ;;
		*) return 1 ;;
	esac
	[ "$WIRELESS_QUARANTINE_ROOT" != / ] || return 1
	quarantine_parent=${WIRELESS_QUARANTINE_ROOT%/*}
	[ -n "$quarantine_parent" ] || quarantine_parent=/
	secure_parent_directory "$quarantine_parent" || return 1
	parent_metadata=$(path_metadata "$quarantine_parent") || return 1
	config_metadata=$(path_metadata "$CONFIG_DIR") || return 1
	[ "${parent_metadata##* }" = "${config_metadata##* }" ]
}

validate_wireless_quarantine_root() {
	validate_wireless_quarantine_root_path || return 1
	secure_private_directory "$WIRELESS_QUARANTINE_ROOT"
}

ensure_wireless_quarantine_root() {
	validate_wireless_quarantine_root_path || return 1
	if ! quarantine_path_present "$WIRELESS_QUARANTINE_ROOT"; then
		umask 077
		mkdir -m 700 "$WIRELESS_QUARANTINE_ROOT" 2>/dev/null ||
			quarantine_path_present "$WIRELESS_QUARANTINE_ROOT" || return 1
	fi
	validate_wireless_quarantine_root
}

wireless_quarantine_state_file() {
	printf '%s\n' "$WIRELESS_QUARANTINE_ROOT/state"
}

wireless_quarantine_recovery_file() {
	printf '%s\n' "$WIRELESS_QUARANTINE_ROOT/recovery"
}

wireless_quarantine_disabled_file() {
	printf '%s\n' "$WIRELESS_QUARANTINE_ROOT/disabled"
}

read_wireless_quarantine_state() {
	state_file=$(wireless_quarantine_state_file)
	secure_wireless_file "$state_file" || return 1
	[ "$(wc -l <"$state_file" | tr -d '[:space:]')" -eq 1 ] || return 1
	IFS= read -r quarantine_state <"$state_file" || return 1
	case "$quarantine_state" in
		PREPARING|DISABLED) printf '%s\n' "$quarantine_state" ;;
		*) return 1 ;;
	esac
}

sync_wireless_quarantine() {
	[ -x "$SYNC" ] || return 1
	"$SYNC"
}

publish_wireless_quarantine_state() {
	new_state=$1
	case "$new_state" in PREPARING|DISABLED) : ;; *) return 1 ;; esac
	ensure_wireless_quarantine_root || return 1
	state_target=$(wireless_quarantine_state_file)
	if quarantine_path_present "$state_target"; then
		secure_wireless_file "$state_target" || return 1
		read_wireless_quarantine_state >/dev/null || return 1
	fi
	state_tmp=$(mktemp "$WIRELESS_QUARANTINE_ROOT/.state.XXXXXX") || return 1
	case "$state_tmp" in
		"$WIRELESS_QUARANTINE_ROOT"/.state.*) : ;;
		*) return 1 ;;
	esac
	chmod 600 "$state_tmp" || return 1
	printf '%s\n' "$new_state" >"$state_tmp" || return 1
	secure_wireless_file "$state_tmp" || return 1
	sync_wireless_quarantine || return 1
	mv -fT "$state_tmp" "$state_target" || return 1
	secure_wireless_file "$state_target" || return 1
	sync_wireless_quarantine
}

publish_wireless_quarantine_copy() {
	source_file=$1
	target_file=$2
	replace=${3:-0}
	secure_wireless_file "$source_file" || return 1
	if quarantine_path_present "$target_file"; then
		secure_wireless_file "$target_file" || return 1
		[ "$replace" -eq 1 ] || return 0
	fi
	copy_tmp=$(mktemp "$WIRELESS_QUARANTINE_ROOT/.copy.XXXXXX") || return 1
	case "$copy_tmp" in
		"$WIRELESS_QUARANTINE_ROOT"/.copy.*) : ;;
		*) return 1 ;;
	esac
	cp -p "$source_file" "$copy_tmp" || return 1
	chmod 600 "$copy_tmp" || return 1
	secure_wireless_file "$copy_tmp" || return 1
	sync_wireless_quarantine || return 1
	mv -fT "$copy_tmp" "$target_file" || return 1
	secure_wireless_file "$target_file" || return 1
	sync_wireless_quarantine
}

run_quarantine_ucode_action() {
	quarantine_action=$1
	quarantine_stage=$2
	quarantine_delta=$3
	if [ -n "$UCI_STAGED_APPLY" ]; then
		[ -x "$UCI_STAGED_APPLY" ] || return 1
		"$UCI_STAGED_APPLY" "$quarantine_action" "$quarantine_stage" "$quarantine_delta"
		return
	fi
	[ -x "$UCODE" ] || return 1
	case "$UCI_STAGED_HELPER" in /*) : ;; *) return 1 ;; esac
	[ "$UCI_STAGED_HELPER" != / ] || return 1
	[ -f "$UCI_STAGED_HELPER" ] && [ ! -L "$UCI_STAGED_HELPER" ] || return 1
	"$UCODE" "$UCI_STAGED_HELPER" "$quarantine_action" "$quarantine_stage" "$quarantine_delta"
}

wireless_file_is_explicitly_disabled() (
	wireless_file=$1
	secure_wireless_file "$wireless_file" || return 1
	[ -s "$wireless_file" ] || return 0
	verify_dir=
	cleanup_verify_dir() {
		[ -n "$verify_dir" ] || return 0
		case "$verify_dir" in
			"$WIRELESS_QUARANTINE_ROOT"/.verify.*) : ;;
			*) return 1 ;;
		esac
		[ -d "$verify_dir" ] && [ ! -L "$verify_dir" ] || return 1
		[ ! -e "$verify_dir/wireless" ] || rm -f "$verify_dir/wireless" || return 1
		[ ! -e "$verify_dir/delta" ] || rmdir "$verify_dir/delta" || return 1
		rmdir "$verify_dir"
	}
	trap 'cleanup_verify_dir || :' EXIT
	verify_dir=$(mktemp -d "$WIRELESS_QUARANTINE_ROOT/.verify.XXXXXX") || return 1
	case "$verify_dir" in
		"$WIRELESS_QUARANTINE_ROOT"/.verify.*) : ;;
		*) return 1 ;;
	esac
	chmod 700 "$verify_dir" || return 1
	mkdir -m 700 "$verify_dir/delta" || return 1
	cp -p "$wireless_file" "$verify_dir/wireless" || return 1
	chmod 600 "$verify_dir/wireless" || return 1
	verify_result=$(run_quarantine_ucode_action verify-all-wireless-disabled \
		"$verify_dir" "$verify_dir/delta" 2>/dev/null) || return 1
	[ "$verify_result" = disabled ] || return 1
	cleanup_verify_dir || return 1
	verify_dir=
)

cleanup_wireless_quarantine_transaction() {
	transaction_dir=$1
	case "$transaction_dir" in
		"$WIRELESS_QUARANTINE_ROOT"/.transaction.*) : ;;
		*) return 1 ;;
	esac
	[ -d "$transaction_dir" ] && [ ! -L "$transaction_dir" ] || return 1
	for transaction_file in "$transaction_dir"/wireless "$transaction_dir"/delta/*; do
		[ -e "$transaction_file" ] || [ -L "$transaction_file" ] || continue
		[ -f "$transaction_file" ] && [ ! -L "$transaction_file" ] || return 1
		rm -f "$transaction_file" || return 1
	done
	[ ! -e "$transaction_dir/delta" ] || rmdir "$transaction_dir/delta" || return 1
	rmdir "$transaction_dir"
}

cleanup_stale_wireless_verify_dir() {
	stale_verify_dir=$1
	case "$stale_verify_dir" in
		"$WIRELESS_QUARANTINE_ROOT"/.verify.*) : ;;
		*) return 1 ;;
	esac
	secure_private_directory "$stale_verify_dir" || return 1
	for verify_entry in "$stale_verify_dir"/*; do
		[ -e "$verify_entry" ] || [ -L "$verify_entry" ] || continue
		case "${verify_entry##*/}" in
			wireless)
				secure_wireless_file "$verify_entry" || return 1
				rm -f "$verify_entry" || return 1
				;;
			delta)
				secure_private_directory "$verify_entry" || return 1
				for delta_entry in "$verify_entry"/*; do
					[ ! -e "$delta_entry" ] && [ ! -L "$delta_entry" ] || return 1
				done
				rmdir "$verify_entry" || return 1
				;;
			*) return 1 ;;
		esac
	done
	rmdir "$stale_verify_dir"
}

cleanup_stale_wireless_quarantine_artifacts() {
	validate_wireless_quarantine_root || return 1
	for quarantine_entry in \
		"$WIRELESS_QUARANTINE_ROOT"/* \
		"$WIRELESS_QUARANTINE_ROOT"/.[!.]* \
		"$WIRELESS_QUARANTINE_ROOT"/..?*; do
		[ -e "$quarantine_entry" ] || [ -L "$quarantine_entry" ] || continue
		entry_name=${quarantine_entry##*/}
		case "$entry_name" in
			state|recovery|disabled) : ;;
			.state.*|.copy.*)
				secure_wireless_file "$quarantine_entry" || return 1
				rm -f "$quarantine_entry" || return 1
				;;
			.transaction.*)
				cleanup_wireless_quarantine_transaction "$quarantine_entry" || return 1
				;;
			.verify.*)
				cleanup_stale_wireless_verify_dir "$quarantine_entry" || return 1
				;;
			*) return 1 ;;
		esac
	done
}

install_quarantined_wireless() {
	candidate=$1
	secure_wireless_file "$CONFIG_DIR/wireless" || return 1
	secure_wireless_file "$candidate" || return 1
	if [ -n "$WIRELESS_INSTALL" ]; then
		[ -x "$WIRELESS_INSTALL" ] || return 1
		"$WIRELESS_INSTALL" "$candidate" "$CONFIG_DIR/wireless" || return 1
	else
		mv -fT "$candidate" "$CONFIG_DIR/wireless" || return 1
	fi
	secure_wireless_file "$CONFIG_DIR/wireless" || return 1
	sync_wireless_quarantine
}

quarantine_wireless_at_boot() {
	# This function is deliberately status-only: the caller must still publish
	# the volatile reboot boundary and independently prove runtime shutdown.
	ensure_wireless_quarantine_root || return 1
	cleanup_stale_wireless_quarantine_artifacts || return 1
	publish_wireless_quarantine_state PREPARING || return 1
	secure_wireless_file "$CONFIG_DIR/wireless" || return 1
	recovery_file=$(wireless_quarantine_recovery_file)
	publish_wireless_quarantine_copy "$CONFIG_DIR/wireless" "$recovery_file" 0 || return 1

	transaction_dir=$(mktemp -d "$WIRELESS_QUARANTINE_ROOT/.transaction.XXXXXX") || return 1
	case "$transaction_dir" in
		"$WIRELESS_QUARANTINE_ROOT"/.transaction.*) : ;;
		*) return 1 ;;
	esac
	chmod 700 "$transaction_dir" || return 1
	mkdir -m 700 "$transaction_dir/delta" || return 1
	cp -p "$CONFIG_DIR/wireless" "$transaction_dir/wireless" || return 1
	chmod 600 "$transaction_dir/wireless" || return 1

	disable_result=$(run_quarantine_ucode_action disable-all-wireless \
		"$transaction_dir" "$transaction_dir/delta" 2>/dev/null) || disable_result=invalid
	case "$disable_result" in
		changed|unchanged)
			wireless_file_is_explicitly_disabled "$transaction_dir/wireless" ||
				disable_result=invalid
			;;
		*) disable_result=invalid ;;
	esac
	if [ "$disable_result" = invalid ]; then
		: >"$transaction_dir/wireless" || return 1
		chmod 600 "$transaction_dir/wireless" || return 1
		secure_wireless_file "$transaction_dir/wireless" || return 1
	fi

	disabled_file=$(wireless_quarantine_disabled_file)
	publish_wireless_quarantine_copy "$transaction_dir/wireless" "$disabled_file" 1 || return 1
	install_quarantined_wireless "$transaction_dir/wireless" || return 1
	[ -x "$CMP" ] || return 1
	"$CMP" -s "$CONFIG_DIR/wireless" "$disabled_file" || return 1
	wireless_file_is_explicitly_disabled "$CONFIG_DIR/wireless" || return 1
	cleanup_wireless_quarantine_transaction "$transaction_dir" || return 1

	[ -x "$UCI" ] || return 0
	pending_after_quarantine=$("$UCI" -q changes wireless 2>/dev/null) || return 0
	[ -z "$pending_after_quarantine" ] || return 0
	publish_wireless_quarantine_state DISABLED
}

wireless_quarantine_matches_live() {
	disabled_file=$(wireless_quarantine_disabled_file)
	secure_wireless_file "$disabled_file" || return 1
	secure_wireless_file "$CONFIG_DIR/wireless" || return 1
	[ -x "$CMP" ] || return 1
	"$CMP" -s "$CONFIG_DIR/wireless" "$disabled_file" || return 1
	wireless_file_is_explicitly_disabled "$CONFIG_DIR/wireless"
}

handle_existing_wireless_quarantine() {
	quarantine_path_present "$WIRELESS_QUARANTINE_ROOT" || return 0
	validate_wireless_quarantine_root || return 1
	cleanup_stale_wireless_quarantine_artifacts || return 1
	quarantine_state=$(read_wireless_quarantine_state) || return 1
	if wireless_quarantine_matches_live; then
		[ -x "$UCI" ] || return 1
		quarantine_pending=$("$UCI" -q changes wireless 2>/dev/null) || return 1
		[ -z "$quarantine_pending" ] || return 1
		if [ "$quarantine_state" = PREPARING ]; then
			publish_wireless_quarantine_state DISABLED || return 1
		fi
		stop_wireless
		WIRELESS_QUARANTINE_SAFE_DISABLED=1
		return 0
	fi
	[ "$quarantine_state" = DISABLED ] || return 1
	WIRELESS_QUARANTINE_REPAIR=1
	return 0
}

clear_wireless_quarantine_after_repair() {
	[ "$WIRELESS_QUARANTINE_REPAIR" -eq 1 ] || return 0
	validate_wireless_quarantine_root || return 1
	[ "$(read_wireless_quarantine_state)" = DISABLED ] || return 1
	recovery_file=$(wireless_quarantine_recovery_file)
	disabled_file=$(wireless_quarantine_disabled_file)
	secure_wireless_file "$recovery_file" || return 1
	secure_wireless_file "$disabled_file" || return 1
	rm -f "$recovery_file" "$disabled_file" || return 1
	sync_wireless_quarantine || return 1
	rm -f "$(wireless_quarantine_state_file)" || return 1
	sync_wireless_quarantine || return 1
	rmdir "$WIRELESS_QUARANTINE_ROOT" || return 1
	sync_wireless_quarantine
}

validate_reboot_marker_path() {
	case "$REBOOT_REQUIRED_MARKER" in
		/*) : ;;
		*) fail "unsafe wireless reboot marker path: $REBOOT_REQUIRED_MARKER" ;;
	esac
	[ "$REBOOT_REQUIRED_MARKER" != / ] || fail 'refusing filesystem-root wireless reboot marker path'
	marker_parent=${REBOOT_REQUIRED_MARKER%/*}
	[ -n "$marker_parent" ] || marker_parent=/
	secure_parent_directory "$marker_parent" ||
		fail "wireless reboot marker parent is not secure: $marker_parent"
}

marker_exists() {
	[ -e "$REBOOT_REQUIRED_MARKER" ] || [ -L "$REBOOT_REQUIRED_MARKER" ]
}

validate_existing_reboot_marker() {
	secure_private_directory "$REBOOT_REQUIRED_MARKER" ||
		fail 'wireless reboot marker is linked, malformed, or not root-only'
}

create_reboot_marker() {
	validate_reboot_marker_path
	if marker_exists; then
		validate_existing_reboot_marker
		return 0
	fi
	umask 077
	mkdir -m 700 "$REBOOT_REQUIRED_MARKER" ||
		fail 'cannot create the wireless cold-reboot marker'
	validate_existing_reboot_marker
}

stop_wireless() {
	[ -x "$WIFI" ] || fail "wifi is not executable: $WIFI"
	attempt=1
	while [ "$attempt" -le 5 ]; do
		# OpenWrt's /sbin/wifi may return the status of a later best-effort
		# operation instead of the ubus down request.  Its exit code is never
		# accepted as proof; only the independent state probe below can release
		# the persistent config transaction.
		"$WIFI" down >/dev/null 2>&1 || :
		if wireless_is_down; then
			return 0
		fi
		[ "$attempt" -lt 5 ] || break
		[ -x "$SLEEP" ] || fail "sleep is not executable: $SLEEP"
		"$SLEEP" 1 || fail 'cannot wait before re-proving Wi-Fi shutdown'
		attempt=$((attempt + 1))
	done
	fail 'cannot independently prove Wi-Fi is down after five attempts'
}

wireless_is_down() {
	if [ -n "$WIRELESS_DOWN_PROBE" ]; then
		[ -x "$WIRELESS_DOWN_PROBE" ] || return 1
		"$WIRELESS_DOWN_PROBE" "$MODE"
		return
	fi

	[ -x "$UBUS" ] || return 1
	hostapd_objects=$("$UBUS" -S list 'hostapd.*' 2>/dev/null) || return 1
	[ -z "$hostapd_objects" ] || return 1

	wireless_status=$("$UBUS" -S call network.wireless status 2>/dev/null) || {
		# S18 precedes netifd on the pinned OpenWrt image.  At that cold-boot
		# boundary ubusd is already available, but network.wireless is not yet
		# registered.  Successful enumeration proving that no hostapd object
		# exists is the only permitted object-absent case.
		[ "$MODE" = boot ]
		return
	}
	[ -f "$JSHN" ] && [ ! -L "$JSHN" ] || return 1
	wireless_status_is_down "$wireless_status"
}

wireless_status_is_down() (
	# OpenWrt 23.05's jshn.sh uses optional positional parameters internally
	# and is not nounset-safe.  Isolate it in a subshell with nounset disabled;
	# the parent helper remains strict and no parser variables leak globally.
	set +u
	# shellcheck disable=SC1090 -- fixed production path or explicit test seam.
	. "$JSHN" || exit 1
	json_init
	json_load "$1" >/dev/null 2>&1 || exit 1
	json_get_keys radios || exit 1
	[ -n "$radios" ] || exit 1
	printf '%s\n' "$radios" |
		grep -Eq '^[[:space:]]*[A-Za-z0-9_.-]+([[:space:]]+[A-Za-z0-9_.-]+)*[[:space:]]*$' ||
		exit 1
	IFS=$(printf ' \t\nx')
	IFS=${IFS%x}
	for radio in $radios; do
		json_select "$radio" >/dev/null 2>&1 || exit 1
		radio_up=
		radio_pending=
		radio_autostart=
		for field in up pending autostart; do
			json_get_type field_type "$field" >/dev/null 2>&1 || exit 1
			[ "$field_type" = boolean ] || exit 1
		done
		json_get_var radio_up up >/dev/null 2>&1 || exit 1
		json_get_var radio_pending pending >/dev/null 2>&1 || exit 1
		json_get_var radio_autostart autostart >/dev/null 2>&1 || exit 1
		[ "$radio_up" = 0 ] || exit 1
		[ "$radio_pending" = 0 ] || exit 1
		[ "$radio_autostart" = 0 ] || exit 1
		json_select .. >/dev/null 2>&1 || exit 1
	done
	exit 0
)

fail_wireless_closed() {
	wireless_failure=$*
	quarantine_result=
	if [ "$MODE" = boot ]; then
		if quarantine_wireless_at_boot; then
			quarantine_result='; persistent wireless quarantine installed'
		else
			quarantine_result='; persistent wireless quarantine remains PREPARING'
		fi
	fi
	# Publish the recovery boundary before stopping an active AP.  If the
	# process is killed at any later instruction, the next S10/S18 or explicit
	# invocation sees the marker and repeats the independently-proven shutdown.
	create_reboot_marker
	stop_wireless
	fail "$wireless_failure$quarantine_result; Wi-Fi remains down until a cold reboot"
}

prepare_wireless_stage() {
	case "$WIRELESS_STAGE_ROOT" in
		/*) : ;;
		*) fail_wireless_closed "unsafe wireless staging root: $WIRELESS_STAGE_ROOT" ;;
	esac
	[ "$WIRELESS_STAGE_ROOT" != / ] ||
		fail_wireless_closed 'refusing filesystem-root wireless staging path'
	if [ ! -e "$WIRELESS_STAGE_ROOT" ] && [ ! -L "$WIRELESS_STAGE_ROOT" ]; then
		umask 077
		mkdir -m 700 "$WIRELESS_STAGE_ROOT" ||
			fail_wireless_closed 'cannot create the wireless staging root'
	fi
	secure_private_directory "$WIRELESS_STAGE_ROOT" ||
		fail_wireless_closed 'wireless staging root is linked, malformed, or not root-only'
	WIRELESS_STAGE_DIR=$(mktemp -d "$WIRELESS_STAGE_ROOT/.wireless-stage.XXXXXX") ||
		fail_wireless_closed 'cannot create the private wireless stage'
	case "$WIRELESS_STAGE_DIR" in
		"$WIRELESS_STAGE_ROOT"/.wireless-stage.*) : ;;
		*) fail_wireless_closed 'mktemp returned an unsafe wireless staging directory' ;;
	esac
	chmod 700 "$WIRELESS_STAGE_DIR" ||
		fail_wireless_closed 'cannot make the wireless stage root-only'
	secure_private_directory "$WIRELESS_STAGE_DIR" ||
		fail_wireless_closed 'private wireless stage is not secure'
	stage_metadata=$(path_metadata "$WIRELESS_STAGE_DIR") ||
		fail_wireless_closed 'cannot inspect wireless stage filesystem'
	config_metadata=$(path_metadata "$CONFIG_DIR") ||
		fail_wireless_closed 'cannot inspect UCI config filesystem'
	[ "${stage_metadata##* }" = "${config_metadata##* }" ] ||
		fail_wireless_closed 'wireless stage is not on the live UCI overlay filesystem'
	cp -p "$CONFIG_DIR/wireless" "$WIRELESS_STAGE_DIR/wireless" ||
		fail_wireless_closed 'cannot copy wireless config into the private stage'
	secure_wireless_file "$WIRELESS_STAGE_DIR/wireless" ||
		fail_wireless_closed 'staged wireless config is not a private single-link root-owned file'
}

install_staged_wireless() {
	secure_wireless_file "$CONFIG_DIR/wireless" ||
		return 1
	secure_wireless_file "$WIRELESS_STAGE_DIR/wireless" ||
		return 1
	if [ -n "$WIRELESS_INSTALL" ]; then
		[ -x "$WIRELESS_INSTALL" ] ||
			return 1
		"$WIRELESS_INSTALL" "$WIRELESS_STAGE_DIR/wireless" "$CONFIG_DIR/wireless" ||
			return 1
	else
		mv -fT "$WIRELESS_STAGE_DIR/wireless" "$CONFIG_DIR/wireless" ||
			return 1
	fi
	secure_wireless_file "$CONFIG_DIR/wireless" ||
		return 1
}

append_line() {
	variable_name=$1
	line=$2
	eval "current_lines=\${$variable_name:-}"
	if [ -n "$current_lines" ]; then
		current_lines="$current_lines
$line"
	else
		current_lines=$line
	fi
	eval "$variable_name=\$current_lines"
}

configure_wireless() {
	validate_reboot_marker_path
	if marker_exists; then
		validate_existing_reboot_marker
		# A prior process may have been killed after publishing the marker but
		# before /sbin/wifi completed.  Every re-entry must therefore repeat and
		# independently prove shutdown before honoring the reboot boundary.
		stop_wireless
		fail 'wireless isolation changed at runtime; a cold reboot is still required'
	fi
	if ! handle_existing_wireless_quarantine; then
		fail_wireless_closed 'cannot safely recover the persistent wireless quarantine'
	fi
	[ "$WIRELESS_QUARANTINE_SAFE_DISABLED" -eq 0 ] || return 0
	[ -x "$UCI" ] || fail_wireless_closed "uci is not executable: $UCI"
	pending=$("$UCI" -q changes wireless 2>/dev/null) ||
		fail_wireless_closed 'cannot prove the wireless package has no pending delta'
	[ -z "$pending" ] || fail_wireless_closed 'wireless has a pending UCI delta'
	case "$CONFIG_DIR" in
		/*) : ;;
		*) fail_wireless_closed "unsafe UCI config directory: $CONFIG_DIR" ;;
	esac
	[ "$CONFIG_DIR" != / ] || fail_wireless_closed 'refusing filesystem-root UCI config directory'
	secure_parent_directory "$CONFIG_DIR" ||
		fail_wireless_closed "UCI config directory is linked, writable by another uid, or not root-owned: $CONFIG_DIR"
	secure_wireless_file "$CONFIG_DIR/wireless" ||
		fail_wireless_closed 'wireless config is not a private single-link root-owned regular file'
	prepare_wireless_stage
	case "$TMP_ROOT" in
		/*) : ;;
		*) fail_wireless_closed "unsafe temporary root: $TMP_ROOT" ;;
	esac
	[ "$TMP_ROOT" != / ] || fail_wireless_closed 'refusing filesystem-root temporary path'
	[ -d "$TMP_ROOT" ] && [ ! -L "$TMP_ROOT" ] ||
		fail_wireless_closed "temporary root is not a real directory: $TMP_ROOT"
	WIRELESS_DELTA_DIR=$(mktemp -d "$TMP_ROOT/proxypool-wireless.XXXXXX") ||
		fail_wireless_closed 'cannot create private wireless UCI savedir'
	case "$WIRELESS_DELTA_DIR" in
		"$TMP_ROOT"/proxypool-wireless.*) : ;;
		*) fail_wireless_closed 'mktemp returned an unsafe wireless UCI savedir' ;;
	esac
	chmod 700 "$WIRELESS_DELTA_DIR" ||
		fail_wireless_closed 'cannot make the private wireless UCI savedir root-only'
	secure_private_directory "$WIRELESS_DELTA_DIR" ||
		fail_wireless_closed 'private wireless UCI savedir is not root-only'

	if [ -n "$UCI_STAGED_APPLY" ]; then
		[ -x "$UCI_STAGED_APPLY" ] ||
			fail_wireless_closed "staged UCI apply seam is not executable: $UCI_STAGED_APPLY"
		wireless_result=$("$UCI_STAGED_APPLY" apply-wireless-isolation \
			"$WIRELESS_STAGE_DIR" "$WIRELESS_DELTA_DIR") ||
			fail_wireless_closed 'cannot apply wireless isolation through the absolute-path UCI transaction'
	else
		[ -x "$UCODE" ] || fail_wireless_closed "ucode is not executable: $UCODE"
		case "$UCI_STAGED_HELPER" in
			/*) : ;;
			*) fail_wireless_closed "unsafe staged UCI helper path: $UCI_STAGED_HELPER" ;;
		esac
		[ "$UCI_STAGED_HELPER" != / ] ||
			fail_wireless_closed 'refusing filesystem-root staged UCI helper path'
		[ -f "$UCI_STAGED_HELPER" ] && [ ! -L "$UCI_STAGED_HELPER" ] ||
			fail_wireless_closed "staged UCI helper is not a regular non-symlink file: $UCI_STAGED_HELPER"
		wireless_result=$("$UCODE" "$UCI_STAGED_HELPER" apply-wireless-isolation \
			"$WIRELESS_STAGE_DIR" "$WIRELESS_DELTA_DIR") ||
			fail_wireless_closed 'cannot apply wireless isolation through the absolute-path UCI transaction'
	fi

	secure_wireless_file "$WIRELESS_STAGE_DIR/wireless" ||
		fail_wireless_closed 'staged wireless helper returned an unsafe config file'
	[ -x "$CMP" ] || fail_wireless_closed "cmp is not executable: $CMP"
	case "$wireless_result" in
		changed)
			"$CMP" -s "$CONFIG_DIR/wireless" "$WIRELESS_STAGE_DIR/wireless" &&
				fail_wireless_closed 'staged wireless helper reported changed without changing its stage'
			WIRELESS_CHANGED=1
			;;
		unchanged)
			"$CMP" -s "$CONFIG_DIR/wireless" "$WIRELESS_STAGE_DIR/wireless" ||
				fail_wireless_closed 'staged wireless helper changed its stage but reported unchanged'
			;;
		*) fail_wireless_closed 'staged wireless helper returned an invalid success state' ;;
	esac

	if [ "$WIRELESS_CHANGED" -eq 1 ]; then
		# The marker precedes every operation that can make runtime Wi-Fi
		# unavailable or replace persistent configuration.  Any later failure
		# therefore leaves an unambiguous cold-reboot requirement.
		create_reboot_marker
		stop_wireless
	fi

	pending=$("$UCI" -q changes wireless 2>/dev/null) || {
		fail_wireless_closed 'cannot recheck wireless pending deltas after staging'
	}
	if [ -n "$pending" ]; then
		fail_wireless_closed 'wireless gained a pending UCI delta after staging'
	fi

	if [ "$WIRELESS_CHANGED" -eq 0 ]; then
		cleanup_wireless_stage || fail 'cannot remove the unchanged wireless stage'
		cleanup_wireless_delta || fail 'cannot remove private wireless UCI savedir'
		clear_wireless_quarantine_after_repair ||
			fail_wireless_closed 'cannot retire the repaired wireless quarantine'
		return 0
	fi

	install_staged_wireless ||
		fail_wireless_closed 'cannot atomically install staged wireless isolation'
	cleanup_wireless_stage || fail 'cannot remove the installed wireless stage'
	cleanup_wireless_delta || fail 'cannot remove private wireless UCI savedir'

	fail 'wireless isolation was installed with Wi-Fi down; cold reboot is required'
}

valid_port_name() {
	printf '%s\n' "$1" | grep -Eq '^[A-Za-z0-9_.:-]{1,15}$'
}

valid_network_device_section() {
	printf '%s\n' "$1" | grep -Eq '^([A-Za-z0-9_]+|@device\[[0-9]+\])$'
}

discover_expected_bridge_ports() {
	[ -x "$UCI" ] || fail "uci is not executable: $UCI"
	pending=$("$UCI" -q changes network 2>/dev/null) ||
		fail 'cannot prove the network package has no pending delta'
	[ -z "$pending" ] || fail 'network has a pending UCI delta'
	network_dump=$("$UCI" -q show network 2>/dev/null) ||
		fail 'cannot enumerate network configuration'
	device_sections=$(printf '%s\n' "$network_dump" |
		sed -n 's/^network\.\([^.=]*\)=device$/\1/p')
	bridge_section=
	old_ifs=$IFS
	IFS='
'
	for section in $device_sections; do
		[ -n "$section" ] || continue
		valid_network_device_section "$section" ||
			fail "unsafe network device section name: $section"
		device_name=$("$UCI" -q get "network.$section.name" 2>/dev/null) ||
			fail "cannot classify network device $section"
		[ "$device_name" = br-lan ] || continue
		device_type=$("$UCI" -q get "network.$section.type" 2>/dev/null) ||
			fail "cannot read type for br-lan device $section"
		[ "$device_type" = bridge ] ||
			fail "network device $section names br-lan but is not a bridge"
		[ -z "$bridge_section" ] || fail 'network defines more than one br-lan bridge device'
		bridge_section=$section
	done
	IFS=$old_ifs
	[ -n "$bridge_section" ] || fail 'network does not define a br-lan bridge device'

	configured_ports=$("$UCI" -q get "network.$bridge_section.ports" 2>/dev/null) ||
		fail 'br-lan bridge has no configured static ports'
	printf '%s\n' "$configured_ports" |
		grep -Eq '^[A-Za-z0-9_.:-]{1,15}([[:space:]]+[A-Za-z0-9_.:-]{1,15})*$' ||
		fail 'br-lan bridge has an unsafe or empty static port list'

	EXPECTED_BRIDGE_PORTS=
	old_ifs=$IFS
	IFS=$(printf ' \t\nx')
	IFS=${IFS%x}
	# The full-list validation above excludes shell glob metacharacters before
	# this intentional word split.
	for port in $configured_ports; do
		valid_port_name "$port" || fail "unsafe configured br-lan port name: $port"
		if printf '%s\n' "$EXPECTED_BRIDGE_PORTS" | grep -Fxq "$port"; then
			fail "duplicate configured br-lan port: $port"
		fi
		append_line EXPECTED_BRIDGE_PORTS "$port"
	done
	IFS=$old_ifs
	[ -n "$EXPECTED_BRIDGE_PORTS" ] || fail 'br-lan bridge has no configured static ports'
	configured_port_count=$(printf '%s\n' "$EXPECTED_BRIDGE_PORTS" |
		sed '/^$/d' | wc -l | tr -d '[:space:]')
	[ "$configured_port_count" = 5 ] ||
		fail 'br-lan must contain exactly the five GL-MT6000 physical LAN ports'
	for required_port in $PHASE1_LAN_PORTS; do
		printf '%s\n' "$EXPECTED_BRIDGE_PORTS" | grep -Fxq "$required_port" ||
			fail "br-lan is missing GL-MT6000 physical port $required_port"
	done

	# The topology is assembled through multiple UCI reads.  Recheck the live
	# savedir after the final read so a concurrent user delta cannot splice two
	# different network truths into one bridge mutation pass.
	pending=$("$UCI" -q changes network 2>/dev/null) ||
		fail 'cannot recheck the network package pending delta'
	[ -z "$pending" ] || fail 'network gained a pending UCI delta during bridge discovery'
}

read_isolated() {
	isolated_path=$1
	[ -f "$isolated_path" ] || return 1
	[ "$(wc -c <"$isolated_path" | tr -d '[:space:]')" -eq 2 ] || return 1
	IFS= read -r value <"$isolated_path" || return 1
	case "$value" in
		0|1) printf '%s\n' "$value" ;;
		*) return 1 ;;
	esac
}

reconcile_bridge_ports() {
	reconcile_mode=${1:-reconcile}
	case "$reconcile_mode" in
		verify|reconcile) : ;;
		*) fail "unsupported bridge reconciliation mode: $reconcile_mode" ;;
	esac
	discover_expected_bridge_ports
	brif="$SYS_CLASS_NET/br-lan/brif"
	[ -d "$brif" ] || fail 'br-lan bridge topology is not ready'

	old_ifs=$IFS
	IFS='
'
	for port in $EXPECTED_BRIDGE_PORTS; do
		member="$brif/$port"
		[ -e "$member" ] || [ -L "$member" ] ||
			fail "configured br-lan member is not ready: $port"
	done
	IFS=$old_ifs

	# Complete the sysfs proof before issuing the first bridge mutation.  A late
	# unreadable member must not produce a partially-applied pass.
	actual_members=
	members_to_change=
	for member in "$brif"/*; do
		[ -e "$member" ] || [ -L "$member" ] || continue
		port=${member##*/}
		valid_port_name "$port" || fail "unsafe br-lan member name: $port"
		isolated="$SYS_CLASS_NET/$port/brport/isolated"
		current=$(read_isolated "$isolated") ||
			fail "cannot prove isolation state for br-lan member $port"
		append_line actual_members "$port"
		[ "$current" = 1 ] || append_line members_to_change "$port"
	done
	[ -n "$actual_members" ] || fail 'br-lan has no provable runtime members'
	if [ "$reconcile_mode" = verify ] && [ -n "$members_to_change" ]; then
		fail 'one or more br-lan members are not isolated'
	fi

	old_ifs=$IFS
	IFS='
'
	for port in $members_to_change; do
		[ -x "$BRIDGE" ] || fail "bridge is not executable: $BRIDGE"
		"$BRIDGE" link set dev "$port" isolated on ||
			fail "cannot isolate br-lan member $port"
		isolated="$SYS_CLASS_NET/$port/brport/isolated"
		current=$(read_isolated "$isolated") ||
			fail "cannot verify isolation state for br-lan member $port"
		[ "$current" = 1 ] || fail "br-lan member $port remained unisolated"
	done
	IFS=$old_ifs

	# Re-enumerate after mutation: every expected static member must still be
	# present and every current dynamic/static member must now prove isolated.
	[ -d "$brif" ] || fail 'br-lan topology disappeared during reconciliation'
	IFS='
'
	for port in $EXPECTED_BRIDGE_PORTS; do
		member="$brif/$port"
		[ -e "$member" ] || [ -L "$member" ] ||
			fail "configured br-lan member disappeared: $port"
	done
	IFS=$old_ifs
	verified_members=
	for member in "$brif"/*; do
		[ -e "$member" ] || [ -L "$member" ] || continue
		port=${member##*/}
		valid_port_name "$port" || fail "unsafe br-lan member name after apply: $port"
		current=$(read_isolated "$SYS_CLASS_NET/$port/brport/isolated") ||
			fail "cannot verify final isolation for br-lan member $port"
		[ "$current" = 1 ] || fail "br-lan member $port is not isolated after apply"
		append_line verified_members "$port"
	done
	[ -n "$verified_members" ] || fail 'br-lan lost every runtime member during reconciliation'
}

run_claimed_lan_reconciliation() {
	claim_lan_reconciliation
	reconcile_bridge_ports reconcile
	complete_lan_reconciliation
}

case "$MODE" in
	configure)
		configure_wireless
		;;
	request)
		request_lan_reconciliation
		;;
	readiness)
		lan_reconciliation_ready
		;;
	verify)
		reconcile_bridge_ports verify
		;;
	reconcile)
		run_claimed_lan_reconciliation
		;;
	boot)
		# S18 precedes netifd.  Persist the safe wireless intent now; strict
		# bridge topology proof happens in the serialized procd worker after S20.
		request_lan_reconciliation
		configure_wireless
		;;
	apply)
		request_lan_reconciliation
		configure_wireless
		run_claimed_lan_reconciliation
		;;
esac
