# ZeanLink V2 SOCKS5 Live Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver independently managed, TCP-only SOCKS5 nodes with proxy-bound DNS, fail-closed device redirects, and traffic reporting.

**Architecture:** Add a protocol dispatcher around the existing L2TP adapter and a new per-node `redsocks` adapter. Reuse the current scheduler and expiring authorization lifecycle, extending protocol-aware gates and the guardian with a timeout-backed redirect map and accounting sets.

**Tech Stack:** Go 1.20, OpenWrt 23.05, `redsocks`, nftables/fw4, LuCI JavaScript/Lua, shell package integration.

**Spec:** `docs/superpowers/specs/2026-08-28-proxypool-socks5-live-design.md`

## Global Constraints

- Keep verified L2TP behavior unchanged.
- SOCKS5 supports TCP only; UDP, IPv6, local LAN, and direct WAN remain blocked.
- Support both no-auth and username/password SOCKS5.
- Maximum 60 nodes; proxy startup concurrency remains bounded by `proxy_concurrency`.
- Secrets never appear in argv, logs, diagnostics, process ownership digests, or world-readable files.
- Test an IPK hot update on the router before starting a full firmware build.

---

### Task 1: Protocol dispatcher and controller admission

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/platform/dispatch.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/dispatch_test.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/controller.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/controller_test.go`

**Interfaces:**
- Consumes: existing `platform.NodeAdapter` and `model.Protocol`.
- Produces: `platform.NewProtocolDispatcher(map[model.Protocol]NodeAdapter) NodeAdapter` and SOCKS5 `node.save` admission.

- [ ] **Step 1: Write failing tests** proving L2TP and SOCKS5 dispatch to different real fake adapters, SLP returns `unsupported`, and controller save accepts a valid SOCKS5 node without weakening credential pairing.
- [ ] **Step 2: Run** `go test ./internal/platform ./internal/engine -run 'ProtocolDispatcher|Controller.*SOCKS5' -count=1` and confirm failures are caused by missing dispatch/admission.
- [ ] **Step 3: Implement** an exact protocol lookup with no default adapter and change the controller admission predicate from L2TP-only to `{l2tp,socks5}`.
- [ ] **Step 4: Re-run the targeted tests**, then `go test ./internal/platform ./internal/engine -count=1`.
- [ ] **Step 5: Commit** dispatcher and controller admission together.

### Task 2: Strict SOCKS5 CONNECT client

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/socks5/client.go`
- Create: `proxypool-core/src/proxypoold/internal/socks5/client_test.go`
- Create: `proxypool-core/src/proxypoold/internal/socks5/testserver_test.go`

**Interfaces:**
- Produces: `socks5.Dialer{ProxyAddress, Username, Password}.DialContext(ctx, network, target) (net.Conn, error)` and typed error codes consumed by adapter/DNS.

- [ ] **Step 1: Write a real in-process fake SOCKS5 server** and failing tests for no-auth, username/password, IPv4 and hostname targets, auth rejection, unsupported auth, partial frames, malformed reply, and deadline cancellation.
- [ ] **Step 2: Run** `go test ./internal/socks5 -count=1 -v` and confirm failure because the client does not exist.
- [ ] **Step 3: Implement** RFC 1928 CONNECT plus RFC 1929 authentication with bounded field lengths, `io.ReadFull`, context deadlines, and no credential formatting.
- [ ] **Step 4: Re-run** the package tests and `go test -race ./internal/socks5 -count=1`.
- [ ] **Step 5: Commit** the reusable SOCKS5 transport.

### Task 3: Independent redsocks adapter

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/platform/openwrt/socks5.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/openwrt/socks5_test.go`
- Modify: `proxypool-core/src/proxypoold/internal/platform/contracts.go`

**Interfaces:**
- Consumes: `socks5.Dialer`, `platform.InputCommandRunner`, `/proc`, boot ID, and policy ID.
- Produces: `NewSOCKS5Adapter(...) platform.NodeAdapter`, session `LocalPort=12000+PolicyID`, and verified ownership metadata.

- [ ] **Step 1: Write failing adapter tests** for private config rendering, quoting/control-character rejection, no `redudp`, exact listener address/port, auth probe classifications, PID reuse, wrong executable/config/start-time, stale boot/generation, timeout, and idempotent owned stop.
- [ ] **Step 2: Run** `go test ./internal/platform/openwrt -run SOCKS5 -count=1 -v` and confirm missing adapter failures.
- [ ] **Step 3: Implement** atomic `0600` config/manifest writes, foreground process startup through a fixed helper, `/proc` ownership proof, local listener proof, SOCKS5 CONNECT probe, and TERM/KILL bounded cleanup only after ownership proof.
- [ ] **Step 4: Re-run** targeted and race tests; inspect generated configs to ensure credentials are absent from errors and ownership digests.
- [ ] **Step 5: Commit** the independent adapter.

### Task 4: Protocol-aware gates and proxy-bound DoH

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/platform/openwrt/doh_socks.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/openwrt/doh_socks_test.go`
- Modify: `proxypool-core/src/proxypoold/internal/live/gates.go`
- Modify: `proxypool-core/src/proxypoold/internal/live/gates_test.go`
- Modify: `proxypool-core/src/proxypoold/cmd/proxypoold/main.go`
- Modify: `proxypool-core/src/proxypoold/cmd/proxypoold/main_test.go`

**Interfaces:**
- Consumes: SOCKS5 dialer, pinned `model.DoHEndpoint`, protocol dispatcher.
- Produces: protocol-aware route gate and `NewSOCKSDoHTransport` with no direct fallback.

- [ ] **Step 1: Write failing tests** proving route open/verify/close are unchanged for L2TP and no-op for SOCKS5; proxy DoH opens TCP through the fake SOCKS5 server; auth/DNS failure cannot call a direct dialer.
- [ ] **Step 2: Run** targeted live/openwrt/main tests and observe expected failures.
- [ ] **Step 3: Implement** the protocol branches, SOCKS-backed HTTP transport, adapter dispatcher registration, and DNS factory switch.
- [ ] **Step 4: Re-run** targeted tests plus `go test ./internal/live ./internal/platform/openwrt ./cmd/proxypoold -count=1`.
- [ ] **Step 5: Commit** protocol-aware composition.

### Task 5: Expiring transparent redirect and accounting

**Files:**
- Modify: `proxypool-core/files/proxypool-guard.nft`
- Modify: `proxypool-core/files/proxypool-firewall-transaction`
- Modify: `proxypool-core/src/proxypoold/internal/platform/openwrt/nft.go`
- Modify: `proxypool-core/src/proxypoold/internal/platform/openwrt/nft_test.go`
- Modify: `scripts/test-proxypool-guard.sh`
- Modify: `scripts/test-firewall-defaults.sh`

**Interfaces:**
- Consumes: `AuthorizationLease.RedirectPort`.
- Produces: timeout-backed `(MAC,IPv4)->port` redirect, exact listener admission, and per-device proxy byte counters.

- [ ] **Step 1: Add failing behavior tests** that execute the guardian fixture and authorizer transactions, proving TCP redirects to the leased port, UDP/IPv6/private destinations do not redirect, expiration fails closed, and revoke removes redirect plus counters.
- [ ] **Step 2: Run** `sh scripts/test-proxypool-guard.sh` and `go test ./internal/platform/openwrt -run Authorizer -count=1`; confirm failures identify missing redirect/accounting state.
- [ ] **Step 3: Implement** the nft timeout map/accounting sets, transaction schema checks, publish/readback/revoke elements, and retain the existing post-DNAT exact tuple gate.
- [ ] **Step 4: Validate syntax** using the cached OpenWrt nft binary/environment, then rerun guard, firewall-default, and authorizer tests.
- [ ] **Step 5: Commit** redirect and accounting as one atomic firewall change.

### Task 6: SOCKS5 traffic reporting

**Files:**
- Modify: `proxypool-core/src/proxypoold/internal/platform/contracts.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/openwrt/proxy_traffic.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/openwrt/proxy_traffic_test.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/scheduler.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/scheduler_test.go`

**Interfaces:**
- Produces: session-aware proxy counter reader aggregated by node while retaining sysfs interface counters for L2TP.

- [ ] **Step 1: Write failing tests** with literal nft JSON counters for multiple devices/nodes, counter reset, malformed output, and scheduler protocol selection.
- [ ] **Step 2: Run** targeted traffic/scheduler tests and confirm the new SOCKS5 samples are unavailable before implementation.
- [ ] **Step 3: Implement** strict JSON parsing, node aggregation from owned authorization manifest, and scheduler selection by session protocol.
- [ ] **Step 4: Re-run** targeted tests and existing traffic tests.
- [ ] **Step 5: Commit** proxy traffic reporting.

### Task 7: LuCI activation and Chinese errors

**Files:**
- Modify: `luci-app-proxypool/luasrc/view/proxypool/main.htm`
- Modify: `luci-app-proxypool/htdocs/luci-static/resources/proxypool-v2.js`
- Modify: `luci-app-proxypool/tests/ui/proxypool-v2.test.mjs`
- Modify: `proxypool-core/src/proxypoold/internal/engine/state_machine.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/state_machine_test.go`

**Interfaces:**
- Consumes: existing node CRUD/import/status APIs.
- Produces: selectable SOCKS5 UI, five-field batch hint, Chinese auth/resolve/connect/probe states, unchanged device binding UX.

- [ ] **Step 1: Write failing UI/state tests** proving SOCKS5 is no longer labelled migration-only, supports blank paired credentials, and maps each safe error code to Chinese.
- [ ] **Step 2: Run** Node UI tests and Go state tests; confirm expected failures.
- [ ] **Step 3: Implement** only copy/protocol availability/error mappings; keep the existing binding list and navigation unchanged.
- [ ] **Step 4: Re-run** all LuCI UI tests and state tests.
- [ ] **Step 5: Commit** SOCKS5 UI activation.

### Task 8: Package integration and hot-device acceptance

**Files:**
- Modify: `proxypool-core/Makefile`
- Modify: `scripts/test-v2-live-integration.sh`
- Create: `scripts/device-test/socks5-acceptance.sh`
- Modify: diagnostic command list if SOCKS5 process evidence is missing.

**Interfaces:**
- Produces: installable core/LuCI IPKs and a repeatable router acceptance report.

- [ ] **Step 1: Write failing package/integration assertions** for the helper, redsocks dependency, private runtime paths, diagnostics redaction, dispatcher startup, and no legacy manager mutation.
- [ ] **Step 2: Run** the smallest host integration set and confirm missing artifacts fail.
- [ ] **Step 3: Implement** package install paths, upgrade-safe service restart, diagnostics evidence, and the device script checks for web, DNS, UDP/LAN/direct-WAN denial, reconnect/delete independence, and traffic movement.
- [ ] **Step 4: Run** all Go, shell, LuCI, release-contract, and package workflow tests; build IPKs on `m1068-proxypool`.
- [ ] **Step 5: Hot-update** the test router over SSH, run one real SOCKS5 node and one bound device, collect the acceptance report, and fix any reproduced failure with a new red-green cycle.
- [ ] **Step 6: Commit and push** the device-verified result. Start a full GL-MT6000 firmware build only after hot acceptance passes.
