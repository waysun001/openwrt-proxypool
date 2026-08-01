#!/usr/bin/ucode

const uci = require("uci");

function fail(message) {
	warn(`ProxyPool staged UCI: ${message}\n`);
	exit(1);
}

if (length(ARGV) != 3)
	fail("expected action, config directory, and private delta directory");

const action = ARGV[0];
const stage = ARGV[1];
const delta = ARGV[2];

if (action != "clamp-offload" && action != "apply-defaults" &&
    action != "apply-wireless-isolation" &&
    action != "disable-all-wireless" &&
    action != "verify-all-wireless-disabled")
	fail(`unsupported action: ${action}`);

if (type(stage) != "string")
	fail("staged config directory must be a string");

if (substr(stage, 0, 1) != "/")
	fail("staged config directory must be absolute");

if (type(delta) != "string")
	fail("private delta directory must be a string");

if (substr(delta, 0, 1) != "/")
	fail("private delta directory must be absolute");

/*
 * confdir must remain the staged directory so libuci creates its commit
 * tempfile beside the staged package and renames it atomically.  Merely
 * passing a private savedir is not enough on OpenWrt 23.05: libuci retains
 * /tmp/.uci in delta_path.  The explicit absolute loads below set
 * has_delta=false and therefore exclude every pending live delta.
 */
const cursor = uci.cursor(stage, delta, "");

if (!cursor)
	fail("cannot create isolated UCI cursor");

function set_option(package, section, option, value) {
	if (!cursor.set(package, section, option, value))
		fail(`cannot set ${package}.${section}.${option}`);
}

function set_section(package, section, section_type) {
	if (!cursor.set(package, section, section_type))
		fail(`cannot set ${package}.${section} type`);
}

function delete_section(package, section) {
	if (!cursor.delete(package, section))
		fail(`cannot delete ${package}.${section}`);
}

function delete_option(package, section, option) {
	if (!cursor.delete(package, section, option))
		fail(`cannot delete ${package}.${section}.${option}`);
}

function find_unique(package, section_type, option, wanted, description) {
	let names = [];

	cursor.foreach(package, section_type, section => {
		if (section[option] == wanted)
			push(names, section[".name"]);
	});

	if (length(names) != 1)
		fail(`expected exactly one ${description}, found ${length(names)}`);

	return names[0];
}

function section_names(package, section_type) {
	let names = [];

	cursor.foreach(package, section_type, section =>
		push(names, section[".name"]));

	return names;
}

function recreate_named(package, name, section_type, options) {
	let all_sections = cursor.get_all(package);

	if (all_sections && name in all_sections)
		delete_section(package, name);

	set_section(package, name, section_type);

	for (let option, value in options)
		set_option(package, name, option, value);
}

function install_owned_includes() {
	recreate_named("firewall", "proxypool_guard", "include", {
		type: "nftables",
		path: "/usr/lib/proxypool/proxypool-guard.nft",
		position: "ruleset-prepend"
	});

	recreate_named("firewall", "proxypool_fw4_input_gate", "include", {
		type: "nftables",
		path: "/usr/lib/proxypool/proxypool-fw4-input-gate.nft",
		position: "chain-prepend",
		chain: "input"
	});

	recreate_named("firewall", "proxypool_fw4_forward_gate", "include", {
		type: "nftables",
		path: "/usr/lib/proxypool/proxypool-fw4-forward-gate.nft",
		position: "chain-prepend",
		chain: "forward"
	});

	recreate_named("firewall", "proxypool_guard_resync", "include", {
		type: "script",
		path: "/usr/lib/proxypool/guard-resync.sh",
		fw4_compatible: "1"
	});
}

function require_fixed_management_ipv4(network_section) {
	const values = cursor.get_all("network", network_section);
	const addresses = values?.ipaddr;
	let address;

	if (type(addresses) == "string")
		address = addresses;
	else if (type(addresses) == "array" && length(addresses) == 1)
		address = addresses[0];

	if (address != "192.168.9.1" && address != "192.168.9.1/24")
		fail("phase-1 management IPv4 must be exactly 192.168.9.1");
}

function require_real_section_name(section_name, description) {
	if (type(section_name) != "string" ||
	    !match(section_name, /^[A-Za-z0-9_]{1,64}$/))
		fail(`invalid real UCI section name for ${description}`);
}

function require_linux_ifname(ifname, description) {
	/*
	 * Use a deliberately strict subset of dev_valid_name(): an interface name
	 * is at most IFNAMSIZ-1 bytes and contains no whitespace, path separator,
	 * colon, control byte, shell metacharacter or ambiguous dot-only name.
	 * GL-MT6000 DSA ports (lan1..lan5) are all inside this subset.
	 */
	if (type(ifname) != "string" ||
	    !match(ifname, /^[A-Za-z0-9_.-]{1,15}$/) ||
	    ifname == "." || ifname == ".." || ifname == "br-lan")
		fail(`unsafe Linux interface name in ${description}`);
}

const phase1_lan_ports = [ "lan1", "lan2", "lan3", "lan4", "lan5" ];

function is_phase1_lan_port(value) {
	for (let port in phase1_lan_ports)
		if (value == port)
			return true;

	return false;
}

function has_prefix(value, prefix) {
	return type(value) == "string" &&
		substr(value, 0, length(prefix)) == prefix;
}

function is_protected_client_reference(value) {
	if (type(value) != "string")
		return false;

	if (value == "br-lan" || value == "@lan" ||
	    has_prefix(value, "@lan.") || has_prefix(value, "@lan:") ||
	    has_prefix(value, "br-lan.") || has_prefix(value, "br-lan:"))
		return true;

	for (let port in phase1_lan_ports)
		if (value == port || has_prefix(value, port + ".") ||
		    has_prefix(value, port + ":"))
			return true;

	return false;
}

function parse_topology_tokens(value, description) {
	let tokens;

	if (type(value) == "string") {
		if (!length(value) || trim(value) != value)
			fail(`empty or non-canonical ${description}`);

		tokens = split(value, /[ \t\r\n]+/);
	}
	else if (type(value) == "array") {
		tokens = value;
	}
	else {
		fail(`${description} must be a string or list`);
	}

	for (let token in tokens)
		if (type(token) != "string" || !length(token) ||
		    trim(token) != token || match(token, /[ \t\r\n]/))
			fail(`invalid token in ${description}`);

	return tokens;
}

function option_references_protected_client(value, description) {
	if (value == null)
		return false;

	for (let token in parse_topology_tokens(value, description))
		if (is_protected_client_reference(token))
			return true;

	return false;
}

function option_is_enabled(value) {
	if (value === true || value == 1)
		return true;

	if (type(value) != "string")
		return false;

	const normalized = lc(trim(value));
	return normalized == "1" || normalized == "true" ||
		normalized == "yes" || normalized == "on";
}

function require_exact_phase1_port_set(ports) {
	if (length(ports) != length(phase1_lan_ports))
		fail("br-lan must contain exactly the five GL-MT6000 LAN ports");

	let configured = {};

	for (let port in ports)
		configured[port] = true;

	for (let wanted in phase1_lan_ports)
		if (!(wanted in configured))
			fail(`br-lan is missing GL-MT6000 physical port ${wanted}`);
}

function require_plain_phase1_port_device(device) {
	if (device.device_type != null)
		fail(`physical port ${device.name} has unsupported topology option type`);

	for (let option in [
		"ifname", "ports", "vid", "vlan", "device",
		"parent", "link", "vlan_filtering"
	])
		if (device[option] != null)
			fail(`physical port ${device.name} has unsupported topology option ${option}`);
}

function validate_network_client_attachments(all_sections, device_sections,
		lan_network_section, bridge) {
	if (option_is_enabled(bridge.vlan_filtering))
		fail("phase-1 forbids VLAN filtering on br-lan");

	if (bridge.ifname != null)
		fail("phase-1 br-lan must use only its exact static ports");

	for (let device in device_sections) {
		if (device.section == bridge.section)
			continue;

		if (is_phase1_lan_port(device.name)) {
			require_plain_phase1_port_device(device);
			continue;
		}

		if (is_protected_client_reference(device.name))
			fail(`network device creates a protected client upper: ${device.name}`);

		for (let lower_option in [ "ifname", "device", "parent", "link" ])
			if (option_references_protected_client(device[lower_option],
			    `network device ${device.section} ${lower_option}`))
				fail(`network device ${device.section} reuses a protected client lower`);

		if (device.ports != null &&
		    option_references_protected_client(device.ports,
		    `network device ${device.section} ports`))
			fail(`network device ${device.section} reuses a protected client port`);
	}

	cursor.foreach("network", "bridge-vlan", section => {
		const section_name = section[".name"];

		require_real_section_name(section_name, "network bridge-vlan");
		if (!(section_name in all_sections) ||
		    all_sections[section_name][".name"] != section_name ||
		    all_sections[section_name][".type"] != "bridge-vlan")
			fail(`network bridge-vlan section identity mismatch: ${section_name}`);

		if (option_references_protected_client(section.device,
		    `network bridge-vlan ${section_name} device`))
			fail(`bridge-vlan ${section_name} uses a protected client bridge`);

		if (option_references_protected_client(section.ports,
		    `network bridge-vlan ${section_name} ports`))
			fail(`bridge-vlan ${section_name} reuses a protected client port`);
	});

	/*
	 * netifd aliases can hide @lan and its VLAN chain behind an unrelated name.
	 * Phase 1 has no legitimate alias requirement, so retain no resolver surface
	 * whose attachment cannot be proven by the direct interface scan below.
	 */
	let has_network_alias = false;
	cursor.foreach("network", "alias", section => {
		has_network_alias = true;
	});

	if (has_network_alias)
		fail("phase-1 does not support network alias sections");

	cursor.foreach("network", "interface", section => {
		const section_name = section[".name"];

		require_real_section_name(section_name, "network interface");
		if (!(section_name in all_sections) ||
		    all_sections[section_name][".name"] != section_name ||
		    all_sections[section_name][".type"] != "interface")
			fail(`network interface section identity mismatch: ${section_name}`);

		for (let option in [ "device", "ifname", "ports" ]) {
			const value = section[option];

			if (value == null)
				continue;

			if (section_name == lan_network_section && option == "device" &&
			    type(value) == "string" && value == "br-lan")
				continue;

			if (option_references_protected_client(value,
			    `network interface ${section_name} ${option}`))
				fail(`network interface ${section_name} attaches outside the phase-1 LAN`);
		}
	});
}

function parse_bridge_ports(value) {
	let raw_ports;

	if (type(value) == "string") {
		if (!length(value) || trim(value) != value)
			fail("br-lan bridge has an empty or non-canonical static port string");

		raw_ports = split(value, /[ \t\r\n]+/);
	}
	else if (type(value) == "array") {
		raw_ports = value;
	}
	else {
		fail("br-lan bridge ports must be a string or list");
	}

	if (!length(raw_ports))
		fail("br-lan bridge has no configured static ports");

	let ports = [];
	let seen = {};

	for (let port in raw_ports) {
		require_linux_ifname(port, "br-lan static ports");

		if (port in seen)
			fail(`duplicate configured br-lan port: ${port}`);

		seen[port] = true;
		push(ports, port);
	}

	return ports;
}

function plan_lan_port_isolation(lan_network_section) {
	const all_sections = cursor.get_all("network");
	let device_sections = [];
	let seen_sections = {};

	if (type(all_sections) != "object")
		fail("cannot enumerate staged network sections");

	cursor.foreach("network", "device", section => {
		const section_name = section[".name"];

		require_real_section_name(section_name, "network device");

		if (section_name in seen_sections)
			fail(`duplicate real network device section: ${section_name}`);

		if (!(section_name in all_sections) ||
		    all_sections[section_name][".name"] != section_name ||
		    all_sections[section_name][".type"] != "device")
			fail(`network device section identity mismatch: ${section_name}`);

		seen_sections[section_name] = true;
		push(device_sections, {
			section: section_name,
			name: section.name,
			device_type: section.type,
			ports: section.ports,
			ifname: section.ifname,
			device: section.device,
			parent: section.parent,
			link: section.link,
			vid: section.vid,
			vlan: section.vlan,
			vlan_filtering: section.vlan_filtering
		});
	});

	let bridges = [];

	for (let device in device_sections)
		if (type(device.name) == "string" && device.name == "br-lan")
			push(bridges, device);

	if (length(bridges) != 1)
		fail(`expected exactly one br-lan bridge device, found ${length(bridges)}`);

	const bridge = bridges[0];

	if (bridge.device_type != "bridge")
		fail("the unique br-lan device is not type bridge");

	const ports = parse_bridge_ports(bridge.ports);
	require_exact_phase1_port_set(ports);
	validate_network_client_attachments(all_sections, device_sections,
		lan_network_section, bridge);
	let plans = [];
	let position = 0;

	for (let port in ports) {
		position++;
		let matches = [];

		for (let device in device_sections)
			if (type(device.name) == "string" && device.name == port)
				push(matches, device.section);

		if (length(matches) > 1)
			fail(`more than one network device names static port ${port}`);

		if (length(matches) == 1) {
			push(plans, { section: matches[0], port: port, create: false });
			continue;
		}

		const owned_section = sprintf("proxypool_lan_port_%02d", position);

		/*
		 * A reserved name is ownership evidence only when the section already
		 * names this exact port (handled above).  Any other occupant is foreign;
		 * never delete or repurpose it.
		 */
		if (owned_section in all_sections)
			fail(`owned network device section name is occupied: ${owned_section}`);

		push(plans, { section: owned_section, port: port, create: true });
	}

	return plans;
}

function apply_lan_port_isolation(plans) {
	for (let plan in plans) {
		if (plan.create) {
			set_section("network", plan.section, "device");
			set_option("network", plan.section, "name", plan.port);
		}

		set_option("network", plan.section, "isolate", "1");
	}

	/* Prove the complete in-memory result before any package is committed. */
	for (let plan in plans) {
		const values = cursor.get_all("network", plan.section);

		if (!values || values[".type"] != "device" ||
		    values.name != plan.port || values.isolate != "1")
			fail(`cannot verify isolated network device for ${plan.port}`);
	}
}

function parse_wireless_networks(value, section_name) {
	let raw_networks;

	if (type(value) == "string") {
		if (!length(value) || trim(value) != value)
			fail(`invalid network string on wireless interface ${section_name}`);

		raw_networks = split(value, /[ \t\r\n]+/);
	}
	else if (type(value) == "array") {
		raw_networks = value;
	}
	else {
		fail(`network must be a string or list on wireless interface ${section_name}`);
	}

	if (!length(raw_networks))
		fail(`wireless interface ${section_name} has no network`);

	let networks = [];
	let seen = {};

	for (let network in raw_networks) {
		if (type(network) != "string" ||
		    !match(network, /^[A-Za-z0-9_-]{1,64}$/))
			fail(`invalid network token on wireless interface ${section_name}`);

		if (network in seen)
			fail(`duplicate network token on wireless interface ${section_name}`);

		seen[network] = true;
		push(networks, network);
	}

	return networks;
}

function wireless_option_has_value(value) {
	if (value == null)
		return false;

	if (type(value) == "string")
		return length(trim(value)) != 0;

	if (type(value) == "array")
		return length(value) != 0;

	return !!value;
}

function wireless_option_is_nonzero(value) {
	if (value == null)
		return false;

	if (type(value) == "string") {
		const normalized = lc(trim(value));
		return length(normalized) && normalized != "0" &&
			normalized != "false" && normalized != "no" &&
			normalized != "off";
	}

	return value != 0 && value !== false;
}

function require_no_wireless_bridge_extensions(section, section_name) {
	if (option_is_enabled(section.wds))
		fail(`phase-1 forbids WDS on wireless interface ${section_name}`);

	if (wireless_option_is_nonzero(section.dynamic_vlan))
		fail(`phase-1 forbids dynamic VLANs on wireless interface ${section_name}`);

	if (wireless_option_is_nonzero(section.multi_ap))
		fail(`phase-1 forbids multi-AP four-address bridging on wireless interface ${section_name}`);

	for (let option in [
		"network_vlan", "vlan_file", "vlan_bridge", "vlan_tagged_interface",
		"multi_ap_backhaul_ssid", "multi_ap_backhaul_key"
	])
		if (wireless_option_has_value(section[option]))
			fail(`phase-1 forbids ${option} on wireless interface ${section_name}`);
}

function plan_wireless_isolation() {
	const all_sections = cursor.get_all("wireless");
	let targets = [];
	let seen_sections = {};
	let changed = false;

	if (type(all_sections) != "object")
		fail("cannot enumerate staged wireless sections");

	let has_wifi_vlan = false;
	let has_wifi_station = false;

	cursor.foreach("wireless", "wifi-vlan", section => {
		has_wifi_vlan = true;
	});

	if (has_wifi_vlan)
		fail("phase-1 does not support wifi-vlan sections");

	cursor.foreach("wireless", "wifi-station", section => {
		has_wifi_station = true;
	});

	if (has_wifi_station)
		fail("phase-1 does not support wifi-station sections");

	/*
	 * This pass is strictly read-only.  Validate and classify every wifi-iface,
	 * including non-AP interfaces, before the first set() call.
	 */
	cursor.foreach("wireless", "wifi-iface", section => {
		const section_name = section[".name"];

		require_real_section_name(section_name, "wireless interface");

		if (section_name in seen_sections)
			fail(`duplicate real wireless section: ${section_name}`);

		if (!(section_name in all_sections) ||
		    all_sections[section_name][".name"] != section_name ||
		    all_sections[section_name][".type"] != "wifi-iface")
			fail(`wireless section identity mismatch: ${section_name}`);

		seen_sections[section_name] = true;

		if (type(section.mode) != "string" ||
		    !match(section.mode, /^[A-Za-z0-9_-]{1,32}$/))
			fail(`invalid mode on wireless interface ${section_name}`);

		const networks = parse_wireless_networks(section.network, section_name);
		const disabled = option_is_enabled(section.disabled);

		if (disabled)
			return;

		require_no_wireless_bridge_extensions(section, section_name);

		/*
		 * Enabled modes are an allowlist, not a denylist.  A future netifd or
		 * hostapd mode must not silently become a new client/bridge surface.
		 */
		if (section.mode != "ap" && section.mode != "sta")
			fail(`phase-1 forbids enabled ${section.mode} interface ${section_name}`);

		if (section.mode == "sta") {
			if (length(networks) != 1 || networks[0] != "wwan")
				fail(`phase-1 STA must be attached only to wwan: ${section_name}`);
			return;
		}

		if (section.mode != "ap")
			return;

		if (length(networks) != 1 || networks[0] != "lan")
			fail(`phase-1 AP must be attached only to lan: ${section_name}`);

		push(targets, section_name);

		if (section.isolate != "1" || section.bridge_isolate != "1")
			changed = true;
	});

	return { targets: targets, changed: changed };
}

function apply_wireless_isolation(plan) {
	if (!plan.changed) {
		print("unchanged\n");
		return;
	}

	for (let section in plan.targets) {
		const values = cursor.get_all("wireless", section);

		if (!values)
			fail(`wireless interface disappeared before mutation: ${section}`);

		if (values.isolate != "1")
			set_option("wireless", section, "isolate", "1");

		if (values.bridge_isolate != "1")
			set_option("wireless", section, "bridge_isolate", "1");
	}

	for (let section in plan.targets) {
		const values = cursor.get_all("wireless", section);

		if (!values || values[".type"] != "wifi-iface" ||
		    values[".name"] != section || values.isolate != "1" ||
		    values.bridge_isolate != "1")
			fail(`cannot verify wireless isolation for ${section}`);
	}

	if (!cursor.commit("wireless"))
		fail("cannot commit wireless isolation");

	print("changed\n");
}

function is_known_wireless_runtime_type(section_type) {
	return section_type == "wifi-device" || section_type == "wifi-iface" ||
		section_type == "wifi-vlan" || section_type == "wifi-station";
}

function plan_all_wireless_disabled() {
	const all_sections = cursor.get_all("wireless");
	let targets = [];
	let changed = false;

	if (type(all_sections) != "object")
		fail("cannot enumerate staged wireless sections for quarantine");

	for (let section_name, section in all_sections) {
		if (type(section) != "object")
			fail(`invalid wireless section object: ${section_name}`);

		const section_type = section[".type"];

		if (type(section_type) != "string" ||
		    substr(section_type, 0, 5) != "wifi-")
			continue;

		if (!is_known_wireless_runtime_type(section_type))
			fail(`unknown wireless runtime section type: ${section_type}`);

		require_real_section_name(section_name, "wireless quarantine target");
		if (section[".name"] != section_name)
			fail(`wireless quarantine identity mismatch: ${section_name}`);

		push(targets, section_name);
		if (section.disabled != "1")
			changed = true;
	}

	return { targets: targets, changed: changed };
}

function require_all_wireless_disabled(plan) {
	for (let section_name in plan.targets) {
		const section = cursor.get_all("wireless", section_name);

		if (!section || section[".name"] != section_name ||
		    !is_known_wireless_runtime_type(section[".type"]) ||
		    section.disabled != "1")
			fail(`wireless quarantine is not explicit for ${section_name}`);
	}
}

function disable_all_wireless(plan) {
	if (!plan.changed) {
		require_all_wireless_disabled(plan);
		print("unchanged\n");
		return;
	}

	for (let section_name in plan.targets)
		set_option("wireless", section_name, "disabled", "1");

	require_all_wireless_disabled(plan);

	if (!cursor.commit("wireless"))
		fail("cannot commit emergency wireless quarantine");

	print("changed\n");
}

if (action == "disable-all-wireless" ||
    action == "verify-all-wireless-disabled") {
	if (!cursor.load(stage + "/wireless"))
		fail("cannot load wireless by absolute path");

	const disabled_plan = plan_all_wireless_disabled();
	if (action == "disable-all-wireless")
		disable_all_wireless(disabled_plan);
	else {
		require_all_wireless_disabled(disabled_plan);
		print("disabled\n");
	}
	exit(0);
}

if (action == "apply-wireless-isolation") {
	if (!cursor.load(stage + "/wireless"))
		fail("cannot load wireless by absolute path");

	apply_wireless_isolation(plan_wireless_isolation());
	exit(0);
}

if (action == "clamp-offload") {
	if (!cursor.load(stage + "/firewall"))
		fail("cannot load firewall by absolute path");
	if (!cursor.load(stage + "/network"))
		fail("cannot load network by absolute path");

	const clamp_lan_network = find_unique("network", "interface", "device", "br-lan", "br-lan network interface");
	if (clamp_lan_network != "lan")
		fail("phase-1 br-lan management interface must be the named lan section");
	require_fixed_management_ipv4(clamp_lan_network);
	/* Reject every alternate physical/VLAN client ingress before the first set. */
	plan_lan_port_isolation(clamp_lan_network);

	const live_defaults = section_names("firewall", "defaults");

	if (!length(live_defaults))
		fail("firewall defaults section is missing");

	for (let section in live_defaults) {
		set_option("firewall", section, "flow_offloading", "0");
		set_option("firewall", section, "flow_offloading_hw", "0");
		set_option("firewall", section, "auto_includes", "0");
	}

	/*
	 * The post-clamp file is the rollback baseline.  It must not retain a
	 * script/nft include capable of recreating a flowtable behind our back.
	 */
	for (let section in section_names("firewall", "include"))
		delete_section("firewall", section);
	install_owned_includes();

	/* Absolute load sets has_delta=false: one atomic commit to this confdir. */
	if (!cursor.commit("firewall"))
		fail("cannot commit clamped firewall");

	exit(0);
}

const packages = [ "firewall", "dhcp", "network" ];

for (let package in packages)
	if (!cursor.load(stage + "/" + package))
		fail(`cannot load staged ${package} by absolute path`);

const lan_zone = find_unique("firewall", "zone", "name", "lan", "LAN firewall zone");
const lan_dhcp = find_unique("dhcp", "dhcp", "interface", "lan", "LAN DHCP section");
const lan_network = find_unique("network", "interface", "device", "br-lan", "br-lan network interface");
const dnsmasq = section_names("dhcp", "dnsmasq");

if (length(dnsmasq) != 1)
	fail(`expected exactly one dnsmasq section, found ${length(dnsmasq)}`);

if (lan_network != "lan")
	fail("phase-1 br-lan management interface must be the named lan section");
require_fixed_management_ipv4(lan_network);

const defaults = section_names("firewall", "defaults");

if (!length(defaults))
	fail("firewall defaults section is missing");

/* Classify every static br-lan port before the first staged mutation. */
const lan_port_isolation = plan_lan_port_isolation(lan_network);

for (let section in defaults) {
	set_option("firewall", section, "flow_offloading", "0");
	set_option("firewall", section, "flow_offloading_hw", "0");
	set_option("firewall", section, "auto_includes", "0");
}

set_option("firewall", lan_zone, "input", "REJECT");
set_option("firewall", lan_zone, "forward", "REJECT");
set_option("firewall", lan_zone, "output", "ACCEPT");

/* Remove only the two literal wildcard tokens left by old ProxyPool images. */
cursor.foreach("firewall", "zone", section => {
	const devices = section.device;

	if (devices == null)
		return;

	const was_list = type(devices) == "array";
	let values;

	if (was_list)
		values = devices;
	else if (type(devices) == "string")
		values = split(devices, /[ \t\r\n]+/);
	else
		return;

	let filtered = [];

	for (let device in values)
		if (type(device) == "string" && length(device) &&
		    device != "ppp-+" && device != "ppp-*")
			push(filtered, device);

	if (!length(filtered))
		delete_option("firewall", section[".name"], "device");
	else
		set_option("firewall", section[".name"], "device",
			was_list ? filtered : join(" ", filtered));
});

let remove_forwardings = [];

cursor.foreach("firewall", "forwarding", section => {
	if (section.src == "lan" && section.dest == "wan")
		push(remove_forwardings, section[".name"]);
});

for (let section in remove_forwardings)
	delete_section("firewall", section);

let remove_rules = {};

cursor.foreach("firewall", "rule", section => {
	const target = type(section.target) == "string" ? lc(section.target) : null;
	const lan_input = section.src == "lan" &&
		(section.dest == null || (type(section.dest) == "string" && trim(section.dest) == ""));

	/*
	 * Converge LAN input to an exact appliance whitelist regardless of legacy
	 * rule order.  Also mirror fw4 parse_enum() ACCEPT-prefix matching (even
	 * the empty prefix) for any remaining LAN forwarding rule.
	 */
	if (lan_input || (section.src == "lan" && target != null &&
	    substr("accept", 0, length(target)) == target))
		remove_rules[section[".name"]] = true;
});

for (let name in [
	"proxypool_allow_dhcp",
	"proxypool_allow_dns",
	"proxypool_allow_admin_http",
	"proxypool_allow_admin_https"
])
	remove_rules[name] = true;

const firewall_before_rule_delete = cursor.get_all("firewall");

for (let name in keys(remove_rules))
	if (firewall_before_rule_delete && name in firewall_before_rule_delete)
		delete_section("firewall", name);

recreate_named("firewall", "proxypool_allow_dhcp", "rule", {
	name: "ProxyPool Allow DHCP",
	src: "lan",
	proto: "udp",
	src_port: "68",
	dest_port: "67",
	family: "ipv4",
	target: "ACCEPT"
});

recreate_named("firewall", "proxypool_allow_admin_http", "rule", {
	name: "ProxyPool Allow HTTP Management",
	src: "lan",
	proto: "tcp",
	dest_ip: "192.168.9.1",
	dest_port: "80",
	family: "ipv4",
	target: "ACCEPT"
});

recreate_named("firewall", "proxypool_allow_admin_https", "rule", {
	name: "ProxyPool Allow HTTPS Management",
	src: "lan",
	proto: "tcp",
	dest_ip: "192.168.9.1",
	dest_port: "443",
	family: "ipv4",
	target: "ACCEPT"
});

let remove_includes = [];

cursor.foreach("firewall", "include", section => {
	/*
	 * This is an appliance policy, not a cooperative host firewall.  Any
	 * foreign nftables or script include can recreate a flowtable after our
	 * static checks, so the candidate contains only the four owned includes
	 * recreated below.
	 */
	push(remove_includes, section[".name"]);
});

for (let section in remove_includes)
	delete_section("firewall", section);

install_owned_includes();

set_option("dhcp", lan_dhcp, "ra", "disabled");
set_option("dhcp", lan_dhcp, "dhcpv6", "disabled");
set_option("dhcp", lan_dhcp, "ndp", "disabled");
set_option("dhcp", dnsmasq[0], "noresolv", "1");

const dnsmasq_values = cursor.get_all("dhcp", dnsmasq[0]);

if (dnsmasq_values && "server" in dnsmasq_values)
	delete_option("dhcp", dnsmasq[0], "server");

set_option("network", lan_network, "delegate", "0");

const lan_network_values = cursor.get_all("network", lan_network);

for (let option in [ "ip6assign", "ip6hint", "ip6class" ])
	if (lan_network_values && option in lan_network_values)
		delete_option("network", lan_network, option);

apply_lan_port_isolation(lan_port_isolation);

/*
 * Every package was explicitly loaded by absolute path.  Its has_delta flag
 * is false, so commit writes the in-memory result straight to STAGE and never
 * consults /tmp/.uci or the private savedir.
 */
for (let package in packages)
	if (!cursor.commit(package))
		fail(`cannot commit staged ${package}`);

exit(0);
