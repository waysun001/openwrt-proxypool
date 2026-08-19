#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
MAIN="$ROOT/proxypool-core/src/proxypoold/cmd/proxypoold/main.go"
CONTROLLER="$ROOT/proxypool-core/src/proxypoold/internal/engine/controller.go"
SCHEDULER="$ROOT/proxypool-core/src/proxypoold/internal/engine/scheduler.go"
GATES="$ROOT/proxypool-core/src/proxypoold/internal/live/gates.go"
INIT="$ROOT/proxypool-core/files/proxypool.init"
LUCI="$ROOT/luci-app-proxypool/luasrc/controller/proxypool.lua"
LUCI_RPC="$ROOT/luci-app-proxypool/luasrc/model/proxypool_rpc.lua"
VIEW="$ROOT/luci-app-proxypool/luasrc/view/proxypool/main.htm"
V2_JS="$ROOT/luci-app-proxypool/htdocs/luci-static/resources/proxypool-v2.js"
THEME_HEADER="$ROOT/luci-theme-proxypool/ucode/template/themes/proxypool/header.ut"

fail() { echo "FAIL: $*" >&2; exit 1; }
require() { grep -Fq "$2" "$1" || fail "$3"; }

require "$MAIN" 'dnsproxy.NewServer(netip.MustParseAddrPort("192.168.9.1:53"))' 'live daemon does not own router DNS'
require "$MAIN" 'openwrtplatform.NewL2TPAdapter' 'live daemon does not assemble the L2TP adapter'
require "$MAIN" 'live.NewRouteGate' 'live daemon does not install the route gate'
require "$MAIN" 'live.NewDNSGate' 'live daemon does not install the node DNS gate'
require "$MAIN" 'live.NewAuthorizationGate' 'live daemon does not install the authorization gate'
require "$MAIN" 'openwrtplatform.NewWANStatusSource' 'live daemon does not supervise the authoritative WAN state'
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
for contract in 'device.bind' 'device.unbind' 'device.bindings.replace' 'node.action' 'node.save' 'node.delete' 'import.preview' 'import.commit' 'job.get' 'job.list' 'diagnostics.create' 'diagnostics.get' 'diagnostics.claim' 'diagnostics.release' 'post("api_write")'; do
	require "$LUCI" "$contract" "LuCI is missing control contract: $contract"
done
require "$V2_JS" "apiCall('diagnostics_create', {}, true)" 'LuCI does not create diagnostics asynchronously'
require "$LUCI" 'post("diagnostics_download")' 'diagnostic download is not POST protected'
require "$LUCI_RPC" 'nixio.socket("unix", "stream")' 'LuCI RPC bridge does not use a Unix socket'
require "$LUCI_RPC" 'socket:setopt("socket", "sndtimeo", TIMEOUT_SECONDS, 0)' 'LuCI control writes can hang without a deadline'
require "$LUCI_RPC" 'socket:setopt("socket", "rcvtimeo", TIMEOUT_SECONDS, 0)' 'LuCI control reads can hang without a deadline'
require "$V2_JS" 'environment.setTimeout(poll, 3000)' 'LuCI does not use one bounded status poller'
require "$V2_JS" "apiCall('import_preview', params, true)" 'LuCI does not use server-authoritative import preview'
require "$V2_JS" "apiCall('import_commit', params, true)" 'LuCI does not use one transactional import commit'
require "$V2_JS" "apiCall('bindings_replace'" 'LuCI does not use one atomic node membership write'
require "$VIEW" '无需手工输入' 'LuCI does not expose automatic device discovery'
require "$VIEW" 'pp-v2-import-raw' 'LuCI batch import modal is missing'
require "$VIEW" 'pp-v2-binding-modal' 'LuCI node binding modal is missing'
require "$THEME_HEADER" 'proxypool-global-menu' 'LuCI global theme navigation is missing'
if grep -Fq 'proxypool-global-menu' "$VIEW"; then fail 'LuCI page duplicates theme-owned navigation'; fi

if grep -Fq 'function pollJob' "$V2_JS"; then fail 'LuCI still blocks mutation UI while polling a whole job'; fi
if grep -Eq 'sequentialConnect|pending marker' "$V2_JS"; then fail 'LuCI still owns per-node import orchestration'; fi

echo 'PASS: V2 live L2TP assembly and LuCI control contract'
