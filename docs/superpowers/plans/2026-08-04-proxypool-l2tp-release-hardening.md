# ProxyPool L2TP Release Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Produce a GL-MT6000 firmware in which ProxyPool reaches the live L2TP data plane after a cold boot and preserves the approved fail-closed, management-only recovery behavior whenever any safety proof or node path is unavailable.

**Architecture:** Retain the approved V2 single-writer daemon, shared netifd/xl2tpd adapter, expiring nft/route leases, node-bound DNS and independent guardian. Audit each boundary in boot order and make only root-cause-backed, test-first changes; do not enable SOCKS5/SLP or weaken LAN/Wi-Fi isolation to make startup pass.

**Tech Stack:** OpenWrt 23.05.3, GL-MT6000 mediatek/filogic, POSIX shell/ucode/ubus, Go 1.20, netifd/xl2tpd/PPP, nftables/firewall4, GitHub Actions.

## Global Constraints

- Up to 60 saved nodes; the live protocol in this release is L2TP only.
- LAN and Wi-Fi clients may use DHCP and LuCI HTTP/HTTPS but may not use SSH or reach each other.
- Client IPv6 and external UDP remain blocked.
- A bound device may reach public TCP only through its assigned, verified L2TP interface; no main-WAN or system-DNS fallback is permitted.
- Missing, stale, failed or ambiguous runtime state must revoke authorization before cleanup and remain fail closed.
- L2TP uses one procd-managed shared xl2tpd through OpenWrt netifd dynamic interfaces.

---

### Task 1: Repair the cold-boot wireless proof

**Files:**
- Modify: `scripts/test-lan-isolation-defaults.sh`
- Modify: `proxypool-core/files/lan-isolation.sh`

**Interfaces:**
- Consumes the complete result of `ubus -S list` and `ubus -S call network.wireless status`.
- Produces a positive wireless-down proof only when the ubus listing succeeds, no `hostapd.*` object exists and netifd reports no active/pending/autostart radio.

- [x] **Step 1: Write a failing regression fixture** that models OpenWrt ubus returning status 4 for an unmatched wildcard and requires the production probe to distinguish “no hostapd objects” from “ubus listing failed.”
- [x] **Step 2: Verify RED** with `sh scripts/test-lan-isolation-defaults.sh`; expected failure is that the production probe does not enumerate the ubus object set.
- [x] **Step 3: Implement the minimal fix** by listing all ubus objects once, rejecting command failure and rejecting any line beginning `hostapd.`.
- [x] **Step 4: Verify GREEN** with the LAN-isolation and guardian tests.

### Task 2: Audit boot and readiness ordering

**Files:**
- Inspect/modify if a failing test proves a defect: `proxypool-core/files/proxypool-guard.init`
- Inspect/modify if a failing test proves a defect: `proxypool-core/files/proxypool.init`
- Test: `scripts/test-proxypool-guard.sh`
- Test: `scripts/test-proxypool-init.sh`
- Test: `scripts/test-fw4-activate.sh`

**Interfaces:**
- S18 installs the independent guardian and quarantine before proving isolation.
- S99 starts the live daemon only after isolation, firewall activation, DNS admission and migration gates are current.

- [x] **Step 1: Trace every S18/S99 success and failure return** against the rc.d order and persisted sentinels.
- [x] **Step 2: Run the three integration tests** and record any failing boundary before changing production code.
- [x] **Step 3: For each confirmed defect, add one failing behavioral test, verify RED, implement the smallest correction and verify GREEN.**

### Task 3: Audit shared L2TP lifecycle and automatic recovery

**Files:**
- Inspect/modify if proven: `proxypool-core/src/proxypoold/internal/platform/openwrt/l2tp.go`
- Inspect/modify if proven: `proxypool-core/src/proxypoold/internal/engine/scheduler.go`
- Inspect/modify if proven: `proxypool-core/src/proxypoold/cmd/proxypoold/main.go`
- Test: matching Go `_test.go` files
- Test: `scripts/test-v2-l2tp-adapter.sh`
- Test: `scripts/test-v2-live-integration.sh`
- Test: `scripts/device-test/round3-bulk-recovery.sh`

**Interfaces:**
- Start creates one deterministic dynamic netifd interface per node without mutating global PPP secrets or restarting xl2tpd.
- Online requires owned interface identity, PPP IPv4, route readiness and health proof.
- Failure, timeout, shared-daemon restart and missed hotplug events converge through bounded retry without manual reconnect.

- [x] **Step 1: Trace add/status/remove and scheduler state transitions**, including cancellation, stale generation, concurrent reconnect and daemon restart.
- [x] **Step 2: Run focused Go and shell tests.**
- [x] **Step 3: Add RED tests and minimal fixes only for reproducible contract gaps, then re-run focused suites and leave the Linux race detector to CI.**

### Task 4: Audit device-to-node data-plane confinement

**Files:**
- Inspect/modify if proven: `proxypool-core/src/proxypoold/internal/platform/openwrt/devices.go`
- Inspect/modify if proven: `proxypool-core/src/proxypoold/internal/platform/openwrt/nft.go`
- Inspect/modify if proven: `proxypool-core/src/proxypoold/internal/platform/openwrt/route.go`
- Inspect/modify if proven: `proxypool-core/src/proxypoold/internal/dnsproxy/`
- Inspect/modify if proven: `proxypool-core/files/proxypool-firewall-transaction`
- Test: firewall, guardian, DNS, live integration and migration suites under `scripts/`

**Interfaces:**
- Device identity is discovered MAC plus managed IPv4; users do not type MAC addresses.
- Authorization is an expiring exact MAC/IP/interface tuple and is published only after route and node-bound DNS are ready.
- Revocation precedes route, DNS and interface teardown.

- [x] **Step 1: Trace bind, DHCP lease, policy mark, route table, nft tuple and DNS channel publication/revocation.**
- [x] **Step 2: Verify no default route, resolver or permissive firewall path can bypass the assigned L2TP interface.**
- [x] **Step 3: Turn each confirmed gap into a failing test and minimal fix, then run all data-plane contract suites.**

### Task 5: Release verification, push and firmware build

**Files:**
- Modify: this plan and any root-cause-backed test/production files from Tasks 1–4.
- Verify: `.github/workflows/build-fast.yml`
- Verify: `.github/workflows/build.yml`

**Interfaces:**
- The fast SDK workflow proves package compilation and payload inspection.
- The full workflow builds the pinned OpenWrt tree with the two MT7531 isolation patches and uploads one unique GL-MT6000 sysupgrade image plus SHA256 evidence.

- [ ] **Step 1: Run `sh scripts/test-host.sh`, `go test -race -count=1 ./...`, `go vet ./...`, shell syntax checks and `git diff --check`.**
- [x] **Step 2: Review the final diff for unrelated changes and secrets.**
- [ ] **Step 3: Commit and push `codex/proxypool-v2-phase1`.**
- [ ] **Step 4: Dispatch both workflows at the pushed commit and use the SDK result as the early package diagnostic.**
- [ ] **Step 5: On full-build success, download the artifact, verify `SHA256SUMS` and identify the unique `*glinet_gl-mt6000*squashfs-sysupgrade.bin`.**
