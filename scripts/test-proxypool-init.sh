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
LEGACY_CONFIG_FILE="$TEST_TMP/proxypool"
V2_CONFIG_FILE="$TEST_TMP/proxypool_v2"
SELECTOR_FILE="$TEST_TMP/proxypool_runtime"
CRONTAB_FILE="$TEST_TMP/crontab"
PID_FILE="$TEST_TMP/xl2tpd.pid"
PROCD_RUNNING_FILE="$TEST_TMP/procd-running"
PROCD_STAGED_FILE="$TEST_TMP/procd-staged"
PROCD_TRANSACTION_FILE="$TEST_TMP/procd-transaction"
LEGACY_RUNNING_FILE="$TEST_TMP/legacy-running"
QUERY_COUNT_FILE="$TEST_TMP/query-count"
export TRACE_FILE MARKER_FILE QUARANTINE_FILE TRANSITION_FILE ACTIVE_SNAPSHOT_FILE
export SNAPSHOT_ROOT LEGACY_CONFIG_FILE V2_CONFIG_FILE SELECTOR_FILE
export PROCD_RUNNING_FILE PROCD_STAGED_FILE PROCD_TRANSACTION_FILE LEGACY_RUNNING_FILE
export QUERY_COUNT_FILE

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
case "${1-}" in
	start)
		case "${PROXYPOOL_TEST_LEGACY_FAIL-}" in start|start_and_stop) exit 1 ;; esac
		: >"$LEGACY_RUNNING_FILE"
		;;
	stop)
		case "${PROXYPOOL_TEST_LEGACY_FAIL-}" in stop|start_and_stop) exit 1 ;; esac
		rm -f "$LEGACY_RUNNING_FILE"
		;;
esac'
make_fake "$TEST_TMP/lease" 'printf "lease:%s\n" "$*" >>"$TRACE_FILE"'
make_fake "$TEST_TMP/cron" 'printf "cron:%s\n" "$*" >>"$TRACE_FILE"'
make_fake "$TEST_TMP/xl2tpd" '
printf "xl2tpd:%s\n" "$*" >>"$TRACE_FILE"
[ "${1-}" = enabled ] && exit 0
exit 0'
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
			bad_output) printf "present unexpected\n"; exit 0 ;;
		esac
		if [ -n "$instance" ]; then
			if [ -f "$PROCD_RUNNING_FILE" ] && grep -Fqx "$instance" "$PROCD_RUNNING_FILE"; then
				printf "present\n"
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

assert_before() {
	first=$(grep -nF "$1" "$TRACE_FILE" | head -n1 | cut -d: -f1)
	second=$(grep -nF "$2" "$TRACE_FILE" | head -n1 | cut -d: -f1)
	[ -n "$first" ] && [ -n "$second" ] && [ "$first" -lt "$second" ] || fail "expected $1 before $2"
}

assert_file_line() {
	[ -f "$1" ] && [ "$(cat "$1")" = "$2" ] || fail "unexpected file content: $1"
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

set_selector() {
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

reset_case() {
	: >"$TRACE_FILE"
	: >"$CRONTAB_FILE"
	rm -f "$MARKER_FILE" "$TRANSITION_FILE" "$ACTIVE_SNAPSHOT_FILE"
	rm -rf "$QUARANTINE_FILE"
	rm -f "$PID_FILE" "$PROCD_RUNNING_FILE" "$PROCD_STAGED_FILE" "$PROCD_TRANSACTION_FILE" "$LEGACY_RUNNING_FILE" "$QUERY_COUNT_FILE"
	rm -rf "$SNAPSHOT_ROOT" "$TEST_TMP/legacy-run" "$SELECTOR_FILE"
	write_legacy_config 1
	write_v2_config v2_shadow 1
	set_selector missing
}

run_action() (
	set +e
	action=$1
	export PROXYPOOL_RUNTIME_MARKER="$MARKER_FILE"
	export PROXYPOOL_QUARANTINE_MARKER="$QUARANTINE_FILE"
	export PROXYPOOL_TRANSITION_MARKER="$TRANSITION_FILE"
	export PROXYPOOL_ACTIVE_SNAPSHOT_MARKER="$ACTIVE_SNAPSHOT_FILE"
	export PROXYPOOL_SNAPSHOT_ROOT="$SNAPSHOT_ROOT"
	export PROXYPOOL_LEGACY_CONFIG_FILE="$LEGACY_CONFIG_FILE"
	export PROXYPOOL_V2_CONFIG_FILE="$V2_CONFIG_FILE"
	export PROXYPOOL_SELECTOR_FILE="$SELECTOR_FILE"
	export PROXYPOOL_CLASSIFIER="$TEST_TMP/controller"
	export PROXYPOOL_LEGACY_PROG="$TEST_TMP/legacy"
	export PROXYPOOL_LEASE_PROG="$TEST_TMP/lease"
	export PROXYPOOL_CRON_INIT="$TEST_TMP/cron"
	export PROXYPOOL_XL2TPD_INIT="$TEST_TMP/xl2tpd"
	export PROXYPOOL_CRONTAB_ROOT="$CRONTAB_FILE"
	export PROXYPOOL_XL2TPD_PID="$PID_FILE"
	export PROXYPOOL_LEGACY_RUN_DIR="$TEST_TMP/legacy-run"

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
	procd_close_service() {
		printf 'procd:service-close:%s\n' "$*" >>"$TRACE_FILE"
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
		command mv "$@"
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
		local stop_result=0
		printf 'rc:stop\n' >>"$TRACE_FILE"
		stop_service || stop_result=$?
		procd_kill proxypool || true
		service_stopped || true
		return "$stop_result"
	}
	sleep() { printf 'sleep:%s\n' "$*" >>"$TRACE_FILE"; }

	. "$INIT"
	case "$action" in
		start) start ;;
		stop) stop ;;
		reload) reload_service ;;
		triggers) service_triggers ;;
	esac
)

run_start() { run_action start; }
run_stop() { run_action stop; }
run_reload() { run_action reload; }

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
assert_not_contains_fragment "$LEGACY_CONFIG_FILE"

# A fresh image selects V2 from its separate selector/config snapshot. The
# daemon never consumes either live source path.
reset_case
set_selector v2_shadow
expect_success run_start
assert_contains_fragment 'procd:open:shadow-'
assert_contains_fragment 'procd:param:command /usr/sbin/proxypoold --config '
assert_not_contains_fragment "--config $V2_CONFIG_FILE"
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
PROXYPOOL_TEST_KILL=noop expect_success run_stop
assert_file_line "$MARKER_FILE" v2_shadow

reset_case
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
PROXYPOOL_TEST_QUERY=unknown expect_success run_stop
assert_file_line "$MARKER_FILE" v2_shadow

reset_case
printf 'v2_shadow\n' >"$MARKER_FILE"
printf 'old-shadow\n' >"$PROCD_RUNNING_FILE"
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
: >"$TRACE_FILE"
expect_success run_reload
second_instance=$(cat "$PROCD_RUNNING_FILE")
[ "$first_instance" != "$second_instance" ] || fail 'V2 reload reused its procd instance token'
assert_contains 'rc:start'
assert_contains "ctl:procd:$second_instance"

reset_case
expect_success run_action triggers
assert_contains 'procd:trigger:proxypool proxypool_v2 proxypool_runtime'

echo 'proxypool init backend transaction matrix: PASS'
