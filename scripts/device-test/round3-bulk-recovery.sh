#!/bin/sh
set -eu

# Runs on the OpenWrt router. It never prints import source text or credentials.
SOCKET=${PROXYPOOL_SOCKET:-/var/run/proxypoold.sock}
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
. "$SCRIPT_DIR/round3-lib.sh"
VALID_IMPORT=${PROXYPOOL_VALID_IMPORT:-$SCRIPT_DIR/fixtures/import-60-valid.txt}
INVALID_IMPORT=${PROXYPOOL_INVALID_IMPORT:-$SCRIPT_DIR/fixtures/import-invalid.txt}
REPORT_DIR=${PROXYPOOL_REPORT_DIR:-/tmp/proxypool-round3-$(date -u +%Y%m%dT%H%M%SZ)}
ALLOW_FAULTS=${PROXYPOOL_ALLOW_FAULTS:-0}
JOB_TIMEOUT=${PROXYPOOL_JOB_TIMEOUT:-1800}
POLL_INTERVAL=${PROXYPOOL_POLL_INTERVAL:-5}
CLIENT_PROBE=${PROXYPOOL_CLIENT_PROBE:-}
TEST_DEVICE_ID=${PROXYPOOL_TEST_DEVICE_ID:-}
WAN_WAS_DOWN=0
SECRET_PATTERNS=
IMPORTED=0
CLEANUP_COMPLETE=0
PROBE_WATCH_PID=
PROBE_WATCH_FAIL=
PROBE_WATCH_COUNT=

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
command -v proxypoolctl >/dev/null 2>&1 || fail "proxypoolctl is missing"
command -v jsonfilter >/dev/null 2>&1 || fail "jsonfilter is missing"
[ -r /usr/share/libubox/jshn.sh ] || fail "jshn is missing"
[ -r "$VALID_IMPORT" ] || fail "valid import fixture is missing"
[ -r "$INVALID_IMPORT" ] || fail "invalid import fixture is missing"
# shellcheck source=/dev/null
. /usr/share/libubox/jshn.sh
umask 077
mkdir -p "$REPORT_DIR"

timestamp() { date -u +%Y-%m-%dT%H:%M:%SZ; }
log() { printf '%s %s\n' "$(timestamp)" "$*" | tee -a "$REPORT_DIR/timeline.log"; }
next_request_id() { printf 'round3-%s' "$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"; }
field() { jsonfilter -s "$1" -e "$2" 2>/dev/null || true; }
record() { printf '%s\n' "$2" >"$REPORT_DIR/$1.json"; }

rpc_empty() {
	method=$1
	json_init
	json_add_int version 1
	json_add_string id "$(next_request_id)"
	json_add_string method "$method"
	json_add_object params
	json_close_object
	json_dump | proxypoolctl call --socket "$SOCKET"
}

rpc_job_get() {
	job_id=$1
	json_init
	json_add_int version 1
	json_add_string id "$(next_request_id)"
	json_add_string method job.get
	json_add_object params
	json_add_string job_id "$job_id"
	json_close_object
	json_dump | proxypoolctl call --socket "$SOCKET"
}

rpc_diagnostic_get() {
	job_id=$1
	json_init
	json_add_int version 1
	json_add_string id "$(next_request_id)"
	json_add_string method diagnostics.get
	json_add_object params
	json_add_string job_id "$job_id"
	json_close_object
	json_dump | proxypoolctl call --socket "$SOCKET"
}

rpc_artifact_action() {
	method=$1 artifact_id=$2
	json_init
	json_add_int version 1
	json_add_string id "$(next_request_id)"
	json_add_string method "$method"
	json_add_object params
	json_add_string artifact_id "$artifact_id"
	json_close_object
	json_dump | proxypoolctl call --socket "$SOCKET"
}

rpc_import_preview() {
	fixture=$1 revision=$2
	raw=$(sed -e 's/\r$//' "$fixture")
	json_init
	json_add_int version 1
	json_add_string id "$(next_request_id)"
	json_add_string method import.preview
	json_add_object params
	json_add_string protocol l2tp
	json_add_string raw "$raw"
	json_add_int expected_revision "$revision"
	json_close_object
	json_dump | proxypoolctl call --socket "$SOCKET"
	raw=
}

rpc_import_commit() {
	preview_id=$1 preview_hash=$2 revision=$3
	json_init
	json_add_int version 1
	json_add_string id "$(next_request_id)"
	json_add_string method import.commit
	json_add_object params
	json_add_string preview_id "$preview_id"
	json_add_string preview_hash "$preview_hash"
	json_add_int expected_revision "$revision"
	json_close_object
	json_dump | proxypoolctl call --socket "$SOCKET"
}

rpc_node_action() {
	node_id=$1 operation=$2 revision=$3
	json_init
	json_add_int version 1
	json_add_string id "$(next_request_id)"
	json_add_string method node.action
	json_add_object params
	json_add_string node_id "$node_id"
	json_add_string action "$operation"
	json_add_int expected_revision "$revision"
	json_close_object
	json_dump | proxypoolctl call --socket "$SOCKET"
}

rpc_node_save() {
	node_id=$1 name=$2 server=$3 port=$4 revision=$5
	json_init
	json_add_int version 1
	json_add_string id "$(next_request_id)"
	json_add_string method node.save
	json_add_object params
	json_add_string node_id "$node_id"
	json_add_string name "$name"
	json_add_string protocol l2tp
	json_add_boolean enabled 1
	json_add_string server "$server"
	json_add_int port "$port"
	json_add_string username ""
	json_add_string password ""
	json_add_string expires_at ""
	json_add_int expected_revision "$revision"
	json_close_object
	json_dump | proxypoolctl call --socket "$SOCKET"
}

rpc_node_delete() {
	node_id=$1 revision=$2
	json_init
	json_add_int version 1
	json_add_string id "$(next_request_id)"
	json_add_string method node.delete
	json_add_object params
	json_add_string node_id "$node_id"
	json_add_int expected_revision "$revision"
	json_close_object
	json_dump | proxypoolctl call --socket "$SOCKET"
}

rpc_device_bind() {
	device_id=$1 node_id=$2 revision=$3
	json_init
	json_add_int version 1
	json_add_string id "$(next_request_id)"
	json_add_string method device.bind
	json_add_object params
	json_add_string device_id "$device_id"
	json_add_string node_id "$node_id"
	json_add_int expected_revision "$revision"
	json_close_object
	json_dump | proxypoolctl call --socket "$SOCKET"
}

assert_success() { [ -z "$(field "$1" '@.error.code')" ] || fail "$2: $(field "$1" '@.error.code')"; }
status() { rpc_empty status.get; }

snapshot_network() {
	label=$1
	/usr/sbin/nft -j list table inet proxypool_guard >"$REPORT_DIR/$label-nft.json" 2>/dev/null || true
	/sbin/ip -4 -j rule show >"$REPORT_DIR/$label-rules.json" 2>/dev/null || true
	/sbin/ip -4 -j route show table all >"$REPORT_DIR/$label-routes.json" 2>/dev/null || true
}

record_leak_counters() {
	label=$1
	owned_rules=$(/sbin/ip -4 rule show 2>/dev/null | awk '$1 + 0 >= 200001 && $1 + 0 <= 200060 { count++ } END { print count + 0 }')
	owned_routes=$(/sbin/ip -4 route show table all 2>/dev/null | awk '$1 == "default" && $0 ~ /table 1000[0-6][0-9]/ { count++ } END { print count + 0 }')
	bad_routes=$(/sbin/ip -4 route show table all 2>/dev/null | awk '$1 == "default" && $0 ~ /table 1000[0-6][0-9]/ && $0 !~ / dev l2tp-ppv2/ { count++ } END { print count + 0 }')
	if /usr/sbin/nft list table inet proxypool_guard >/dev/null 2>&1; then guard_table=1; else guard_table=0; fi
	printf '%s label=%s guard_table=%s owned_rules=%s owned_routes=%s bad_owned_routes=%s\n' \
		"$(timestamp)" "$label" "$guard_table" "$owned_rules" "$owned_routes" "$bad_routes" >>"$REPORT_DIR/leak-counters.log"
	[ "$guard_table" -eq 1 ] || fail "$label: fail-closed nft guard table is missing"
	[ "$bad_routes" -eq 0 ] || fail "$label: an owned policy table points outside a managed L2TP interface"
}

poll_job() {
	job_id=$1 label=$2 expected_total=$3 accepted=$4 deadline=$(( $(date +%s) + JOB_TIMEOUT )) count=0
	while [ "$(date +%s)" -lt "$deadline" ]; do
		response=$(rpc_job_get "$job_id") || fail "$label job query failed"
		assert_success "$response" "$label job query"
		state=$(field "$response" '@.result.state')
		count=$((count + 1))
		record "$label-job-$count" "$response"
		log "$label job=$job_id state=$state"
		case "$state" in
			succeeded|failed|cancelled|replaced)
				total=$(field "$response" '@.result.total')
				queued=$(field "$response" '@.result.queued')
				running=$(field "$response" '@.result.running')
				[ "$total" = "$expected_total" ] || fail "$label total=$total, expected $expected_total"
				[ "${queued:-0}" -eq 0 ] && [ "${running:-0}" -eq 0 ] || fail "$label retained queued/running nodes"
				case ",$accepted," in *,$state,*) ;; *) fail "$label ended as $state";; esac
				if [ "$expected_total" -gt 0 ]; then
					progress_states=$(jsonfilter -s "$response" -e '@.result.nodes[*].state' 2>/dev/null || true)
					progress_count=$(printf '%s\n' "$progress_states" | awk 'NF { count++ } END { print count + 0 }')
					[ "$progress_count" -eq "$expected_total" ] || fail "$label returned $progress_count/$expected_total node results"
					if [ "$state" != "replaced" ] && [ "$state" != "cancelled" ] && printf '%s\n' "$progress_states" | grep -Ev '^(online|failed|disabled|backoff)$' >/dev/null; then
						fail "$label contains a non-terminal node state"
					fi
				fi
				return 0
				;;
		esac
		sleep "$POLL_INTERVAL"
	done
	fail "$label job did not reach a terminal state in $JOB_TIMEOUT seconds"
}

interface_up() {
	result=$(/bin/ubus call network.interface.wan status 2>/dev/null) || return 1
	field "$result" '@.up'
}

wait_interface_up() {
	want=$1 deadline=$(( $(date +%s) + 60 ))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		[ "$(interface_up || true)" = "$want" ] && return 0
		sleep 2
	done
	return 1
}

process_alive() { kill -0 "$1" 2>/dev/null; }
wait_pid_gone() { round3_wait_pid_gone "$1" 30 2 process_alive; }

wait_new_pid() {
	name=$1 old=$2 deadline=$(( $(date +%s) + 60 ))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		new=$(pidof "$name" 2>/dev/null | awk '{print $1}')
		if ! kill -0 "$old" 2>/dev/null && [ -n "$new" ] && [ "$new" != "$old" ]; then return 0; fi
		sleep 2
	done
	return 1
}

session_pppd_pid() {
	logical_interface=$1
	round3_session_pppd_pid /proc "$logical_interface" "$(pidof pppd 2>/dev/null || true)"
}

wait_new_session_pppd() {
	logical_interface=$1 old=$2 deadline=$(( $(date +%s) + 90 ))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		new=$(session_pppd_pid "$logical_interface" || true)
		if ! kill -0 "$old" 2>/dev/null && [ -n "$new" ] && [ "$new" != "$old" ]; then
			printf '%s\n' "$new"
			return 0
		fi
		sleep 2
	done
	return 1
}

wait_nodes_settled() {
	label=$1 expected_count=$2 deadline=$(( $(date +%s) + JOB_TIMEOUT ))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		observed=$(status) || fail "$label status unavailable"
		assert_success "$observed" "$label status"
		desired_count=$(jsonfilter -s "$observed" -e '@.result.desired.nodes[*].id' 2>/dev/null | awk 'NF { count++ } END { print count + 0 }')
		[ "$desired_count" -eq "$expected_count" ] || fail "$label desired inventory changed: $desired_count/$expected_count"
		states=$(jsonfilter -s "$observed" -e '@.result.runtime.nodes[*].state' 2>/dev/null || true)
		runtime_count=$(printf '%s\n' "$states" | awk 'NF { count++ } END { print count + 0 }')
		if round3_states_settled "$desired_count" "$runtime_count" "$states"; then
			record "$label-settled" "$observed"
			return 0
		fi
		sleep "$POLL_INTERVAL"
	done
	fail "$label nodes did not settle"
}

run_client_probe() {
	stage=$1
	[ -n "$CLIENT_PROBE" ] && [ -x "$CLIENT_PROBE" ] || fail "full fault mode requires executable PROXYPOOL_CLIENT_PROBE"
	"$CLIENT_PROBE" "$stage" >>"$REPORT_DIR/client-probe.log" 2>&1 || fail "client probe failed at $stage"
	log "client probe passed stage=$stage"
}

start_client_watch() {
	stage=$1
	[ -z "$PROBE_WATCH_PID" ] || fail "client probe watch is already running"
	PROBE_WATCH_FAIL="$REPORT_DIR/client-probe-$stage.failed"
	PROBE_WATCH_COUNT="$REPORT_DIR/client-probe-$stage.count"
	rm -f "$PROBE_WATCH_FAIL" "$PROBE_WATCH_COUNT"
	(
		watch_count=0
		while :; do
			if ! "$CLIENT_PROBE" "$stage" >>"$REPORT_DIR/client-probe.log" 2>&1; then
				printf '%s\n' failed >"$PROBE_WATCH_FAIL"
				exit 1
			fi
			watch_count=$((watch_count + 1))
			printf '%s\n' "$watch_count" >"$PROBE_WATCH_COUNT"
			sleep 1
		done
	) &
	PROBE_WATCH_PID=$!
	watch_deadline=$(( $(date +%s) + 15 ))
	while [ "$(date +%s)" -lt "$watch_deadline" ]; do
		[ -s "$PROBE_WATCH_FAIL" ] && fail "client probe watch failed at $stage"
		[ -s "$PROBE_WATCH_COUNT" ] && return 0
		kill -0 "$PROBE_WATCH_PID" 2>/dev/null || fail "client probe watch stopped at $stage"
		sleep 1
	done
	fail "client probe watch did not start at $stage"
}

client_watch_count() {
	count=$(cat "$PROBE_WATCH_COUNT" 2>/dev/null || echo 0)
	case "$count" in ''|*[!0-9]*) fail "client probe watch count is invalid";; esac
	printf '%s\n' "$count"
}

wait_client_watch_growth() {
	stage=$1 baseline=$2
	round3_wait_counter_growth "$PROBE_WATCH_COUNT" "$PROBE_WATCH_FAIL" "$baseline" 15 1 process_alive "$PROBE_WATCH_PID" || \
		fail "client probe watch did not sample the injected $stage fault"
}

stop_client_watch() {
	stage=$1 minimum_samples=${2:-2}
	watch_pid=$PROBE_WATCH_PID
	[ -n "$watch_pid" ] || fail "client probe watch is not running at $stage"
	while [ "$(cat "$PROBE_WATCH_COUNT" 2>/dev/null || echo 0)" -lt "$minimum_samples" ]; do
		[ -s "$PROBE_WATCH_FAIL" ] && fail "client probe watch failed at $stage"
		kill -0 "$watch_pid" 2>/dev/null || fail "client probe watch stopped at $stage"
		sleep 1
	done
	kill -TERM "$watch_pid" 2>/dev/null || true
	wait "$watch_pid" 2>/dev/null || true
	PROBE_WATCH_PID=
	[ ! -s "$PROBE_WATCH_FAIL" ] || fail "client probe watch failed at $stage"
	log "client probe watch passed stage=$stage samples=$(cat "$PROBE_WATCH_COUNT")"
}

cleanup_nodes() {
	strict=$1
	current_cleanup=$(status 2>/dev/null) || return 1
	if [ "$strict" = 1 ]; then assert_success "$current_cleanup" "cleanup status"; elif [ -n "$(field "$current_cleanup" '@.error.code')" ]; then return 1; fi
	current_ids=$(jsonfilter -s "$current_cleanup" -e '@.result.desired.nodes[*].id' 2>/dev/null || true)
	ids=$(round3_tracked_current_ids "$REPORT_DIR/imported-node-ids.txt" "$current_ids" || true)
	cleanup_jobs=
	for cleanup_id in $ids; do
		cleanup_revision=$(field "$current_cleanup" '@.result.config.revision')
		cleanup_response=$(rpc_node_delete "$cleanup_id" "$cleanup_revision" 2>/dev/null) || { [ "$strict" = 0 ] && continue; return 1; }
		[ -z "$(field "$cleanup_response" '@.error.code')" ] || { [ "$strict" = 0 ] && continue; return 1; }
		cleanup_job=$(field "$cleanup_response" '@.result.job_id')
		[ -z "$cleanup_job" ] || cleanup_jobs="$cleanup_jobs $cleanup_job"
		current_cleanup=$(status 2>/dev/null) || return 1
		if [ "$strict" = 1 ]; then assert_success "$current_cleanup" "cleanup refresh status"; elif [ -n "$(field "$current_cleanup" '@.error.code')" ]; then return 1; fi
	done
	if [ "$strict" = 1 ]; then
		for cleanup_job in $cleanup_jobs; do poll_job "$cleanup_job" "cleanup-$cleanup_job" 1 succeeded; done
		remaining=$(status) || fail "final cleanup status unavailable"
		assert_success "$remaining" "final cleanup status"
		remaining_ids=$(jsonfilter -s "$remaining" -e '@.result.desired.nodes[*].id' 2>/dev/null || true)
		remaining_tracked=$(round3_tracked_current_ids "$REPORT_DIR/imported-node-ids.txt" "$remaining_ids" || true)
		[ -z "$remaining_tracked" ] || fail "cleanup left imported nodes"
	fi
}

on_exit() {
	code=$?
	trap - EXIT INT TERM
	set +e
	if [ -n "$PROBE_WATCH_PID" ]; then kill -TERM "$PROBE_WATCH_PID" 2>/dev/null; wait "$PROBE_WATCH_PID" 2>/dev/null; fi
	if [ "$WAN_WAS_DOWN" -eq 1 ]; then
		/sbin/ifup wan >/dev/null 2>&1
		wait_interface_up true
	fi
	if [ "$IMPORTED" -eq 1 ] && [ "$CLEANUP_COMPLETE" -eq 0 ]; then cleanup_nodes 0; fi
	[ -z "$SECRET_PATTERNS" ] || rm -f "$SECRET_PATTERNS"
	exit "$code"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

log "round 3 started report=$REPORT_DIR"
import_count=$(awk 'NF { count++ } END { print count + 0 }' "$VALID_IMPORT")
[ "$import_count" -ge 40 ] && [ "$import_count" -le 60 ] || fail "valid import must contain 40-60 non-empty nodes"
if [ "$ALLOW_FAULTS" = "1" ]; then
	[ -n "$TEST_DEVICE_ID" ] || fail "full fault mode requires PROXYPOOL_TEST_DEVICE_ID"
	[ -n "$CLIENT_PROBE" ] && [ -x "$CLIENT_PROBE" ] || fail "full fault mode requires executable PROXYPOOL_CLIENT_PROBE"
fi
initial=$(status) || fail "daemon status unavailable"
assert_success "$initial" "initial status"
record initial-status "$initial"
revision=$(field "$initial" '@.result.config.revision')
node_count=$(jsonfilter -s "$initial" -e '@.result.desired.nodes[*].id' 2>/dev/null | wc -l | tr -d ' ')
[ "$node_count" -eq 0 ] || fail "round 3 requires an empty V2 node set (found $node_count)"
snapshot_network before
record_leak_counters before

log "checking invalid import atomicity"
invalid=$(rpc_import_preview "$INVALID_IMPORT" "$revision")
assert_success "$invalid" "invalid preview"
record invalid-preview "$invalid"
[ "$(field "$invalid" '@.result.blocked')" = "true" ] || fail "invalid preview was not blocked"
after_invalid=$(status)
[ "$(field "$after_invalid" '@.result.config.revision')" = "$revision" ] || fail "invalid preview changed configuration"

log "previewing and atomically committing $import_count nodes"
preview=$(rpc_import_preview "$VALID_IMPORT" "$revision")
assert_success "$preview" "$import_count-node preview"
record valid-preview "$preview"
[ "$(field "$preview" '@.result.blocked')" != "true" ] || fail "valid preview was blocked"
preview_id=$(field "$preview" '@.result.preview_id')
preview_hash=$(field "$preview" '@.result.preview_hash')
base_revision=$(field "$preview" '@.result.base_revision')
[ -n "$preview_id" ] && [ -n "$preview_hash" ] || fail "valid preview returned no commit token"
commit=$(rpc_import_commit "$preview_id" "$preview_hash" "$base_revision")
assert_success "$commit" "$import_count-node commit"
record valid-commit "$commit"
job_id=$(field "$commit" '@.result.job_id')
[ -n "$job_id" ] || fail "commit returned no job"
committed_revision=$(field "$commit" '@.result.config_revision')
[ "$committed_revision" -eq $((revision + 1)) ] || fail "valid import did not increment revision exactly once"
IMPORTED=1
post_commit_status=$(status); assert_success "$post_commit_status" "post-commit status"
jsonfilter -s "$post_commit_status" -e '@.result.desired.nodes[*].id' >"$REPORT_DIR/imported-node-ids.txt"

log "checking refresh-safe polling while the batch is running"
index=0
while [ "$index" -lt 3 ]; do
	index=$((index + 1)); current=$(status); assert_success "$current" "status poll"; record "status-refresh-$index" "$current"
done

current=$(status)
revision=$(field "$current" '@.result.config.revision')
duplicate=$(rpc_import_preview "$VALID_IMPORT" "$revision")
assert_success "$duplicate" "duplicate preview"
record duplicate-preview "$duplicate"
[ "$(field "$duplicate" '@.result.skipped')" = "$import_count" ] || fail "duplicate preview did not skip all $import_count nodes"

extra_fixture="$REPORT_DIR/import-over-capacity.txt"
needed=$((61 - import_count)); extra_index=1
: >"$extra_fixture"
while [ "$extra_index" -le "$needed" ]; do
	printf '203.0.113.%s|round3-capacity-user-%s|round3-capacity-password-%s\n' "$extra_index" "$extra_index" "$extra_index" >>"$extra_fixture"
	extra_index=$((extra_index + 1))
done
over_capacity=$(rpc_import_preview "$extra_fixture" "$revision")
assert_success "$over_capacity" "61-node preview"
record over-capacity-preview "$over_capacity"
[ "$(field "$over_capacity" '@.result.blocked')" = "true" ] || fail "61-node preview was not blocked"
rm -f "$extra_fixture"

poll_job "$job_id" bulk-import "$import_count" succeeded,failed
wait_nodes_settled bulk-import "$import_count"

if [ "$ALLOW_FAULTS" = "1" ]; then
	current=$(status); assert_success "$current" "pre-fault status"
	online_node=$(field "$current" '@.result.runtime.nodes[@.state="online"].node_id' | awk 'NR == 1 { print; exit }')
	[ -n "$online_node" ] || fail "full fault mode requires at least one online L2TP node"
	online_policy_id=$(field "$current" '@.result.desired.nodes[@.id="'$online_node'"].policy_id')
	case "$online_policy_id" in ''|*[!0-9]*) fail "online node has no valid policy id";; esac
	[ "$online_policy_id" -ge 1 ] && [ "$online_policy_id" -le 60 ] || fail "online node policy id is out of range"
	logical_interface=$(printf 'ppv2%04d' "$online_policy_id")
	revision=$(field "$current" '@.result.config.revision')
	bind=$(rpc_device_bind "$TEST_DEVICE_ID" "$online_node" "$revision"); assert_success "$bind" "test device bind"; record device-bind "$bind"
	bind_job=$(field "$bind" '@.result.job_id'); poll_job "$bind_job" device-bind 1 succeeded
	run_client_probe baseline

	log "injecting WAN down/up"
	start_client_watch wan-window
	/sbin/ifdown wan >/dev/null 2>&1 || fail "ifdown wan failed"
	WAN_WAS_DOWN=1
	wait_interface_up false || fail "WAN did not enter down state"
	fault_sample=$(client_watch_count)
	wait_client_watch_growth wan-window "$fault_sample"
	run_client_probe wan-down
	record_leak_counters wan-down
	/sbin/ifup wan >/dev/null 2>&1 || fail "ifup wan failed"
	wait_interface_up true || fail "WAN did not recover"
	WAN_WAS_DOWN=0
	wait_nodes_settled wan-recovery "$import_count"
	stop_client_watch wan-window
	run_client_probe wan-recovered
	record_leak_counters wan-recovered

	log "terminating proxypoold for procd recovery"
	daemon_pid=$(pidof proxypoold 2>/dev/null | awk '{print $1}')
	[ -n "$daemon_pid" ] || fail "proxypoold pid is missing"
	start_client_watch daemon-window
	kill -TERM "$daemon_pid" || fail "proxypoold TERM failed"
	wait_pid_gone "$daemon_pid" || fail "proxypoold old PID remained alive"
	fault_sample=$(client_watch_count)
	wait_client_watch_growth daemon-window "$fault_sample"
	wait_new_pid proxypoold "$daemon_pid" || fail "proxypoold PID did not change"
	status >/dev/null || fail "proxypoold did not recover"
	wait_nodes_settled daemon-recovery "$import_count"
	stop_client_watch daemon-window
	run_client_probe daemon-recovered
	record_leak_counters daemon-recovered

	log "terminating shared xl2tpd"
	xl2tp_pid=$(pidof xl2tpd 2>/dev/null | awk '{print $1}')
	[ -n "$xl2tp_pid" ] || fail "xl2tpd is missing before fault injection"
	start_client_watch xl2tp-window
	kill -TERM "$xl2tp_pid" || fail "xl2tpd TERM failed"
	wait_pid_gone "$xl2tp_pid" || fail "xl2tpd old PID remained alive"
	fault_sample=$(client_watch_count)
	wait_client_watch_growth xl2tp-window "$fault_sample"
	wait_new_pid xl2tpd "$xl2tp_pid" || fail "xl2tpd PID did not change"
	wait_nodes_settled xl2tp-recovery "$import_count"
	stop_client_watch xl2tp-window
	run_client_probe xl2tp-recovered
	record_leak_counters l2tp-recovered

	pppd_pid=$(session_pppd_pid "$logical_interface" || true)
	[ -n "$pppd_pid" ] || fail "bound node pppd is missing before fault injection"
	log "terminating bound-node pppd interface=$logical_interface pid=$pppd_pid"
	start_client_watch pppd-window
	kill -TERM "$pppd_pid" || fail "managed pppd TERM failed"
	wait_pid_gone "$pppd_pid" || fail "bound-node pppd old PID remained alive"
	fault_sample=$(client_watch_count)
	wait_client_watch_growth pppd-window "$fault_sample"
	new_pppd_pid=$(wait_new_session_pppd "$logical_interface" "$pppd_pid") || fail "bound-node pppd session did not restart"
	[ "$new_pppd_pid" != "$pppd_pid" ] || fail "bound-node pppd PID did not change"
	wait_nodes_settled pppd-recovery "$import_count"
	stop_client_watch pppd-window
	run_client_probe pppd-recovered
	record_leak_counters pppd-recovered

	start_client_watch services-window
	fault_sample=$(client_watch_count)
	/etc/init.d/network reload || fail "network reload failed"
	/etc/init.d/firewall reload || fail "firewall reload failed"
	wait_client_watch_growth services-window "$fault_sample"
	wait_nodes_settled services-reload "$import_count"
	stop_client_watch services-window
	run_client_probe services-reloaded
	record_leak_counters services-reloaded
else
	log "fault injection skipped; rerun with PROXYPOOL_ALLOW_FAULTS=1 on the test router"
fi

current=$(status); assert_success "$current" "post-fault status"; record post-fault-status "$current"
node_id=$(field "$current" '@.result.desired.nodes[0].id')
node_name=$(field "$current" '@.result.desired.nodes[0].name')
node_server=$(field "$current" '@.result.desired.nodes[0].server')
node_port=$(field "$current" '@.result.desired.nodes[0].port')
revision=$(field "$current" '@.result.config.revision')
[ -n "$node_id" ] || fail "imported node is missing"
edit_name="$node_name-round3-edit"
edit=$(rpc_node_save "$node_id" "$edit_name" "$node_server" "$node_port" "$revision")
assert_success "$edit" "node edit"
record node-edit "$edit"
revision=$(field "$edit" '@.result.config_revision')
edit_job=$(field "$edit" '@.result.job_id')
poll_job "$edit_job" node-edit 1 succeeded,failed

reconnect=$(rpc_node_action "$node_id" reconnect "$revision")
assert_success "$reconnect" "node reconnect"
record node-reconnect "$reconnect"
revision=$(field "$reconnect" '@.result.config_revision')
reconnect_job=$(field "$reconnect" '@.result.job_id')

final_name="$node_name-round3-final"
edit_after_reconnect=$(rpc_node_save "$node_id" "$final_name" "$node_server" "$node_port" "$revision")
assert_success "$edit_after_reconnect" "edit during reconnect"
record edit-during-reconnect "$edit_after_reconnect"
final_revision=$(field "$edit_after_reconnect" '@.result.config_revision')
final_edit_job=$(field "$edit_after_reconnect" '@.result.job_id')
poll_job "$reconnect_job" reconnect-before-edit 1 succeeded,failed,replaced
poll_job "$final_edit_job" edit-after-reconnect 1 succeeded,failed
after_edit=$(status); assert_success "$after_edit" "post-edit status"
[ "$(field "$after_edit" '@.result.config.revision')" = "$final_revision" ] || fail "stale reconnect overwrote the edited revision"
[ "$(field "$after_edit" '@.result.desired.nodes[@.id="'$node_id'"].name')" = "$final_name" ] || fail "stale reconnect overwrote the edited node"

revision=$final_revision
delete=$(rpc_node_delete "$node_id" "$revision")
assert_success "$delete" "node delete"
record node-delete "$delete"
delete_job=$(field "$delete" '@.result.job_id')
poll_job "$delete_job" node-delete 1 succeeded
after_delete=$(status)
if jsonfilter -s "$after_delete" -e '@.result.desired.nodes[*].id' 2>/dev/null | grep -qx "$node_id"; then fail "deleted node remained in desired state"; fi

snapshot_network after
record_leak_counters after
final=$(status); assert_success "$final" "final status"; record final-status "$final"

diagnostic=$(rpc_empty diagnostics.create); assert_success "$diagnostic" "diagnostic create"; record diagnostic-create "$diagnostic"
diagnostic_job=$(field "$diagnostic" '@.result.job_id')
[ -n "$diagnostic_job" ] || fail "diagnostic job id is missing"
diagnostic_deadline=$(( $(date +%s) + 90 ))
while [ "$(date +%s)" -lt "$diagnostic_deadline" ]; do
	diagnostic_status=$(rpc_diagnostic_get "$diagnostic_job"); assert_success "$diagnostic_status" "diagnostic status"
	diagnostic_state=$(field "$diagnostic_status" '@.result.state')
	case "$diagnostic_state" in ready) break;; failed) fail "diagnostic collection failed";; esac
	sleep 2
done
[ "${diagnostic_state:-}" = "ready" ] || fail "diagnostic collection timed out"
artifact_id=$(field "$diagnostic_status" '@.result.artifact.artifact_id')
claim=$(rpc_artifact_action diagnostics.claim "$artifact_id"); assert_success "$claim" "diagnostic claim"
artifact_path=$(field "$claim" '@.result.path')
[ "$artifact_path" = "/tmp/proxypool/diagnostics/$artifact_id.tar.gz" ] || fail "diagnostic claim returned an unsafe path"
[ -f "$artifact_path" ] || fail "diagnostic artifact is not a regular file"
gzip -t "$artifact_path" || fail "diagnostic artifact is not a valid gzip archive"
SECRET_PATTERNS=$(mktemp /tmp/proxypool-round3-secrets.XXXXXX)
chmod 600 "$SECRET_PATTERNS"
awk -F '|' '
	NF == 3 { print $2; print $3; next }
	NF == 4 && $2 ~ /^[0-9]+$/ { print $3; print $4; next }
	NF == 4 { print $2; print $3; next }
	NF == 5 { print $3; print $4 }
' "$VALID_IMPORT" | sort -u >"$SECRET_PATTERNS"
if gzip -dc "$artifact_path" | grep -a -F -f "$SECRET_PATTERNS" >/dev/null 2>&1; then fail "diagnostic artifact leaked fixture authentication data"; fi
release=$(rpc_artifact_action diagnostics.release "$artifact_id"); assert_success "$release" "diagnostic release"
[ ! -e "$artifact_path" ] || fail "diagnostic artifact remained after release"
second_claim=$(rpc_artifact_action diagnostics.claim "$artifact_id")
[ "$(field "$second_claim" '@.error.code')" = "not_found" ] || fail "diagnostic artifact was claimable twice"
rm -f "$SECRET_PATTERNS"; SECRET_PATTERNS=
log "removing all imported nodes through the daemon API"
cleanup_nodes 1 || fail "final node cleanup failed"
CLEANUP_COMPLETE=1
IMPORTED=0
clean_status=$(status); assert_success "$clean_status" "clean status"; record clean-status "$clean_status"
log "round 3 completed; inspect $REPORT_DIR"
