#!/usr/bin/env sh
set -eu

if [ "$#" -ne 2 ]; then
	echo 'usage: verify-openwrt-kernel-isolation.sh OPENWRT_TREE PATCH_SOURCE_DIR' >&2
	exit 2
fi

OPENWRT_TREE=$1
PATCH_SOURCE_DIR=$2
EXPECTED_COMMIT=01170d518da1c8ade9d26e56d0135d12cda8e781
TARGET_PATCH_DIR="$OPENWRT_TREE/target/linux/generic/backport-5.15"

[ -d "$OPENWRT_TREE/.git" ] || {
	echo 'OpenWrt source is not a git checkout' >&2
	exit 1
}
[ "$(git -C "$OPENWRT_TREE" rev-parse HEAD)" = "$EXPECTED_COMMIT" ] || {
	echo 'OpenWrt source commit is not the pinned v23.05.3 revision' >&2
	exit 1
}

for patch_name in \
	998-net-bridge-offload-br-isolated.patch \
	999-net-dsa-mt7530-bridge-port-isolation.patch; do
	source_patch="$PATCH_SOURCE_DIR/$patch_name"
	target_patch="$TARGET_PATCH_DIR/$patch_name"
	[ -f "$source_patch" ] && [ ! -L "$source_patch" ] || {
		echo "missing regular source patch: $source_patch" >&2
		exit 1
	}
	[ -f "$target_patch" ] && [ ! -L "$target_patch" ] || {
		echo "missing regular installed patch: $target_patch" >&2
		exit 1
	}
	cmp -s "$source_patch" "$target_patch" || {
		echo "installed kernel patch differs from source: $patch_name" >&2
		exit 1
	}
done

set -- "$OPENWRT_TREE"/build_dir/target-*/linux-mediatek_filogic/linux-5.15.150
[ "$#" -eq 1 ] && [ -d "$1" ] || {
	echo 'expected exactly one prepared mediatek/filogic Linux 5.15.150 tree' >&2
	exit 1
}
KERNEL_TREE=$1
BRIDGE="$KERNEL_TREE/net/bridge/br_switchdev.c"
DSA_PORT="$KERNEL_TREE/net/dsa/port.c"
DSA_SLAVE="$KERNEL_TREE/net/dsa/slave.c"
MT7530="$KERNEL_TREE/drivers/net/dsa/mt7530.c"
MT7530_H="$KERNEL_TREE/drivers/net/dsa/mt7530.h"

for source_file in "$BRIDGE" "$DSA_PORT" "$DSA_SLAVE" "$MT7530" "$MT7530_H"; do
	[ -f "$source_file" ] && [ ! -L "$source_file" ] || {
		echo "missing prepared kernel source: $source_file" >&2
		exit 1
	}
done

bridge_flags=$(sed -n '/#define BR_PORT_FLAGS_HW_OFFLOAD/,/^$/p' "$BRIDGE")
printf '%s\n' "$bridge_flags" | grep -Fq 'BR_MCAST_FLOOD | BR_BCAST_FLOOD |' || {
	echo 'prepared bridge hardware-offload mask has unexpected shape' >&2
	exit 1
}
printf '%s\n' "$bridge_flags" | grep -Fq 'BR_ISOLATED' || {
	echo 'prepared bridge does not send BR_ISOLATED to switchdev' >&2
	exit 1
}

update_member=$(sed -n '/static void mt7530_update_port_member/,/^}/p' "$MT7530")
printf '%s\n' "$update_member" | grep -Fq 'BIT(cpu_dp->index)' || {
	echo 'MT7530 isolation matrix no longer preserves the CPU port' >&2
	exit 1
}
printf '%s\n' "$update_member" | grep -Fq 'p->isolated && other_p->isolated' || {
	echo 'MT7530 isolation matrix does not require both ports to be isolated' >&2
	exit 1
}
printf '%s\n' "$update_member" | grep -Fq 'other_p->pm &= ~PCR_MATRIX(BIT(port))' || {
	echo 'MT7530 isolation matrix does not remove isolated peer forwarding' >&2
	exit 1
}

pre_flags=$(sed -n '/mt7530_port_pre_bridge_flags/,/^}/p' "$MT7530")
printf '%s\n' "$pre_flags" | grep -Fq 'BR_BCAST_FLOOD | BR_ISOLATED' || {
	echo 'MT7530 driver does not accept the BR_ISOLATED flag' >&2
	exit 1
}
flag_apply=$(sed -n '/mt7530_port_bridge_flags/,/^}/p' "$MT7530")
printf '%s\n' "$flag_apply" | grep -Fq 'flags.mask & BR_ISOLATED' || {
	echo 'MT7530 driver does not apply the BR_ISOLATED flag' >&2
	exit 1
}
grep -Fq 'bool isolated;' "$MT7530_H" || {
	echo 'MT7530 driver does not retain per-port isolation state' >&2
	exit 1
}

grep -Fq 'static bool proxypool_mt7531_cpu_only_bridge = true;' "$MT7530" || {
	echo 'MT7531 hardware bridge offload is not fail-closed by default' >&2
	exit 1
}
grep -Fq 'module_param_named(proxypool_cpu_only_bridge,' "$MT7530" || {
	echo 'MT7531 CPU-only bridge policy has no runtime proof parameter' >&2
	exit 1
}
grep -Fq 'proxypool_mt7531_cpu_only_bridge, bool, 0444);' "$MT7530" || {
	echo 'MT7531 CPU-only bridge proof parameter is not read-only' >&2
	exit 1
}
bridge_join=$(sed -n '/mt7530_port_bridge_join/,/^}/p' "$MT7530")
printf '%s\n' "$bridge_join" | grep -Fq 'priv->id == ID_MT7531 && proxypool_mt7531_cpu_only_bridge' || {
	echo 'MT7531 hardware bridge offload is not fail-closed' >&2
	exit 1
}
printf '%s\n' "$bridge_join" | grep -Fq 'return -EOPNOTSUPP;' || {
	echo 'MT7531 bridge join does not remain on the CPU path' >&2
	exit 1
}
cpu_only_line=$(printf '%s\n' "$bridge_join" | grep -n -F 'priv->id == ID_MT7531' | cut -d: -f1)
matrix_line=$(printf '%s\n' "$bridge_join" | grep -n -F 'mt7530_update_port_member' | cut -d: -f1)
[ -n "$cpu_only_line" ] && [ -n "$matrix_line" ] && [ "$cpu_only_line" -lt "$matrix_line" ] || {
	echo 'MT7531 CPU-only gate does not precede hardware matrix programming' >&2
	exit 1
}
port_enable=$(sed -n '/mt7530_port_enable/,/^}/p' "$MT7530")
printf '%s\n' "$port_enable" | grep -Fq 'priv->ports[port].pm |= PCR_MATRIX(BIT(cpu_dp->index))' || {
	echo 'MT7531 standalone user-port matrix does not preserve the CPU path' >&2
	exit 1
}
dsa_join=$(sed -n '/^int dsa_port_bridge_join/,/^}/p' "$DSA_PORT")
printf '%s\n' "$dsa_join" | grep -Fq 'out_rollback:' || {
	echo 'DSA does not roll back a refused hardware bridge join' >&2
	exit 1
}
printf '%s\n' "$dsa_join" | grep -Fq 'dp->bridge_dev = NULL;' || {
	echo 'DSA refused bridge join leaves stale offload ownership' >&2
	exit 1
}
dsa_changeupper=$(sed -n '/^static int dsa_slave_changeupper/,/^}/p' "$DSA_SLAVE")
printf '%s\n' "$dsa_changeupper" | grep -Fq 'if (err == -EOPNOTSUPP)' || {
	echo 'DSA does not preserve the software bridge fallback' >&2
	exit 1
}
printf '%s\n' "$dsa_changeupper" | grep -Fq 'err = 0;' || {
	echo 'DSA software bridge fallback no longer accepts refused offload' >&2
	exit 1
}

[ -f "$KERNEL_TREE/.config" ] || {
	echo 'prepared target kernel has no .config' >&2
	exit 1
}
grep -Fqx 'CONFIG_NET_DSA_MT7530=y' "$KERNEL_TREE/.config" || {
	echo 'MT7530 driver is not built into the prepared target kernel' >&2
	exit 1
}
grep -Fqx 'CONFIG_NET_DSA_MT7530_MDIO=y' "$KERNEL_TREE/.config" || {
	echo 'MT7530 MDIO transport is not built into the prepared target kernel' >&2
	exit 1
}

if find "$KERNEL_TREE" -name '*.rej' -o -name '*.orig' | grep -q .; then
	echo 'prepared target kernel contains patch rejects or backup files' >&2
	exit 1
fi

echo 'prepared OpenWrt MT7531 hardware-isolation kernel: PASS'
