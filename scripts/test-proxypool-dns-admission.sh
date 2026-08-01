#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
PROXYPOOL="$ROOT/proxypool-core/files/proxypool.sh"
TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM

fail() {
	printf 'ProxyPool DNS admission contract: %s\n' "$*" >&2
	exit 1
}

BIN="$TEST_TMP/bin"
RUN_DIR="$TEST_TMP/run"
LOG_FILE="$TEST_TMP/proxypool.log"
TRACE="$TEST_TMP/trace"
mkdir -p "$BIN"

cat >"$BIN/dns-manager.sh" <<'EOF_DNS'
#!/usr/bin/env sh
printf 'dns:%s\n' "$1" >>"$PROXYPOOL_TEST_TRACE"
case "$1" in
	status)
		case "${PROXYPOOL_TEST_DNS_STATUS_MODE:-unavailable}" in
			unavailable) printf '%s\n' dns_path_unavailable; exit 1 ;;
			false-ready) printf '%s\n' dns_path_ready; exit 1 ;;
			ready) printf '%s\n' dns_path_ready; exit 0 ;;
			*) exit 2 ;;
		esac
		;;
esac
exit 1
EOF_DNS

cat >"$BIN/firewall.sh" <<'EOF_FIREWALL'
#!/usr/bin/env sh
printf 'firewall:%s%s\n' "$1" "${2:+:$2}" >>"$PROXYPOOL_TEST_TRACE"
exit 0
EOF_FIREWALL

for manager in l2tp socks5 slp; do
	cat >"$BIN/$manager-manager.sh" <<EOF_MANAGER
#!/usr/bin/env sh
printf '$manager:%s%s\\n' "\$1" "\${2:+:\$2}" >>"\$PROXYPOOL_TEST_TRACE"
exit 0
EOF_MANAGER
done

cat >"$BIN/status.sh" <<'EOF_STATUS'
#!/usr/bin/env sh
printf 'status:%s\n' "$1" >>"$PROXYPOOL_TEST_TRACE"
printf '%s\n' '{}'
EOF_STATUS

cat >"$BIN/uci" <<'EOF_UCI'
#!/usr/bin/env sh
quiet=0
if [ "${1:-}" = -q ]; then quiet=1; shift; fi
case "${1:-}" in
	get)
		case "${2:-}" in
			proxypool.c_off.enabled) printf '%s\n' 0 ;;
			proxypool.*.enabled) printf '%s\n' 1 ;;
			proxypool.*.type) printf '%s\n' socks5 ;;
			proxypool.*.name) printf '%s\n' test-client ;;
			*) exit 1 ;;
		esac
		;;
	show)
		# Global restart/reload fixtures deliberately have no configured clients.
		exit 0
		;;
	*) [ "$quiet" -eq 1 ] || printf 'unsupported fake uci command\n' >&2; exit 1 ;;
esac
EOF_UCI
chmod 755 "$BIN"/*

run_action() {
	env \
		PATH="$BIN:$PATH" \
		PROXYPOOL_SCRIPT_DIR="$BIN" \
		PROXYPOOL_RUN_DIR="$RUN_DIR" \
		PROXYPOOL_LOG_FILE="$LOG_FILE" \
		PROXYPOOL_UCI="$BIN/uci" \
		PROXYPOOL_TEST_TRACE="$TRACE" \
		PROXYPOOL_TEST_DNS_STATUS_MODE="${PROXYPOOL_TEST_DNS_STATUS_MODE:-unavailable}" \
		sh "$PROXYPOOL" "$@"
}

# Phase 1 supersedes the retained V1 DNS admission code with the stronger
# legacy quarantine.  Every mutating V1 entry must now stop before DNS, UCI,
# teardown, logging, or runtime-directory creation; otherwise an unreachable
# historical branch could silently become a second runtime owner again.
for invocation in \
	'start' 'stop' 'restart' 'reload' \
	'start_client c1' 'stop_client c1' 'restart_client c1' 'save_restart_client c1' \
	'batch_connect c1' 'batch_disconnect c1' 'batch_enable c1' 'batch_disable c1' \
	'batch_delete c1' 'sequential_start c1' 'toggle_client c1' 'probe_all'; do
	rm -rf "$RUN_DIR" "$LOG_FILE"
	: >"$TRACE"
	stdout_file="$TEST_TMP/stdout"
	set -- $invocation
	if run_action "$@" >"$stdout_file" 2>/dev/null; then
		fail "$invocation crossed the legacy quarantine"
	else
		rc=$?
	fi
	[ "$rc" -eq 125 ] || fail "$invocation returned $rc instead of quarantine status 125"
	[ "$(cat "$stdout_file")" = legacy_runtime_quarantined ] ||
		fail "$invocation returned the wrong quarantine token"
	[ ! -e "$RUN_DIR" ] || fail "$invocation mutated runtime state before quarantine"
	[ ! -e "$LOG_FILE" ] || fail "$invocation wrote a log before quarantine"
	[ ! -s "$TRACE" ] ||
		fail "$invocation touched a retained V1 dependency: $(tr '\n' ' ' <"$TRACE")"
done

# A forged ready token from the historical DNS helper cannot weaken the earlier
# quarantine boundary.
: >"$TRACE"
if PROXYPOOL_TEST_DNS_STATUS_MODE=ready run_action start >"$TEST_TMP/stdout" 2>/dev/null; then
	fail 'legacy start trusted a forged ready DNS path'
else
	rc=$?
fi
[ "$rc" -eq 125 ] || fail 'ready DNS fixture changed the quarantine status'
[ ! -s "$TRACE" ] || fail 'ready DNS fixture was consulted before quarantine'

# Status remains the sole read-only entry and must not initialize runtime state.
rm -rf "$RUN_DIR" "$LOG_FILE"
: >"$TRACE"
run_action status >"$TEST_TMP/status.json" || fail 'read-only status was quarantined'
[ "$(cat "$TEST_TMP/status.json")" = '{}' ] || fail 'status returned unexpected JSON'
[ "$(cat "$TRACE")" = 'status:get' ] || fail 'status did not delegate exactly once'
[ ! -e "$RUN_DIR" ] && [ ! -e "$LOG_FILE" ] || fail 'status created legacy runtime state'

echo 'ProxyPool legacy-before-DNS admission contract: PASS'
