#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
DEFAULTS="$ROOT/proxypool-core/files/proxypool-firewall-defaults"
TRANSACTION_HELPER_SOURCE="$ROOT/proxypool-core/files/proxypool-firewall-transaction"
STAGED_UC="$ROOT/proxypool-core/files/proxypool-uci-staged.uc"
TEST_TMP=$(mktemp -d)
cleanup_test_tmp() {
	if [ "${PROXYPOOL_TEST_KEEP_TMP:-0}" = 1 ]; then
		printf 'firewall defaults contract: preserved test workspace %s\n' "$TEST_TMP" >&2
	else
		rm -rf "$TEST_TMP"
	fi
}
trap cleanup_test_tmp EXIT HUP INT TERM

fail() {
	printf 'firewall defaults contract: %s\n' "$*" >&2
	exit 1
}

[ -f "$DEFAULTS" ] || fail "missing production file: $DEFAULTS"
[ -f "$TRANSACTION_HELPER_SOURCE" ] || fail "missing production file: $TRANSACTION_HELPER_SOURCE"
[ -f "$STAGED_UC" ] || fail "missing production file: $STAGED_UC"
if grep -Fq 'recreate_named("firewall", "proxypool_allow_dns"' "$STAGED_UC"; then
	fail 'phase-1 staged UCI publishes router DNS without an owned DNS data plane'
fi
guard_source_line=$(grep -n -F '"$GUARD_RESET" reset-empty' "$DEFAULTS" | sed -n '1s/:.*//p')
invalidate_source_line=$(grep -n -F '"$TRANSACTION_HELPER" invalidate-activation-locked' "$DEFAULTS" | sed -n '1s/:.*//p')
clamp_source_line=$(grep -n -F 'run_uci_action clamp-offload' "$DEFAULTS" | sed -n '1s/:.*//p')
[ -n "$guard_source_line" ] && [ -n "$invalidate_source_line" ] && [ -n "$clamp_source_line" ] ||
	fail 'cannot locate guardian/invalidation/clamp conversion steps'
[ "$guard_source_line" -lt "$invalidate_source_line" ] && [ "$invalidate_source_line" -lt "$clamp_source_line" ] ||
	fail 'first conversion mutation must be empty guardian, then marker invalidation, then one-way clamp'

BIN="$TEST_TMP/bin"
mkdir -p "$BIN"
TRANSACTION_HELPER="$BIN/proxypool-firewall-transaction"
cp "$TRANSACTION_HELPER_SOURCE" "$TRANSACTION_HELPER"
chmod 755 "$TRANSACTION_HELPER"

# Execute the real staged UCI source through a small Node-hosted compatibility
# layer.  The compatibility layer models the pinned ucode cursor API at the
# package boundary (absolute load, foreach/get_all, in-memory mutation and
# commit) while leaving all policy and validation decisions to the production
# ucode source itself.  Target images still run the same source with libuci.
cat >"$BIN/run-staged-ucode.js" <<'NODE_STAGED_UCODE'
const fs = require('fs');
const path = require('path');
const vm = require('vm');

class ScriptExit {
	constructor(code) { this.code = Number(code ?? 0); }
}

const sourcePath = process.argv[2];
const action = process.argv[3];
const hostStage = process.argv[4];
const hostDelta = process.argv[5];
const tracePath = process.env.PROXYPOOL_UCODE_TEST_TRACE;

function scriptPath(hostPath) {
	const normalized = String(hostPath).replace(/\\/g, '/');
	return /^[A-Za-z]:\//.test(normalized) ? `/${normalized}` : normalized;
}

function nativePath(value) {
	const normalized = String(value).replace(/\\/g, '/');
	return /^\/[A-Za-z]:\//.test(normalized) ? normalized.slice(1) : normalized;
}

function trace(line) {
	if (tracePath)
		fs.appendFileSync(tracePath, `${line}\n`);
}

if (process.env.PROXYPOOL_UCODE_TEST_AMBIENT_DELTA)
	trace(`ambient-armed:${process.env.PROXYPOOL_UCODE_TEST_AMBIENT_DELTA}`);

function clone(value) {
	return value == null ? value : JSON.parse(JSON.stringify(value));
}

function normalizePackage(raw) {
	const normalized = {};
	let index = 0;

	for (const [name, section] of Object.entries(raw)) {
		const value = clone(section);
		if (value['.name'] == null)
			value['.name'] = name;
		if (value['.anonymous'] == null)
			value['.anonymous'] = false;
		if (value['.index'] == null)
			value['.index'] = index;
		normalized[name] = value;
		index++;
	}

	return normalized;
}

class Cursor {
	constructor(confdir, savedir, searchdir) {
		this.confdir = confdir;
		this.savedir = savedir;
		this.searchdir = searchdir;
		this.packages = {};
		this.files = {};
		trace(`cursor:${confdir}:${savedir}:${searchdir}`);
	}

	load(specifier) {
		trace(`load:${specifier}`);
		if (!String(specifier).startsWith('/')) {
			if (process.env.PROXYPOOL_UCODE_TEST_AMBIENT_DELTA) {
				trace(`ambient-read:${process.env.PROXYPOOL_UCODE_TEST_AMBIENT_DELTA}`);
				fs.readFileSync(process.env.PROXYPOOL_UCODE_TEST_AMBIENT_DELTA, 'utf8');
			}
			throw new Error(`test cursor rejected non-absolute load: ${specifier}`);
		}
		const filename = nativePath(specifier);
		const packageName = path.basename(filename);
		this.packages[packageName] = normalizePackage(JSON.parse(fs.readFileSync(filename, 'utf8')));
		this.files[packageName] = filename;
		return true;
	}

	get_all(packageName, sectionName) {
		const packageValue = this.packages[packageName];
		if (!packageValue)
			return null;
		return clone(sectionName == null ? packageValue : packageValue[sectionName] ?? null);
	}

	foreach(packageName, sectionType, callback) {
		const packageValue = this.packages[packageName];
		if (!packageValue)
			return false;
		for (const section of Object.values(packageValue)) {
			if (sectionType != null && section['.type'] !== sectionType)
				continue;
			callback(clone(section));
		}
		return true;
	}

	set(packageName, sectionName, option, value) {
		const packageValue = this.packages[packageName];
		if (!packageValue)
			return false;
		if (arguments.length === 3) {
			if (!packageValue[sectionName]) {
				packageValue[sectionName] = {
					'.name': sectionName,
					'.anonymous': false,
					'.index': Object.keys(packageValue).length
				};
			}
			packageValue[sectionName]['.type'] = option;
			return true;
		}
		if (!packageValue[sectionName])
			return false;
		packageValue[sectionName][option] = clone(value);
		return true;
	}

	delete(packageName, sectionName, option) {
		const packageValue = this.packages[packageName];
		if (!packageValue || !packageValue[sectionName])
			return false;
		if (option == null)
			delete packageValue[sectionName];
		else
			delete packageValue[sectionName][option];
		return true;
	}

	commit(packageName) {
		if (!this.packages[packageName] || !this.files[packageName])
			return false;
		fs.writeFileSync(this.files[packageName], `${JSON.stringify(this.packages[packageName], null, 2)}\n`);
		trace(`commit:${packageName}`);
		return true;
	}
}

globalThis.__uci = { cursor: (...args) => new Cursor(...args) };
globalThis.__uc_iter = value => Array.isArray(value) ? value : Object.keys(value ?? {});
globalThis.__uc_entries = value => Object.entries(value ?? {});
globalThis.ARGV = [ action, scriptPath(hostStage), scriptPath(hostDelta) ];
globalThis.length = value => {
	if (value == null) return 0;
	if (typeof value === 'string' || Array.isArray(value)) return value.length;
	return Object.keys(value).length;
};
globalThis.type = value => Array.isArray(value) ? 'array' :
	(value === null ? 'null' : typeof value === 'object' ? 'object' : typeof value);
globalThis.substr = (value, start, count) => String(value).substr(start, count);
globalThis.split = (value, separator) => String(value).split(separator);
globalThis.join = (separator, values) => values.join(separator);
globalThis.trim = value => String(value).trim();
globalThis.lc = value => String(value).toLowerCase();
globalThis.push = (values, value) => values.push(value);
globalThis.keys = value => Object.keys(value);
globalThis.match = (value, expression) => String(value).match(expression);
globalThis.sprintf = (format, value) => {
	const padded = String(Number(value));
	return String(format).replace(/%0(\d+)d/, (_, width) => padded.padStart(Number(width), '0'));
};
globalThis.print = (...values) => process.stdout.write(values.map(String).join(''));
globalThis.warn = (...values) => process.stderr.write(values.map(String).join(''));
globalThis.exit = code => { throw new ScriptExit(code); };

let source = fs.readFileSync(sourcePath, 'utf8').replace(/^#![^\n]*\n/, '');
source = source.replace('const uci = require("uci");', 'const uci = globalThis.__uci;');
source = source.replace(
	/for\s*\(let\s+([A-Za-z_][A-Za-z0-9_]*)\s*,\s*([A-Za-z_][A-Za-z0-9_]*)\s+in\s+([^)]+)\)/g,
	'for (const [$1, $2] of __uc_entries($3))'
);
source = source.replace(
	/for\s*\(let\s+([A-Za-z_][A-Za-z0-9_]*)\s+in\s+([^)]+)\)/g,
	'for (const $1 of __uc_iter($2))'
);

try {
	vm.runInThisContext(source, { filename: sourcePath });
} catch (error) {
	if (error instanceof ScriptExit)
		process.exitCode = error.code;
	else {
		process.stderr.write(`${error.stack || error}\n`);
		process.exitCode = 2;
	}
}
NODE_STAGED_UCODE

run_real_staged_ucode() {
	action=$1
	stage=$2
	delta=$3
	trace=$4
	stdout_file=$5
	: >"$trace"
	env PROXYPOOL_UCODE_TEST_TRACE="$trace" \
		node "$BIN/run-staged-ucode.js" "$STAGED_UC" "$action" "$stage" "$delta" \
		>"$stdout_file"
}

json_value() {
	file=$1
	section=$2
	option=$3
	node -e '
		const fs = require("fs");
		const value = JSON.parse(fs.readFileSync(process.argv[1], "utf8"))[process.argv[2]]?.[process.argv[3]];
		if (value === undefined) process.exit(1);
		process.stdout.write(Array.isArray(value) ? JSON.stringify(value) : String(value));
	' "$file" "$section" "$option"
}

assert_json_value() {
	file=$1
	section=$2
	option=$3
	expected=$4
	actual=$(json_value "$file" "$section" "$option" 2>/dev/null || true)
	[ "$actual" = "$expected" ] ||
		fail "$(basename "$file").$section.$option: expected $expected, got ${actual:-<missing>}"
}

assert_json_missing() {
	file=$1
	section=$2
	option=$3
	if json_value "$file" "$section" "$option" >/dev/null 2>&1; then
		fail "$(basename "$file").$section.$option must remain absent"
	fi
}

write_staged_ucode_defaults_fixture() {
	directory=$1
	ports_json=$2
	mkdir -p "$directory"
	cat >"$directory/firewall" <<'JSON_FIREWALL'
{
  "defaults_main": { ".type": "defaults", "flow_offloading": "1", "flow_offloading_hw": "1", "auto_includes": "1" },
  "lan_zone": { ".type": "zone", "name": "lan", "input": "ACCEPT", "output": "ACCEPT", "forward": "ACCEPT" },
  "guest_zone": { ".type": "zone", "name": "guest", "input": "REJECT" }
}
JSON_FIREWALL
	cat >"$directory/dhcp" <<'JSON_DHCP'
{
  "dnsmasq_main": { ".type": "dnsmasq", "noresolv": "0", "port": "53", "server": "/tmp/resolv.conf" },
  "lan_dhcp": { ".type": "dhcp", "interface": "lan", "ra": "server", "dhcpv6": "server", "ndp": "relay", "dhcp_option": ["3,192.168.9.254", "6,8.8.8.8"] }
}
JSON_DHCP
	cat >"$directory/network" <<JSON_NETWORK
{
  "lan": { ".type": "interface", "device": "br-lan", "proto": "static", "ipaddr": "192.168.9.1", "delegate": "1" },
  "br_lan": { ".type": "device", "name": "br-lan", "type": "bridge", "ports": $ports_json },
  "existing_lan1": { ".type": "device", "name": "lan1", "isolate": "0", "mtu": "1500" },
  "foreign_uplink": { ".type": "device", "name": "eth0.2", "mtu": "1492" }
}
JSON_NETWORK
}

snapshot_staged_packages() {
	stage=$1
	snapshot=$2
	mkdir "$snapshot"
	cp "$stage/firewall" "$stage/dhcp" "$stage/network" "$snapshot/"
}

assert_staged_packages_unchanged() {
	stage=$1
	snapshot=$2
	for package in firewall dhcp network; do
		cmp -s "$snapshot/$package" "$stage/$package" ||
			fail "failed staged UCI validation changed $package bytes"
	done
}

assert_defaults_validation_fails_unchanged() {
	stage=$1
	label=$2
	action=${3:-apply-defaults}
	snapshot="$stage.before"
	delta="$stage.delta"
	trace="$stage.trace"
	stdout_file="$stage.stdout"
	mkdir "$delta"
	snapshot_staged_packages "$stage" "$snapshot"
	if run_real_staged_ucode "$action" "$stage" "$delta" "$trace" "$stdout_file" \
		>"$stage.log" 2>&1; then
		fail "staged UCI $action accepted $label"
	else
		rc=$?
	fi
	[ "$rc" -eq 1 ] || fail "$label failed with unexpected status $rc"
	assert_staged_packages_unchanged "$stage" "$snapshot"
	[ ! -s "$stdout_file" ] || fail "failed $label validation printed a success result"
	[ "$(grep -c '^commit:' "$trace" 2>/dev/null || true)" -eq 0 ] ||
		fail "failed $label validation committed a staged package"
}

write_staged_topology_variant() {
	directory=$1
	variant=$2
	write_staged_ucode_defaults_fixture "$directory" '["lan1", "lan2", "lan3", "lan4", "lan5"]'
	node -e '
		const fs = require("fs");
		const file = process.argv[1];
		const variant = process.argv[2];
		const value = JSON.parse(fs.readFileSync(file, "utf8"));
		switch (variant) {
		case "missing_lan5":
			value.br_lan.ports = ["lan1", "lan2", "lan3", "lan4"];
			break;
		case "vlan_filtering":
			value.br_lan.vlan_filtering = "1";
			break;
		case "bridge_vlan_device":
			value.guest_vlan = { ".type": "bridge-vlan", device: "br-lan", vlan: "30", ports: ["lan5:u*"] };
			break;
		case "bridge_vlan_port":
			value.usb_vlan = { ".type": "bridge-vlan", device: "br-usb", vlan: "30", ports: ["usb0:t", "lan5:t"] };
			break;
		case "other_bridge_port":
			value.br_guest = { ".type": "device", name: "br-guest", type: "bridge", ports: ["lan5"] };
			break;
		case "interface_physical":
			value.guest = { ".type": "interface", device: "lan5", proto: "static" };
			break;
		case "interface_port_upper":
			value.guest = { ".type": "interface", device: "lan5.30", proto: "static" };
			break;
		case "interface_bridge_upper":
			value.guest = { ".type": "interface", device: "br-lan.30", proto: "static" };
			break;
		case "interface_lan_alias_upper":
			value.guest = { ".type": "interface", device: "@lan.30", proto: "static" };
			break;
		case "interface_second_br_lan":
			value.guest = { ".type": "interface", device: "br-lan", proto: "static" };
			break;
		case "device_8021q_lower":
			value.guest_vlan = { ".type": "device", name: "guest.30", type: "8021q", ifname: "lan5", vid: "30" };
			break;
		case "device_8021ad_lower":
			value.guest_vlan = { ".type": "device", name: "guest.30", type: "8021ad", ifname: "br-lan", vid: "30" };
			break;
		case "device_macvlan_lower":
			value.guest_mac = { ".type": "device", name: "guest-mac", type: "macvlan", ifname: "br-lan" };
			break;
		case "physical_device_topology":
			value.existing_lan1.type = "8021q";
			value.existing_lan1.ifname = "eth1";
			value.existing_lan1.vid = "30";
			break;
		case "physical_device_type_only":
			value.existing_lan1.type = "bridge";
			break;
		case "lan_interface_renamed":
			value.mgmt = value.lan;
			delete value.lan;
			value.lan = { ".type": "interface", device: "br-dormant", proto: "none" };
			break;
		case "network_alias_chain":
			value.shadow = { ".type": "alias", interface: "lan" };
			value.guest = { ".type": "interface", device: "@shadow", proto: "none" };
			break;
		default:
			throw new Error(`unknown staged topology variant: ${variant}`);
		}
		fs.writeFileSync(file, `${JSON.stringify(value, null, 2)}\n`);
	' "$directory/network" "$variant"
}

assert_staged_topology_rejected() {
	variant=$1
	label=$2
	for action in apply-defaults clamp-offload; do
		stage="$TEST_TMP/ucode-topology-$variant-$action"
		write_staged_topology_variant "$stage" "$variant"
		assert_defaults_validation_fails_unchanged "$stage" "$label during $action" "$action"
	done
}

assert_wireless_validation_fails_unchanged() {
	stage=$1
	label=$2
	delta="$stage.delta"
	trace="$stage.trace"
	stdout_file="$stage.stdout"
	mkdir "$delta"
	cp "$stage/wireless" "$stage/wireless.before"
	if run_real_staged_ucode apply-wireless-isolation "$stage" "$delta" "$trace" "$stdout_file" \
		>"$stage.log" 2>&1; then
		fail "wireless isolation accepted $label"
	else
		rc=$?
	fi
	[ "$rc" -eq 1 ] || fail "$label failed with unexpected status $rc"
	cmp -s "$stage/wireless.before" "$stage/wireless" ||
		fail "failed $label changed staged wireless bytes"
	[ ! -s "$stdout_file" ] || fail "failed $label printed a success result"
	[ "$(grep -c '^commit:wireless$' "$trace" 2>/dev/null || true)" -eq 0 ] ||
		fail "failed $label committed wireless"
}

run_staged_ucode_contracts() {
	# Array-valued ports update a pre-existing real device section and create one
	# deterministic owned section for the missing port without recreating either
	# foreign section.
	stage="$TEST_TMP/ucode-defaults-array"
	delta="$TEST_TMP/ucode-defaults-array.delta"
	trace="$TEST_TMP/ucode-defaults-array.trace"
	stdout_file="$TEST_TMP/ucode-defaults-array.stdout"
	write_staged_ucode_defaults_fixture "$stage" '["lan1", "lan2", "lan3", "lan4", "lan5"]'
	mkdir "$delta"
	before_foreign=$(json_value "$stage/network" foreign_uplink mtu)
	run_real_staged_ucode apply-defaults "$stage" "$delta" "$trace" "$stdout_file" ||
		fail 'real staged UCI rejected array-valued br-lan ports'
	[ ! -s "$stdout_file" ] || fail 'apply-defaults wrote unexpected stdout'
	assert_json_value "$stage/dhcp" lan_dhcp dhcp_option '["6,192.168.9.1"]'
	assert_json_value "$stage/firewall" proxypool_allow_admin_ssh .type rule
	assert_json_value "$stage/firewall" proxypool_allow_admin_ssh name 'ProxyPool Allow SSH Management'
	assert_json_value "$stage/firewall" proxypool_allow_admin_ssh src lan
	assert_json_value "$stage/firewall" proxypool_allow_admin_ssh proto tcp
	assert_json_value "$stage/firewall" proxypool_allow_admin_ssh dest_ip 192.168.9.1
	assert_json_value "$stage/firewall" proxypool_allow_admin_ssh dest_port 22
	assert_json_value "$stage/firewall" proxypool_allow_admin_ssh family ipv4
	assert_json_value "$stage/firewall" proxypool_allow_admin_ssh target ACCEPT
	assert_json_value "$stage/network" existing_lan1 isolate 1
	assert_json_value "$stage/network" existing_lan1 mtu 1500
	assert_json_value "$stage/network" proxypool_lan_port_02 .type device
	assert_json_value "$stage/network" proxypool_lan_port_02 name lan2
	assert_json_value "$stage/network" proxypool_lan_port_02 isolate 1
	assert_json_value "$stage/network" proxypool_lan_port_03 name lan3
	assert_json_value "$stage/network" proxypool_lan_port_04 name lan4
	assert_json_value "$stage/network" proxypool_lan_port_05 name lan5
	assert_json_value "$stage/network" foreign_uplink mtu "$before_foreign"
	[ "$(grep -c '^commit:firewall$' "$trace")" -eq 1 ] &&
		[ "$(grep -c '^commit:dhcp$' "$trace")" -eq 1 ] &&
		[ "$(grep -c '^commit:network$' "$trace")" -eq 1 ] ||
		fail 'apply-defaults did not commit all staged packages exactly once'
	grep -Eq '^load:/.*/network$' "$trace" ||
		fail 'apply-defaults did not load network by absolute staged path'

	# A string-valued static port list is tokenized exactly and produces stable
	# sequence names.  A malicious default-delta fixture is neither read nor
	# committed because every package load is absolute.
	stage="$TEST_TMP/ucode-defaults-string"
	delta="$TEST_TMP/ucode-defaults-string.delta"
	trace="$TEST_TMP/ucode-defaults-string.trace"
	stdout_file="$TEST_TMP/ucode-defaults-string.stdout"
	ambient="$TEST_TMP/default-.uci-network"
	write_staged_ucode_defaults_fixture "$stage" '"lan5 lan3 lan1 lan4 lan2"'
	mkdir "$delta"
	printf '%s\n' '{"br_lan":{"ports":["attacker0"]}}' >"$ambient"
	ambient_before=$(sha256sum "$ambient" | awk '{print $1}')
	PROXYPOOL_UCODE_TEST_AMBIENT_DELTA="$ambient" \
		run_real_staged_ucode apply-defaults "$stage" "$delta" "$trace" "$stdout_file" ||
		fail 'real staged UCI rejected string-valued br-lan ports'
	assert_json_value "$stage/network" proxypool_lan_port_01 name lan5
	assert_json_value "$stage/network" proxypool_lan_port_02 name lan3
	assert_json_missing "$stage/network" proxypool_lan_port_03 name
	assert_json_value "$stage/network" proxypool_lan_port_04 name lan4
	assert_json_value "$stage/network" proxypool_lan_port_05 name lan2
	[ "$(sha256sum "$ambient" | awk '{print $1}')" = "$ambient_before" ] ||
		fail 'absolute staged loads touched the default-delta attack fixture'
	[ "$(grep -c '^ambient-armed:' "$trace" 2>/dev/null || true)" -eq 1 ] ||
		fail 'default-delta attack fixture was not armed'
	[ "$(grep -c '^ambient-read:' "$trace" 2>/dev/null || true)" -eq 0 ] ||
		fail 'absolute staged loads read the default-delta attack fixture'

	stage="$TEST_TMP/ucode-defaults-duplicate-port"
	write_staged_ucode_defaults_fixture "$stage" '["lan1", "lan2", "lan3", "lan4", "lan5", "lan5"]'
	assert_defaults_validation_fails_unchanged "$stage" 'duplicate br-lan port'

	stage="$TEST_TMP/ucode-defaults-duplicate-device"
	write_staged_ucode_defaults_fixture "$stage" '["lan1", "lan2", "lan3", "lan4", "lan5"]'
	node -e '
		const fs=require("fs"), f=process.argv[1], o=JSON.parse(fs.readFileSync(f));
		o.duplicate_lan1={".type":"device",name:"lan1"}; fs.writeFileSync(f, JSON.stringify(o,null,2)+"\n");
	' "$stage/network"
	assert_defaults_validation_fails_unchanged "$stage" 'duplicate config-device name for a static port'

	stage="$TEST_TMP/ucode-defaults-owned-collision"
	write_staged_ucode_defaults_fixture "$stage" '["lan1", "lan2", "lan3", "lan4", "lan5"]'
	node -e '
		const fs=require("fs"), f=process.argv[1], o=JSON.parse(fs.readFileSync(f));
		o.proxypool_lan_port_02={".type":"interface",proto:"none"}; fs.writeFileSync(f, JSON.stringify(o,null,2)+"\n");
	' "$stage/network"
	assert_defaults_validation_fails_unchanged "$stage" 'foreign owned-section name collision'

	stage="$TEST_TMP/ucode-defaults-malicious-port"
	write_staged_ucode_defaults_fixture "$stage" '["lan1", "lan2", "lan3", "lan4", "lan5;reboot"]'
	assert_defaults_validation_fails_unchanged "$stage" 'unsafe Linux interface name'

	# GL-MT6000 has exactly five physical LAN client ports.  Any omission,
	# reuse, VLAN upper, or protected lower-device reference creates an ingress
	# whose routed name is no longer br-lan and therefore bypasses the guardian.
	# Both conversion actions must reject the complete topology before their
	# first set()/commit, not merely during the later apply-defaults pass.
	assert_staged_topology_rejected missing_lan5 'incomplete GL-MT6000 LAN port set'
	assert_staged_topology_rejected vlan_filtering 'br-lan VLAN filtering'
	assert_staged_topology_rejected bridge_vlan_device 'bridge-vlan on br-lan'
	assert_staged_topology_rejected bridge_vlan_port 'bridge-vlan reusing a physical LAN port'
	assert_staged_topology_rejected other_bridge_port 'second bridge reusing a physical LAN port'
	assert_staged_topology_rejected interface_physical 'interface directly attached to a physical LAN port'
	assert_staged_topology_rejected interface_port_upper 'interface attached to a physical-port VLAN upper'
	assert_staged_topology_rejected interface_bridge_upper 'interface attached to a br-lan VLAN upper'
	assert_staged_topology_rejected interface_lan_alias_upper 'interface attached to an @lan VLAN upper'
	assert_staged_topology_rejected interface_second_br_lan 'second interface attached to br-lan'
	assert_staged_topology_rejected device_8021q_lower '8021q device on a physical LAN lower'
	assert_staged_topology_rejected device_8021ad_lower '8021ad device on the br-lan lower'
	assert_staged_topology_rejected device_macvlan_lower 'macvlan device on the br-lan lower'
	assert_staged_topology_rejected physical_device_topology 'topology options on a stock per-port device'
	assert_staged_topology_rejected physical_device_type_only 'device type on a stock per-port device'
	assert_staged_topology_rejected lan_interface_renamed 'br-lan management interface renamed away from lan'
	assert_staged_topology_rejected network_alias_chain 'network alias attachment chain'

	# Dormant logical policy and bridges wholly unrelated to the five physical
	# client ports are harmless and must not be confused with an attachment.
	stage="$TEST_TMP/ucode-topology-unrelated"
	delta="$TEST_TMP/ucode-topology-unrelated.delta"
	trace="$TEST_TMP/ucode-topology-unrelated.trace"
	stdout_file="$TEST_TMP/ucode-topology-unrelated.stdout"
	write_staged_ucode_defaults_fixture "$stage" '["lan1", "lan2", "lan3", "lan4", "lan5"]'
	node -e '
		const fs=require("fs"), f=process.argv[1], o=JSON.parse(fs.readFileSync(f));
		o.dormant_guest={".type":"interface",proto:"none"};
		o.br_usb={".type":"device",name:"br-usb",type:"bridge",ports:["usb0"]};
		o.usb_vlan={".type":"bridge-vlan",device:"br-usb",vlan:"30",ports:["usb0:u*"]};
		fs.writeFileSync(f, JSON.stringify(o,null,2)+"\n");
	' "$stage/network"
	mkdir "$delta"
	run_real_staged_ucode apply-defaults "$stage" "$delta" "$trace" "$stdout_file" ||
		fail 'staged topology validator rejected unrelated dormant/USB configuration'
	assert_json_value "$stage/network" dormant_guest proto none
	assert_json_value "$stage/network" br_usb ports '["usb0"]'
	assert_json_value "$stage/network" usb_vlan device br-usb

	# Wireless classification validates every wifi-iface before the first set.
	# Phase 1 admits only APs whose entire network token/list is exactly lan.
	stage="$TEST_TMP/ucode-wireless"
	delta="$TEST_TMP/ucode-wireless.delta"
	trace="$TEST_TMP/ucode-wireless.trace"
	stdout_file="$TEST_TMP/ucode-wireless.stdout"
	ambient="$TEST_TMP/default-.uci-wireless"
	mkdir "$stage" "$delta"
	cat >"$stage/wireless" <<'JSON_WIRELESS'
{
  "radio0": { ".type": "wifi-device", "type": "mac80211" },
  "ap_lan_string": { ".type": "wifi-iface", "mode": "ap", "network": "lan", "isolate": "0" },
  "cfg123abc": { ".type": "wifi-iface", ".anonymous": true, "mode": "ap", "network": ["lan"], "bridge_isolate": "0" },
  "sta_wwan": { ".type": "wifi-iface", "mode": "sta", "network": "wwan", "isolate": "0" }
}
JSON_WIRELESS
	chmod 600 "$stage/wireless" || fail 'cannot request restrictive wireless fixture mode'
	wireless_mode_before=$(stat -c '%a' "$stage/wireless")
	wireless_owner_before=$(stat -c '%u:%g' "$stage/wireless")
	printf '%s\n' '{"ap_lan_string":{"isolate":"attacker"}}' >"$ambient"
	ambient_before=$(sha256sum "$ambient" | awk '{print $1}')
	PROXYPOOL_UCODE_TEST_AMBIENT_DELTA="$ambient" \
		run_real_staged_ucode apply-wireless-isolation "$stage" "$delta" "$trace" "$stdout_file" ||
		fail 'real staged UCI rejected valid wireless isolation topology'
	[ "$(cat "$stdout_file")" = changed ] || fail 'wireless changed run did not print exactly changed'
	[ "$(wc -l <"$stdout_file" | tr -d '[:space:]')" -eq 1 ] || fail 'wireless changed stdout is not one line'
	for section in ap_lan_string cfg123abc; do
		assert_json_value "$stage/wireless" "$section" isolate 1
		assert_json_value "$stage/wireless" "$section" bridge_isolate 1
	done
	assert_json_value "$stage/wireless" sta_wwan isolate 0
	assert_json_missing "$stage/wireless" sta_wwan bridge_isolate
	[ "$(grep -c '^commit:wireless$' "$trace")" -eq 1 ] ||
		fail 'changed wireless isolation did not commit exactly once'
	[ "$(stat -c '%a' "$stage/wireless")" = "$wireless_mode_before" ] ||
		fail 'wireless isolation commit changed staged wireless mode'
	[ "$(stat -c '%u:%g' "$stage/wireless")" = "$wireless_owner_before" ] ||
		fail 'wireless isolation commit changed staged wireless owner'
	[ "$(grep -c '^ambient-armed:' "$trace" 2>/dev/null || true)" -eq 1 ] &&
		[ "$(grep -c '^ambient-read:' "$trace" 2>/dev/null || true)" -eq 0 ] ||
		fail 'wireless absolute load consulted the default-delta attack fixture'
	[ "$(sha256sum "$ambient" | awk '{print $1}')" = "$ambient_before" ] ||
		fail 'wireless action changed the default-delta attack fixture'
	grep -Eq '^load:/.*/wireless$' "$trace" ||
		fail 'wireless isolation did not load wireless by absolute staged path'

	: >"$trace"
	run_real_staged_ucode apply-wireless-isolation "$stage" "$delta" "$trace" "$stdout_file" ||
		fail 'converged wireless isolation run failed'
	[ "$(cat "$stdout_file")" = unchanged ] || fail 'converged wireless run did not print exactly unchanged'
	[ "$(wc -l <"$stdout_file" | tr -d '[:space:]')" -eq 1 ] ||
		fail 'wireless unchanged stdout is not one line'
	[ "$(grep -c '^commit:wireless$' "$trace" 2>/dev/null || true)" -eq 0 ] ||
		fail 'unchanged wireless isolation committed the package'

	# Phase 1 admits only an AP attached exclusively to the LAN network.  The
	# valid AP before the late guest section proves that rejecting the complete
	# topology happens before any earlier target is persisted.
	stage="$TEST_TMP/ucode-wireless-non-lan-ap"
	mkdir "$stage"
	cat >"$stage/wireless" <<'JSON_WIRELESS_NON_LAN_AP'
{
  "valid_first": { ".type": "wifi-iface", "mode": "ap", "network": "lan", "isolate": "0" },
  "guest_late": { ".type": "wifi-iface", "mode": "ap", "network": "guest", "isolate": "0" }
}
JSON_WIRELESS_NON_LAN_AP
	assert_wireless_validation_fails_unchanged "$stage" 'AP attached to a non-LAN network'

	stage="$TEST_TMP/ucode-wireless-mixed-string-ap"
	mkdir "$stage"
	cat >"$stage/wireless" <<'JSON_WIRELESS_MIXED_STRING_AP'
{
  "mixed_ap": { ".type": "wifi-iface", "mode": "ap", "network": "guest lan", "isolate": "0" }
}
JSON_WIRELESS_MIXED_STRING_AP
	assert_wireless_validation_fails_unchanged "$stage" 'AP attached to mixed string networks'

	stage="$TEST_TMP/ucode-wireless-mixed-list-ap"
	mkdir "$stage"
	cat >"$stage/wireless" <<'JSON_WIRELESS_MIXED_LIST_AP'
{
  "mixed_ap": { ".type": "wifi-iface", "mode": "ap", "network": ["lan", "iot"], "isolate": "0" }
}
JSON_WIRELESS_MIXED_LIST_AP
	assert_wireless_validation_fails_unchanged "$stage" 'AP attached to mixed list networks'

	# OpenWrt 23.05 netifd gives wifi-vlan its own network, isolate and
	# bridge_isolate state.  Phase 1 does not model that hidden bridge surface,
	# so even an apparently LAN-only wifi-vlan must be rejected explicitly.
	stage="$TEST_TMP/ucode-wireless-vlan"
	mkdir "$stage"
	cat >"$stage/wireless" <<'JSON_WIRELESS_VLAN'
{
  "parent_lan": { ".type": "wifi-iface", "mode": "ap", "network": "lan", "isolate": "1", "bridge_isolate": "1" },
  "vlan_lan": { ".type": "wifi-vlan", "iface": "parent_lan", "network": "lan" }
}
JSON_WIRELESS_VLAN
	assert_wireless_validation_fails_unchanged "$stage" 'unsupported wifi-vlan section'

	# OpenWrt 23.05 also passes wifi-station sections into the hostapd driver as
	# per-client policy.  Phase 1 does not model that admission/VLAN surface.
	stage="$TEST_TMP/ucode-wireless-station"
	mkdir "$stage"
	cat >"$stage/wireless" <<'JSON_WIRELESS_STATION'
{
  "parent_lan": { ".type": "wifi-iface", "mode": "ap", "network": "lan", "isolate": "1", "bridge_isolate": "1" },
  "station_override": { ".type": "wifi-station", "iface": "parent_lan", "mac": "00:11:22:33:44:55" }
}
JSON_WIRELESS_STATION
	assert_wireless_validation_fails_unchanged "$stage" 'unsupported wifi-station section'

	for wireless_case in mesh adhoc monitor p2p unknown_mode sta_lan wds multi_ap network_vlan dynamic_vlan dynamic_vlan_required vlan_file vlan_bridge vlan_tagged_interface; do
		stage="$TEST_TMP/ucode-wireless-$wireless_case"
		mkdir "$stage"
		case "$wireless_case" in
			mesh|adhoc|monitor|p2p|unknown_mode)
				case "$wireless_case" in
					unknown_mode) fixture_mode='future-unsafe-mode' ;;
					*) fixture_mode=$wireless_case ;;
				esac
				cat >"$stage/wireless" <<JSON_WIRELESS_MODE
{
  "unsafe_mode": { ".type": "wifi-iface", "mode": "$fixture_mode", "network": "lan" }
}
JSON_WIRELESS_MODE
				;;
			sta_lan)
				cat >"$stage/wireless" <<'JSON_WIRELESS_STA_LAN'
{
  "unsafe_sta": { ".type": "wifi-iface", "mode": "sta", "network": "lan" }
}
JSON_WIRELESS_STA_LAN
				;;
			wds)
				cat >"$stage/wireless" <<'JSON_WIRELESS_WDS'
{
  "unsafe_wds": { ".type": "wifi-iface", "mode": "ap", "network": "lan", "wds": "1" }
}
JSON_WIRELESS_WDS
				;;
			multi_ap)
				cat >"$stage/wireless" <<'JSON_WIRELESS_MULTI_AP'
{
  "unsafe_multi_ap": { ".type": "wifi-iface", "mode": "sta", "network": "wwan", "multi_ap": "1" }
}
JSON_WIRELESS_MULTI_AP
				;;
			network_vlan)
				cat >"$stage/wireless" <<'JSON_WIRELESS_NETWORK_VLAN'
{
  "unsafe_network_vlan": { ".type": "wifi-iface", "mode": "ap", "network": "lan", "network_vlan": ["30:u*"] }
}
JSON_WIRELESS_NETWORK_VLAN
				;;
			dynamic_vlan)
				cat >"$stage/wireless" <<'JSON_WIRELESS_DYNAMIC_VLAN'
{
  "unsafe_vlan": { ".type": "wifi-iface", "mode": "ap", "network": "lan", "dynamic_vlan": "1" }
}
JSON_WIRELESS_DYNAMIC_VLAN
				;;
			dynamic_vlan_required)
				cat >"$stage/wireless" <<'JSON_WIRELESS_DYNAMIC_VLAN_REQUIRED'
{
  "unsafe_vlan": { ".type": "wifi-iface", "mode": "ap", "network": "lan", "dynamic_vlan": "2" }
}
JSON_WIRELESS_DYNAMIC_VLAN_REQUIRED
				;;
			vlan_file|vlan_bridge|vlan_tagged_interface)
				WIRELESS_OPTION="$wireless_case" node -e '
					const fs=require("fs"), option=process.env.WIRELESS_OPTION;
					const value={ unsafe_vlan: { ".type":"wifi-iface", mode:"ap", network:"lan" } };
					value.unsafe_vlan[option]="configured";
					fs.writeFileSync(process.argv[1], JSON.stringify(value,null,2)+"\n");
				' "$stage/wireless"
				;;
		esac
		assert_wireless_validation_fails_unchanged "$stage" "enabled unsafe wireless surface: $wireless_case"
	done

	# Explicitly disabled non-terminal sections are dormant configuration, while
	# a normal STA used only as the wwan uplink remains supported.
	stage="$TEST_TMP/ucode-wireless-dormant"
	delta="$TEST_TMP/ucode-wireless-dormant.delta"
	trace="$TEST_TMP/ucode-wireless-dormant.trace"
	stdout_file="$TEST_TMP/ucode-wireless-dormant.stdout"
	mkdir "$stage" "$delta"
	cat >"$stage/wireless" <<'JSON_WIRELESS_DORMANT'
{
  "dormant_mesh": { ".type": "wifi-iface", "mode": "mesh", "network": "guest", "disabled": "1" },
  "sta_wwan": { ".type": "wifi-iface", "mode": "sta", "network": "wwan" },
  "ap_lan": { ".type": "wifi-iface", "mode": "ap", "network": "lan", "isolate": "1", "bridge_isolate": "1" }
}
JSON_WIRELESS_DORMANT
	run_real_staged_ucode apply-wireless-isolation "$stage" "$delta" "$trace" "$stdout_file" ||
		fail 'wireless validator rejected disabled mesh or STA-on-wwan'
	[ "$(cat "$stdout_file")" = unchanged ] ||
		fail 'dormant/wwan wireless topology unexpectedly changed configuration'

	# Cold-start validation failures use a separate emergency action.  It must
	# preserve credentials and topology verbatim while explicitly disabling
	# every OpenWrt 23.05 wireless section that can participate in an interface.
	stage="$TEST_TMP/ucode-wireless-quarantine"
	delta="$TEST_TMP/ucode-wireless-quarantine.delta"
	trace="$TEST_TMP/ucode-wireless-quarantine.trace"
	stdout_file="$TEST_TMP/ucode-wireless-quarantine.stdout"
	mkdir "$stage" "$delta"
	cat >"$stage/wireless" <<'JSON_WIRELESS_QUARANTINE'
{
  "radio0": { ".type": "wifi-device", "type": "mac80211", "disabled": "0" },
  "unsafe_guest": { ".type": "wifi-iface", "mode": "mesh", "network": "guest", "ssid": "keep-ssid", "key": "keep-secret" },
  "dynamic_vlan": { ".type": "wifi-vlan", "iface": "unsafe_guest", "network": "guest" },
  "dynamic_station": { ".type": "wifi-station", "iface": "unsafe_guest", "key": "keep-station-secret" }
}
JSON_WIRELESS_QUARANTINE
	run_real_staged_ucode disable-all-wireless "$stage" "$delta" "$trace" "$stdout_file" ||
		fail 'emergency wireless quarantine rejected a parseable unsafe topology'
	[ "$(cat "$stdout_file")" = changed ] ||
		fail 'emergency wireless quarantine did not report changed'
	for section in radio0 unsafe_guest dynamic_vlan dynamic_station; do
		assert_json_value "$stage/wireless" "$section" disabled 1
	done
	assert_json_value "$stage/wireless" unsafe_guest ssid keep-ssid
	assert_json_value "$stage/wireless" unsafe_guest key keep-secret
	assert_json_value "$stage/wireless" dynamic_station key keep-station-secret
	[ "$(grep -c '^commit:wireless$' "$trace")" -eq 1 ] ||
		fail 'changed emergency wireless quarantine did not commit exactly once'

	: >"$trace"
	run_real_staged_ucode verify-all-wireless-disabled "$stage" "$delta" "$trace" "$stdout_file" ||
		fail 'read-only quarantine verifier rejected a fully disabled package'
	[ "$(cat "$stdout_file")" = disabled ] ||
		fail 'quarantine verifier did not print exactly disabled'
	[ "$(grep -c '^commit:' "$trace" 2>/dev/null || true)" -eq 0 ] ||
		fail 'read-only quarantine verifier committed wireless'

	node -e '
		const fs=require("fs"), f=process.argv[1], o=JSON.parse(fs.readFileSync(f));
		o.unsafe_guest.disabled="0"; fs.writeFileSync(f, JSON.stringify(o,null,2)+"\n");
	' "$stage/wireless"
	if run_real_staged_ucode verify-all-wireless-disabled "$stage" "$delta" "$trace" "$stdout_file" \
		>"$stage.verify.log" 2>&1; then
		fail 'quarantine verifier accepted an enabled wireless interface'
	fi

	stage="$TEST_TMP/ucode-wireless-quarantine-empty"
	delta="$TEST_TMP/ucode-wireless-quarantine-empty.delta"
	trace="$TEST_TMP/ucode-wireless-quarantine-empty.trace"
	stdout_file="$TEST_TMP/ucode-wireless-quarantine-empty.stdout"
	mkdir "$stage" "$delta"
	printf '{}\n' >"$stage/wireless"
	run_real_staged_ucode verify-all-wireless-disabled "$stage" "$delta" "$trace" "$stdout_file" ||
		fail 'quarantine verifier rejected an empty inert wireless package'
	[ "$(cat "$stdout_file")" = disabled ] ||
		fail 'empty quarantine verifier did not print exactly disabled'

	# A malformed late wifi-iface proves classification is wholly read-only:
	# the earlier valid LAN AP must not be persisted on failure.
	stage="$TEST_TMP/ucode-wireless-invalid-late"
	delta="$TEST_TMP/ucode-wireless-invalid-late.delta"
	trace="$TEST_TMP/ucode-wireless-invalid-late.trace"
	stdout_file="$TEST_TMP/ucode-wireless-invalid-late.stdout"
	mkdir "$stage" "$delta"
	cat >"$stage/wireless" <<'JSON_WIRELESS_INVALID'
{
  "valid_first": { ".type": "wifi-iface", "mode": "ap", "network": "lan", "isolate": "0" },
  "invalid_late": { ".type": "wifi-iface", "mode": "ap" }
}
JSON_WIRELESS_INVALID
	rmdir "$delta"
	assert_wireless_validation_fails_unchanged "$stage" 'AP with missing network classification'

	stage="$TEST_TMP/ucode-wireless-invalid-name"
	mkdir "$stage"
	cat >"$stage/wireless" <<'JSON_WIRELESS_INVALID_NAME'
{
  "safe_key": { ".type": "wifi-iface", ".name": "bad.section", "mode": "ap", "network": "lan" }
}
JSON_WIRELESS_INVALID_NAME
	assert_wireless_validation_fails_unchanged "$stage" 'wifi-iface with an unsafe real section name'

	stage="$TEST_TMP/ucode-wireless-invalid-mode"
	mkdir "$stage"
	cat >"$stage/wireless" <<'JSON_WIRELESS_INVALID_MODE'
{
  "bad_mode": { ".type": "wifi-iface", "mode": ["ap"], "network": "lan" }
}
JSON_WIRELESS_INVALID_MODE
	assert_wireless_validation_fails_unchanged "$stage" 'wifi-iface with a non-string mode'

	stage="$TEST_TMP/ucode-wireless-invalid-network-list"
	mkdir "$stage"
	cat >"$stage/wireless" <<'JSON_WIRELESS_INVALID_NETWORK'
{
  "bad_network": { ".type": "wifi-iface", "mode": "ap", "network": ["lan", 7] }
}
JSON_WIRELESS_INVALID_NETWORK
	assert_wireless_validation_fails_unchanged "$stage" 'wifi-iface with a non-string network list entry'
}

run_staged_ucode_contracts
if [ "${PROXYPOOL_TEST_FOCUS_STAGED_UCI:-0}" = 1 ]; then
	echo 'ProxyPool real staged UCI focused matrix: PASS'
	exit 0
fi

BOOT_ID_FILE="$TEST_TMP/boot-id"
printf 'test-boot-one\n' >"$BOOT_ID_FILE"
NFTABLES_USER_DIR="$TEST_TMP/nftables.d"
mkdir -p "$NFTABLES_USER_DIR"
CONTRACT_DIR="$TEST_TMP/contracts"
mkdir -p "$CONTRACT_DIR"
GUARD_CONTRACT_FILE="$CONTRACT_DIR/proxypool-guard.nft"
INPUT_GATE_CONTRACT_FILE="$CONTRACT_DIR/proxypool-fw4-input-gate.nft"
FORWARD_GATE_CONTRACT_FILE="$CONTRACT_DIR/proxypool-fw4-forward-gate.nft"
LAN_ISOLATION_CONTRACT_FILE="$CONTRACT_DIR/lan-isolation.sh"
LAN_HOTPLUG_CONTRACT_FILE="$CONTRACT_DIR/99-proxypool-lan-isolation"
LAN_IFACE_HOTPLUG_CONTRACT_FILE="$CONTRACT_DIR/99-proxypool-lan-isolation-iface"
LAN_WORKER_CONTRACT_FILE="$CONTRACT_DIR/lan-isolation-worker.sh"
GUARD_INIT_CONTRACT_FILE="$CONTRACT_DIR/proxypool-guard.init"
UCI_STAGED_CONTRACT_FILE="$CONTRACT_DIR/proxypool-uci-staged.uc"
LEGACY_GATE_CONTRACT_FILE="$CONTRACT_DIR/legacy-gate.sh"
cp "$ROOT/proxypool-core/files/proxypool-guard.nft" "$GUARD_CONTRACT_FILE"
cp "$ROOT/proxypool-core/files/proxypool-fw4-input-gate.nft" "$INPUT_GATE_CONTRACT_FILE"
cp "$ROOT/proxypool-core/files/proxypool-fw4-forward-gate.nft" "$FORWARD_GATE_CONTRACT_FILE"
cp "$ROOT/proxypool-core/files/lan-isolation.sh" "$LAN_ISOLATION_CONTRACT_FILE"
cp "$ROOT/proxypool-core/files/proxypool-lan-isolation.hotplug" "$LAN_HOTPLUG_CONTRACT_FILE"
cp "$ROOT/proxypool-core/files/proxypool-lan-isolation.hotplug" "$LAN_IFACE_HOTPLUG_CONTRACT_FILE"
cp "$ROOT/proxypool-core/files/lan-isolation-worker.sh" "$LAN_WORKER_CONTRACT_FILE"
cp "$ROOT/proxypool-core/files/proxypool-guard.init" "$GUARD_INIT_CONTRACT_FILE"
cp "$ROOT/proxypool-core/files/proxypool-uci-staged.uc" "$UCI_STAGED_CONTRACT_FILE"
cp "$ROOT/proxypool-core/files/legacy-gate.sh" "$LEGACY_GATE_CONTRACT_FILE"
chmod 644 "$GUARD_CONTRACT_FILE" "$INPUT_GATE_CONTRACT_FILE" "$FORWARD_GATE_CONTRACT_FILE"
chmod 644 "$UCI_STAGED_CONTRACT_FILE"
chmod 755 "$LAN_ISOLATION_CONTRACT_FILE" "$LAN_HOTPLUG_CONTRACT_FILE" \
	"$LAN_IFACE_HOTPLUG_CONTRACT_FILE" "$LAN_WORKER_CONTRACT_FILE" \
	"$GUARD_INIT_CONTRACT_FILE" "$LEGACY_GATE_CONTRACT_FILE"
DATA_CONTRACT_FILES="$GUARD_CONTRACT_FILE $INPUT_GATE_CONTRACT_FILE $FORWARD_GATE_CONTRACT_FILE $UCI_STAGED_CONTRACT_FILE"
EXEC_CONTRACT_FILES="$LAN_ISOLATION_CONTRACT_FILE $LAN_HOTPLUG_CONTRACT_FILE $LAN_IFACE_HOTPLUG_CONTRACT_FILE $LAN_WORKER_CONTRACT_FILE $GUARD_INIT_CONTRACT_FILE $LEGACY_GATE_CONTRACT_FILE"
CONTRACT_FILES="$DATA_CONTRACT_FILES $EXEC_CONTRACT_FILES"

# File-backed fake UCI used by the target-shaped staged-helper seam and by the
# assertions below.  Production never receives this cursor: it must invoke its
# single-process ucode helper with absolute package loads.
cat >"$BIN/uci" <<'FAKE_UCI'
#!/usr/bin/env sh
set -eu

config_dir=${UCI_CONFIG_DIR:-/etc/config}
quiet=0
while [ "$#" -gt 0 ]; do
	case "$1" in
		-q) quiet=1; shift ;;
		-c) config_dir=$2; shift 2 ;;
		*) break ;;
	esac
done

command=${1:-}
[ "$#" -eq 0 ] || shift

die() {
	[ "$quiet" -eq 1 ] || printf 'fake uci: %s\n' "$*" >&2
	exit 1
}

package_for_key() {
	printf '%s\n' "${1%%.*}"
}

lookup() {
	key=$1
	package=$(package_for_key "$key")
	store="$config_dir/$package"
	[ -f "$store" ] || return 1
	awk -F= -v wanted="$key" '$1 == wanted { sub(/^[^=]*=/, ""); print; found=1; exit } END { if (!found) exit 1 }' "$store"
}

write_value() {
	key=$1
	value=$2
	if current=$(lookup "$key" 2>/dev/null) && [ "$current" = "$value" ]; then
		return 0
	fi
	package=$(package_for_key "$key")
	store="$config_dir/$package"
	mkdir -p "$config_dir"
	[ -f "$store" ] || : >"$store"
	tmp="$store.tmp.$$"
	awk -F= -v wanted="$key" '$1 != wanted { print }' "$store" >"$tmp"
	printf '%s=%s\n' "$key" "$value" >>"$tmp"
	mv "$tmp" "$store"
}

delete_key() {
	key=$1
	package=$(package_for_key "$key")
	store="$config_dir/$package"
	[ -f "$store" ] || return 0
	tmp="$store.tmp.$$"
	awk -F= -v wanted="$key" '$1 != wanted && index($1, wanted ".") != 1 { print }' "$store" >"$tmp"
	section=${key#*.}
	case "$section" in
		@*\[*\])
			anonymous_type=${section#@}
			anonymous_type=${anonymous_type%%[*}
			anonymous_index=${section#*[}
			anonymous_index=${anonymous_index%]}
			reindexed="$tmp.reindexed"
			awk -F= -v prefix="$package.@$anonymous_type[" -v removed="$anonymous_index" '
				{
					lhs=$1
					if (index(lhs, prefix) == 1) {
						tail=substr(lhs, length(prefix) + 1)
						closing=index(tail, "]")
						idx=substr(tail, 1, closing - 1) + 0
						if (idx > removed)
							lhs=prefix (idx - 1) substr(tail, closing)
					}
					sub(/^[^=]*=/, "")
					print lhs "=" $0
				}
			' "$tmp" >"$reindexed"
			mv "$reindexed" "$tmp"
			;;
	esac
	mv "$tmp" "$store"
}

case "$command" in
	show)
		package=${1:-}
		[ -n "$package" ] || die 'show needs a package'
		[ -f "$config_dir/$package" ] || die "missing package $package"
		cat "$config_dir/$package"
		;;
	get)
		lookup "${1:-}" || die "missing key ${1:-}"
		;;
	set)
		assignment=${1:-}
		case "$assignment" in *=*) : ;; *) die 'set needs key=value' ;; esac
		write_value "${assignment%%=*}" "${assignment#*=}"
		;;
	delete)
		delete_key "${1:-}"
		;;
	commit)
		: # Every fake mutation is already persisted inside the selected staging dir.
		;;
	*) die "unsupported command $command" ;;
esac
FAKE_UCI
chmod 755 "$BIN/uci"

cat >"$BIN/staged-apply" <<'FAKE_STAGED_APPLY'
#!/usr/bin/env sh
set -eu

[ "$#" -eq 3 ] || { echo 'staged apply needs action, config directory, and delta directory' >&2; exit 2; }
action=$1
config_dir=$2
delta_dir=$3
[ -d "$config_dir" ] || { echo "missing staged config directory: $config_dir" >&2; exit 2; }
mkdir -p "$delta_dir"
UCI=${PROXYPOOL_TEST_FAKE_UCI:?}
export UCI_CONFIG_DIR="$config_dir"
printf 'uci:%s:%s\n' "$action" "$config_dir" >>"$PROXYPOOL_TEST_TRACE"
[ "${PROXYPOOL_TEST_UCI_FAIL_ACTION:-}" != "$action" ] || exit 41

get_value() {
	"$UCI" -q get "$1"
}

section_names() {
	package=$1
	type=$2
	"$UCI" -q show "$package" | while IFS= read -r line; do
		case "$line" in
			"$package".*="$type")
				section=${line#"$package".}
				printf '%s\n' "${section%%=*}"
				;;
		esac
	done
}

find_unique() {
	package=$1
	type=$2
	option=$3
	wanted=$4
	found=
	count=0
	for section in $(section_names "$package" "$type"); do
		actual=$(get_value "$package.$section.$option" 2>/dev/null || true)
		if [ "$actual" = "$wanted" ]; then
			found=$section
			count=$((count + 1))
		fi
	done
	[ "$count" -eq 1 ] || return 1
	printf '%s\n' "$found"
}

set_value() {
	"$UCI" -q set "$1=$2"
}

delete_value() {
	"$UCI" -q delete "$1"
}

set_offload_clamp() {
	found=0
	for section in $(section_names firewall defaults); do
		found=1
		set_value "firewall.$section.flow_offloading" 0
		set_value "firewall.$section.flow_offloading_hw" 0
		set_value "firewall.$section.auto_includes" 0
	done
	[ "$found" -eq 1 ]
}

verify_management_ip() {
	lan_network=$(find_unique network interface device br-lan) || return 1
	management_ip=$(get_value "network.$lan_network.ipaddr" 2>/dev/null || true)
	case "$management_ip" in
		192.168.9.1|192.168.9.1/24) return 0 ;;
		*) return 1 ;;
	esac
}

set_bridge_port_isolation() {
	bridge=$(find_unique network device name br-lan) || return 1
	[ "$(get_value "network.$bridge.type" 2>/dev/null || true)" = bridge ] || return 1
	ports=$(get_value "network.$bridge.ports" 2>/dev/null) || return 1
	printf '%s\n' "$ports" |
		grep -Eq '^[A-Za-z0-9_.-]{1,15}([[:space:]]+[A-Za-z0-9_.-]{1,15})*$' || return 1

	plans=
	seen=' '
	position=0
	for port in $ports; do
		position=$((position + 1))
		case "$port" in .|..|br-lan) return 1 ;; esac
		case "$seen" in *" $port "*) return 1 ;; esac
		seen="$seen$port "
		match=
		count=0
		for section in $(section_names network device); do
			if [ "$(get_value "network.$section.name" 2>/dev/null || true)" = "$port" ]; then
				match=$section
				count=$((count + 1))
			fi
		done
		[ "$count" -le 1 ] || return 1
		create=0
		if [ "$count" -eq 0 ]; then
			owned=$(printf 'proxypool_lan_port_%02d' "$position")
			if grep -Fq "network.$owned=" "$config_dir/network"; then return 1; fi
			match=$owned
			create=1
		fi
		plans="${plans:+$plans }$match|$port|$create"
	done
	[ -n "$plans" ] || return 1

	# The classification above is complete before this first fake UCI mutation.
	for plan in $plans; do
		section=${plan%%|*}
		rest=${plan#*|}
		port=${rest%%|*}
		create=${rest##*|}
		if [ "$create" -eq 1 ]; then
			set_value "network.$section" device
			set_value "network.$section.name" "$port"
		fi
		set_value "network.$section.isolate" 1
	done
}

install_owned_includes() {
	while :; do
		match=$(section_names firewall include | sed -n '1p')
		[ -n "$match" ] || break
		delete_value "firewall.$match"
	done
	set_value firewall.proxypool_guard include
	set_value firewall.proxypool_guard.type nftables
	set_value firewall.proxypool_guard.path /usr/lib/proxypool/proxypool-guard.nft
	set_value firewall.proxypool_guard.position ruleset-prepend
	set_value firewall.proxypool_fw4_input_gate include
	set_value firewall.proxypool_fw4_input_gate.type nftables
	set_value firewall.proxypool_fw4_input_gate.path /usr/lib/proxypool/proxypool-fw4-input-gate.nft
	set_value firewall.proxypool_fw4_input_gate.position chain-prepend
	set_value firewall.proxypool_fw4_input_gate.chain input
	set_value firewall.proxypool_fw4_forward_gate include
	set_value firewall.proxypool_fw4_forward_gate.type nftables
	set_value firewall.proxypool_fw4_forward_gate.path /usr/lib/proxypool/proxypool-fw4-forward-gate.nft
	set_value firewall.proxypool_fw4_forward_gate.position chain-prepend
	set_value firewall.proxypool_fw4_forward_gate.chain forward
	set_value firewall.proxypool_guard_resync include
	set_value firewall.proxypool_guard_resync.type script
	set_value firewall.proxypool_guard_resync.path /usr/lib/proxypool/guard-resync.sh
	set_value firewall.proxypool_guard_resync.fw4_compatible 1
}

if [ "$action" = clamp-offload ]; then
	[ -f "$config_dir/network" ] || exit 2
	verify_management_ip || exit 1
	set_offload_clamp
	install_owned_includes
	"$UCI" -q commit firewall
	if [ -n "${PROXYPOOL_TEST_CLAMPED_SNAPSHOT:-}" ]; then
		for package in firewall dhcp network; do
			cp "$config_dir/$package" "$PROXYPOOL_TEST_CLAMPED_SNAPSHOT/$package"
		done
	fi
	exit 0
fi

[ "$action" = apply-defaults ] || { echo "unsupported fake staged action: $action" >&2; exit 2; }
for package in firewall dhcp network; do
	[ -f "$config_dir/$package" ] || exit 2
done

lan_zone=$(find_unique firewall zone name lan) || exit 1
lan_dhcp=$(find_unique dhcp dhcp interface lan) || exit 1
lan_network=$(find_unique network interface device br-lan) || exit 1
dnsmasq=
dnsmasq_count=0
for section in $(section_names dhcp dnsmasq); do
	dnsmasq=$section
	dnsmasq_count=$((dnsmasq_count + 1))
done
[ "$dnsmasq_count" -eq 1 ] || exit 1
verify_management_ip || exit 1
set_bridge_port_isolation || exit 1
set_offload_clamp
set_value "firewall.$lan_zone.input" REJECT
set_value "firewall.$lan_zone.forward" REJECT
set_value "firewall.$lan_zone.output" ACCEPT

for section in $(section_names firewall zone); do
	if devices=$(get_value "firewall.$section.device" 2>/dev/null); then
		filtered=
		for device in $devices; do
			case "$device" in ppp-+|ppp-\*) continue ;; esac
			filtered="${filtered:+$filtered }$device"
		done
		if [ -n "$filtered" ]; then
			set_value "firewall.$section.device" "$filtered"
		else
			delete_value "firewall.$section.device"
		fi
	fi
done

# Remove every old LAN input rule regardless of target or ordering.  An early
# DROP/REJECT would otherwise make the explicit management whitelist unusable.
while :; do
	match=
	for section in $(section_names firewall rule); do
		src=$(get_value "firewall.$section.src" 2>/dev/null || true)
		dest=$(get_value "firewall.$section.dest" 2>/dev/null || true)
		if [ "$src" = lan ] && [ -z "$dest" ]; then match=$section; break; fi
	done
	[ -n "$match" ] || break
	delete_value "firewall.$match"
done

while :; do
	match=
	for section in $(section_names firewall forwarding); do
		src=$(get_value "firewall.$section.src" 2>/dev/null || true)
		dest=$(get_value "firewall.$section.dest" 2>/dev/null || true)
		if [ "$src:$dest" = lan:wan ]; then match=$section; break; fi
	done
	[ -n "$match" ] || break
	delete_value "firewall.$match"
done

while :; do
	match=
	for section in $(section_names firewall rule); do
		src=$(get_value "firewall.$section.src" 2>/dev/null || true)
		[ "$src" = lan ] || continue
		if target=$(get_value "firewall.$section.target" 2>/dev/null); then
			target=$(printf '%s' "$target" | tr '[:upper:]' '[:lower:]')
			case accept in "$target"*) match=$section; break ;; esac
		fi
	done
	[ -n "$match" ] || break
	delete_value "firewall.$match"
done

for section in \
	proxypool_allow_dhcp proxypool_allow_dns \
	proxypool_allow_admin_ssh \
	proxypool_allow_admin_http proxypool_allow_admin_https; do
	delete_value "firewall.$section"
done

set_value firewall.proxypool_allow_dhcp rule
set_value firewall.proxypool_allow_dhcp.name 'ProxyPool Allow DHCP'
set_value firewall.proxypool_allow_dhcp.src lan
set_value firewall.proxypool_allow_dhcp.proto udp
set_value firewall.proxypool_allow_dhcp.src_port 68
set_value firewall.proxypool_allow_dhcp.dest_port 67
set_value firewall.proxypool_allow_dhcp.family ipv4
set_value firewall.proxypool_allow_dhcp.target ACCEPT

for spec in \
	'proxypool_allow_admin_ssh|ProxyPool Allow SSH Management|22' \
	'proxypool_allow_admin_http|ProxyPool Allow HTTP Management|80' \
	'proxypool_allow_admin_https|ProxyPool Allow HTTPS Management|443'; do
	section=${spec%%|*}
	rest=${spec#*|}
	name=${rest%%|*}
	port=${rest##*|}
	set_value "firewall.$section" rule
	set_value "firewall.$section.name" "$name"
	set_value "firewall.$section.src" lan
	set_value "firewall.$section.proto" tcp
	set_value "firewall.$section.dest_port" "$port"
	set_value "firewall.$section.dest_ip" 192.168.9.1
	set_value "firewall.$section.family" ipv4
	set_value "firewall.$section.target" ACCEPT
done

install_owned_includes

set_value "dhcp.$lan_dhcp.ra" disabled
set_value "dhcp.$lan_dhcp.dhcpv6" disabled
set_value "dhcp.$lan_dhcp.ndp" disabled
set_value "dhcp.$lan_dhcp.dhcp_option" 6,192.168.9.1
set_value "dhcp.$dnsmasq.noresolv" 1
set_value "dhcp.$dnsmasq.port" 0
delete_value "dhcp.$dnsmasq.server"
set_value "network.$lan_network.delegate" 0
delete_value "network.$lan_network.ip6assign"
delete_value "network.$lan_network.ip6hint"
delete_value "network.$lan_network.ip6class"
for package in firewall dhcp network; do "$UCI" -q commit "$package"; done
exit 0
FAKE_STAGED_APPLY
chmod 755 "$BIN/staged-apply"

cat >"$BIN/flowtable-probe" <<'FAKE_FLOWTABLE_PROBE'
#!/usr/bin/env sh
set -eu
count=$(grep -c '^flowtable:probe:' "$PROXYPOOL_TEST_TRACE" 2>/dev/null || true)
count=$((count + 1))
sequence=${PROXYPOOL_TEST_FLOWTABLE_SEQUENCE:-1,1,1}
status=$(printf '%s\n' "$sequence" | awk -F, -v n="$count" '{ print (n <= NF ? $n : $NF) }')
printf 'flowtable:probe:%s:%s\n' "$count" "$status" >>"$PROXYPOOL_TEST_TRACE"
exit "$status"
FAKE_FLOWTABLE_PROBE

cat >"$BIN/pending-delta-probe" <<'FAKE_PENDING_PROBE'
#!/usr/bin/env sh
set -eu
status=${PROXYPOOL_TEST_PENDING_DELTA_STATUS:-0}
printf 'uci:pending-probe:%s\n' "$status" >>"$PROXYPOOL_TEST_TRACE"
exit "$status"
FAKE_PENDING_PROBE

cat >"$BIN/fw4-mode-probe" <<'FAKE_FW4_MODE_PROBE'
#!/usr/bin/env sh
set -eu
case "${1:-}" in cold|live) : ;; *) exit 2 ;; esac
status=${PROXYPOOL_TEST_FW4_MODE_STATUS:-0}
printf 'fw4:mode-probe:%s:%s\n' "$1" "$status" >>"$PROXYPOOL_TEST_TRACE"
exit "$status"
FAKE_FW4_MODE_PROBE

cat >"$BIN/flock" <<'FAKE_FLOCK'
#!/usr/bin/env sh
set -eu
[ "${1:-}" = -x ] || exit 2
fd=${2:-}
case "$fd" in
	8) kind=transaction ;;
	9) kind=fw4 ;;
	*) kind="fd-$fd" ;;
esac
printf 'lock:%s:acquire\n' "$kind" >>"$PROXYPOOL_TEST_TRACE"
[ "${PROXYPOOL_TEST_FLOCK_FAIL_KIND:-}" != "$kind" ] || exit 73
exit 0
FAKE_FLOCK

cat >"$BIN/sync" <<'FAKE_SYNC'
#!/usr/bin/env sh
set -eu
printf 'storage:sync\n' >>"$PROXYPOOL_TEST_TRACE"
[ "${PROXYPOOL_TEST_SYNC_FAIL:-0}" = 0 ] || exit 74
exit 0
FAKE_SYNC

cat >"$BIN/id" <<'FAKE_ID'
#!/usr/bin/env sh
[ "$#" -eq 1 ] && [ "$1" = -u ] || exit 2
printf '0\n'
FAKE_ID

cat >"$BIN/nft-runtime" <<'FAKE_NFT_RUNTIME'
#!/usr/bin/env sh
set -eu
case "$*" in
	'list table inet proxypool_guard')
		cat <<'RUNTIME_GUARD_PREFIX'
table inet proxypool_guard {
	set v1_l2tp_paths {
		type ipv4_addr . ifname
RUNTIME_GUARD_PREFIX
		if [ "${PROXYPOOL_TEST_RUNTIME_FAULT:-}" = nonempty_set ]; then
			printf '%s\n' '		elements = { 192.0.2.10 . "ppp-leak" }'
		fi
		cat <<'RUNTIME_GUARD_BODY'
	}
	set v1_tcp_redirects {
		type ipv4_addr . inet_service
	}
	set v2_l2tp_paths {
		type ether_addr . ipv4_addr . ifname
RUNTIME_GUARD_BODY
		if [ "${PROXYPOOL_TEST_RUNTIME_FAULT:-}" != missing_timeout ]; then
			printf '\t\ttimeout 20s\n'
		fi
		cat <<'RUNTIME_GUARD_BODY'
	}
	set v2_l2tp_return_paths {
		type ipv4_addr . ifname
		timeout 20s
	}
	set v2_tcp_redirects {
		type ether_addr . ipv4_addr . inet_service
		timeout 20s
	}
	map v2_tcp_redirect_ports {
		type ether_addr . ipv4_addr : inet_service
		timeout 20s
	}
	set v2_proxy_uploads {
		type ether_addr . ipv4_addr
		timeout 20s
		counter
	}
	set v2_proxy_downloads {
		type ipv4_addr . inet_service
		timeout 20s
		counter
	}
	set v2_dns_clients {
		type ether_addr . ipv4_addr
		timeout 20s
	}
	map v2_policy_marks {
		type ether_addr . ipv4_addr : mark
		timeout 20s
	}
	set blocked_v4_destinations {
		type ipv4_addr
		flags interval
		auto-merge
		elements = { 0.0.0.0/8,
			10.0.0.0/8,
			100.64.0.0/10,
			127.0.0.0/8,
			169.254.0.0/16,
			172.16.0.0/12,
			192.0.0.0/24,
			192.0.2.0/24,
			192.88.99.0/24,
			192.168.0.0/16,
			198.18.0.0/15,
			198.51.100.0/24,
			203.0.113.0/24,
			224.0.0.0/3 }
	}
	chain guard_prerouting {
		type filter hook prerouting priority raw - 10; policy accept;
		iifname "br-lan" meta mark set meta mark & 0xff000000
		iifname "br-lan" meta mark set ether saddr . ip saddr map @v2_policy_marks
	}
	chain guard_postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		meta mark & 0x00ff0000 == 0x005a0000 meta l4proto tcp ip saddr . oifname @v2_l2tp_return_paths masquerade
	}
	chain guard_proxy_redirect {
		type nat hook prerouting priority dstnat; policy accept;
		iifname "br-lan" ip daddr != @blocked_v4_destinations meta l4proto tcp ether saddr . ip saddr @v2_proxy_uploads redirect to :ether saddr . ip saddr map @v2_tcp_redirect_ports
	}
	chain guard_proxy_output {
		type filter hook output priority filter + 10; policy accept;
		oifname "br-lan" ct status dnat ct direction reply meta l4proto tcp ip daddr . tcp sport @v2_proxy_downloads
	}
	chain guard_input {
		type filter hook input priority filter + 10; policy drop;
		iifname "br-lan" meta nfproto ipv4 udp sport 68 udp dport 67 accept
		iifname "br-lan" ip daddr 192.168.9.1 tcp dport { 22, 80, 443 } accept
		iifname "br-lan" ip daddr 192.168.9.1 ether saddr . ip saddr @v2_dns_clients meta l4proto { tcp, udp } th dport 53 accept
		iifname "br-lan" meta nfproto ipv4 ct original ip daddr @blocked_v4_destinations drop
		iifname "br-lan" meta mark & 0x00ff0000 == 0x005a0000 ct status dnat ip saddr . tcp dport @v1_tcp_redirects accept
		iifname "br-lan" meta mark & 0x00ff0000 == 0x005a0000 ct status dnat ether saddr . ip saddr . tcp dport @v2_tcp_redirects accept
		iifname "br-lan" drop
		iifname "lo" accept
		ct state established,related accept
		iifname "eth1" meta nfproto ipv4 udp sport 67 udp dport 68 accept
	}
	chain guard_forward {
		type filter hook forward priority filter + 10; policy drop;
RUNTIME_GUARD_BODY
		if [ "${PROXYPOOL_TEST_RUNTIME_FAULT:-}" != missing_rule ]; then
			printf '%s\n' '		iifname "br-lan" meta nfproto ipv6 drop'
		fi
		cat <<'RUNTIME_GUARD_SUFFIX'
		iifname "br-lan" meta nfproto ipv4 meta l4proto udp drop
		iifname "br-lan" ip daddr @blocked_v4_destinations drop
		iifname "br-lan" meta mark & 0x00ff0000 == 0x005a0000 meta l4proto tcp ip saddr . oifname @v1_l2tp_paths accept
		iifname "br-lan" meta mark & 0x00ff0000 == 0x005a0000 meta l4proto tcp ether saddr . ip saddr . oifname @v2_l2tp_paths accept
		iifname "br-lan" drop
		oifname "br-lan" meta nfproto ipv6 drop
		oifname "br-lan" meta nfproto ipv4 meta l4proto udp drop
		oifname "br-lan" ip saddr @blocked_v4_destinations drop
		oifname "br-lan" ct state established ct direction reply meta l4proto tcp ip daddr . iifname @v1_l2tp_paths accept
		oifname "br-lan" ct state established ct direction reply meta l4proto tcp ip daddr . iifname @v2_l2tp_return_paths accept
		oifname "br-lan" drop
RUNTIME_GUARD_SUFFIX
		if [ "${PROXYPOOL_TEST_RUNTIME_FAULT:-}" = extra_accept ]; then
			printf '%s\n' '		iifname "br-lan" accept'
		fi
		printf '%s\n' '	}' '}'
		;;
	'list table bridge proxypool_l2_guard')
		[ "${PROXYPOOL_TEST_RUNTIME_FAULT:-}" != missing_bridge ] || exit 2
		cat <<'RUNTIME_BRIDGE'
table bridge proxypool_l2_guard {
	chain guard_l2_forward {
		type filter hook forward priority 10; policy drop;
		meta ibrname "br-lan" meta obrname "br-lan" drop
	}
}
RUNTIME_BRIDGE
		;;
	'list table inet fw4')
		cat <<'RUNTIME_FW4'
table inet fw4 {
	chain input {
		type filter hook input priority filter; policy drop;
		iifname "br-lan" meta nfproto ipv4 meta mark & 0x00ff0000 == 0x005a0000 meta l4proto tcp ct status dnat accept
		iifname "br-lan" ip daddr 192.168.9.1 meta l4proto { tcp, udp } th dport 53 accept
		ct state established,related accept
	}
	chain forward {
		type filter hook forward priority filter; policy drop;
		iifname "br-lan" meta nfproto ipv4 meta mark & 0x00ff0000 == 0x005a0000 meta l4proto tcp accept
		ct state established,related accept
	}
}
RUNTIME_FW4
		;;
	*) exit 2 ;;
esac
FAKE_NFT_RUNTIME

cat >"$BIN/ls" <<'FAKE_LS'
#!/usr/bin/env sh
set -eu
[ "$#" -eq 2 ] && [ "$1" = -nd ] || exit 2
path=$2
metadata=$(/usr/bin/ls -ldn -- "$path") || exit 1
set -- $metadata
links=$2
owner=0
if [ -d "$path" ]; then
	permissions=drwx------
else
	permissions=-rw-------
fi
case " ${PROXYPOOL_TEST_CONTRACT_FILES:-} " in
	*" $path "*)
		permissions=-rw-r--r--
		case " ${PROXYPOOL_TEST_EXEC_CONTRACT_FILES:-} " in
			*" $path "*) permissions=-rwxr-xr-x ;;
		esac
		case "${PROXYPOOL_TEST_CONTRACT_METADATA_FAULT:-}" in
			mode) permissions=-rw-rw-rw- ;;
			owner) owner=1000 ;;
			links) links=2 ;;
		esac
		;;
esac
permissions=${PROXYPOOL_TEST_METADATA_PERMISSIONS:-$permissions}
links=${PROXYPOOL_TEST_METADATA_LINKS:-$links}
owner=${owner:-0}
printf '%s %s %s 0 0 Jan 1 00:00 fixture\n' "$permissions" "$links" "$owner"
FAKE_LS

chmod 755 "$BIN/flowtable-probe" "$BIN/pending-delta-probe" \
	"$BIN/fw4-mode-probe" "$BIN/flock" "$BIN/sync" "$BIN/id" "$BIN/ls" \
	"$BIN/nft-runtime"

cat >"$BIN/fw4-check-staged" <<'FAKE_FW4'
#!/usr/bin/env sh
set -eu
[ "$#" -eq 1 ] || { echo 'staged fw4 wrapper needs exactly one config-directory argument' >&2; exit 1; }
staged_config_dir=$1
[ -d "$staged_config_dir" ] || { echo 'staged fw4 wrapper received a missing directory' >&2; exit 1; }
[ "$staged_config_dir" != "${PROXYPOOL_CONFIG_DIR:-/etc/config}" ] || {
	echo 'staged fw4 wrapper received the live/original configuration' >&2
	exit 1
}
for package in firewall dhcp network; do
	[ -f "$staged_config_dir/$package" ] || { echo "staging is missing $package" >&2; exit 1; }
done
for package in firewall dhcp network; do
	expected_root=$PROXYPOOL_TEST_ORIGINAL_SNAPSHOT
	[ "$package" != firewall ] || expected_root=$PROXYPOOL_TEST_CLAMPED_SNAPSHOT
	cmp -s "$expected_root/$package" "$PROXYPOOL_CONFIG_DIR/$package" || {
		echo "live $package changed before staged fw4 check" >&2
		exit 1
	}
done
printf 'fw4:check:%s\n' "$staged_config_dir" >>"$PROXYPOOL_TEST_TRACE"
[ "${PROXYPOOL_TEST_FW4_FAIL:-0}" = 0 ] || exit 42
exit 0
FAKE_FW4
chmod 755 "$BIN/fw4-check-staged"

cat >"$BIN/config-install" <<'FAKE_INSTALL'
#!/usr/bin/env sh
set -eu
[ "$#" -eq 2 ] || { echo 'config install needs source and destination' >&2; exit 1; }
source_file=$1
destination=$2
[ -f "$source_file" ] || { echo "config install source is missing: $source_file" >&2; exit 1; }
case "$destination" in
	"$PROXYPOOL_CONFIG_DIR"/firewall|"$PROXYPOOL_CONFIG_DIR"/dhcp|"$PROXYPOOL_CONFIG_DIR"/network) : ;;
	*) echo "config install escaped the managed files: $destination" >&2; exit 1 ;;
esac
install_count=$(grep -c '^config:install:' "$PROXYPOOL_TEST_TRACE" 2>/dev/null || true)
install_count=$((install_count + 1))
printf 'config:install:%s\n' "${destination##*/}" >>"$PROXYPOOL_TEST_TRACE"
case ",${PROXYPOOL_TEST_INSTALL_FAIL_COUNTS:-}," in
	*,"$install_count",*) exit 44 ;;
esac
if [ "${PROXYPOOL_TEST_INSTALL_FAIL_AT:-0}" -eq "$install_count" ]; then
	exit 44
fi
cp "$source_file" "$destination"
if [ "${PROXYPOOL_TEST_INSTALL_KILL_AFTER_AT:-0}" -eq "$install_count" ]; then
	kill -KILL "$PPID"
	exit 137
fi
FAKE_INSTALL
chmod 755 "$BIN/config-install"

cat >"$BIN/fw4-activate" <<'FAKE_RELOAD'
#!/usr/bin/env sh
set -eu
[ "${1:-}" = live ] || { echo 'test activator only accepts live mode' >&2; exit 2; }
reload_count=$(grep -c '^firewall:reload$' "$PROXYPOOL_TEST_TRACE" 2>/dev/null || true)
if [ "$reload_count" -eq 0 ]; then
	all_original=1
	for package in firewall dhcp network; do
		cmp -s "$PROXYPOOL_TEST_ORIGINAL_SNAPSHOT/$package" "$PROXYPOOL_CONFIG_DIR/$package" || all_original=0
	done
	[ "$all_original" -eq 0 ] || [ "${PROXYPOOL_TEST_ALLOW_UNCHANGED_RELOAD:-0}" = 1 ] || {
		echo 'firewall reload occurred before the staged configuration was installed' >&2
		exit 1
	}
elif [ "${PROXYPOOL_TEST_RELOAD_FAIL:-0}" = once ]; then
	for package in firewall dhcp network; do
		expected_root=$PROXYPOOL_TEST_ORIGINAL_SNAPSHOT
		[ "$package" != firewall ] || expected_root=$PROXYPOOL_TEST_CLAMPED_SNAPSHOT
		cmp -s "$expected_root/$package" "$PROXYPOOL_CONFIG_DIR/$package" || {
			echo "rollback reload saw unrestored $package" >&2
			exit 1
		}
	done
fi
printf 'firewall:reload\n' >>"$PROXYPOOL_TEST_TRACE"
if [ "${PROXYPOOL_TEST_RELOAD_KILL_SELF:-0}" = 1 ] && [ "$reload_count" -eq 0 ]; then
	(
		sleep 2
		printf 'firewall:orphan-completed\n' >>"$PROXYPOOL_TEST_TRACE"
	) &
	kill -KILL $$
	exit 137
fi
if [ "${PROXYPOOL_TEST_RELOAD_FAIL:-0}" = once ] && [ "$reload_count" -eq 0 ]; then
	exit 43
fi
[ "${PROXYPOOL_TEST_RELOAD_FAIL:-0}" != always ] || exit 43
ACTION=includes "$PROXYPOOL_TRANSACTION_HELPER" finalize-fw4-locked
exit 0
FAKE_RELOAD
chmod 755 "$BIN/fw4-activate"

cat >"$BIN/guard-reset" <<'FAKE_GUARD'
#!/usr/bin/env sh
set -eu
[ "${1:-}" = reset-empty ] || { echo 'guard reset must request reset-empty' >&2; exit 1; }
printf 'guardian:reset-empty\n' >>"$PROXYPOOL_TEST_TRACE"
if [ "${PROXYPOOL_TEST_GUARD_KILL_PARENT:-0}" = 1 ]; then
	kill -KILL "$PPID"
	exit 137
fi
exit "${PROXYPOOL_TEST_GUARD_RESET_RC:-0}"
FAKE_GUARD
chmod 755 "$BIN/guard-reset"

write_fixture() {
	directory=$1
	layout=$2
	mkdir -p "$directory"
	case "$layout" in
		wan_first)
			cat >"$directory/firewall" <<'EOF_FIREWALL'
firewall.zone_wan=zone
firewall.zone_wan.name=wan
firewall.zone_wan.input=REJECT
firewall.zone_wan.masq=1
firewall.zone_wan.device=eth0 ppp-+
firewall.zone_guest=zone
firewall.zone_guest.name=guest
firewall.zone_guest.input=REJECT
firewall.defaults_main=defaults
firewall.defaults_main.flow_offloading=1
firewall.defaults_main.flow_offloading_hw=1
firewall.defaults_main.auto_includes=1
firewall.zone_lan_late=zone
firewall.zone_lan_late.name=lan
firewall.zone_lan_late.input=ACCEPT
firewall.zone_lan_late.output=ACCEPT
firewall.zone_lan_late.forward=ACCEPT
EOF_FIREWALL
			;;
		lan_first)
			cat >"$directory/firewall" <<'EOF_FIREWALL'
firewall.zone_lan_early=zone
firewall.zone_lan_early.name=lan
firewall.zone_lan_early.input=ACCEPT
firewall.zone_lan_early.output=ACCEPT
firewall.zone_lan_early.forward=ACCEPT
firewall.defaults_last=defaults
firewall.defaults_last.flow_offloading=1
firewall.defaults_last.flow_offloading_hw=1
firewall.defaults_last.auto_includes=1
firewall.zone_iot=zone
firewall.zone_iot.name=iot
firewall.zone_wan_last=zone
firewall.zone_wan_last.name=wan
firewall.zone_wan_last.input=REJECT
firewall.zone_wan_last.masq=1
firewall.zone_wan_last.device=eth0 ppp-+
EOF_FIREWALL
			;;
		*) fail "unknown fixture layout: $layout" ;;
	esac
	# These guest-zone entries exercise only the wired firewall transform's
	# ownership boundary.  Wireless admission is tested separately above and
	# rejects every AP that is not attached exclusively to lan.
	cat >>"$directory/firewall" <<'EOF_FIREWALL'
firewall.legacy_lan_wan_a=forwarding
firewall.legacy_lan_wan_a.src=lan
firewall.legacy_lan_wan_a.dest=wan
firewall.unrelated_guest_wan=forwarding
firewall.unrelated_guest_wan.src=guest
firewall.unrelated_guest_wan.dest=wan
firewall.legacy_lan_wan_b=forwarding
firewall.legacy_lan_wan_b.src=lan
firewall.legacy_lan_wan_b.dest=wan
firewall.unrelated_lan_guest=forwarding
firewall.unrelated_lan_guest.src=lan
firewall.unrelated_lan_guest.dest=guest
firewall.legacy_lan_ssh=rule
firewall.legacy_lan_ssh.name=Legacy LAN SSH
firewall.legacy_lan_ssh.src=lan
firewall.legacy_lan_ssh.proto=tcp
firewall.legacy_lan_ssh.dest_port=22
firewall.legacy_lan_ssh.target=ACCEPT
firewall.legacy_lan_input_reject=rule
firewall.legacy_lan_input_reject.name=Early legacy LAN input reject
firewall.legacy_lan_input_reject.src=lan
firewall.legacy_lan_input_reject.proto=all
firewall.legacy_lan_input_reject.target=REJECT
firewall.third_party_rule=rule
firewall.third_party_rule.name=Keep third party rule
firewall.third_party_rule.src=guest
firewall.third_party_rule.dest_port=22
firewall.third_party_rule.target=ACCEPT
firewall.@forwarding[0]=forwarding
firewall.@forwarding[0].src=lan
firewall.@forwarding[0].dest=wan
firewall.@forwarding[1]=forwarding
firewall.@forwarding[1].src=lan
firewall.@forwarding[1].dest=wan
firewall.@forwarding[2]=forwarding
firewall.@forwarding[2].src=guest
firewall.@forwarding[2].dest=iot
firewall.@rule[0]=rule
firewall.@rule[0].name=Anonymous lowercase LAN accept
firewall.@rule[0].src=lan
firewall.@rule[0].proto=tcp
firewall.@rule[0].dest_port=8080
firewall.@rule[0].target=accept
firewall.@rule[1]=rule
firewall.@rule[1].name=Anonymous uppercase LAN accept
firewall.@rule[1].src=lan
firewall.@rule[1].proto=tcp
firewall.@rule[1].dest_port=8443
firewall.@rule[1].target=ACCEPT
firewall.@rule[2]=rule
firewall.@rule[2].name=Preserve anonymous guest accept
firewall.@rule[2].src=guest
firewall.@rule[2].proto=tcp
firewall.@rule[2].dest_port=9443
firewall.@rule[2].target=accept
firewall.@rule[3]=rule
firewall.@rule[3].name=Anonymous abbreviated LAN accept
firewall.@rule[3].src=lan
firewall.@rule[3].proto=tcp
firewall.@rule[3].dest_port=9080
firewall.@rule[3].target=ac
firewall.@rule[4]=rule
firewall.@rule[4].name=Anonymous empty-prefix LAN accept
firewall.@rule[4].src=lan
firewall.@rule[4].proto=tcp
firewall.@rule[4].dest_port=9081
firewall.@rule[4].target=
firewall.@include[0]=include
firewall.@include[0].type=nftables
firewall.@include[0].path=/usr/lib/proxypool/proxypool-guard.nft
firewall.@include[0].position=ruleset-prepend
firewall.@include[1]=include
firewall.@include[1].type=nftables
firewall.@include[1].path=/usr/lib/proxypool/proxypool-fw4-input-gate.nft
firewall.@include[1].position=chain-prepend
firewall.@include[1].chain=input
firewall.@include[2]=include
firewall.@include[2].type=nftables
firewall.@include[2].path=/usr/lib/third-party/preserve.nft
firewall.@include[2].position=table-append
firewall.@include[3]=include
firewall.@include[3].type=script
firewall.@include[3].path=/usr/lib/third-party/post-fw4.sh
firewall.@include[3].fw4_compatible=1
EOF_FIREWALL
	cat >"$directory/dhcp" <<'EOF_DHCP'
dhcp.dnsmasq_main=dnsmasq
dhcp.dnsmasq_main.noresolv=0
dhcp.dnsmasq_main.port=53
dhcp.dnsmasq_main.server=/tmp/resolv.conf.d/resolv.conf.auto
dhcp.lan_custom=dhcp
dhcp.lan_custom.interface=lan
dhcp.lan_custom.ra=server
dhcp.lan_custom.dhcpv6=server
dhcp.lan_custom.ndp=relay
dhcp.guest=dhcp
dhcp.guest.interface=guest
dhcp.guest.ra=server
EOF_DHCP
	cat >"$directory/network" <<'EOF_NETWORK'
network.wan=interface
network.wan.proto=dhcp
network.lan_custom=interface
network.lan_custom.device=br-lan
network.lan_custom.proto=static
network.lan_custom.ipaddr=192.168.9.1
network.lan_custom.delegate=1
network.lan_custom.ip6assign=64
network.lan_custom.ip6hint=1
network.lan_custom.ip6class=wan6 local
network.br_lan_bridge=device
network.br_lan_bridge.name=br-lan
network.br_lan_bridge.type=bridge
network.br_lan_bridge.ports=lan1 lan2
network.lan1_physical=device
network.lan1_physical.name=lan1
network.lan1_physical.isolate=0
network.lan1_physical.mtu=1500
network.foreign_uplink=device
network.foreign_uplink.name=eth0.2
network.foreign_uplink.mtu=1492
network.guest=interface
network.guest.delegate=1
EOF_NETWORK
}

get_value() {
	config_dir=$1
	key=$2
	UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q get "$key"
}

find_section() {
	config_dir=$1
	package=$2
	type=$3
	option=$4
	value=$5
	while IFS= read -r line; do
		case "$line" in
			"$package".*="$type")
				section=${line#"$package".}
				section=${section%%=*}
				actual=$(get_value "$config_dir" "$package.$section.$option" 2>/dev/null || true)
				[ "$actual" = "$value" ] && { printf '%s\n' "$section"; return 0; }
				;;
		esac
	done <"$config_dir/$package"
	return 1
}

assert_value() {
	config_dir=$1
	key=$2
	expected=$3
	actual=$(get_value "$config_dir" "$key" 2>/dev/null || true)
	[ "$actual" = "$expected" ] || fail "$key: expected $expected, got ${actual:-<missing>}"
}

assert_missing_value() {
	config_dir=$1
	key=$2
	if get_value "$config_dir" "$key" >/dev/null 2>&1; then
		fail "$key must be absent"
	fi
}

assert_no_lan_wan_forwarding() {
	config_dir=$1
	while IFS= read -r line; do
		case "$line" in
			firewall.*=forwarding)
				section=${line#firewall.}
				section=${section%%=*}
				src=$(get_value "$config_dir" "firewall.$section.src" 2>/dev/null || true)
				dest=$(get_value "$config_dir" "firewall.$section.dest" 2>/dev/null || true)
				[ "$src:$dest" != lan:wan ] || fail "lan -> wan forwarding survived: $section"
				;;
		esac
	done <"$config_dir/firewall"
}

assert_named_rule() {
	config_dir=$1
	section=$2
	name=$3
	proto=$4
	port=$5
	assert_value "$config_dir" "firewall.$section" rule
	assert_value "$config_dir" "firewall.$section.name" "$name"
	assert_value "$config_dir" "firewall.$section.src" lan
	assert_value "$config_dir" "firewall.$section.proto" "$proto"
	assert_value "$config_dir" "firewall.$section.dest_port" "$port"
	assert_value "$config_dir" "firewall.$section.family" ipv4
	assert_value "$config_dir" "firewall.$section.target" ACCEPT
}

assert_named_include() {
	config_dir=$1
	section=$2
	type=$3
	path=$4
	position=$5
	chain=$6
	fw4_compatible=$7
	assert_value "$config_dir" "firewall.$section" include
	assert_value "$config_dir" "firewall.$section.type" "$type"
	assert_value "$config_dir" "firewall.$section.path" "$path"
	if [ "$position" = absent ]; then
		assert_missing_value "$config_dir" "firewall.$section.position"
	else
		assert_value "$config_dir" "firewall.$section.position" "$position"
	fi
	if [ "$chain" = absent ]; then
		assert_missing_value "$config_dir" "firewall.$section.chain"
	else
		assert_value "$config_dir" "firewall.$section.chain" "$chain"
	fi
	if [ "$fw4_compatible" = absent ]; then
		assert_missing_value "$config_dir" "firewall.$section.fw4_compatible"
	else
		assert_value "$config_dir" "firewall.$section.fw4_compatible" "$fw4_compatible"
	fi
}

assert_only_named_lan_whitelist() {
	config_dir=$1
	while IFS= read -r line; do
		case "$line" in
			firewall.*=rule)
				section=${line#firewall.}
				section=${section%%=*}
				src=$(get_value "$config_dir" "firewall.$section.src" 2>/dev/null || true)
				dest=$(get_value "$config_dir" "firewall.$section.dest" 2>/dev/null || true)
				target=$(get_value "$config_dir" "firewall.$section.target" 2>/dev/null || true)
				[ "$src" = lan ] && [ -z "$dest" ] || continue
				case "$section" in
					proxypool_allow_dhcp|proxypool_allow_admin_ssh|proxypool_allow_admin_http|proxypool_allow_admin_https)
						[ "$target" = ACCEPT ] || fail "named LAN input whitelist has non-ACCEPT target: $section"
						;;
					*) fail "non-whitelisted LAN input rule survived: $section" ;;
				esac
				;;
		esac
	done <"$config_dir/firewall"
}

assert_all_offload_disabled() {
	config_dir=$1
	found=0
	while IFS= read -r line; do
		case "$line" in
			firewall.*=defaults)
				found=$((found + 1))
				section=${line#firewall.}
				section=${section%%=*}
				assert_value "$config_dir" "firewall.$section.flow_offloading" 0
				assert_value "$config_dir" "firewall.$section.flow_offloading_hw" 0
				;;
		esac
	done <"$config_dir/firewall"
	[ "$found" -gt 0 ] || fail 'firewall defaults section disappeared'
}

assert_success_state() {
	config_dir=$1
	trace=$2
	mode=${3:-live}
	lan_zone=$(find_section "$config_dir" firewall zone name lan) || fail 'LAN firewall zone disappeared'
	wan_zone=$(find_section "$config_dir" firewall zone name wan) || fail 'WAN firewall zone disappeared'
	defaults_section=$(find_section "$config_dir" firewall defaults flow_offloading 0) || fail 'firewall defaults were not found by type'
	lan_dhcp=$(find_section "$config_dir" dhcp dhcp interface lan) || fail 'LAN DHCP section disappeared'
	lan_network=$(find_section "$config_dir" network interface device br-lan) || fail 'LAN network section disappeared'

	assert_value "$config_dir" "firewall.$lan_zone.input" REJECT
	assert_value "$config_dir" "firewall.$lan_zone.forward" REJECT
	assert_value "$config_dir" "firewall.$lan_zone.output" ACCEPT
	assert_value "$config_dir" "firewall.$wan_zone.masq" 1
	assert_value "$config_dir" "firewall.$wan_zone.device" eth0
	assert_no_lan_wan_forwarding "$config_dir"
	assert_value "$config_dir" firewall.unrelated_guest_wan.src guest
	assert_value "$config_dir" firewall.unrelated_guest_wan.dest wan
	assert_value "$config_dir" firewall.unrelated_lan_guest.dest guest
	assert_value "$config_dir" firewall.third_party_rule.dest_port 22
	preserved_anonymous_forward=$(find_section "$config_dir" firewall forwarding dest iot) ||
		fail 'unrelated anonymous forwarding was removed while deleting LAN-to-WAN entries'
	assert_value "$config_dir" "firewall.$preserved_anonymous_forward.src" guest
	preserved_anonymous_rule=$(find_section "$config_dir" firewall rule name 'Preserve anonymous guest accept') ||
		fail 'unrelated anonymous LAN-input rule was removed while converging the whitelist'
	assert_value "$config_dir" "firewall.$preserved_anonymous_rule.dest_port" 9443
	for forbidden_include in /usr/lib/third-party/preserve.nft /usr/lib/third-party/post-fw4.sh; do
		if find_section "$config_dir" firewall include path "$forbidden_include" >/dev/null 2>&1; then
			fail "non-owned firewall include survived appliance convergence: $forbidden_include"
		fi
	done
	if get_value "$config_dir" firewall.legacy_lan_ssh.dest_port >/dev/null 2>&1; then
		fail 'legacy LAN SSH allow rule survived management whitelist convergence'
	fi
	if get_value "$config_dir" firewall.legacy_lan_input_reject.target >/dev/null 2>&1; then
		fail 'early legacy LAN input reject survived and can shadow the management whitelist'
	fi
	for forbidden_rule_name in \
		'Anonymous lowercase LAN accept' \
		'Anonymous uppercase LAN accept' \
		'Anonymous abbreviated LAN accept' \
		'Anonymous empty-prefix LAN accept'; do
		if find_section "$config_dir" firewall rule name "$forbidden_rule_name" >/dev/null 2>&1; then
			fail "fw4 ACCEPT-prefix LAN rule survived whitelist convergence: $forbidden_rule_name"
		fi
	done

	assert_named_rule "$config_dir" proxypool_allow_dhcp 'ProxyPool Allow DHCP' udp 67
	assert_named_rule "$config_dir" proxypool_allow_admin_ssh 'ProxyPool Allow SSH Management' tcp 22
	assert_named_rule "$config_dir" proxypool_allow_admin_http 'ProxyPool Allow HTTP Management' tcp 80
	assert_named_rule "$config_dir" proxypool_allow_admin_https 'ProxyPool Allow HTTPS Management' tcp 443
	assert_value "$config_dir" firewall.proxypool_allow_dhcp.src_port 68
	for section in proxypool_allow_admin_ssh proxypool_allow_admin_http proxypool_allow_admin_https; do
		assert_value "$config_dir" "firewall.$section.dest_ip" 192.168.9.1
	done
	assert_missing_value "$config_dir" firewall.proxypool_allow_dhcp.dest_ip
	assert_missing_value "$config_dir" firewall.proxypool_allow_dns
	assert_only_named_lan_whitelist "$config_dir"
	[ "$(get_value "$config_dir" firewall.proxypool_allow_admin_ssh.dest_port)" = 22 ] ||
		fail 'ProxyPool SSH management rule does not expose TCP 22'

	assert_value "$config_dir" "firewall.$defaults_section.flow_offloading" 0
	assert_all_offload_disabled "$config_dir"
	assert_value "$config_dir" "firewall.$defaults_section.auto_includes" 0
	assert_value "$config_dir" "dhcp.$lan_dhcp.ra" disabled
	assert_value "$config_dir" "dhcp.$lan_dhcp.dhcpv6" disabled
	assert_value "$config_dir" "dhcp.$lan_dhcp.ndp" disabled
	assert_value "$config_dir" "dhcp.$lan_dhcp.dhcp_option" 6,192.168.9.1
	assert_value "$config_dir" dhcp.dnsmasq_main.noresolv 1
	assert_value "$config_dir" dhcp.dnsmasq_main.port 0
	assert_missing_value "$config_dir" dhcp.dnsmasq_main.server
	assert_value "$config_dir" "network.$lan_network.delegate" 0
	assert_missing_value "$config_dir" "network.$lan_network.ip6assign"
	assert_missing_value "$config_dir" "network.$lan_network.ip6hint"
	assert_missing_value "$config_dir" "network.$lan_network.ip6class"
	assert_value "$config_dir" network.lan1_physical.name lan1
	assert_value "$config_dir" network.lan1_physical.isolate 1
	assert_value "$config_dir" network.lan1_physical.mtu 1500
	assert_value "$config_dir" network.proxypool_lan_port_02 device
	assert_value "$config_dir" network.proxypool_lan_port_02.name lan2
	assert_value "$config_dir" network.proxypool_lan_port_02.isolate 1
	assert_value "$config_dir" network.foreign_uplink.mtu 1492
	assert_value "$config_dir" dhcp.guest.ra server
	assert_value "$config_dir" network.guest.delegate 1

	[ "$(grep -Ec '^firewall\.proxypool_[^.]*=include$' "$config_dir/firewall")" -eq 4 ] ||
		fail 'firewall must contain exactly four named ProxyPool include sections'
	[ "$(grep -Ec '^firewall\.[^=]*=include$' "$config_dir/firewall")" -eq 4 ] ||
		fail 'strict appliance firewall retained a non-ProxyPool include section'
	assert_named_include "$config_dir" proxypool_guard nftables \
		/usr/lib/proxypool/proxypool-guard.nft ruleset-prepend absent absent
	assert_named_include "$config_dir" proxypool_fw4_input_gate nftables \
		/usr/lib/proxypool/proxypool-fw4-input-gate.nft chain-prepend input absent
	assert_named_include "$config_dir" proxypool_fw4_forward_gate nftables \
		/usr/lib/proxypool/proxypool-fw4-forward-gate.nft chain-prepend forward absent
	assert_named_include "$config_dir" proxypool_guard_resync script \
		/usr/lib/proxypool/guard-resync.sh absent absent 1

	if grep -Eiq 'ppp-[+*]|\.dest=(tunnel|proxypool)' "$config_dir/firewall"; then
		fail 'defaults retained a wildcard PPP device or ProxyPool tunnel forwarding'
	fi
	case "$mode" in
		live)
			[ "$(grep -c '^firewall:reload$' "$trace")" -eq 1 ] || fail 'successful live transaction did not activate firewall exactly once'
			activation_marker="$config_dir/.proxypool-state/firewall-safety-activated"
			[ -f "$activation_marker" ] && [ ! -L "$activation_marker" ] ||
				fail 'successful live activation did not publish its authority marker'
			[ "$(wc -l <"$activation_marker" | tr -d '[:space:]')" = 5 ] &&
				grep -Fxq 'schema_version=2' "$activation_marker" &&
				grep -Fxq 'projection_schema=2' "$activation_marker" &&
				grep -Fxq 'contract_schema=4' "$activation_marker" ||
				fail 'live activation marker has an invalid schema'
			for digest_key in projection_digest contract_digest; do
				digest=$(sed -n "s/^$digest_key=//p" "$activation_marker")
				[ "${#digest}" -eq 64 ] && printf '%s\n' "$digest" | grep -Eq '^[0-9A-Fa-f]{64}$' ||
					fail "activation marker has an invalid $digest_key"
			done
		;;
		cold)
			[ "$(grep -c '^firewall:reload$' "$trace" 2>/dev/null || true)" -eq 0 ] || fail 'cold transaction attempted a firewall reload'
			[ ! -e "$config_dir/.proxypool-state/firewall-safety-activated" ] &&
				[ ! -L "$config_dir/.proxypool-state/firewall-safety-activated" ] ||
				fail 'cold transaction published activation authority before S19 runtime proof'
		;;
		*) fail "unknown success assertion mode: $mode" ;;
	esac
	[ "$(grep -c '^guardian:reset-empty$' "$trace")" -ge 1 ] || fail 'transaction did not establish an empty guardian first'
	[ "$(grep -c '^fw4:check:' "$trace")" -eq 1 ] || fail 'transaction did not validate exactly one staged ruleset'
	[ "$(grep -c '^config:install:' "$trace")" -eq 3 ] || fail 'successful transaction did not install exactly three validated config files'
	probe_count=$(grep -c '^flowtable:probe:' "$trace")
	case "$mode:$probe_count" in
		live:4|cold:3) : ;;
		*) fail "transaction executed the wrong number of flowtable proofs (mode=$mode count=$probe_count)" ;;
	esac
	[ "$(grep -c '^uci:pending-probe:0$' "$trace")" -eq 1 ] || fail 'transaction did not reject pending UCI delta before mutation'
	[ "$(grep -c "^fw4:mode-probe:$mode:0$" "$trace")" -eq 1 ] || fail "$mode transaction did not verify fw4 state/table mode"
	[ "$(grep -c '^uci:clamp-offload:' "$trace")" -eq 1 ] || fail 'transaction did not perform exactly one one-way offload clamp'
	[ "$(grep -c '^uci:apply-defaults:' "$trace")" -eq 1 ] || fail 'transaction did not perform exactly one isolated staged mutation'
	[ "$(grep -c '^lock:transaction:acquire$' "$trace")" -eq 1 ] || fail 'transaction lock was not acquired exactly once'
	reset_line=$(grep -n '^guardian:reset-empty$' "$trace" | sed -n '1s/:.*//p')
	clamp_line=$(grep -n '^uci:clamp-offload:' "$trace" | sed -n '1s/:.*//p')
	check_line=$(grep -n '^fw4:check:' "$trace" | sed -n '1s/:.*//p')
	install_line=$(grep -n '^config:install:' "$trace" | sed -n '1s/:.*//p')
	probe_one_line=$(grep -n '^flowtable:probe:1:' "$trace" | sed -n '1s/:.*//p')
	probe_two_line=$(grep -n '^flowtable:probe:2:' "$trace" | sed -n '1s/:.*//p')
	probe_three_line=$(grep -n '^flowtable:probe:3:' "$trace" | sed -n '1s/:.*//p')
		[ "$probe_one_line" -lt "$probe_two_line" ] && [ "$probe_two_line" -lt "$reset_line" ] && \
		[ "$reset_line" -lt "$clamp_line" ] && [ "$clamp_line" -lt "$check_line" ] && \
		[ "$check_line" -lt "$probe_three_line" ] && [ "$probe_three_line" -lt "$install_line" ] && \
		[ "$check_line" -lt "$install_line" ] ||
		fail 'transaction order is not clamp -> empty guardian -> staged check -> install -> activation'
	if [ "$mode" = live ]; then
		reload_line=$(grep -n '^firewall:reload$' "$trace" | sed -n '1s/:.*//p')
		probe_four_line=$(grep -n '^flowtable:probe:4:' "$trace" | sed -n '1s/:.*//p')
		[ "$install_line" -lt "$reload_line" ] || fail 'live activation ran before config installation'
		[ "$reload_line" -lt "$probe_four_line" ] ||
			fail 'live activation retired its journal without a post-apply flowtable proof'
	fi
}

run_defaults() {
	config_dir=$1
	trace=$2
	shift 2
	: >"$trace"
	original_snapshot="$trace.original"
	clamped_snapshot="$trace.clamped"
	mkdir "$original_snapshot"
	mkdir "$clamped_snapshot"
	cp "$config_dir/firewall" "$config_dir/dhcp" "$config_dir/network" "$original_snapshot/"
	cp "$config_dir/firewall" "$config_dir/dhcp" "$config_dir/network" "$clamped_snapshot/"
	env \
		PATH="$BIN:$PATH" \
		PROXYPOOL_CONFIG_DIR="$config_dir" \
		PROXYPOOL_UCI="$BIN/uci" \
		PROXYPOOL_UCI_STAGED_APPLY="$BIN/staged-apply" \
		PROXYPOOL_TEST_FAKE_UCI="$BIN/uci" \
		PROXYPOOL_FLOWTABLE_PROBE="$BIN/flowtable-probe" \
		PROXYPOOL_PENDING_DELTA_PROBE="$BIN/pending-delta-probe" \
		PROXYPOOL_FW4_MODE_PROBE="$BIN/fw4-mode-probe" \
		PROXYPOOL_FLOCK="$BIN/flock" \
		PROXYPOOL_TRANSACTION_LOCK="$TEST_TMP/proxypool-firewall.lock" \
		PROXYPOOL_FW4_LOCK="$TEST_TMP/fw4.lock" \
		PROXYPOOL_FIREWALL_TRANSACTION_DIR="${PROXYPOOL_TEST_TRANSACTION_DIR:-$config_dir/.proxypool-state/firewall-transaction}" \
		PROXYPOOL_FIREWALL_ACTIVATION_MARKER="${PROXYPOOL_TEST_ACTIVATION_MARKER:-$config_dir/.proxypool-state/firewall-safety-activated}" \
		PROXYPOOL_TRANSACTION_HELPER="$TRANSACTION_HELPER" \
		PROXYPOOL_LS_PROG="$BIN/ls" \
		PROXYPOOL_GUARD_CONTRACT_FILE="$GUARD_CONTRACT_FILE" \
		PROXYPOOL_FW4_INPUT_GATE_CONTRACT_FILE="$INPUT_GATE_CONTRACT_FILE" \
		PROXYPOOL_FW4_FORWARD_GATE_CONTRACT_FILE="$FORWARD_GATE_CONTRACT_FILE" \
		PROXYPOOL_LAN_ISOLATION_CONTRACT_FILE="$LAN_ISOLATION_CONTRACT_FILE" \
		PROXYPOOL_LAN_HOTPLUG_CONTRACT_FILE="$LAN_HOTPLUG_CONTRACT_FILE" \
		PROXYPOOL_LAN_IFACE_HOTPLUG_CONTRACT_FILE="$LAN_IFACE_HOTPLUG_CONTRACT_FILE" \
		PROXYPOOL_LAN_WORKER_CONTRACT_FILE="$LAN_WORKER_CONTRACT_FILE" \
		PROXYPOOL_GUARD_INIT_CONTRACT_FILE="$GUARD_INIT_CONTRACT_FILE" \
		PROXYPOOL_UCI_STAGED_CONTRACT_FILE="$UCI_STAGED_CONTRACT_FILE" \
		PROXYPOOL_LEGACY_GATE_CONTRACT_FILE="$LEGACY_GATE_CONTRACT_FILE" \
		PROXYPOOL_TEST_CONTRACT_FILES="$CONTRACT_FILES" \
		PROXYPOOL_TEST_EXEC_CONTRACT_FILES="$EXEC_CONTRACT_FILES" \
		PROXYPOOL_NFT="$BIN/nft-runtime" \
		PROXYPOOL_BOOT_ID_FILE="${PROXYPOOL_TEST_BOOT_ID_FILE:-$BOOT_ID_FILE}" \
		PROXYPOOL_NFTABLES_USER_DIR="$NFTABLES_USER_DIR" \
		PROXYPOOL_SYNC="$BIN/sync" \
		PROXYPOOL_FW4_CHECK="$BIN/fw4-check-staged" \
		PROXYPOOL_CONFIG_INSTALL="$BIN/config-install" \
		PROXYPOOL_FW4_ACTIVATOR="$BIN/fw4-activate" \
		PROXYPOOL_GUARD_RESET="$BIN/guard-reset" \
		PROXYPOOL_TEST_TRACE="$trace" \
		PROXYPOOL_TEST_ORIGINAL_SNAPSHOT="$original_snapshot" \
		PROXYPOOL_TEST_CLAMPED_SNAPSHOT="$clamped_snapshot" \
		"$@" \
		sh "$DEFAULTS"
}

assert_unchanged() {
	before=$1
	after=$2
	for package in firewall dhcp network; do
		cmp -s "$before/$package" "$after/$package" ||
			fail "failed transaction mutated original $package configuration"
	done
}

assert_clamped_baseline() {
	clamped=$1
	after=$2
	for package in firewall dhcp network; do
		cmp -s "$clamped/$package" "$after/$package" ||
			fail "failed transaction did not restore clamp-safe $package baseline"
	done
}

assert_no_managed_mutation_trace() {
	trace=$1
	if grep -Eq '^(guardian:|uci:(clamp-offload|apply-defaults):|fw4:check:|config:install:|firewall:reload$|storage:sync$)' "$trace"; then
		fail 'read-only preflight failure performed a managed mutation'
	fi
}

assert_compensating_install_rollback() {
	trace=$1
	fail_at=$2
	expected_installs=$((fail_at + 3))
	rollback_start=$((fail_at + 1))
	rollback_second=$((fail_at + 2))
	rollback_third=$((fail_at + 3))

	[ "$(grep -c '^config:install:' "$trace" 2>/dev/null || true)" -eq "$expected_installs" ] ||
		fail "config install failure $fail_at did not perform exactly one complete three-file rollback"
	[ "$(grep '^config:install:' "$trace" | sed -n "${rollback_start}p")" = 'config:install:firewall' ] &&
		[ "$(grep '^config:install:' "$trace" | sed -n "${rollback_second}p")" = 'config:install:dhcp' ] &&
		[ "$(grep '^config:install:' "$trace" | sed -n "${rollback_third}p")" = 'config:install:network' ] ||
		fail "config install failure $fail_at did not restore all managed files in order"
	[ "$(grep -c '^firewall:reload$' "$trace" 2>/dev/null || true)" -eq 1 ] ||
		fail "config install failure $fail_at did not perform exactly one compensating firewall reload"
	last_install_line=$(grep -n '^config:install:' "$trace" | sed -n "${expected_installs}s/:.*//p")
	reload_line=$(grep -n '^firewall:reload$' "$trace" | sed -n '1s/:.*//p')
	[ -n "$last_install_line" ] && [ -n "$reload_line" ] && [ "$last_install_line" -lt "$reload_line" ] ||
		fail "config install failure $fail_at reloaded before the complete baseline was restored"
}

run_sigkill_recovery_case() {
	kill_after=$1
	expected_dir=$2
	config_dir="$TEST_TMP/sigkill-after-$kill_after"
	first_trace="$TEST_TMP/sigkill-after-$kill_after.first.trace"
	recovery_trace="$TEST_TMP/sigkill-after-$kill_after.recovery.trace"
	transaction_dir="$config_dir/.proxypool-state/firewall-transaction"
	write_fixture "$config_dir" wan_first
	if run_defaults "$config_dir" "$first_trace" \
		PROXYPOOL_TEST_INSTALL_KILL_AFTER_AT="$kill_after" \
		>"$TEST_TMP/sigkill-after-$kill_after.first.log" 2>&1; then
		fail "defaults survived injected SIGKILL after install $kill_after"
	fi
	[ -d "$transaction_dir" ] || fail "SIGKILL after install $kill_after lost the persistent journal"
	if ! run_defaults "$config_dir" "$recovery_trace" \
		PROXYPOOL_TEST_ORIGINAL_SNAPSHOT="$first_trace.original" \
		PROXYPOOL_TEST_CLAMPED_SNAPSHOT="$first_trace.clamped" \
		>"$TEST_TMP/sigkill-after-$kill_after.recovery.log" 2>&1; then
		cat "$TEST_TMP/sigkill-after-$kill_after.recovery.log" >&2
		fail "next invocation could not recover SIGKILL after install $kill_after"
	fi
	for package in firewall dhcp network; do
		if ! cmp -s "$expected_dir/$package" "$config_dir/$package"; then
			diff -u "$expected_dir/$package" "$config_dir/$package" >&2 || true
			fail "SIGKILL recovery $kill_after did not converge $package"
		fi
	done
	[ ! -e "$transaction_dir" ] || fail "successful SIGKILL recovery $kill_after retained stale journal"
	[ "$(grep -c '^firewall:reload$' "$recovery_trace" 2>/dev/null || true)" -eq 1 ] ||
		fail "SIGKILL recovery $kill_after activated an unexpected number of rulesets"
}

run_incomplete_rollback_recovery_case() {
	expected_dir=$1
	config_dir="$TEST_TMP/rollback-incomplete"
	first_trace="$TEST_TMP/rollback-incomplete.first.trace"
	recovery_trace="$TEST_TMP/rollback-incomplete.recovery.trace"
	transaction_dir="$config_dir/.proxypool-state/firewall-transaction"
	write_fixture "$config_dir" lan_first
	# Attempt 2 fails the candidate install. Attempts 3 and 6 fail the first
	# file of both the immediate rollback and the EXIT-trap retry, proving that
	# an actually incomplete recovery retains its only durable journal.
	if run_defaults "$config_dir" "$first_trace" PROXYPOOL_TEST_INSTALL_FAIL_COUNTS=2,3,6 \
		>"$TEST_TMP/rollback-incomplete.first.log" 2>&1; then
		fail 'defaults reported success after install and both rollback attempts failed'
	fi
	[ -d "$transaction_dir" ] || fail 'incomplete rollback deleted the only persistent recovery journal'
	[ "$(grep -c '^config:install:' "$first_trace" 2>/dev/null || true)" -eq 8 ] ||
		fail 'incomplete rollback fixture did not exercise both three-file recovery attempts'
	if ! run_defaults "$config_dir" "$recovery_trace" \
		PROXYPOOL_TEST_ORIGINAL_SNAPSHOT="$first_trace.original" \
		PROXYPOOL_TEST_CLAMPED_SNAPSHOT="$first_trace.clamped" \
		>"$TEST_TMP/rollback-incomplete.recovery.log" 2>&1; then
		cat "$TEST_TMP/rollback-incomplete.recovery.log" >&2
		fail 'next invocation could not recover an incomplete rollback'
	fi
	for package in firewall dhcp network; do
		if ! cmp -s "$expected_dir/$package" "$config_dir/$package"; then
			diff -u "$expected_dir/$package" "$config_dir/$package" >&2 || true
			fail "incomplete rollback recovery did not converge $package"
		fi
	done
	[ ! -e "$transaction_dir" ] || fail 'recovered rollback retained stale transaction journal'
}

run_post_apply_flowtable_case() {
	config_dir="$TEST_TMP/runtime-flowtable-after-apply"
	trace="$TEST_TMP/runtime-flowtable-after-apply.trace"
	transaction_dir="$config_dir/.proxypool-state/firewall-transaction"
	write_fixture "$config_dir" wan_first
	if run_defaults "$config_dir" "$trace" PROXYPOOL_TEST_FLOWTABLE_SEQUENCE=1,1,1,0 \
		>"$TEST_TMP/runtime-flowtable-after-apply.log" 2>&1; then
		fail 'defaults accepted a flowtable discovered by post-apply runtime proof'
	fi
	assert_clamped_baseline "$trace.clamped" "$config_dir"
	[ -d "$transaction_dir" ] || fail 'post-apply flowtable failure lost its rollback WAL'
	grep -Fxq rollback-awaiting-fw4-start "$transaction_dir/state" ||
		fail 'post-apply flowtable failure retained the wrong WAL state'
	[ "$(grep -c '^firewall:reload$' "$trace" 2>/dev/null || true)" -eq 2 ] || {
		cat "$trace" >&2
		fail 'post-apply flowtable failure did not perform exactly one candidate and one compensation activation'
	}
	[ "$(grep -c '^flowtable:probe:' "$trace" 2>/dev/null || true)" -eq 5 ] ||
		fail 'post-apply flowtable failure did not exercise candidate and compensation runtime proofs'
	[ ! -e "$config_dir/.proxypool-state/firewall-safety-activated" ] ||
		fail 'post-apply flowtable failure published activation authority'
}

if [ "${PROXYPOOL_TEST_FOCUS_INSTALL_FAILURE:-0}" = 1 ]; then
	for fail_at in 2 3; do
		config_dir="$TEST_TMP/focused-fail-install-$fail_at"
		trace="$TEST_TMP/focused-fail-install-$fail_at.trace"
		write_fixture "$config_dir" wan_first
		if run_defaults "$config_dir" "$trace" PROXYPOOL_TEST_INSTALL_FAIL_AT="$fail_at" \
			>"$TEST_TMP/focused-fail-install-$fail_at.log" 2>&1; then
			fail "focused defaults reported success after config install $fail_at failed"
		fi
		assert_clamped_baseline "$trace.clamped" "$config_dir"
		assert_compensating_install_rollback "$trace" "$fail_at"
	done
	echo 'ProxyPool focused install-failure rollback: PASS'
	exit 0
fi

if [ "${PROXYPOOL_TEST_FOCUS_POST_APPLY_FLOWTABLE:-0}" = 1 ]; then
	run_post_apply_flowtable_case
	echo 'ProxyPool focused post-apply flowtable recovery: PASS'
	exit 0
fi

# These gates precede guardian reset, the one-way offload clamp, persistent
# journal creation, staging, and every live config replacement.
if [ "${PROXYPOOL_TEST_FOCUS_SUCCESS:-0}" != 1 ] &&
	[ "${PROXYPOOL_TEST_FOCUS_ACTIVATION:-0}" != 1 ] &&
	[ "${PROXYPOOL_TEST_FOCUS_ACTIVATION_HELPER:-0}" != 1 ]; then
for preflight_case in active_flowtable unknown_flowtable pending_delta unknown_delta fw4_mode_mismatch tx_lock_busy unowned_nft_file; do
	config_dir="$TEST_TMP/preflight-$preflight_case"
	before="$TEST_TMP/preflight-$preflight_case.before"
	trace="$TEST_TMP/preflight-$preflight_case.trace"
	write_fixture "$config_dir" wan_first
	mkdir "$before"
	cp "$config_dir/firewall" "$config_dir/dhcp" "$config_dir/network" "$before/"
	case "$preflight_case" in
		active_flowtable) knobs='PROXYPOOL_TEST_FLOWTABLE_SEQUENCE=0' ;;
		unknown_flowtable) knobs='PROXYPOOL_TEST_FLOWTABLE_SEQUENCE=2' ;;
		pending_delta) knobs='PROXYPOOL_TEST_PENDING_DELTA_STATUS=1' ;;
		unknown_delta) knobs='PROXYPOOL_TEST_PENDING_DELTA_STATUS=2' ;;
		fw4_mode_mismatch) knobs='PROXYPOOL_TEST_FW4_MODE_STATUS=1' ;;
		tx_lock_busy) knobs='PROXYPOOL_TEST_FLOCK_FAIL_KIND=transaction' ;;
		unowned_nft_file)
			malicious_nft_dir="$TEST_TMP/preflight-unowned-nftables.d"
			mkdir -p "$malicious_nft_dir"
			printf '%s\n' 'flowtable bypass { hook ingress priority 0; devices = { eth0 }; }' >"$malicious_nft_dir/bypass.nft"
			knobs="PROXYPOOL_NFTABLES_USER_DIR=$malicious_nft_dir"
			;;
	esac
	if run_defaults "$config_dir" "$trace" "$knobs" >"$TEST_TMP/preflight-$preflight_case.log" 2>&1; then
		fail "defaults accepted unsafe preflight state: $preflight_case"
	fi
	assert_unchanged "$before" "$config_dir"
	assert_no_managed_mutation_trace "$trace"
	[ ! -e "$config_dir/.proxypool-state/firewall-transaction" ] ||
		fail "preflight $preflight_case created a persistent transaction journal"
done

for mutation_probe_case in active unknown; do
	config_dir="$TEST_TMP/pre-mutation-probe-$mutation_probe_case"
	before="$TEST_TMP/pre-mutation-probe-$mutation_probe_case.before"
	trace="$TEST_TMP/pre-mutation-probe-$mutation_probe_case.trace"
	write_fixture "$config_dir" lan_first
	mkdir "$before"
	cp "$config_dir/firewall" "$config_dir/dhcp" "$config_dir/network" "$before/"
	case "$mutation_probe_case" in active) sequence=1,0 ;; unknown) sequence=1,2 ;; esac
	if run_defaults "$config_dir" "$trace" PROXYPOOL_TEST_FLOWTABLE_SEQUENCE="$sequence" \
		>"$TEST_TMP/pre-mutation-probe-$mutation_probe_case.log" 2>&1; then
		fail "defaults accepted $mutation_probe_case flowtable result before first mutation"
	fi
	assert_unchanged "$before" "$config_dir"
	assert_no_managed_mutation_trace "$trace"
done

for install_probe_case in active unknown; do
	config_dir="$TEST_TMP/pre-install-probe-$install_probe_case"
	trace="$TEST_TMP/pre-install-probe-$install_probe_case.trace"
	write_fixture "$config_dir" wan_first
	case "$install_probe_case" in active) sequence=1,1,0 ;; unknown) sequence=1,1,2 ;; esac
	if run_defaults "$config_dir" "$trace" PROXYPOOL_TEST_FLOWTABLE_SEQUENCE="$sequence" \
		>"$TEST_TMP/pre-install-probe-$install_probe_case.log" 2>&1; then
		fail "defaults accepted $install_probe_case flowtable result before live install"
	fi
	assert_clamped_baseline "$trace.clamped" "$config_dir"
	if [ "$(grep -c '^guardian:reset-empty$' "$trace" 2>/dev/null || true)" -lt 1 ]; then
		cat "$trace" >&2
		cat "$TEST_TMP/pre-install-probe-$install_probe_case.log" >&2
		fail "pre-install $install_probe_case did not retain an empty guardian"
	fi
	[ "$(grep -c '^config:install:' "$trace" 2>/dev/null || true)" -eq 0 ] ||
		fail "pre-install $install_probe_case still replaced live configuration"
	[ "$(grep -c '^firewall:reload$' "$trace" 2>/dev/null || true)" -eq 0 ] ||
		fail "pre-install $install_probe_case still activated firewall"
done

if [ "${PROXYPOOL_TEST_FOCUS_PREFLIGHT:-0}" = 1 ]; then
	echo 'ProxyPool focused preflight flowtable boundaries: PASS'
	exit 0
fi

# The empty guardian is the first conversion mutation.  If it fails, every
# managed byte remains untouched.  If its atomic nft child succeeds but kills
# the parent, no marker/clamp/WAL mutation has happened and the network verdict
# is already empty/fail-closed.
for guard_boundary in failure parent_killed; do
	config_dir="$TEST_TMP/guard-boundary-$guard_boundary"
	before="$TEST_TMP/guard-boundary-$guard_boundary.before"
	trace="$TEST_TMP/guard-boundary-$guard_boundary.trace"
	write_fixture "$config_dir" wan_first
	mkdir "$before"
	cp "$config_dir/firewall" "$config_dir/dhcp" "$config_dir/network" "$before/"
	case "$guard_boundary" in
		failure) knob=PROXYPOOL_TEST_GUARD_RESET_RC=77 ;;
		parent_killed) knob=PROXYPOOL_TEST_GUARD_KILL_PARENT=1 ;;
	esac
	if run_defaults "$config_dir" "$trace" "$knob" >"$TEST_TMP/guard-boundary-$guard_boundary.log" 2>&1; then
		fail "defaults accepted guardian boundary $guard_boundary"
	fi
	assert_unchanged "$before" "$config_dir"
	[ "$(grep -c '^flowtable:probe:' "$trace" 2>/dev/null || true)" -eq 2 ] ||
		fail "guardian boundary $guard_boundary did not occur immediately after probe two"
	[ "$(grep -c '^guardian:reset-empty$' "$trace" 2>/dev/null || true)" -eq 1 ] ||
		fail "guardian boundary $guard_boundary did not exercise exactly one empty reset"
	[ "$(grep -c '^uci:clamp-offload:' "$trace" 2>/dev/null || true)" -eq 0 ] ||
		fail "guardian boundary $guard_boundary reached the one-way clamp"
	[ ! -e "$config_dir/.proxypool-state/firewall-transaction" ] ||
		fail "guardian boundary $guard_boundary published a premature WAL"
done
fi

# Both layouts contain extra zones and duplicate legacy forwardings.  Their
# order differs so numeric/anonymous UCI indices cannot satisfy both cases.
activation_current_for_fixture() {
	config_dir=$1
	contract_metadata_fault=${2:-}
	env \
		PATH="$BIN:$PATH" \
		PROXYPOOL_CONFIG_DIR="$config_dir" \
		PROXYPOOL_UCI="$BIN/uci" \
		PROXYPOOL_FIREWALL_TRANSACTION_DIR="$config_dir/.proxypool-state/firewall-transaction" \
		PROXYPOOL_FIREWALL_ACTIVATION_MARKER="$config_dir/.proxypool-state/firewall-safety-activated" \
		PROXYPOOL_TRANSACTION_LOCK="$TEST_TMP/proxypool-firewall.lock" \
		PROXYPOOL_FW4_LOCK="$TEST_TMP/fw4.lock" \
		PROXYPOOL_FW4_STATE="$TEST_TMP/fw4.state" \
		PROXYPOOL_BOOT_ID_FILE="$BOOT_ID_FILE" \
		PROXYPOOL_LS_PROG="$BIN/ls" \
		PROXYPOOL_GUARD_CONTRACT_FILE="$GUARD_CONTRACT_FILE" \
		PROXYPOOL_FW4_INPUT_GATE_CONTRACT_FILE="$INPUT_GATE_CONTRACT_FILE" \
		PROXYPOOL_FW4_FORWARD_GATE_CONTRACT_FILE="$FORWARD_GATE_CONTRACT_FILE" \
		PROXYPOOL_LAN_ISOLATION_CONTRACT_FILE="$LAN_ISOLATION_CONTRACT_FILE" \
		PROXYPOOL_LAN_HOTPLUG_CONTRACT_FILE="$LAN_HOTPLUG_CONTRACT_FILE" \
		PROXYPOOL_LAN_IFACE_HOTPLUG_CONTRACT_FILE="$LAN_IFACE_HOTPLUG_CONTRACT_FILE" \
		PROXYPOOL_LAN_WORKER_CONTRACT_FILE="$LAN_WORKER_CONTRACT_FILE" \
		PROXYPOOL_GUARD_INIT_CONTRACT_FILE="$GUARD_INIT_CONTRACT_FILE" \
		PROXYPOOL_UCI_STAGED_CONTRACT_FILE="$UCI_STAGED_CONTRACT_FILE" \
		PROXYPOOL_LEGACY_GATE_CONTRACT_FILE="$LEGACY_GATE_CONTRACT_FILE" \
		PROXYPOOL_TEST_CONTRACT_FILES="$CONTRACT_FILES" \
		PROXYPOOL_TEST_EXEC_CONTRACT_FILES="$EXEC_CONTRACT_FILES" \
		PROXYPOOL_TEST_CONTRACT_METADATA_FAULT="$contract_metadata_fault" \
		PROXYPOOL_SYNC="$BIN/sync" \
		PROXYPOOL_TEST_TRACE="$TEST_TMP/activation-current.trace" \
		"$TRANSACTION_HELPER" activation-current
}

activation_runtime_current_for_fixture() {
	config_dir=$1
	flowtable_status=$2
	runtime_fault=${3:-}
	runtime_trace="$TEST_TMP/activation-runtime-current.trace"
	: >"$runtime_trace"
	env \
		PATH="$BIN:$PATH" \
		PROXYPOOL_CONFIG_DIR="$config_dir" \
		PROXYPOOL_UCI="$BIN/uci" \
		PROXYPOOL_FIREWALL_TRANSACTION_DIR="$config_dir/.proxypool-state/firewall-transaction" \
		PROXYPOOL_FIREWALL_ACTIVATION_MARKER="$config_dir/.proxypool-state/firewall-safety-activated" \
		PROXYPOOL_TRANSACTION_LOCK="$TEST_TMP/proxypool-firewall.lock" \
		PROXYPOOL_FW4_LOCK="$TEST_TMP/fw4.lock" \
		PROXYPOOL_FW4_STATE="$TEST_TMP/fw4.state" \
		PROXYPOOL_BOOT_ID_FILE="$BOOT_ID_FILE" \
		PROXYPOOL_LS_PROG="$BIN/ls" \
		PROXYPOOL_GUARD_CONTRACT_FILE="$GUARD_CONTRACT_FILE" \
		PROXYPOOL_FW4_INPUT_GATE_CONTRACT_FILE="$INPUT_GATE_CONTRACT_FILE" \
		PROXYPOOL_FW4_FORWARD_GATE_CONTRACT_FILE="$FORWARD_GATE_CONTRACT_FILE" \
		PROXYPOOL_LAN_ISOLATION_CONTRACT_FILE="$LAN_ISOLATION_CONTRACT_FILE" \
		PROXYPOOL_LAN_HOTPLUG_CONTRACT_FILE="$LAN_HOTPLUG_CONTRACT_FILE" \
		PROXYPOOL_LAN_IFACE_HOTPLUG_CONTRACT_FILE="$LAN_IFACE_HOTPLUG_CONTRACT_FILE" \
		PROXYPOOL_LAN_WORKER_CONTRACT_FILE="$LAN_WORKER_CONTRACT_FILE" \
		PROXYPOOL_GUARD_INIT_CONTRACT_FILE="$GUARD_INIT_CONTRACT_FILE" \
		PROXYPOOL_UCI_STAGED_CONTRACT_FILE="$UCI_STAGED_CONTRACT_FILE" \
		PROXYPOOL_LEGACY_GATE_CONTRACT_FILE="$LEGACY_GATE_CONTRACT_FILE" \
		PROXYPOOL_TEST_CONTRACT_FILES="$CONTRACT_FILES" \
		PROXYPOOL_TEST_EXEC_CONTRACT_FILES="$EXEC_CONTRACT_FILES" \
		PROXYPOOL_SYNC="$BIN/sync" \
		PROXYPOOL_NFT="$BIN/nft-runtime" \
		PROXYPOOL_FLOWTABLE_PROBE="$BIN/flowtable-probe" \
		PROXYPOOL_TEST_FLOWTABLE_SEQUENCE="$flowtable_status" \
		PROXYPOOL_TEST_RUNTIME_FAULT="$runtime_fault" \
		PROXYPOOL_TEST_TRACE="$runtime_trace" \
		"$TRANSACTION_HELPER" activation-runtime-current
}

if [ "${PROXYPOOL_TEST_FOCUS_ACTIVATION_HELPER:-0}" = 1 ]; then
	focus_activation_case=${PROXYPOOL_TEST_FOCUS_ACTIVATION_CASE:-all}
	config_dir="$TEST_TMP/activation-helper"
	trace="$TEST_TMP/activation-helper.trace"
	mkdir "$config_dir"
	cat >"$config_dir/firewall" <<'FOCUSED_FIREWALL'
firewall.defaults_main=defaults
firewall.defaults_main.flow_offloading=0
firewall.defaults_main.flow_offloading_hw=0
firewall.defaults_main.auto_includes=0
firewall.lan_zone=zone
firewall.lan_zone.name=lan
firewall.lan_zone.input=REJECT
firewall.lan_zone.forward=REJECT
firewall.lan_zone.output=ACCEPT
firewall.proxypool_guard=include
firewall.proxypool_guard.type=nftables
firewall.proxypool_guard.path=/usr/lib/proxypool/proxypool-guard.nft
firewall.proxypool_guard.position=ruleset-prepend
firewall.proxypool_fw4_input_gate=include
firewall.proxypool_fw4_input_gate.type=nftables
firewall.proxypool_fw4_input_gate.path=/usr/lib/proxypool/proxypool-fw4-input-gate.nft
firewall.proxypool_fw4_input_gate.position=chain-prepend
firewall.proxypool_fw4_input_gate.chain=input
firewall.proxypool_fw4_forward_gate=include
firewall.proxypool_fw4_forward_gate.type=nftables
firewall.proxypool_fw4_forward_gate.path=/usr/lib/proxypool/proxypool-fw4-forward-gate.nft
firewall.proxypool_fw4_forward_gate.position=chain-prepend
firewall.proxypool_fw4_forward_gate.chain=forward
firewall.proxypool_guard_resync=include
firewall.proxypool_guard_resync.type=script
firewall.proxypool_guard_resync.path=/usr/lib/proxypool/guard-resync.sh
firewall.proxypool_guard_resync.fw4_compatible=1
firewall.proxypool_allow_dhcp=rule
firewall.proxypool_allow_dhcp.src=lan
firewall.proxypool_allow_dhcp.proto=udp
firewall.proxypool_allow_dhcp.src_port=68
firewall.proxypool_allow_dhcp.dest_port=67
firewall.proxypool_allow_dhcp.family=ipv4
firewall.proxypool_allow_dhcp.target=ACCEPT
firewall.proxypool_allow_admin_ssh=rule
firewall.proxypool_allow_admin_ssh.src=lan
firewall.proxypool_allow_admin_ssh.proto=tcp
firewall.proxypool_allow_admin_ssh.dest_port=22
firewall.proxypool_allow_admin_ssh.dest_ip=192.168.9.1
firewall.proxypool_allow_admin_ssh.family=ipv4
firewall.proxypool_allow_admin_ssh.target=ACCEPT
firewall.proxypool_allow_admin_http=rule
firewall.proxypool_allow_admin_http.src=lan
firewall.proxypool_allow_admin_http.proto=tcp
firewall.proxypool_allow_admin_http.dest_port=80
firewall.proxypool_allow_admin_http.dest_ip=192.168.9.1
firewall.proxypool_allow_admin_http.family=ipv4
firewall.proxypool_allow_admin_http.target=ACCEPT
firewall.proxypool_allow_admin_https=rule
firewall.proxypool_allow_admin_https.src=lan
firewall.proxypool_allow_admin_https.proto=tcp
firewall.proxypool_allow_admin_https.dest_port=443
firewall.proxypool_allow_admin_https.dest_ip=192.168.9.1
firewall.proxypool_allow_admin_https.family=ipv4
firewall.proxypool_allow_admin_https.target=ACCEPT
FOCUSED_FIREWALL
	cat >"$config_dir/dhcp" <<'FOCUSED_DHCP'
dhcp.dnsmasq_main=dnsmasq
dhcp.dnsmasq_main.noresolv=1
dhcp.dnsmasq_main.port=0
dhcp.lan_main=dhcp
dhcp.lan_main.interface=lan
dhcp.lan_main.ra=disabled
dhcp.lan_main.dhcpv6=disabled
dhcp.lan_main.ndp=disabled
FOCUSED_DHCP
	cat >"$config_dir/network" <<'FOCUSED_NETWORK'
network.lan_main=interface
network.lan_main.device=br-lan
network.lan_main.proto=static
network.lan_main.ipaddr=192.168.9.1
network.lan_main.delegate=0
FOCUSED_NETWORK
	: >"$trace"

	transaction_dir="$config_dir/.proxypool-state/firewall-transaction"
	mkdir -p "$transaction_dir/original" "$transaction_dir/staged"
	chmod 700 "$config_dir/.proxypool-state" "$transaction_dir" \
		"$transaction_dir/original" "$transaction_dir/staged"
	for package in firewall dhcp network; do
		cp "$config_dir/$package" "$transaction_dir/original/$package"
		cp "$config_dir/$package" "$transaction_dir/staged/$package"
		sha256sum "$transaction_dir/original/$package" | awk '{ print $1 }' >"$transaction_dir/original/$package.sha256"
		sha256sum "$transaction_dir/staged/$package" | awk '{ print $1 }' >"$transaction_dir/staged/$package.sha256"
	done
	printf '%s\n' 1 >"$transaction_dir/schema_version"
	printf '%s\n' awaiting-fw4-start >"$transaction_dir/state"
	printf '%s\n' test-boot-one >"$transaction_dir/boot_id"
	printf '%s\n' live >"$transaction_dir/mode"
	chmod 600 "$transaction_dir/schema_version" "$transaction_dir/state" \
		"$transaction_dir/boot_id" "$transaction_dir/mode" \
		"$transaction_dir/original"/* "$transaction_dir/staged"/*

	ACTION=includes env \
		PATH="$BIN:$PATH" \
		PROXYPOOL_CONFIG_DIR="$config_dir" \
		PROXYPOOL_UCI="$BIN/uci" \
		PROXYPOOL_FIREWALL_TRANSACTION_DIR="$transaction_dir" \
		PROXYPOOL_FIREWALL_ACTIVATION_MARKER="$config_dir/.proxypool-state/firewall-safety-activated" \
		PROXYPOOL_TRANSACTION_LOCK="$TEST_TMP/proxypool-firewall.lock" \
		PROXYPOOL_FW4_LOCK="$TEST_TMP/fw4.lock" \
		PROXYPOOL_FW4_STATE="$TEST_TMP/fw4.state" \
		PROXYPOOL_BOOT_ID_FILE="$BOOT_ID_FILE" \
		PROXYPOOL_LS_PROG="$BIN/ls" \
		PROXYPOOL_SYNC=true \
		PROXYPOOL_NFT="$BIN/nft-runtime" \
		PROXYPOOL_FLOWTABLE_PROBE="$BIN/flowtable-probe" \
		PROXYPOOL_TEST_FLOWTABLE_SEQUENCE=1 \
		PROXYPOOL_GUARD_CONTRACT_FILE="$GUARD_CONTRACT_FILE" \
		PROXYPOOL_FW4_INPUT_GATE_CONTRACT_FILE="$INPUT_GATE_CONTRACT_FILE" \
		PROXYPOOL_FW4_FORWARD_GATE_CONTRACT_FILE="$FORWARD_GATE_CONTRACT_FILE" \
		PROXYPOOL_LAN_ISOLATION_CONTRACT_FILE="$LAN_ISOLATION_CONTRACT_FILE" \
		PROXYPOOL_LAN_HOTPLUG_CONTRACT_FILE="$LAN_HOTPLUG_CONTRACT_FILE" \
		PROXYPOOL_LAN_IFACE_HOTPLUG_CONTRACT_FILE="$LAN_IFACE_HOTPLUG_CONTRACT_FILE" \
		PROXYPOOL_LAN_WORKER_CONTRACT_FILE="$LAN_WORKER_CONTRACT_FILE" \
		PROXYPOOL_GUARD_INIT_CONTRACT_FILE="$GUARD_INIT_CONTRACT_FILE" \
		PROXYPOOL_UCI_STAGED_CONTRACT_FILE="$UCI_STAGED_CONTRACT_FILE" \
		PROXYPOOL_LEGACY_GATE_CONTRACT_FILE="$LEGACY_GATE_CONTRACT_FILE" \
		PROXYPOOL_TEST_CONTRACT_FILES="$CONTRACT_FILES" \
		PROXYPOOL_TEST_EXEC_CONTRACT_FILES="$EXEC_CONTRACT_FILES" \
		PROXYPOOL_TEST_TRACE="$trace" \
		"$TRANSACTION_HELPER" finalize-fw4-locked

	activation_marker="$config_dir/.proxypool-state/firewall-safety-activated"
	[ "$(wc -l <"$activation_marker" | tr -d '[:space:]')" = 5 ] &&
		grep -Fxq schema_version=2 "$activation_marker" &&
		grep -Fxq projection_schema=2 "$activation_marker" &&
		grep -Fxq contract_schema=4 "$activation_marker" ||
		fail 'focused finalizer did not publish canonical schema-2 authority'
	marker_backup="$TEST_TMP/activation-helper.marker"
	cp "$activation_marker" "$marker_backup"
	lan_dhcp=$(find_section "$config_dir" dhcp dhcp interface lan) || fail 'focused fixture lost LAN DHCP'
	if [ "$focus_activation_case" = baseline ]; then
		activation_runtime_current_for_fixture "$config_dir" 1 >/dev/null 2>&1 ||
			fail 'focused baseline authority/runtime verification failed'
		echo 'ProxyPool focused activation baseline: PASS'
		exit 0
	fi

	if [ "$focus_activation_case" != runtime ]; then
		projection_case=${PROXYPOOL_TEST_PROJECTION_CASE:-all}
		case "$projection_case" in
			lease)
				UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set "dhcp.$lan_dhcp.leasetime=8h"
				activation_runtime_current_for_fixture "$config_dir" 1 >/dev/null 2>&1 ||
					fail 'lease-only update was not restart-admissible'
				;;
			noresolv)
				UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set dhcp.dnsmasq_main.noresolv=0
				if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
					fail 'focused authority accepted noresolv=0'
				fi
				;;
			server)
				UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set dhcp.dnsmasq_main.server=/tmp/resolv.conf.auto
				if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
					fail 'focused authority accepted restored DNS server'
				fi
				;;
			port)
				UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set dhcp.dnsmasq_main.port=53
				if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
					fail 'focused authority accepted dnsmasq port=53'
				fi
				;;
			ra|dhcpv6|ndp)
				UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set "dhcp.$lan_dhcp.$projection_case=server"
				if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
					fail "focused authority accepted LAN $projection_case restore"
				fi
				;;
			forwarding)
				UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set firewall.regression_lan_wan=forwarding
				UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set firewall.regression_lan_wan.src=lan
				UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set firewall.regression_lan_wan.dest=wan
				if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
					fail 'focused authority accepted LAN-to-WAN restore'
				fi
				;;
			network_topology)
				UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set network.regression_alt_bridge=device
				UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set network.regression_alt_bridge.name=br-guest
				UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set network.regression_alt_bridge.type=bridge
				UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set network.regression_alt_bridge.ports=lan5
				if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
					fail 'focused authority accepted alternate physical-client topology'
				fi
				;;
			contract)
				printf '%s\n' '# L2 contract drift' >>"$LAN_HOTPLUG_CONTRACT_FILE"
				if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
					fail 'focused authority accepted L2 hotplug contract drift'
				fi
				;;
			contract_iface)
				printf '%s\n' '# iface contract drift' >>"$LAN_IFACE_HOTPLUG_CONTRACT_FILE"
				if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
					fail 'focused authority accepted iface hotplug contract drift'
				fi
				;;
			contract_worker)
				printf '%s\n' '# worker contract drift' >>"$LAN_WORKER_CONTRACT_FILE"
				if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
					fail 'focused authority accepted LAN worker contract drift'
				fi
				;;
			contract_guard_init)
				printf '%s\n' '# guard init contract drift' >>"$GUARD_INIT_CONTRACT_FILE"
				if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
					fail 'focused authority accepted guardian lifecycle contract drift'
				fi
				;;
			contract_ucode)
				printf '%s\n' '/* staged UCI contract drift */' >>"$UCI_STAGED_CONTRACT_FILE"
				if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
					fail 'focused authority accepted staged UCI contract drift'
				fi
				;;
			contract_legacy)
				printf '%s\n' '# legacy gate contract drift' >>"$LEGACY_GATE_CONTRACT_FILE"
				if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
					fail 'focused authority accepted legacy gate contract drift'
				fi
				;;
			contract_mode|contract_owner|contract_links)
				contract_fault=${projection_case#contract_}
				if activation_current_for_fixture "$config_dir" "$contract_fault" >/dev/null 2>&1; then
					fail "focused authority accepted unsafe contract metadata: $contract_fault"
				fi
				;;
			all)
				# The complete Linux suite exercises every case above.  Keep this
				# branch for CI; Windows-focused runs select one fork-heavy case.
				for delegated_case in lease noresolv server port ra dhcpv6 ndp forwarding network_topology contract contract_iface contract_worker contract_guard_init contract_ucode contract_legacy contract_mode contract_owner contract_links; do
					PROXYPOOL_TEST_FOCUS_ACTIVATION_CASE=projection \
					PROXYPOOL_TEST_PROJECTION_CASE="$delegated_case" \
						"$0"
				done
				;;
			*) fail "unknown focused projection case: $projection_case" ;;
		esac
		echo "ProxyPool focused activation projection ($projection_case): PASS"
		exit 0
	fi

	runtime_faults=${PROXYPOOL_TEST_RUNTIME_CASE:-"missing_rule missing_timeout nonempty_set missing_bridge extra_accept"}
	for runtime_fault in $runtime_faults; do
		if activation_runtime_current_for_fixture "$config_dir" 1 "$runtime_fault" >/dev/null 2>&1; then
			fail "focused runtime gate accepted $runtime_fault"
		fi
	done
	activation_runtime_current_for_fixture "$config_dir" 1 >/dev/null 2>&1 ||
		fail 'focused runtime gate rejected canonical inet/bridge dumps'

	{
		printf '%s\n' schema_version=1
		sed -n '2,$p' "$marker_backup"
	} >"$activation_marker"
	chmod 600 "$activation_marker"
	if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
		fail 'focused authority accepted legacy marker schema'
	fi
	echo 'ProxyPool focused activation helper matrix: PASS'
	exit 0
fi

success_layouts=${PROXYPOOL_TEST_FOCUS_LAYOUT:-"wan_first lan_first"}
for layout in $success_layouts; do
	config_dir="$TEST_TMP/success-$layout"
	trace="$TEST_TMP/success-$layout.trace"
	write_fixture "$config_dir" "$layout"
	if ! run_defaults "$config_dir" "$trace" >"$TEST_TMP/success-$layout.log" 2>&1; then
		cat "$TEST_TMP/success-$layout.log" >&2
		fail "defaults rejected valid $layout fixture"
	fi
	assert_success_state "$config_dir" "$trace"
	canonical_success="$TEST_TMP/canonical-success-$layout"
	mkdir "$canonical_success"
	cp "$config_dir/firewall" "$config_dir/dhcp" "$config_dir/network" "$canonical_success/"
	if [ "$layout" = wan_first ] &&
		{ [ "${PROXYPOOL_TEST_FOCUS_SUCCESS:-0}" != 1 ] ||
		  [ "${PROXYPOOL_TEST_FOCUS_ACTIVATION:-0}" = 1 ]; }; then
		activation_marker="$config_dir/.proxypool-state/firewall-safety-activated"
		marker_backup="$TEST_TMP/activation-marker.backup"
		cp "$activation_marker" "$marker_backup"
		printf '%s' trailing-garbage >>"$activation_marker"
		if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
			fail 'activation authority accepted unterminated trailing bytes'
		fi
		cp "$marker_backup" "$activation_marker"
		marker_bytes=$(wc -c <"$activation_marker" | tr -d '[:space:]')
		dd if="$activation_marker" of="$activation_marker.no-final-lf" bs=1 \
			count=$((marker_bytes - 1)) 2>/dev/null
		mv "$activation_marker.no-final-lf" "$activation_marker"
		if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
			fail 'activation authority accepted a missing final newline'
		fi
		cp "$marker_backup" "$activation_marker"
		activation_current_for_fixture "$config_dir" >/dev/null 2>&1 ||
			fail 'activation authority rejected its exact canonical marker'
		activation_runtime_current_for_fixture "$config_dir" 1 >/dev/null 2>&1 ||
			fail 'runtime activation gate rejected a marker with valid live gates'
		if activation_runtime_current_for_fixture "$config_dir" 0 >/dev/null 2>&1; then
			fail 'runtime activation gate accepted a current marker with an active flowtable'
		fi

		# DHCP lease duration remains outside the semantic safety projection.  The
		# complete network package is bound because any alternate bridge/VLAN/lower
		# can become a client ingress; such edits require explicit reactivation.
		UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set "dhcp.$lan_dhcp.leasetime=6h"
		activation_current_for_fixture "$config_dir" >/dev/null 2>&1 ||
			fail 'LAN leasetime-only update revoked semantic activation authority'
		activation_runtime_current_for_fixture "$config_dir" 1 >/dev/null 2>&1 ||
			fail 'LAN leasetime-only update blocked runtime restart admission'

		UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set dhcp.dnsmasq_main.noresolv=0
		if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
			fail 'activation authority accepted dnsmasq noresolv=0'
		fi
		UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set dhcp.dnsmasq_main.noresolv=1

		UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set dhcp.dnsmasq_main.server=/tmp/resolv.conf.d/resolv.conf.auto
		if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
			fail 'activation authority accepted a restored dnsmasq server'
		fi
		UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q delete dhcp.dnsmasq_main.server

		for unsafe_ipv6_option in ra dhcpv6 ndp; do
			UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set "dhcp.$lan_dhcp.$unsafe_ipv6_option=server"
			if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
				fail "activation authority accepted restored LAN $unsafe_ipv6_option"
			fi
			UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set "dhcp.$lan_dhcp.$unsafe_ipv6_option=disabled"
		done

		UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set firewall.regression_lan_wan=forwarding
		UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set firewall.regression_lan_wan.src=lan
		UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set firewall.regression_lan_wan.dest=wan
		if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
			fail 'activation authority accepted a restored LAN-to-WAN forwarding'
		fi
		UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q delete firewall.regression_lan_wan
		activation_current_for_fixture "$config_dir" >/dev/null 2>&1 ||
			fail 'restoring the exact safety projection did not restore authority'

		printf '%s\n' '# contract drift' >>"$GUARD_CONTRACT_FILE"
		if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
			fail 'activation authority accepted changed safety contract bytes'
		fi
		cp "$ROOT/proxypool-core/files/proxypool-guard.nft" "$GUARD_CONTRACT_FILE"
		chmod 644 "$GUARD_CONTRACT_FILE"
		for contract_metadata_fault in mode owner links; do
			if activation_current_for_fixture "$config_dir" "$contract_metadata_fault" >/dev/null 2>&1; then
				fail "activation authority accepted unsafe contract metadata: $contract_metadata_fault"
			fi
		done
		activation_current_for_fixture "$config_dir" >/dev/null 2>&1 ||
			fail 'restored safety contract did not restore authority'

		{
			printf '%s\n' schema_version=1
			sed -n '2,$p' "$marker_backup"
		} >"$activation_marker"
		chmod 600 "$activation_marker"
		if activation_current_for_fixture "$config_dir" >/dev/null 2>&1; then
			fail 'activation authority accepted the legacy whole-file marker schema'
		fi
		cp "$marker_backup" "$activation_marker"

		for runtime_fault in missing_rule nonempty_set missing_bridge extra_accept; do
			if activation_runtime_current_for_fixture "$config_dir" 1 "$runtime_fault" >/dev/null 2>&1; then
				fail "runtime activation gate accepted corrupt guardian schema: $runtime_fault"
			fi
		done
		activation_runtime_current_for_fixture "$config_dir" 1 >/dev/null 2>&1 ||
			fail 'runtime activation gate rejected the complete canonical guardian schema'

		# Starting a new conversion must revoke the old marker before its first
		# mutation.  If the third flowtable gate then aborts, neither an old
		# marker nor a journal may make the backend gate mistake this for a
		# successfully acknowledged runtime.
		stale_marker_trace="$TEST_TMP/stale-marker-pre-install.trace"
		if run_defaults "$config_dir" "$stale_marker_trace" \
			PROXYPOOL_TEST_FLOWTABLE_SEQUENCE=1,1,0 \
			>"$TEST_TMP/stale-marker-pre-install.log" 2>&1; then
			fail 'new transaction accepted an active pre-install flowtable'
		fi
		assert_clamped_baseline "$stale_marker_trace.clamped" "$config_dir"
		[ ! -e "$activation_marker" ] && [ ! -L "$activation_marker" ] ||
			fail 'failed new transaction retained stale activation authority'
		[ ! -e "$config_dir/.proxypool-state/firewall-transaction" ] &&
			[ ! -L "$config_dir/.proxypool-state/firewall-transaction" ] ||
			fail 'pre-install abort retained a completed transaction journal'
	fi
	idempotent_before="$TEST_TMP/idempotent-$layout.before"
	idempotent_trace="$TEST_TMP/idempotent-$layout.trace"
	mkdir "$idempotent_before"
	cp "$config_dir/firewall" "$config_dir/dhcp" "$config_dir/network" "$idempotent_before/"
	if ! run_defaults "$config_dir" "$idempotent_trace" PROXYPOOL_TEST_ALLOW_UNCHANGED_RELOAD=1 \
		>"$TEST_TMP/idempotent-$layout.log" 2>&1; then
		cat "$TEST_TMP/idempotent-$layout.log" >&2
		fail "second defaults run rejected converged $layout fixture"
	fi
	for package in firewall dhcp network; do
		if ! cmp -s "$idempotent_before/$package" "$config_dir/$package"; then
			diff -u "$idempotent_before/$package" "$config_dir/$package" >&2 || true
			fail "second defaults run changed converged $layout $package bytes"
		fi
	done
done

if [ "${PROXYPOOL_TEST_FOCUS_SIGKILL_RECOVERY:-0}" = 1 ]; then
	for kill_after in ${PROXYPOOL_TEST_FOCUS_SIGKILL_AFTER:-1 2 3}; do
		run_sigkill_recovery_case "$kill_after" "$TEST_TMP/canonical-success-wan_first"
	done
	echo 'ProxyPool focused SIGKILL recovery: PASS'
	exit 0
fi

if [ "${PROXYPOOL_TEST_FOCUS_INCOMPLETE_ROLLBACK:-0}" = 1 ]; then
	run_incomplete_rollback_recovery_case "$TEST_TMP/canonical-success-lan_first"
	echo 'ProxyPool focused incomplete rollback recovery: PASS'
	exit 0
fi

if [ "${PROXYPOOL_TEST_FOCUS_ACTIVATION:-0}" = 1 ]; then
	echo 'ProxyPool activation authority focused matrix: PASS'
	exit 0
fi

if [ "${PROXYPOOL_TEST_FOCUS_SUCCESS:-0}" = 1 ]; then
	echo 'ProxyPool firewall defaults focused success/idempotence: PASS'
	exit 0
fi

# At boot there is no fw4.state yet.  The validated files are installed under
# an awaiting journal, but activation is left to S19; calling reload here would
# deterministically fail on the pinned firewall4 implementation.
config_dir="$TEST_TMP/success-cold"
trace="$TEST_TMP/success-cold.trace"
write_fixture "$config_dir" wan_first
if ! run_defaults "$config_dir" "$trace" PROXYPOOL_COLD_BOOT=1 \
	>"$TEST_TMP/success-cold.log" 2>&1; then
	cat "$TEST_TMP/success-cold.log" >&2
	fail 'defaults rejected a valid cold-boot transaction'
fi
assert_success_state "$config_dir" "$trace" cold
[ -d "$config_dir/.proxypool-state/firewall-transaction" ] ||
	fail 'cold transaction discarded its journal before S19 activation'
grep -Rqs 'awaiting-fw4-start' "$config_dir/.proxypool-state/firewall-transaction" ||
	fail 'cold transaction journal is not awaiting S19 activation'
printf '%s' trailing-garbage >>"$config_dir/.proxypool-state/firewall-transaction/state"
if env \
	PATH="$BIN:$PATH" \
	PROXYPOOL_CONFIG_DIR="$config_dir" \
	PROXYPOOL_FIREWALL_TRANSACTION_DIR="$config_dir/.proxypool-state/firewall-transaction" \
	PROXYPOOL_FIREWALL_ACTIVATION_MARKER="$config_dir/.proxypool-state/firewall-safety-activated" \
	PROXYPOOL_TRANSACTION_LOCK="$TEST_TMP/proxypool-firewall.lock" \
	PROXYPOOL_FW4_LOCK="$TEST_TMP/fw4.lock" \
	PROXYPOOL_FW4_STATE="$TEST_TMP/fw4.state" \
	PROXYPOOL_BOOT_ID_FILE="$BOOT_ID_FILE" \
	PROXYPOOL_LS_PROG="$BIN/ls" \
	PROXYPOOL_FLOCK="$BIN/flock" \
	PROXYPOOL_SYNC="$BIN/sync" \
	PROXYPOOL_TEST_TRACE="$TEST_TMP/cold-corrupt-state.trace" \
	"$TRANSACTION_HELPER" recover-only >/dev/null 2>&1; then
	fail 'journal parser accepted unterminated trailing state bytes'
fi
[ -d "$config_dir/.proxypool-state/firewall-transaction" ] ||
	fail 'corrupt journal validation deleted the only recovery evidence'

delete_fixture_section() {
	config_dir=$1
	package=$2
	section=$3
	UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q delete "$package.$section"
}

# Missing semantic LAN sections are configuration corruption, not permission
# to synthesize a new topology.  Each case fails before installing any file and
# leaves all original package bytes untouched.
for missing in zone dhcp network; do
	config_dir="$TEST_TMP/missing-lan-$missing"
	before="$TEST_TMP/missing-lan-$missing.before"
	trace="$TEST_TMP/missing-lan-$missing.trace"
	write_fixture "$config_dir" wan_first
	case "$missing" in
		zone)
			section=$(find_section "$config_dir" firewall zone name lan) || fail 'fixture lost LAN zone'
			delete_fixture_section "$config_dir" firewall "$section"
			;;
		dhcp)
			section=$(find_section "$config_dir" dhcp dhcp interface lan) || fail 'fixture lost LAN DHCP section'
			delete_fixture_section "$config_dir" dhcp "$section"
			;;
		network)
			section=$(find_section "$config_dir" network interface device br-lan) || fail 'fixture lost LAN network section'
			delete_fixture_section "$config_dir" network "$section"
			;;
	esac
	mkdir "$before"
	cp "$config_dir/firewall" "$config_dir/dhcp" "$config_dir/network" "$before/"
	if run_defaults "$config_dir" "$trace" >"$TEST_TMP/missing-lan-$missing.log" 2>&1; then
		fail "defaults accepted a missing LAN $missing section"
	fi
	assert_clamped_baseline "$trace.clamped" "$config_dir"
	[ "$(grep -c '^config:install:' "$trace" 2>/dev/null || true)" -eq 0 ] ||
		fail "missing LAN $missing installed a partial configuration"
	[ "$(grep -c '^firewall:reload$' "$trace" 2>/dev/null || true)" -eq 0 ] ||
		fail "missing LAN $missing reloaded firewall"
done

# DNS fail-closed ownership is only safe with one unambiguous dnsmasq section.
# Missing or duplicate sections fail before any live package replacement and
# leave the already-installed clamp as the recovery baseline.
for dnsmasq_shape in missing duplicate; do
	config_dir="$TEST_TMP/dnsmasq-$dnsmasq_shape"
	trace="$TEST_TMP/dnsmasq-$dnsmasq_shape.trace"
	write_fixture "$config_dir" wan_first
	case "$dnsmasq_shape" in
		missing)
			delete_fixture_section "$config_dir" dhcp dnsmasq_main
			;;
		duplicate)
			UCI_CONFIG_DIR="$config_dir" "$BIN/uci" -q set dhcp.dnsmasq_extra=dnsmasq
			;;
	esac
	if run_defaults "$config_dir" "$trace" >"$TEST_TMP/dnsmasq-$dnsmasq_shape.log" 2>&1; then
		fail "defaults accepted a $dnsmasq_shape dnsmasq topology"
	fi
	assert_clamped_baseline "$trace.clamped" "$config_dir"
	[ "$(grep -c '^config:install:' "$trace" 2>/dev/null || true)" -eq 0 ] ||
		fail "$dnsmasq_shape dnsmasq topology installed a partial configuration"
	[ "$(grep -c '^firewall:reload$' "$trace" 2>/dev/null || true)" -eq 0 ] ||
		fail "$dnsmasq_shape dnsmasq topology activated a firewall"
done

# A failed target fw4 validation never reaches live replacement or reload.
config_dir="$TEST_TMP/fail-check"
before="$TEST_TMP/fail-check.before"
trace="$TEST_TMP/fail-check.trace"
write_fixture "$config_dir" wan_first
mkdir "$before"
cp "$config_dir/firewall" "$config_dir/dhcp" "$config_dir/network" "$before/"
if run_defaults "$config_dir" "$trace" PROXYPOOL_TEST_FW4_FAIL=1 >"$TEST_TMP/fail-check.log" 2>&1; then
	fail 'defaults reported success after fw4 check failure'
fi
assert_clamped_baseline "$trace.clamped" "$config_dir"
[ "$(grep -c '^firewall:reload$' "$trace" 2>/dev/null || true)" -eq 0 ] ||
	fail 'fw4 check failure still reloaded firewall'
[ "$(grep -c '^config:install:' "$trace" 2>/dev/null || true)" -eq 0 ] ||
	fail 'fw4 check failure installed live configuration files'
[ "$(grep -c '^guardian:reset-empty$' "$trace" 2>/dev/null || true)" -ge 1 ] ||
	fail 'fw4 check failure did not retain an empty guardian'

# Installation is a three-file transaction.  A failure after one or two live
# install attempts must restore every package byte-for-byte before exactly one
# compensating firewall reload converges the kernel to that restored baseline.
for fail_at in 2 3; do
	config_dir="$TEST_TMP/fail-install-$fail_at"
	before="$TEST_TMP/fail-install-$fail_at.before"
	trace="$TEST_TMP/fail-install-$fail_at.trace"
	write_fixture "$config_dir" wan_first
	mkdir "$before"
	cp "$config_dir/firewall" "$config_dir/dhcp" "$config_dir/network" "$before/"
	if run_defaults "$config_dir" "$trace" PROXYPOOL_TEST_INSTALL_FAIL_AT="$fail_at" \
		>"$TEST_TMP/fail-install-$fail_at.log" 2>&1; then
		fail "defaults reported success after config install $fail_at failed"
	fi
	assert_clamped_baseline "$trace.clamped" "$config_dir"
	assert_compensating_install_rollback "$trace" "$fail_at"
	[ "$(grep -c '^guardian:reset-empty$' "$trace" 2>/dev/null || true)" -ge 1 ] ||
		fail "config install failure $fail_at did not retain an empty guardian"
done

# SIGKILL cannot run traps.  A root-only persistent journal must survive each
# partial replacement boundary, and the next invocation must restore the
# clamp-safe baseline before it constructs or validates a new transaction.
for kill_after in 1 2 3; do
	run_sigkill_recovery_case "$kill_after" "$TEST_TMP/canonical-success-wan_first"
done

run_incomplete_rollback_recovery_case "$TEST_TMP/canonical-success-lan_first"

# Reload happens only after atomic replacement; if it fails, all three live
# files are restored byte-for-byte and authorization remains empty.
config_dir="$TEST_TMP/fail-reload"
before="$TEST_TMP/fail-reload.before"
trace="$TEST_TMP/fail-reload.trace"
write_fixture "$config_dir" lan_first
mkdir "$before"
cp "$config_dir/firewall" "$config_dir/dhcp" "$config_dir/network" "$before/"
if run_defaults "$config_dir" "$trace" PROXYPOOL_TEST_RELOAD_FAIL=once >"$TEST_TMP/fail-reload.log" 2>&1; then
	fail 'defaults reported success after firewall reload failure'
fi
assert_clamped_baseline "$trace.clamped" "$config_dir"
[ "$(grep -c '^firewall:reload$' "$trace" 2>/dev/null || true)" -eq 2 ] ||
	fail 'reload failure path did not reload the byte-restored original configuration'
[ "$(grep -c '^guardian:reset-empty$' "$trace" 2>/dev/null || true)" -ge 1 ] ||
	fail 'reload failure did not retain an empty guardian'

# If the activator shell is SIGKILLed, an inherited-lock child may still be
# committing nft/includes.  The parent must not start a compensating activator
# or retire/rollback the only WAL until a reboot establishes a known kernel
# state.
config_dir="$TEST_TMP/activator-sigkill"
trace="$TEST_TMP/activator-sigkill.trace"
transaction_dir="$config_dir/.proxypool-state/firewall-transaction"
write_fixture "$config_dir" wan_first
if run_defaults "$config_dir" "$trace" PROXYPOOL_TEST_RELOAD_KILL_SELF=1 \
	>"$TEST_TMP/activator-sigkill.log" 2>&1; then
	fail 'defaults accepted an activator with an unknown SIGKILL outcome'
fi
sleep 3
[ -d "$transaction_dir" ] || fail 'activator SIGKILL lost the persistent transaction journal'
grep -Fqx awaiting-fw4-start "$transaction_dir/state" ||
	fail 'activator SIGKILL rewrote or retired the awaiting journal'
[ "$(grep -c '^firewall:reload$' "$trace" 2>/dev/null || true)" -eq 1 ] ||
	fail 'activator SIGKILL launched a competing compensating activation'
[ "$(grep -c '^config:install:' "$trace" 2>/dev/null || true)" -eq 3 ] ||
	fail 'activator SIGKILL performed an unsafe live rollback'
grep -Fxq 'firewall:orphan-completed' "$trace" ||
	fail 'activator SIGKILL fixture did not prove a child outlived its shell'
[ ! -e "$config_dir/.proxypool-state/firewall-safety-activated" ] ||
	fail 'activator SIGKILL published a successful activation marker'

# The fourth flowtable proof occurs after candidate nft apply but before marker
# publication and WAL retirement.  An active runtime flowtable must fail the
# candidate, restore the post-clamp bytes, attempt compensation under the same
# WAL, and retain rollback evidence when the flowtable also blocks compensation.
run_post_apply_flowtable_case

echo 'ProxyPool firewall defaults transaction: PASS'
