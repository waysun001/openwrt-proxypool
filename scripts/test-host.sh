#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$ROOT/proxypool-core/src/proxypoold"

case "$(uname -s)" in
	Linux*)
		go test -race -count=1 ./...
		;;
	*)
		go test -count=1 ./...
		echo "SKIP: Go race detector requires a supported Linux host"
		;;
esac
go vet ./...

cd "$ROOT"
node --test luci-app-proxypool/tests/test_controller.mjs
node --test luci-app-proxypool/tests/ui/proxypool-v2.test.mjs
if command -v lua5.1 >/dev/null 2>&1; then
	lua5.1 luci-app-proxypool/tests/test_rpc.lua
elif command -v lua >/dev/null 2>&1; then
	lua luci-app-proxypool/tests/test_rpc.lua
elif command -v npx >/dev/null 2>&1; then
	npx --yes --package fengari-node-cli@0.1.0 fengari luci-app-proxypool/tests/test_rpc.lua
else
	echo "FAIL: Lua 5.1-compatible test runner is unavailable" >&2
	exit 1
fi

for shell_file in \
	luci-app-proxypool/root/etc/uci-defaults/luci-proxypool \
	proxypool-core/files/dns-manager.sh \
	proxypool-core/files/guard-resync.sh \
	proxypool-core/files/lan-isolation.sh \
	proxypool-core/files/lan-isolation-worker.sh \
	proxypool-core/files/legacy-gate.sh \
	proxypool-core/files/proxypool-lan-isolation.hotplug \
	proxypool-core/files/proxypool-netifd-event \
	proxypool-core/files/proxypool-firewall-defaults \
	proxypool-core/files/proxypool-firewall-transaction \
	proxypool-core/files/proxypool-fw4-activate \
	proxypool-core/files/proxypool-fw4-check-staged \
	proxypool-core/files/proxypool-guard.init \
	proxypool-core/files/proxypool-activate.init \
	proxypool-core/files/proxypool.init \
	proxypool-core/files/proxypool-postinst \
	proxypool-core/files/proxypool-backend-activate \
	proxypool-core/files/proxypool-migrate.sh \
	proxypool-core/files/proxypool-safety-uci-default \
	proxypool-core/files/status.sh \
	scripts/check-whitespace-range.sh \
	scripts/inspect-ipk.sh \
	scripts/inspect-luci-ipk.sh \
	scripts/prepare-image-files.sh \
	scripts/test-prepare-sdk-base-packages.sh \
	scripts/regenerate-sha256sums.sh \
	scripts/test-artifact-sha256sums.sh \
	scripts/test-backup-contract.sh \
	scripts/test-backend-activation-integration.sh \
	scripts/test-dns-fail-closed.sh \
	scripts/test-firewall-defaults.sh \
	scripts/test-fw4-activate.sh \
	scripts/test-guardian-terminal-policy.sh \
	scripts/test-image-files.sh \
	scripts/test-inspect-ipk.sh \
	scripts/test-inspect-luci-ipk.sh \
	scripts/test-kernel-isolation-contract.sh \
	scripts/test-legacy-quarantine.sh \
	scripts/test-lan-isolation-defaults.sh \
	scripts/test-lan-isolation-worker.sh \
	scripts/test-luci-package-source-safety.sh \
	scripts/test-package-safety-integration.sh \
	scripts/test-proxypool-dns-admission.sh \
	scripts/test-proxypool-guard.sh \
	scripts/test-proxypool-init.sh \
	scripts/test-release-contracts.sh \
	scripts/test-round3-hardware-contract.sh \
	scripts/test-status-readonly.sh \
	scripts/test-v2-l2tp-adapter.sh \
	scripts/test-v2-live-integration.sh \
	scripts/test-v2-migration.sh \
	scripts/test-v2-phase5-gates.sh \
	scripts/test-whitespace-range.sh; do
	sh -n "$shell_file"
done
sh scripts/test-artifact-sha256sums.sh
sh scripts/test-backup-contract.sh
sh scripts/test-backend-activation-integration.sh
sh scripts/test-dns-fail-closed.sh
sh scripts/test-firewall-defaults.sh
sh scripts/test-fw4-activate.sh
sh scripts/test-guardian-terminal-policy.sh
sh scripts/test-image-files.sh
sh scripts/test-inspect-ipk.sh
sh scripts/test-inspect-luci-ipk.sh
sh scripts/test-kernel-isolation-contract.sh
sh scripts/test-legacy-quarantine.sh
sh scripts/test-lan-isolation-defaults.sh
sh scripts/test-lan-isolation-worker.sh
sh scripts/test-luci-package-source-safety.sh
sh scripts/test-package-safety-integration.sh
sh scripts/test-prepare-sdk-base-packages.sh
sh scripts/test-proxypool-dns-admission.sh
sh scripts/test-proxypool-guard.sh
sh scripts/test-proxypool-init.sh
sh scripts/test-release-contracts.sh
sh scripts/test-round3-hardware-contract.sh
sh scripts/test-status-readonly.sh
sh scripts/test-v2-l2tp-adapter.sh
sh scripts/test-v2-live-integration.sh
sh scripts/test-v2-migration.sh
sh scripts/test-v2-phase5-gates.sh
sh scripts/test-whitespace-range.sh
git diff --check

echo "PASS: Node-backed LuCI behavior contracts ran in test-dns-fail-closed.sh"
