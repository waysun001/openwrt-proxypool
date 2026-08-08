#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
MAKEFILE="$ROOT/proxypool-core/Makefile"
LUCI_MAKEFILE="$ROOT/luci-app-proxypool/Makefile"
TEST_WORKFLOW="$ROOT/.github/workflows/test.yml"
FAST_WORKFLOW="$ROOT/.github/workflows/build-fast.yml"
FULL_WORKFLOW="$ROOT/.github/workflows/build.yml"
HOST_RUNNER="$ROOT/scripts/test-host.sh"
IPK_INSPECTOR="$ROOT/scripts/inspect-ipk.sh"
LUCI_IPK_INSPECTOR="$ROOT/scripts/inspect-luci-ipk.sh"
PACKAGE_DEFAULT="$ROOT/proxypool-core/files/proxypool.config"
IMAGE_OVERLAY_DEFAULT="$ROOT/files/etc/config/proxypool"
IMAGE_OVERLAY_V2="$ROOT/files/etc/config/proxypool_v2"
IMAGE_OVERLAY_SELECTOR="$ROOT/files/etc/config/proxypool_runtime"
IMAGE_ACTIVATION_REQUEST="$ROOT/files/etc/proxypool/v2-activation-request"
BACKEND_ACTIVATE_INIT="$ROOT/proxypool-core/files/proxypool-activate.init"
MAIN_INIT="$ROOT/proxypool-core/files/proxypool.init"
PACKAGED_V2_DEFAULT="$ROOT/proxypool-core/files/proxypool-v2-default"
UPGRADE_KEEP="$ROOT/proxypool-core/files/proxypool.keep"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT HUP INT TERM

require_fixed() {
	file=$1
	text=$2
	if ! grep -Fq -- "$text" "$file"; then
		printf '%s is missing contract: %s\n' "$file" "$text" >&2
		exit 1
	fi
}

job_block() {
	workflow=$1
	job=$2
	awk -v job="$job" '
		$0 == "  " job ":" { inside=1; next }
		inside && /^  [A-Za-z0-9_-]+:$/ { exit }
		inside { print }
	' "$workflow"
}

workflow_jobs() {
	awk '
		$0 == "jobs:" { in_jobs=1; next }
		in_jobs && /^[^[:space:]]/ { exit }
		in_jobs && /^  [A-Za-z0-9_-]+:$/ {
			name=$0
			sub(/^  /, "", name)
			sub(/:$/, "", name)
			print name
		}
	' "$1"
}

[ "$(grep -Ec '^define[[:space:]]+Build/Compile[[:space:]]*$' "$MAKEFILE" || true)" -eq 1 ] || {
	echo 'proxypool-core must define exactly one combined Go and C compiler path' >&2
	exit 1
}

require_fixed "$MAKEFILE" 'PKG_BUILD_DEPENDS:=golang/host'
require_fixed "$MAKEFILE" 'PKG_VERSION:=2.0.0'
require_fixed "$MAKEFILE" 'PKG_RELEASE:=13'
require_fixed "$MAKEFILE" 'GO_PKG:=proxypoold'
require_fixed "$MAKEFILE" 'GO_PKG_BUILD_PKG:=$(GO_PKG)/cmd/proxypoold $(GO_PKG)/cmd/proxypoolctl'
require_fixed "$MAKEFILE" 'GO_PKG_LDFLAGS_X:=$(GO_PKG)/internal/buildinfo.Version=$(PKG_VERSION)'
require_fixed "$MAKEFILE" 'include $(TOPDIR)/feeds/packages/lang/golang/golang-package.mk'
require_fixed "$MAKEFILE" '$(call GoPackage/Build/Compile)'
require_fixed "$MAKEFILE" '$(call Build/Compile/Ip2Region)'
require_fixed "$MAKEFILE" '$(CP) ./src/proxypoold/. $(PKG_BUILD_DIR)/'
require_fixed "$MAKEFILE" '$(GO_PKG_BUILD_BIN_DIR)/proxypoold $(1)/usr/sbin/proxypoold'
require_fixed "$MAKEFILE" '$(GO_PKG_BUILD_BIN_DIR)/proxypoolctl $(1)/usr/bin/proxypoolctl'
require_fixed "$MAKEFILE" '$(PKG_BUILD_DIR)/ip2region_searcher $(1)/usr/lib/proxypool/ip2region_searcher'
require_fixed "$MAKEFILE" '$(INSTALL_DIR) $(1)/lib/upgrade/keep.d'
require_fixed "$MAKEFILE" '$(INSTALL_DATA) ./files/proxypool.keep $(1)/lib/upgrade/keep.d/proxypool'
for dependency in firewall4 kmod-nft-bridge uci ucode ucode-mod-ubus ubus jshn ip-bridge coreutils-stat coreutils-timeout; do
	require_fixed "$MAKEFILE" "+$dependency"
done
require_fixed "$ROOT/proxypool-core/files/lan-isolation.sh" 'STAT="${PROXYPOOL_STAT:-/bin/stat}"'
require_fixed "$ROOT/proxypool-core/files/lan-isolation.sh" 'UBUS="${PROXYPOOL_UBUS:-/bin/ubus}"'
require_fixed "$ROOT/proxypool-core/files/lan-isolation.sh" 'JSHN="${PROXYPOOL_JSHN:-/usr/share/libubox/jshn.sh}"'
require_fixed "$MAKEFILE" 'define Package/proxypool-core/postinst'
require_fixed "$MAKEFILE" '"$${IPKG_INSTROOT}/usr/lib/proxypool/proxypool-postinst"'
for install_contract in \
	'$(INSTALL_BIN) ./files/proxypool-guard.init $(1)/etc/init.d/proxypool-guard' \
	'$(INSTALL_BIN) ./files/proxypool-activate.init $(1)/etc/init.d/proxypool-activate' \
	'$(INSTALL_BIN) ./files/guard-resync.sh $(1)/usr/lib/proxypool/guard-resync.sh' \
	'$(INSTALL_BIN) ./files/legacy-gate.sh $(1)/usr/lib/proxypool/legacy-gate.sh' \
	'$(INSTALL_BIN) ./files/lan-isolation.sh $(1)/usr/lib/proxypool/lan-isolation.sh' \
	'$(INSTALL_BIN) ./files/lan-isolation-worker.sh $(1)/usr/lib/proxypool/lan-isolation-worker.sh' \
	'$(INSTALL_BIN) ./files/proxypool-lan-isolation.hotplug $(1)/etc/hotplug.d/net/99-proxypool-lan-isolation' \
	'$(INSTALL_BIN) ./files/proxypool-lan-isolation.hotplug $(1)/etc/hotplug.d/iface/99-proxypool-lan-isolation' \
	'$(INSTALL_BIN) ./files/proxypool-netifd-event $(1)/etc/hotplug.d/iface/98-proxypool-v2-event' \
	'$(INSTALL_BIN) ./files/proxypool-firewall-defaults $(1)/usr/lib/proxypool/proxypool-firewall-defaults' \
	'$(INSTALL_BIN) ./files/proxypool-firewall-transaction $(1)/usr/lib/proxypool/proxypool-firewall-transaction' \
	'$(INSTALL_BIN) ./files/proxypool-fw4-activate $(1)/usr/lib/proxypool/proxypool-fw4-activate' \
	'$(INSTALL_BIN) ./files/proxypool-fw4-check-staged $(1)/usr/lib/proxypool/proxypool-fw4-check-staged' \
	'$(INSTALL_BIN) ./files/proxypool-postinst $(1)/usr/lib/proxypool/proxypool-postinst' \
	'$(INSTALL_BIN) ./files/proxypool-migrate.sh $(1)/usr/lib/proxypool/proxypool-migrate.sh' \
	'$(INSTALL_BIN) ./files/proxypool-backend-activate $(1)/usr/lib/proxypool/proxypool-backend-activate' \
	'$(INSTALL_BIN) ./files/proxypool-safety-uci-default $(1)/usr/lib/proxypool/proxypool-safety-uci-default' \
	'$(INSTALL_DATA) ./files/proxypool-v2-default $(1)/usr/lib/proxypool/proxypool-v2-default' \
	'$(INSTALL_DATA) ./files/proxypool-guard.nft $(1)/usr/lib/proxypool/proxypool-guard.nft' \
	'$(INSTALL_DATA) ./files/proxypool-fw4-input-gate.nft $(1)/usr/lib/proxypool/proxypool-fw4-input-gate.nft' \
	'$(INSTALL_DATA) ./files/proxypool-fw4-forward-gate.nft $(1)/usr/lib/proxypool/proxypool-fw4-forward-gate.nft' \
	'$(INSTALL_BIN) ./files/proxypool-ubus-call-stdin.uc $(1)/usr/lib/proxypool/ubus-call-stdin.uc' \
	'$(INSTALL_DATA) ./files/proxypool-uci-staged.uc $(1)/usr/lib/proxypool/proxypool-uci-staged.uc'; do
	require_fixed "$MAKEFILE" "$install_contract"
done
if grep -Fq '$(1)/etc/uci-defaults' "$MAKEFILE"; then
	echo 'core package must not directly own an /etc/uci-defaults payload' >&2
	exit 1
fi
if grep -Fq '$(1)/etc/proxypool/firewall-transaction' "$MAKEFILE"; then
	echo 'core package must not precreate the runtime firewall journal' >&2
	exit 1
fi
for forbidden_ppp_callback in \
	'$(1)/etc/ppp/ip-up' \
	'$(1)/etc/ppp/ip-down' \
	'$(1)/etc/ppp/ip-up.d' \
	'$(1)/etc/ppp/ip-down.d'; do
	if grep -Fq "$forbidden_ppp_callback" "$MAKEFILE"; then
		printf 'core package must not own PPP callback path: %s\n' "$forbidden_ppp_callback" >&2
		exit 1
	fi
done
if grep -Eq 'INSTALL_(CONF|DATA).*proxypool_(v2|runtime).*etc/config' "$MAKEFILE"; then
	echo 'package payload must not own the ImageBuilder-only V2 configs' >&2
	exit 1
fi
[ -f "$UPGRADE_KEEP" ] || { echo 'missing sysupgrade keep list for overlay-only configs' >&2; exit 1; }
expected_keep=$(printf '/etc/config/proxypool_v2\n/etc/proxypool/migration-v1.json\n/etc/proxypool/backups/\n')
[ "$(cat "$UPGRADE_KEEP")" = "$expected_keep" ] || { echo 'unexpected ProxyPool sysupgrade keep list' >&2; exit 1; }
require_fixed "$IPK_INSPECTOR" 'for runtime_state in activated-backend cleanup-required firewall-transaction firewall-safety-activated wireless-quarantine; do'
require_fixed "$IPK_INSPECTOR" '/etc/proxypool/migration-v1.json'
require_fixed "$IPK_INSPECTOR" 'usr/lib/proxypool/proxypool-migrate.sh'
require_fixed "$IPK_INSPECTOR" 'usr/lib/proxypool/ubus-call-stdin.uc'
require_fixed "$IPK_INSPECTOR" 'etc/hotplug.d/iface/98-proxypool-v2-event'
require_fixed "$IPK_INSPECTOR" 'ucode-mod-ubus'
require_fixed "$IPK_INSPECTOR" 'coreutils-timeout'

require_fixed "$HOST_RUNNER" 'go test -race -count=1 ./...'
require_fixed "$HOST_RUNNER" 'go test -count=1 ./...'
require_fixed "$HOST_RUNNER" 'SKIP: Go race detector requires a supported Linux host'
require_fixed "$HOST_RUNNER" 'test-proxypool-init.sh'
require_fixed "$HOST_RUNNER" 'test-legacy-quarantine.sh'
require_fixed "$HOST_RUNNER" 'test-status-readonly.sh'
require_fixed "$HOST_RUNNER" 'test-dns-fail-closed.sh'
require_fixed "$HOST_RUNNER" 'test-backup-contract.sh'
require_fixed "$HOST_RUNNER" 'test-backend-activation-integration.sh'
require_fixed "$HOST_RUNNER" 'test-proxypool-guard.sh'
require_fixed "$HOST_RUNNER" 'test-guardian-terminal-policy.sh'
require_fixed "$HOST_RUNNER" 'test-firewall-defaults.sh'
require_fixed "$HOST_RUNNER" 'test-fw4-activate.sh'
require_fixed "$HOST_RUNNER" 'test-release-contracts.sh'
require_fixed "$HOST_RUNNER" 'test-whitespace-range.sh'
require_fixed "$HOST_RUNNER" 'inspect-ipk.sh'
require_fixed "$HOST_RUNNER" 'test-inspect-ipk.sh'
require_fixed "$HOST_RUNNER" 'inspect-luci-ipk.sh'
require_fixed "$HOST_RUNNER" 'test-inspect-luci-ipk.sh'
require_fixed "$HOST_RUNNER" 'test-luci-package-source-safety.sh'
require_fixed "$HOST_RUNNER" 'test-package-safety-integration.sh'
require_fixed "$HOST_RUNNER" 'test-proxypool-dns-admission.sh'
require_fixed "$HOST_RUNNER" 'test-lan-isolation-defaults.sh'
require_fixed "$HOST_RUNNER" 'proxypool-core/files/lan-isolation-worker.sh'
require_fixed "$HOST_RUNNER" 'test-image-files.sh'
require_fixed "$HOST_RUNNER" 'regenerate-sha256sums.sh'
require_fixed "$HOST_RUNNER" 'test-artifact-sha256sums.sh'

for asset in proxypool.sh l2tp-manager.sh socks5-manager.sh slp-manager.sh firewall.sh status.sh backup.sh watchdog.sh lease.sh iplocation.sh timeout-check.sh timeout-rotate.sh update-ipdb.sh install-global-menu.sh uninstall-global-menu.sh dns-manager.sh slp-client ip2region.xdb; do
	[ -e "$ROOT/proxypool-core/files/$asset" ] || { echo "missing retained V1 asset: $asset" >&2; exit 1; }
	require_fixed "$MAKEFILE" "./files/$asset"
done
for source_only_callback in ip-up ip-down ppp-up.sh ppp-down.sh; do
	[ -e "$ROOT/proxypool-core/files/$source_only_callback" ] || {
		echo "missing retained source-only PPP callback: $source_only_callback" >&2
		exit 1
	}
done

[ -f "$TEST_WORKFLOW" ] || { echo 'missing Linux test workflow' >&2; exit 1; }
require_fixed "$TEST_WORKFLOW" "go-version: '1.20.14'"
require_fixed "$TEST_WORKFLOW" './scripts/test-host.sh'
require_fixed "$TEST_WORKFLOW" './scripts/check-whitespace-range.sh'
require_fixed "$TEST_WORKFLOW" 'GOARCH=arm64'
require_fixed "$TEST_WORKFLOW" 'CGO_ENABLED=0'

[ -f "$FAST_WORKFLOW" ] && [ ! -L "$FAST_WORKFLOW" ] || { echo 'missing regular fast build workflow' >&2; exit 1; }
[ -f "$FULL_WORKFLOW" ] && [ ! -L "$FULL_WORKFLOW" ] || { echo 'missing regular full-source build workflow' >&2; exit 1; }

# The fast path is package evidence only. ImageBuilder cannot replace the
# built-in MT7531 driver, so any firmware-producing token here is a release bug.
[ "$(workflow_jobs "$FAST_WORKFLOW")" = "$(printf 'host\nbuild')" ] || {
	echo 'fast workflow must contain only host and SDK build jobs' >&2
	exit 1
}
require_fixed "$FAST_WORKFLOW" 'pull_request:'
require_fixed "$FAST_WORKFLOW" 'needs: host'
require_fixed "$FAST_WORKFLOW" 'openwrt-sdk-23.05.3-mediatek-filogic_gcc-12.3.0_musl.Linux-x86_64.tar.xz'
require_fixed "$FAST_WORKFLOW" 'e51af2eff648e0f6ee8d8a918ed0973105a11daa9fe63e31eb52e315ca852fe5'
require_fixed "$FAST_WORKFLOW" 'OPENWRT_COMMIT: 01170d518da1c8ade9d26e56d0135d12cda8e781'
require_fixed "$FAST_WORKFLOW" 'sh ./scripts/prepare-sdk-base-packages.sh sdk openwrt-base'
require_fixed "$FAST_WORKFLOW" 'make package/proxypool/proxypool-core/clean'
require_fixed "$FAST_WORKFLOW" 'make package/proxypool/proxypool-core/compile V=s -j1'
require_fixed "$FAST_WORKFLOW" 'make package/proxypool/luci-app-proxypool/clean'
require_fixed "$FAST_WORKFLOW" 'make package/proxypool/luci-app-proxypool/compile V=s -j1'
require_fixed "$FAST_WORKFLOW" './scripts/inspect-ipk.sh'
require_fixed "$FAST_WORKFLOW" 'mapfile -t luci_packages'
require_fixed "$FAST_WORKFLOW" "find sdk/bin -type f -name 'luci-app-proxypool_*.ipk'"
require_fixed "$FAST_WORKFLOW" '[ "${#luci_packages[@]}" -eq 1 ]'
require_fixed "$FAST_WORKFLOW" 'sh ./scripts/inspect-luci-ipk.sh "${luci_packages[0]}"'
require_fixed "$FAST_WORKFLOW" 'feed_updated=0'
require_fixed "$FAST_WORKFLOW" 'for attempt in 1 2 3; do'
require_fixed "$FAST_WORKFLOW" 'feed_updated=1'
require_fixed "$FAST_WORKFLOW" '[ "$feed_updated" = "1" ] || {'
require_fixed "$FAST_WORKFLOW" 'PACKAGES_FEED_COMMIT: 063b2393cbc3e5aab9d2b40b2911cab1c3967c59'
require_fixed "$FAST_WORKFLOW" 'LUCI_FEED_COMMIT: b07cf9dcfc37e021e5619a41c847e63afbd5d34a'
require_fixed "$FAST_WORKFLOW" 'ROUTING_FEED_COMMIT: 648753932d5a7deff7f2bdb33c000018a709ad84'
require_fixed "$FAST_WORKFLOW" 'TELEPHONY_FEED_COMMIT: 86af194d03592121f5321474ec9918dd109d3057'
require_fixed "$FAST_WORKFLOW" 'git -C feeds/packages rev-parse HEAD)" = "$PACKAGES_FEED_COMMIT"'
require_fixed "$FAST_WORKFLOW" 'git -C feeds/luci rev-parse HEAD)" = "$LUCI_FEED_COMMIT"'
require_fixed "$FAST_WORKFLOW" 'git -C feeds/routing rev-parse HEAD)" = "$ROUTING_FEED_COMMIT"'
require_fixed "$FAST_WORKFLOW" 'git -C feeds/telephony rev-parse HEAD)" = "$TELEPHONY_FEED_COMMIT"'
require_fixed "$FAST_WORKFLOW" 'uses: actions/upload-artifact@v4'
require_fixed "$FAST_WORKFLOW" 'path: package-evidence/'
if grep -Eiq 'imagebuilder|make[[:space:]]+image|sysupgrade|factory|softprops/action-gh-release|contents:[[:space:]]*write' "$FAST_WORKFLOW"; then
	echo 'fast workflow still contains firmware or release publication behavior' >&2
	exit 1
fi
if grep -Eq '\|\|[[:space:]]*make[[:space:]]+package/' "$FAST_WORKFLOW"; then
	echo 'SDK package failure is swallowed by a fallback build' >&2
	exit 1
fi

# Only the pinned full-source workflow may publish a GL-MT6000 sysupgrade.
[ "$(workflow_jobs "$FULL_WORKFLOW")" = "$(printf 'host\nbuild')" ] || {
	echo 'full-source workflow must contain only host and build jobs' >&2
	exit 1
}
require_fixed "$FULL_WORKFLOW" 'OPENWRT_COMMIT: 01170d518da1c8ade9d26e56d0135d12cda8e781'
require_fixed "$FULL_WORKFLOW" 'PACKAGES_FEED_COMMIT: 063b2393cbc3e5aab9d2b40b2911cab1c3967c59'
require_fixed "$FULL_WORKFLOW" 'LUCI_FEED_COMMIT: b07cf9dcfc37e021e5619a41c847e63afbd5d34a'
require_fixed "$FULL_WORKFLOW" 'ROUTING_FEED_COMMIT: 648753932d5a7deff7f2bdb33c000018a709ad84'
require_fixed "$FULL_WORKFLOW" 'TELEPHONY_FEED_COMMIT: 86af194d03592121f5321474ec9918dd109d3057'
require_fixed "$FULL_WORKFLOW" 'git -C openwrt fetch --filter=blob:none --depth 1 origin "$OPENWRT_COMMIT"'
require_fixed "$FULL_WORKFLOW" '[ "$source_fetched" = "1" ]'
require_fixed "$FULL_WORKFLOW" 'git checkout --detach "$OPENWRT_COMMIT"'
require_fixed "$FULL_WORKFLOW" '[ "$(git rev-parse HEAD)" = "$OPENWRT_COMMIT" ]'
require_fixed "$FULL_WORKFLOW" 'mkdir -p local-feed'
require_fixed "$FULL_WORKFLOW" 'cp -a proxypool-core luci-app-proxypool local-feed/'
require_fixed "$FULL_WORKFLOW" 'src-link proxypool $GITHUB_WORKSPACE/local-feed'
require_fixed "$FULL_WORKFLOW" 'git -C feeds/packages rev-parse HEAD)" = "$PACKAGES_FEED_COMMIT"'
require_fixed "$FULL_WORKFLOW" 'git -C feeds/luci rev-parse HEAD)" = "$LUCI_FEED_COMMIT"'
require_fixed "$FULL_WORKFLOW" 'git -C feeds/routing rev-parse HEAD)" = "$ROUTING_FEED_COMMIT"'
require_fixed "$FULL_WORKFLOW" 'git -C feeds/telephony rev-parse HEAD)" = "$TELEPHONY_FEED_COMMIT"'
require_fixed "$FULL_WORKFLOW" 'sh ./scripts/prepare-image-files.sh files image-files'
require_fixed "$FULL_WORKFLOW" 'openwrt-patches/23.05.3/998-net-bridge-offload-br-isolated.patch'
require_fixed "$FULL_WORKFLOW" 'openwrt-patches/23.05.3/999-net-dsa-mt7530-bridge-port-isolation.patch'
require_fixed "$FULL_WORKFLOW" 'target/linux/generic/backport-5.15/'
require_fixed "$FULL_WORKFLOW" './scripts/feeds install -p proxypool proxypool-core luci-app-proxypool'
require_fixed "$FULL_WORKFLOW" "grep -Fqx 'CONFIG_PACKAGE_proxypool-core=y' .config"
require_fixed "$FULL_WORKFLOW" "grep -Fqx 'CONFIG_PACKAGE_luci-app-proxypool=y' .config"
require_fixed "$FULL_WORKFLOW" 'sh ../scripts/verify-openwrt-kernel-isolation.sh . ../openwrt-patches/23.05.3'
require_fixed "$FULL_WORKFLOW" 'make -j"$(nproc)" V=s'
require_fixed "$FULL_WORKFLOW" "find bin -type f -name 'proxypool-core_*.ipk'"
require_fixed "$FULL_WORKFLOW" "find bin -type f -name 'luci-app-proxypool_*.ipk'"
require_fixed "$FULL_WORKFLOW" "-name '*glinet_gl-mt6000*squashfs-sysupgrade.bin'"
require_fixed "$FULL_WORKFLOW" '[ "${#firmware[@]}" -eq 1 ]'
require_fixed "$FULL_WORKFLOW" 'cp "${packages[0]}" "${luci_packages[0]}" "${firmware[0]}" ../firmware-evidence/'
require_fixed "$FULL_WORKFLOW" 'uses: actions/upload-artifact@v4'
require_fixed "$FULL_WORKFLOW" 'path: firmware-evidence/'
if grep -Eiq 'imagebuilder|make[[:space:]]+image|factory|softprops/action-gh-release|contents:[[:space:]]*write|make[^#]*\|\|[[:space:]]*make' "$FULL_WORKFLOW"; then
	echo 'full-source workflow uses ImageBuilder/factory output, grants write authority, publishes a release, or swallows make failure' >&2
	exit 1
fi

firmware_workflow_count=0
for workflow in "$ROOT"/.github/workflows/*.yml "$ROOT"/.github/workflows/*.yaml; do
	[ -f "$workflow" ] || continue
	if grep -Eiq 'imagebuilder|make[[:space:]]+image|sysupgrade|factory' "$workflow"; then
		[ "$workflow" = "$FULL_WORKFLOW" ] || {
			echo "$workflow must not produce firmware; only the pinned full-source workflow may do so" >&2
			exit 1
		}
		firmware_workflow_count=$((firmware_workflow_count + 1))
	fi
done
[ "$firmware_workflow_count" -eq 1 ] || {
	echo 'the pinned full-source workflow must be the sole firmware producer' >&2
	exit 1
}
[ -f "$IMAGE_OVERLAY_DEFAULT" ] || { echo 'missing legacy rollback ImageBuilder default' >&2; exit 1; }
[ -f "$IMAGE_OVERLAY_V2" ] || { echo 'missing strict V2 ImageBuilder default' >&2; exit 1; }
[ -f "$IMAGE_OVERLAY_SELECTOR" ] || { echo 'missing ImageBuilder runtime selector' >&2; exit 1; }
[ -f "$IMAGE_ACTIVATION_REQUEST" ] || { echo 'missing ImageBuilder V2 activation request' >&2; exit 1; }
[ -f "$BACKEND_ACTIVATE_INIT" ] || { echo 'missing backend activation init script' >&2; exit 1; }
[ -f "$MAIN_INIT" ] || { echo 'missing main init script' >&2; exit 1; }
[ -f "$PACKAGED_V2_DEFAULT" ] || { echo 'missing packaged V2 migration target' >&2; exit 1; }
cmp -s "$PACKAGE_DEFAULT" "$IMAGE_OVERLAY_DEFAULT" || { echo 'ImageBuilder legacy rollback config differs from the package V1 baseline' >&2; exit 1; }
require_fixed "$IMAGE_OVERLAY_V2" "option runtime_backend 'v2_shadow'"
require_fixed "$IMAGE_OVERLAY_SELECTOR" "option runtime_backend 'v1'"
[ "$(cat "$IMAGE_ACTIVATION_REQUEST")" = image ] || { echo 'ImageBuilder V2 activation request is invalid' >&2; exit 1; }
require_fixed "$BACKEND_ACTIVATE_INIT" 'START=98'
require_fixed "$MAIN_INIT" 'START=99'
sed '/^#/d' "$PACKAGED_V2_DEFAULT" >"$TMP/packaged-v2.normalized"
sed '/^#/d' "$IMAGE_OVERLAY_V2" >"$TMP/image-v2.normalized"
cmp -s "$TMP/packaged-v2.normalized" "$TMP/image-v2.normalized" || {
	echo 'packaged and ImageBuilder V2 defaults differ semantically' >&2
	exit 1
}
if grep -Fq "option runtime_backend 'v2_shadow'" "$IMAGE_OVERLAY_SELECTOR"; then
	echo 'phase-1 ImageBuilder selector would silently switch a legacy V1 sysupgrade to V2' >&2
	exit 1
fi
if grep -Fq 'schema_version' "$IMAGE_OVERLAY_DEFAULT"; then
	echo 'legacy rollback config was replaced by a V2 config' >&2
	exit 1
fi
if grep -Fq 'FILES=../files' "$FAST_WORKFLOW" "$FULL_WORKFLOW"; then
	echo 'build workflow consumes unstaged overlay files without enforcing config mode 0600' >&2
	exit 1
fi
require_fixed "$IPK_INSPECTOR" 'etc/config/proxypool'
require_fixed "$IPK_INSPECTOR" 'etc/init.d/proxypool'
require_fixed "$IPK_INSPECTOR" 'usr/bin/slp-client'
require_fixed "$IPK_INSPECTOR" "printf '/etc/config/proxypool\\n'"
require_fixed "$LUCI_IPK_INSPECTOR" 'Architecture: all'
require_fixed "$LUCI_IPK_INSPECTOR" 'etc/uci-defaults/luci-proxypool'
require_fixed "$LUCI_IPK_INSPECTOR" 'www/luci-static/resources/proxypool-global.css'
require_fixed "$LUCI_IPK_INSPECTOR" 'www/luci-static/resources/proxypool-global.js'
require_fixed "$LUCI_MAKEFILE" 'PKG_FILE_MODES:=/etc/uci-defaults/luci-proxypool:root:root:0755'
if grep -Fq 'luci-proxypool-menu' "$LUCI_MAKEFILE"; then
	echo 'LuCI package still installs the legacy global-menu mutator' >&2
	exit 1
fi
if grep -Eq '^define[[:space:]]+Package/luci-app-proxypool/install' "$LUCI_MAKEFILE" ||
	grep -Fqx '$(eval $(call BuildPackage,luci-app-proxypool))' "$LUCI_MAKEFILE"; then
	echo 'LuCI package duplicates the install/build rules already owned by luci.mk' >&2
	exit 1
fi

for workflow in "$FAST_WORKFLOW" "$FULL_WORKFLOW"; do
	host_block=$(job_block "$workflow" host)
	[ -n "$host_block" ] || { echo "$workflow is missing the host gate job" >&2; exit 1; }
	normalized_host=$(printf '%s\n' "$host_block" | sed 's/^[[:space:]]*//')
	for host_contract in \
		'sh ./scripts/prepare-openwrt-nft.sh "$OPENWRT_NFT_ROOTFS"' \
		'echo "PROXYPOOL_OPENWRT_ROOTFS=$OPENWRT_NFT_ROOTFS"' \
		'echo "PROXYPOOL_TARGET_NFT=$GITHUB_WORKSPACE/scripts/openwrt-target-nft.sh"' \
		'} >> "$GITHUB_ENV"' \
		'run: ./scripts/test-host.sh'; do
		[ "$(printf '%s\n' "$normalized_host" | grep -Fxc "$host_contract" || true)" -eq 1 ] || {
			printf '%s host job is missing pinned nft contract: %s\n' "$workflow" "$host_contract" >&2
			exit 1
		}
	done
	prepare_line=$(printf '%s\n' "$normalized_host" | grep -nFx 'sh ./scripts/prepare-openwrt-nft.sh "$OPENWRT_NFT_ROOTFS"' | cut -d: -f1)
	host_test_line=$(printf '%s\n' "$normalized_host" | grep -nFx 'run: ./scripts/test-host.sh' | cut -d: -f1)
	[ "$prepare_line" -lt "$host_test_line" ] || {
		echo "$workflow runs host tests before preparing the pinned OpenWrt nft checker" >&2
		exit 1
	}
	printf '%s\n' "$host_block" | grep -Fq 'contents: read' || { echo "$workflow host job is not read-only" >&2; exit 1; }
done

fast_build=$(job_block "$FAST_WORKFLOW" build)
[ -n "$fast_build" ] || { echo 'missing fast SDK build job' >&2; exit 1; }
printf '%s\n' "$fast_build" | grep -Fq 'needs: host' || { echo 'fast SDK build does not depend on host gates' >&2; exit 1; }
printf '%s\n' "$fast_build" | grep -Fq 'contents: read' || { echo 'fast SDK build is not read-only' >&2; exit 1; }
normalized_fast_build=$(printf '%s\n' "$fast_build" | sed 's/^[[:space:]]*//')
for package_prefix in packages luci_packages; do
	case "$package_prefix" in
		packages)
			find_contract="mapfile -t packages < <(find sdk/bin -type f -name 'proxypool-core_*.ipk')"
			inspect_contract='sh ./scripts/inspect-ipk.sh "${packages[0]}" aarch64_cortex-a53'
			;;
		luci_packages)
			find_contract="mapfile -t luci_packages < <(find sdk/bin -type f -name 'luci-app-proxypool_*.ipk')"
			inspect_contract='sh ./scripts/inspect-luci-ipk.sh "${luci_packages[0]}"'
			;;
	esac
	count_contract="[ \"\${#$package_prefix[@]}\" -eq 1 ]"
	find_line=$(printf '%s\n' "$normalized_fast_build" | grep -nFx "$find_contract" | cut -d: -f1)
	count_line=$(printf '%s\n' "$normalized_fast_build" | grep -nFx "$count_contract" | cut -d: -f1)
	inspect_line=$(printf '%s\n' "$normalized_fast_build" | grep -nFx "$inspect_contract" | cut -d: -f1)
	[ -n "$find_line" ] && [ -n "$count_line" ] && [ -n "$inspect_line" ] &&
		[ "$find_line" -lt "$count_line" ] && [ "$count_line" -lt "$inspect_line" ] || {
		echo "fast SDK $package_prefix discovery, uniqueness, and inspection are missing or out of order" >&2
		exit 1
	}
done

full_build=$(job_block "$FULL_WORKFLOW" build)
[ -n "$full_build" ] || { echo 'missing full-source build job' >&2; exit 1; }
printf '%s\n' "$full_build" | grep -Fq 'needs: host' || { echo 'full-source build does not depend on host gates' >&2; exit 1; }
printf '%s\n' "$full_build" | grep -Fq 'contents: read' || { echo 'full-source build is not read-only' >&2; exit 1; }
normalized_full_build=$(printf '%s\n' "$full_build" | sed 's/^[[:space:]]*//')
patch_line=$(printf '%s\n' "$normalized_full_build" | grep -nF '../openwrt-patches/23.05.3/998-net-bridge-offload-br-isolated.patch \' | cut -d: -f1)
verify_line=$(printf '%s\n' "$normalized_full_build" | grep -nF 'sh ../scripts/verify-openwrt-kernel-isolation.sh . ../openwrt-patches/23.05.3' | cut -d: -f1)
compile_line=$(printf '%s\n' "$normalized_full_build" | grep -nF 'make -j"$(nproc)" V=s' | cut -d: -f1)
[ -n "$patch_line" ] && [ -n "$verify_line" ] && [ -n "$compile_line" ] &&
	[ "$patch_line" -lt "$compile_line" ] && [ "$compile_line" -lt "$verify_line" ] || {
	echo 'full-source patch, firmware compile, and kernel verifier gates are missing or out of order' >&2
	exit 1
}
for package_prefix in packages luci_packages; do
	case "$package_prefix" in
		packages)
			find_contract="mapfile -t packages < <(find bin -type f -name 'proxypool-core_*.ipk')"
			inspect_contract='sh ../scripts/inspect-ipk.sh "${packages[0]}" aarch64_cortex-a53'
			;;
		luci_packages)
			find_contract="mapfile -t luci_packages < <(find bin -type f -name 'luci-app-proxypool_*.ipk')"
			inspect_contract='sh ../scripts/inspect-luci-ipk.sh "${luci_packages[0]}"'
			;;
	esac
	count_contract="[ \"\${#$package_prefix[@]}\" -eq 1 ]"
	find_line=$(printf '%s\n' "$normalized_full_build" | grep -nFx "$find_contract" | cut -d: -f1)
	count_line=$(printf '%s\n' "$normalized_full_build" | grep -nFx "$count_contract" | cut -d: -f1)
	inspect_line=$(printf '%s\n' "$normalized_full_build" | grep -nFx "$inspect_contract" | cut -d: -f1)
	[ -n "$find_line" ] && [ -n "$count_line" ] && [ -n "$inspect_line" ] &&
		[ "$compile_line" -lt "$find_line" ] && [ "$find_line" -lt "$count_line" ] &&
		[ "$count_line" -lt "$inspect_line" ] || {
		echo "full-source $package_prefix discovery, uniqueness, and inspection are missing or out of order" >&2
		exit 1
	}
done
core_inspect_line=$(printf '%s\n' "$normalized_full_build" | grep -nF 'sh ../scripts/inspect-ipk.sh "${packages[0]}" aarch64_cortex-a53' | cut -d: -f1)
luci_inspect_line=$(printf '%s\n' "$normalized_full_build" | grep -nF 'sh ../scripts/inspect-luci-ipk.sh "${luci_packages[0]}"' | cut -d: -f1)
firmware_count_line=$(printf '%s\n' "$normalized_full_build" | grep -nF '[ "${#firmware[@]}" -eq 1 ]' | cut -d: -f1)
[ -n "$core_inspect_line" ] && [ -n "$luci_inspect_line" ] && [ -n "$firmware_count_line" ] &&
	[ "$compile_line" -lt "$core_inspect_line" ] && [ "$compile_line" -lt "$luci_inspect_line" ] &&
	[ "$compile_line" -lt "$firmware_count_line" ] || {
	echo 'full-source artifacts are inspected or selected before the full compile succeeds' >&2
	exit 1
}

echo 'release contracts: PASS'
