#!/usr/bin/env sh
set -eu

if [ "$#" -ne 2 ]; then
	echo 'usage: inspect-ipk.sh PACKAGE.ipk EXPECTED_PACKAGE_ARCH' >&2
	exit 2
fi

package=$1
expected_arch=$2
[ -f "$package" ] || { echo "package not found: $package" >&2; exit 1; }

inspect_tmp=$(mktemp -d)
trap 'rm -rf "$inspect_tmp"' EXIT HUP INT TERM
mkdir "$inspect_tmp/outer" "$inspect_tmp/data" "$inspect_tmp/control"
tar -xzf "$package" -C "$inspect_tmp/outer"
[ "$(tr -d '\r\n' <"$inspect_tmp/outer/debian-binary")" = 2.0 ] || { echo 'invalid IPK format version' >&2; exit 1; }
tar -xzf "$inspect_tmp/outer/data.tar.gz" -C "$inspect_tmp/data"
tar -xzf "$inspect_tmp/outer/control.tar.gz" -C "$inspect_tmp/control"
grep -Fqx "Architecture: $expected_arch" "$inspect_tmp/control/control" || { echo 'unexpected package architecture metadata' >&2; exit 1; }

dependency_names=$(
	sed -n 's/^Depends:[[:space:]]*//p' "$inspect_tmp/control/control" |
		tr ',' '\n' |
		sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*(.*$//' -e 's/[[:space:]]*$//'
)
for required_dependency in firewall4 kmod-nft-bridge uci ucode ucode-mod-ubus ubus jshn ip-bridge coreutils-stat coreutils-timeout; do
	printf '%s\n' "$dependency_names" | grep -Fqx "$required_dependency" || {
		echo "missing required dependency: $required_dependency" >&2
		exit 1
	}
done

conffiles="$inspect_tmp/control/conffiles"
expected_conffiles="$inspect_tmp/expected-conffiles"
printf '/etc/config/proxypool\n' >"$expected_conffiles"
[ -f "$conffiles" ] && cmp -s "$expected_conffiles" "$conffiles" || {
	echo 'control/conffiles must contain exactly /etc/config/proxypool followed by LF' >&2
	exit 1
}

postinst_pkg="$inspect_tmp/control/postinst-pkg"
expected_postinst="$inspect_tmp/expected-postinst-pkg"
printf '%s\n' \
	'#!/bin/sh' \
	'"${IPKG_INSTROOT}/usr/lib/proxypool/proxypool-postinst"' \
	>"$expected_postinst"
[ -f "$postinst_pkg" ] && [ -x "$postinst_pkg" ] || {
	echo 'missing executable control/postinst-pkg' >&2
	exit 1
}
[ "$(stat -c '%a' "$postinst_pkg")" = 755 ] || {
	echo 'unexpected mode for control/postinst-pkg' >&2
	exit 1
}
cmp -s "$expected_postinst" "$postinst_pkg" || {
	echo 'invalid control/postinst-pkg' >&2
	exit 1
}

config="$inspect_tmp/data/etc/config/proxypool"
[ -f "$config" ] || { echo 'missing /etc/config/proxypool' >&2; exit 1; }
[ "$(stat -c '%a' "$config")" = 600 ] || { echo 'unexpected mode for /etc/config/proxypool' >&2; exit 1; }
[ "$(sha256sum "$config" | awk '{ print $1 }')" = '00f37918933d1e7a66fc0b83b7791c164e15ea835a7fa6bee5761701f9291958' ] || {
	echo 'packaged /etc/config/proxypool is not the exact V1 rollback baseline' >&2
	exit 1
}
for overlay_only in proxypool_v2 proxypool_runtime; do
	[ ! -e "$inspect_tmp/data/etc/config/$overlay_only" ] || {
		echo "package payload unexpectedly owns /etc/config/$overlay_only" >&2
		exit 1
	}
done
for runtime_state in activated-backend cleanup-required firewall-transaction firewall-safety-activated wireless-quarantine; do
	[ ! -e "$inspect_tmp/data/etc/proxypool/$runtime_state" ] || {
		echo "package payload unexpectedly owns /etc/proxypool/$runtime_state" >&2
		exit 1
	}
done
image_authorization="$inspect_tmp/data/usr/lib/proxypool/v2-image-activation-authority"
if [ -e "$image_authorization" ] || [ -L "$image_authorization" ]; then
	echo 'package payload must not grant full-image activation authority' >&2
	exit 1
fi

for forbidden_ppp_callback in etc/ppp/ip-up etc/ppp/ip-down etc/ppp/ip-up.d etc/ppp/ip-down.d; do
	callback_path="$inspect_tmp/data/$forbidden_ppp_callback"
	if [ -e "$callback_path" ] || [ -L "$callback_path" ]; then
		echo "package payload must not own PPP callback path: /$forbidden_ppp_callback" >&2
		exit 1
	fi
done

uci_defaults="$inspect_tmp/data/etc/uci-defaults"
if [ -e "$uci_defaults" ] || [ -L "$uci_defaults" ]; then
	[ -d "$uci_defaults" ] && [ ! -L "$uci_defaults" ] || {
		echo 'package payload must not own /etc/uci-defaults entries' >&2
		exit 1
	}
	if find "$uci_defaults" -mindepth 1 -print -quit | grep -q .; then
		echo 'package payload must not own /etc/uci-defaults entries' >&2
		exit 1
	fi
fi

keep="$inspect_tmp/data/lib/upgrade/keep.d/proxypool"
expected_keep="$inspect_tmp/expected-keep"
printf '/etc/config/proxypool_v2\n/etc/proxypool/migration-v1.json\n/etc/proxypool/backups/\n' >"$expected_keep"
[ -f "$keep" ] && cmp -s "$expected_keep" "$keep" || { echo 'missing or invalid sysupgrade keep list' >&2; exit 1; }
[ "$(stat -c '%a' "$keep")" = 644 ] || { echo 'unexpected mode for sysupgrade keep list' >&2; exit 1; }

require_mode() {
	relative=$1
	expected_mode=$2
	packaged_file="$inspect_tmp/data/$relative"
	[ -f "$packaged_file" ] && [ ! -L "$packaged_file" ] || {
		if [ "$expected_mode" = 755 ]; then
			echo "missing executable /$relative" >&2
		else
			echo "missing /$relative" >&2
		fi
		exit 1
	}
	if [ "$expected_mode" = 755 ] && [ ! -x "$packaged_file" ]; then
		echo "missing executable /$relative" >&2
		exit 1
	fi
	[ "$(stat -c '%a' "$packaged_file")" = "$expected_mode" ] || {
		echo "unexpected mode for /$relative" >&2
		exit 1
	}
}

for init_relative in etc/init.d/proxypool etc/init.d/proxypool-guard etc/init.d/proxypool-activate; do
	require_mode "$init_relative" 755
done

for executable_relative in \
	usr/lib/proxypool/guard-resync.sh \
	usr/lib/proxypool/legacy-gate.sh \
	usr/lib/proxypool/lan-isolation.sh \
	usr/lib/proxypool/lan-isolation-worker.sh \
	usr/lib/proxypool/proxypool-firewall-defaults \
	usr/lib/proxypool/proxypool-firewall-transaction \
	usr/lib/proxypool/proxypool-fw4-activate \
	usr/lib/proxypool/proxypool-fw4-check-staged \
	usr/lib/proxypool/proxypool-postinst \
	usr/lib/proxypool/proxypool-migrate.sh \
	usr/lib/proxypool/proxypool-backend-activate \
	usr/lib/proxypool/proxypool-safety-uci-default \
	usr/lib/proxypool/ubus-call-stdin.uc; do
	require_mode "$executable_relative" 755
done

require_mode etc/hotplug.d/net/99-proxypool-lan-isolation 755
require_mode etc/hotplug.d/iface/99-proxypool-lan-isolation 755
require_mode etc/hotplug.d/iface/98-proxypool-v2-event 755

for data_relative in \
	usr/lib/proxypool/proxypool-guard.nft \
	usr/lib/proxypool/proxypool-fw4-input-gate.nft \
	usr/lib/proxypool/proxypool-fw4-forward-gate.nft \
	usr/lib/proxypool/proxypool-uci-staged.uc \
	usr/lib/proxypool/proxypool-v2-default; do
	require_mode "$data_relative" 644
done

for relative in usr/sbin/proxypoold usr/bin/proxypoolctl usr/lib/proxypool/ip2region_searcher usr/bin/slp-client; do
	binary="$inspect_tmp/data/$relative"
	[ -f "$binary" ] && [ -x "$binary" ] || { echo "missing executable /$relative" >&2; exit 1; }
	[ "$(stat -c '%a' "$binary")" = 755 ] || { echo "unexpected mode for /$relative" >&2; exit 1; }
	description=$(file -b "$binary")
	printf '/%s: %s\n' "$relative" "$description"
	printf '%s\n' "$description" | grep -Eq 'ELF 64-bit LSB.*ARM aarch64' || { echo "unexpected target binary architecture for /$relative" >&2; exit 1; }
done

echo 'IPK contents: PASS'
