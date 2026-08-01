#!/bin/sh
# ProxyPool DNS compatibility gate.
#
# Phase 1 deliberately has no publishable DNS data plane.  The legacy SLP files
# prove only that a PID existed and a port was assigned; they do not bind the
# process executable, configuration, generation, owner, or listener socket to
# ProxyPool.  Consequently configure/check/restore all converge dnsmasq to an
# base-UCI fallback-disabled state while the guardian closes LAN TCP/UDP 53.
# noresolv plus deleting `server` does not prove that serversfile/confdir or
# other dnsmasq fragments contain no upstream.  A later owned DNS data plane
# must validate every configuration source and replace this gate as one atomic
# publication; this script must never infer ownership from a live PID or port.

set -f

RUN_DIR=${PROXYPOOL_DNS_RUN_DIR:-/var/run/proxypool}
DNS_PORT_FILE=${PROXYPOOL_DNS_PORT_FILE:-$RUN_DIR/dns-proxy-port}
LOG_FILE=${PROXYPOOL_DNS_LOG_FILE:-/var/log/proxypool.log}
UCI=${PROXYPOOL_UCI:-uci}
DNSMASQ_INIT=${PROXYPOOL_DNSMASQ_INIT:-/etc/init.d/dnsmasq}
DNS_PATH_STATUS=dns_path_unavailable

log_info() {
	printf '[%s] [dns] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*" >>"$LOG_FILE" 2>/dev/null || :
}

log_error() {
	printf '[%s] [dns] [ERROR] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*" >>"$LOG_FILE" 2>/dev/null || :
}

stop_dnsmasq() {
	"$DNSMASQ_INIT" stop >/dev/null 2>&1 || :
	"$DNSMASQ_INIT" status >/dev/null 2>&1
	status_result=$?
	# OpenWrt 23.05.3 rc.common/procd returns exactly 3 when the service is
	# absent.  Exit 0 means it still exists; 4/other values are unknown and
	# must not be guessed as stopped.
	if [ "$status_result" -eq 3 ]; then
		return 0
	fi
	log_error "Could not prove dnsmasq stopped (status=$status_result)"
	return 1
}

restart_dnsmasq() {
	"$DNSMASQ_INIT" restart >/dev/null 2>&1 || return 1
	"$DNSMASQ_INIT" running >/dev/null 2>&1
	[ "$?" -eq 0 ]
}

clear_legacy_dns_claim() {
	if [ -e "$DNS_PORT_FILE" ] || [ -L "$DNS_PORT_FILE" ]; then
		if ! rm -f -- "$DNS_PORT_FILE"; then
			log_error "Could not remove obsolete DNS listener claim: $DNS_PORT_FILE"
			return 1
		fi
	fi
	return 0
}

# Print the one and only dnsmasq UCI section.  Multiple/missing sections are
# not guessed because changing the wrong instance could leave a WAN resolver
# active.  awk excludes option lines by requiring exactly one dot in the key.
find_unique_dnsmasq_section() {
	sections=$(
		"$UCI" -q show dhcp 2>/dev/null |
			awk -F= '$2 == "dnsmasq" && $1 ~ /^dhcp\.[^.]+$/ { print $1 }'
	) || return 1

	set -- $sections
	[ "$#" -eq 1 ] || return 1
	case "$1" in
		dhcp.*) printf '%s\n' "$1" ;;
		*) return 1 ;;
	esac
}

write_unavailable_dnsmasq_config() {
	dnsmasq_section=$(find_unique_dnsmasq_section) || return 1

	"$UCI" set "${dnsmasq_section}.noresolv=1" >/dev/null 2>&1 || return 1
	# A missing option is already the desired state.  Any other delete failure
	# is caught by the post-commit, full-section verification below.
	"$UCI" -q delete "${dnsmasq_section}.server" >/dev/null 2>&1 || :
	"$UCI" commit dhcp >/dev/null 2>&1 || return 1

	verified_section=$(find_unique_dnsmasq_section) || return 1
	[ "$verified_section" = "$dnsmasq_section" ] || return 1
	noresolv=$("$UCI" -q get "${dnsmasq_section}.noresolv" 2>/dev/null) || return 1
	[ "$noresolv" = 1 ] || return 1
	section_dump=$("$UCI" -q show "$dnsmasq_section" 2>/dev/null) || return 1
	if printf '%s\n' "$section_dump" | grep -Fq "${dnsmasq_section}.server="; then
		return 1
	fi
	return 0
}

enforce_dns_unavailable() {
	legacy_claim_ok=1
	clear_legacy_dns_claim || legacy_claim_ok=0

	if ! write_unavailable_dnsmasq_config; then
		log_error 'Could not persist noresolv=1 with no explicit UCI server; stopping dnsmasq'
		stop_dnsmasq || log_error 'dnsmasq stop verification failed; guardian DNS input must remain closed'
		return 1
	fi

	if ! restart_dnsmasq; then
		log_error 'dnsmasq restart failed; stopping the old process to prevent WAN DNS fallback'
		stop_dnsmasq || log_error 'dnsmasq stop verification failed; guardian DNS input must remain closed'
		return 1
	fi

	[ "$legacy_claim_ok" -eq 1 ] || log_error 'Obsolete DNS listener claim remains, but it is never trusted'
	log_info 'DNS path unavailable: base UCI fallback disabled and LAN TCP/UDP 53 remains closed'
	# Internal callers may use this success only to confirm safe convergence.
	# It does not mean that an Internet DNS path exists.
	return 0
}

case "${1-}" in
	configure|restore|check)
		# Explicit ports and legacy PID/port files are intentionally ignored.  No
		# Phase 1 ownership manifest can authenticate them.
		enforce_dns_unavailable
		# The public compatibility actions always report DNS path failure, even
		# after successfully converging base UCI while guardian keeps LAN DNS closed.
		# Backend admission must treat this as unavailable and must not publish
		# client access.
		exit 1
		;;
	enforce-unavailable)
		# Private control-plane action: return zero only when noresolv=1, no
		# server, restart, and strict procd running verification all succeeded.
		enforce_dns_unavailable
		;;
	status)
		printf '%s\n' "$DNS_PATH_STATUS"
		exit 1
		;;
	*)
		echo "Usage: $0 {configure|restore|check|status|enforce-unavailable} [ignored_legacy_port]"
		exit 2
		;;
esac
