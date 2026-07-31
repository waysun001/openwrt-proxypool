# ProxyPool V2 Phase 3 Isolation and Device Binding Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 建立从开机即生效的 fail-closed 基线、自动 DHCP/MAC 设备绑定、GL-MT6000 有线/无线隔离和只从指定 L2TP PPP 接口转发的数据面。

**Architecture:** fw4 持久 include 提供不依赖 daemon 的默认拒绝；daemon 在 bridge ingress 按 MAC 赋 policy mark，原子发布当前 generation 的 nftables 授权，并为每个在线 L2TP 节点建立独立策略路由表。授权总是最后发布、最先撤销。

**Tech Stack:** Go 1.20、nftables、fw4、iproute2、Linux bridge/DSA、hostapd/netifd、dnsmasq DHCP、ubus、GL-MT6000。

## Global Constraints

- 继承路线图约束；本阶段只开放 L2TP 数据面，SOCKS5/SLP 仍明确不可用。
- 基础规则不依赖 `proxypoold`；daemon 崩溃或 firewall reload 后终端默认断网。
- 管理白名单仅允许到路由器自身 IPv4 的 DHCP、ProxyPool DNS、LuCI 80/443 和必要 ARP；在 Phase 4 DNS listener 就绪前，DNS 动态入口保持关闭，绝不转给系统 dnsmasq 上游。
- 客户端对其他本地/私有/链路本地/组播地址拒绝；客户端 IPv6 和外部 UDP 拒绝。
- MAC 是设备策略主键，固定 IPv4 是 DHCP 管理地址；绑定时不要求用户输入 MAC。
- 规则切换顺序必须通过故障注入测试证明没有 LAN->WAN 窗口。

---

### Task 1: 渲染并测试永久 fw4 fail-closed 基线

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/dataplane/base_rules.go`
- Create: `proxypool-core/src/proxypoold/internal/dataplane/base_rules_test.go`
- Create: `proxypool-core/src/proxypoold/internal/dataplane/testdata/base-rules.nft`
- Create: `proxypool-core/files/proxypool-fw4.include`
- Create: `proxypool-core/files/99-proxypool-firewall`
- Modify: `proxypool-core/Makefile`

**Interfaces:**
- Produces: deterministic base table/chain names and persistent fw4 include installation.

- [ ] **Step 1: Write golden tests for base policy**

Golden assertions must include: LAN->forward final drop, IPv6 forward drop, client external UDP drop, router-address-only DHCP/LuCI accepts, a default-closed ProxyPool DNS jump, no RFC1918 peer accept, and empty dynamic authorization sets after fw4 reload.

```go
func TestBaseRulesNeverAllowLANToWAN(t *testing.T) {
    got := RenderBaseRules(BaseInput{LANDevice: "br-lan", RouterIPv4: "192.168.9.1"})
    requireOrdered(t, got, "jump proxypool_authorized", `iifname "br-lan" counter drop`)
}
```

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/dataplane -run TestBaseRules -v
```

- [ ] **Step 3: Implement a syntactically complete nft include**

Use fixed owned names `inet proxypool` and `bridge proxypool_bridge`. The inet input chain allows only destination equal to configured router LAN IPv4 and ready services. DNS 53 enters a dynamic chain that is empty until Phase 4 publishes the ProxyPool DNS redirect; it must never fall through to system dnsmasq. The forward chain jumps to a dynamic chain and then drops all remaining LAN-origin traffic. Do not reference `firewall.@zone[1]` or positional UCI sections.

- [ ] **Step 4: Install idempotently through named UCI include**

The uci-default script finds/creates a section named `proxypool_v2`, type `nftables`, path `/usr/share/proxypool/proxypool-fw4.include`. While backend is V1/shadow it stages the section disabled and does not reload; `system.activate` runs syntax checks, enables the include and reloads it before stopping V1. A fresh final V2 image enables it on first boot. On check failure, retain an emergency minimal LAN->WAN drop include and return nonzero.

- [ ] **Step 5: Verify nft syntax in an OpenWrt test root and commit**

```bash
./scripts/test-host.sh
# On OpenWrt/SDK test root:
fw4 check
nft -c -f /usr/share/proxypool/proxypool-fw4.include
git diff --check
git add proxypool-core
git commit -m "feat: install permanent fail-closed firewall baseline"
```

### Task 2: 实现原子 nftables generation 和策略路由 reconciler

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/platform/nft.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/nft_test.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/route.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/route_test.go`
- Create: `proxypool-core/src/proxypoold/internal/dataplane/reconciler_linux.go`
- Create: `proxypool-core/src/proxypoold/internal/dataplane/reconciler_test.go`

**Interfaces:**
- Consumes: `dataplane.Snapshot`, Phase 2 L2TP observed interface.
- Produces: atomic `Publish`, `RevokeNode`, `Inspect`; use of the immutable node `PolicyID` from Phase 1.

- [ ] **Step 1: Write operation-order failure tests**

Prove publish performs route preparation before nft authorization; revoke removes nft authorization before routes; nft failure leaves no authorization; route cleanup failure cannot reauthorize; stale revision cannot replace a newer generation.

```go
func TestPublishNeverAuthorizesBeforeRoute(t *testing.T) {
    rec := newRecorder()
    _ = reconciler.Publish(ctx, snapshot)
    rec.RequireOrder(t, "route.ensure", "nft.atomic-publish")
}
```

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/platform ./internal/dataplane -run 'TestPublish|TestRevoke' -v
```

- [ ] **Step 3: Implement safe argv runners and owned-object parsing**

`nft -j list table` and `ip -j rule/route` output is parsed as JSON. Writes use complete `nft -f -` transactions and explicit `ip rule add/del` argv. Only touch tables/rules carrying ProxyPool fixed names, priority range and protocol marker; never flush global rules or main table.

- [ ] **Step 4: Implement policy allocation and exact L2TP checks**

Use `model.Node.PolicyID`, allocated as the smallest free value 1～60 at node creation and never changed. Derive mark `0x50000000 | PolicyID` and route table `10000 + PolicyID`. L2TP authorized rule requires both exact mark and exact observed PPP `oifname`; its table has a default route only through that PPP interface.

- [ ] **Step 5: Verify atomic fixtures and commit**

```bash
cd proxypool-core/src/proxypoold
go test -race ./internal/platform ./internal/dataplane ./internal/model
git add internal/platform internal/dataplane internal/model
git commit -m "feat: atomically publish l2tp policy dataplane"
```

### Task 3: 自动发现 DHCP 设备并原子管理固定租约

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/device/inventory.go`
- Create: `proxypool-core/src/proxypoold/internal/device/inventory_test.go`
- Create: `proxypool-core/src/proxypoold/internal/device/reservation.go`
- Create: `proxypool-core/src/proxypoold/internal/device/reservation_test.go`
- Create: `proxypool-core/src/proxypoold/internal/device/testdata/dhcp.leases`
- Create: `proxypool-core/src/proxypoold/internal/api/device_methods.go`
- Create: `proxypool-core/src/proxypoold/internal/api/device_methods_test.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/engine.go`

**Interfaces:**
- Produces: `Inventory.List`, `Inventory.Events`, `ReservationStore.Bind`, `Unbind`, runtime `AllowedIPv4`.

- [ ] **Step 1: Write discovery/validation tests**

Cover dnsmasq lease parsing, uppercase/lowercase MAC normalization, expired lease filtering, hostname sanitation, neighbor enrichment, duplicate IP, requested address outside LAN, router address, active foreign lease and no-MAC binding rejection.

```go
func TestBindRequiresDiscoveredMAC(t *testing.T) {
    _, err := reservations.Bind(ctx, BindRequest{CurrentIP: netip.MustParseAddr("192.168.9.77")})
    assertCode(t, err, "not_found")
}
```

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/device -v
```

- [ ] **Step 3: Implement inventory without shell parsing**

Read `/tmp/dhcp.leases` directly, query ubus/neighbor through typed platform clients, merge by normalized MAC and keep last-seen/current IPv4. Static-IP-only unknown devices are listed as unconfirmed and cannot be bound until DHCP supplies a MAC/IP identity.

- [ ] **Step 4: Implement DHCP reservation transaction**

Create named `config host 'proxypool_<deviceID>'` with MAC, IPv4 and hostname in a staged DHCP config, validate with `uci -c <stage> show dhcp`, atomically replace/commit, then call `ubus call service event '{"type":"config.change","data":{"package":"dhcp"}}'`. On failure, roll back both DHCP and ProxyPool expected config.

- [ ] **Step 5: Preserve transition IP safely**

After binding, runtime `AllowedIPv4` contains current lease plus desired fixed IP until DHCP confirms the fixed lease, with a maximum transition deadline of one lease interval. Both addresses remain tied to the same MAC mark; unbound devices receive no internet mark.

- [ ] **Step 6: Add device list/bind/unbind RPC methods**

`device.list` returns discovered and configured devices without secrets. `device.bind` accepts discovered device ID, node ID, optional fixed IPv4 and expected revision; it never accepts a caller-supplied MAC. `device.unbind` clears node authorization but retains the discovered device/reservation according to an explicit `keep_reservation` boolean.

- [ ] **Step 7: Verify and commit**

```bash
cd proxypool-core/src/proxypoold
go test -race ./internal/device ./internal/engine ./internal/api
git add internal/device internal/engine internal/api
git commit -m "feat: discover and reserve bound devices automatically"
```

### Task 4: 按 MAC 分类、执行 IP source guard 并接通 L2TP

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/dataplane/device_rules.go`
- Create: `proxypool-core/src/proxypoold/internal/dataplane/device_rules_test.go`
- Modify: `proxypool-core/src/proxypoold/internal/dataplane/reconciler_linux.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/coordinator.go`
- Create: `tests/integration/fake-openwrt/nft`
- Create: `tests/integration/fake-openwrt/ip`
- Create: `tests/integration/fail_closed_test.sh`

**Interfaces:**
- Consumes: Device MAC/AllowedIPv4/PolicyID and L2TP online observed interface.
- Produces: bridge mark classification, IP source guard, authorized L2TP forward path.

- [ ] **Step 1: Write golden rules and integration failure tests**

Cases: bound correct MAC/IP/PPP accepted; unbound dropped; node offline dropped; correct mark/wrong PPP dropped; spoofed IP dropped; device moved from node A to B never has both authorizations; daemon killed leaves base drop.

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/dataplane -run TestDeviceRules -v
cd ../../..
bash tests/integration/fail_closed_test.sh
```

- [ ] **Step 3: Render bridge and inet dynamic chains**

Bridge prerouting maps `ether saddr` to policy mark and copies it to `ct mark`; established packets restore only marks belonging to current generation. Inet prerouting checks source IPv4 against the MAC-associated allowed set. Forward authorization is `mark + exact oifname`; all nonmatching LAN traffic reaches persistent drop.

- [ ] **Step 4: Wire online/offline order into coordinator**

Online: adapter validation -> route ensure -> nft publish -> state online. Offline: nft revoke -> state degraded/offline -> route remove -> adapter stop. Until Phase 4, L2TP hardware checks use an IPv4 destination and client DNS remains deliberately unavailable. If any publish step fails, state is `dataplane_failed`, device stays offline and retry operates on node without enabling WAN.

- [ ] **Step 5: Verify and commit**

```bash
./scripts/test-host.sh
bash tests/integration/fail_closed_test.sh
git diff --check
git add proxypool-core/src/proxypoold tests/integration
git commit -m "feat: bind device mac policy to exact l2tp interface"
```

### Task 5: 实现 GL-MT6000 有线端口与无线客户端二层隔离

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/platform/bridge.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/bridge_test.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/wireless.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/wireless_test.go`
- Create: `proxypool-core/files/99-proxypool-network-isolation`
- Modify: `proxypool-core/Makefile`
- Create: `scripts/device-test/isolation-preflight.sh`

**Interfaces:**
- Produces: discovered LAN bridge ports, idempotent DSA isolated flags, Wi-Fi isolate/bridge_isolate configuration, verification report.

- [ ] **Step 1: Write platform parsing and idempotence tests**

Fixtures contain lan1～lan4, br-lan, WAN and wireless interfaces. Assert only LAN member ports become isolated, CPU/bridge self and WAN are untouched, every enabled terminal AP has `isolate=1` and `bridge_isolate=1`, and a second reconcile emits no changes.

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/platform -run 'TestBridge|TestWireless' -v
```

- [ ] **Step 3: Implement discovery and reapply on network events**

Read network device status via ubus, then execute `bridge link set dev <lan-port> isolated on` with argv. Subscribe to network reload events and reapply. Stage wireless UCI changes by section name, preserving SSID/security; do not modify STA/mesh interfaces.

- [ ] **Step 4: Add bridge nft fallback**

The bridge forward hook rejects frames whose input and output are both discovered terminal-facing ports/radios. DHCP/ARP to bridge self does not traverse this forward hook and remains available.

- [ ] **Step 5: Create hardware preflight and commit**

Preflight prints bridge links, wireless isolation values and nft counters, then asks the operator to test each port/radio pair. It makes no destructive changes itself.

```bash
./scripts/test-host.sh
git add proxypool-core scripts/device-test/isolation-preflight.sh
git commit -m "feat: isolate wired and wireless client segments"
```

### Task 6: 添加 V2 激活事务和隔离预验收

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/engine/activation.go`
- Create: `proxypool-core/src/proxypoold/internal/engine/activation_test.go`
- Modify: `proxypool-core/files/proxypool.init`
- Modify: `proxypool-core/files/proxypool.config`
- Create: `scripts/device-test/phase3-activation.sh`
- Create: `docs/testing/phase3-isolation-precheck.md`

**Interfaces:**
- Produces: guarded `system.activate` flow from V1/shadow to L2TP-capable V2.

- [ ] **Step 1: Write activation interruption tests**

For every boundary—base install, fw4 check, V1 stop, V1 cleanup, V2 start, reconciliation—inject failure and assert final state is either V1 still active before cutover or V2 fail-closed after cutover. No case may leave LAN->WAN allowed.

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/engine -run TestActivation -v
```

- [ ] **Step 3: Implement explicit activation state file**

Persist only coarse phase (`v1`, `base_installed`, `v1_stopped`, `v2_active`) in `/etc/proxypool/runtime-backend`, fsync each transition, and make init recovery idempotent. Never infer activation from a PID file alone.

- [ ] **Step 4: Run host/SDK verification and hardware isolation precheck**

```bash
./scripts/test-host.sh
bash tests/integration/fail_closed_test.sh
# SDK package build, upload scripts/device-test to the test router, then:
/tmp/proxypool-test/phase3-activation.sh precheck
```

Verify LuCI 80/443 remains reachable, peer ports/radios are blocked, unbound devices have no internet, and a bound L2TP device loses internet immediately when PPP is stopped.

- [ ] **Step 5: Commit phase gate**

```bash
git add proxypool-core scripts/device-test docs/testing
git commit -m "feat: activate l2tp v2 behind fail-closed guard"
```

## Phase 3 Exit Gate

- [ ] Permanent fw4 base exists and `fw4 check` passes with daemon stopped.
- [ ] Unbound device can use DHCP/LuCI but cannot reach WAN or another terminal; DNS fails locally without an upstream leak until Phase 4.
- [ ] Bound L2TP device uses its exact PPP; wrong/missing PPP produces drop, not main-table fallback.
- [ ] LAN-LAN、同 Wi-Fi、跨 Wi-Fi、有线-无线预检均隔离。
- [ ] Device binding is completed from discovered lease without manual MAC input.
- [ ] Activation fault-injection proves every interrupted stage ends in V1-before-cutover or V2 fail-closed.
