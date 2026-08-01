#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
GUARD="$ROOT/proxypool-core/files/proxypool-guard.nft"
TRANSACTION="$ROOT/proxypool-core/files/proxypool-firewall-transaction"

fail() {
	echo "guardian terminal policy: $*" >&2
	exit 1
}

[ -f "$GUARD" ] && [ ! -L "$GUARD" ] || fail 'guardian ruleset is missing or unsafe'
[ -f "$TRANSACTION" ] && [ ! -L "$TRANSACTION" ] || fail 'runtime verifier is missing or unsafe'

normalized=$(mktemp)
trap 'rm -f "$normalized"' EXIT HUP INT TERM
sed 's/^[[:space:]]*//; s/[[:space:]][[:space:]]*/ /g; s/[[:space:]]*$//' \
	"$GUARD" >"$normalized"

assert_once() {
	line=$1
	description=$2
	count=$(grep -Fxc "$line" "$normalized" 2>/dev/null || true)
	[ "$count" -eq 1 ] || fail "$description count is $count, expected 1"
}

# These are independent base chains which run after fw4.  An earlier fw4
# accept must never become a bypass for an unclassified bridge, VLAN, or AP.
assert_once 'type filter hook input priority 10; policy drop;' 'terminal input policy'

assert_chain_header() {
	chain=$1
	header=$2
	description=$3
	awk -v wanted_chain="$chain" -v wanted_header="$header" '
		$0 == "chain " wanted_chain " {" { inside=1; chains++; next }
		inside && $0 == wanted_header { matches++ }
		inside && $0 == "}" { inside=0 }
		END { exit !(chains == 1 && matches == 1) }
	' "$normalized" || fail "$description is missing or duplicated"
}

assert_chain_header guard_forward \
	'type filter hook forward priority 10; policy drop;' \
	'terminal routed-forward policy'
assert_chain_header guard_l2_forward \
	'type filter hook forward priority 10; policy drop;' \
	'terminal bridge-forward policy'

# The input policy still has to support the router control plane without
# reopening a client surface: loopback, replies to router-originated traffic,
# and the exact physical-WAN DHCP client exchange.
assert_once 'iifname "lo" accept' 'loopback admission'
assert_once 'ct state established,related accept' 'router-originated reply admission'
assert_once 'iifname "eth1" meta nfproto ipv4 udp sport 67 udp dport 68 accept' 'physical-WAN DHCP admission'

# Client classification remains ordered ahead of the generic reply rule.  In
# particular, a stale client conntrack entry must not make local DNS or another
# router service reachable after policy changes.
awk '
	/^chain guard_input \{$/ { inside=1; chains++; next }
	inside && /^iifname "br-lan" drop$/ { client_drop=NR }
	inside && /^ct state established,related accept$/ { established=NR }
	inside && /^}$/ { inside=0 }
	END {
		exit !(chains == 1 && client_drop && established && client_drop < established)
	}
' "$normalized" || fail 'generic established admission is not after the br-lan terminal drop'

# No positive forwarding rule may exist for the physical WAN.  Only exact,
# daemon-owned L2TP tuples can cross the routed chain.
if awk '
	/^chain guard_forward \{$/ { inside=1; next }
	inside && /^}$/ { inside=0 }
	inside && /eth1/ && /accept/ { found=1 }
	END { exit !found }
' "$normalized"; then
	fail 'guardian contains a direct physical-WAN forwarding admission'
fi

# Runtime acknowledgement is authority-bearing.  Its exact parser must reject
# an older permissive chain even when the installed source file is correct.
for verifier_contract in \
	'expected["guard_input",1]="type filter hook input priority filter + 10; policy drop;"' \
	'expected["guard_input",9]="iifname \"lo\" accept"' \
	'expected["guard_input",10]="ct state established,related accept"' \
	'expected["guard_input",11]="iifname \"eth1\" meta nfproto ipv4 udp sport 67 udp dport 68 accept"' \
	'maximum["guard_input"]=11' \
	'expected["guard_forward",1]="type filter hook forward priority filter + 10; policy drop;"' \
	'expected="type filter hook forward priority 10; policy drop;"'; do
	grep -Fq "$verifier_contract" "$TRANSACTION" ||
		fail "runtime verifier is missing: $verifier_contract"
done

echo 'guardian terminal policy: PASS'
