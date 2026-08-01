#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
INSPECTOR="$ROOT/scripts/inspect-ipk.sh"
TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT HUP INT TERM

mkdir "$TEST_TMP/bin"
printf '%s\n' \
	'#!/usr/bin/env sh' \
	"printf '%s\\n' 'ELF 64-bit LSB executable, ARM aarch64'" \
	>"$TEST_TMP/bin/file"
chmod 755 "$TEST_TMP/bin/file"
printf '%s\n' \
	'#!/usr/bin/env sh' \
	'target=' \
	'for argument in "$@"; do target=$argument; done' \
	'case "${PROXYPOOL_TEST_WRONG_MODE:-}:$target" in' \
	'  guard-resync:*/usr/lib/proxypool/guard-resync.sh) printf "644\\n"; exit 0 ;;' \
	'  lan-isolation:*/usr/lib/proxypool/lan-isolation.sh) printf "644\\n"; exit 0 ;;' \
	'  lan-worker:*/usr/lib/proxypool/lan-isolation-worker.sh) printf "644\\n"; exit 0 ;;' \
	'  lan-hotplug:*/etc/hotplug.d/net/99-proxypool-lan-isolation) printf "644\\n"; exit 0 ;;' \
	'  lan-iface-hotplug:*/etc/hotplug.d/iface/99-proxypool-lan-isolation) printf "644\\n"; exit 0 ;;' \
	'  legacy-gate:*/usr/lib/proxypool/legacy-gate.sh) printf "644\\n"; exit 0 ;;' \
	'esac' \
	'case "$target" in' \
	'  */etc/config/proxypool) printf "600\\n" ;;' \
	'  */lib/upgrade/keep.d/proxypool|*.nft|*.uc) printf "644\\n" ;;' \
	'  *) printf "755\\n" ;;' \
	'esac' \
	>"$TEST_TMP/bin/stat"
chmod 755 "$TEST_TMP/bin/stat"

write_postinst() {
	destination=$1
	printf '%s\n' \
		'#!/bin/sh' \
		'"${IPKG_INSTROOT}/usr/lib/proxypool/proxypool-postinst"' \
		>"$destination"
	chmod 755 "$destination"
}

make_ipk() {
	name=$1
	fixture_kind=$2
	fixture="$TEST_TMP/$name"
	outer="$fixture/outer"
	control="$fixture/control"
	data="$fixture/data"
	mkdir -p "$outer" "$control" \
		"$data/etc/config" "$data/etc/init.d" "$data/etc/hotplug.d/net" \
		"$data/etc/hotplug.d/iface" \
		"$data/usr/sbin" "$data/usr/bin" "$data/usr/lib/proxypool" \
		"$data/lib/upgrade/keep.d"
	depends='libc, firewall4, kmod-nft-bridge, uci, ucode, ubus, jshn, nftables, ip-bridge, coreutils-stat'
	case "$fixture_kind" in
		missing_firewall4_dep) depends='libc, kmod-nft-bridge, uci, ucode, ubus, jshn, nftables, ip-bridge, coreutils-stat' ;;
		missing_bridge_dep) depends='libc, firewall4, uci, ucode, ubus, jshn, nftables, ip-bridge, coreutils-stat' ;;
		missing_uci_dep) depends='libc, firewall4, kmod-nft-bridge, ucode, ubus, jshn, nftables, ip-bridge, coreutils-stat' ;;
		missing_ucode_dep) depends='libc, firewall4, kmod-nft-bridge, uci, ubus, jshn, nftables, ip-bridge, coreutils-stat' ;;
		missing_ubus_dep) depends='libc, firewall4, kmod-nft-bridge, uci, ucode, jshn, nftables, ip-bridge, coreutils-stat' ;;
		missing_jshn_dep) depends='libc, firewall4, kmod-nft-bridge, uci, ucode, ubus, nftables, ip-bridge, coreutils-stat' ;;
		missing_ip_bridge_dep) depends='libc, firewall4, kmod-nft-bridge, uci, ucode, ubus, jshn, nftables, coreutils-stat' ;;
		missing_coreutils_stat_dep) depends='libc, firewall4, kmod-nft-bridge, uci, ucode, ubus, jshn, nftables, ip-bridge' ;;
	esac
	printf '%s\n' \
		'Package: proxypool-core' \
		'Version: 2.0.0-2' \
		'Architecture: aarch64_cortex-a53' \
		"Depends: $depends" >"$control/control"
	case "$fixture_kind" in
		missing_conffiles) : ;;
		extra_conffiles) printf '/etc/config/proxypool\n/etc/config/unrelated\n' >"$control/conffiles" ;;
		crlf_conffiles) printf '/etc/config/proxypool\r\n' >"$control/conffiles" ;;
		no_newline_conffiles) printf '/etc/config/proxypool' >"$control/conffiles" ;;
		*) printf '/etc/config/proxypool\n' >"$control/conffiles" ;;
	esac
	case "$fixture_kind" in
		missing_postinst) : ;;
		bad_postinst)
			printf '%s\n' '#!/bin/sh' 'exit 0' >"$control/postinst-pkg"
			chmod 755 "$control/postinst-pkg"
			;;
		*) write_postinst "$control/postinst-pkg" ;;
	esac

	cp "$ROOT/proxypool-core/files/proxypool.config" "$data/etc/config/proxypool"
	printf '/etc/config/proxypool_v2\n/etc/config/proxypool_runtime\n/etc/proxypool/activated-backend\n/etc/proxypool/cleanup-required\n/etc/proxypool/firewall-transaction\n/etc/proxypool/wireless-quarantine\n' \
		>"$data/lib/upgrade/keep.d/proxypool"
	case "$fixture_kind" in
		payload_v2) printf "config global 'global'\n" >"$data/etc/config/proxypool_v2" ;;
		payload_state)
			mkdir -p "$data/etc/proxypool"
			printf 'v1\n' >"$data/etc/proxypool/activated-backend"
			;;
		payload_ppp_ip_up)
			mkdir -p "$data/etc/ppp"
			printf '%s\n' '#!/bin/sh' 'exit 0' >"$data/etc/ppp/ip-up"
			;;
		payload_ppp_ip_down)
			mkdir -p "$data/etc/ppp"
			printf '%s\n' '#!/bin/sh' 'exit 0' >"$data/etc/ppp/ip-down"
			;;
		payload_ppp_ip_up_d)
			mkdir -p "$data/etc/ppp/ip-up.d"
			printf '%s\n' '#!/bin/sh' 'exit 0' >"$data/etc/ppp/ip-up.d/99-proxypool"
			;;
		payload_ppp_ip_down_d)
			mkdir -p "$data/etc/ppp/ip-down.d"
			printf '%s\n' '#!/bin/sh' 'exit 0' >"$data/etc/ppp/ip-down.d/99-proxypool"
			;;
		payload_journal)
			mkdir -p "$data/etc/proxypool/firewall-transaction"
			printf 'prepared\n' >"$data/etc/proxypool/firewall-transaction/state"
			;;
		payload_wireless_quarantine)
			mkdir -p "$data/etc/proxypool/wireless-quarantine"
			printf 'DISABLED\n' >"$data/etc/proxypool/wireless-quarantine/state"
			;;
		payload_activation_marker)
			mkdir -p "$data/etc/proxypool"
			printf 'schema_version=1\n' >"$data/etc/proxypool/firewall-safety-activated"
			;;
		payload_uci_default)
			mkdir -p "$data/etc/uci-defaults"
			printf '%s\n' '#!/bin/sh' 'exit 0' >"$data/etc/uci-defaults/99-proxypool-safety"
			;;
		bad_keep)
			printf '/etc/config/proxypool_v2\n/etc/config/proxypool_runtime\n/etc/proxypool/activated-backend\n/etc/proxypool/cleanup-required\n' \
				>"$data/lib/upgrade/keep.d/proxypool"
			;;
	esac

	printf '%s\n' '#!/bin/sh' 'exit 0' >"$data/etc/init.d/proxypool"
	printf '%s\n' '#!/bin/sh' 'exit 0' >"$data/etc/init.d/proxypool-guard"
	for binary in \
		"$data/usr/sbin/proxypoold" \
		"$data/usr/bin/proxypoolctl" \
		"$data/usr/lib/proxypool/ip2region_searcher" \
		"$data/usr/bin/slp-client"; do
		printf '%s\n' '#!/bin/sh' 'exit 0' >"$binary"
		chmod 755 "$binary"
	done
	for helper in \
		guard-resync.sh \
		legacy-gate.sh \
		lan-isolation.sh \
		lan-isolation-worker.sh \
		proxypool-firewall-defaults \
		proxypool-firewall-transaction \
		proxypool-fw4-activate \
		proxypool-fw4-check-staged \
		proxypool-postinst \
		proxypool-safety-uci-default; do
		printf '%s\n' '#!/bin/sh' 'exit 0' >"$data/usr/lib/proxypool/$helper"
		chmod 755 "$data/usr/lib/proxypool/$helper"
	done
	printf '%s\n' '#!/bin/sh' 'exit 0' >"$data/etc/hotplug.d/net/99-proxypool-lan-isolation"
	chmod 755 "$data/etc/hotplug.d/net/99-proxypool-lan-isolation"
	cp "$data/etc/hotplug.d/net/99-proxypool-lan-isolation" \
		"$data/etc/hotplug.d/iface/99-proxypool-lan-isolation"
	for ruleset in \
		proxypool-guard.nft \
		proxypool-fw4-input-gate.nft \
		proxypool-fw4-forward-gate.nft; do
		printf 'table inet test {}\n' >"$data/usr/lib/proxypool/$ruleset"
		chmod 644 "$data/usr/lib/proxypool/$ruleset"
	done
	printf 'exit(0);\n' >"$data/usr/lib/proxypool/proxypool-uci-staged.uc"
	chmod 644 "$data/usr/lib/proxypool/proxypool-uci-staged.uc"
	case "$fixture_kind" in
		missing_guard) rm -f "$data/etc/init.d/proxypool-guard" ;;
		missing_legacy_gate) rm -f "$data/usr/lib/proxypool/legacy-gate.sh" ;;
		missing_template) rm -f "$data/usr/lib/proxypool/proxypool-safety-uci-default" ;;
		missing_lan_isolation) rm -f "$data/usr/lib/proxypool/lan-isolation.sh" ;;
		missing_lan_worker) rm -f "$data/usr/lib/proxypool/lan-isolation-worker.sh" ;;
		missing_lan_hotplug) rm -f "$data/etc/hotplug.d/net/99-proxypool-lan-isolation" ;;
		missing_lan_iface_hotplug) rm -f "$data/etc/hotplug.d/iface/99-proxypool-lan-isolation" ;;
	esac

	chmod 600 "$data/etc/config/proxypool"
	chmod 755 "$data/etc/init.d/proxypool"
	[ ! -e "$data/etc/init.d/proxypool-guard" ] || chmod 755 "$data/etc/init.d/proxypool-guard"
	printf '2.0\n' >"$outer/debian-binary"
	tar -czf "$outer/control.tar.gz" -C "$control" .
	tar -czf "$outer/data.tar.gz" -C "$data" .
	tar -czf "$TEST_TMP/$name.ipk" -C "$outer" .
}

run_inspector() {
	name=$1
	wrong_mode=${2:-}
	PROXYPOOL_TEST_WRONG_MODE="$wrong_mode" PATH="$TEST_TMP/bin:$PATH" \
		sh "$INSPECTOR" "$TEST_TMP/$name.ipk" aarch64_cortex-a53 \
		>"$TEST_TMP/$name.log" 2>&1
}

make_ipk valid valid
if ! run_inspector valid; then
	cat "$TEST_TMP/valid.log" >&2
	exit 1
fi
grep -Fq 'IPK contents: PASS' "$TEST_TMP/valid.log"

for invalid_kind in \
	missing_conffiles extra_conffiles crlf_conffiles no_newline_conffiles \
	payload_v2 payload_state \
	payload_ppp_ip_up payload_ppp_ip_down payload_ppp_ip_up_d payload_ppp_ip_down_d \
	payload_journal payload_wireless_quarantine payload_activation_marker payload_uci_default bad_keep \
	missing_firewall4_dep missing_bridge_dep missing_uci_dep missing_ucode_dep missing_ubus_dep missing_jshn_dep \
	missing_ip_bridge_dep missing_coreutils_stat_dep \
	missing_postinst bad_postinst missing_guard missing_template \
	missing_legacy_gate missing_lan_isolation missing_lan_worker missing_lan_hotplug missing_lan_iface_hotplug; do
	make_ipk "$invalid_kind" "$invalid_kind"
	if run_inspector "$invalid_kind"; then
		printf 'invalid IPK fixture passed: %s\n' "$invalid_kind" >&2
		exit 1
	fi
done

make_ipk wrong_mode valid
if run_inspector wrong_mode guard-resync; then
	echo 'wrong safety-helper mode passed inspection' >&2
	exit 1
fi
make_ipk wrong_lan_mode valid
if run_inspector wrong_lan_mode lan-isolation; then
	echo 'wrong LAN isolation helper mode passed inspection' >&2
	exit 1
fi
make_ipk wrong_lan_worker_mode valid
if run_inspector wrong_lan_worker_mode lan-worker; then
	echo 'wrong LAN isolation worker mode passed inspection' >&2
	exit 1
fi
make_ipk wrong_legacy_gate_mode valid
if run_inspector wrong_legacy_gate_mode legacy-gate; then
	echo 'wrong legacy gate mode passed inspection' >&2
	exit 1
fi
make_ipk wrong_hotplug_mode valid
if run_inspector wrong_hotplug_mode lan-hotplug; then
	echo 'wrong LAN isolation hotplug mode passed inspection' >&2
	exit 1
fi
make_ipk wrong_iface_hotplug_mode valid
if run_inspector wrong_iface_hotplug_mode lan-iface-hotplug; then
	echo 'wrong LAN isolation iface hotplug mode passed inspection' >&2
	exit 1
fi

grep -Fq 'package payload unexpectedly owns /etc/config/proxypool_v2' "$TEST_TMP/payload_v2.log"
grep -Fq 'package payload unexpectedly owns /etc/proxypool/activated-backend' "$TEST_TMP/payload_state.log"
grep -Fq 'package payload must not own PPP callback path: /etc/ppp/ip-up' "$TEST_TMP/payload_ppp_ip_up.log"
grep -Fq 'package payload must not own PPP callback path: /etc/ppp/ip-down' "$TEST_TMP/payload_ppp_ip_down.log"
grep -Fq 'package payload must not own PPP callback path: /etc/ppp/ip-up.d' "$TEST_TMP/payload_ppp_ip_up_d.log"
grep -Fq 'package payload must not own PPP callback path: /etc/ppp/ip-down.d' "$TEST_TMP/payload_ppp_ip_down_d.log"
grep -Fq 'package payload unexpectedly owns /etc/proxypool/firewall-transaction' "$TEST_TMP/payload_journal.log"
grep -Fq 'package payload unexpectedly owns /etc/proxypool/wireless-quarantine' "$TEST_TMP/payload_wireless_quarantine.log"
grep -Fq 'package payload unexpectedly owns /etc/proxypool/firewall-safety-activated' "$TEST_TMP/payload_activation_marker.log"
grep -Fq 'package payload must not own /etc/uci-defaults entries' "$TEST_TMP/payload_uci_default.log"
grep -Fq 'missing or invalid sysupgrade keep list' "$TEST_TMP/bad_keep.log"
grep -Fq 'missing required dependency: firewall4' "$TEST_TMP/missing_firewall4_dep.log"
grep -Fq 'missing required dependency: kmod-nft-bridge' "$TEST_TMP/missing_bridge_dep.log"
grep -Fq 'missing required dependency: uci' "$TEST_TMP/missing_uci_dep.log"
grep -Fq 'missing required dependency: ucode' "$TEST_TMP/missing_ucode_dep.log"
grep -Fq 'missing required dependency: ubus' "$TEST_TMP/missing_ubus_dep.log"
grep -Fq 'missing required dependency: jshn' "$TEST_TMP/missing_jshn_dep.log"
grep -Fq 'missing required dependency: ip-bridge' "$TEST_TMP/missing_ip_bridge_dep.log"
grep -Fq 'missing required dependency: coreutils-stat' "$TEST_TMP/missing_coreutils_stat_dep.log"
grep -Fq 'missing executable control/postinst-pkg' "$TEST_TMP/missing_postinst.log"
grep -Fq 'invalid control/postinst-pkg' "$TEST_TMP/bad_postinst.log"
grep -Fq 'missing executable /etc/init.d/proxypool-guard' "$TEST_TMP/missing_guard.log"
grep -Fq 'missing executable /usr/lib/proxypool/legacy-gate.sh' "$TEST_TMP/missing_legacy_gate.log"
grep -Fq 'missing executable /usr/lib/proxypool/proxypool-safety-uci-default' "$TEST_TMP/missing_template.log"
grep -Fq 'unexpected mode for /usr/lib/proxypool/guard-resync.sh' "$TEST_TMP/wrong_mode.log"
grep -Fq 'missing executable /usr/lib/proxypool/lan-isolation.sh' "$TEST_TMP/missing_lan_isolation.log"
grep -Fq 'missing executable /usr/lib/proxypool/lan-isolation-worker.sh' "$TEST_TMP/missing_lan_worker.log"
grep -Fq 'missing executable /etc/hotplug.d/net/99-proxypool-lan-isolation' "$TEST_TMP/missing_lan_hotplug.log"
grep -Fq 'missing executable /etc/hotplug.d/iface/99-proxypool-lan-isolation' "$TEST_TMP/missing_lan_iface_hotplug.log"
grep -Fq 'unexpected mode for /usr/lib/proxypool/lan-isolation.sh' "$TEST_TMP/wrong_lan_mode.log"
grep -Fq 'unexpected mode for /usr/lib/proxypool/lan-isolation-worker.sh' "$TEST_TMP/wrong_lan_worker_mode.log"
grep -Fq 'unexpected mode for /usr/lib/proxypool/legacy-gate.sh' "$TEST_TMP/wrong_legacy_gate_mode.log"
grep -Fq 'unexpected mode for /etc/hotplug.d/net/99-proxypool-lan-isolation' "$TEST_TMP/wrong_hotplug_mode.log"
grep -Fq 'unexpected mode for /etc/hotplug.d/iface/99-proxypool-lan-isolation' "$TEST_TMP/wrong_iface_hotplug_mode.log"

echo 'IPK safety integration inspection: PASS'
