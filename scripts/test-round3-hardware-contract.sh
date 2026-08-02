#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
SCRIPT="$ROOT/scripts/device-test/round3-bulk-recovery.sh"
LIB="$ROOT/scripts/device-test/round3-lib.sh"

fail() { echo "FAIL: $*" >&2; exit 1; }
require() { grep -Fq "$2" "$1" || fail "$3"; }
reject() { grep -Eq "$2" "$1" && fail "$3" || true; }

sh -n "$SCRIPT"
sh -n "$LIB"
reject "$SCRIPT" 'ifdown wan.*\|\| true|ifup wan.*\|\| true|kill -TERM "\$(daemon_pid|xl2tp_pid|pppd_pid)".*\|\| true' 'fault injection may silently no-op'
require "$SCRIPT" 'wait_interface_up false || fail' 'WAN-down state is not verified'
require "$SCRIPT" 'wait_interface_up true || fail' 'WAN recovery is not verified'
require "$SCRIPT" 'wait_new_pid proxypoold "$daemon_pid" || fail' 'daemon restart PID is not verified'
require "$SCRIPT" 'wait_new_pid xl2tpd "$xl2tp_pid" || fail' 'xl2tpd restart PID is not verified'
require "$SCRIPT" 'wait_new_session_pppd "$logical_interface" "$pppd_pid"' 'bound-node pppd restart is not verified'
require "$SCRIPT" 'start_client_watch daemon-window' 'daemon recovery has no concurrent LAN probe'
require "$SCRIPT" 'start_client_watch xl2tp-window' 'xl2tp recovery has no concurrent LAN probe'
require "$SCRIPT" 'start_client_watch pppd-window' 'pppd recovery has no concurrent LAN probe'
require "$SCRIPT" 'wait_client_watch_growth daemon-window "$fault_sample"' 'daemon fault has no post-injection probe sample'
require "$SCRIPT" 'wait_client_watch_growth xl2tp-window "$fault_sample"' 'xl2tp fault has no post-injection probe sample'
require "$SCRIPT" 'wait_client_watch_growth pppd-window "$fault_sample"' 'pppd fault has no post-injection probe sample'
require "$SCRIPT" 'round3_tracked_current_ids' 'cleanup is not restricted to imported nodes'
require "$SCRIPT" 'assert_success "$observed" "$label status"' 'node convergence accepts RPC error envelopes'
require "$SCRIPT" 'desired inventory changed' 'node convergence accepts an empty or changed inventory'
require "$SCRIPT" 'assert_success "$remaining" "final cleanup status"' 'strict cleanup accepts RPC error envelopes'
require "$SCRIPT" 'full fault mode requires executable PROXYPOOL_CLIENT_PROBE' 'LAN client fail-closed probe is not mandatory'
require "$SCRIPT" 'poll_job "$job_id" bulk-import "$import_count" succeeded,failed' 'bulk job total and terminal state are not checked'
require "$SCRIPT" 'valid import did not increment revision exactly once' 'atomic revision is not checked'
require "$SCRIPT" 'stale reconnect overwrote the edited node' 'stale task overwrite is not checked'
require "$SCRIPT" 'cleanup_nodes 1' 'successful hardware run does not clean imported nodes'
require "$SCRIPT" 'valid import must contain 40-60 non-empty nodes' 'hardware fixture count is hard-coded'
sh "$ROOT/tests/integration/round3_harness_semantics_test.sh"
echo 'PASS: Round 3 hardware harness contract'
