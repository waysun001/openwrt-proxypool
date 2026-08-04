#!/bin/sh
set -u

LAN_ISOLATION="${PROXYPOOL_LAN_ISOLATION:-/usr/lib/proxypool/lan-isolation.sh}"
SLEEP="${PROXYPOOL_WORKER_SLEEP:-/bin/sleep}"
PROXYPOOL_INIT="${PROXYPOOL_INIT:-/etc/init.d/proxypool}"
DEFERRED_START_MARKER="${PROXYPOOL_DEFERRED_START_MARKER:-/var/run/proxypool.start-deferred}"
TRANSACTION_HELPER="${PROXYPOOL_TRANSACTION_HELPER:-/usr/lib/proxypool/proxypool-firewall-transaction}"
FIREWALL_DEFAULTS="${PROXYPOOL_FIREWALL_DEFAULTS:-/usr/lib/proxypool/proxypool-firewall-defaults}"
TRANSACTION_DIR="${PROXYPOOL_FIREWALL_TRANSACTION_DIR:-/etc/proxypool/firewall-transaction}"
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

firewall_journal_is_absent() {
	local journal_status
	if "$TRANSACTION_HELPER" journal-present; then
		return 1
	else
		journal_status=$?
	fi
	# journal-present uses status 1 for both exact absence and validation
	# failure.  Only an absent authority path permits a new live transaction.
	[ "$journal_status" -eq 1 ] || return 1
	[ ! -e "$TRANSACTION_DIR" ] && [ ! -L "$TRANSACTION_DIR" ]
}

ensure_firewall_activation() {
	case "$TRANSACTION_DIR" in
		/*) : ;;
		*) return 1 ;;
	esac
	[ "$TRANSACTION_DIR" != / ] || return 1
	[ -f "$TRANSACTION_HELPER" ] && [ ! -L "$TRANSACTION_HELPER" ] &&
		[ -x "$TRANSACTION_HELPER" ] || return 1
	firewall_journal_is_absent || return 1
	if "$TRANSACTION_HELPER" activation-current; then
		return 0
	fi
	[ -f "$FIREWALL_DEFAULTS" ] && [ ! -L "$FIREWALL_DEFAULTS" ] &&
		[ -x "$FIREWALL_DEFAULTS" ] || return 1
	PROXYPOOL_COLD_BOOT=0 "$FIREWALL_DEFAULTS" || return 1
	# Do not trust the activator's exit status alone.  Recheck both durable WAL
	# absence and the hash-bound runtime acknowledgement before daemon start.
	firewall_journal_is_absent && "$TRANSACTION_HELPER" activation-current
}

retry_deferred_start() {
	case "$DEFERRED_START_MARKER" in
		/*) : ;;
		*) return 1 ;;
	esac
	[ "$DEFERRED_START_MARKER" != / ] || return 1
	if [ ! -e "$DEFERRED_START_MARKER" ] && [ ! -L "$DEFERRED_START_MARKER" ]; then
		return 0
	fi
	[ -f "$DEFERRED_START_MARKER" ] && [ ! -L "$DEFERRED_START_MARKER" ] || return 1
	ensure_firewall_activation || return 1
	[ -f "$PROXYPOOL_INIT" ] && [ ! -L "$PROXYPOOL_INIT" ] && [ -x "$PROXYPOOL_INIT" ] || return 1
	"$PROXYPOOL_INIT" start || return 1
	[ ! -e "$DEFERRED_START_MARKER" ] && [ ! -L "$DEFERRED_START_MARKER" ]
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
		if ! retry_deferred_start; then
			log_failure "deferred ProxyPool start failed; retrying in $backoff seconds"
			wait_delay "$backoff" || exit 1
			[ "$backoff" -ge 30 ] || backoff=$((backoff * 2))
			[ "$backoff" -le 30 ] || backoff=30
			continue
		fi
		backoff=1
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
