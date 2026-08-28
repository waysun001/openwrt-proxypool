# ZeanLink V2 SOCKS5 Live Design

## Goal

Add a production SOCKS5 TCP dataplane to the existing V2 daemon without changing the verified L2TP behavior. A device bound to a SOCKS5 node can browse the web and use TCP application traffic, while UDP, IPv6, local-LAN access, and direct-WAN fallback remain blocked.

## Scope

- Support SOCKS5 servers with no authentication or username/password authentication.
- Run one independently supervised `redsocks` process per node, for at most 60 nodes.
- Keep client DNS on `192.168.9.1`, but carry every upstream DoH connection through the selected SOCKS5 node.
- Publish device authorization only after process ownership, listener readiness, SOCKS5 CONNECT probing, DNS preflight, and firewall redirect verification all succeed.
- Preserve existing node jobs, backoff, reconnect, delete, device binding, Chinese status, and LuCI workflows.
- Report SOCKS5 upload, download, current speed, and cumulative traffic from nftables per-device counters.
- Do not enable SLP in this slice.

## Non-negotiable safety rules

1. No `redudp`, UDP ASSOCIATE, direct fallback, or IPv6 forwarding.
2. The proxy listener binds only to `192.168.9.1:<12000+policy_id>`; the guardian admits only an exact `(MAC, IPv4, listener port)` lease.
3. Router-owned connections may reach only the configured SOCKS5 endpoint through WAN. Client packets never receive a WAN forward authorization.
4. DoH targets are connected with SOCKS5 CONNECT. A direct DoH fallback is forbidden.
5. A stale PID, reused PID, wrong executable, wrong config path, wrong boot ID, wrong start time, or wrong generation is not owned and is never killed or declared ready.
6. Revocation occurs before process stop. Expiring nft leases make daemon crashes fail closed.
7. Existing L2TP adapter, route construction, and interface-bound DoH transport retain their current behavior and tests.

## Architecture

### Protocol dispatch

The scheduler keeps one `platform.NodeAdapter`, implemented by a protocol dispatcher. L2TP requests go to the existing adapter; SOCKS5 requests go to the new adapter; SLP returns `unsupported`. Protocol-aware gates make the route gate a no-op for SOCKS5, while DNS and authorization remain mandatory for both live protocols.

### SOCKS5 adapter

For policy ID `N`, the adapter renders a private configuration at `/var/run/proxypool/nodes/<node-id>/redsocks.conf` and starts an independent `redsocks` process listening on `192.168.9.1:(12000+N)`. The configuration contains exactly one TCP `redsocks` stanza and no UDP or fallback stanza.

The adapter records a boot-scoped ownership manifest with node ID, revision, generation, executable, PID, `/proc` start time, config path, listener, and a credential-safe digest. `Probe` first re-proves ownership and listener identity, then uses the internal SOCKS5 client to connect through the configured remote node to the fixed TCP probe target. Authentication rejection, name resolution failure, timeout, and generic probe failure remain distinguishable error classes.

### DNS

The existing per-device DNS server stays authoritative at `192.168.9.1:53`. For a SOCKS5 session, the DNS factory creates an HTTP transport whose dialer performs SOCKS5 CONNECT to the pinned AliDNS DoH IP and TLS name. It never calls the direct/bootstrap transport. The DNS gate must complete a real query before device authorization is published.

### Firewall and accounting

The guardian adds an expiring map from `(MAC, IPv4)` to local TCP listener port and performs a TCP-only prerouting redirect. Existing input admission still requires the exact post-DNAT listener tuple. Private, multicast, link-local, IPv6, and UDP traffic continue to hit the existing drops.

Expiring upload and download accounting elements are published with each proxy authorization lease. The traffic reader aggregates their packet-byte counters by node and feeds the existing status/UI speed tracker. Rebinding or node failure removes the old counters and authorization in the same ownership transaction.

## Lifecycle

1. Validate node and resolve only the SOCKS5 server endpoint needed for router-owned connection setup.
2. Reserve ownership and start the per-node process.
3. Prove exact process and listener ownership.
4. Complete an authenticated SOCKS5 CONNECT probe.
5. Open proxy DoH and complete DNS preflight.
6. Publish redirect, DNS-client, policy-mark, and accounting leases.
7. Report online.
8. On any failure, revoke authorization/counters, stop only the proven-owned process, and enter existing backoff.

## Delivery and acceptance

Automated tests use a real in-process fake SOCKS5 server and real TCP/TLS endpoints. The package workflow must pass before deployment. The first device test uses SSH hot-update IPKs and verifies: web access, WeChat TCP messaging, DNS through the proxy, UDP failure, local-LAN failure, direct-WAN failure during proxy outage, independent reconnect/delete, and traffic counters. A full firmware build is produced only after that hot test passes.
