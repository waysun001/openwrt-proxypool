#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
L2TP_GO="$ROOT/proxypool-core/src/proxypoold/internal/platform/openwrt/l2tp.go"
HELPER="$ROOT/proxypool-core/files/proxypool-ubus-call-stdin.uc"
EVENT="$ROOT/proxypool-core/files/proxypool-netifd-event"
MAKEFILE="$ROOT/proxypool-core/Makefile"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

[ -f "$L2TP_GO" ] || fail "L2TP adapter is missing"
[ -f "$HELPER" ] || fail "stdin ubus bridge is missing"
[ -f "$EVENT" ] || fail "netifd event helper is missing"

grep -Fq 'RunInput(ctx, input, "/usr/bin/ucode", l2tpUbusHelperPath, "network", "add_dynamic")' "$L2TP_GO" ||
	fail "secret-bearing add_dynamic payload is not sent over stdin"
grep -Fq '"call", "network.interface."+logical, "remove"' "$L2TP_GO" ||
	fail "dynamic interface is not removed through its official object method"
grep -Fq 'fs.readfile("/dev/stdin", 65537)' "$HELPER" ||
	fail "ubus bridge does not read the payload from stdin"
grep -Fq 'ARGV[0] != "network" || ARGV[1] != "add_dynamic"' "$HELPER" ||
	fail "ubus bridge is not restricted to add_dynamic"

if grep -E 'xl2tpd[^\n]*(restart|disable)|chap-secrets|ppp-(up|down)' "$L2TP_GO" >/dev/null; then
	fail "adapter directly mutates shared xl2tpd or global PPP state"
fi
if grep -F 'network del_dynamic' "$L2TP_GO" >/dev/null; then
	fail "adapter uses nonexistent network del_dynamic API"
fi

grep -Fq '+ucode-mod-ubus' "$MAKEFILE" || fail "ucode ubus module dependency is missing"
grep -Fq '98-proxypool-v2-event' "$MAKEFILE" || fail "event helper is not packaged"
grep -Fq 'ubus-call-stdin.uc' "$MAKEFILE" || fail "stdin ubus bridge is not packaged"

CAPTURE="$TMP/capture"
cat >"$TMP/ctl" <<'EOF'
#!/bin/sh
cat >"$PROXYPOOL_TEST_CAPTURE"
EOF
cat >"$TMP/timeout" <<'EOF'
#!/bin/sh
shift 3
exec "$@"
EOF
chmod +x "$TMP/ctl" "$TMP/timeout"

PROXYPOOL_TEST_CAPTURE="$CAPTURE" PROXYPOOL_CTL="$TMP/ctl" PROXYPOOL_TIMEOUT="$TMP/timeout" \
	INTERFACE='ppv20042' ACTION='ifup' sh "$EVENT"
EXPECTED='{"version":1,"id":"netifd","method":"system.interface_event","params":{"interface":"ppv20042","action":"ifup"}}'
[ "$(tr -d '\r\n' <"$CAPTURE")" = "$EXPECTED" ] || fail "valid hotplug hint is malformed"

rm -f "$CAPTURE"
PROXYPOOL_TEST_CAPTURE="$CAPTURE" PROXYPOOL_CTL="$TMP/ctl" PROXYPOOL_TIMEOUT="$TMP/timeout" \
	INTERFACE='ppv20042\"},\"action\":\"ifup' ACTION='ifup' sh "$EVENT"
[ ! -e "$CAPTURE" ] || fail "spoofed hotplug interface reached the daemon"

echo "PASS: V2 shared-netifd L2TP adapter contract"
