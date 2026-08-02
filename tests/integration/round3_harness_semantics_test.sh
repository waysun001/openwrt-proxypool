#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
. "$ROOT/scripts/device-test/round3-lib.sh"

fail() { echo "FAIL: $*" >&2; exit 1; }

round3_states_settled 2 2 'online
backoff' || fail 'terminal runtime states were rejected'
round3_states_settled 0 0 '' || fail 'empty desired/runtime inventory was rejected'
for transient in queued starting validating degraded stopping recovering; do
	if round3_states_settled 1 1 "$transient"; then fail "transient state $transient was accepted"; fi
done
if round3_states_settled 2 1 online; then fail 'incomplete runtime inventory was accepted'; fi
if round3_states_settled 1 0 ''; then fail 'empty runtime inventory was accepted for a desired node'; fi

TMP_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/proxypool-round3-semantics.XXXXXX")
trap 'rm -rf "$TMP_ROOT"' EXIT INT TERM
mkdir -p "$TMP_ROOT/proc/101" "$TMP_ROOT/proc/202" "$TMP_ROOT/proc/303"
printf 'pppd\000ifname\000ppv20007\000' >"$TMP_ROOT/proc/101/cmdline"
printf 'pppd\000ipparam\000ppv20042\000password\000not-logged\000' >"$TMP_ROOT/proc/202/cmdline"
printf 'pppd\000ipparam\000ppv200420\000' >"$TMP_ROOT/proc/303/cmdline"

pid=$(round3_session_pppd_pid "$TMP_ROOT/proc" ppv20042 '101 202 303') || fail 'exact pppd session was not found'
[ "$pid" = 202 ] || fail "wrong pppd session selected: $pid"
if round3_session_pppd_pid "$TMP_ROOT/proc" ppv2004 '101 202 303' >/dev/null; then fail 'partial interface name matched a pppd session'; fi

printf '%s\n' node-a node-b node-deleted >"$TMP_ROOT/tracked"
filtered=$(round3_tracked_current_ids "$TMP_ROOT/tracked" 'node-b
node-foreign')
[ "$filtered" = node-b ] || fail "cleanup selection escaped tracked imports: $filtered"

alive_calls=0
dies_on_second_check() {
	alive_calls=$((alive_calls + 1))
	[ "$alive_calls" -lt 2 ]
}
always_alive() { return 0; }
round3_wait_pid_gone 42 3 0 dies_on_second_check || fail 'old PID disappearance was not detected'
if round3_wait_pid_gone 42 2 0 always_alive; then fail 'live old PID was accepted as gone'; fi

printf '%s\n' 1 >"$TMP_ROOT/watch.count"
: >"$TMP_ROOT/watch.failed"
(
	sleep 1
	printf '%s\n' 2 >"$TMP_ROOT/watch.count"
) &
counter_writer=$!
round3_wait_counter_growth "$TMP_ROOT/watch.count" "$TMP_ROOT/watch.failed" 1 3 1 always_alive 99 || fail 'post-injection probe sample was not detected'
wait "$counter_writer"
printf '%s\n' failed >"$TMP_ROOT/watch.failed"
if round3_wait_counter_growth "$TMP_ROOT/watch.count" "$TMP_ROOT/watch.failed" 2 1 0 always_alive 99; then fail 'failed probe watcher was accepted'; fi

echo 'PASS: Round 3 hardware harness semantics'
