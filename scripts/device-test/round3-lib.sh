#!/bin/sh

# Side-effect-free decisions shared by the router harness and host tests.

round3_states_settled() {
	desired_count=$1 runtime_count=$2 states=$3
	[ "$runtime_count" -eq "$desired_count" ] || return 1
	[ "$desired_count" -eq 0 ] && return 0
	[ -n "$states" ] || return 1
	! printf '%s\n' "$states" | grep -Ev '^(online|failed|disabled|backoff)$' >/dev/null
}

round3_session_pppd_pid() {
	proc_root=$1 logical_interface=$2 pids=$3
	for candidate in $pids; do
		case "$candidate" in ''|*[!0-9]*) continue;; esac
		[ -r "$proc_root/$candidate/cmdline" ] || continue
		if tr '\000' '\n' <"$proc_root/$candidate/cmdline" 2>/dev/null | grep -Fx "$logical_interface" >/dev/null; then
			printf '%s\n' "$candidate"
			return 0
		fi
	done
	return 1
}

round3_tracked_current_ids() {
	tracked_file=$1 current_ids=$2
	[ -r "$tracked_file" ] || return 1
	for tracked_id in $(cat "$tracked_file"); do
		printf '%s\n' "$current_ids" | grep -Fx "$tracked_id" >/dev/null && printf '%s\n' "$tracked_id"
	done
	return 0
}

round3_wait_pid_gone() {
	pid=$1 attempts=$2 delay=$3 alive_fn=$4
	while [ "$attempts" -gt 0 ]; do
		"$alive_fn" "$pid" || return 0
		attempts=$((attempts - 1))
		[ "$attempts" -eq 0 ] || sleep "$delay"
	done
	return 1
}

round3_wait_counter_growth() {
	count_file=$1 fail_file=$2 baseline=$3 attempts=$4 delay=$5 alive_fn=$6 watcher_pid=$7
	while [ "$attempts" -gt 0 ]; do
		[ ! -s "$fail_file" ] || return 1
		count=$(cat "$count_file" 2>/dev/null || echo 0)
		case "$count" in ''|*[!0-9]*) return 1;; esac
		[ "$count" -gt "$baseline" ] && return 0
		"$alive_fn" "$watcher_pid" || return 1
		attempts=$((attempts - 1))
		[ "$attempts" -eq 0 ] || sleep "$delay"
	done
	return 1
}
