#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
GATE="$ROOT/proxypool-core/files/legacy-gate.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT HUP INT TERM
FAKE_BIN="$TMP/bin"
STAGE="$TMP/stage"
STATE="$TMP/state"
MUTATIONS="$TMP/mutations"
mkdir -p "$FAKE_BIN" "$STAGE" "$STATE/run" "$STATE/log" "$STATE/config"
: >"$MUTATIONS"
export MUTATIONS

fail() { echo "FAIL: $*" >&2; exit 1; }

# The admission helper itself must be deterministic and use no external tool.
for args in 'mutation test:scope' 'test:scope'; do
	: >"$MUTATIONS"
	set +e
	out=$(PATH="$FAKE_BIN" /bin/sh "$GATE" $args 2>"$TMP/gate.err")
	rc=$?
	set -e
	[ "$rc" -eq 125 ] || fail "gate returned $rc instead of 125"
	[ "$out" = legacy_runtime_quarantined ] || fail "gate output is not deterministic"
	[ ! -s "$TMP/gate.err" ] || fail "gate wrote stderr"
done
echo 'PASS: legacy gate is deterministic and fail-closed'

cat >"$FAKE_BIN/mutator" <<'EOF'
#!/bin/sh
printf '%s %s\n' "${0##*/}" "$*" >>"$MUTATIONS"
exit 99
EOF
chmod +x "$FAKE_BIN/mutator"
for name in date mkdir touch rm mv cp uci ip kill pkill xl2tpd pppd redsocks \
	slp-client curl nc setsid ubus logger sleep ps mktemp chmod chown service ifup ifdown; do
	ln -s mutator "$FAKE_BIN/$name"
done
cat >"$FAKE_BIN/nft" <<'EOF'
#!/bin/sh
case " $* " in *' list '*) exit 1;; esac
exec "$(dirname "$0")/mutator" "$@"
EOF
chmod +x "$FAKE_BIN/nft"

stage_script() {
	source=$1
	target="$STAGE/${source##*/}"
	sed \
		-e "s#/usr/lib/proxypool#$STAGE#g" \
		-e "s#/var/run/proxypool#$STATE/run#g" \
		-e "s#/var/log/proxypool.log#$STATE/log/proxypool.log#g" \
		-e "s#/etc/config/proxypool#$STATE/config/proxypool#g" \
		"$source" >"$target"
	chmod +x "$target"
}

for file in proxypool.sh l2tp-manager.sh socks5-manager.sh slp-manager.sh firewall.sh watchdog.sh; do
	stage_script "$ROOT/proxypool-core/files/$file"
done
stage_script "$GATE"
printf '%s\n' '#!/bin/sh' 'printf "%s\n" readonly_status' >"$STAGE/status.sh"
chmod +x "$STAGE/status.sh"
printf '%s\n' sentinel >"$STATE/run/sentinel"
printf '%s\n' sentinel >"$STATE/log/proxypool.log"
printf '%s\n' sentinel >"$STATE/config/proxypool"

snapshot() {
	cksum "$STATE/run/sentinel" "$STATE/log/proxypool.log" "$STATE/config/proxypool"
}

run_denied() {
	label=$1 script=$2
	shift 2
	: >"$MUTATIONS"
	before=$(snapshot)
	set +e
	PATH="$FAKE_BIN:$PATH" PROXYPOOL_LEGACY_GATE="$STAGE/legacy-gate.sh" \
		sh "$script" "$@" >"$TMP/$label.out" 2>"$TMP/$label.err"
	rc=$?
	set -e
	[ "$rc" -eq 125 ] || fail "$label returned $rc"
	[ "$(cat "$TMP/$label.out")" = legacy_runtime_quarantined ] || fail "$label output changed"
	[ ! -s "$TMP/$label.err" ] || fail "$label wrote stderr"
	[ ! -s "$MUTATIONS" ] || fail "$label reached side effects: $(cat "$MUTATIONS")"
	[ "$(snapshot)" = "$before" ] || fail "$label changed staged state"
}

run_readonly() {
	label=$1 script=$2
	shift 2
	: >"$MUTATIONS"
	before=$(snapshot)
	PATH="$FAKE_BIN:$PATH" PROXYPOOL_LEGACY_GATE="$STAGE/legacy-gate.sh" \
		sh "$script" "$@" >"$TMP/$label.out" 2>"$TMP/$label.err" ||
		fail "$label read-only action failed"
	[ ! -s "$MUTATIONS" ] || fail "$label reached side effects: $(cat "$MUTATIONS")"
	[ "$(snapshot)" = "$before" ] || fail "$label changed staged state"
}

for action in start stop restart reload start_client stop_client restart_client \
	save_restart_client toggle_client probe_all batch_connect batch_disconnect \
	batch_enable batch_disable batch_delete sequential_start; do
	run_denied "proxypool_$action" "$STAGE/proxypool.sh" "$action" client7
done
run_readonly proxypool_status "$STAGE/proxypool.sh" status

for action in start stop status test probe; do
	run_denied "l2tp_$action" "$STAGE/l2tp-manager.sh" "$action" client7
	run_denied "socks5_$action" "$STAGE/socks5-manager.sh" "$action" client7
	run_denied "slp_$action" "$STAGE/slp-manager.sh" "$action" client7
done
run_readonly socks5_port "$STAGE/socks5-manager.sh" port client7 tcp
run_readonly slp_port "$STAGE/slp-manager.sh" port client7

for action in init cleanup rebuild remove_client add_client update_client; do
	run_denied "firewall_$action" "$STAGE/firewall.sh" "$action" client7
done
run_readonly firewall_show "$STAGE/firewall.sh" show
run_denied watchdog_run "$STAGE/watchdog.sh" run

# The retained LuCI controller has one fail-closed gate before its dispatch.
CONTROLLER="$ROOT/luci-app-proxypool/luasrc/controller/proxypool.lua"
MUTATIVE='save_client delete_client toggle_client start_client stop_client save_remark restart_client set_dhcp_lease reload probe_all clear_log batch_import batch_action'
READONLY='status get_client get_dhcp_lease log syslog export_all backup_create'
for action in $MUTATIVE; do
	grep -Fq "[\"$action\"] = true" "$CONTROLLER" || fail "LuCI action $action is not quarantined"
done
for action in $READONLY; do
	if grep -Fq "[\"$action\"] = true" "$CONTROLLER"; then
		fail "LuCI read action $action is incorrectly quarantined"
	fi
done
gate_line=$(grep -n 'if LEGACY_MUTATION_ACTIONS\[action\] then' "$CONTROLLER" | head -n1 | cut -d: -f1 || true)
dispatch_line=$(grep -n 'if action == "status" then' "$CONTROLLER" | head -n1 | cut -d: -f1 || true)
[ -n "$gate_line" ] && [ -n "$dispatch_line" ] && [ "$gate_line" -lt "$dispatch_line" ] ||
	fail 'LuCI mutation gate does not precede dispatch'

echo 'PASS: every retained legacy mutation entry is quarantined before side effects'
