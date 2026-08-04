#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
GUARD="$ROOT/proxypool-core/files/proxypool-guard.nft"
INPUT_GATE="$ROOT/proxypool-core/files/proxypool-fw4-input-gate.nft"
FORWARD_GATE="$ROOT/proxypool-core/files/proxypool-fw4-forward-gate.nft"
GUARD_INIT="$ROOT/proxypool-core/files/proxypool-guard.init"
GUARD_RESYNC="$ROOT/proxypool-core/files/guard-resync.sh"
LAN_WORKER_SOURCE="$ROOT/proxypool-core/files/lan-isolation-worker.sh"
STAGED_CHECKER="$ROOT/proxypool-core/files/proxypool-fw4-check-staged"
TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM

fail() {
	printf 'guardian contract: %s\n' "$*" >&2
	exit 1
}

require_file() {
	[ -f "$1" ] || fail "missing production file: $1"
}

normalize() {
	sed 's/[[:space:]]*#.*$//' "$1" |
		tr '\t' ' ' |
		sed 's/[[:space:]][[:space:]]*/ /g; s/^ //; s/ $//; /^[[:space:]]*$/d'
}

require_fixed() {
	file=$1
	text=$2
	description=$3
	grep -Fq "$text" "$file" || fail "$description"
}

reject_regex() {
	file=$1
	pattern=$2
	description=$3
	if grep -Eq "$pattern" "$file"; then
		fail "$description"
	fi
}

require_regex() {
	file=$1
	pattern=$2
	description=$3
	grep -Eq "$pattern" "$file" || fail "$description"
}

require_rule() {
	file=$1
	description=$2
	shift 2
	found=0
	while IFS= read -r line; do
		matched=1
		for token in "$@"; do
			case "$line" in
				*"$token"*) : ;;
				*) matched=0; break ;;
			esac
		done
		[ "$matched" -eq 0 ] || found=$((found + 1))
	done <"$file"
	[ "$found" -eq 1 ] || fail "$description (matching rules: $found)"
}

rule_line() {
	file=$1
	description=$2
	shift 2
	found=0
	line_number=0
	matched_line=
	while IFS= read -r line; do
		line_number=$((line_number + 1))
		matched=1
		for token in "$@"; do
			case "$line" in
				*"$token"*) : ;;
				*) matched=0; break ;;
			esac
		done
		if [ "$matched" -eq 1 ]; then
			found=$((found + 1))
			matched_line=$line_number
		fi
	done <"$file"
	[ "$found" -eq 1 ] || fail "$description (matching rules: $found)"
	printf '%s\n' "$matched_line"
}

first_line() {
	grep -nF "$2" "$1" | sed -n '1s/:.*//p'
}

assert_transaction_order() {
	file=$1
	family=$2
	table=$3
	declare_line=$(first_line "$file" "table $family $table;")
	delete_line=$(first_line "$file" "delete table $family $table;")
	create_line=$(first_line "$file" "table $family $table {")
	[ -n "$declare_line" ] || fail "$family $table reload does not first ensure the table exists"
	[ -n "$delete_line" ] || fail "$family $table reload does not delete the prior table"
	[ -n "$create_line" ] || fail "$family $table reload does not recreate the table"
	[ "$declare_line" -lt "$delete_line" ] && [ "$delete_line" -lt "$create_line" ] ||
		fail "$family $table reload is not ordered as ensure/delete/recreate"
}

extract_chain() {
	file=$1
	chain=$2
	output=$3
	awk -v wanted="$chain" '
	function braces(text, open, closing) {
		open = gsub(/\{/, "{", text)
		closing = gsub(/\}/, "}", text)
		return open - closing
		}
		!inside && index($0, "chain " wanted " {") {
			inside=1
			depth=braces($0)
			print
			next
		}
		inside {
			depth += braces($0)
			print
			if (depth == 0) exit
		}
	' "$file" >"$output"
	[ -s "$output" ] || fail "missing chain $chain"
}

assert_final_rule() {
	block=$1
	expected=$2
	description=$3
	last_rule=$(sed '1d; $d; /^type filter hook /d; /^[[:space:]]*$/d' "$block" | tail -n 1)
	[ "$last_rule" = "$expected" ] || fail "$description (last rule: ${last_rule:-<none>})"
}

assert_chain_shape() {
	block=$1
	expected_rules=$2
	expected_accepts=$3
	description=$4
	rules="$TEST_TMP/chain-shape.$$"
	sed '1d; $d; /^type filter hook /d; /^[[:space:]]*$/d' "$block" >"$rules"
	actual_rules=$(wc -l <"$rules" | tr -d ' ')
	[ "$actual_rules" -eq "$expected_rules" ] ||
		fail "$description has unexpected extra or missing rules (expected $expected_rules, got $actual_rules)"
	actual_accepts=$(grep -Ec '(^|[[:space:]])accept([[:space:]]|$)' "$rules" || true)
	[ "$actual_accepts" -eq "$expected_accepts" ] ||
		fail "$description has an unexpected accept verdict (expected $expected_accepts, got $actual_accepts)"
	if grep -Eq '(^|[[:space:]])(return|jump|goto|continue|queue)([[:space:]]|$)|(^|[[:space:]])v(map|erdict)([[:space:]]|$)' "$rules"; then
		fail "$description contains a control-flow verdict that can bypass its exact allowlist"
	fi
}

require_file "$GUARD"
require_file "$INPUT_GATE"
require_file "$FORWARD_GATE"
require_file "$GUARD_INIT"
require_file "$GUARD_RESYNC"
require_file "$LAN_WORKER_SOURCE"
require_file "$STAGED_CHECKER"

GUARD_NORM="$TEST_TMP/guard.norm"
INPUT_NORM="$TEST_TMP/input-gate.norm"
FORWARD_NORM="$TEST_TMP/forward-gate.norm"
GUARD_FLAT="$TEST_TMP/guard.flat"
normalize "$GUARD" >"$GUARD_NORM"
normalize "$INPUT_GATE" >"$INPUT_NORM"
normalize "$FORWARD_GATE" >"$FORWARD_NORM"
tr '\n' ' ' <"$GUARD_NORM" | sed 's/[[:space:]][[:space:]]*/ /g' >"$GUARD_FLAT"

# Reload starts from empty authorization sets/maps.  The leading empty
# declaration makes delete/recreate idempotent even after a cold boot.
assert_transaction_order "$GUARD_NORM" inet proxypool_guard
assert_transaction_order "$GUARD_NORM" bridge proxypool_l2_guard

require_regex "$GUARD_FLAT" 'set v1_l2tp_paths \{[^}]*type ipv4_addr[[:space:]]*\.[[:space:]]*ifname[[:space:]]*;' \
	'V1 L2TP authorization is not keyed by exact IPv4 and output interface'
require_regex "$GUARD_FLAT" 'set v1_tcp_redirects \{[^}]*type ipv4_addr[[:space:]]*\.[[:space:]]*inet_service[[:space:]]*;' \
	'V1 proxy authorization is not keyed by exact IPv4 and listener port'
require_regex "$GUARD_FLAT" 'set v2_l2tp_paths \{[^}]*type ether_addr[[:space:]]*\.[[:space:]]*ipv4_addr[[:space:]]*\.[[:space:]]*ifname[[:space:]]*;' \
	'V2 L2TP authorization is not keyed by exact MAC, IPv4 and output interface'
require_regex "$GUARD_FLAT" 'set v2_l2tp_return_paths \{[^}]*type ipv4_addr[[:space:]]*\.[[:space:]]*ifname[[:space:]]*;' \
	'V2 L2TP return authorization is not derived as exact IPv4 and input interface'
require_regex "$GUARD_FLAT" 'set v2_tcp_redirects \{[^}]*type ether_addr[[:space:]]*\.[[:space:]]*ipv4_addr[[:space:]]*\.[[:space:]]*inet_service[[:space:]]*;' \
	'V2 proxy authorization is not keyed by exact MAC, IPv4 and listener port'
require_regex "$GUARD_FLAT" 'set v2_dns_clients \{[^}]*type ether_addr[[:space:]]*\.[[:space:]]*ipv4_addr[[:space:]]*;' \
	'V2 router DNS admission is not keyed by exact MAC and IPv4'
require_regex "$GUARD_FLAT" 'map v2_policy_marks \{[^}]*type ether_addr[[:space:]]*\.[[:space:]]*ipv4_addr[[:space:]]*:[[:space:]]*mark[[:space:]]*;' \
	'V2 policy mark map is not keyed by exact MAC and IPv4'

tuple_count=$(grep -Eo '(set|map) (v1_l2tp_paths|v1_tcp_redirects|v2_l2tp_paths|v2_l2tp_return_paths|v2_tcp_redirects|v2_dns_clients|v2_policy_marks)[[:space:]]*\{' "$GUARD_FLAT" | wc -l | tr -d ' ')
[ "$tuple_count" -eq 7 ] || fail "expected exactly seven named authorization sets/maps, found $tuple_count"
for tuple_set in v1_l2tp_paths v1_tcp_redirects v2_l2tp_paths v2_l2tp_return_paths v2_tcp_redirects v2_dns_clients v2_policy_marks; do
	reject_regex "$GUARD_FLAT" "set $tuple_set \\{[^}]*elements[[:space:]]*=" \
		"$tuple_set is pre-authorized instead of empty after reload"
done
for expiring_set in v2_l2tp_paths v2_l2tp_return_paths v2_tcp_redirects v2_dns_clients v2_policy_marks; do
	require_regex "$GUARD_FLAT" "(set|map) $expiring_set \\{[^}]*flags timeout[[:space:]]*;[^}]*timeout 20s[[:space:]]*;" \
		"$expiring_set is not a 20-second crash-expiring lease"
done

# Guardian is a later, independent base-chain verdict and never joins the V1
# inet proxypool/ip proxypool_nat tables.
require_fixed "$GUARD_NORM" 'table inet proxypool_guard {' 'missing independent inet guardian table'
require_fixed "$GUARD_NORM" 'table bridge proxypool_l2_guard {' 'missing independent bridge guardian table'
reject_regex "$GUARD_NORM" '^table (inet proxypool|ip proxypool_nat)[ ;{]' 'guardian writes into a V1-owned table'
[ "$(grep -Fxc 'type filter hook input priority 10; policy drop;' "$GUARD_NORM")" -eq 1 ] ||
	fail 'guardian input must have exactly one priority +10 base chain'
[ "$(grep -Fxc 'type filter hook forward priority 10; policy drop;' "$GUARD_NORM")" -eq 2 ] ||
	fail 'inet and bridge guardians must each have a priority +10 forward base chain'
[ "$(grep -Fxc 'type filter hook prerouting priority -310; policy accept;' "$GUARD_NORM")" -eq 1 ] ||
	fail 'guardian must have exactly one br-lan mark scrub chain at priority -310'

extract_chain "$GUARD_NORM" guard_prerouting "$TEST_TMP/guard-prerouting.block"
extract_chain "$GUARD_NORM" guard_input "$TEST_TMP/guard-input.block"
extract_chain "$GUARD_NORM" guard_forward "$TEST_TMP/guard-forward.block"
extract_chain "$GUARD_NORM" guard_l2_forward "$TEST_TMP/guard-l2-forward.block"
assert_chain_shape "$TEST_TMP/guard-prerouting.block" 2 0 guard_prerouting
assert_chain_shape "$TEST_TMP/guard-input.block" 10 8 guard_input
assert_chain_shape "$TEST_TMP/guard-forward.block" 12 4 guard_forward
assert_chain_shape "$TEST_TMP/guard-l2-forward.block" 1 0 guard_l2_forward
require_fixed "$TEST_TMP/guard-prerouting.block" \
	'type filter hook prerouting priority -310; policy accept;' \
	'guard_prerouting is not the unique priority -310 mark scrub chain'

# Router input whitelist: DHCP, LuCI HTTP/HTTPS, and DNS only for a current
# daemon-owned MAC+IPv4 lease. SSH is intentionally absent. Transparent proxy input remains a separate exact tuple path
# requiring mark provenance and DNAT provenance together.
require_rule "$TEST_TMP/guard-input.block" 'missing exact DHCP input allowance' \
	'iifname "br-lan"' 'meta nfproto ipv4' 'udp sport 68' 'udp dport 67' 'accept'
require_rule "$TEST_TMP/guard-input.block" 'management input is not limited to TCP 80/443' \
	'iifname "br-lan"' 'meta nfproto ipv4' 'ip daddr 192.168.9.1' 'tcp dport' '80' '443' 'accept'
reject_regex "$TEST_TMP/guard-input.block" '(^|[^0-9])22([^0-9]|$)' 'SSH port 22 is present in the LAN input whitelist'
require_rule "$TEST_TMP/guard-input.block" 'router DNS is not limited to an admitted MAC+IPv4 tuple' \
	'iifname "br-lan"' 'meta nfproto ipv4' 'ip daddr 192.168.9.1' \
	'ether saddr . ip saddr @v2_dns_clients' 'tcp, udp' 'th dport 53' 'accept'

while IFS= read -r input_accept; do
	case "$input_accept" in
		*'udp sport 68'*'udp dport 67'*'accept'*) : ;;
		*'tcp dport {'*'80'*'443'*'accept'*)
			case "$input_accept" in
				*'ip daddr 192.168.9.1'*) : ;;
				*) fail "router service allowance is not bound to the phase-1 management IP: $input_accept" ;;
			esac
			;;
		*'ether saddr . ip saddr @v2_dns_clients'*'th dport 53'*'accept'*) : ;;
	esac
done <"$TEST_TMP/guard-input.block"

dhcp_input_line=$(rule_line "$TEST_TMP/guard-input.block" 'missing exact DHCP input allowance' \
	'iifname "br-lan"' 'meta nfproto ipv4' 'udp sport 68' 'udp dport 67' 'accept')
management_input_line=$(rule_line "$TEST_TMP/guard-input.block" 'management input is not limited to TCP 80/443' \
	'iifname "br-lan"' 'meta nfproto ipv4' 'ip daddr 192.168.9.1' 'tcp dport' '80' '443' 'accept')
dns_input_line=$(rule_line "$TEST_TMP/guard-input.block" 'router DNS is not limited to an admitted MAC+IPv4 tuple' \
	'iifname "br-lan"' 'ip daddr 192.168.9.1' 'ether saddr . ip saddr @v2_dns_clients' 'th dport 53' 'accept')
blocked_input_line=$(rule_line "$TEST_TMP/guard-input.block" \
	'original special-use destinations can be hidden by transparent DNAT' \
	'iifname "br-lan"' 'meta nfproto ipv4' 'ct original ip daddr @blocked_v4_destinations' 'drop')
for whitelist_line in "$dhcp_input_line" "$management_input_line" "$dns_input_line"; do
	[ "$whitelist_line" -lt "$blocked_input_line" ] ||
		fail 'the explicit router whitelist must precede the original-destination safety drop'
done

MARK_MASK='0x00ff0000'
MAGIC="meta mark & $MARK_MASK == 0x005a0000"
require_rule "$TEST_TMP/guard-prerouting.block" 'br-lan prerouting does not clear the complete ProxyPool policy mark' \
	'iifname "br-lan"' 'meta mark set meta mark & 0xff000000'
require_rule "$TEST_TMP/guard-prerouting.block" 'br-lan prerouting does not derive its mark from the admitted MAC+IPv4 map' \
	'iifname "br-lan"' 'meta nfproto ipv4' 'meta mark set ether saddr . ip saddr map @v2_policy_marks'
reject_regex "$TEST_TMP/guard-prerouting.block" 'meta mark set (0x)?0([^0-9a-fA-F]|$)' \
	'br-lan prerouting clears unrelated mark bits instead of preserving them'
require_rule "$TEST_TMP/guard-input.block" 'V1 transparent input lacks magic, DNAT or exact IPv4/listener tuple' \
	'iifname "br-lan"' 'meta nfproto ipv4' "$MAGIC" 'meta l4proto tcp' 'ct status dnat' \
	'ip saddr . tcp dport @v1_tcp_redirects' 'accept'
require_rule "$TEST_TMP/guard-input.block" 'V2 transparent input lacks magic, DNAT or exact MAC/IPv4/listener tuple' \
	'iifname "br-lan"' 'meta nfproto ipv4' "$MAGIC" 'meta l4proto tcp' 'ct status dnat' \
	'ether saddr . ip saddr . tcp dport @v2_tcp_redirects' 'accept'
v1_input_accept_line=$(rule_line "$TEST_TMP/guard-input.block" \
	'V1 transparent input lacks magic, DNAT or exact IPv4/listener tuple' \
	'iifname "br-lan"' 'meta nfproto ipv4' "$MAGIC" 'meta l4proto tcp' 'ct status dnat' \
	'ip saddr . tcp dport @v1_tcp_redirects' 'accept')
v2_input_accept_line=$(rule_line "$TEST_TMP/guard-input.block" \
	'V2 transparent input lacks magic, DNAT or exact MAC/IPv4/listener tuple' \
	'iifname "br-lan"' 'meta nfproto ipv4' "$MAGIC" 'meta l4proto tcp' 'ct status dnat' \
	'ether saddr . ip saddr . tcp dport @v2_tcp_redirects' 'accept')
[ "$blocked_input_line" -lt "$v1_input_accept_line" ] &&
	[ "$blocked_input_line" -lt "$v2_input_accept_line" ] ||
	fail 'the original-destination safety drop appears after a transparent proxy accept'

# Forward authorization is TCP-only and binds the mark to the actual routed
# path.  No mark-only or interface-only accept is a final guardian verdict.
v1_accept_line=$(rule_line "$TEST_TMP/guard-forward.block" 'V1 L2TP forward lacks magic or exact IPv4/output tuple' \
	'iifname "br-lan"' 'meta nfproto ipv4' "$MAGIC" 'meta l4proto tcp' \
	'ip saddr . oifname @v1_l2tp_paths' 'accept')
v2_accept_line=$(rule_line "$TEST_TMP/guard-forward.block" 'V2 L2TP forward lacks magic or exact MAC/IPv4/output tuple' \
	'iifname "br-lan"' 'meta nfproto ipv4' "$MAGIC" 'meta l4proto tcp' \
	'ether saddr . ip saddr . oifname @v2_l2tp_paths' 'accept')
v1_return_accept_line=$(rule_line "$TEST_TMP/guard-forward.block" 'V1 L2TP return lacks established state or exact IPv4/input tuple' \
	'oifname "br-lan"' 'meta nfproto ipv4' 'ct state established' 'ct direction reply' 'meta l4proto tcp' \
	'ip daddr . iifname @v1_l2tp_paths' 'accept')
v2_return_accept_line=$(rule_line "$TEST_TMP/guard-forward.block" 'V2 L2TP return lacks established state or exact derived IPv4/input tuple' \
	'oifname "br-lan"' 'meta nfproto ipv4' 'ct state established' 'ct direction reply' 'meta l4proto tcp' \
	'ip daddr . iifname @v2_l2tp_return_paths' 'accept')

while IFS= read -r rule; do
	case "$rule" in
		*"$MAGIC"*accept*)
			case "$rule" in
				*@v1_l2tp_paths*|*@v2_l2tp_paths*|*@v1_tcp_redirects*|*@v2_tcp_redirects*) : ;;
				*) fail "guardian contains a mark-only final accept: $rule" ;;
			esac
			;;
	esac
done <"$GUARD_NORM"

# Forward deny ordering: client IPv6, external UDP and special-use IPv4
# destinations are denied before exact TCP authorization, and unknown LAN
# output is denied by the last rule regardless of earlier fw4 accepts.
ipv6_drop_line=$(rule_line "$TEST_TMP/guard-forward.block" 'LAN IPv6 forwarding is not denied' \
	'iifname "br-lan"' 'meta nfproto ipv6' 'drop')
udp_drop_line=$(rule_line "$TEST_TMP/guard-forward.block" 'LAN IPv4 UDP forwarding is not denied' \
	'iifname "br-lan"' 'meta nfproto ipv4' 'meta l4proto udp' 'drop')
require_regex "$GUARD_FLAT" 'set blocked_v4_destinations \{[^}]*type ipv4_addr[[:space:]]*;' \
	'missing special-use IPv4 destination set'
for prefix in \
	0.0.0.0/8 \
	10.0.0.0/8 \
	100.64.0.0/10 \
	127.0.0.0/8 \
	169.254.0.0/16 \
	172.16.0.0/12 \
	192.0.0.0/24 \
	192.0.2.0/24 \
	192.88.99.0/24 \
	192.168.0.0/16 \
	198.18.0.0/15 \
	198.51.100.0/24 \
	203.0.113.0/24 \
	224.0.0.0/4 \
	240.0.0.0/4; do
	require_fixed "$GUARD_FLAT" "$prefix" "special-use destination set is missing $prefix"
done

# An earlier fw4 established/related accept is not final.  Reverse traffic to
# LAN must be TCP established on the exact authorized PPP/client tuple; stale
# WAN, UDP, IPv6, private-source and unknown return paths are dropped.
reverse_ipv6_drop_line=$(rule_line "$TEST_TMP/guard-forward.block" 'reverse IPv6 forwarding to LAN is not denied' \
	'oifname "br-lan"' 'meta nfproto ipv6' 'drop')
reverse_udp_drop_line=$(rule_line "$TEST_TMP/guard-forward.block" 'reverse IPv4 UDP forwarding to LAN is not denied' \
	'oifname "br-lan"' 'meta nfproto ipv4' 'meta l4proto udp' 'drop')
reverse_blocked_drop_line=$(rule_line "$TEST_TMP/guard-forward.block" 'special-use IPv4 sources can return to LAN' \
	'oifname "br-lan"' 'meta nfproto ipv4' 'ip saddr @blocked_v4_destinations' 'drop')
for deny_line in "$reverse_ipv6_drop_line" "$reverse_udp_drop_line" "$reverse_blocked_drop_line"; do
	[ "$deny_line" -lt "$v1_return_accept_line" ] && [ "$deny_line" -lt "$v2_return_accept_line" ] ||
		fail 'reverse IPv6/UDP/special-use source deny appears after an exact return accept'
done
blocked_drop_line=$(rule_line "$TEST_TMP/guard-forward.block" 'special-use IPv4 destinations are not denied' \
	'iifname "br-lan"' 'meta nfproto ipv4' 'ip daddr @blocked_v4_destinations' 'drop')
for deny_line in "$ipv6_drop_line" "$udp_drop_line" "$blocked_drop_line"; do
	[ "$deny_line" -lt "$v1_accept_line" ] && [ "$deny_line" -lt "$v2_accept_line" ] ||
		fail 'IPv6/UDP/special-use destination deny appears after an exact forward accept'
done
br_lan_input_drop_line=$(first_line "$TEST_TMP/guard-input.block" 'iifname "br-lan" drop')
[ -n "$br_lan_input_drop_line" ] || fail 'LAN input does not contain a terminal br-lan drop'
loopback_input_line=$(rule_line "$TEST_TMP/guard-input.block" \
	'missing loopback admission after client termination' 'iifname "lo"' 'accept')
established_input_line=$(rule_line "$TEST_TMP/guard-input.block" \
	'missing router-originated reply admission' 'ct state established,related' 'accept')
wan_dhcp_input_line=$(rule_line "$TEST_TMP/guard-input.block" \
	'missing physical-WAN DHCP reply admission' 'iifname "eth1"' 'udp sport 67' 'udp dport 68' 'accept')
[ "$br_lan_input_drop_line" -lt "$loopback_input_line" ] &&
	[ "$br_lan_input_drop_line" -lt "$established_input_line" ] &&
	[ "$br_lan_input_drop_line" -lt "$wan_dhcp_input_line" ] ||
	fail 'router-control input allowances precede the terminal br-lan client drop'
lan_egress_final_line=$(grep -n -Fx 'iifname "br-lan" drop' "$TEST_TMP/guard-forward.block" |
	sed -n '1s/:.*//p')
[ -n "$lan_egress_final_line" ] || fail 'LAN egress has no exact final drop before return-path rules'
[ "$lan_egress_final_line" -lt "$reverse_ipv6_drop_line" ] ||
	fail 'LAN egress final drop appears after reverse-path handling'
assert_final_rule "$TEST_TMP/guard-forward.block" 'oifname "br-lan" drop' \
	'LAN return forwarding does not end in a final br-lan output drop'
assert_final_rule "$TEST_TMP/guard-l2-forward.block" 'meta ibrname "br-lan" meta obrname "br-lan" drop' \
	'bridge guardian does not end in a br-lan-to-br-lan frame drop'

# fw4 chain-pre fragments may only pass candidates through fw4's priority-0
# reject.  Guardian priority +10 remains the sole exact final decision.
for gate in "$INPUT_NORM" "$FORWARD_NORM"; do
	reject_regex "$gate" '(^|[[:space:]])(table|chain)[[:space:]]|hook[[:space:]]' \
		'fw4 candidate include is a ruleset instead of a chain fragment'
	reject_regex "$gate" '(^|[[:space:]])drop([[:space:]]|$)' 'fw4 candidate include contains a final drop verdict'
	reject_regex "$gate" '(^|[^0-9])22([^0-9]|$)' 'fw4 candidate include exposes SSH port 22'
done
require_rule "$INPUT_NORM" 'fw4 input gate is not a single marked IPv4 TCP DNAT candidate accept' \
	'iifname "br-lan"' 'meta nfproto ipv4' "$MAGIC" 'meta l4proto tcp' 'ct status dnat' 'accept'
require_rule "$INPUT_NORM" 'fw4 input gate does not pass router DNS candidates to the terminal guardian' \
	'iifname "br-lan"' 'ip daddr 192.168.9.1' \
	'meta l4proto' 'tcp, udp' 'th dport 53' 'accept'
[ "$(grep -c 'accept' "$INPUT_NORM")" -eq 2 ] || fail 'fw4 input gate must contain exactly two candidate accepts'
reject_regex "$INPUT_NORM" 'redirect|@v[12]_(tcp_redirects|dns_clients)' \
	'fw4 input gate directly opens a listener instead of deferring exact judgment to guardian'
require_rule "$FORWARD_NORM" 'fw4 forward gate is not a single marked IPv4 TCP candidate accept' \
	'iifname "br-lan"' 'meta nfproto ipv4' "$MAGIC" 'meta l4proto tcp' 'accept'
[ "$(grep -c 'accept' "$FORWARD_NORM")" -eq 1 ] || fail 'fw4 forward gate must contain exactly one candidate accept'
reject_regex "$FORWARD_NORM" 'meta l4proto udp|@v[12]_l2tp_paths|oifname[[:space:]]+"ppp-\*"' \
	'fw4 forward gate grants a broad final path instead of a TCP candidate'

# OpenWrt 23.05 default_postinst invokes every packaged init script's `start`
# action on live install and upgrade.  Guardian start may only publish durable
# LAN work and register its procd child; it must not touch fw4 quarantine or
# reset firewall state.  The real S18 boot first creates a sentinel, then
# resets the independent guardian and validates every pre-S19 gate.  Every
# failure after sentinel publication must retain that exact root-only sentinel;
# rc.common must not swallow reset failures from boot/reload/stop.
require_fixed "$GUARD_INIT" 'START=18' 'guardian does not start before firewall'
require_fixed "$GUARD_INIT" 'LAN_ISOLATION="${PROXYPOOL_LAN_ISOLATION:-/usr/lib/proxypool/lan-isolation.sh}"' \
	'guardian does not bind the LAN isolation reconciler'
require_fixed "$GUARD_INIT" 'reconcile_lan_isolation boot' \
	'S18 boot does not establish persistent Wi-Fi and bridge-port isolation'
require_fixed "$GUARD_INIT" 'hold_lan_boot_inhibited' \
	'S18 can return into S20 after an unprovable wireless/LAN boot'
require_fixed "$GUARD_INIT" 'firewall dhcp network wireless' \
	'S18 does not recheck wireless pending deltas before releasing S20'
require_fixed "$GUARD_INIT" 'procd_add_reload_trigger firewall network wireless' \
	'LAN isolation is not retriggered after network or wireless reload'
BOOT_BIN="$TEST_TMP/boot-bin"
BOOT_CONFIG="$TEST_TMP/boot-config"
BOOT_NFTABLES="$TEST_TMP/boot-nftables.d"
BOOT_STATE="$TEST_TMP/fw4.state"
BOOT_LOCK="$TEST_TMP/fw4.lock"
BOOT_TRACE="$TEST_TMP/boot.trace"
FAKE_RESET="$BOOT_BIN/guard-reset"
FAKE_TRANSACTION="$BOOT_BIN/firewall-transaction"
FAKE_CHECK="$BOOT_BIN/fw4-check"
FAKE_MODE="$BOOT_BIN/mode-probe"
FAKE_PENDING="$BOOT_BIN/pending-probe"
FAKE_NFT="$BOOT_BIN/nft"
FAKE_FLOCK="$BOOT_BIN/flock"
FAKE_SYNC="$BOOT_BIN/sync"
FAKE_LS="$BOOT_BIN/ls"
FAKE_ID="$BOOT_BIN/id"
FAKE_HOLD="$BOOT_BIN/quarantine-hold"
FAKE_ISOLATION="$BOOT_BIN/lan-isolation"
mkdir -p "$BOOT_BIN" "$BOOT_CONFIG" "$BOOT_NFTABLES"
LAN_WORKER="$BOOT_BIN/lan-isolation-worker"
cp "$LAN_WORKER_SOURCE" "$LAN_WORKER"
chmod 755 "$LAN_WORKER"
for package in firewall dhcp network; do
	printf 'boot fixture %s\n' "$package" >"$BOOT_CONFIG/$package"
done

cat >"$FAKE_RESET" <<'FAKE_RESET_EOF'
#!/bin/sh
set -eu
[ "${1:-}" = reset-empty ] || exit 2
[ "$(cat "$PROXYPOOL_FW4_STATE" 2>/dev/null)" = proxypool-fw4-quarantine-v1 ] || exit 3
printf 'reset\n' >>"$PROXYPOOL_TEST_BOOT_TRACE"
exit "${PROXYPOOL_TEST_RESET_RC:-0}"
FAKE_RESET_EOF
cat >"$FAKE_TRANSACTION" <<'FAKE_TRANSACTION_EOF'
#!/bin/sh
set -eu
[ "${1:-}" = recover-only ] || exit 2
[ "$(cat "$PROXYPOOL_FW4_STATE" 2>/dev/null)" = proxypool-fw4-quarantine-v1 ] || exit 3
[ ! -e "$PROXYPOOL_TEST_STOCK_NFT_INCLUDE" ] && [ ! -L "$PROXYPOOL_TEST_STOCK_NFT_INCLUDE" ] || exit 4
printf 'recover\n' >>"$PROXYPOOL_TEST_BOOT_TRACE"
exit "${PROXYPOOL_TEST_RECOVERY_RC:-0}"
FAKE_TRANSACTION_EOF
cat >"$FAKE_CHECK" <<'FAKE_CHECK_EOF'
#!/bin/sh
set -eu
[ "$#" -eq 1 ] && [ "$1" = "$PROXYPOOL_CONFIG_DIR" ] || exit 2
[ "$(cat "$PROXYPOOL_FW4_STATE" 2>/dev/null)" = proxypool-fw4-quarantine-v1 ] || exit 3
printf 'check\n' >>"$PROXYPOOL_TEST_BOOT_TRACE"
exit "${PROXYPOOL_TEST_CHECK_RC:-0}"
FAKE_CHECK_EOF
cat >"$FAKE_MODE" <<'FAKE_MODE_EOF'
#!/bin/sh
set -eu
[ "$#" -eq 1 ] && [ "$1" = cold ] || exit 2
printf 'mode\n' >>"$PROXYPOOL_TEST_BOOT_TRACE"
exit "${PROXYPOOL_TEST_MODE_RC:-0}"
FAKE_MODE_EOF
cat >"$FAKE_PENDING" <<'FAKE_PENDING_EOF'
#!/bin/sh
set -eu
[ "$#" -eq 0 ] || exit 2
[ "$(cat "$PROXYPOOL_FW4_STATE" 2>/dev/null)" = proxypool-fw4-quarantine-v1 ] || exit 3
printf 'pending\n' >>"$PROXYPOOL_TEST_BOOT_TRACE"
exit "${PROXYPOOL_TEST_PENDING_RC:-0}"
FAKE_PENDING_EOF
cat >"$FAKE_NFT" <<'FAKE_NFT_EOF'
#!/bin/sh
set -eu
[ "$#" -eq 2 ] && [ "$1" = list ] && [ "$2" = tables ] || exit 2
printf 'nft-list-tables\n' >>"$PROXYPOOL_TEST_BOOT_TRACE"
[ "${PROXYPOOL_TEST_NFT_TABLE_RC:-0}" -eq 0 ] || exit "$PROXYPOOL_TEST_NFT_TABLE_RC"
[ "${PROXYPOOL_TEST_FW4_TABLE_PRESENT:-0}" -eq 0 ] || printf 'table inet fw4\n'
FAKE_NFT_EOF
cat >"$FAKE_FLOCK" <<'FAKE_FLOCK_EOF'
#!/bin/sh
[ "$#" -eq 2 ] && [ "$1" = -x ] || exit 2
printf 'flock-%s\n' "$2" >>"$PROXYPOOL_TEST_BOOT_TRACE"
exit "${PROXYPOOL_TEST_FLOCK_RC:-0}"
FAKE_FLOCK_EOF
cat >"$FAKE_SYNC" <<'FAKE_SYNC_EOF'
#!/bin/sh
printf 'sync\n' >>"$PROXYPOOL_TEST_BOOT_TRACE"
exit "${PROXYPOOL_TEST_SYNC_RC:-0}"
FAKE_SYNC_EOF
cat >"$FAKE_LS" <<'FAKE_LS_EOF'
#!/bin/sh
set -eu
[ "$#" -eq 2 ] && [ "$1" = -nd ] || exit 2
case "$2" in
	*/10-custom-filter-chains.nft)
		printf '%s %s 0 0 1139 Jan 1 00:00 %s\n' \
			"${PROXYPOOL_TEST_STOCK_PERMISSIONS:--rw-r--r--}" \
			"${PROXYPOOL_TEST_STOCK_LINKS:-1}" "$2"
		;;
	*)
		printf '%s %s 0 0 28 Jan 1 00:00 %s\n' \
			"${PROXYPOOL_TEST_STATE_PERMISSIONS:--rw-------}" \
			"${PROXYPOOL_TEST_STATE_LINKS:-1}" "$2"
		;;
esac
FAKE_LS_EOF
cat >"$FAKE_ID" <<'FAKE_ID_EOF'
#!/bin/sh
[ "$#" -eq 1 ] && [ "$1" = -u ] || exit 2
printf '0\n'
FAKE_ID_EOF
cat >"$FAKE_HOLD" <<'FAKE_HOLD_EOF'
#!/bin/sh
printf 'quarantine-hold\n' >>"$PROXYPOOL_TEST_BOOT_TRACE"
exit 0
FAKE_HOLD_EOF
cat >"$FAKE_ISOLATION" <<'FAKE_ISOLATION_EOF'
#!/bin/sh
set -eu
case "${1:-}" in
	boot)
		[ "$(cat "$PROXYPOOL_FW4_STATE" 2>/dev/null)" = proxypool-fw4-quarantine-v1 ] || exit 3
		;;
	configure|request) : ;;
	*) exit 2 ;;
esac
printf 'isolation:%s\n' "$1" >>"$PROXYPOOL_TEST_BOOT_TRACE"
case "$1" in
	request) exit "${PROXYPOOL_TEST_REQUEST_RC:-${PROXYPOOL_TEST_ISOLATION_RC:-0}}" ;;
	configure) exit "${PROXYPOOL_TEST_CONFIGURE_RC:-${PROXYPOOL_TEST_ISOLATION_RC:-0}}" ;;
	*) exit "${PROXYPOOL_TEST_ISOLATION_RC:-0}" ;;
esac
FAKE_ISOLATION_EOF
chmod 755 "$FAKE_RESET" "$FAKE_TRANSACTION" "$FAKE_CHECK" "$FAKE_MODE" \
	"$FAKE_PENDING" "$FAKE_NFT" "$FAKE_FLOCK" "$FAKE_SYNC" "$FAKE_LS" "$FAKE_ID" \
	"$FAKE_HOLD" "$FAKE_ISOLATION"

run_guard_boot() {
	env \
		PATH="$BOOT_BIN:$PATH" \
		PROXYPOOL_GUARD_RESYNC="$FAKE_RESET" \
		PROXYPOOL_LAN_ISOLATION="$FAKE_ISOLATION" \
		PROXYPOOL_LAN_ISOLATION_WORKER="$LAN_WORKER" \
		PROXYPOOL_TRANSACTION_HELPER="$FAKE_TRANSACTION" \
		PROXYPOOL_FW4_CHECK="$FAKE_CHECK" \
		PROXYPOOL_FW4_MODE_PROBE="$FAKE_MODE" \
		PROXYPOOL_PENDING_DELTA_PROBE="$FAKE_PENDING" \
		PROXYPOOL_CONFIG_DIR="$BOOT_CONFIG" \
		PROXYPOOL_FW4_STATE="$BOOT_STATE" \
		PROXYPOOL_FW4_LOCK="$BOOT_LOCK" \
		PROXYPOOL_NFTABLES_USER_DIR="$BOOT_NFTABLES" \
		PROXYPOOL_NFT="$FAKE_NFT" \
		PROXYPOOL_FLOCK="$FAKE_FLOCK" \
		PROXYPOOL_SYNC="$FAKE_SYNC" \
		PROXYPOOL_LS_PROG="$FAKE_LS" \
		PROXYPOOL_QUARANTINE_HOLD="${PROXYPOOL_TEST_QUARANTINE_HOLD:-}" \
		PROXYPOOL_TEST_BOOT_TRACE="$BOOT_TRACE" \
		PROXYPOOL_TEST_STOCK_NFT_INCLUDE="$BOOT_NFTABLES/10-custom-filter-chains.nft" \
		PROXYPOOL_TEST_RESET_RC="${PROXYPOOL_TEST_RESET_RC:-0}" \
		PROXYPOOL_TEST_RECOVERY_RC="${PROXYPOOL_TEST_RECOVERY_RC:-0}" \
		PROXYPOOL_TEST_CHECK_RC="${PROXYPOOL_TEST_CHECK_RC:-0}" \
		PROXYPOOL_TEST_MODE_RC="${PROXYPOOL_TEST_MODE_RC:-0}" \
		PROXYPOOL_TEST_PENDING_RC="${PROXYPOOL_TEST_PENDING_RC:-0}" \
		PROXYPOOL_TEST_NFT_TABLE_RC="${PROXYPOOL_TEST_NFT_TABLE_RC:-0}" \
		PROXYPOOL_TEST_FW4_TABLE_PRESENT="${PROXYPOOL_TEST_FW4_TABLE_PRESENT:-0}" \
		PROXYPOOL_TEST_FLOCK_RC="${PROXYPOOL_TEST_FLOCK_RC:-0}" \
		PROXYPOOL_TEST_SYNC_RC="${PROXYPOOL_TEST_SYNC_RC:-0}" \
		PROXYPOOL_TEST_ISOLATION_RC="${PROXYPOOL_TEST_ISOLATION_RC:-0}" \
		PROXYPOOL_TEST_REQUEST_RC="${PROXYPOOL_TEST_REQUEST_RC:-0}" \
		PROXYPOOL_TEST_CONFIGURE_RC="${PROXYPOOL_TEST_CONFIGURE_RC:-0}" \
		PROXYPOOL_TEST_STATE_PERMISSIONS="${PROXYPOOL_TEST_STATE_PERMISSIONS:--rw-------}" \
		PROXYPOOL_TEST_STATE_LINKS="${PROXYPOOL_TEST_STATE_LINKS:-1}" \
		PROXYPOOL_TEST_STOCK_PERMISSIONS="${PROXYPOOL_TEST_STOCK_PERMISSIONS:--rw-r--r--}" \
		PROXYPOOL_TEST_STOCK_LINKS="${PROXYPOOL_TEST_STOCK_LINKS:-1}" \
		sh -c '
			procd_open_instance() { printf "procd:open:%s\n" "$1" >>"$PROXYPOOL_TEST_BOOT_TRACE"; }
			procd_set_param() { printf "procd:param:%s" "$1" >>"$PROXYPOOL_TEST_BOOT_TRACE"; shift; for value in "$@"; do printf ":%s" "$value" >>"$PROXYPOOL_TEST_BOOT_TRACE"; done; printf "\n" >>"$PROXYPOOL_TEST_BOOT_TRACE"; }
			procd_close_instance() { printf "procd:close\n" >>"$PROXYPOOL_TEST_BOOT_TRACE"; }
			. "$1"
			start() { start_service; }
			boot
		' sh "$GUARD_INIT"
}

rm -f "$BOOT_STATE" "$BOOT_TRACE"
PATH="$BOOT_BIN:$PATH" PROXYPOOL_GUARD_RESYNC="$FAKE_RESET" \
	PROXYPOOL_LAN_ISOLATION="$FAKE_ISOLATION" PROXYPOOL_LAN_ISOLATION_WORKER="$LAN_WORKER" \
	PROXYPOOL_TEST_BOOT_TRACE="$BOOT_TRACE" PROXYPOOL_FW4_STATE="$BOOT_STATE" \
	sh -c '
		procd_open_instance() { printf "procd:open:%s\n" "$1" >>"$PROXYPOOL_TEST_BOOT_TRACE"; }
		procd_set_param() { printf "procd:param:%s" "$1" >>"$PROXYPOOL_TEST_BOOT_TRACE"; shift; for value in "$@"; do printf ":%s" "$value" >>"$PROXYPOOL_TEST_BOOT_TRACE"; done; printf "\n" >>"$PROXYPOOL_TEST_BOOT_TRACE"; }
		procd_close_instance() { printf "procd:close\n" >>"$PROXYPOOL_TEST_BOOT_TRACE"; }
		. "$1"; start_service
	' sh "$GUARD_INIT" || fail 'ordinary guardian start could not register the LAN convergence worker'
[ ! -e "$BOOT_STATE" ] || fail 'ordinary guardian start created an S19 quarantine sentinel'
grep -Fxq 'isolation:request' "$BOOT_TRACE" || fail 'ordinary guardian start did not publish pending LAN work'
grep -Fxq 'procd:open:lan-reconciler' "$BOOT_TRACE" || fail 'ordinary guardian start did not use one named procd instance'
grep -Fxq "procd:param:command:$LAN_WORKER" "$BOOT_TRACE" || fail 'guardian registered the wrong worker command'
grep -Fxq 'procd:param:respawn:3600:5:0' "$BOOT_TRACE" || fail 'LAN worker is not configured for durable procd respawn'
grep -Fxq "procd:param:file:$LAN_WORKER:$FAKE_ISOLATION" "$BOOT_TRACE" ||
	fail 'procd does not restart the worker when its safety code changes during upgrade'
if grep -Eq '^reset$|^recover$|^check$|^mode$|^nft-list-tables$' "$BOOT_TRACE"; then
	fail 'ordinary guardian start touched the firewall boot transaction'
fi

rm -f "$BOOT_TRACE"
if PATH="$BOOT_BIN:$PATH" PROXYPOOL_LAN_ISOLATION="$FAKE_ISOLATION" \
	PROXYPOOL_LAN_ISOLATION_WORKER="$LAN_WORKER" PROXYPOOL_TEST_REQUEST_RC=74 \
	PROXYPOOL_TEST_BOOT_TRACE="$BOOT_TRACE" sh -c '
		procd_open_instance() { printf "procd:open\n" >>"$PROXYPOOL_TEST_BOOT_TRACE"; }
		procd_set_param() { :; }
		procd_close_instance() { :; }
		. "$1"; start_service
	' sh "$GUARD_INIT"; then
	fail 'guardian start registered a worker after pending publication failed'
fi
[ "$(cat "$BOOT_TRACE")" = 'isolation:request' ] ||
	fail 'failed guardian start continued into procd registration'

rm -f "$BOOT_STATE" "$BOOT_TRACE"
run_guard_boot || fail 'S18 boot rejects a fully validated cold-start sequence'
[ ! -e "$BOOT_STATE" ] || fail 'S18 boot did not release its sentinel after every gate passed'
for step in reset isolation:boot isolation:request procd:open:lan-reconciler recover pending check nft-list-tables; do
	grep -Fxq "$step" "$BOOT_TRACE" || fail "successful S18 boot skipped $step"
done
reset_line=$(first_line "$BOOT_TRACE" reset)
isolation_line=$(first_line "$BOOT_TRACE" isolation:boot)
worker_request_line=$(first_line "$BOOT_TRACE" isolation:request)
worker_open_line=$(first_line "$BOOT_TRACE" procd:open:lan-reconciler)
recover_line=$(first_line "$BOOT_TRACE" recover)
pending_line=$(first_line "$BOOT_TRACE" pending)
check_line=$(first_line "$BOOT_TRACE" check)
nft_line=$(first_line "$BOOT_TRACE" nft-list-tables)
[ "$reset_line" -lt "$isolation_line" ] && [ "$isolation_line" -lt "$worker_request_line" ] &&
	[ "$worker_request_line" -lt "$worker_open_line" ] && [ "$worker_open_line" -lt "$recover_line" ] &&
	[ "$recover_line" -lt "$pending_line" ] &&
	[ "$pending_line" -lt "$check_line" ] && [ "$check_line" -lt "$nft_line" ] ||
	fail 'S18 validation gates ran out of fail-closed order'

# Pinned OpenWrt 23.05.3 firewall4 installs one comment-only template into the
# otherwise forbidden user nftables include directory.  S18 already owns both
# the independent empty guardian and the pre-S19 quarantine, so it must retire
# that exact stock template before enforcing the empty-directory boundary.
STOCK_NFT_INCLUDE="$BOOT_NFTABLES/10-custom-filter-chains.nft"
printf '%s' 'IyMgVGhlIGZpcmV3YWxsNCBpbnB1dCwgZm9yd2FyZCBhbmQgb3V0cHV0IGNoYWlucyBhcmUgcmVnaXN0ZXJlZCB3aXRoCiMjIHByaW9yaXR5IGBmaWx0ZXJgICgwKS4KCgojIyBVbmNvbW1lbnQgdGhlIGNoYWlucyBiZWxvdyBpZiB5b3Ugd2FudCB0byBzdGFnZSBydWxlcyAqYmVmb3JlKiB0aGUKIyMgZGVmYXVsdCBmaXJld2FsbCBpbnB1dCwgZm9yd2FyZCBhbmQgb3V0cHV0IGNoYWlucy4KCiMgY2hhaW4gdXNlcl9wcmVfaW5wdXQgewojICAgICB0eXBlIGZpbHRlciBob29rIGlucHV0IHByaW9yaXR5IC0xOyBwb2xpY3kgYWNjZXB0OwojICAgICB0Y3AgZHBvcnQgc3NoIGN0IHN0YXRlIG5ldyBsb2cgcHJlZml4ICJTU0ggY29ubmVjdGlvbiBhdHRlbXB0OiAiCiMgfQojCiMgY2hhaW4gdXNlcl9wcmVfZm9yd2FyZCB7CiMgICAgIHR5cGUgZmlsdGVyIGhvb2sgZm9yd2FyZCBwcmlvcml0eSAtMTsgcG9saWN5IGFjY2VwdDsKIyB9CiMKIyBjaGFpbiB1c2VyX3ByZV9vdXRwdXQgewojICAgICB0eXBlIGZpbHRlciBob29rIG91dHB1dCBwcmlvcml0eSAtMTsgcG9saWN5IGFjY2VwdDsKIyB9CgoKIyMgVW5jb21tZW50IHRoZSBjaGFpbnMgYmVsb3cgaWYgeW91IHdhbnQgdG8gc3RhZ2UgcnVsZXMgKmFmdGVyKiB0aGUKIyMgZGVmYXVsdCBmaXJld2FsbCBpbnB1dCwgZm9yd2FyZCBhbmQgb3V0cHV0IGNoYWlucy4KCiMgY2hhaW4gdXNlcl9wb3N0X2lucHV0IHsKIyAgICAgdHlwZSBmaWx0ZXIgaG9vayBpbnB1dCBwcmlvcml0eSAxOyBwb2xpY3kgYWNjZXB0OwojICAgICBjdCBzdGF0ZSBuZXcgbG9nIHByZWZpeCAiRmlyZXdhbGw0IGFjY2VwdGVkIGluZ3Jlc3M6ICIKIyB9CiMKIyBjaGFpbiB1c2VyX3Bvc3RfZm9yd2FyZCB7CiMgICAgIHR5cGUgZmlsdGVyIGhvb2sgZm9yd2FyZCBwcmlvcml0eSAxOyBwb2xpY3kgYWNjZXB0OwojICAgICBjdCBzdGF0ZSBuZXcgbG9nIHByZWZpeCAiRmlyZXdhbGw0IGFjY2VwdGVkIGZvcndhcmQ6ICIKIyB9CiMKIyBjaGFpbiB1c2VyX3Bvc3Rfb3V0cHV0IHsKIyAgICAgdHlwZSBmaWx0ZXIgaG9vayBvdXRwdXQgcHJpb3JpdHkgMTsgcG9saWN5IGFjY2VwdDsKIyAgICAgY3Qgc3RhdGUgbmV3IGxvZyBwcmVmaXggIkZpcmV3YWxsNCBhY2NlcHRlZCBlZ3Jlc3M6ICIKIyB9Cgo=' |
	base64 -d >"$STOCK_NFT_INCLUDE"
chmod 644 "$STOCK_NFT_INCLUDE"
[ "$(sha256sum "$STOCK_NFT_INCLUDE" | cut -d' ' -f1)" = af5cbfeb3e3b61d32ce134ae33d15330ee27da838e6f4fb9c717f034923b8b16 ] ||
	fail 'stock firewall4 fixture does not match the pinned source digest'
STOCK_NFT_FIXTURE="$TEST_TMP/10-custom-filter-chains.stock"
cp "$STOCK_NFT_INCLUDE" "$STOCK_NFT_FIXTURE"
rm -f "$BOOT_STATE" "$BOOT_TRACE"
run_guard_boot || fail 'S18 rejected the pinned comment-only firewall4 template'
[ ! -e "$STOCK_NFT_INCLUDE" ] && [ ! -L "$STOCK_NFT_INCLUDE" ] ||
	fail 'S18 did not retire the pinned comment-only firewall4 template'
[ ! -e "$BOOT_STATE" ] ||
	fail 'S18 retained its quarantine after retiring the stock firewall4 template'

printf '%s\n' 'chain modified_by_user { type filter hook forward priority 1; }' >"$STOCK_NFT_INCLUDE"
chmod 644 "$STOCK_NFT_INCLUDE"
rm -f "$BOOT_STATE" "$BOOT_TRACE"
if run_guard_boot; then
	fail 'S18 accepted a modified firewall4 user include as the stock template'
fi
[ -f "$STOCK_NFT_INCLUDE" ] && [ ! -L "$STOCK_NFT_INCLUDE" ] ||
	fail 'S18 deleted a modified firewall4 user include'
[ -f "$BOOT_STATE" ] && [ ! -L "$BOOT_STATE" ] ||
	fail 'S18 did not retain its quarantine for a modified firewall4 user include'
rm -f "$STOCK_NFT_INCLUDE"

cp "$STOCK_NFT_FIXTURE" "$STOCK_NFT_INCLUDE"
chmod 600 "$STOCK_NFT_INCLUDE"
rm -f "$BOOT_STATE" "$BOOT_TRACE"
if PROXYPOOL_TEST_STOCK_PERMISSIONS=-rw------- run_guard_boot; then
	fail 'S18 accepted unsafe metadata on the stock firewall4 template'
fi
[ -f "$STOCK_NFT_INCLUDE" ] && [ ! -L "$STOCK_NFT_INCLUDE" ] ||
	fail 'S18 deleted a stock firewall4 template with unsafe metadata'
[ -f "$BOOT_STATE" ] && [ ! -L "$BOOT_STATE" ] ||
	fail 'S18 did not retain its quarantine for unsafe stock-template metadata'
rm -f "$STOCK_NFT_INCLUDE"

cp "$STOCK_NFT_FIXTURE" "$STOCK_NFT_INCLUDE"
chmod 644 "$STOCK_NFT_INCLUDE"
rm -f "$BOOT_STATE" "$BOOT_TRACE"
if PROXYPOOL_TEST_STOCK_LINKS=2 run_guard_boot; then
	fail 'S18 accepted a hard-linked stock firewall4 template'
fi
[ -f "$STOCK_NFT_INCLUDE" ] && [ ! -L "$STOCK_NFT_INCLUDE" ] ||
	fail 'S18 deleted a hard-linked stock firewall4 template'
[ -f "$BOOT_STATE" ] && [ ! -L "$BOOT_STATE" ] ||
	fail 'S18 did not retain its quarantine for a hard-linked stock template'
rm -f "$STOCK_NFT_INCLUDE"

# Invoke the shell function directly so assignment prefixes remain visible to
# run_guard_boot and are forwarded into its explicit env block.
for failure_case in reset isolation recovery pending pending_unknown check bad_metadata fw4_table; do
	case "$failure_case" in
		reset) PROXYPOOL_TEST_RESET_RC=1 ;;
		isolation)
			PROXYPOOL_TEST_ISOLATION_RC=1
			PROXYPOOL_TEST_QUARANTINE_HOLD="$FAKE_HOLD"
			;;
		recovery) PROXYPOOL_TEST_RECOVERY_RC=1 ;;
		pending)
			PROXYPOOL_TEST_PENDING_RC=1
			PROXYPOOL_TEST_QUARANTINE_HOLD="$FAKE_HOLD"
			;;
		pending_unknown)
			PROXYPOOL_TEST_PENDING_RC=2
			PROXYPOOL_TEST_QUARANTINE_HOLD="$FAKE_HOLD"
			;;
		check) PROXYPOOL_TEST_CHECK_RC=1 ;;
		bad_metadata) PROXYPOOL_TEST_STATE_LINKS=2 ;;
		fw4_table) PROXYPOOL_TEST_FW4_TABLE_PRESENT=1 ;;
	esac
	rm -f "$BOOT_STATE" "$BOOT_TRACE"
	if run_guard_boot; then
		fail "S18 $failure_case failure was accepted"
	fi
	[ -f "$BOOT_STATE" ] && [ ! -L "$BOOT_STATE" ] ||
		fail "S18 $failure_case failure did not retain the S19 sentinel"
	[ "$(cat "$BOOT_STATE")" = proxypool-fw4-quarantine-v1 ] ||
		fail "S18 $failure_case failure retained the wrong sentinel bytes"
	if [ "$failure_case" = isolation ]; then
		if grep -Fxq quarantine-hold "$BOOT_TRACE"; then
			fail 'isolation cold-proof failure blocked rcS before wired management could start'
		fi
		if grep -Eq '^recover$|^pending$|^check$|^nft-list-tables$' "$BOOT_TRACE"; then
			fail 'isolation cold-proof failure continued into firewall recovery or release gates'
		fi
	elif [ "$failure_case" = pending ] || [ "$failure_case" = pending_unknown ]; then
		grep -Fxq quarantine-hold "$BOOT_TRACE" ||
			fail "$failure_case cold proof returned without entering the S20 boot hold"
	fi
	unset PROXYPOOL_TEST_RESET_RC PROXYPOOL_TEST_RECOVERY_RC PROXYPOOL_TEST_PENDING_RC \
		PROXYPOOL_TEST_CHECK_RC PROXYPOOL_TEST_STATE_LINKS PROXYPOOL_TEST_FW4_TABLE_PRESENT \
		PROXYPOOL_TEST_ISOLATION_RC PROXYPOOL_TEST_REQUEST_RC PROXYPOOL_TEST_CONFIGURE_RC \
		PROXYPOOL_TEST_QUARANTINE_HOLD
done

# Mismatched/unknown cold-mode proof still writes the emergency sentinel and
# resets the guardian, but may not proceed into journal recovery or release.
for mode_rc in 1 2; do
	rm -f "$BOOT_STATE" "$BOOT_TRACE"
	if PROXYPOOL_TEST_MODE_RC=$mode_rc run_guard_boot; then
		fail "S18 accepted cold-mode probe status $mode_rc"
	fi
	[ "$(cat "$BOOT_STATE" 2>/dev/null)" = proxypool-fw4-quarantine-v1 ] ||
		fail "S18 cold-mode status $mode_rc did not retain its emergency sentinel"
	grep -Fxq reset "$BOOT_TRACE" || fail "S18 cold-mode status $mode_rc skipped guardian reset"
	if grep -Fxq recover "$BOOT_TRACE"; then
		fail "S18 cold-mode status $mode_rc continued into journal recovery"
	fi
done

# Pinned fw4 only treats a regular state path as an S19 inhibitor.  Unsafe
# pre-existing objects must be displaced under the lock and replaced by the
# exact regular sentinel; their mere existence is not fail-closed.
for unsafe_state in broken_symlink symlink_regular symlink_directory directory fifo; do
	rm -f "$BOOT_STATE" "$BOOT_TRACE"
	PROXYPOOL_TEST_QUARANTINE_HOLD="$FAKE_HOLD"
	case "$unsafe_state" in
		broken_symlink)
			if ! ln -s "$BOOT_STATE.missing" "$BOOT_STATE" 2>/dev/null; then
				echo 'SKIP: host cannot create a broken-symlink fw4.state fixture'
				unset PROXYPOOL_TEST_QUARANTINE_HOLD
				continue
			fi
			;;
		symlink_regular)
			printf '%s\n' foreign >"$BOOT_STATE.target"
			if ! ln -s "$BOOT_STATE.target" "$BOOT_STATE" 2>/dev/null || [ ! -L "$BOOT_STATE" ]; then
				rm -f "$BOOT_STATE" "$BOOT_STATE.target"
				echo 'SKIP: host cannot create a regular-target symlink fw4.state fixture'
				unset PROXYPOOL_TEST_QUARANTINE_HOLD
				continue
			fi
			;;
		symlink_directory)
			case "$(uname -s 2>/dev/null || printf unknown)" in
				MINGW*|MSYS*|CYGWIN*|Windows_NT*)
					echo 'SKIP: Windows host cannot create a directory-target symlink fixture'
					unset PROXYPOOL_TEST_QUARANTINE_HOLD
					continue
					;;
			esac
			mkdir "$BOOT_STATE.target-dir"
			printf '%s\n' preserved >"$BOOT_STATE.target-dir/payload"
			if ! ln -s "$BOOT_STATE.target-dir" "$BOOT_STATE" 2>/dev/null || [ ! -L "$BOOT_STATE" ]; then
				rm -f "$BOOT_STATE"
				rm -f "$BOOT_STATE.target-dir/payload"
				rmdir "$BOOT_STATE.target-dir"
				echo 'SKIP: host cannot create a directory-target symlink fw4.state fixture'
				unset PROXYPOOL_TEST_QUARANTINE_HOLD
				continue
			fi
			;;
		directory)
			mkdir "$BOOT_STATE"
			printf '%s\n' preserved >"$BOOT_STATE/payload"
			;;
		fifo)
			case "$(uname -s 2>/dev/null || printf unknown)" in
				MINGW*|MSYS*|CYGWIN*|Windows_NT*)
					echo 'SKIP: Windows host cannot prove atomic FIFO replacement'
					unset PROXYPOOL_TEST_QUARANTINE_HOLD
					continue
					;;
			esac
			if ! mkfifo "$BOOT_STATE" 2>/dev/null; then
				echo 'SKIP: host cannot create FIFO fw4.state fixture'
				unset PROXYPOOL_TEST_QUARANTINE_HOLD
				continue
			fi
			;;
	esac
	if PROXYPOOL_TEST_MODE_RC=0 run_guard_boot; then
		fail "S18 accepted pre-existing $unsafe_state fw4.state"
	fi
	case "$unsafe_state" in
		directory)
			[ -d "$BOOT_STATE" ] && [ ! -L "$BOOT_STATE" ] ||
				fail 'S18 moved a non-atomically-replaceable fw4.state directory'
			grep -Fxq quarantine-hold "$BOOT_TRACE" ||
				fail 'S18 did not retain the fw4 lock for a directory state path'
			rm -f "$BOOT_STATE/payload"
			rmdir "$BOOT_STATE"
			;;
		symlink_directory)
			[ -L "$BOOT_STATE" ] && [ -d "$BOOT_STATE" ] ||
				fail 'S18 moved a non-atomically-replaceable directory symlink'
			[ "$(cat "$BOOT_STATE.target-dir/payload")" = preserved ] ||
				fail 'S18 modified the target of a directory symlink'
			grep -Fxq quarantine-hold "$BOOT_TRACE" ||
				fail 'S18 did not retain the fw4 lock for a directory symlink state path'
			rm -f "$BOOT_STATE"
			rm -f "$BOOT_STATE.target-dir/payload"
			rmdir "$BOOT_STATE.target-dir"
			;;
		*)
			[ -f "$BOOT_STATE" ] && [ ! -L "$BOOT_STATE" ] ||
				fail "S18 did not replace pre-existing $unsafe_state with a regular sentinel"
			[ "$(cat "$BOOT_STATE")" = proxypool-fw4-quarantine-v1 ] ||
				fail "S18 replaced pre-existing $unsafe_state with the wrong sentinel"
			grep -Fxq reset "$BOOT_TRACE" ||
				fail "S18 pre-existing $unsafe_state path skipped guardian reset"
			;;
	esac
	if grep -Fxq recover "$BOOT_TRACE"; then
		fail "S18 pre-existing $unsafe_state path continued into journal recovery"
	fi
	unset PROXYPOOL_TEST_QUARANTINE_HOLD
done

FAKE_SIMPLE_RESET="$BOOT_BIN/simple-reset"
cat >"$FAKE_SIMPLE_RESET" <<'FAKE_SIMPLE_RESET_EOF'
#!/bin/sh
[ "${1:-}" = reset-empty ] || exit 2
exit "${PROXYPOOL_TEST_RESET_RC:-0}"
FAKE_SIMPLE_RESET_EOF
chmod 755 "$FAKE_SIMPLE_RESET"
for action in reload_service stop_service; do
	PATH="$BOOT_BIN:$PATH" PROXYPOOL_TEST_RESET_RC=0 PROXYPOOL_GUARD_RESYNC="$FAKE_SIMPLE_RESET" \
		PROXYPOOL_LAN_ISOLATION="$FAKE_ISOLATION" PROXYPOOL_TEST_ISOLATION_RC=0 \
		PROXYPOOL_LAN_ISOLATION_WORKER="$LAN_WORKER" \
		PROXYPOOL_TEST_BOOT_TRACE="$BOOT_TRACE" \
		PROXYPOOL_CONFIG_DIR="$BOOT_CONFIG" PROXYPOOL_FW4_STATE="$BOOT_STATE" \
		PROXYPOOL_FW4_LOCK="$BOOT_LOCK" PROXYPOOL_NFTABLES_USER_DIR="$BOOT_NFTABLES" \
		sh -c '. "$1"; "$2"; exit 0' sh "$GUARD_INIT" "$action" ||
		fail "$action rejects a successful guardian reset"
	if PATH="$BOOT_BIN:$PATH" PROXYPOOL_TEST_RESET_RC=1 PROXYPOOL_GUARD_RESYNC="$FAKE_SIMPLE_RESET" \
		PROXYPOOL_LAN_ISOLATION="$FAKE_ISOLATION" PROXYPOOL_TEST_ISOLATION_RC=0 \
		PROXYPOOL_LAN_ISOLATION_WORKER="$LAN_WORKER" \
		PROXYPOOL_TEST_BOOT_TRACE="$BOOT_TRACE" \
		PROXYPOOL_CONFIG_DIR="$BOOT_CONFIG" PROXYPOOL_FW4_STATE="$BOOT_STATE" \
		PROXYPOOL_FW4_LOCK="$BOOT_LOCK" PROXYPOOL_NFTABLES_USER_DIR="$BOOT_NFTABLES" \
		sh -c '. "$1"; "$2" || :; exit 0' sh "$GUARD_INIT" "$action"; then
		fail "$action lets rc.common swallow a failed guardian reset"
	fi
done

rm -f "$BOOT_TRACE"
if PATH="$BOOT_BIN:$PATH" PROXYPOOL_TEST_RESET_RC=0 PROXYPOOL_GUARD_RESYNC="$FAKE_SIMPLE_RESET" \
	PROXYPOOL_LAN_ISOLATION="$FAKE_ISOLATION" PROXYPOOL_TEST_ISOLATION_RC=1 \
	PROXYPOOL_LAN_ISOLATION_WORKER="$LAN_WORKER" \
	PROXYPOOL_TEST_BOOT_TRACE="$BOOT_TRACE" PROXYPOOL_CONFIG_DIR="$BOOT_CONFIG" \
	PROXYPOOL_FW4_STATE="$BOOT_STATE" PROXYPOOL_FW4_LOCK="$BOOT_LOCK" \
	PROXYPOOL_NFTABLES_USER_DIR="$BOOT_NFTABLES" \
	sh -c '. "$1"; reload_service || :; exit 0' sh "$GUARD_INIT"; then
	fail 'reload_service lets rc.common swallow a failed wireless isolation configuration'
fi
grep -Fxq 'isolation:configure' "$BOOT_TRACE" ||
	fail 'reload_service did not preserve the live wireless isolation gate'

# Exercise the real staged checker orchestration with target-shaped fakes.  In
# particular, ordinary OpenWrt BusyBox images do not provide GNU `stat -c`, and
# an isolated check must patch fw4's firewall load to an absolute staged path
# instead of trusting libuci's default /tmp/.uci delta search path.
CHECK_BIN="$TEST_TMP/check-bin"
CHECK_STAGE="$TEST_TMP/check-stage"
CHECK_FW4="$TEST_TMP/fw4.uc"
CHECK_MAIN="$TEST_TMP/main.uc"
CHECK_TRACE="$TEST_TMP/check.trace"
CHECK_NFTABLES_DIR="$TEST_TMP/nftables.d"
mkdir -p "$CHECK_BIN" "$CHECK_STAGE" "$CHECK_NFTABLES_DIR"
chmod 700 "$CHECK_STAGE"
for package in firewall dhcp network; do
	printf 'fixture %s\n' "$package" >"$CHECK_STAGE/$package"
done
cat >"$CHECK_FW4" <<'CHECK_FW4_EOF'
this.cursor = uci.cursor();
this.cursor.load("firewall");
CHECK_FW4_EOF
printf 'fixture main\n' >"$CHECK_MAIN"
cat >"$CHECK_BIN/id" <<'CHECK_ID_EOF'
#!/bin/sh
[ "${1:-}" = -u ] || exit 2
printf '0\n'
CHECK_ID_EOF
cat >"$CHECK_BIN/stat" <<'CHECK_STAT_EOF'
#!/bin/sh
echo 'GNU/coreutils stat is intentionally unavailable' >&2
exit 127
CHECK_STAT_EOF
cat >"$CHECK_BIN/utpl" <<'CHECK_UTPL_EOF'
#!/bin/sh
set -eu
module_dir=
while [ "$#" -gt 0 ]; do
	case "$1" in
		-L) module_dir=$2; shift 2 ;;
		-S) [ -f "$2" ] || exit 3; shift 2 ;;
		*) exit 4 ;;
	esac
done
[ -n "$module_dir" ] && [ -f "$module_dir/fw4.uc" ] || exit 5
grep -Fq 'this.cursor = uci.cursor();' "$module_dir/fw4.uc" || exit 6
grep -Fq 'this.cursor.load(getenv("PROXYPOOL_STAGED_CONFIG") + "/firewall");' "$module_dir/fw4.uc" || exit 7
if grep -Fq 'this.cursor.load("firewall");' "$module_dir/fw4.uc"; then
	exit 8
fi

guardian_line="include \"$PROXYPOOL_GUARD_RULESET\""
input_line="include \"$PROXYPOOL_INPUT_GATE\""
forward_line="include \"$PROXYPOOL_FORWARD_GATE\""
printf '%s\n' 'table inet fw4' 'flush table inet fw4'
[ "${PROXYPOOL_TEST_RENDER_MODE:-valid}" = guardian_late ] || printf '%s\n' "$guardian_line"
printf '%s\n' 'table inet fw4 {' " include \"$PROXYPOOL_NFTABLES_USER_DIR/*.nft\"" \
	' chain input {' '  type filter hook input priority 0; policy drop;'
case "${PROXYPOOL_TEST_RENDER_MODE:-valid}" in
	wrong_chain) printf '%s\n' "$forward_line" ;;
	missing_input|input_late) : ;;
	*) printf '%s\n' "$input_line" ;;
esac
case "${PROXYPOOL_TEST_RENDER_MODE:-valid}" in
	input_late) printf '%s\n' '  ct state vmap { established : accept, related : accept, invalid : drop }' "$input_line" ;;
	missing_input_anchor) : ;;
	duplicate_input_anchor) printf '%s\n' \
		'  ct state vmap { established : accept, related : accept, invalid : drop }' \
		'  ct state vmap { established : accept, related : accept, invalid : drop }' ;;
	*) printf '%s\n' '  ct state vmap { established : accept, related : accept, invalid : drop }' ;;
esac
printf '%s\n' ' }' ' chain forward {' '  type filter hook forward priority 0; policy drop;'
case "${PROXYPOOL_TEST_RENDER_MODE:-valid}" in
	wrong_chain) printf '%s\n' "$input_line" ;;
	forward_late) : ;;
	*) printf '%s\n' "$forward_line" ;;
esac
[ "${PROXYPOOL_TEST_RENDER_MODE:-valid}" = flow_add ] && printf '%s\n' '  ip protocol tcp flow add @ft'
case "${PROXYPOOL_TEST_RENDER_MODE:-valid}" in
	missing_forward_anchor) : ;;
	duplicate_forward_anchor) printf '%s\n' \
		'  ct state vmap { established : accept, related : accept, invalid : drop }' \
		'  ct state vmap { established : accept, related : accept, invalid : drop }' ;;
	*) printf '%s\n' '  ct state vmap { established : accept, related : accept, invalid : drop }' ;;
esac
[ "${PROXYPOOL_TEST_RENDER_MODE:-valid}" = forward_late ] && printf '%s\n' "$forward_line"
printf '%s\n' ' }'
[ "${PROXYPOOL_TEST_RENDER_MODE:-valid}" = flowtable ] && \
	printf '%s\n' ' flowtable ft {' '  hook ingress priority 0;' '  devices = { eth0 };' ' }'
printf '%s\n' '}'
[ "${PROXYPOOL_TEST_RENDER_MODE:-valid}" = guardian_late ] && printf '%s\n' "$guardian_line"
exit 0
CHECK_UTPL_EOF
cat >"$CHECK_BIN/nft" <<'CHECK_NFT_EOF'
#!/bin/sh
set -eu
[ "$#" -eq 3 ] && [ "$1" = -c ] && [ "$2" = -f ] && [ -s "$3" ] || exit 9
printf 'nft-check\n' >>"$PROXYPOOL_TEST_CHECK_TRACE"
CHECK_NFT_EOF
chmod 755 "$CHECK_BIN/id" "$CHECK_BIN/stat" "$CHECK_BIN/utpl" "$CHECK_BIN/nft"

run_real_staged_check() {
	mode=$1
	PATH="$CHECK_BIN:$PATH" \
	PROXYPOOL_FW4_UCODE="$CHECK_FW4" \
	PROXYPOOL_FW4_MAIN="$CHECK_MAIN" \
	PROXYPOOL_UTPL="$CHECK_BIN/utpl" \
	PROXYPOOL_NFT="$CHECK_BIN/nft" \
	PROXYPOOL_GUARD_RULESET="$GUARD" \
	PROXYPOOL_INPUT_GATE="$INPUT_GATE" \
	PROXYPOOL_FORWARD_GATE="$FORWARD_GATE" \
	PROXYPOOL_NFTABLES_USER_DIR="$CHECK_NFTABLES_DIR" \
	PROXYPOOL_TEST_RENDER_MODE="$mode" \
	PROXYPOOL_TEST_CHECK_TRACE="$CHECK_TRACE" \
		sh "$STAGED_CHECKER" "$CHECK_STAGE"
}

: >"$CHECK_TRACE"
run_real_staged_check valid || fail 'real staged checker rejected an isolated valid render'
[ "$(grep -c '^nft-check$' "$CHECK_TRACE")" -eq 1 ] ||
	fail 'real staged checker did not invoke nft exactly once for a valid render'
for invalid_render in \
	wrong_chain guardian_late missing_input input_late forward_late \
	missing_input_anchor duplicate_input_anchor \
	missing_forward_anchor duplicate_forward_anchor \
	flowtable flow_add; do
	: >"$CHECK_TRACE"
	if run_real_staged_check "$invalid_render" >"$TEST_TMP/check-$invalid_render.log" 2>&1; then
		fail "real staged checker accepted invalid include placement: $invalid_render"
	fi
	[ ! -s "$CHECK_TRACE" ] || fail "real staged checker invoked nft after invalid placement: $invalid_render"
done

printf '%s\n' 'flowtable bypass { hook ingress priority 0; devices = { eth0 }; }' \
	>"$CHECK_NFTABLES_DIR/evil-flowtable.nft"
: >"$CHECK_TRACE"
if run_real_staged_check valid >"$TEST_TMP/check-user-nftables.log" 2>&1; then
	fail 'real staged checker accepted an unowned /etc/nftables.d include'
fi
[ ! -s "$CHECK_TRACE" ] || fail 'real staged checker invoked nft after an unowned user include'

# Only the guardian ruleset itself may perform its bounded delete/recreate.
# Normal V1/V2 cleanup must never remove the guardian or flush global fw4/nft.
for source_root in "$ROOT/proxypool-core/files" "$ROOT/luci-app-proxypool" "$ROOT/files"; do
	find "$source_root" -type f 2>/dev/null | while IFS= read -r source; do
		[ "$source" = "$GUARD" ] && continue
		case "$source" in
			*.sh|*.init|*.nft|*.lua|*/etc/uci-defaults/*|*/proxypool-firewall-defaults) : ;;
			*) continue ;;
		esac
		clean="$TEST_TMP/cleanup.$$.txt"
		sed 's/[[:space:]]*#.*$//' "$source" >"$clean"
		if grep -Eiq 'delete[[:space:]]+table[[:space:]]+(inet[[:space:]]+proxypool_guard|bridge[[:space:]]+proxypool_l2_guard)|nft[[:space:]][^;]*flush[[:space:]]+ruleset|fw4[[:space:]]+flush' "$clean"; then
			fail "cleanup-capable source can delete the permanent guardian or flush global firewall: $source"
		fi
	done
done

# Gate fragments are checked inside real synthetic chains.  CI may inject the
# OpenWrt 23.05.3 nft binary/container wrapper with PROXYPOOL_TARGET_NFT; that
# target check takes precedence over the supplemental host nft check.
SYNTAX_RULESET="$TEST_TMP/proxypool-guard.syntax.nft"
{
	cat "$GUARD"
	printf '%s\n' \
		'table inet proxypool_fw4_gate_syntax;' \
		'delete table inet proxypool_fw4_gate_syntax;' \
		'table inet proxypool_fw4_gate_syntax {' \
		' chain input {' \
		'  type filter hook input priority 0; policy accept;'
	cat "$INPUT_GATE"
	printf '%s\n' \
		' }' \
		' chain forward {' \
		'  type filter hook forward priority 0; policy accept;'
	cat "$FORWARD_GATE"
	printf '%s\n' ' }' '}'
} >"$SYNTAX_RULESET"

run_target_nft_check() {
	nft_command=$1
	if ! "$nft_command" -c -f "$SYNTAX_RULESET" >"$TEST_TMP/nft-check.log" 2>&1; then
		cat "$TEST_TMP/nft-check.log" >&2
		fail "nft syntax check failed via $nft_command"
	fi
}

# PROXYPOOL_TARGET_NFT is an executable wrapper contract, not necessarily the
# nft binary itself.  CI may point it at a sudo helper or a pinned OpenWrt
# 23.05.3 container; the wrapper must forward the supplied -c -f arguments and
# provide the capabilities required by its target nft.
if [ -n "${PROXYPOOL_TARGET_NFT:-}" ]; then
	[ -x "$PROXYPOOL_TARGET_NFT" ] || fail "PROXYPOOL_TARGET_NFT is not executable: $PROXYPOOL_TARGET_NFT"
	run_target_nft_check "$PROXYPOOL_TARGET_NFT"
else
	host_os=$(uname -s 2>/dev/null || printf unknown)
	case "$host_os" in
		Linux*)
			[ "${CI:-}" != true ] ||
				fail 'Linux CI requires an explicit executable PROXYPOOL_TARGET_NFT wrapper; plain host nft is only supplemental'
			if command -v nft >/dev/null 2>&1; then
				if "$(command -v nft)" -c -f "$SYNTAX_RULESET" >"$TEST_TMP/host-nft-check.log" 2>&1; then
					echo 'Host nft syntax: PASS (supplemental only)'
				else
					echo 'SKIP: supplemental host nft check lacks compatible syntax/capability; use PROXYPOOL_TARGET_NFT for the gate'
				fi
			else
				echo 'SKIP: supplemental host nft is unavailable; use PROXYPOOL_TARGET_NFT for the gate'
			fi
			;;
		MINGW*|MSYS*|CYGWIN*|Windows_NT*)
			echo 'SKIP: nft syntax check requires Linux or PROXYPOOL_TARGET_NFT'
			;;
		*) echo 'SKIP: nft syntax check requires Linux or PROXYPOOL_TARGET_NFT' ;;
	esac
fi

echo 'ProxyPool guardian contract: PASS'
