#!/usr/bin/env sh
set -u

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
DNS_MANAGER="$ROOT/proxypool-core/files/dns-manager.sh"
STATUS_SCRIPT="$ROOT/proxypool-core/files/status.sh"
LUCI_CONTROLLER="$ROOT/luci-app-proxypool/luasrc/controller/proxypool.lua"
GLOBAL_JS="$ROOT/luci-app-proxypool/htdocs/luci-static/resources/proxypool-global.js"
MAIN_VIEW="$ROOT/luci-app-proxypool/luasrc/view/proxypool/main.htm"
TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM

BIN="$TEST_TMP/bin"
mkdir -p "$BIN"

fail() {
	printf '%s\n' "$*" >&2
	return 1
}

assert_file_line() {
	[ -f "$1" ] && [ "$(cat "$1")" = "$2" ] ||
		fail "expected $1 to contain exactly: $2"
}

assert_file_absent() {
	[ ! -e "$1" ] && [ ! -L "$1" ] || fail "expected path to be absent: $1"
}

assert_trace_line() {
	grep -Fqx -- "$2" "$1" || fail "missing trace line '$2' in $1"
}

assert_trace_absent() {
	if grep -Fq -- "$2" "$1"; then
		fail "unexpected trace fragment '$2' in $1"
	fi
}

cat >"$BIN/uci" <<'FAKE_UCI'
#!/usr/bin/env sh
set -u

quiet=0
if [ "${1-}" = -q ]; then
	quiet=1
	shift
fi
action=${1-}
[ "$#" -gt 0 ] && shift

if [ "${PROXYPOOL_TEST_UCI_MODE:-dns}" = status ]; then
	case "$action:${1-}" in
		show:proxypool)
			cat <<'EOF_STATUS_UCI'
proxypool.hostnode=client
proxypool.ipnode=client
EOF_STATUS_UCI
			exit 0
			;;
		get:proxypool.global.enabled) printf '1\n'; exit 0 ;;
		get:proxypool.hostnode.type|get:proxypool.ipnode.type) printf 'socks5\n'; exit 0 ;;
		get:proxypool.hostnode.name) printf 'hostname endpoint\n'; exit 0 ;;
		get:proxypool.ipnode.name) printf 'IPv4 endpoint\n'; exit 0 ;;
		get:proxypool.hostnode.server) printf 'node.example\n'; exit 0 ;;
		get:proxypool.ipnode.server) printf '203.0.113.9\n'; exit 0 ;;
		get:proxypool.hostnode.enabled|get:proxypool.ipnode.enabled) printf '0\n'; exit 0 ;;
		*) exit 1 ;;
	esac
fi

state_dir=$PROXYPOOL_TEST_UCI_STATE
trace=$PROXYPOOL_TEST_TRACE
printf 'uci:%s:%s\n' "$action" "$*" >>"$trace"

if [ "${PROXYPOOL_TEST_UCI_FAIL:-}" = "$action" ]; then
	exit 70
fi

case "$action" in
	show)
		target=${1-}
		case "$target" in
			dhcp|dhcp.dnsmasq_main)
				printf 'dhcp.dnsmasq_main=dnsmasq\n'
				if [ "${PROXYPOOL_TEST_UCI_DUPLICATE_DNSMASQ:-0}" = 1 ]; then
					printf 'dhcp.dnsmasq_other=dnsmasq\n'
				fi
				if [ -f "$state_dir/noresolv" ]; then
					printf "dhcp.dnsmasq_main.noresolv='%s'\n" "$(cat "$state_dir/noresolv")"
				fi
				if [ -f "$state_dir/server" ]; then
					printf "dhcp.dnsmasq_main.server='%s'\n" "$(cat "$state_dir/server")"
				fi
				if [ -f "$state_dir/port" ]; then
					printf "dhcp.dnsmasq_main.port='%s'\n" "$(cat "$state_dir/port")"
				fi
				;;
			*) exit 1 ;;
		esac
		;;
	get)
		case "${1-}" in
			*.noresolv) [ -f "$state_dir/noresolv" ] || exit 1; cat "$state_dir/noresolv" ;;
			*.server) [ -f "$state_dir/server" ] || exit 1; cat "$state_dir/server" ;;
			*.port) [ -f "$state_dir/port" ] || exit 1; cat "$state_dir/port" ;;
			*) exit 1 ;;
		esac
		;;
	set)
		assignment=${1-}
		case "$assignment" in
			*.noresolv=*) printf '%s\n' "${assignment#*=}" >"$state_dir/noresolv" ;;
			*.server=*) printf '%s\n' "${assignment#*=}" >"$state_dir/server" ;;
			*.port=*) printf '%s\n' "${assignment#*=}" >"$state_dir/port" ;;
			*) exit 1 ;;
		esac
		;;
	delete)
		case "${1-}" in
			*.server)
				[ -f "$state_dir/server" ] || exit 1
				rm -f "$state_dir/server"
				;;
			*) exit 1 ;;
		esac
		;;
	add_list)
		assignment=${1-}
		case "$assignment" in
			*.server=*) printf '%s\n' "${assignment#*=}" >"$state_dir/server" ;;
			*) exit 1 ;;
		esac
		;;
	commit)
		[ "${1-}" = dhcp ] || exit 1
		;;
	*)
		[ "$quiet" -eq 1 ] || printf 'unsupported fake UCI action: %s\n' "$action" >&2
		exit 2
		;;
esac
FAKE_UCI
chmod 755 "$BIN/uci"

cat >"$BIN/dnsmasq-init" <<'FAKE_DNSMASQ'
#!/usr/bin/env sh
set -u
printf 'dnsmasq:%s\n' "${1-}" >>"$PROXYPOOL_TEST_TRACE"
case "${1-}" in
	restart)
		[ "${PROXYPOOL_TEST_DNSMASQ_RESTART_FAIL:-0}" != 1 ] || exit 1
		printf '%s\n' "${PROXYPOOL_TEST_DNSMASQ_RESTART_STATE:-running}" >"$PROXYPOOL_TEST_DNSMASQ_STATE"
		;;
	stop)
		[ "${PROXYPOOL_TEST_DNSMASQ_STOP_FAIL:-0}" != 1 ] || exit 1
		printf 'stopped\n' >"$PROXYPOOL_TEST_DNSMASQ_STATE"
		;;
	status)
		case "$(cat "$PROXYPOOL_TEST_DNSMASQ_STATE" 2>/dev/null || true)" in
			stopped) exit 3 ;;
			unknown|'') exit 4 ;;
			*) exit 0 ;;
		esac
		;;
	running)
		[ "$(cat "$PROXYPOOL_TEST_DNSMASQ_STATE" 2>/dev/null || true)" = running ]
		;;
	*) exit 2 ;;
esac
FAKE_DNSMASQ
chmod 755 "$BIN/dnsmasq-init"

cat >"$BIN/nc" <<'FAKE_NC'
#!/usr/bin/env sh
exit 0
FAKE_NC
chmod 755 "$BIN/nc"

cat >"$BIN/nft" <<'FAKE_NFT'
#!/usr/bin/env sh
exit 0
FAKE_NFT
chmod 755 "$BIN/nft"

cat >"$BIN/ip" <<'FAKE_IP'
#!/usr/bin/env sh
exit 0
FAKE_IP
chmod 755 "$BIN/ip"

cat >"$BIN/nohup" <<'FAKE_NOHUP'
#!/usr/bin/env sh
exit 0
FAKE_NOHUP
chmod 755 "$BIN/nohup"

new_dns_fixture() {
	fixture=$1
	mkdir -p "$fixture/state" "$fixture/run" "$fixture/slp/client_a"
	printf '0\n' >"$fixture/state/noresolv"
	printf '/tmp/resolv.conf.d/resolv.conf.auto\n' >"$fixture/state/server"
	printf '53\n' >"$fixture/state/port"
	printf '5301\n' >"$fixture/run/dns-proxy-port"
	printf 'running\n' >"$fixture/dnsmasq.state"
	: >"$fixture/trace"
	: >"$fixture/dns.log"
}

run_dns() {
	fixture=$1
	shift
	env \
		PATH="$BIN:$PATH" \
		PROXYPOOL_UCI="$BIN/uci" \
		PROXYPOOL_DNSMASQ_INIT="$BIN/dnsmasq-init" \
		PROXYPOOL_DNS_RUN_DIR="$fixture/run" \
		PROXYPOOL_SLP_RUN_DIR="$fixture/slp" \
		PROXYPOOL_DNS_PORT_FILE="$fixture/run/dns-proxy-port" \
		PROXYPOOL_DNS_LOG_FILE="$fixture/dns.log" \
		PROXYPOOL_TEST_UCI_STATE="$fixture/state" \
		PROXYPOOL_TEST_TRACE="$fixture/trace" \
		PROXYPOOL_TEST_UCI_FAIL="${PROXYPOOL_TEST_UCI_FAIL:-}" \
		PROXYPOOL_TEST_UCI_DUPLICATE_DNSMASQ="${PROXYPOOL_TEST_UCI_DUPLICATE_DNSMASQ:-0}" \
		PROXYPOOL_TEST_DNSMASQ_RESTART_FAIL="${PROXYPOOL_TEST_DNSMASQ_RESTART_FAIL:-0}" \
		PROXYPOOL_TEST_DNSMASQ_RESTART_STATE="${PROXYPOOL_TEST_DNSMASQ_RESTART_STATE:-running}" \
		PROXYPOOL_TEST_DNSMASQ_STOP_FAIL="${PROXYPOOL_TEST_DNSMASQ_STOP_FAIL:-0}" \
		PROXYPOOL_TEST_DNSMASQ_STATE="$fixture/dnsmasq.state" \
		sh "$DNS_MANAGER" "$@"
}

assert_dns_unavailable() {
	fixture=$1
	assert_file_line "$fixture/state/noresolv" 1 || return 1
	assert_file_line "$fixture/state/port" 0 || return 1
	assert_file_absent "$fixture/state/server" || return 1
	assert_file_absent "$fixture/run/dns-proxy-port" || return 1
	assert_trace_absent "$fixture/trace" 'noresolv=0' || return 1
	assert_trace_absent "$fixture/trace" 'add_list' || return 1
}

test_restore_never_restores_wan_dns() {
	fixture="$TEST_TMP/restore"
	new_dns_fixture "$fixture"
	if run_dns "$fixture" restore >/dev/null 2>&1; then
		fail 'restore exposed safe convergence as an available DNS path' || return 1
	fi
	assert_dns_unavailable "$fixture" || return 1
	assert_trace_line "$fixture/trace" 'dnsmasq:restart'
}

test_configure_rejects_unowned_live_listener() {
	fixture="$TEST_TMP/unowned-listener"
	new_dns_fixture "$fixture"
	printf '%s\n' "$$" >"$fixture/slp/client_a/slp.pid"
	printf '5301\n' >"$fixture/slp/client_a/dns.port"
	if run_dns "$fixture" configure 5301 >/dev/null 2>&1; then
		fail 'configure exposed an unowned listener as an available DNS path' || return 1
	fi
	assert_dns_unavailable "$fixture"
}

test_configure_rejects_bad_ports_without_publishing_server() {
	for port in 0 65536 not-a-port '5301 trailing'; do
		case_dir=$(printf '%s' "$port" | tr -c 'A-Za-z0-9' '_')
		fixture="$TEST_TMP/bad-port-$case_dir"
		new_dns_fixture "$fixture"
		if run_dns "$fixture" configure "$port" >/dev/null 2>&1; then
			fail "configure exposed an unsafe DNS port as available: $port" || return 1
		fi
		assert_dns_unavailable "$fixture" || return 1
	done
}

test_configure_rejects_bad_legacy_port_file() {
	fixture="$TEST_TMP/bad-port-file"
	new_dns_fixture "$fixture"
	printf '%s\n' "$$" >"$fixture/slp/client_a/slp.pid"
	printf '5301 trailing-data\n' >"$fixture/slp/client_a/dns.port"
	if run_dns "$fixture" configure >/dev/null 2>&1; then
		fail 'configure exposed a malformed legacy DNS port file as available' || return 1
	fi
	assert_dns_unavailable "$fixture"
}

test_check_listener_loss_stays_fail_closed() {
	fixture="$TEST_TMP/check-loss"
	new_dns_fixture "$fixture"
	rm -f "$fixture/slp/client_a/slp.pid" "$fixture/slp/client_a/dns.port"
	if run_dns "$fixture" check >/dev/null 2>&1; then
		fail 'check exposed listener loss as an available DNS path' || return 1
	fi
	assert_dns_unavailable "$fixture"
}

test_restart_failure_stops_old_dnsmasq() {
	fixture="$TEST_TMP/restart-failure"
	new_dns_fixture "$fixture"
	if PROXYPOOL_TEST_DNSMASQ_RESTART_FAIL=1 run_dns "$fixture" restore >/dev/null 2>&1; then
		fail 'dnsmasq restart failure was reported as success' || return 1
	fi
	assert_dns_unavailable "$fixture" || return 1
	assert_trace_line "$fixture/trace" 'dnsmasq:restart' || return 1
	assert_trace_line "$fixture/trace" 'dnsmasq:stop' || return 1
	assert_trace_line "$fixture/trace" 'dnsmasq:status' || return 1
	assert_file_line "$fixture/dnsmasq.state" stopped
}

test_restart_requires_running_true() {
	for restart_state in configured-not-running active-no-instances; do
		fixture="$TEST_TMP/restart-$restart_state"
		new_dns_fixture "$fixture"
		if PROXYPOOL_TEST_DNSMASQ_RESTART_STATE="$restart_state" \
			run_dns "$fixture" enforce-unavailable >/dev/null 2>&1; then
			fail "dnsmasq state '$restart_state' was accepted as running" || return 1
		fi
		assert_dns_unavailable "$fixture" || return 1
		assert_trace_line "$fixture/trace" 'dnsmasq:restart' || return 1
		assert_trace_line "$fixture/trace" 'dnsmasq:running' || return 1
		assert_trace_line "$fixture/trace" 'dnsmasq:stop' || return 1
		assert_trace_line "$fixture/trace" 'dnsmasq:status' || return 1
		assert_file_line "$fixture/dnsmasq.state" stopped || return 1
	done
}

test_stop_failure_is_detected_after_restart_failure() {
	fixture="$TEST_TMP/stop-failure"
	new_dns_fixture "$fixture"
	if PROXYPOOL_TEST_DNSMASQ_RESTART_FAIL=1 PROXYPOOL_TEST_DNSMASQ_STOP_FAIL=1 \
		run_dns "$fixture" check >/dev/null 2>&1; then
		fail 'running old dnsmasq was reported as safely stopped' || return 1
	fi
	assert_trace_line "$fixture/trace" 'dnsmasq:stop' || return 1
	assert_trace_line "$fixture/trace" 'dnsmasq:status' || return 1
	assert_file_line "$fixture/dnsmasq.state" running
}

test_uci_failure_stops_old_dnsmasq() {
	fixture="$TEST_TMP/uci-failure"
	new_dns_fixture "$fixture"
	if PROXYPOOL_TEST_UCI_FAIL=commit run_dns "$fixture" check >/dev/null 2>&1; then
		fail 'UCI commit failure was reported as success' || return 1
	fi
	assert_trace_line "$fixture/trace" 'dnsmasq:stop'
}

test_multiple_dnsmasq_sections_are_not_guessed() {
	fixture="$TEST_TMP/duplicate-dnsmasq"
	new_dns_fixture "$fixture"
	if PROXYPOOL_TEST_UCI_DUPLICATE_DNSMASQ=1 run_dns "$fixture" restore >/dev/null 2>&1; then
		fail 'multiple dnsmasq sections were reported as safely configured' || return 1
	fi
	assert_trace_line "$fixture/trace" 'dnsmasq:stop' || return 1
	assert_trace_absent "$fixture/trace" 'uci:set:'
}

test_dns_manager_status_is_explicitly_unavailable() {
	fixture="$TEST_TMP/manager-status"
	new_dns_fixture "$fixture"
	status_output="$fixture/status.out"
	if run_dns "$fixture" status >"$status_output" 2>/dev/null; then
		fail 'DNS manager status returned success before a safe data plane exists' || return 1
	fi
	assert_file_line "$status_output" dns_path_unavailable
}

test_internal_enforcement_reports_safe_convergence() {
	fixture="$TEST_TMP/internal-enforce"
	new_dns_fixture "$fixture"
	run_dns "$fixture" enforce-unavailable >/dev/null 2>&1 ||
		fail 'internal fail-closed enforcement did not report safe convergence' || return 1
	assert_dns_unavailable "$fixture" || return 1
	assert_trace_line "$fixture/trace" 'dnsmasq:restart' || return 1
	assert_trace_line "$fixture/trace" 'dnsmasq:running'
}

test_luci_actions_stop_on_dns_rejection() {
	grep -Fq 'data-api-read=' "$MAIN_VIEW" || fail 'LuCI page does not use the read-only daemon route' || return 1
	grep -Fq 'data-api-write=' "$MAIN_VIEW" || fail 'LuCI page does not use the CSRF-protected write route' || return 1
	if grep -Fq 'sequentialConnect' "$MAIN_VIEW"; then
		fail 'LuCI still drives sequential node connection from the browser' || return 1
	fi
}

test_shell_status_reports_dns_and_endpoint_limits() {
	fixture="$TEST_TMP/status"
	mkdir -p "$fixture/run"
	status_output="$fixture/status.json"
	if ! env \
		PATH="$BIN:$PATH" \
		PROXYPOOL_STATUS_RUN_DIR="$fixture/run" \
		PROXYPOOL_STATUS_LOG_FILE="$fixture/status.log" \
		PROXYPOOL_STATUS_PROBE_COMMAND="$fixture/no-probe" \
		PROXYPOOL_TEST_UCI_MODE=status \
		sh "$STATUS_SCRIPT" get >"$status_output"; then
		fail 'status.sh get failed under the controlled fixture' || return 1
	fi
	grep -Fq '"dns_path_status": "dns_path_unavailable"' "$status_output" ||
		fail 'shell status omitted dns_path_unavailable' || return 1
	grep -Fq '"internet_ready": false' "$status_output" ||
		fail 'shell status falsely claimed internet readiness' || return 1
	[ "$(grep -Fc '"endpoint_resolution": "dns_path_unavailable"' "$status_output")" -eq 1 ] ||
		fail 'hostname endpoint did not report unavailable safe resolution' || return 1
	[ "$(grep -Fc '"endpoint_resolution": "literal_ipv4"' "$status_output")" -eq 1 ] ||
		fail 'strict literal IPv4 endpoint classification is missing'
}

test_luci_status_contract_exposes_dns_failure() {
	grep -Fq 'status = "status.get"' "$LUCI_CONTROLLER" ||
		fail 'LuCI status no longer comes from the fail-closed daemon' || return 1
	grep -Fq 'post("api_write")' "$LUCI_CONTROLLER" ||
		fail 'LuCI mutations bypass dispatcher POST security' || return 1
	for forbidden in dnsmasq dns-manager.sh 'luci.model.uci' 'uci:set' 'uci:commit' 'os.execute'; do
		if grep -Fq "$forbidden" "$LUCI_CONTROLLER"; then
			fail "LuCI directly mutates DNS or UCI state: $forbidden" || return 1
		fi
	done
	grep -Fq 'data-status-url="<%=url([[admin]], [[services]], [[proxypool]], [[api]], [[read]])%>?action=status"' "$MAIN_VIEW" ||
		fail 'global menu template does not generate the read-only V2 status route' || return 1
	grep -Fq "menu.getAttribute('data-status-url')" "$GLOBAL_JS" ||
		fail 'global menu script does not read the template-generated status route' || return 1
	grep -Fq 'window.fetch(statusURL' "$GLOBAL_JS" ||
		fail 'global menu script does not request the template-generated status route'
}

failures=0
for test_name in \
	test_restore_never_restores_wan_dns \
	test_configure_rejects_unowned_live_listener \
	test_configure_rejects_bad_ports_without_publishing_server \
	test_configure_rejects_bad_legacy_port_file \
	test_check_listener_loss_stays_fail_closed \
	test_restart_failure_stops_old_dnsmasq \
	test_restart_requires_running_true \
	test_stop_failure_is_detected_after_restart_failure \
	test_uci_failure_stops_old_dnsmasq \
	test_multiple_dnsmasq_sections_are_not_guessed \
	test_dns_manager_status_is_explicitly_unavailable \
	test_internal_enforcement_reports_safe_convergence \
	test_luci_actions_stop_on_dns_rejection \
	test_shell_status_reports_dns_and_endpoint_limits \
	test_luci_status_contract_exposes_dns_failure; do
	if ("$test_name"); then
		printf 'PASS: %s\n' "$test_name"
	else
		failures=$((failures + 1))
		printf 'FAIL: %s\n' "$test_name" >&2
	fi
done

[ "$failures" -eq 0 ] || {
	printf 'DNS fail-closed tests failed: %s\n' "$failures" >&2
	exit 1
}

printf 'DNS fail-closed tests passed\n'
