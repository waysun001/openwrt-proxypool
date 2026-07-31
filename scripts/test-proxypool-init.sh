#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
INIT="$ROOT/proxypool-core/files/proxypool.init"
TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM
TRACE_FILE="$TEST_TMP/trace"
MARKER_FILE="$TEST_TMP/backend"
CRONTAB_FILE="$TEST_TMP/crontab"
PID_FILE="$TEST_TMP/xl2tpd.pid"
PROCD_RUNNING_FILE="$TEST_TMP/procd-running"
PROCD_STAGED_FILE="$TEST_TMP/procd-staged"
PROCD_TRANSACTION_FILE="$TEST_TMP/procd-transaction"
export TRACE_FILE

make_fake() {
	path=$1
	body=$2
	cat >"$path" <<EOF
#!/usr/bin/env sh
$body
EOF
	chmod +x "$path"
}

make_fake "$TEST_TMP/legacy" 'printf "legacy:%s\n" "$*" >>"$TRACE_FILE"'
make_fake "$TEST_TMP/lease" 'printf "lease:%s\n" "$*" >>"$TRACE_FILE"'
make_fake "$TEST_TMP/cron" 'printf "cron:%s\n" "$*" >>"$TRACE_FILE"'
make_fake "$TEST_TMP/xl2tpd" '
printf "xl2tpd:%s\n" "$*" >>"$TRACE_FILE"
[ "${1-}" = enabled ] && exit 0
exit 0'

assert_contains() {
	needle=$1
	if ! grep -Fqx "$needle" "$TRACE_FILE"; then
		printf 'missing trace line: %s\n' "$needle" >&2
		cat "$TRACE_FILE" >&2
		exit 1
	fi
}

assert_contains_fragment() {
	needle=$1
	if ! grep -Fq "$needle" "$TRACE_FILE"; then
		printf 'missing trace fragment: %s\n' "$needle" >&2
		cat "$TRACE_FILE" >&2
		exit 1
	fi
}

assert_not_contains_fragment() {
	needle=$1
	if grep -Fq "$needle" "$TRACE_FILE"; then
		printf 'unexpected trace fragment: %s\n' "$needle" >&2
		cat "$TRACE_FILE" >&2
		exit 1
	fi
}

assert_before() {
	first=$(grep -nF "$1" "$TRACE_FILE" | head -n1 | cut -d: -f1)
	second=$(grep -nF "$2" "$TRACE_FILE" | head -n1 | cut -d: -f1)
	if [ -z "$first" ] || [ -z "$second" ] || [ "$first" -ge "$second" ]; then
		printf 'expected %s before %s\n' "$1" "$2" >&2
		cat "$TRACE_FILE" >&2
		exit 1
	fi
}

reset_case() {
	: >"$TRACE_FILE"
	: >"$CRONTAB_FILE"
	rm -f "$MARKER_FILE" "$PID_FILE"
	rm -f "$PROCD_RUNNING_FILE" "$PROCD_STAGED_FILE" "$PROCD_TRANSACTION_FILE"
	rm -rf "$TEST_TMP/legacy-run"
}

run_action() (
	TEST_ACTION=$1
	TEST_BACKEND=$2
	TEST_ENABLED=${3-1}
	export PROXYPOOL_RUNTIME_MARKER="${PROXYPOOL_TEST_MARKER-$MARKER_FILE}"
	export PROXYPOOL_LEGACY_PROG="$TEST_TMP/legacy"
	export PROXYPOOL_LEASE_PROG="$TEST_TMP/lease"
	export PROXYPOOL_CRON_INIT="$TEST_TMP/cron"
	export PROXYPOOL_XL2TPD_INIT="$TEST_TMP/xl2tpd"
	export PROXYPOOL_CRONTAB_ROOT="$CRONTAB_FILE"
	export PROXYPOOL_XL2TPD_PID="$PID_FILE"
	export PROXYPOOL_LEGACY_RUN_DIR="$TEST_TMP/legacy-run"

	config_load() { printf 'config_load:%s\n' "$*" >>"$TRACE_FILE"; }
	config_get() {
		case "$3" in
			enabled) value=$TEST_ENABLED ;;
			runtime_backend)
				if [ "$TEST_BACKEND" = __missing__ ]; then value=$4; else value=$TEST_BACKEND; fi
				;;
			*) value=$4 ;;
		esac
		eval "$1=\$value"
	}
	procd_open_service() {
		printf 'procd:service-open:%s\n' "$*" >>"$TRACE_FILE"
		: >"$PROCD_TRANSACTION_FILE"
		rm -f "$PROCD_STAGED_FILE"
	}
	procd_open_instance() {
		printf 'procd:open:%s\n' "$*" >>"$TRACE_FILE"
		: >"$PROCD_STAGED_FILE"
		[ "${PROXYPOOL_TEST_FAIL_PROCD_STEP-}" != open_instance ]
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
		if [ -e "$PROCD_STAGED_FILE" ]; then
			: >"$PROCD_RUNNING_FILE"
		else
			rm -f "$PROCD_RUNNING_FILE"
		fi
		rm -f "$PROCD_STAGED_FILE" "$PROCD_TRANSACTION_FILE"
	}
	procd_kill() {
		printf 'procd:kill:%s\n' "$*" >>"$TRACE_FILE"
		if [ -e "$PROCD_TRANSACTION_FILE" ]; then
			printf 'procd:transaction-corrupted\n' >>"$TRACE_FILE"
			rm -f "$PROCD_STAGED_FILE" "$PROCD_TRANSACTION_FILE"
		fi
		rm -f "$PROCD_RUNNING_FILE"
	}
	start() {
		printf 'rc:start:%s\n' "$*" >>"$TRACE_FILE"
		procd_open_service proxypool "$INIT"
		start_service "$@" || true
		procd_close_service set
		service_started
	}
	stop() {
		printf 'rc:stop:%s\n' "$*" >>"$TRACE_FILE"
		result=0
		stop_service "$@" || result=$?
		procd_kill proxypool
		return "$result"
	}
	sleep() { printf 'sleep:%s\n' "$*" >>"$TRACE_FILE"; }

	. "$INIT"
	[ "$USE_PROCD" = 1 ]
	"$TEST_ACTION"
)

run_start() { run_action start "$1" "${2-1}"; }
run_stop() { run_action stop "$1" "${2-1}"; }
run_reload() { run_action reload_service "$1" "${2-1}"; }

# Explicit V1 preserves the legacy start path, including its historical
# xl2tpd and cron behavior.
reset_case
run_start v1
assert_contains 'legacy:start'
assert_contains 'xl2tpd:enabled'
assert_contains 'xl2tpd:stop'
assert_contains 'xl2tpd:disable'
assert_contains 'lease:boot'
assert_contains 'cron:restart'
[ "$(cat "$MARKER_FILE")" = v1 ]

# A marker write is part of startup atomicity. If it cannot be committed, the
# backend that was just started is rolled back instead of becoming orphaned.
reset_case
if PROXYPOOL_TEST_MARKER="$TEST_TMP/missing/backend" run_start v1; then
	echo 'V1 unexpectedly survived a marker write failure' >&2
	exit 1
fi
assert_contains 'legacy:start'
assert_contains 'legacy:stop'
assert_contains 'lease:flush'

reset_case
if PROXYPOOL_TEST_MARKER="$TEST_TMP/missing/backend" run_start v2_shadow; then
	echo 'shadow unexpectedly survived a marker write failure' >&2
	exit 1
fi
assert_contains 'procd:open:'
assert_contains 'procd:kill:proxypool'
assert_before 'procd:service-close:set' 'procd:kill:proxypool'
[ ! -e "$PROCD_RUNNING_FILE" ]
assert_not_contains_fragment 'procd:transaction-corrupted'

# A partially staged instance is still submitted by rc_procd even when a
# start_shadow operation fails. The post-commit hook must remove it and leave
# no marker. The builder must also stop issuing subsequent instance fields.
reset_case
if PROXYPOOL_TEST_FAIL_PROCD_STEP=param:respawn run_start v2_shadow; then
	echo 'shadow unexpectedly survived a procd parameter failure' >&2
	exit 1
fi
assert_contains 'procd:param:respawn 3600 5 5'
assert_not_contains_fragment 'procd:param:stdout'
assert_contains 'procd:service-close:set'
assert_contains 'procd:kill:proxypool'
assert_before 'procd:service-close:set' 'procd:kill:proxypool'
assert_not_contains_fragment 'procd:transaction-corrupted'
[ ! -e "$PROCD_RUNNING_FILE" ]
[ ! -e "$MARKER_FILE" ]

reset_case
if PROXYPOOL_TEST_FAIL_PROCD_STEP=close_instance run_start v2_shadow; then
	echo 'shadow unexpectedly survived a procd instance-close failure' >&2
	exit 1
fi
assert_contains 'procd:close:'
assert_contains 'procd:service-close:set'
assert_contains 'procd:kill:proxypool'
assert_before 'procd:service-close:set' 'procd:kill:proxypool'
assert_not_contains_fragment 'procd:transaction-corrupted'
[ ! -e "$PROCD_RUNNING_FILE" ]
[ ! -e "$MARKER_FILE" ]

# Disable cleans the recorded side and does not create a replacement marker.
reset_case
printf 'v1\n' >"$MARKER_FILE"
run_start v2_shadow 0
assert_contains 'legacy:stop'
assert_contains 'lease:flush'
assert_not_contains_fragment 'procd:open:'
[ ! -e "$PROCD_RUNNING_FILE" ]
[ ! -e "$MARKER_FILE" ]

# Missing runtime_backend is an upgrade-safe V1 default.
reset_case
run_start __missing__
assert_contains 'legacy:start'
[ "$(cat "$MARKER_FILE")" = v1 ]

# A clean shadow start is exactly one isolated procd instance and has no
# legacy/network/process-manager side effects.
reset_case
before_crontab=$(cksum "$CRONTAB_FILE")
run_start v2_shadow
after_crontab=$(cksum "$CRONTAB_FILE")
[ "$before_crontab" = "$after_crontab" ]
assert_contains 'procd:open:'
assert_contains 'procd:param:command /usr/sbin/proxypoold --config /etc/config/proxypool --socket /var/run/proxypoold.sock --shadow'
assert_contains 'procd:param:respawn 3600 5 5'
assert_contains 'procd:param:stdout 1'
assert_contains 'procd:param:stderr 1'
assert_contains 'procd:close:'
assert_contains 'procd:service-close:set'
[ "$(grep -Fc 'procd:open:' "$TRACE_FILE")" -eq 1 ]
[ "$(cat "$MARKER_FILE")" = v2_shadow ]
[ -e "$PROCD_RUNNING_FILE" ]
case "$(uname -s)" in Linux*) [ "$(stat -c '%a' "$MARKER_FILE")" = 600 ] ;; esac
assert_not_contains_fragment 'legacy:'
assert_not_contains_fragment 'xl2tpd:'
assert_not_contains_fragment 'lease:'
assert_not_contains_fragment 'cron:'

# Unknown backends fail closed and never fall back to legacy.
reset_case
if run_start surprise; then
	echo 'unknown runtime_backend unexpectedly started' >&2
	exit 1
fi
assert_not_contains_fragment 'legacy:'
assert_not_contains_fragment 'procd:open:'
[ ! -e "$PROCD_RUNNING_FILE" ]
[ ! -e "$MARKER_FILE" ]

# A direct rc.common start while shadow is already running must replace the
# service through the open JSON transaction. It must not call procd_kill,
# which performs json_init/json_cleanup and corrupts that transaction.
reset_case
printf 'v2_shadow\n' >"$MARKER_FILE"
: >"$PROCD_RUNNING_FILE"
run_start v2_shadow
assert_contains_fragment 'procd:service-open:proxypool '
assert_contains 'procd:service-close:set'
assert_not_contains_fragment 'procd:kill:'
assert_not_contains_fragment 'procd:transaction-corrupted'
[ -e "$PROCD_RUNNING_FILE" ]
[ "$(cat "$MARKER_FILE")" = v2_shadow ]

# Direct backend switches use the marker to clean the old side before the new
# side starts, independent of the newly edited UCI value.
reset_case
printf 'v1\n' >"$MARKER_FILE"
printf '%s\n' \
	'* * * * * /usr/lib/proxypool/watchdog.sh run' \
	'*/5 * * * * /usr/lib/proxypool/lease.sh accrue' \
	'7 7 * * * keep-me' >"$CRONTAB_FILE"
run_start v2_shadow
assert_contains 'legacy:stop'
assert_contains 'lease:flush'
assert_contains 'cron:restart'
assert_before 'legacy:stop' 'procd:open:'
! grep -Fq '/usr/lib/proxypool/watchdog.sh' "$CRONTAB_FILE"
! grep -Fq '/usr/lib/proxypool/lease.sh' "$CRONTAB_FILE"
grep -Fq 'keep-me' "$CRONTAB_FILE"
[ "$(cat "$MARKER_FILE")" = v2_shadow ]

reset_case
printf 'v2_shadow\n' >"$MARKER_FILE"
: >"$PROCD_RUNNING_FILE"
run_start v1
assert_contains 'legacy:start'
assert_not_contains_fragment 'procd:kill:'
assert_before 'procd:service-close:set' 'legacy:start'
[ ! -e "$PROCD_RUNNING_FILE" ]
[ "$(cat "$MARKER_FILE")" = v1 ]

# Disabled and invalid direct starts submit an empty procd service-set. Neither
# calls procd_kill inside the open JSON transaction; the invalid path performs
# an additional safe post-commit kill before clearing the stale marker.
reset_case
printf 'v2_shadow\n' >"$MARKER_FILE"
: >"$PROCD_RUNNING_FILE"
run_start v2_shadow 0
assert_contains 'procd:service-close:set'
assert_not_contains_fragment 'procd:kill:'
assert_not_contains_fragment 'procd:transaction-corrupted'
[ ! -e "$PROCD_RUNNING_FILE" ]
[ ! -e "$MARKER_FILE" ]

reset_case
printf 'v2_shadow\n' >"$MARKER_FILE"
: >"$PROCD_RUNNING_FILE"
if run_start surprise; then
	echo 'invalid backend unexpectedly survived an rc.common start' >&2
	exit 1
fi
assert_contains 'procd:service-close:set'
assert_contains 'procd:kill:proxypool'
assert_before 'procd:service-close:set' 'procd:kill:proxypool'
assert_not_contains_fragment 'procd:transaction-corrupted'
[ ! -e "$PROCD_RUNNING_FILE" ]
[ ! -e "$MARKER_FILE" ]

# stop_service follows the recorded old side. rc.common performs the final
# unconditional procd_kill for a shadow instance after stop_service returns.
reset_case
printf 'v1\n' >"$MARKER_FILE"
run_stop v2_shadow
assert_contains 'legacy:stop'
assert_contains 'lease:flush'
assert_contains 'cron:restart'
[ ! -e "$MARKER_FILE" ]

reset_case
printf 'v2_shadow\n' >"$MARKER_FILE"
: >"$PROCD_RUNNING_FILE"
run_stop v1
assert_not_contains_fragment 'legacy:'
assert_not_contains_fragment 'xl2tpd:'
assert_contains 'procd:kill:proxypool'
[ ! -e "$PROCD_RUNNING_FILE" ]
[ ! -e "$MARKER_FILE" ]

reset_case
printf 'corrupt-marker\n' >"$MARKER_FILE"
if run_stop v1; then
	echo 'unknown runtime marker unexpectedly selected a stop path' >&2
	exit 1
fi
assert_not_contains_fragment 'legacy:'

# Same-backend V1 reload retains the historical lightweight reload. Shadow
# reload is a bounded procd replacement because Phase 1 has no mutable reload.
reset_case
printf 'v1\n' >"$MARKER_FILE"
run_reload v1
assert_contains 'legacy:reload'
assert_not_contains_fragment 'legacy:stop'
assert_not_contains_fragment 'legacy:start'

reset_case
printf 'v1\n' >"$MARKER_FILE"
run_reload v1 0
assert_contains 'legacy:stop'
assert_contains 'lease:flush'
assert_not_contains_fragment 'legacy:reload'
assert_not_contains_fragment 'legacy:start'
[ ! -e "$MARKER_FILE" ]

reset_case
printf 'v2_shadow\n' >"$MARKER_FILE"
mkdir "$TEST_TMP/legacy-run"
run_reload v2_shadow
assert_contains 'procd:kill:proxypool'
assert_contains 'rc:start:'
assert_contains 'procd:open:'
assert_before 'procd:kill:proxypool' 'rc:start:'
assert_before 'rc:start:' 'procd:open:'
assert_not_contains_fragment 'legacy:'
assert_not_contains_fragment 'lease:'
assert_not_contains_fragment 'cron:'
assert_not_contains_fragment 'xl2tpd:'
[ "$(cat "$MARKER_FILE")" = v2_shadow ]

echo 'proxypool init behavior: PASS'
