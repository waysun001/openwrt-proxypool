#!/bin/sh
set -u

LAN_ISOLATION="${PROXYPOOL_LAN_ISOLATION:-/usr/lib/proxypool/lan-isolation.sh}"
SLEEP="${PROXYPOOL_WORKER_SLEEP:-/bin/sleep}"
SLEEP_PID=
WAKE_PENDING=0

log_failure() {
	echo "ProxyPool LAN convergence worker: $*" >&2
}

publish_pending_on_exit() {
	status=$?
	trap - EXIT HUP INT TERM USR1
	if [ -n "$SLEEP_PID" ]; then
		kill "$SLEEP_PID" >/dev/null 2>&1 || :
		wait "$SLEEP_PID" >/dev/null 2>&1 || :
	fi
	"$LAN_ISOLATION" request >/dev/null 2>&1 ||
		log_failure 'could not retain pending state while exiting'
	exit "$status"
}

terminate_worker() {
	exit "$1"
}

wake_worker() {
	WAKE_PENDING=1
	if [ -n "$SLEEP_PID" ]; then
		kill "$SLEEP_PID" >/dev/null 2>&1 || :
	fi
}

wait_delay() {
	delay=$1
	[ -x "$SLEEP" ] || {
		log_failure "sleep is not executable: $SLEEP"
		return 1
	}
	WAKE_PENDING=0
	"$SLEEP" "$delay" &
	SLEEP_PID=$!
	wait "$SLEEP_PID"
	wait_status=$?
	SLEEP_PID=
	if [ "$WAKE_PENDING" -eq 1 ]; then
		WAKE_PENDING=0
		return 0
	fi
	return "$wait_status"
}

trap publish_pending_on_exit EXIT
trap 'terminate_worker 129' HUP
trap 'terminate_worker 130' INT
trap 'terminate_worker 143' TERM
trap wake_worker USR1

[ -f "$LAN_ISOLATION" ] && [ ! -L "$LAN_ISOLATION" ] && [ -x "$LAN_ISOLATION" ] || {
	log_failure "LAN isolation helper is missing or unsafe: $LAN_ISOLATION"
	exit 1
}

"$LAN_ISOLATION" request || {
	log_failure 'initial convergence request failed'
	exit 1
}

backoff=1
while :; do
	if "$LAN_ISOLATION" readiness; then
		if "$LAN_ISOLATION" verify; then
			wait_delay 30 || {
				log_failure 'periodic audit sleep failed'
				exit 1
			}
			continue
		fi
		if ! "$LAN_ISOLATION" request; then
			log_failure 'could not publish drift detected by periodic audit'
			wait_delay "$backoff" || exit 1
			[ "$backoff" -ge 30 ] || backoff=$((backoff * 2))
			[ "$backoff" -le 30 ] || backoff=30
		fi
		continue
	fi

	if "$LAN_ISOLATION" reconcile; then
		backoff=1
		continue
	fi

	log_failure "reconciliation failed; retrying in $backoff seconds"
	wait_delay "$backoff" || {
		log_failure 'reconciliation backoff sleep failed'
		exit 1
	}
	[ "$backoff" -ge 30 ] || backoff=$((backoff * 2))
	[ "$backoff" -le 30 ] || backoff=30
done
