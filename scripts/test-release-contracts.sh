#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
MAKEFILE="$ROOT/proxypool-core/Makefile"
TEST_WORKFLOW="$ROOT/.github/workflows/test.yml"
BUILD_WORKFLOW="$ROOT/.github/workflows/build-fast.yml"
HOST_RUNNER="$ROOT/scripts/test-host.sh"
IPK_INSPECTOR="$ROOT/scripts/inspect-ipk.sh"
PACKAGE_DEFAULT="$ROOT/proxypool-core/files/proxypool.config"
IMAGE_OVERLAY_DEFAULT="$ROOT/files/etc/config/proxypool"
IMAGE_OVERLAY_V2="$ROOT/files/etc/config/proxypool_v2"
IMAGE_OVERLAY_SELECTOR="$ROOT/files/etc/config/proxypool_runtime"
UPGRADE_KEEP="$ROOT/proxypool-core/files/proxypool.keep"

require_fixed() {
	file=$1
	text=$2
	if ! grep -Fq "$text" "$file"; then
		printf '%s is missing contract: %s\n' "$file" "$text" >&2
		exit 1
	fi
}

if grep -Eq '^define[[:space:]]+Build/Compile[[:space:]]*$' "$MAKEFILE"; then
	echo 'proxypool-core must not override Go-owned Build/Compile' >&2
	exit 1
fi

require_fixed "$MAKEFILE" 'PKG_BUILD_DEPENDS:=golang/host'
require_fixed "$MAKEFILE" 'PKG_VERSION:=2.0.0'
require_fixed "$MAKEFILE" 'PKG_RELEASE:=1'
require_fixed "$MAKEFILE" 'GO_PKG:=proxypoold'
require_fixed "$MAKEFILE" 'GO_PKG_BUILD_PKG:=$(GO_PKG)/cmd/proxypoold $(GO_PKG)/cmd/proxypoolctl'
require_fixed "$MAKEFILE" 'GO_PKG_LDFLAGS_X:=$(GO_PKG)/internal/buildinfo.Version=$(PKG_VERSION)'
require_fixed "$MAKEFILE" 'include $(TOPDIR)/feeds/packages/lang/golang/golang-package.mk'
require_fixed "$MAKEFILE" 'Hooks/Compile/Post+=Build/Compile/Ip2Region'
require_fixed "$MAKEFILE" '$(CP) ./src/proxypoold/. $(PKG_BUILD_DIR)/'
require_fixed "$MAKEFILE" '$(GO_PKG_BUILD_BIN_DIR)/proxypoold $(1)/usr/sbin/proxypoold'
require_fixed "$MAKEFILE" '$(GO_PKG_BUILD_BIN_DIR)/proxypoolctl $(1)/usr/bin/proxypoolctl'
require_fixed "$MAKEFILE" '$(PKG_BUILD_DIR)/ip2region_searcher $(1)/usr/lib/proxypool/ip2region_searcher'
require_fixed "$MAKEFILE" '$(INSTALL_DIR) $(1)/lib/upgrade/keep.d'
require_fixed "$MAKEFILE" '$(INSTALL_DATA) ./files/proxypool.keep $(1)/lib/upgrade/keep.d/proxypool'
if grep -Eq 'INSTALL_(CONF|DATA).*proxypool_(v2|runtime).*etc/config' "$MAKEFILE"; then
	echo 'package payload must not own the ImageBuilder-only V2 configs' >&2
	exit 1
fi
[ -f "$UPGRADE_KEEP" ] || { echo 'missing sysupgrade keep list for overlay-only configs' >&2; exit 1; }
expected_keep=$(printf '/etc/config/proxypool_v2\n/etc/config/proxypool_runtime\n/etc/proxypool/activated-backend\n/etc/proxypool/cleanup-required\n')
[ "$(cat "$UPGRADE_KEEP")" = "$expected_keep" ] || { echo 'unexpected ProxyPool sysupgrade keep list' >&2; exit 1; }

require_fixed "$HOST_RUNNER" 'go test -race -count=1 ./...'
require_fixed "$HOST_RUNNER" 'go test -count=1 ./...'
require_fixed "$HOST_RUNNER" 'SKIP: Go race detector requires a supported Linux host'
require_fixed "$HOST_RUNNER" 'test-proxypool-init.sh'
require_fixed "$HOST_RUNNER" 'test-release-contracts.sh'
require_fixed "$HOST_RUNNER" 'test-whitespace-range.sh'
require_fixed "$HOST_RUNNER" 'inspect-ipk.sh'
require_fixed "$HOST_RUNNER" 'test-inspect-ipk.sh'
require_fixed "$HOST_RUNNER" 'test-image-files.sh'
require_fixed "$HOST_RUNNER" 'regenerate-sha256sums.sh'
require_fixed "$HOST_RUNNER" 'test-artifact-sha256sums.sh'

for asset in proxypool.sh l2tp-manager.sh socks5-manager.sh slp-manager.sh firewall.sh status.sh backup.sh watchdog.sh lease.sh iplocation.sh timeout-check.sh timeout-rotate.sh update-ipdb.sh install-global-menu.sh uninstall-global-menu.sh dns-manager.sh slp-client ip-up ip-down ppp-up.sh ppp-down.sh ip2region.xdb; do
	[ -e "$ROOT/proxypool-core/files/$asset" ] || { echo "missing retained V1 asset: $asset" >&2; exit 1; }
	require_fixed "$MAKEFILE" "./files/$asset"
done

[ -f "$TEST_WORKFLOW" ] || { echo 'missing Linux test workflow' >&2; exit 1; }
require_fixed "$TEST_WORKFLOW" "go-version: '1.20.14'"
require_fixed "$TEST_WORKFLOW" './scripts/test-host.sh'
require_fixed "$TEST_WORKFLOW" './scripts/check-whitespace-range.sh'
require_fixed "$TEST_WORKFLOW" 'GOARCH=arm64'
require_fixed "$TEST_WORKFLOW" 'CGO_ENABLED=0'

require_fixed "$BUILD_WORKFLOW" 'pull_request:'
require_fixed "$BUILD_WORKFLOW" 'needs: host'
require_fixed "$BUILD_WORKFLOW" 'openwrt-sdk-23.05.3-mediatek-filogic_gcc-12.3.0_musl.Linux-x86_64.tar.xz'
require_fixed "$BUILD_WORKFLOW" 'e51af2eff648e0f6ee8d8a918ed0973105a11daa9fe63e31eb52e315ca852fe5'
require_fixed "$BUILD_WORKFLOW" '3fbe4df2261718d7824fc71eb0b063e4711bc2614ec89556b314df3996e8dddb'
require_fixed "$BUILD_WORKFLOW" 'make package/proxypool/proxypool-core/clean'
require_fixed "$BUILD_WORKFLOW" 'make package/proxypool/proxypool-core/compile V=s -j1'
require_fixed "$BUILD_WORKFLOW" 'make package/proxypool/luci-app-proxypool/clean'
require_fixed "$BUILD_WORKFLOW" 'make package/proxypool/luci-app-proxypool/compile V=s -j1'
require_fixed "$BUILD_WORKFLOW" './scripts/inspect-ipk.sh'
require_fixed "$BUILD_WORKFLOW" "github.event_name != 'pull_request'"
require_fixed "$BUILD_WORKFLOW" 'feed_updated=0'
require_fixed "$BUILD_WORKFLOW" 'for attempt in 1 2 3; do'
require_fixed "$BUILD_WORKFLOW" 'feed_updated=1'
require_fixed "$BUILD_WORKFLOW" '[ "$feed_updated" = "1" ] || {'
require_fixed "$BUILD_WORKFLOW" 'sh ./scripts/prepare-image-files.sh files image-files'
require_fixed "$BUILD_WORKFLOW" 'FILES=../image-files'
require_fixed "$BUILD_WORKFLOW" 'sh ./scripts/regenerate-sha256sums.sh output'
if grep -Fq -- '-o -name sha256sums' "$BUILD_WORKFLOW"; then
	echo 'artifact collection copies an upstream manifest that references omitted files' >&2
	exit 1
fi
if grep -Fq 'repositories.conf' "$BUILD_WORKFLOW"; then
	echo 'ImageBuilder workflow must use its built-in packages/ feed without rewriting repositories.conf' >&2
	exit 1
fi
if grep -Fq 'openwrt-sha256sums' "$BUILD_WORKFLOW"; then
	echo 'ImageBuilder checksum must be pinned rather than trusted from a second dynamic download' >&2
	exit 1
fi
[ -f "$IMAGE_OVERLAY_DEFAULT" ] || { echo 'missing legacy rollback ImageBuilder default' >&2; exit 1; }
[ -f "$IMAGE_OVERLAY_V2" ] || { echo 'missing strict V2 ImageBuilder default' >&2; exit 1; }
[ -f "$IMAGE_OVERLAY_SELECTOR" ] || { echo 'missing ImageBuilder runtime selector' >&2; exit 1; }
cmp -s "$PACKAGE_DEFAULT" "$IMAGE_OVERLAY_DEFAULT" || { echo 'ImageBuilder legacy rollback config differs from the package V1 baseline' >&2; exit 1; }
require_fixed "$IMAGE_OVERLAY_V2" "option runtime_backend 'v2_shadow'"
require_fixed "$IMAGE_OVERLAY_SELECTOR" "option runtime_backend 'v1'"
if grep -Fq "option runtime_backend 'v2_shadow'" "$IMAGE_OVERLAY_SELECTOR"; then
	echo 'phase-1 ImageBuilder selector would silently switch a legacy V1 sysupgrade to V2' >&2
	exit 1
fi
if grep -Fq 'schema_version' "$IMAGE_OVERLAY_DEFAULT"; then
	echo 'legacy rollback config was replaced by a V2 config' >&2
	exit 1
fi
if grep -Fq 'FILES=../files' "$BUILD_WORKFLOW"; then
	echo 'ImageBuilder consumes unstaged overlay files without enforcing config mode 0600' >&2
	exit 1
fi
require_fixed "$IPK_INSPECTOR" 'etc/config/proxypool'
require_fixed "$IPK_INSPECTOR" 'etc/init.d/proxypool'
require_fixed "$IPK_INSPECTOR" 'usr/bin/slp-client'
require_fixed "$IPK_INSPECTOR" "printf '/etc/config/proxypool\\n'"
job_block() {
	awk -v job="$1" '
		$0 == "  " job ":" { inside=1; next }
		inside && /^  [A-Za-z0-9_-]+:$/ { exit }
		inside { print }
	' "$BUILD_WORKFLOW"
}
build_block=$(job_block build)
release_block=$(job_block release)
printf '%s\n' "$build_block" | grep -Fq 'contents: read' || { echo 'PR build job must have read-only repository permissions' >&2; exit 1; }
if printf '%s\n' "$build_block" | grep -Fq 'contents: write'; then
	echo 'PR build job exposes a repository write token' >&2
	exit 1
fi
[ -n "$release_block" ] || { echo 'missing isolated release job' >&2; exit 1; }
printf '%s\n' "$release_block" | grep -Fq 'needs: build' || { echo 'release job does not depend on verified build' >&2; exit 1; }
printf '%s\n' "$release_block" | grep -Fq 'contents: write' || { echo 'release job lacks scoped write permission' >&2; exit 1; }
printf '%s\n' "$release_block" | grep -Fq "github.event_name != 'pull_request'" || { echo 'release job is not excluded from pull requests' >&2; exit 1; }
printf '%s\n' "$release_block" | grep -Fq 'actions/download-artifact@v4' || { echo 'release job does not consume the verified artifact' >&2; exit 1; }
if printf '%s\n' "$build_block" | grep -Fq 'softprops/action-gh-release'; then
	echo 'release publisher still runs in the PR build job' >&2
	exit 1
fi
if grep -Eq '\|\|[[:space:]]*make[[:space:]]+package/' "$BUILD_WORKFLOW"; then
	echo 'SDK package failure is swallowed by a fallback build' >&2
	exit 1
fi

echo 'release contracts: PASS'
