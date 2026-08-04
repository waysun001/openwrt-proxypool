#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
WORKER="$ROOT/proxypool-core/files/lan-isolation-worker.sh"
TEST_TMP=${TMPDIR:-/tmp}/proxypool-lan-worker-test.$$
BIN="$TEST_TMP/bin"
TRACE="$TEST_TMP/trace"
AUTH_STATE="$TEST_TMP/activation-state"
TRANSACTION_DIR="$TEST_TMP/firewall-transaction"
MARKER="$TEST_TMP/start-deferred"

fail() {
	echo "LAN worker test: $*" >&2
	exit 1
}

trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM
mkdir -p "$BIN"

cat >"$BIN/lan-isolation" <<'EOF_HELPER'
#!/bin/sh
set -eu
printf 'lan:%s\n' "${1:-}" >>"$PROXYPOOL_TEST_TRACE"
case "${1:-}" in
	request|readiness|verify) exit 0 ;;
	*) exit 2 ;;
esac
EOF_HELPER

cat >"$BIN/transaction" <<'EOF_TRANSACTION'
#!/bin/sh
set -eu
printf 'transaction:%s\n' "${1:-}" >>"$PROXYPOOL_TEST_TRACE"
case "${1:-}" in
	journal-present)
		[ -e "$PROXYPOOL_FIREWALL_TRANSACTION_DIR" ] ||
			[ -L "$PROXYPOOL_FIREWALL_TRANSACTION_DIR" ]
		;;
	activation-current)
		[ "$(cat "$PROXYPOOL_TEST_AUTH_STATE" 2>/dev/null)" = current ]
		;;
	*) exit 2 ;;
esac
EOF_TRANSACTION

cat >"$BIN/firewall-defaults" <<'EOF_DEFAULTS'
#!/bin/sh
set -eu
printf 'defaults:%s\n' "${PROXYPOOL_COLD_BOOT:-unset}" >>"$PROXYPOOL_TEST_TRACE"
[ "${PROXYPOOL_COLD_BOOT:-}" = 0 ] || exit 2
[ ! -e "$PROXYPOOL_FIREWALL_TRANSACTION_DIR" ] &&
	[ ! -L "$PROXYPOOL_FIREWALL_TRANSACTION_DIR" ] || exit 3
[ "${PROXYPOOL_TEST_MODE:-}" != defaults-fail ] || exit 7
printf 'current\n' >"$PROXYPOOL_TEST_AUTH_STATE"
EOF_DEFAULTS

cat >"$BIN/proxypool-init" <<'EOF_INIT'
#!/bin/sh
set -eu
[ "$#" -eq 1 ] && [ "$1" = start ] || exit 2
printf 'init:start\n' >>"$PROXYPOOL_TEST_TRACE"
[ "$(cat "$PROXYPOOL_TEST_AUTH_STATE" 2>/dev/null)" = current ] || exit 3
[ "${PROXYPOOL_TEST_MODE:-}" = init-leaves-marker ] ||
	rm -f "$PROXYPOOL_DEFERRED_START_MARKER"
EOF_INIT

cat >"$BIN/sleep" <<'EOF_SLEEP'
#!/bin/sh
set -eu
printf 'sleep:%s\n' "${1:-}" >>"$PROXYPOOL_TEST_TRACE"
exit 93
EOF_SLEEP

chmod 755 "$BIN/lan-isolation" "$BIN/transaction" "$BIN/firewall-defaults" \
	"$BIN/proxypool-init" "$BIN/sleep"

run_worker() {
	PROXYPOOL_LAN_ISOLATION="$BIN/lan-isolation" \
	PROXYPOOL_WORKER_SLEEP="$BIN/sleep" \
	PROXYPOOL_INIT="$BIN/proxypool-init" \
	PROXYPOOL_TRANSACTION_HELPER="$BIN/transaction" \
	PROXYPOOL_FIREWALL_DEFAULTS="$BIN/firewall-defaults" \
	PROXYPOOL_FIREWALL_TRANSACTION_DIR="$TRANSACTION_DIR" \
	PROXYPOOL_DEFERRED_START_MARKER="$MARKER" \
	PROXYPOOL_TEST_TRACE="$TRACE" \
	PROXYPOOL_TEST_AUTH_STATE="$AUTH_STATE" \
	PROXYPOOL_TEST_MODE="$1" \
	sh "$WORKER" >"$TEST_TMP/worker.log" 2>&1
}

reset_case() {
	rm -rf "$TRANSACTION_DIR"
	rm -f "$MARKER"
	: >"$TRACE"
	printf '%s\n' "$1" >"$AUTH_STATE"
	: >"$MARKER"
}

assert_worker_stops_at_test_sleep() {
	mode=$1
	if run_worker "$mode"; then
		fail "$mode worker ignored terminal sleep failure"
	fi
}

reset_case absent
assert_worker_stops_at_test_sleep success
[ ! -e "$MARKER" ] && [ ! -L "$MARKER" ] ||
	fail 'live activation did not allow the deferred start marker to be consumed'
[ "$(grep -c '^defaults:0$' "$TRACE")" -eq 1 ] ||
	fail 'missing or repeated live firewall activation'
[ "$(grep -c '^init:start$' "$TRACE")" -eq 1 ] ||
	fail 'deferred daemon start did not run exactly once after activation'
expected_success='transaction:journal-present
transaction:activation-current
defaults:0
transaction:journal-present
transaction:activation-current
init:start'
actual_success=$(grep -E '^(transaction:|defaults:|init:)' "$TRACE")
[ "$actual_success" = "$expected_success" ] ||
	fail 'firewall activation and daemon start occurred out of order'

reset_case absent
assert_worker_stops_at_test_sleep defaults-fail
[ -f "$MARKER" ] && [ ! -L "$MARKER" ] ||
	fail 'activation failure consumed the deferred start marker'
! grep -q '^init:' "$TRACE" ||
	fail 'daemon start ran after firewall activation failed'
grep -q '^sleep:1$' "$TRACE" ||
	fail 'activation failure did not enter bounded retry backoff'

reset_case absent
mkdir "$TRANSACTION_DIR"
assert_worker_stops_at_test_sleep journal-present
[ -f "$MARKER" ] && [ ! -L "$MARKER" ] ||
	fail 'pending transaction consumed the deferred start marker'
! grep -Eq '^(defaults:|init:)' "$TRACE" ||
	fail 'pending transaction allowed firewall activation or daemon start'

reset_case current
assert_worker_stops_at_test_sleep init-leaves-marker
[ -f "$MARKER" ] && [ ! -L "$MARKER" ] ||
	fail 'fake-success daemon start unexpectedly consumed its marker'
! grep -q '^defaults:' "$TRACE" ||
	fail 'current firewall activation was needlessly repeated'
[ "$(grep -c '^init:start$' "$TRACE")" -eq 1 ] ||
	fail 'current activation did not attempt deferred daemon start once'
grep -q '^sleep:1$' "$TRACE" ||
	fail 'unconsumed start marker did not enter bounded retry backoff'

echo 'LAN isolation worker safety: PASS'
