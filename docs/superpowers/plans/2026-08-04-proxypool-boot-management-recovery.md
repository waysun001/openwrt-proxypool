# ProxyPool Boot Management Recovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep GL-MT6000 wired DHCP and HTTP/HTTPS management reachable when the early LAN-isolation proof fails, while retaining a fail-closed firewall that prevents LAN forwarding and local-WAN access.

**Architecture:** The existing S18 guardian already installs the independent nftables guardian and an fw4 quarantine sentinel before running LAN-isolation checks. On an isolation failure, return control to rcS with that fail-closed state intact instead of entering an infinite sleep that prevents netifd and dnsmasq from starting. The normal fully validated path remains unchanged and still releases the fw4 sentinel only after every proof succeeds.

**Tech Stack:** OpenWrt 23.05 rc.common shell, nftables/firewall4, POSIX shell integration tests.

## Global Constraints

- Management remains limited to DHCP and HTTP/HTTPS at `192.168.9.1`.
- Client forwarding and local-WAN fallback remain denied whenever isolation is not proven.
- No SSH or ICMP management exception is added.
- The successful S18 boot path and firewall transaction ordering remain unchanged.

---

### Task 1: Preserve management boot on LAN-isolation failure

**Files:**
- Modify: `scripts/test-proxypool-guard.sh`
- Modify: `proxypool-core/files/proxypool-guard.init`

**Interfaces:**
- Consumes: `reconcile_lan_isolation boot`, the installed independent guardian, and the regular `proxypool-fw4-quarantine-v1` sentinel.
- Produces: a prompt nonzero S18 return that leaves the sentinel and guardian installed but permits rcS to continue to netifd, dnsmasq, and uhttpd.

- [ ] **Step 1: Write the failing integration test**

Change the isolation-failure fixture to require a prompt return, require the quarantine sentinel to remain, and reject any invocation of the hold helper or later firewall recovery/release gates.

- [ ] **Step 2: Run the focused test to verify RED**

Run: `sh scripts/test-proxypool-guard.sh`

Expected: FAIL because the current S18 implementation invokes `hold_lan_boot_inhibited`.

- [ ] **Step 3: Implement the minimal boot change**

When `reconcile_lan_isolation boot` fails after `reset_guardian` succeeds, print a fail-closed recovery message and return nonzero without invoking the infinite hold. Do not release the sentinel or run transaction recovery.

- [ ] **Step 4: Run focused and full host verification**

Run: `sh scripts/test-proxypool-guard.sh`, then `sh scripts/test-host.sh`.

Expected: both PASS.

- [ ] **Step 5: Commit and push**

Stage only the plan, test, and guardian change; commit the boot-management fix and push `codex/proxypool-v2-phase1` so the full GL-MT6000 firmware workflow can build a replacement image.
