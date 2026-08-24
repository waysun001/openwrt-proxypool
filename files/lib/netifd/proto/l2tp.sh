#!/bin/sh

# OpenWrt 23.05's stock L2TP teardown waits forever for a PPP interface to
# disappear. ProxyPool can own dozens of independent LACs, so every shared
# xl2tpd operation and every device wait must have a hard deadline.

[ -n "${INCLUDE_ONLY:-}" ] || [ -x /usr/sbin/xl2tpd ] || exit 0

PROXYPOOL_L2TP_CONTROL="${PROXYPOOL_L2TP_CONTROL:-/usr/sbin/xl2tpd-control}"
PROXYPOOL_L2TP_TIMEOUT="${PROXYPOOL_L2TP_TIMEOUT:-/usr/bin/timeout}"
PROXYPOOL_L2TP_SLEEP="${PROXYPOOL_L2TP_SLEEP:-/bin/sleep}"
PROXYPOOL_L2TP_KILL="${PROXYPOOL_L2TP_KILL:-/bin/kill}"
PROXYPOOL_L2TP_SYS_CLASS_NET="${PROXYPOOL_L2TP_SYS_CLASS_NET:-/sys/class/net}"
PROXYPOOL_L2TP_PROC="${PROXYPOOL_L2TP_PROC:-/proc}"

proxypool_l2tp_control() {
	"$PROXYPOOL_L2TP_TIMEOUT" -s KILL 5 "$PROXYPOOL_L2TP_CONTROL" "$@"
}

proxypool_l2tp_wait_device_removed() {
	local device="$1"
	local limit="${2:-8}"
	local waited=0
	while [ -d "$PROXYPOOL_L2TP_SYS_CLASS_NET/$device" ] && [ "$waited" -lt "$limit" ]; do
		"$PROXYPOOL_L2TP_SLEEP" 1
		waited=$((waited + 1))
	done
	[ ! -d "$PROXYPOOL_L2TP_SYS_CLASS_NET/$device" ]
}

proxypool_l2tp_pid_owns_device() {
	local pid="$1"
	local device="$2"
	local command
	[ -r "$PROXYPOOL_L2TP_PROC/$pid/cmdline" ] || return 1
	command="|$(tr '\000' '|' <"$PROXYPOOL_L2TP_PROC/$pid/cmdline" 2>/dev/null)|" || return 1
	case "$command" in
		*"|pppol2tp.so|"*) ;;
		*) return 1 ;;
	esac
	case "$command" in
		*"|ifname|$device|"*) return 0 ;;
	esac
	return 1
}

proxypool_l2tp_stop_owned_ppp() {
	local device="$1"
	local path pid owned=""
	case "$device" in
		''|*[!A-Za-z0-9_.:-]*) return 1 ;;
	esac
	for path in "$PROXYPOOL_L2TP_PROC"/[0-9]*; do
		[ -d "$path" ] || continue
		pid=${path##*/}
		if proxypool_l2tp_pid_owns_device "$pid" "$device"; then
			owned="$owned $pid"
			"$PROXYPOOL_L2TP_KILL" -TERM "$pid" 2>/dev/null || true
		fi
	done
	[ -n "$owned" ] || return 0
	local attempt=0
	while [ "$attempt" -lt 3 ]; do
		local alive=""
		for pid in $owned; do
			proxypool_l2tp_pid_owns_device "$pid" "$device" && alive="$alive $pid"
		done
		[ -z "$alive" ] && return 0
		"$PROXYPOOL_L2TP_SLEEP" 1
		attempt=$((attempt + 1))
	done
	for pid in $owned; do
		proxypool_l2tp_pid_owns_device "$pid" "$device" || continue
		"$PROXYPOOL_L2TP_KILL" -KILL "$pid" 2>/dev/null || true
	done
	return 0
}

[ -n "${INCLUDE_ONLY:-}" ] || {
	. /lib/functions.sh
	. ../netifd-proto.sh
	init_proto "$@"
}

proto_l2tp_init_config() {
	proto_config_add_string "username"
	proto_config_add_string "password"
	proto_config_add_string "keepalive"
	proto_config_add_string "pppd_options"
	proto_config_add_boolean "ipv6"
	proto_config_add_int "mtu"
	proto_config_add_int "checkup_interval"
	proto_config_add_string "server"
	available=1
	no_device=1
	no_proto_task=1
	teardown_on_l3_link_down=1
}

proto_l2tp_setup() {
	local interface="$1"
	local optfile="/tmp/l2tp/options.${interface}"
	local ip serv_addr server host
	json_get_var server server
	host="${server%:*}"
	for ip in $(resolveip -t 5 "$host"); do
		( proto_add_host_dependency "$interface" "$ip" )
		serv_addr=1
	done
	[ -n "$serv_addr" ] || {
		echo "Could not resolve server address" >&2
		proto_notify_error "$interface" RESOLVE_FAILED
		proto_setup_failed "$interface"
		exit 1
	}

	if [ ! -p /var/run/xl2tpd/l2tp-control ] || [ -z "$(pidof xl2tpd)" ]; then
		"$PROXYPOOL_L2TP_TIMEOUT" -s KILL 10 /etc/init.d/xl2tpd restart || {
			echo "Cannot start xl2tpd." >&2
			proto_notify_error "$interface" XL2TPD_FAILED
			proto_setup_failed "$interface"
			exit 1
		}
		local wait_timeout=0
		while [ ! -p /var/run/xl2tpd/l2tp-control ]; do
			wait_timeout=$((wait_timeout + 1))
			[ "$wait_timeout" -gt 5 ] && {
				echo "Cannot find xl2tpd control file." >&2
				proto_notify_error "$interface" XL2TPD_FAILED
				proto_setup_failed "$interface"
				exit 1
			}
			"$PROXYPOOL_L2TP_SLEEP" 1
		done
	fi

	local ipv6 keepalive username password pppd_options mtu
	json_get_vars ipv6 keepalive username password pppd_options mtu
	[ "$ipv6" = 1 ] || ipv6=""
	local interval="${keepalive##*[, ]}"
	[ "$interval" != "$keepalive" ] || interval=5
	keepalive="${keepalive:+lcp-echo-interval $interval lcp-echo-failure ${keepalive%%[, ]*}}"
	username="${username:+user \"$username\" password \"$password\"}"
	ipv6="${ipv6:++ipv6}"
	mtu="${mtu:+mtu $mtu mru $mtu}"
	mkdir -p /tmp/l2tp
	cat <<EOF >"$optfile"
usepeerdns
nodefaultroute
ipparam "$interface"
ifname "l2tp-$interface"
ip-up-script /lib/netifd/ppp-up
ipv6-up-script /lib/netifd/ppp-up
ip-down-script /lib/netifd/ppp-down
ipv6-down-script /lib/netifd/ppp-down
lcp-max-terminate 0
$keepalive
$username
$ipv6
$mtu
$pppd_options
EOF
	proxypool_l2tp_control add-lac "l2tp-${interface}" "pppoptfile=${optfile}" "lns=${server}" || {
		echo "xl2tpd-control: Add l2tp-$interface failed" >&2
		proto_notify_error "$interface" INTERFACE_FAILED
		proto_setup_failed "$interface"
		exit 1
	}
	proxypool_l2tp_control connect-lac "l2tp-${interface}" || {
		proxypool_l2tp_control remove-lac "l2tp-${interface}" >/dev/null 2>&1 || true
		echo "xl2tpd-control: Connect l2tp-$interface failed" >&2
		proto_notify_error "$interface" CONNECT_FAILED
		proto_setup_failed "$interface"
		exit 1
	}
}

proto_l2tp_teardown() {
	local interface="$1"
	local device="l2tp-${interface}"
	local optfile="/tmp/l2tp/options.${interface}"
	rm -f "$optfile"
	if [ -p /var/run/xl2tpd/l2tp-control ]; then
		proxypool_l2tp_control disconnect-lac "l2tp-${interface}" >/dev/null 2>&1 || true
		proxypool_l2tp_control remove-lac "l2tp-${interface}" >/dev/null 2>&1 || true
	fi
	proxypool_l2tp_wait_device_removed "$device" 8 && return 0
	proxypool_l2tp_stop_owned_ppp "$device" || true
	proxypool_l2tp_wait_device_removed "$device" 3 && return 0
	echo "L2TP teardown timed out for $interface" >&2
	return 1
}

[ -n "${INCLUDE_ONLY:-}" ] || add_protocol l2tp
