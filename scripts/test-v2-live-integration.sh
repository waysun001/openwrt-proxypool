#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
MAIN="$ROOT/proxypool-core/src/proxypoold/cmd/proxypoold/main.go"
CONTROLLER="$ROOT/proxypool-core/src/proxypoold/internal/engine/controller.go"
SCHEDULER="$ROOT/proxypool-core/src/proxypoold/internal/engine/scheduler.go"
GATES="$ROOT/proxypool-core/src/proxypoold/internal/live/gates.go"
INIT="$ROOT/proxypool-core/files/proxypool.init"
LUCI="$ROOT/luci-app-proxypool/luasrc/controller/proxypool.lua"
VIEW="$ROOT/luci-app-proxypool/luasrc/view/proxypool/main.htm"

fail() { echo "FAIL: $*" >&2; exit 1; }
require() { grep -Fq "$2" "$1" || fail "$3"; }

require "$MAIN" 'dnsproxy.NewServer(netip.MustParseAddrPort("192.168.9.1:53"))' 'live daemon does not own router DNS'
require "$MAIN" 'openwrtplatform.NewL2TPAdapter' 'live daemon does not assemble the L2TP adapter'
require "$MAIN" 'live.NewRouteGate' 'live daemon does not install the route gate'
require "$MAIN" 'live.NewDNSGate' 'live daemon does not install the node DNS gate'
require "$MAIN" 'live.NewAuthorizationGate' 'live daemon does not install the authorization gate'
require "$MAIN" 'controller.ReconcileStartup' 'live daemon does not reconcile restart state'
require "$SCHEDULER" 'func (scheduler *Scheduler) refreshNode' 'online nodes cannot refresh expiring leases'
require "$SCHEDULER" 'func (scheduler *Scheduler) Shutdown' 'graceful shutdown cannot remove owned sessions'
require "$CONTROLLER" 'case "system.interface_event":' 'netifd events cannot trigger recovery'
require "$GATES" 'defaultAuthorizationRenewInterval = 8 * time.Second' 'authorization leases are not renewed'

live_line=$(grep -F 'procd_set_param command /usr/sbin/proxypoold' "$INIT" || true)
case "$live_line" in
	*'--config "$V2_CONFIG_FILE"'*'--state "$V2_RUNTIME_STATE"'*'--live'*) ;;
	*) fail 'OpenWrt init still starts a non-live daemon' ;;
esac
case "$live_line" in *--shadow*) fail 'OpenWrt init still launches shadow mode' ;; esac

for forbidden in proxypool.sh l2tp-manager.sh socks5-manager.sh slp-manager.sh 'luci.model.uci' 'os.execute'; do
	if grep -Fq "$forbidden" "$LUCI"; then fail "LuCI directly invokes forbidden path: $forbidden"; fi
done
for contract in 'nixio.socket("unix", "stream")' 'device.bind' 'device.unbind' 'node.action' 'job.get' 'job.list'; do
	require "$LUCI" "$contract" "LuCI is missing control contract: $contract"
done
require "$LUCI" 'socket:setopt("socket", "sndtimeo", 12, 0)' 'LuCI control writes can hang without a deadline'
require "$LUCI" 'socket:setopt("socket", "rcvtimeo", 12, 0)' 'LuCI control reads can hang without a deadline'
require "$VIEW" '页面只使用一个轮询器' 'LuCI does not expose bounded job polling'
require "$VIEW" 'MAC 自动识别，无需手工输入' 'LuCI does not expose automatic device discovery'

echo 'PASS: V2 live L2TP assembly and LuCI control contract'
