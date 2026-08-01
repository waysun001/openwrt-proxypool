#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
INIT="$ROOT/proxypool-core/files/proxypool.init"
TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM

TRACE_FILE="$TEST_TMP/trace"
MARKER_FILE="$TEST_TMP/backend"
QUARANTINE_FILE="$TEST_TMP/backend.cleanup-required"
TRANSITION_FILE="$TEST_TMP/transition"
ACTIVE_SNAPSHOT_FILE="$TEST_TMP/active-snapshot"
SNAPSHOT_ROOT="$TEST_TMP/snapshots"
PERSISTENT_STATE_DIR="$TEST_TMP/persistent-state"
ACTIVATED_BACKEND_FILE="$PERSISTENT_STATE_DIR/activated-backend"
CLEANUP_REQUIRED_FILE="$PERSISTENT_STATE_DIR/cleanup-required"
LEGACY_CONFIG_FILE="$TEST_TMP/proxypool"
V2_CONFIG_FILE="$TEST_TMP/proxypool_v2"
SELECTOR_FILE="$TEST_TMP/proxypool_runtime"
CRONTAB_FILE="$TEST_TMP/crontab"
WATCHDOG_FILE="$TEST_TMP/watchdog"
PID_FILE="$TEST_TMP/xl2tpd.pid"
PROCD_RUNNING_FILE="$TEST_TMP/procd-running"
PROCD_STAGED_FILE="$TEST_TMP/procd-staged"
PROCD_TRANSACTION_FILE="$TEST_TMP/procd-transaction"
LEGACY_RUNNING_FILE="$TEST_TMP/legacy-running"
QUERY_COUNT_FILE="$TEST_TMP/query-count"
SED_COUNT_FILE="$TEST_TMP/sed-count"
FIREWALL_SAFETY_TEMPLATE_FILE="$TEST_TMP/proxypool-safety-uci-default"
FIREWALL_TRANSACTION_HELPER_FILE="$TEST_TMP/proxypool-firewall-transaction"
FIREWALL_TRANSACTION_DIR="$TEST_TMP/firewall-transaction"
LAN_ISOLATION_HELPER_FILE="$TEST_TMP/lan-isolation"
LEGACY_GATE_FILE="$TEST_TMP/legacy-gate"
export TRACE_FILE MARKER_FILE QUARANTINE_FILE TRANSITION_FILE ACTIVE_SNAPSHOT_FILE
export SNAPSHOT_ROOT LEGACY_CONFIG_FILE V2_CONFIG_FILE SELECTOR_FILE
export PROCD_RUNNING_FILE PROCD_STAGED_FILE PROCD_TRANSACTION_FILE LEGACY_RUNNING_FILE
export QUERY_COUNT_FILE SED_COUNT_FILE PERSISTENT_STATE_DIR ACTIVATED_BACKEND_FILE CLEANUP_REQUIRED_FILE

make_fake() {
	path=$1
	body=$2
	cat >"$path" <<EOF
#!/usr/bin/env sh
$body
EOF
	chmod +x "$path"
}

make_fake "$TEST_TMP/legacy" '
printf "legacy:%s\n" "$*" >>"$TRACE_FILE"
if [ -n "${PROXYPOOL_TEST_REQUIRE_ACTIVATED-}" ]; then
	[ -f "$ACTIVATED_BACKEND_FILE" ] && [ "$(cat "$ACTIVATED_BACKEND_FILE")" = "$PROXYPOOL_TEST_REQUIRE_ACTIVATED" ] || {
		printf "activated:missing-before-legacy:%s\n" "${1-}" >>"$TRACE_FILE"
		exit 96
	}
fi
if [ -n "${PROXYPOOL_TEST_REQUIRE_WAL-}" ]; then
	[ -f "$CLEANUP_REQUIRED_FILE" ] && [ "$(cat "$CLEANUP_REQUIRED_FILE")" = "$PROXYPOOL_TEST_REQUIRE_WAL" ] || {
		printf "wal:missing-before-legacy:%s\n" "${1-}" >>"$TRACE_FILE"
		exit 97
	}
fi
case "${1-}" in
	start)
		case "${PROXYPOOL_TEST_LEGACY_FAIL-}" in
			start|start_and_stop)
				if [ "${PROXYPOOL_TEST_MUTATE_LEGACY_ON_START_FAILURE-0}" = 1 ]; then
					printf "# changed-during-failed-start\n" >>"$LEGACY_CONFIG_FILE"
				fi
				exit 1
				;;
		esac
		: >"$LEGACY_RUNNING_FILE"
		;;
	stop)
		case "${PROXYPOOL_TEST_LEGACY_FAIL-}" in stop|start_and_stop|reload_and_stop) exit 1 ;; esac
		rm -f "$LEGACY_RUNNING_FILE"
		if [ "${PROXYPOOL_TEST_MUTATE_LEGACY_AFTER_STOP-0}" = 1 ]; then
			printf "# changed-after-stop\n" >>"$LEGACY_CONFIG_FILE"
		fi
		;;
	reload)
		case "${PROXYPOOL_TEST_LEGACY_FAIL-}" in reload|reload_and_stop) exit 1 ;; esac
		;;
esac'
make_fake "$TEST_TMP/lease" '
printf "lease:%s\n" "$*" >>"$TRACE_FILE"
[ "${PROXYPOOL_TEST_LEASE_FAIL-}:${1-}" != "flush:flush" ]'
make_fake "$TEST_TMP/cron" '
printf "cron:%s\n" "$*" >>"$TRACE_FILE"
[ "${PROXYPOOL_TEST_CRON_FAIL-}:${1-}" != "restart:restart" ]'
make_fake "$TEST_TMP/xl2tpd" '
printf "xl2tpd:%s\n" "$*" >>"$TRACE_FILE"
[ "${PROXYPOOL_TEST_MUTATE_LEGACY_ON_XL2TPD_DISABLE-0}:${1-}" != "1:disable" ] ||
	printf "# changed-during-xl2tpd-disable\n" >>"$LEGACY_CONFIG_FILE"
[ "${1-}" = enabled ] && exit 0
exit 0'
make_fake "$LEGACY_GATE_FILE" '
printf "gate:%s\n" "$*" >>"$TRACE_FILE"
printf "%s\n" legacy_runtime_quarantined
exit 125'
make_fake "$FIREWALL_TRANSACTION_HELPER_FILE" '
printf "safety:%s\n" "$*" >>"$TRACE_FILE"
case "${1-}" in
	journal-present)
		case "${PROXYPOOL_TEST_FIREWALL_JOURNAL:-absent}" in
			present) exit 0 ;;
			absent) exit 1 ;;
			*) exit 2 ;;
		esac
		;;
	activation-current)
		[ "${PROXYPOOL_TEST_FIREWALL_ACTIVATION:-current}" = current ]
		;;
	activation-runtime-current)
		[ "${PROXYPOOL_TEST_FIREWALL_RUNTIME:-current}" = current ]
		;;
	*) exit 2 ;;
esac'
make_fake "$LAN_ISOLATION_HELPER_FILE" '
printf "l2:%s\n" "$*" >>"$TRACE_FILE"
[ "${PROXYPOOL_TEST_LAN_ISOLATION:-ready}" = ready ]'
make_fake "$TEST_TMP/ls-metadata" '
[ "$#" -eq 2 ] && [ "$1" = -nd ] || exit 2
path=$2
[ -e "$path" ] && [ ! -L "$path" ] || exit 1
owner=0
group=0
host_metadata=$(LC_ALL=C ls -nd "$path") || exit 1
set -- $host_metadata
links=$2
if [ -d "$path" ]; then
	permissions=drwx------
elif [ "$path" = "${PROXYPOOL_FIREWALL_TRANSACTION_HELPER:-}" ]; then
	permissions=-rwxr-xr-x
else
	permissions=-rw-------
fi
case "${PROXYPOOL_TEST_METADATA_FAULT-}" in
	state_owner) [ "$path" != "$PERSISTENT_STATE_DIR" ] || owner=1000 ;;
	state_mode) [ "$path" != "$PERSISTENT_STATE_DIR" ] || permissions=drwxrwxrwx ;;
	state_links) [ "$path" != "$PERSISTENT_STATE_DIR" ] || links=7 ;;
	activated_owner) [ "$path" != "$ACTIVATED_BACKEND_FILE" ] || owner=1000 ;;
	activated_mode) [ "$path" != "$ACTIVATED_BACKEND_FILE" ] || permissions=-rw-rw-rw- ;;
	activated_links) [ "$path" != "$ACTIVATED_BACKEND_FILE" ] || links=2 ;;
	cleanup_owner) [ "$path" != "$CLEANUP_REQUIRED_FILE" ] || owner=1000 ;;
	cleanup_mode) [ "$path" != "$CLEANUP_REQUIRED_FILE" ] || permissions=-rw-rw-rw- ;;
	cleanup_links) [ "$path" != "$CLEANUP_REQUIRED_FILE" ] || links=2 ;;
	firewall_helper_owner) [ "$path" != "${PROXYPOOL_FIREWALL_TRANSACTION_HELPER:-}" ] || owner=1000 ;;
	firewall_helper_mode) [ "$path" != "${PROXYPOOL_FIREWALL_TRANSACTION_HELPER:-}" ] || permissions=-rwxrwxrwx ;;
	firewall_helper_links) [ "$path" != "${PROXYPOOL_FIREWALL_TRANSACTION_HELPER:-}" ] || links=2 ;;
esac
printf "%s %s %s %s 0 Jan 1 00:00 %s\n" "$permissions" "$links" "$owner" "$group" "$path"'
make_fake "$TEST_TMP/controller" '
command_name=${1-}
case "$command_name" in
	select-backend)
		[ "$#" -eq 3 ] && [ "$2" = --config ] || exit 2
		printf "ctl:select:%s\n" "$3" >>"$TRACE_FILE"
		case "${PROXYPOOL_TEST_SELECTOR_OUTPUT-}" in
			bad) printf "v1 unexpected\n"; exit 0 ;;
			no_newline) printf v1; exit 0 ;;
			failure) exit 1 ;;
		esac
		[ -e "$3" ] || { printf "missing\n"; exit 0; }
		[ -f "$3" ] || { printf "unknown\n"; exit 1; }
		if grep -Fq "runtime_backend '\''v1'\''" "$3"; then
			printf "v1\n"
		elif grep -Fq "runtime_backend '\''v2_shadow'\''" "$3"; then
			printf "v2_shadow\n"
		else
			printf "unknown\n"
			exit 1
		fi
		;;
	classify)
		[ "$#" -eq 3 ] && [ "$2" = --config ] || exit 2
		printf "ctl:classify:%s\n" "$3" >>"$TRACE_FILE"
		if [ "${PROXYPOOL_TEST_MUTATE_SELECTOR_DURING_CLASSIFY-0}" = 1 ]; then
			printf "config global '\''global'\''\n\toption runtime_backend '\''v1'\''\n" >"$SELECTOR_FILE"
		fi
		class=$(sed -n "s/^# test-class://p" "$3" | head -n1)
		if [ "${PROXYPOOL_TEST_MUTATE_LEGACY_DURING_CLASSIFY-0}" = 1 ] && [ "$class" = v1 ]; then
			printf "# changed-during-classification\n" >>"$LEGACY_CONFIG_FILE"
		fi
		case "$class" in
			v1|v2_shadow|v2_shadow_invalid) printf "%s\n" "$class" ;;
			bad_output) printf "v1 unexpected\n" ;;
			*) printf "unknown\n"; exit 1 ;;
		esac
		;;
	config-enabled)
		[ "$#" -eq 3 ] && [ "$2" = --config ] || exit 2
		printf "ctl:enabled:%s\n" "$3" >>"$TRACE_FILE"
		[ "${PROXYPOOL_TEST_ENABLED_FAILURE-0}" != 1 ] || exit 1
		if grep -Fq "option enabled '\''0'\''" "$3"; then printf "0\n"; else printf "1\n"; fi
		;;
	procd-state)
		[ "$#" -eq 3 ] || [ "$#" -eq 5 ] || exit 2
		[ "$2" = --service ] && [ "$3" = proxypool ] || exit 2
		instance=
		if [ "$#" -eq 5 ]; then
			[ "$4" = --instance ] || exit 2
			instance=$5
		fi
		printf "ctl:procd:%s\n" "${instance:-any}" >>"$TRACE_FILE"
		query_count=0
		[ ! -f "$QUERY_COUNT_FILE" ] || query_count=$(cat "$QUERY_COUNT_FILE")
		query_count=$((query_count + 1))
		printf "%s\n" "$query_count" >"$QUERY_COUNT_FILE"
		case "${PROXYPOOL_TEST_QUERY-}" in
			unknown) printf "unknown\n"; exit 1 ;;
			unknown_after_first) [ "$query_count" -le 1 ] || { printf "unknown\n"; exit 1; } ;;
			unknown_after_second) [ "$query_count" -le 2 ] || { printf "unknown\n"; exit 1; } ;;
			bad_output) printf "present unexpected\n"; exit 0 ;;
			global_running) [ -n "$instance" ] || { printf "running\n"; exit 0; } ;;
		esac
		if [ -n "$instance" ]; then
			if [ "${PROXYPOOL_TEST_EXACT_STATE-}" = present ]; then
				printf "present\n"
			elif [ -f "$PROCD_RUNNING_FILE" ] && grep -Fqx "$instance" "$PROCD_RUNNING_FILE"; then
				printf "running\n"
			else
				printf "absent\n"
			fi
		elif [ -s "$PROCD_RUNNING_FILE" ]; then
			printf "present\n"
		else
			printf "absent\n"
		fi
		;;
	*) exit 2 ;;
esac'

fail() {
	printf '%s\n' "$*" >&2
	printf '%s\n' '--- trace ---' >&2
	cat "$TRACE_FILE" >&2
	exit 1
}

assert_contains() {
	grep -Fqx -- "$1" "$TRACE_FILE" || fail "missing trace line: $1"
}

assert_contains_fragment() {
	grep -Fq -- "$1" "$TRACE_FILE" || fail "missing trace fragment: $1"
}

assert_not_contains_fragment() {
	if grep -Fq -- "$1" "$TRACE_FILE"; then fail "unexpected trace fragment: $1"; fi
}

assert_not_contains() {
	if grep -Fqx -- "$1" "$TRACE_FILE"; then fail "unexpected trace line: $1"; fi
}

assert_no_backend_mutation() {
	assert_not_contains_fragment 'legacy:'
	assert_not_contains_fragment 'procd:open:'
	assert_not_contains_fragment 'procd:service-close:'
	assert_not_contains_fragment 'procd:kill:'
}

assert_before() {
	first=$(grep -nF "$1" "$TRACE_FILE" | head -n1 | cut -d: -f1)
	second=$(grep -nF "$2" "$TRACE_FILE" | head -n1 | cut -d: -f1)
	[ -n "$first" ] && [ -n "$second" ] && [ "$first" -lt "$second" ] || fail "expected $1 before $2"
}

assert_file_line() {
	[ -f "$1" ] && [ "$(cat "$1")" = "$2" ] || fail "unexpected file content: $1"
}

generation_count() {
	count=0
	if [ -d "$SNAPSHOT_ROOT" ] && [ ! -L "$SNAPSHOT_ROOT" ]; then
		for generation in "$SNAPSHOT_ROOT"/generation.*; do
			[ -e "$generation" ] || [ -L "$generation" ] || continue
			count=$((count + 1))
		done
	fi
	printf '%s\n' "$count"
}

assert_generation_count() {
	actual=$(generation_count)
	[ "$actual" -eq "$1" ] || fail "expected $1 snapshot generation(s), found $actual"
}

make_managed_generation() {
	generation="$SNAPSHOT_ROOT/generation.$1"
	mkdir -p "$generation"
	cp "$V2_CONFIG_FILE" "$generation/proxypool"
	printf '%s\n' "$generation"
}

expect_success() {
	if ! "$@"; then fail "expected success: $*"; fi
}

expect_failure() {
	if "$@"; then fail "expected failure: $*"; fi
}

write_legacy_config() {
	enabled=${1-1}
	cat >"$LEGACY_CONFIG_FILE" <<EOF
# test-class:v1
config global 'global'
	option enabled '$enabled'
	option max_clients '60'
config client 'old-client'
	option password 'legacy-test-secret'
EOF
}

write_v2_config() {
	class=${1-v2_shadow}
	enabled=${2-1}
	cat >"$V2_CONFIG_FILE" <<EOF
# test-class:$class
config global 'global'
	option schema_version '2'
	option enabled '$enabled'
	option runtime_backend 'v2_shadow'
	option password 'v2-test-secret'
EOF
}

set_activated_backend() {
	mkdir -p "$PERSISTENT_STATE_DIR"
	printf '%s\n' "$1" >"$ACTIVATED_BACKEND_FILE"
}

set_selector_only() {
	case "$1" in
		missing) rm -rf "$SELECTOR_FILE" ;;
		v1|v2_shadow)
			cat >"$SELECTOR_FILE" <<EOF
config global 'global'
	option runtime_backend '$1'
EOF
			;;
		unknown) printf "config global 'global'\n\toption runtime_backend 'future'\n" >"$SELECTOR_FILE" ;;
		directory) rm -rf "$SELECTOR_FILE"; mkdir "$SELECTOR_FILE" ;;
	esac
}

set_selector() {
	set_selector_only "$1"
	[ "$1" != v2_shadow ] || set_activated_backend v2_shadow
}

reset_case() {
	: >"$TRACE_FILE"
	: >"$CRONTAB_FILE"
	rm -f "$MARKER_FILE" "$TRANSITION_FILE" "$ACTIVE_SNAPSHOT_FILE"
	rm -rf "$QUARANTINE_FILE"
	rm -f "$PID_FILE" "$PROCD_RUNNING_FILE" "$PROCD_STAGED_FILE" "$PROCD_TRANSACTION_FILE" "$LEGACY_RUNNING_FILE" "$QUERY_COUNT_FILE" "$SED_COUNT_FILE"
	rm -rf "$SNAPSHOT_ROOT" "$PERSISTENT_STATE_DIR" "$TEST_TMP/legacy-run" "$SELECTOR_FILE"
	rm -rf "$FIREWALL_TRANSACTION_DIR"
	printf '%s\n' '# safety template fixture' >"$FIREWALL_SAFETY_TEMPLATE_FILE"
	write_legacy_config 1
	write_v2_config v2_shadow 1
	set_selector_only missing
}

run_action() (
	set +e
	action=$1
	export PROXYPOOL_RUNTIME_MARKER="$MARKER_FILE"
	export PROXYPOOL_QUARANTINE_MARKER="$QUARANTINE_FILE"
	export PROXYPOOL_TRANSITION_MARKER="$TRANSITION_FILE"
	export PROXYPOOL_ACTIVE_SNAPSHOT_MARKER="$ACTIVE_SNAPSHOT_FILE"
	export PROXYPOOL_SNAPSHOT_ROOT="$SNAPSHOT_ROOT"
	export PROXYPOOL_PERSISTENT_STATE_DIR="$PERSISTENT_STATE_DIR"
	export PROXYPOOL_ACTIVATED_BACKEND_MARKER="$ACTIVATED_BACKEND_FILE"
	export PROXYPOOL_CLEANUP_REQUIRED_MARKER="$CLEANUP_REQUIRED_FILE"
	export PROXYPOOL_LEGACY_CONFIG_FILE="$LEGACY_CONFIG_FILE"
	export PROXYPOOL_V2_CONFIG_FILE="$V2_CONFIG_FILE"
	export PROXYPOOL_SELECTOR_FILE="$SELECTOR_FILE"
	export PROXYPOOL_CLASSIFIER="$TEST_TMP/controller"
	export PROXYPOOL_LEGACY_PROG="$TEST_TMP/legacy"
	export PROXYPOOL_LEASE_PROG="$TEST_TMP/lease"
	export PROXYPOOL_CRON_INIT="$TEST_TMP/cron"
	export PROXYPOOL_WATCHDOG_PROG="$WATCHDOG_FILE"
	export PROXYPOOL_XL2TPD_INIT="$TEST_TMP/xl2tpd"
	export PROXYPOOL_CRONTAB_ROOT="$CRONTAB_FILE"
	export PROXYPOOL_XL2TPD_PID="$PID_FILE"
	export PROXYPOOL_LEGACY_RUN_DIR="$TEST_TMP/legacy-run"
	export PROXYPOOL_LS_PROG="$TEST_TMP/ls-metadata"
	export PROXYPOOL_FIREWALL_SAFETY_TEMPLATE="$FIREWALL_SAFETY_TEMPLATE_FILE"
	export PROXYPOOL_FIREWALL_TRANSACTION_HELPER="$FIREWALL_TRANSACTION_HELPER_FILE"
	export PROXYPOOL_FIREWALL_TRANSACTION_DIR="$FIREWALL_TRANSACTION_DIR"
	export PROXYPOOL_LAN_ISOLATION="$LAN_ISOLATION_HELPER_FILE"
	export PROXYPOOL_LEGACY_GATE="$LEGACY_GATE_FILE"

	procd_open_service() {
		printf 'procd:service-open:%s\n' "$*" >>"$TRACE_FILE"
		: >"$PROCD_TRANSACTION_FILE"
		rm -f "$PROCD_STAGED_FILE"
	}
	procd_open_instance() {
		printf 'procd:open:%s\n' "$*" >>"$TRACE_FILE"
		[ "${PROXYPOOL_TEST_FAIL_PROCD_STEP-}" != open_instance ] || return 1
		printf '%s\n' "$1" >"$PROCD_STAGED_FILE"
	}
	procd_set_param() {
		printf 'procd:param:%s\n' "$*" >>"$TRACE_FILE"
		[ "${PROXYPOOL_TEST_FAIL_PROCD_STEP-}" != "param:${1-}" ]
	}
	procd_close_instance() {
		printf 'procd:close:%s\n' "$*" >>"$TRACE_FILE"
		[ "${PROXYPOOL_TEST_FAIL_PROCD_STEP-}" != close_instance ]
	}
	require_test_wal() {
		if [ -n "${PROXYPOOL_TEST_REQUIRE_ACTIVATED-}" ]; then
			[ -f "$ACTIVATED_BACKEND_FILE" ] && [ "$(cat "$ACTIVATED_BACKEND_FILE")" = "$PROXYPOOL_TEST_REQUIRE_ACTIVATED" ] || {
				printf 'activated:missing-before-procd\n' >>"$TRACE_FILE"
				return 96
			}
		fi
		[ -n "${PROXYPOOL_TEST_REQUIRE_WAL-}" ] || return 0
		[ -f "$CLEANUP_REQUIRED_FILE" ] && [ "$(cat "$CLEANUP_REQUIRED_FILE")" = "$PROXYPOOL_TEST_REQUIRE_WAL" ] || {
			printf 'wal:missing-before-procd\n' >>"$TRACE_FILE"
			return 97
		}
	}
	procd_close_service() {
		printf 'procd:service-close:%s\n' "$*" >>"$TRACE_FILE"
		require_test_wal || return $?
		if [ "${PROXYPOOL_TEST_SERVICE_SET-}" = ignore ]; then
			:
		elif [ -s "$PROCD_STAGED_FILE" ]; then
			if [ "${PROXYPOOL_TEST_SERVICE_SET-}" = wrong_instance ]; then
				printf 'different-instance\n' >"$PROCD_RUNNING_FILE"
			else
				cp "$PROCD_STAGED_FILE" "$PROCD_RUNNING_FILE"
			fi
		else
			rm -f "$PROCD_RUNNING_FILE"
		fi
		rm -f "$PROCD_STAGED_FILE" "$PROCD_TRANSACTION_FILE"
		[ "${PROXYPOOL_TEST_SERVICE_SET-}" != return_failure ]
	}
	procd_kill() {
		printf 'procd:kill:%s\n' "$*" >>"$TRACE_FILE"
		require_test_wal || return $?
		if [ -e "$PROCD_TRANSACTION_FILE" ]; then
			printf 'procd:transaction-corrupted\n' >>"$TRACE_FILE"
		fi
		[ "${PROXYPOOL_TEST_KILL-}" = noop ] || rm -f "$PROCD_RUNNING_FILE"
		return 0
	}
	procd_add_reload_trigger() { printf 'procd:trigger:%s\n' "$*" >>"$TRACE_FILE"; }
	mv() {
		last=
		for argument in "$@"; do last=$argument; done
		if [ "${PROXYPOOL_TEST_FAIL_MARKER_WRITE-}" = runtime ] && [ "$last" = "$MARKER_FILE" ]; then return 1; fi
		if [ "${PROXYPOOL_TEST_FAIL_MARKER_WRITE-}" = active ] && [ "$last" = "$ACTIVE_SNAPSHOT_FILE" ]; then return 1; fi
		if [ "${PROXYPOOL_TEST_FAIL_MARKER_WRITE-}" = activated ] && [ "$last" = "$ACTIVATED_BACKEND_FILE" ]; then return 1; fi
		if [ "${PROXYPOOL_TEST_FAIL_MARKER_WRITE-}" = cleanup ] && [ "$last" = "$CLEANUP_REQUIRED_FILE" ]; then return 1; fi
		command mv "$@"
	}
	rm() {
		for argument in "$@"; do
			case "${PROXYPOOL_TEST_FAIL_MARKER_CLEAR-}:$argument" in
				cleanup:"$CLEANUP_REQUIRED_FILE"|runtime:"$MARKER_FILE"|active:"$ACTIVE_SNAPSHOT_FILE") return 1 ;;
			esac
		done
		command rm "$@"
	}
	sed() {
		printf 'sed:%s\n' "$*" >>"$TRACE_FILE"
		sed_count=0
		[ ! -f "$SED_COUNT_FILE" ] || sed_count=$(cat "$SED_COUNT_FILE")
		sed_count=$((sed_count + 1))
		printf '%s\n' "$sed_count" >"$SED_COUNT_FILE"
		[ "${PROXYPOOL_TEST_SED_FAIL-}" != "$sed_count" ] || return 1
		command sed "$@"
	}
	start() {
		local start_result=0 service_result=0
		printf 'rc:start\n' >>"$TRACE_FILE"
		procd_open_service proxypool "$INIT"
		start_service || start_result=$?
		procd_close_service set || true
		service_started || service_result=$?
		[ "$service_result" -eq 0 ] || return "$service_result"
		return "$start_result"
	}
	stop() {
		printf 'rc:stop\n' >>"$TRACE_FILE"
		stop_service || true
		procd_kill proxypool || true
		service_stopped
	}
	sleep() { printf 'sleep:%s\n' "$*" >>"$TRACE_FILE"; }

	. "$INIT"
	# The historical transaction matrix still exercises unreachable V1 code as
	# rollback documentation. Production admission is enforced only in the new
	# explicit quarantine cases below; this local redefinition never reaches the
	# packaged init script.
	if [ "${PROXYPOOL_TEST_ENFORCE_LEGACY_QUARANTINE:-0}" != 1 ]; then
		legacy_quarantine() { return 0; }
	fi
	case "$action" in
		start) start ;;
		stop) stop ;;
		reload) reload_service ;;
		restart) restart ;;
		triggers) service_triggers ;;
	esac
)

run_start() { run_action start; }
run_stop() { run_action stop; }
run_reload() { run_action reload; }
run_restart() { run_action restart; }

write_legacy_cron() {
	printf '* * * * * %s run\n*/5 * * * * %s accrue\n' \
		"$WATCHDOG_FILE" "$TEST_TMP/lease" >"$CRONTAB_FILE"
}

assert_crontab_contains() {
	grep -Fq -- "$1" "$CRONTAB_FILE" || fail "missing crontab entry: $1"
}

assert_crontab_not_contains() {
	if grep -Fq -- "$1" "$CRONTAB_FILE"; then fail "unexpected crontab entry: $1"; fi
}

make_dangling_symlink() {
	link_path=$1
	target_path=$2
	if ln -s "$target_path" "$link_path" 2>/dev/null && [ -L "$link_path" ]; then
		return 0
	fi
	rm -f "$link_path"
	command -v powershell.exe >/dev/null 2>&1 || return 1
	command -v cygpath >/dev/null 2>&1 || return 1
	PROXYPOOL_TEST_NATIVE_LINK=$(cygpath -w "$link_path")
	PROXYPOOL_TEST_NATIVE_TARGET=$(cygpath -w "$target_path")
	export PROXYPOOL_TEST_NATIVE_LINK PROXYPOOL_TEST_NATIVE_TARGET
	powershell.exe -NoProfile -NonInteractive -Command '
		$ErrorActionPreference = "Stop"
		$link = $env:PROXYPOOL_TEST_NATIVE_LINK
		$target = $env:PROXYPOOL_TEST_NATIVE_TARGET
		New-Item -ItemType File -Path $target | Out-Null
		try {
			New-Item -ItemType SymbolicLink -Path $link -Target $target | Out-Null
		} finally {
			Remove-Item -LiteralPath $target -Force
		}
	' >/dev/null 2>&1 || return 1
	[ -L "$link_path" ]
}

# Phase 1 keeps V2 shadow available but quarantines every V1 lifecycle before
# firewall reconciliation, snapshot creation, cron edits, or process teardown.
reset_case
set_selector_only v1
PROXYPOOL_TEST_ENFORCE_LEGACY_QUARANTINE=1 expect_failure run_start
assert_contains 'gate:mutation init:start'
assert_not_contains_fragment 'l2:'
assert_no_backend_mutation
assert_generation_count 0
[ ! -e "$PERSISTENT_STATE_DIR" ] || fail 'quarantined V1 start created persistent state'

for lifecycle in stop reload restart; do
	reset_case
	set_activated_backend v1
	printf 'v1\n' >"$MARKER_FILE"
	: >"$LEGACY_RUNNING_FILE"
	set_selector_only v1
	PROXYPOOL_TEST_ENFORCE_LEGACY_QUARANTINE=1 expect_failure run_action "$lifecycle"
	assert_contains "gate:mutation init:$lifecycle"
	assert_not_contains_fragment 'l2:'
	assert_no_backend_mutation
	assert_file_line "$MARKER_FILE" v1
	[ -e "$LEGACY_RUNNING_FILE" ] || fail "quarantined V1 $lifecycle changed runtime evidence"
done

reset_case
set_selector v2_shadow
PROXYPOOL_TEST_ENFORCE_LEGACY_QUARANTINE=1 PROXYPOOL_TEST_REQUIRE_WAL=v2_shadow expect_success run_start
assert_not_contains_fragment 'gate:'
assert_contains_fragment 'procd:open:shadow-'
assert_file_line "$MARKER_FILE" v2_shadow

if [ "${PROXYPOOL_TEST_FOCUS-}" = legacy_quarantine ]; then
	echo 'proxypool init legacy quarantine matrix: PASS'
	exit 0
fi

# Firewall activation is a prerequisite for every mutation-capable admission
# path.  Missing package evidence and stale live runtime both leave an existing
# backend untouched; restart must check before its stop half.
reset_case
rm -f "$FIREWALL_SAFETY_TEMPLATE_FILE"
set_selector_only v1
expect_success run_start
assert_no_backend_mutation

reset_case
set_selector_only v1
PROXYPOOL_TEST_FIREWALL_RUNTIME=stale expect_success run_start
assert_contains 'safety:activation-current'
assert_contains 'l2:readiness'
assert_contains 'safety:activation-runtime-current'
assert_before 'safety:activation-current' 'l2:readiness'
assert_before 'l2:readiness' 'safety:activation-runtime-current'
assert_no_backend_mutation

for authority_fault in contract_drift contract_mode; do
	reset_case
	set_selector_only v1
	PROXYPOOL_TEST_FIREWALL_ACTIVATION="$authority_fault" expect_success run_start
	assert_contains 'safety:activation-current'
	assert_not_contains_fragment 'l2:readiness'
	assert_not_contains_fragment 'safety:activation-runtime-current'
	assert_no_backend_mutation
done

for metadata_fault in firewall_helper_owner firewall_helper_mode firewall_helper_links; do
	reset_case
	set_selector_only v1
	PROXYPOOL_TEST_METADATA_FAULT="$metadata_fault" expect_success run_start
	assert_not_contains_fragment 'safety:'
	assert_not_contains_fragment 'l2:readiness'
	assert_no_backend_mutation
done

reset_case
set_selector_only v1
PROXYPOOL_TEST_LAN_ISOLATION=failed expect_success run_start
assert_contains 'safety:activation-current'
assert_contains 'l2:readiness'
assert_not_contains_fragment 'safety:activation-runtime-current'
assert_no_backend_mutation

reset_case
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
PROXYPOOL_TEST_LAN_ISOLATION=failed expect_failure run_reload
assert_contains 'l2:readiness'
assert_not_contains_fragment 'safety:activation-runtime-current'
assert_no_backend_mutation
[ -e "$LEGACY_RUNNING_FILE" ] || fail 'failed L2 admission stopped V1 during reload'

reset_case
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
PROXYPOOL_TEST_FIREWALL_RUNTIME=stale expect_failure run_reload
assert_contains 'safety:activation-runtime-current'
assert_no_backend_mutation
[ -e "$LEGACY_RUNNING_FILE" ] || fail 'blocked reload stopped the existing V1 backend'

reset_case
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
PROXYPOOL_TEST_FIREWALL_RUNTIME=stale expect_failure run_restart
assert_contains 'safety:activation-runtime-current'
assert_no_backend_mutation
[ -e "$LEGACY_RUNNING_FILE" ] || fail 'restart stopped V1 before checking firewall runtime authority'

if [ "${PROXYPOOL_TEST_FOCUS-}" = activation_authority ]; then
	echo 'proxypool init activation authority matrix: PASS'
	exit 0
fi

# Follow-up security review matrix. Each case asserts a client-visible safety
# outcome (persistent quarantine or zero cross-backend mutation), not merely
# that a failure-injection fake was called.
reset_case
set_activated_backend v1
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
write_legacy_cron
PROXYPOOL_TEST_LEASE_FAIL=flush PROXYPOOL_TEST_REQUIRE_WAL=v1 expect_failure run_stop
assert_contains 'legacy:stop'
assert_crontab_not_contains "$WATCHDOG_FILE"
assert_crontab_not_contains "$TEST_TMP/lease"
assert_file_line "$MARKER_FILE" v1
assert_file_line "$CLEANUP_REQUIRED_FILE" v1
assert_file_line "$QUARANTINE_FILE" v1

reset_case
set_activated_backend v1
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
write_legacy_cron
PROXYPOOL_TEST_SED_FAIL=1 PROXYPOOL_TEST_REQUIRE_WAL=v1 expect_failure run_stop
assert_not_contains_fragment 'legacy:stop'
[ -e "$LEGACY_RUNNING_FILE" ] || fail 'first cron-edit failure stopped V1 before disabling its watchdog'
assert_crontab_contains "$WATCHDOG_FILE"
assert_crontab_contains "$TEST_TMP/lease"
assert_file_line "$MARKER_FILE" v1
assert_file_line "$CLEANUP_REQUIRED_FILE" v1
assert_file_line "$QUARANTINE_FILE" v1

reset_case
set_activated_backend v1
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
write_legacy_cron
PROXYPOOL_TEST_SED_FAIL=2 PROXYPOOL_TEST_REQUIRE_WAL=v1 expect_failure run_stop
assert_not_contains_fragment 'legacy:stop'
[ -e "$LEGACY_RUNNING_FILE" ] || fail 'second cron-edit failure stopped V1 before disabling its watchdog'
assert_crontab_not_contains "$WATCHDOG_FILE"
assert_crontab_contains "$TEST_TMP/lease"
assert_file_line "$MARKER_FILE" v1
assert_file_line "$CLEANUP_REQUIRED_FILE" v1
assert_file_line "$QUARANTINE_FILE" v1

reset_case
set_activated_backend v1
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
write_legacy_cron
PROXYPOOL_TEST_CRON_FAIL=restart PROXYPOOL_TEST_REQUIRE_WAL=v1 expect_failure run_stop
assert_not_contains_fragment 'legacy:stop'
[ -e "$LEGACY_RUNNING_FILE" ] || fail 'cron restart failure stopped V1 before the disabled schedule was loaded'
assert_crontab_not_contains "$WATCHDOG_FILE"
assert_crontab_not_contains "$TEST_TMP/lease"
assert_file_line "$MARKER_FILE" v1
assert_file_line "$CLEANUP_REQUIRED_FILE" v1
assert_file_line "$QUARANTINE_FILE" v1

# Existing persistent evidence is authority-bearing. A root process must not
# repair and then trust a pre-positioned non-root-owned or over-broad object.
for metadata_fault in state_owner state_mode activated_owner activated_mode activated_links; do
	reset_case
	set_activated_backend v1
	set_selector_only v1
	PROXYPOOL_TEST_METADATA_FAULT="$metadata_fault" expect_failure run_start
	assert_no_backend_mutation
	assert_file_line "$ACTIVATED_BACKEND_FILE" v1
done

for metadata_fault in cleanup_owner cleanup_mode cleanup_links; do
	reset_case
	set_activated_backend v1
	printf 'v1\n' >"$CLEANUP_REQUIRED_FILE"
	printf 'v1\n' >"$MARKER_FILE"
	: >"$LEGACY_RUNNING_FILE"
	set_selector_only v1
	PROXYPOOL_TEST_METADATA_FAULT="$metadata_fault" expect_failure run_stop
	assert_no_backend_mutation
	assert_file_line "$CLEANUP_REQUIRED_FILE" v1
	assert_file_line "$MARKER_FILE" v1
done

# Directory link counts are filesystem-dependent and must not be constrained
# to one. Authority-bearing regular marker files, however, may not have a
# second hard link that permits mutation through another pathname.
reset_case
set_activated_backend v1
set_selector_only v1
PROXYPOOL_TEST_METADATA_FAULT=state_links expect_success run_start
assert_contains 'legacy:start'

reset_case
set_activated_backend v1
set_selector_only v1
hardlink_alias="$TEST_TMP/activated-backend-hardlink"
if ln "$ACTIVATED_BACKEND_FILE" "$hardlink_alias" 2>/dev/null; then
	expect_failure run_start
	assert_no_backend_mutation
	assert_file_line "$ACTIVATED_BACKEND_FILE" v1
	rm -f "$hardlink_alias"
else
	echo 'SKIP: activated-backend hardlink assertion requires hardlink support'
fi

# Recovery is backend-specific. Opposite-backend evidence blocks before any
# legacy/procd mutation and leaves the write-ahead log intact for inspection.
reset_case
set_activated_backend v1
printf 'v1\n' >"$CLEANUP_REQUIRED_FILE"
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector_only v1
expect_failure run_stop
assert_no_backend_mutation
assert_file_line "$CLEANUP_REQUIRED_FILE" v1
assert_file_line "$MARKER_FILE" v1
[ -s "$PROCD_RUNNING_FILE" ] || fail 'V1 recovery destroyed opposite procd evidence'

reset_case
set_activated_backend v2_shadow
printf 'v2_shadow\n' >"$CLEANUP_REQUIRED_FILE"
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
mkdir "$TEST_TMP/legacy-run"
set_selector_only v2_shadow
expect_failure run_stop
assert_no_backend_mutation
assert_file_line "$CLEANUP_REQUIRED_FILE" v2_shadow
assert_file_line "$MARKER_FILE" v2_shadow
[ -d "$TEST_TMP/legacy-run" ] || fail 'V2 recovery destroyed opposite legacy evidence'
[ -s "$PROCD_RUNNING_FILE" ] || fail 'blocked V2 recovery killed its procd instance'

reset_case
set_activated_backend v2_shadow
printf 'v2_shadow\n' >"$CLEANUP_REQUIRED_FILE"
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
: >"$TEST_TMP/legacy-run"
set_selector_only v2_shadow
expect_failure run_stop
assert_no_backend_mutation
assert_file_line "$CLEANUP_REQUIRED_FILE" v2_shadow
assert_file_line "$MARKER_FILE" v2_shadow
[ -f "$TEST_TMP/legacy-run" ] || fail 'V2 recovery destroyed regular-file legacy evidence'
[ -s "$PROCD_RUNNING_FILE" ] || fail 'regular-file legacy evidence did not block V2 recovery'

reset_case
set_activated_backend v2_shadow
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector_only v2_shadow
if make_dangling_symlink "$TEST_TMP/legacy-run" "$TEST_TMP/missing-legacy-run-target"; then
	expect_failure run_stop
	assert_no_backend_mutation
	assert_file_line "$MARKER_FILE" v2_shadow
	[ -L "$TEST_TMP/legacy-run" ] || fail 'V2 stop destroyed dangling legacy runtime evidence'
	[ -s "$PROCD_RUNNING_FILE" ] || fail 'dangling legacy evidence did not block V2 stop'
else
	echo 'SKIP: V2 dangling legacy runtime assertion requires symlink support'
fi

# This V1 comparison narrows the admission window by running after xl2tpd
# preparation. Task 7.5 still needs a writer lock to close the remaining race
# between the comparison and the actual legacy start command.
reset_case
PROXYPOOL_TEST_MUTATE_LEGACY_ON_XL2TPD_DISABLE=1 PROXYPOOL_TEST_REQUIRE_WAL=v1 expect_failure run_start
assert_contains 'xl2tpd:disable'
assert_not_contains_fragment 'legacy:start'
assert_file_line "$ACTIVATED_BACKEND_FILE" v1
assert_file_line "$CLEANUP_REQUIRED_FILE" v1
assert_file_line "$QUARANTINE_FILE" v1

if [ "${PROXYPOOL_TEST_FOCUS-}" = task6_followup ]; then
	echo 'proxypool init Task6 follow-up matrix: PASS'
	exit 0
fi

# Missing selector is compatibility-only: a genuine V1 file starts after an
# explicit any-instance absence query.
reset_case
expect_success run_start
assert_contains_fragment 'ctl:select:'
assert_contains_fragment 'ctl:classify:'
assert_contains 'ctl:procd:any'
assert_before 'ctl:procd:any' 'legacy:start'
assert_contains 'legacy:start'
assert_file_line "$MARKER_FILE" v1
assert_file_line "$ACTIVATED_BACKEND_FILE" v1
[ ! -e "$CLEANUP_REQUIRED_FILE" ] || fail 'successful V1 bootstrap retained cleanup-required'
case "$(uname -s)" in
	Linux*)
		[ "$(stat -c '%a' "$PERSISTENT_STATE_DIR")" = 700 ] || fail 'persistent state directory is not mode 0700'
		[ "$(stat -c '%a' "$ACTIVATED_BACKEND_FILE")" = 600 ] || fail 'activated-backend is not mode 0600'
		;;
	*) echo 'SKIP: persistent state mode assertion requires a POSIX filesystem' ;;
esac
assert_not_contains "ctl:classify:$LEGACY_CONFIG_FILE"
assert_not_contains_fragment "--config $LEGACY_CONFIG_FILE"

# A fresh image selects V2 from its validated snapshot, then gives the daemon
# the persistent V2 source so LuCI mutations survive service restarts.
reset_case
set_selector v2_shadow
expect_success run_start
assert_contains_fragment 'procd:open:shadow-'
assert_contains_fragment "procd:param:command /usr/sbin/proxypoold --config $V2_CONFIG_FILE"
assert_contains_fragment "--state $PERSISTENT_STATE_DIR/runtime-v2.json --socket /var/run/proxypoold.sock --live"
assert_not_contains_fragment '--shadow'
instance=$(cat "$PROCD_RUNNING_FILE")
assert_contains "ctl:procd:$instance"
assert_file_line "$MARKER_FILE" v2_shadow
[ -f "$ACTIVE_SNAPSHOT_FILE" ] || fail 'missing active snapshot marker'
active_snapshot=$(cat "$ACTIVE_SNAPSHOT_FILE")
[ -f "$active_snapshot/proxypool" ] || fail 'active V2 snapshot was not retained'

# Selector and selected file are bound in one generation. A live selector
# mutation during config classification cannot redirect the current start.
reset_case
set_selector v2_shadow
PROXYPOOL_TEST_MUTATE_SELECTOR_DURING_CLASSIFY=1 expect_success run_start
assert_file_line "$MARKER_FILE" v2_shadow
grep -Fq "runtime_backend 'v1'" "$SELECTOR_FILE" || fail 'fake did not mutate live selector'

# An unmarked legacy runtime directory means this is not a fresh V2 image.
# First activation must not overlap an unexplained V1 runtime.
reset_case
mkdir "$TEST_TMP/legacy-run"
set_selector v2_shadow
expect_failure run_start
assert_not_contains_fragment 'legacy:'
assert_not_contains_fragment 'procd:open:'
assert_not_contains_fragment 'procd:service-close:'

# Missing/unknown selector states and a missing-selector V2 file fail before
# any legacy or procd backend mutation.
reset_case
set_selector unknown
expect_failure run_start
assert_not_contains_fragment 'legacy:'
assert_not_contains_fragment 'procd:open:'
[ ! -e "$MARKER_FILE" ] || fail 'invalid selector published a marker'

reset_case
set_selector directory
expect_failure run_start
assert_not_contains_fragment 'legacy:'
assert_not_contains_fragment 'procd:open:'

reset_case
cp "$V2_CONFIG_FILE" "$LEGACY_CONFIG_FILE"
expect_failure run_start
assert_not_contains_fragment 'legacy:'
assert_not_contains_fragment 'procd:open:'

# Exact stdout framing is part of all helper protocols.
reset_case
PROXYPOOL_TEST_SELECTOR_OUTPUT=bad expect_failure run_start
assert_not_contains_fragment 'legacy:'
assert_not_contains_fragment 'procd:open:'

reset_case
PROXYPOOL_TEST_SELECTOR_OUTPUT=no_newline expect_failure run_start
assert_not_contains_fragment 'legacy:'
assert_not_contains_fragment 'procd:open:'

# Cross-backend changes are blocked until the persistent fail-closed baseline
# exists. The open procd transaction is aborted before it can mutate either
# side.
reset_case
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector v2_shadow
expect_failure run_start
assert_not_contains_fragment 'legacy:'
assert_not_contains_fragment 'procd:open:'
assert_not_contains_fragment 'procd:service-close:'
assert_file_line "$MARKER_FILE" v1

reset_case
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
old_snapshot="$SNAPSHOT_ROOT/generation.old"
mkdir -p "$old_snapshot"
cp "$V2_CONFIG_FILE" "$old_snapshot/proxypool"
printf '%s\n' "$old_snapshot" >"$ACTIVE_SNAPSHOT_FILE"
set_activated_backend v2_shadow
set_selector v1
expect_failure run_start
assert_not_contains_fragment 'legacy:'
assert_not_contains_fragment 'procd:open:'
assert_not_contains_fragment 'procd:service-close:'
assert_file_line "$MARKER_FILE" v2_shadow

reset_case
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector v2_shadow
expect_failure run_reload
assert_not_contains_fragment 'legacy:'
assert_not_contains_fragment 'procd:'
assert_file_line "$MARKER_FILE" v1

# Same-backend V1 restart remains available and never overlaps a procd
# instance. Failure retains the old evidence.
reset_case
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector v1
expect_success run_start
assert_before 'ctl:procd:any' 'legacy:stop'
assert_before 'legacy:stop' 'procd:service-close:set'
assert_before 'procd:service-close:set' 'legacy:start'
[ "$(grep -Fxc 'ctl:procd:any' "$TRACE_FILE")" -eq 2 ] || fail 'V1 restart did not query procd both before and after commit'
assert_file_line "$MARKER_FILE" v1

reset_case
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
printf 'unexpected-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector v1
PROXYPOOL_TEST_SERVICE_SET=ignore expect_failure run_start
assert_not_contains_fragment 'legacy:'
assert_not_contains_fragment 'legacy:start'
assert_not_contains_fragment 'procd:service-close:'
assert_file_line "$MARKER_FILE" v1

# The V1 visible start failure is rolled back and no fresh marker is created.
reset_case
set_selector v1
PROXYPOOL_TEST_LEGACY_FAIL=start expect_failure run_start
assert_contains 'legacy:start'
assert_contains 'legacy:stop'
[ ! -e "$MARKER_FILE" ] || fail 'failed fresh V1 start published a marker'

reset_case
set_selector v1
PROXYPOOL_TEST_LEGACY_FAIL=start_and_stop expect_failure run_start
assert_file_line "$QUARANTINE_FILE" v1

reset_case
set_selector v1
PROXYPOOL_TEST_FAIL_MARKER_WRITE=runtime PROXYPOOL_TEST_LEGACY_FAIL=stop expect_failure run_start
assert_file_line "$QUARANTINE_FILE" v1

# Only an exact, query-confirmed procd instance commits V2. procd helper return
# codes and procd_kill return codes are never treated as state evidence.
reset_case
set_selector v2_shadow
PROXYPOOL_TEST_SERVICE_SET=ignore expect_failure run_start
[ ! -e "$MARKER_FILE" ] || fail 'ignored service-set published V2 marker'

reset_case
set_selector v2_shadow
PROXYPOOL_TEST_SERVICE_SET=wrong_instance expect_failure run_start
[ ! -e "$MARKER_FILE" ] || fail 'wrong instance published V2 marker'
assert_contains 'procd:kill:proxypool'
assert_generation_count 0

reset_case
set_selector v2_shadow
PROXYPOOL_TEST_QUERY=unknown expect_failure run_start
[ ! -e "$MARKER_FILE" ] || fail 'unknown procd state published V2 marker'
assert_not_contains_fragment 'procd:open:'
assert_not_contains_fragment 'procd:service-close:'

reset_case
set_selector v2_shadow
PROXYPOOL_TEST_QUERY=unknown_after_first expect_failure run_start
[ ! -e "$MARKER_FILE" ] || fail 'post-commit unknown procd state published V2 marker'
assert_contains_fragment 'procd:open:shadow-'
assert_contains 'procd:service-close:set'

reset_case
set_selector v2_shadow
PROXYPOOL_TEST_QUERY=bad_output expect_failure run_start
[ ! -e "$MARKER_FILE" ] || fail 'malformed procd state published V2 marker'

# A global query has no exact instance identity, so "running" is malformed
# even if an old runtime marker would otherwise allow a same-backend restart.
reset_case
set_selector v2_shadow
printf 'v2_shadow\n' >"$MARKER_FILE"
PROXYPOOL_TEST_QUERY=global_running expect_failure run_start
assert_no_backend_mutation
assert_file_line "$MARKER_FILE" v2_shadow

reset_case
set_selector v2_shadow
PROXYPOOL_TEST_FAIL_PROCD_STEP='param:stdout' PROXYPOOL_TEST_KILL=noop expect_failure run_start
assert_contains 'procd:kill:proxypool'
assert_contains 'ctl:procd:any'
[ ! -e "$MARKER_FILE" ] || fail 'partial procd transaction published V2 marker'

reset_case
set_selector v2_shadow
PROXYPOOL_TEST_FAIL_MARKER_WRITE=runtime expect_failure run_start
assert_contains 'procd:kill:proxypool'
assert_contains 'ctl:procd:any'
[ ! -e "$MARKER_FILE" ] || fail 'failed marker publication left runtime evidence'
[ ! -e "$PROCD_RUNNING_FILE" ] || fail 'failed marker publication left shadow running'

# Disabled same-backend starts clear evidence only after confirmed global
# procd absence. Query uncertainty or a lingering instance retains evidence.

# A markerless live V1 upgrade is recognized only through a genuine V1
# selector/config plus concrete legacy runtime evidence. Stop/disable must not
# leave that runtime behind, and rc.common must report teardown failures via
# service_stopped (the final hook in the real stop sequence).
reset_case
mkdir "$TEST_TMP/legacy-run"
: >"$LEGACY_RUNNING_FILE"
PROXYPOOL_TEST_REQUIRE_ACTIVATED=v1 PROXYPOOL_TEST_REQUIRE_WAL=v1 expect_success run_stop
assert_before 'legacy:stop' 'procd:kill:proxypool'
[ ! -e "$LEGACY_RUNNING_FILE" ] || fail 'markerless V1 stop left legacy runtime running'

reset_case
mkdir "$TEST_TMP/legacy-run"
: >"$LEGACY_RUNNING_FILE"
PROXYPOOL_TEST_LEGACY_FAIL=stop expect_failure run_stop
assert_contains 'legacy:stop'
[ -e "$LEGACY_RUNNING_FILE" ] || fail 'failed markerless V1 stop lost runtime evidence'

reset_case
mkdir "$TEST_TMP/legacy-run"
: >"$LEGACY_RUNNING_FILE"
write_legacy_config 0
PROXYPOOL_TEST_REQUIRE_ACTIVATED=v1 PROXYPOOL_TEST_REQUIRE_WAL=v1 expect_success run_start
assert_before 'legacy:stop' 'procd:service-close:set'
assert_not_contains_fragment 'legacy:start'
[ ! -e "$LEGACY_RUNNING_FILE" ] || fail 'disabled markerless V1 left legacy runtime running'
[ ! -e "$MARKER_FILE" ] || fail 'disabled markerless V1 published a backend marker'

reset_case
mkdir "$TEST_TMP/legacy-run"
: >"$LEGACY_RUNNING_FILE"
write_legacy_config 0
PROXYPOOL_TEST_LEGACY_FAIL=stop expect_failure run_start
assert_contains 'legacy:stop'
[ -e "$LEGACY_RUNNING_FILE" ] || fail 'failed markerless V1 disable lost runtime evidence'

reset_case
mkdir "$TEST_TMP/legacy-run"
: >"$LEGACY_RUNNING_FILE"
set_selector v2_shadow
expect_failure run_stop
assert_not_contains_fragment 'legacy:'
[ -e "$LEGACY_RUNNING_FILE" ] || fail 'non-V1 stop mutated markerless legacy evidence'
assert_generation_count 0

reset_case
mkdir "$TEST_TMP/legacy-run"
: >"$LEGACY_RUNNING_FILE"
PROXYPOOL_TEST_FAIL_MARKER_WRITE=activated expect_failure run_stop
assert_no_backend_mutation
[ -e "$LEGACY_RUNNING_FILE" ] || fail 'failed V1 preclaim mutated markerless stop target'
[ ! -e "$ACTIVATED_BACKEND_FILE" ] || fail 'failed markerless stop preclaim published activation'

reset_case
mkdir "$TEST_TMP/legacy-run"
: >"$LEGACY_RUNNING_FILE"
write_legacy_config 0
PROXYPOOL_TEST_FAIL_MARKER_WRITE=activated expect_failure run_start
assert_no_backend_mutation
[ -e "$LEGACY_RUNNING_FILE" ] || fail 'failed V1 preclaim mutated markerless disable target'
[ ! -e "$ACTIVATED_BACKEND_FILE" ] || fail 'failed markerless disable preclaim published activation'

reset_case
mkdir "$TEST_TMP/legacy-run"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v2_shadow
expect_failure run_stop
assert_no_backend_mutation
[ -e "$LEGACY_RUNNING_FILE" ] || fail 'unactivated V2 selector mutated markerless V1 runtime'
[ ! -e "$ACTIVATED_BACKEND_FILE" ] || fail 'unactivated V2 stop claimed ownership'

reset_case
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector v2_shadow
write_v2_config v2_shadow 0
expect_success run_start
[ ! -e "$MARKER_FILE" ] || fail 'confirmed disabled V2 retained marker'

reset_case
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector v2_shadow
write_v2_config v2_shadow 0
PROXYPOOL_TEST_SERVICE_SET=ignore expect_failure run_start
assert_file_line "$MARKER_FILE" v2_shadow

reset_case
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector v2_shadow
write_v2_config v2_shadow 0
PROXYPOOL_TEST_QUERY=unknown_after_first expect_failure run_start
assert_file_line "$MARKER_FILE" v2_shadow

# Stop follows the same evidence rule: even a successful procd_kill call may
# not clear the marker while the queried instance is present or unknown.
reset_case
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector v2_shadow
PROXYPOOL_TEST_KILL=noop expect_failure run_stop
assert_file_line "$MARKER_FILE" v2_shadow

reset_case
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector v2_shadow
PROXYPOOL_TEST_QUERY=unknown expect_failure run_stop
assert_file_line "$MARKER_FILE" v2_shadow

reset_case
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector v2_shadow
expect_success run_stop
[ ! -e "$MARKER_FILE" ] || fail 'confirmed stop retained marker'

# Any quarantine/transition artifact, including an unreadable path type, makes
# runtime state uncertain. Dual evidence is preserved without V1 cleanup.
reset_case
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'v1\n' >"$QUARANTINE_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector v2_shadow
expect_failure run_start
assert_not_contains_fragment 'legacy:'
assert_file_line "$MARKER_FILE" v2_shadow
assert_file_line "$QUARANTINE_FILE" v1

reset_case
printf 'v1\n' >"$MARKER_FILE"
mkdir "$QUARANTINE_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector v1
expect_failure run_start
assert_not_contains_fragment 'legacy:'
[ -d "$QUARANTINE_FILE" ] || fail 'unreadable quarantine evidence was removed'

reset_case
printf 'v1\n' >"$MARKER_FILE"
printf 'pending\n' >"$TRANSITION_FILE"
set_selector v1
expect_failure run_reload
assert_not_contains_fragment 'legacy:'
assert_file_line "$TRANSITION_FILE" pending

reset_case
printf 'v1\n' >"$TEST_TMP/symlink-target"
if ln -s "$TEST_TMP/symlink-target" "$MARKER_FILE" 2>/dev/null && [ -L "$MARKER_FILE" ]; then
	set_selector v1
	expect_failure run_start
	assert_not_contains_fragment 'legacy:'
else
	echo 'SKIP: runtime marker symlink assertion requires symlink support'
fi

# Marker framing is strict: missing newline and extra data are corrupt.
reset_case
printf v1 >"$MARKER_FILE"
set_selector v1
expect_failure run_start
assert_not_contains_fragment 'legacy:'

reset_case
printf 'v1\nextra\n' >"$MARKER_FILE"
set_selector v1
expect_failure run_start
assert_not_contains_fragment 'legacy:'

# Invalid declared V2 still runs the diagnostic shadow and ignores a partial
# enabled value. Same-backend reloads stay on their side.
reset_case
set_selector v2_shadow
write_v2_config v2_shadow_invalid 0
expect_success run_start
assert_file_line "$MARKER_FILE" v2_shadow

reset_case
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector v1
expect_success run_reload
assert_contains 'legacy:reload'
assert_not_contains_fragment 'legacy:stop'

reset_case
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector v1
PROXYPOOL_TEST_QUERY=unknown expect_failure run_reload
assert_not_contains_fragment 'legacy:'
assert_file_line "$MARKER_FILE" v1

reset_case
set_selector v2_shadow
expect_success run_start
first_instance=$(cat "$PROCD_RUNNING_FILE")
first_snapshot=$(cat "$ACTIVE_SNAPSHOT_FILE")
assert_generation_count 1
: >"$TRACE_FILE"
expect_success run_reload
second_instance=$(cat "$PROCD_RUNNING_FILE")
second_snapshot=$(cat "$ACTIVE_SNAPSHOT_FILE")
[ "$first_instance" != "$second_instance" ] || fail 'V2 reload reused its procd instance token'
[ "$first_snapshot" != "$second_snapshot" ] || fail 'V2 reload reused its immutable snapshot'
[ ! -e "$first_snapshot" ] || fail 'successful V2 reload retained the superseded snapshot'
[ -f "$second_snapshot/proxypool" ] || fail 'successful V2 reload lost its active snapshot'
assert_generation_count 1
assert_contains 'rc:start'
assert_contains "ctl:procd:$second_instance"

# I: Every snapshot-builder rejection owns and removes the generation it
# created, including failures after the config copy has completed.
reset_case
set_selector v2_shadow
PROXYPOOL_TEST_SELECTOR_OUTPUT=failure expect_failure run_start
assert_generation_count 0

reset_case
set_selector v2_shadow
rm -f "$V2_CONFIG_FILE"
expect_failure run_start
assert_generation_count 0

reset_case
set_selector v2_shadow
write_v2_config bad_output 1
expect_failure run_start
assert_generation_count 0

reset_case
set_selector v2_shadow
PROXYPOOL_TEST_ENABLED_FAILURE=1 expect_failure run_start
assert_generation_count 0

# J: A valid snapshot is also discarded when admission rejects it before an
# instance is staged.
reset_case
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector v2_shadow
expect_failure run_start
assert_generation_count 0

reset_case
mkdir "$TEST_TMP/legacy-run"
set_selector v2_shadow
expect_failure run_start
assert_generation_count 0

reset_case
set_selector v2_shadow
PROXYPOOL_TEST_QUERY=unknown expect_failure run_start
assert_generation_count 0

# K: V1 and disabled paths never retain classifier-only generations, whether
# the legacy action succeeds, fails, or needs rollback.
reset_case
set_selector v1
expect_success run_start
assert_generation_count 0

reset_case
set_selector v1
PROXYPOOL_TEST_LEGACY_FAIL=start expect_failure run_start
assert_generation_count 0

reset_case
set_selector v1
PROXYPOOL_TEST_LEGACY_FAIL=start_and_stop expect_failure run_start
assert_generation_count 0

reset_case
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector v1
expect_success run_reload
assert_generation_count 0

reset_case
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector v1
write_legacy_config 0
expect_success run_start
assert_generation_count 0

reset_case
mkdir "$TEST_TMP/legacy-run"
: >"$LEGACY_RUNNING_FILE"
expect_success run_stop
assert_generation_count 0

# M: A V2 candidate is removed only after absence is confirmed. Any exact or
# global query uncertainty, or a still-present instance, keeps the candidate.
reset_case
set_selector v2_shadow
PROXYPOOL_TEST_SERVICE_SET=ignore expect_failure run_start
assert_generation_count 0

reset_case
set_selector v2_shadow
PROXYPOOL_TEST_QUERY=unknown_after_first expect_failure run_start
assert_generation_count 1

reset_case
set_selector v2_shadow
PROXYPOOL_TEST_FAIL_PROCD_STEP='param:stdout' expect_failure run_start
assert_generation_count 0

reset_case
set_selector v2_shadow
PROXYPOOL_TEST_FAIL_PROCD_STEP='param:stdout' PROXYPOOL_TEST_KILL=noop expect_failure run_start
assert_generation_count 1

reset_case
set_selector v2_shadow
PROXYPOOL_TEST_FAIL_MARKER_WRITE=runtime expect_failure run_start
assert_generation_count 0

reset_case
set_selector v2_shadow
PROXYPOOL_TEST_FAIL_MARKER_WRITE=runtime PROXYPOOL_TEST_KILL=noop expect_failure run_start
assert_generation_count 1

# N: Once global procd absence is confirmed, stop and disable safely sweep all
# managed generations. A present/unknown service retains both data and pointer.
reset_case
old_one=$(make_managed_generation ABC123)
old_two=$(make_managed_generation DEF456)
printf '%s\n' "$old_one" >"$ACTIVE_SNAPSHOT_FILE"
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector v2_shadow
expect_success run_stop
assert_generation_count 0
[ ! -e "$ACTIVE_SNAPSHOT_FILE" ] || fail 'confirmed stop retained active snapshot pointer'

reset_case
old_one=$(make_managed_generation ABC123)
old_two=$(make_managed_generation DEF456)
printf '%s\n' "$old_one" >"$ACTIVE_SNAPSHOT_FILE"
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector v2_shadow
PROXYPOOL_TEST_KILL=noop expect_failure run_stop
assert_generation_count 2
assert_file_line "$ACTIVE_SNAPSHOT_FILE" "$old_one"

reset_case
old_one=$(make_managed_generation ABC123)
old_two=$(make_managed_generation DEF456)
printf '%s\n' "$old_one" >"$ACTIVE_SNAPSHOT_FILE"
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector v2_shadow
PROXYPOOL_TEST_QUERY=unknown expect_failure run_stop
assert_generation_count 2
assert_file_line "$ACTIVE_SNAPSHOT_FILE" "$old_one"

reset_case
old_one=$(make_managed_generation ABC123)
old_two=$(make_managed_generation DEF456)
printf '%s\n' "$old_one" >"$ACTIVE_SNAPSHOT_FILE"
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector v2_shadow
write_v2_config v2_shadow 0
PROXYPOOL_TEST_SERVICE_SET=ignore expect_failure run_start
assert_generation_count 2
assert_file_line "$ACTIVE_SNAPSHOT_FILE" "$old_one"

reset_case
old_one=$(make_managed_generation ABC123)
old_two=$(make_managed_generation DEF456)
printf '%s\n' "$old_one" >"$ACTIVE_SNAPSHOT_FILE"
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector v2_shadow
write_v2_config v2_shadow 0
expect_success run_start
assert_generation_count 0
[ ! -e "$ACTIVE_SNAPSHOT_FILE" ] || fail 'confirmed disable retained active snapshot pointer'

# O: ACTIVE is only a best-effort GC pointer. Failure to publish it cannot
# roll back a query-confirmed healthy V2 backend or guess which snapshot to
# delete; a later confirmed stop can sweep both safely.
reset_case
old_one=$(make_managed_generation ABC123)
printf '%s\n' "$old_one" >"$ACTIVE_SNAPSHOT_FILE"
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector v2_shadow
PROXYPOOL_TEST_FAIL_MARKER_WRITE=active expect_success run_start
assert_file_line "$MARKER_FILE" v2_shadow
assert_file_line "$ACTIVE_SNAPSHOT_FILE" "$old_one"
[ -s "$PROCD_RUNNING_FILE" ] || fail 'ACTIVE write failure stopped a healthy V2 instance'
assert_generation_count 2
expect_success run_stop
assert_generation_count 0

# P: ACTIVE data is never trusted as a deletion target. Traversal, multiline,
# and symlink forms may cause conservative retention but cannot delete outside
# the managed generation namespace.
reset_case
outside_dir="$TEST_TMP/outside-generation"
mkdir "$outside_dir"
: >"$outside_dir/sentinel"
old_one=$(make_managed_generation ABC123)
printf '%s/../..%s\n' "$old_one" "/outside-generation" >"$ACTIVE_SNAPSHOT_FILE"
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector v2_shadow
expect_success run_start
[ -f "$outside_dir/sentinel" ] || fail 'traversal ACTIVE pointer deleted outside snapshot root'
[ -d "$old_one" ] || fail 'invalid traversal pointer authorized generation deletion'

reset_case
old_one=$(make_managed_generation ABC123)
printf '%s\n%s\n' "$old_one" "$TEST_TMP/outside-generation" >"$ACTIVE_SNAPSHOT_FILE"
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector v2_shadow
expect_success run_start
[ -d "$old_one" ] || fail 'multiline ACTIVE pointer authorized generation deletion'

reset_case
outside_pointer="$TEST_TMP/outside-active-pointer"
printf '%s\n' "$TEST_TMP/outside-generation" >"$outside_pointer"
if ln -s "$outside_pointer" "$ACTIVE_SNAPSHOT_FILE" 2>/dev/null && [ -L "$ACTIVE_SNAPSHOT_FILE" ]; then
	printf 'v2_shadow\n' >"$MARKER_FILE"
	printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
	set_selector v2_shadow
	expect_success run_start
	[ -f "$outside_pointer" ] || fail 'symlink ACTIVE marker deleted its target'
	[ -L "$ACTIVE_SNAPSHOT_FILE" ] || fail 'symlink ACTIVE marker was followed or replaced'
else
	echo 'SKIP: active snapshot symlink assertion requires symlink support'
fi

# Q: Persistent activation is backend ownership, not a running flag. A fresh
# image may bootstrap only strict V1 (missing selector is V1 compatibility),
# and ownership must be atomically reserved before any backend mutation.
reset_case
write_legacy_config 0
set_selector_only missing
expect_success run_start
assert_no_backend_mutation
assert_file_line "$ACTIVATED_BACKEND_FILE" v1
[ ! -e "$MARKER_FILE" ] || fail 'disabled fresh V1 published runtime'
[ ! -e "$CLEANUP_REQUIRED_FILE" ] || fail 'disabled fresh V1 retained cleanup intent'

reset_case
PROXYPOOL_TEST_FAIL_MARKER_WRITE=activated expect_failure run_start
assert_no_backend_mutation
[ ! -e "$ACTIVATED_BACKEND_FILE" ] || fail 'failed activation write published ownership'
[ ! -e "$MARKER_FILE" ] || fail 'failed activation write published runtime'
[ ! -e "$CLEANUP_REQUIRED_FILE" ] || fail 'failed activation write published cleanup intent'

reset_case
set_selector_only v2_shadow
expect_failure run_start
assert_no_backend_mutation
[ ! -e "$ACTIVATED_BACKEND_FILE" ] || fail 'unactivated V2 start claimed ownership'
[ ! -e "$MARKER_FILE" ] || fail 'unactivated V2 start published runtime'

reset_case
set_activated_backend v1
set_selector_only v2_shadow
expect_failure run_start
assert_no_backend_mutation
assert_file_line "$ACTIVATED_BACKEND_FILE" v1

reset_case
set_activated_backend v2_shadow
set_selector_only v1
expect_failure run_start
assert_no_backend_mutation
assert_file_line "$ACTIVATED_BACKEND_FILE" v2_shadow

reset_case
set_activated_backend v1
printf 'v2_shadow\n' >"$MARKER_FILE"
set_selector_only v1
expect_failure run_start
assert_no_backend_mutation
assert_file_line "$ACTIVATED_BACKEND_FILE" v1
assert_file_line "$MARKER_FILE" v2_shadow

reset_case
mkdir -p "$PERSISTENT_STATE_DIR"
printf v1 >"$ACTIVATED_BACKEND_FILE"
set_selector_only v1
expect_failure run_start
assert_no_backend_mutation

reset_case
mkdir -p "$PERSISTENT_STATE_DIR"
printf 'v1\nextra\n' >"$ACTIVATED_BACKEND_FILE"
set_selector_only v1
expect_failure run_start
assert_no_backend_mutation

reset_case
mkdir -p "$PERSISTENT_STATE_DIR"
mkdir "$ACTIVATED_BACKEND_FILE"
set_selector_only v1
expect_failure run_start
assert_no_backend_mutation
[ -d "$ACTIVATED_BACKEND_FILE" ] || fail 'activated-backend directory was replaced'

reset_case
mkdir -p "$PERSISTENT_STATE_DIR"
printf 'v1\n' >"$TEST_TMP/activated-symlink-target"
if ln -s "$TEST_TMP/activated-symlink-target" "$ACTIVATED_BACKEND_FILE" 2>/dev/null && [ -L "$ACTIVATED_BACKEND_FILE" ]; then
	set_selector_only v1
	expect_failure run_start
	assert_no_backend_mutation
	[ -L "$ACTIVATED_BACKEND_FILE" ] || fail 'activated-backend symlink was replaced'
else
	echo 'SKIP: activated-backend symlink assertion requires symlink support'
fi

reset_case
printf 'not-a-directory\n' >"$PERSISTENT_STATE_DIR"
set_selector_only v1
expect_failure run_start
assert_no_backend_mutation
[ -f "$PERSISTENT_STATE_DIR" ] || fail 'persistent state non-directory was replaced'

reset_case
persistent_target="$TEST_TMP/persistent-state-target"
mkdir "$persistent_target"
printf 'v1\n' >"$persistent_target/activated-backend"
rm -rf "$PERSISTENT_STATE_DIR"
if ln -s "$persistent_target" "$PERSISTENT_STATE_DIR" 2>/dev/null && [ -L "$PERSISTENT_STATE_DIR" ]; then
	set_selector_only v1
	expect_failure run_start
	assert_no_backend_mutation
	[ -L "$PERSISTENT_STATE_DIR" ] || fail 'persistent state symlink was replaced'
else
	echo 'SKIP: persistent state directory symlink assertion requires symlink support'
fi

# A normal stop clears boot-local evidence but preserves ownership. After a
# selector change and simulated /var/run loss, activation still cross-blocks.
reset_case
set_activated_backend v1
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
PROXYPOOL_TEST_REQUIRE_WAL=v1 expect_success run_stop
assert_file_line "$ACTIVATED_BACKEND_FILE" v1
[ ! -e "$MARKER_FILE" ] || fail 'confirmed stop retained boot-local runtime marker'
[ ! -e "$CLEANUP_REQUIRED_FILE" ] || fail 'confirmed stop retained cleanup intent'
set_selector_only v2_shadow
: >"$TRACE_FILE"
expect_failure run_start
assert_no_backend_mutation
assert_file_line "$ACTIVATED_BACKEND_FILE" v1

# A preserved V2 selector is valid only when its activated ownership was
# restored with it; loss of /var/run does not erase that ownership.
reset_case
set_selector v2_shadow
PROXYPOOL_TEST_REQUIRE_WAL=v2_shadow expect_success run_start
assert_file_line "$ACTIVATED_BACKEND_FILE" v2_shadow
assert_file_line "$MARKER_FILE" v2_shadow
[ ! -e "$CLEANUP_REQUIRED_FILE" ] || fail 'successful V2 start retained cleanup intent'

# R: cleanup-required is a persistent write-ahead log. Failure to publish it
# aborts before legacy/procd mutation while preserving any successful V1
# ownership reservation.
reset_case
set_selector_only v1
PROXYPOOL_TEST_FAIL_MARKER_WRITE=cleanup expect_failure run_start
assert_no_backend_mutation
assert_file_line "$ACTIVATED_BACKEND_FILE" v1
[ ! -e "$MARKER_FILE" ] || fail 'WAL write failure published V1 runtime'
[ ! -e "$CLEANUP_REQUIRED_FILE" ] || fail 'failed WAL write left a valid intent'

reset_case
set_selector v2_shadow
PROXYPOOL_TEST_FAIL_MARKER_WRITE=cleanup expect_failure run_start
assert_no_backend_mutation
assert_file_line "$ACTIVATED_BACKEND_FILE" v2_shadow
[ ! -e "$MARKER_FILE" ] || fail 'WAL write failure published V2 runtime'

# Exact, malformed, symlink, or backend-mismatched cleanup evidence blocks
# start/reload without mutation. Only stop may recover an exact match.
reset_case
set_activated_backend v1
printf 'v1\n' >"$CLEANUP_REQUIRED_FILE"
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
expect_failure run_start
assert_no_backend_mutation
assert_file_line "$CLEANUP_REQUIRED_FILE" v1

reset_case
set_activated_backend v1
printf 'v1\n' >"$CLEANUP_REQUIRED_FILE"
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
expect_failure run_reload
assert_no_backend_mutation
assert_file_line "$CLEANUP_REQUIRED_FILE" v1

reset_case
set_activated_backend v1
printf 'v1\nextra\n' >"$CLEANUP_REQUIRED_FILE"
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
expect_failure run_start
assert_no_backend_mutation

reset_case
set_activated_backend v1
printf 'v2_shadow\n' >"$CLEANUP_REQUIRED_FILE"
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
expect_failure run_reload
assert_no_backend_mutation

reset_case
set_activated_backend v1
printf 'v1\n' >"$TEST_TMP/cleanup-symlink-target"
if ln -s "$TEST_TMP/cleanup-symlink-target" "$CLEANUP_REQUIRED_FILE" 2>/dev/null && [ -L "$CLEANUP_REQUIRED_FILE" ]; then
	printf 'v1\n' >"$MARKER_FILE"
	: >"$LEGACY_RUNNING_FILE"
	set_selector_only v1
	expect_failure run_start
	assert_no_backend_mutation
	[ -L "$CLEANUP_REQUIRED_FILE" ] || fail 'cleanup-required symlink was replaced'
else
	echo 'SKIP: cleanup-required symlink assertion requires symlink support'
fi

reset_case
set_activated_backend v1
printf 'v1\n' >"$CLEANUP_REQUIRED_FILE"
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
PROXYPOOL_TEST_REQUIRE_WAL=v1 expect_success run_stop
assert_contains 'legacy:stop'
assert_file_line "$ACTIVATED_BACKEND_FILE" v1
[ ! -e "$MARKER_FILE" ] || fail 'V1 recovery stop retained runtime marker'
[ ! -e "$CLEANUP_REQUIRED_FILE" ] || fail 'V1 recovery stop retained WAL after confirmed clean'

reset_case
set_activated_backend v2_shadow
printf 'v2_shadow\n' >"$CLEANUP_REQUIRED_FILE"
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector_only v2_shadow
PROXYPOOL_TEST_REQUIRE_WAL=v2_shadow expect_success run_stop
assert_contains 'procd:kill:proxypool'
assert_file_line "$ACTIVATED_BACKEND_FILE" v2_shadow
[ ! -e "$MARKER_FILE" ] || fail 'V2 recovery stop retained runtime marker'
[ ! -e "$CLEANUP_REQUIRED_FILE" ] || fail 'V2 recovery stop retained WAL after confirmed clean'

reset_case
set_activated_backend v2_shadow
printf 'corrupt\n' >"$CLEANUP_REQUIRED_FILE"
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector_only v2_shadow
expect_failure run_stop
assert_no_backend_mutation
[ -s "$PROCD_RUNNING_FILE" ] || fail 'corrupt recovery evidence allowed procd teardown'
assert_file_line "$MARKER_FILE" v2_shadow

reset_case
set_activated_backend v1
printf 'v1\n' >"$CLEANUP_REQUIRED_FILE"
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
PROXYPOOL_TEST_LEGACY_FAIL=stop PROXYPOOL_TEST_REQUIRE_WAL=v1 expect_failure run_stop
assert_file_line "$ACTIVATED_BACKEND_FILE" v1
assert_file_line "$MARKER_FILE" v1
assert_file_line "$CLEANUP_REQUIRED_FILE" v1
[ -e "$LEGACY_RUNNING_FILE" ] || fail 'failed V1 recovery lost legacy runtime evidence'

# Clearing the WAL is itself part of commit. A clean stop may clear boot-local
# runtime, but failure to remove persistent cleanup evidence remains visible.
reset_case
set_activated_backend v1
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
PROXYPOOL_TEST_FAIL_MARKER_CLEAR=cleanup PROXYPOOL_TEST_REQUIRE_WAL=v1 expect_failure run_stop
assert_file_line "$ACTIVATED_BACKEND_FILE" v1
[ ! -e "$MARKER_FILE" ] || fail 'clean stop with WAL-clear failure retained runtime marker'
assert_file_line "$CLEANUP_REQUIRED_FILE" v1

reset_case
set_selector v2_shadow
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
old_one=$(make_managed_generation ABC123)
printf '%s\n' "$old_one" >"$ACTIVE_SNAPSHOT_FILE"
PROXYPOOL_TEST_FAIL_MARKER_CLEAR=runtime PROXYPOOL_TEST_REQUIRE_WAL=v2_shadow expect_failure run_stop
assert_file_line "$MARKER_FILE" v2_shadow
assert_file_line "$CLEANUP_REQUIRED_FILE" v2_shadow
assert_file_line "$ACTIVE_SNAPSHOT_FILE" "$old_one"

reset_case
set_selector v2_shadow
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
old_one=$(make_managed_generation ABC123)
printf '%s\n' "$old_one" >"$ACTIVE_SNAPSHOT_FILE"
PROXYPOOL_TEST_FAIL_MARKER_CLEAR=active PROXYPOOL_TEST_REQUIRE_WAL=v2_shadow expect_failure run_stop
assert_file_line "$MARKER_FILE" v2_shadow
assert_file_line "$CLEANUP_REQUIRED_FILE" v2_shadow
assert_file_line "$ACTIVE_SNAPSHOT_FILE" "$old_one"

# S: C1 distinguishes occupancy from health. An exact configured-but-stopped
# instance is never committed; cleanup is attempted and only confirmed global
# absence permits volatile evidence/WAL/snapshot cleanup.
reset_case
set_selector v2_shadow
PROXYPOOL_TEST_EXACT_STATE=present PROXYPOOL_TEST_REQUIRE_WAL=v2_shadow expect_failure run_start
assert_contains 'procd:kill:proxypool'
assert_file_line "$ACTIVATED_BACKEND_FILE" v2_shadow
[ ! -e "$MARKER_FILE" ] || fail 'non-running exact instance published runtime'
[ ! -e "$CLEANUP_REQUIRED_FILE" ] || fail 'confirmed cleanup retained V2 WAL'
assert_generation_count 0

reset_case
set_selector v2_shadow
PROXYPOOL_TEST_EXACT_STATE=present PROXYPOOL_TEST_KILL=noop PROXYPOOL_TEST_REQUIRE_WAL=v2_shadow expect_failure run_start
assert_contains 'procd:kill:proxypool'
assert_file_line "$ACTIVATED_BACKEND_FILE" v2_shadow
assert_file_line "$CLEANUP_REQUIRED_FILE" v2_shadow
[ ! -e "$MARKER_FILE" ] || fail 'unclean non-running fresh V2 published runtime'
assert_generation_count 1

# A destructive same-backend failure cannot retain a successful runtime
# illusion after global absence is confirmed. Uncertain cleanup retains the
# old runtime plus WAL/quarantine for explicit stop recovery.
reset_case
set_activated_backend v2_shadow
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector_only v2_shadow
PROXYPOOL_TEST_FAIL_PROCD_STEP='param:stdout' PROXYPOOL_TEST_REQUIRE_WAL=v2_shadow expect_failure run_start
assert_file_line "$ACTIVATED_BACKEND_FILE" v2_shadow
[ ! -e "$MARKER_FILE" ] || fail 'confirmed failed V2 replacement retained runtime marker'
[ ! -e "$CLEANUP_REQUIRED_FILE" ] || fail 'confirmed failed V2 replacement retained WAL'

reset_case
set_activated_backend v2_shadow
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
set_selector_only v2_shadow
PROXYPOOL_TEST_FAIL_PROCD_STEP='param:stdout' PROXYPOOL_TEST_KILL=noop PROXYPOOL_TEST_REQUIRE_WAL=v2_shadow expect_failure run_start
assert_file_line "$MARKER_FILE" v2_shadow
assert_file_line "$CLEANUP_REQUIRED_FILE" v2_shadow
assert_generation_count 1

reset_case
set_activated_backend v1
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
PROXYPOOL_TEST_LEGACY_FAIL=start PROXYPOOL_TEST_REQUIRE_WAL=v1 expect_failure run_start
assert_file_line "$ACTIVATED_BACKEND_FILE" v1
[ ! -e "$MARKER_FILE" ] || fail 'clean failed V1 replacement retained runtime marker'
[ ! -e "$CLEANUP_REQUIRED_FILE" ] || fail 'clean failed V1 replacement retained WAL'

reset_case
set_activated_backend v1
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
PROXYPOOL_TEST_LEGACY_FAIL=stop PROXYPOOL_TEST_REQUIRE_WAL=v1 expect_failure run_start
assert_file_line "$MARKER_FILE" v1
assert_file_line "$CLEANUP_REQUIRED_FILE" v1
assert_file_line "$QUARANTINE_FILE" v1

# T: The classified V1 bytes are compared again at the tested pre-call
# checkpoints. Task 7.5 must still add a writer lock to eliminate mutation in
# the remaining compare-to-call window. If stop already completed, runtime is
# cleared and ownership remains for recovery.
reset_case
set_selector_only v1
PROXYPOOL_TEST_MUTATE_LEGACY_DURING_CLASSIFY=1 expect_failure run_start
assert_no_backend_mutation
assert_file_line "$ACTIVATED_BACKEND_FILE" v1
[ ! -e "$CLEANUP_REQUIRED_FILE" ] || fail 'pre-mutation V1 mismatch wrote WAL'
assert_generation_count 0

reset_case
set_activated_backend v1
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
PROXYPOOL_TEST_MUTATE_LEGACY_DURING_CLASSIFY=1 expect_failure run_stop
assert_no_backend_mutation
assert_file_line "$MARKER_FILE" v1
[ -e "$LEGACY_RUNNING_FILE" ] || fail 'V1 stop mutated backend after snapshot mismatch'

reset_case
set_activated_backend v1
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
PROXYPOOL_TEST_MUTATE_LEGACY_DURING_CLASSIFY=1 expect_failure run_reload
assert_no_backend_mutation
assert_file_line "$MARKER_FILE" v1

reset_case
set_activated_backend v1
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
PROXYPOOL_TEST_MUTATE_LEGACY_AFTER_STOP=1 PROXYPOOL_TEST_REQUIRE_WAL=v1 expect_failure run_start
assert_contains 'legacy:stop'
assert_not_contains_fragment 'legacy:start'
assert_file_line "$ACTIVATED_BACKEND_FILE" v1
[ ! -e "$MARKER_FILE" ] || fail 'post-stop V1 mutation retained runtime marker'
[ ! -e "$CLEANUP_REQUIRED_FILE" ] || fail 'post-stop V1 mutation retained WAL after clean stop'
[ ! -e "$LEGACY_RUNNING_FILE" ] || fail 'post-stop V1 mutation resurrected legacy runtime'

reset_case
set_selector_only v1
PROXYPOOL_TEST_LEGACY_FAIL=start PROXYPOOL_TEST_MUTATE_LEGACY_ON_START_FAILURE=1 PROXYPOOL_TEST_REQUIRE_WAL=v1 expect_failure run_start
assert_contains 'legacy:start'
[ "$(grep -Fxc 'legacy:stop' "$TRACE_FILE" || true)" -eq 0 ] || fail 'V1 rollback ignored changed live config'
assert_file_line "$ACTIVATED_BACKEND_FILE" v1
assert_file_line "$CLEANUP_REQUIRED_FILE" v1
assert_file_line "$QUARANTINE_FILE" v1

reset_case
set_activated_backend v1
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
PROXYPOOL_TEST_LEGACY_FAIL=reload PROXYPOOL_TEST_REQUIRE_WAL=v1 expect_failure run_reload
assert_contains 'legacy:reload'
assert_contains 'legacy:stop'
assert_file_line "$ACTIVATED_BACKEND_FILE" v1
[ ! -e "$MARKER_FILE" ] || fail 'clean failed V1 reload retained runtime marker'
[ ! -e "$CLEANUP_REQUIRED_FILE" ] || fail 'clean failed V1 reload retained WAL'

reset_case
set_activated_backend v1
printf 'v1\n' >"$MARKER_FILE"
: >"$LEGACY_RUNNING_FILE"
set_selector_only v1
PROXYPOOL_TEST_LEGACY_FAIL=reload_and_stop PROXYPOOL_TEST_REQUIRE_WAL=v1 expect_failure run_reload
assert_file_line "$MARKER_FILE" v1
assert_file_line "$CLEANUP_REQUIRED_FILE" v1
assert_file_line "$QUARANTINE_FILE" v1

# Successful backend commit followed by WAL-clear failure reports failure and
# leaves persistent cleanup evidence rather than claiming a fully clean start.
reset_case
set_selector_only v1
PROXYPOOL_TEST_FAIL_MARKER_CLEAR=cleanup PROXYPOOL_TEST_REQUIRE_WAL=v1 expect_failure run_start
assert_file_line "$ACTIVATED_BACKEND_FILE" v1
assert_file_line "$MARKER_FILE" v1
assert_file_line "$CLEANUP_REQUIRED_FILE" v1
[ -e "$LEGACY_RUNNING_FILE" ] || fail 'WAL-clear failure rolled back a committed V1 backend'

reset_case
expect_success run_action triggers
assert_contains 'procd:trigger:proxypool proxypool_v2 proxypool_runtime'

echo 'proxypool init backend transaction matrix: PASS'
