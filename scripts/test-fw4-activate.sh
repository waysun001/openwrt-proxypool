#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
ACTIVATOR="$ROOT/proxypool-core/files/proxypool-fw4-activate"
TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM

fail() {
	printf 'fw4 activator contract: %s\n' "$*" >&2
	exit 1
}

[ -f "$ACTIVATOR" ] || fail 'production activator is missing'
BIN="$TEST_TMP/bin"
ASSETS="$TEST_TMP/assets"
mkdir -p "$BIN" "$ASSETS/nftables.d"

cat >"$BIN/id" <<'EOF_ID'
#!/bin/sh
[ "$#" -eq 1 ] && [ "$1" = -u ] || exit 2
printf '0\n'
EOF_ID

cat >"$BIN/ls" <<'EOF_LS'
#!/bin/sh
[ "$#" -eq 2 ] && [ "$1" = -nd ] || exit 2
[ -f "$2" ] && [ ! -L "$2" ] || exit 1
printf '%s\n' '-rw------- 1 0 0 16 Jan 1 00:00 candidate'
EOF_LS

cat >"$BIN/flock" <<'EOF_FLOCK'
#!/bin/sh
[ "$#" -eq 2 ] && [ "$1" = -x ] && [ "$2" = 1000 ] || exit 2
printf 'lock:fw4\n' >>"$PROXYPOOL_TEST_TRACE"
EOF_FLOCK

cat >"$BIN/sync" <<'EOF_SYNC'
#!/bin/sh
exit 0
EOF_SYNC

cat >"$BIN/mode-probe" <<'EOF_MODE'
#!/bin/sh
[ "$#" -eq 1 ] && [ "$1" = live ] || exit 2
exit 0
EOF_MODE

cat >"$BIN/checker" <<'EOF_CHECKER'
#!/bin/sh
[ "$#" -eq 1 ] && [ "$1" = "$PROXYPOOL_CONFIG_DIR" ] || exit 2
printf 'checker\n' >>"$PROXYPOOL_TEST_TRACE"
exit "${PROXYPOOL_TEST_CHECKER_STATUS:-0}"
EOF_CHECKER

cat >"$BIN/utpl" <<'EOF_UTPL'
#!/bin/sh
set -eu
case "${ACTION:-}" in
	start)
		printf 'utpl:start\n' >>"$PROXYPOOL_TEST_TRACE"
		status=${PROXYPOOL_TEST_RENDER_STATUS:-0}
		[ "$status" -eq 0 ] || exit "$status"
		printf 'candidate-state\n' >"$PROXYPOOL_FW4_STATE"
		chmod 600 "$PROXYPOOL_FW4_STATE"
		printf '%s\n' \
			"include \"$PROXYPOOL_TEST_GUARD_RULESET\"" \
			"include \"$PROXYPOOL_TEST_INPUT_GATE\"" \
			"include \"$PROXYPOOL_TEST_FORWARD_GATE\"" \
			"include \"$PROXYPOOL_TEST_NFTABLES_USER_DIR/*.nft\"" \
			'table inet fw4 { chain input { type filter hook input priority 0; policy drop; } }'
		;;
	includes)
		printf 'utpl:includes\n' >>"$PROXYPOOL_TEST_TRACE"
		status=${PROXYPOOL_TEST_INCLUDES_STATUS:-0}
		[ "$status" -eq 0 ] || exit "$status"
		if [ "${PROXYPOOL_TEST_RETIRE_JOURNAL:-1}" -eq 1 ]; then
			rm -f "$PROXYPOOL_TEST_JOURNAL"
		fi
		;;
	*) exit 2 ;;
esac
EOF_UTPL

cat >"$BIN/nft" <<'EOF_NFT'
#!/bin/sh
set -eu
case "$*" in
	'-c -f '*)
		printf 'nft:check\n' >>"$PROXYPOOL_TEST_TRACE"
		exit "${PROXYPOOL_TEST_NFT_CHECK_STATUS:-0}"
		;;
	'-f '*)
		printf 'nft:apply\n' >>"$PROXYPOOL_TEST_TRACE"
		exit "${PROXYPOOL_TEST_NFT_APPLY_STATUS:-0}"
		;;
	*) exit 2 ;;
esac
EOF_NFT

cat >"$BIN/transaction" <<'EOF_TRANSACTION'
#!/bin/sh
set -eu
[ "$#" -eq 1 ] && [ "$1" = journal-present ] || exit 2
printf 'journal:present\n' >>"$PROXYPOOL_TEST_TRACE"
status=${PROXYPOOL_TEST_JOURNAL_STATUS:-auto}
if [ "$status" != auto ]; then
	exit "$status"
fi
[ -e "$PROXYPOOL_TEST_JOURNAL" ] || [ -L "$PROXYPOOL_TEST_JOURNAL" ]
EOF_TRANSACTION

chmod 755 "$BIN/id" "$BIN/ls" "$BIN/flock" "$BIN/sync" "$BIN/mode-probe" \
	"$BIN/checker" "$BIN/utpl" "$BIN/nft" "$BIN/transaction"

cat >"$ASSETS/fw4.uc" <<'EOF_FW4_UCODE'
const STATEFILE = "/var/run/fw4.state";
this.cursor = uci.cursor();
this.cursor.load("firewall");
EOF_FW4_UCODE
printf '%s\n' 'fw4 main fixture' >"$ASSETS/main.uc"
printf '%s\n' 'table inet proxypool_guard { }' >"$ASSETS/guard.nft"
printf '%s\n' '# input gate fixture' >"$ASSETS/input.nft"
printf '%s\n' '# forward gate fixture' >"$ASSETS/forward.nft"

setup_case() {
	case_name=$1
	CASE_ROOT="$TEST_TMP/$case_name"
	CASE_CONFIG="$CASE_ROOT/config"
	CASE_STATE="$CASE_ROOT/fw4.state"
	CASE_JOURNAL="$CASE_ROOT/firewall-transaction"
	CASE_TRACE="$CASE_ROOT/trace"
	CASE_TMP="$CASE_ROOT/tmp"
	mkdir -p "$CASE_CONFIG" "$CASE_TMP"
	for package in firewall dhcp network; do
		printf '%s fixture\n' "$package" >"$CASE_CONFIG/$package"
	done
	printf 'old-state\n' >"$CASE_STATE"
	printf 'awaiting\n' >"$CASE_JOURNAL"
	: >"$CASE_TRACE"
}

run_case() {
	if env \
		PATH="$BIN:$PATH" \
		TMPDIR="$CASE_TMP" \
		PROXYPOOL_CONFIG_DIR="$CASE_CONFIG" \
		PROXYPOOL_FW4_UCODE="$ASSETS/fw4.uc" \
		PROXYPOOL_FW4_MAIN="$ASSETS/main.uc" \
		PROXYPOOL_FW4_STATE="$CASE_STATE" \
		PROXYPOOL_FW4_LOCK="$CASE_ROOT/fw4.lock" \
		PROXYPOOL_UTPL="$BIN/utpl" \
		PROXYPOOL_NFT="$BIN/nft" \
		PROXYPOOL_FLOCK="$BIN/flock" \
		PROXYPOOL_SYNC="$BIN/sync" \
		PROXYPOOL_FW4_CHECK="$BIN/checker" \
		PROXYPOOL_TRANSACTION_HELPER="$BIN/transaction" \
		PROXYPOOL_FIREWALL_TRANSACTION_DIR="$CASE_JOURNAL" \
		PROXYPOOL_FW4_MODE_PROBE="$BIN/mode-probe" \
		PROXYPOOL_NFTABLES_USER_DIR="$ASSETS/nftables.d" \
		PROXYPOOL_GUARD_RULESET="$ASSETS/guard.nft" \
		PROXYPOOL_INPUT_GATE="$ASSETS/input.nft" \
		PROXYPOOL_FORWARD_GATE="$ASSETS/forward.nft" \
		PROXYPOOL_LS_PROG="$BIN/ls" \
		PROXYPOOL_TEST_TRACE="$CASE_TRACE" \
		PROXYPOOL_TEST_JOURNAL="$CASE_JOURNAL" \
		PROXYPOOL_TEST_GUARD_RULESET="$ASSETS/guard.nft" \
		PROXYPOOL_TEST_INPUT_GATE="$ASSETS/input.nft" \
		PROXYPOOL_TEST_FORWARD_GATE="$ASSETS/forward.nft" \
		PROXYPOOL_TEST_NFTABLES_USER_DIR="$ASSETS/nftables.d" \
		"$@" \
		sh "$ACTIVATOR" live >"$CASE_ROOT/output" 2>&1; then
		CASE_STATUS=0
	else
		CASE_STATUS=$?
	fi
}

workspace_present() {
	find "$CASE_TMP" -mindepth 1 -maxdepth 1 -type d -name 'proxypool-fw4-activate.*' \
		-print -quit | grep -q .
}

assert_unknown_case() {
	case_name=$1
	expected_state=$2
	shift 2
	setup_case "$case_name"
	run_case "$@"
	[ "$CASE_STATUS" -eq 137 ] || fail "$case_name returned $CASE_STATUS instead of preserving status 137"
	[ -e "$CASE_JOURNAL" ] || fail "$case_name retired the awaiting journal"
	[ "$(cat "$CASE_STATE")" = "$expected_state" ] || fail "$case_name left an unexpected fw4 state"
	workspace_present || fail "$case_name deleted a workspace which an orphan may still use"
}

setup_case success
run_case
[ "$CASE_STATUS" -eq 0 ] || fail "successful activation returned $CASE_STATUS"
[ ! -e "$CASE_JOURNAL" ] || fail 'successful activation retained its journal'
[ "$(cat "$CASE_STATE")" = candidate-state ] || fail 'successful activation did not publish candidate state'
workspace_present && fail 'successful activation retained a stale workspace'
for event in checker utpl:start nft:check nft:apply utpl:includes journal:present; do
	grep -Fxq "$event" "$CASE_TRACE" || fail "successful activation skipped $event"
done

assert_unknown_case checker-signal old-state PROXYPOOL_TEST_CHECKER_STATUS=137
assert_unknown_case render-signal old-state PROXYPOOL_TEST_RENDER_STATUS=137
assert_unknown_case nft-check-signal old-state PROXYPOOL_TEST_NFT_CHECK_STATUS=137
assert_unknown_case nft-apply-signal candidate-state PROXYPOOL_TEST_NFT_APPLY_STATUS=137
assert_unknown_case includes-signal candidate-state \
	PROXYPOOL_TEST_INCLUDES_STATUS=137 PROXYPOOL_TEST_RETIRE_JOURNAL=0
assert_unknown_case journal-signal candidate-state \
	PROXYPOOL_TEST_RETIRE_JOURNAL=0 PROXYPOOL_TEST_JOURNAL_STATUS=137

setup_case known-nft-failure
run_case PROXYPOOL_TEST_NFT_APPLY_STATUS=43
[ "$CASE_STATUS" -eq 1 ] || fail "known nft failure returned $CASE_STATUS instead of 1"
[ "$(cat "$CASE_STATE")" = old-state ] || fail 'known nft failure did not restore old state'
[ -e "$CASE_JOURNAL" ] || fail 'known nft failure retired the parent-owned journal'
workspace_present && fail 'known nft failure retained a stale workspace'

setup_case false-absent
run_case PROXYPOOL_TEST_RETIRE_JOURNAL=0 PROXYPOOL_TEST_JOURNAL_STATUS=1
[ "$CASE_STATUS" -eq 1 ] || fail 'journal status 1 bypassed exact-path evidence'
[ -e "$CASE_JOURNAL" ] || fail 'false absent result removed journal evidence'

echo 'fw4 activator contract: PASS'
