#!/usr/bin/ucode

const fs = require("fs");
const libubus = require("ubus");

function fail(message) {
	warn(`ProxyPool ubus bridge: ${message}\n`);
	exit(1);
}

/*
 * The stock ubus CLI only accepts JSON in argv. This deliberately tiny bridge
 * keeps the L2TP username and password on stdin and only exposes the single
 * netifd mutation ProxyPool needs.
 */
if (length(ARGV) != 2 || ARGV[0] != "network" || ARGV[1] != "add_dynamic")
	fail("unsupported call");

const raw = fs.readfile("/dev/stdin", 65537);
if (type(raw) != "string" || length(raw) == 0 || length(raw) > 65536)
	fail("invalid input size");

let request;
try {
	request = json(raw);
}
catch (error) {
	fail("invalid JSON input");
}

if (type(request) != "object" || length(request) != 10 ||
    type(request.name) != "string" || !match(request.name, /^ppv2[0-9]{4}$/) ||
    request.proto != "l2tp" ||
    type(request.server) != "string" || !match(request.server, /^[0-9.]+:[0-9]+$/) ||
    type(request.username) != "string" || length(request.username) == 0 || length(request.username) > 256 ||
    type(request.password) != "string" || length(request.password) == 0 || length(request.password) > 1024 ||
    request.ipv6 != false || request.keepalive != "3 5" || request.mtu != 1400 ||
    request.checkup_interval != 5 || request.pppd_options != "noauth")
	fail("invalid dynamic L2TP configuration");

const connection = libubus.connect(null, 10);
if (!connection)
	fail("cannot connect to ubus");

connection.call("network", "add_dynamic", request);
const call_error = connection.error();
connection.disconnect();

if (call_error != null)
	fail("netifd call failed");
