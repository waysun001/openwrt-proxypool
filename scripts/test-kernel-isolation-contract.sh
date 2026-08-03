#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
PATCH_DIR="$ROOT/openwrt-patches/23.05.3"
BRIDGE_PATCH="$PATCH_DIR/998-net-bridge-offload-br-isolated.patch"
MT7530_PATCH="$PATCH_DIR/999-net-dsa-mt7530-bridge-port-isolation.patch"
FULL_WORKFLOW="$ROOT/.github/workflows/build.yml"
FAST_WORKFLOW="$ROOT/.github/workflows/build-fast.yml"
VERIFY="$ROOT/scripts/verify-openwrt-kernel-isolation.sh"
CONFIG="$ROOT/config/gl-mt6000.config"

require_fixed() {
	file=$1
	text=$2
	[ -f "$file" ] || {
		printf 'missing contract file: %s\n' "$file" >&2
		exit 1
	}
	grep -Fq "$text" "$file" || {
		printf '%s is missing contract: %s\n' "$file" "$text" >&2
		exit 1
	}
}

for patch in "$BRIDGE_PATCH" "$MT7530_PATCH"; do
	[ -f "$patch" ] && [ ! -L "$patch" ] || {
		printf 'missing regular OpenWrt kernel patch: %s\n' "$patch" >&2
		exit 1
	}
	[ -s "$patch" ] || {
		printf 'empty OpenWrt kernel patch: %s\n' "$patch" >&2
		exit 1
	}
done

# These IDs are the upstream provenance. The checked-in patches are deliberate
# adaptations to OpenWrt 23.05.3's final 5.15.150 patch-stack API, not blind
# copies of patches for a newer kernel.
require_fixed "$BRIDGE_PATCH" 'c3976a3f84451ca05ea5be013af6071bf9acab2c'
require_fixed "$BRIDGE_PATCH" 'BR_PORT_FLAGS_HW_OFFLOAD'
require_fixed "$BRIDGE_PATCH" '+				  BR_ISOLATED)'
require_fixed "$MT7530_PATCH" 'c25c961fc7f36682f0a530150f1b7453ebc344cd'
require_fixed "$MT7530_PATCH" '3d49ee2127c26fd2c77944fd2e3168c057f99439'
require_fixed "$MT7530_PATCH" 'mt7530_update_port_member'
require_fixed "$MT7530_PATCH" 'p->isolated && other_p->isolated'
require_fixed "$MT7530_PATCH" 'other_p->pm &= ~PCR_MATRIX(BIT(port))'
require_fixed "$MT7530_PATCH" 'BR_BCAST_FLOOD | BR_ISOLATED'
require_fixed "$MT7530_PATCH" 'bool isolated;'
require_fixed "$MT7530_PATCH" 'proxypool_mt7531_cpu_only_bridge'
require_fixed "$MT7530_PATCH" 'module_param_named(proxypool_cpu_only_bridge'
require_fixed "$MT7530_PATCH" 'priv->id == ID_MT7531'
require_fixed "$MT7530_PATCH" 'return -EOPNOTSUPP;'

require_fixed "$CONFIG" 'CONFIG_PACKAGE_ip-bridge=y'

# Only the full-source build is allowed to emit a GL-MT6000 firmware image.
# SDK remains useful for package inspection; ImageBuilder cannot replace the
# built-in MT7530 driver and therefore must never publish a testable image.
if grep -Eq 'IMAGEBUILDER|imagebuilder|make[[:space:]]+image|sysupgrade' "$FAST_WORKFLOW"; then
	echo 'fast workflow still emits firmware without the required custom kernel' >&2
	exit 1
fi

require_fixed "$FULL_WORKFLOW" 'OPENWRT_COMMIT: 01170d518da1c8ade9d26e56d0135d12cda8e781'
require_fixed "$FULL_WORKFLOW" 'git checkout --detach "$OPENWRT_COMMIT"'
require_fixed "$FULL_WORKFLOW" '[ "$(git rev-parse HEAD)" = "$OPENWRT_COMMIT" ]'
require_fixed "$FULL_WORKFLOW" 'openwrt-patches/23.05.3/998-net-bridge-offload-br-isolated.patch'
require_fixed "$FULL_WORKFLOW" 'openwrt-patches/23.05.3/999-net-dsa-mt7530-bridge-port-isolation.patch'
require_fixed "$FULL_WORKFLOW" 'target/linux/generic/backport-5.15/'
require_fixed "$FULL_WORKFLOW" 'make target/linux/prepare V=s'
require_fixed "$FULL_WORKFLOW" 'sh ../scripts/verify-openwrt-kernel-isolation.sh .'
if grep -Eq 'make[^#\n]*\|\|[[:space:]]*make' "$FULL_WORKFLOW"; then
	echo 'full-source kernel or firmware build failure is swallowed by fallback make' >&2
	exit 1
fi

require_fixed "$VERIFY" 'net/bridge/br_switchdev.c'
require_fixed "$VERIFY" 'net/dsa/port.c'
require_fixed "$VERIFY" 'net/dsa/slave.c'
require_fixed "$VERIFY" 'drivers/net/dsa/mt7530.c'
require_fixed "$VERIFY" 'drivers/net/dsa/mt7530.h'
require_fixed "$VERIFY" 'BR_MCAST_FLOOD | BR_BCAST_FLOOD |'
require_fixed "$VERIFY" 'BR_ISOLATED'
require_fixed "$VERIFY" 'p->isolated && other_p->isolated'
require_fixed "$VERIFY" 'other_p->pm &= ~PCR_MATRIX(BIT(port))'
require_fixed "$VERIFY" 'proxypool_mt7531_cpu_only_bridge'
require_fixed "$VERIFY" 'MT7531 hardware bridge offload is not fail-closed'
require_fixed "$VERIFY" 'DSA does not roll back a refused hardware bridge join'
require_fixed "$VERIFY" 'DSA does not preserve the software bridge fallback'

echo 'OpenWrt MT7531 hardware-isolation contracts: PASS'
