# ProxyPool V2 Live L2TP Vertical Slice Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在不重新启用 V1 的前提下，让一个自动发现的设备绑定一个真实 L2TP 节点，经节点 DNS 和节点 PPP 接口进行 TCP 上网，并在任意失败时自动撤权和恢复。

**Architecture:** 将现有只读 `engine.Shadow` 保留为迁移诊断入口，新增正式 `engine.Controller`、持久 runtime snapshot、限并发 scheduler 及窄平台接口。OpenWrt L2TP 适配器通过 `ubus network add_dynamic/del_dynamic` 创建每节点 `proto=l2tp` 接口，复用 OpenWrt 23.05 官方 `l2tp.sh` 管理的共享 xl2tpd；DNS listener 和 nft 租约只在节点、DNS、路由、MAC/IP tuple 全部验证后开放。

**Tech Stack:** Go 1.20.14、OpenWrt 23.05.3 netifd/ubus/xl2tpd、nftables、procd、UCI、LuCI Lua/JavaScript、POSIX shell 测试夹具。

## Global Constraints

- 目标设备固定为 GL-MT6000，目标系统固定为 OpenWrt 23.05.3。
- 旧 V1 manager、watchdog、PPP 全局 hook 和 LuCI 写入口继续隔离，不作为临时回退。
- 客户端只能访问 DHCP、路由器 TCP/UDP 53、管理页 TCP 80/443，以及经过当前节点授权的 TCP；外部 UDP 和 IPv6 永远丢弃。
- 节点、DNS、daemon、接口、路由或授权状态不确定时保持断网，绝不使用主 WAN 回退。
- 一个节点可绑定多个设备；一个设备同一时间只绑定一个节点；用户不手工输入 MAC。
- 所有新行为遵循 RED→GREEN→REFACTOR；每个任务独立提交。
- 当前规则不允许使用子代理，本计划在当前会话内按任务顺序执行。
- OpenWrt 23.05 官方 L2TP 协议脚本参考：`https://raw.githubusercontent.com/openwrt/packages/openwrt-23.05/net/xl2tpd/files/l2tp.sh`。

## File Structure

- `internal/engine/runtime_store.go`：root-only runtime snapshot 的严格 JSON codec 与原子替换；与 jobs/status 同包，避免 `engine`/`persist` 循环依赖。
- `internal/engine/controller.go`：正式 V2 API、配置 revision、状态摘要和 scheduler 生命周期。
- `internal/engine/scheduler.go`：按协议限并发、deadline、generation、恢复与事件持久化。
- `internal/platform/contracts.go`：设备、L2TP、DNS dial、授权发布的窄接口和 DTO。
- `internal/platform/openwrt/runner.go`：无 shell 拼接的 argv 命令执行器。
- `internal/platform/openwrt/devices.go`：DHCP/ubus 设备发现和稳定租约事务。
- `internal/platform/openwrt/l2tp.go`：netifd 动态 L2TP 接口生命周期与所有权验证。
- `internal/dnsproxy/server.go`：按源 IPv4 选择节点通道的 TCP/UDP 53 listener。
- `internal/platform/openwrt/nft.go`：短时授权租约的原子 nft 发布和撤销。
- `proxypool-core/files/proxypool-guard.nft`：DNS tuple 与带 timeout 的 V2 动态集合。
- `cmd/proxypoold/main.go`、`proxypool.init`：正式 `--live` daemon 和平台依赖装配。
- LuCI controller/view/JS：设备发现、绑定、节点操作和作业进度。

---

### Task 1: Durable Runtime Snapshot

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/engine/runtime_store.go`
- Create: `proxypool-core/src/proxypoold/internal/engine/runtime_store_test.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/jobs.go`
- Test: `proxypool-core/src/proxypoold/internal/engine/jobs_test.go`

**Interfaces:**
- Produces: `engine.RuntimeStore.Load() (engine.RuntimeSnapshot, error)` and `Save(context.Context, engine.RuntimeSnapshot) error`.
- Snapshot contains schema version, config revision, jobs, node events, node statuses and next event sequence; it contains no node credentials.
- `engine.JobStore.Snapshot()` and `Restore(engine.JobSnapshot) error` serialize existing bounded job/event state without exposing internal maps.

- [ ] **Step 1: Write failing persistence tests**

Add tests proving: exact round trip, mode `0600`, file and directory sync before/after rename, corrupt/unknown schema rejection, symlink rejection, duplicate job/event rejection, cancelled context zero mutation, and crash-old-or-new atomicity. A credential-shaped sentinel placed in desired node data must never appear in the runtime snapshot.

- [ ] **Step 2: Verify RED**

Run:

```sh
cd proxypool-core/src/proxypoold
go test ./internal/engine -run 'Runtime|Snapshot|Restore' -count=1
```

Expected: FAIL because runtime snapshot interfaces do not exist.

- [ ] **Step 3: Implement the minimal store**

Use strict JSON with `DisallowUnknownFields`, schema `1`, bounded array sizes, explicit duplicate detection and an injectable filesystem interface matching `config.Store` durability semantics. Encode through a `0600` temporary file, sync file, close, decode/validate, rename and sync the parent directory.

- [ ] **Step 4: Verify GREEN and regression**

Run the focused command, then `go test ./... -count=1` and `go vet ./...`.

- [ ] **Step 5: Commit**

```sh
git add proxypool-core/src/proxypoold/internal/engine/runtime_store* proxypool-core/src/proxypoold/internal/engine/jobs.go proxypool-core/src/proxypoold/internal/engine/jobs_test.go
git commit -m "feat: persist v2 runtime jobs safely"
```

### Task 2: Live Controller and Strict Write API

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/engine/controller.go`
- Create: `proxypool-core/src/proxypoold/internal/engine/controller_test.go`
- Modify: `proxypool-core/src/proxypoold/cmd/proxypoold/main.go`
- Modify: `proxypool-core/src/proxypoold/cmd/proxypoold/main_test.go`
- Modify: `proxypool-core/src/proxypoold/cmd/proxypoolctl/main.go`
- Modify: `proxypool-core/src/proxypoold/cmd/proxypoolctl/main_test.go`

**Interfaces:**
- Consumes: `config.Store`, `engine.RuntimeStore`, `engine.Machine`, `engine.JobStore`.
- Produces API methods `status.get`, `device.list`, `device.bind`, `device.unbind`, `node.action`, `job.get`, `job.list`, `system.events`.
- Produces daemon mode `--live --config PATH --state PATH --socket PATH`; `--shadow` remains mutually exclusive.

- [ ] **Step 1: Write failing controller/API tests**

Assert strict per-method JSON schemas, duplicate/unknown field rejection, revision conflicts, redacted status, bind-to-missing-node rejection, one-device/one-node replacement semantics, idempotent request IDs, job creation only after config persistence, and restart loading the same jobs/generations. Assert `--live` cannot be combined with `--shadow`, cannot omit `--state`, and does not silently fall back to shadow.

- [ ] **Step 2: Verify RED**

```sh
cd proxypool-core/src/proxypoold
go test ./internal/engine ./cmd/proxypoold ./cmd/proxypoolctl -run 'Controller|Live|Bind|NodeAction|Job' -count=1
```

Expected: FAIL because `Controller` and `--live` are absent.

- [ ] **Step 3: Implement minimal controller**

Use explicit request DTOs, `json.Decoder.DisallowUnknownFields`, optimistic `expected_revision`, and a bounded idempotency cache persisted in the runtime snapshot. Never include password, SLP token or obfs key in responses or errors. The controller initially queues work but does not call a platform adapter until Task 4.

- [ ] **Step 4: Verify GREEN and API regression**

Run focused tests, `go test ./internal/api ./internal/engine ./cmd/... -count=1`, and `go vet ./...`.

- [ ] **Step 5: Commit**

```sh
git add proxypool-core/src/proxypoold/internal/engine/controller* proxypool-core/src/proxypoold/cmd
git commit -m "feat: add strict live v2 control API"
```

### Task 3: Automatic Device Discovery and Stable Binding

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/platform/contracts.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/openwrt/runner.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/openwrt/runner_test.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/openwrt/devices.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/openwrt/devices_test.go`
- Modify: `proxypool-core/src/proxypoold/internal/model/runtime.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/controller.go`

**Interfaces:**
- Produces `platform.DeviceSource.List(context.Context) ([]platform.DiscoveredDevice, error)`.
- Produces `platform.LeaseManager.Apply(context.Context, model.Device, uint64) error` and `Remove(...) error`.
- `DiscoveredDevice` contains normalized MAC, IPv4, hostname, ingress and last-seen timestamp; no caller supplies a MAC string during ordinary UI binding.

- [ ] **Step 1: Write failing discovery/lease tests**

Use real parsers over fixture `/tmp/dhcp.leases` and ubus JSON. Cover malformed/duplicate MAC, duplicate IPv4, hostname control bytes, expired lease, LAN/Wi-Fi ingress, command timeout, argv injection strings, pending UCI delta and dnsmasq reload failure. Assert lease failure does not publish a V2 device binding.

- [ ] **Step 2: Verify RED**

```sh
cd proxypool-core/src/proxypoold
go test ./internal/platform/openwrt ./internal/engine -run 'Device|Lease|Runner' -count=1
```

Expected: FAIL because OpenWrt platform types do not exist.

- [ ] **Step 3: Implement parsers and transactional lease manager**

Runner accepts executable plus an argv slice only; it never accepts a shell command string. Device discovery merges leases and ubus by normalized MAC and rejects ambiguity. LeaseManager stages a named `host` section, verifies exact UCI projection, commits only `dhcp`, reloads dnsmasq, then re-reads the live lease before the controller commits the binding. If stable-lease creation succeeds but desired-config persistence fails, no traffic authorization is published; the controller records cleanup intent and reconciles/removes the orphaned owned host section on the same run and after restart.

- [ ] **Step 4: Verify GREEN and fail-closed integration**

Run focused tests plus `scripts/test-firewall-defaults.sh` focus for pending UCI deltas and `scripts/test-status-readonly.sh`.

- [ ] **Step 5: Commit**

```sh
git add proxypool-core/src/proxypoold/internal/platform proxypool-core/src/proxypoold/internal/model/runtime.go proxypool-core/src/proxypoold/internal/engine/controller.go
git commit -m "feat: discover and bind client devices"
```

### Task 4: Durable Scheduler and Adapter Boundary

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/engine/scheduler.go`
- Create: `proxypool-core/src/proxypoold/internal/engine/scheduler_test.go`
- Modify: `proxypool-core/src/proxypoold/internal/platform/contracts.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/controller.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/state_machine.go`

**Interfaces:**
- Produces `platform.NodeAdapter.Start/Probe/Stop(context.Context, platform.NodeRequest)`.
- Produces `platform.Session{NodeID, Generation, Protocol, Interface, LocalPort, StartedAt, OwnershipDigest}`.
- Scheduler consumes persisted jobs and emits existing state-machine events; it never treats adapter return alone as `online`.

- [ ] **Step 1: Write failing scheduler tests**

Cover L2TP concurrency exactly 4, proxy concurrency exactly 8, per-node deadline, single-node failure isolation, daemon restart recovery, stale generation completion, manual reconnect coalescing, stop-before-start cleanup, shared-adapter failure fan-out, cancellation and persist-before-side-effect ordering. A fake adapter records real calls; tests must assert state and persisted snapshots rather than mock call counts alone.

- [ ] **Step 2: Verify RED**

```sh
cd proxypool-core/src/proxypoold
go test ./internal/engine -run 'Scheduler' -count=1
```

Expected: FAIL because `Scheduler` does not exist.

- [ ] **Step 3: Implement scheduler**

Use one owner goroutine, bounded protocol semaphores and contexts derived from configured deadlines. Persist queued/running generation before `Start`; after start, require `Probe`, DNS readiness and authorization publication events before `EventValidated`. On any error call revoke first, then stop, persist backoff and release capacity.

- [ ] **Step 4: Verify GREEN under race detector**

```sh
cd proxypool-core/src/proxypoold
go test -race ./internal/engine -run 'Scheduler|Machine|Job' -count=1
go test ./... -count=1
```

- [ ] **Step 5: Commit**

```sh
git add proxypool-core/src/proxypoold/internal/engine proxypool-core/src/proxypoold/internal/platform/contracts.go
git commit -m "feat: schedule v2 node lifecycles durably"
```

### Task 5: Expiring Guardian Authorization and Router DNS Admission

**Files:**
- Modify: `proxypool-core/files/proxypool-guard.nft`
- Modify: `proxypool-core/files/proxypool-firewall-transaction`
- Create: `proxypool-core/src/proxypoold/internal/platform/openwrt/nft.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/openwrt/nft_test.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/openwrt/route.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/openwrt/route_test.go`
- Modify: `scripts/test-guardian-terminal-policy.sh`
- Modify: `scripts/test-proxypool-guard.sh`
- Modify: `scripts/test-firewall-defaults.sh`

**Interfaces:**
- Produces `platform.Authorizer.Publish(context.Context, platform.AuthorizationLease) error`, `RevokeNode(...)`, and `RevokeAll(...)`.
- Produces `platform.RouteManager.Install/Verify/Remove(context.Context, platform.RouteLease) error` for ProxyPool-owned `ip rule` and per-node route tables.
- Lease contains exact MAC, IPv4, policy ID, generation, interface/redirect port and expiry; expiry is fixed to 20 seconds and refreshed before 10 seconds.
- Policy mark is exactly `0x005a0000 | policyID`, where policy ID is `1..65535`; no unmarked or stale-marked packet is allowed to use the node route.

- [ ] **Step 1: Write failing nft contract tests**

Require timeout-enabled V2 L2TP/return/redirect sets, a new `v2_dns_clients` MAC+IPv4 set, and a timeout-enabled `v2_policy_marks` map keyed by MAC+IPv4. Require `guard_prerouting` to clear the safety bits first and then derive the exact mark only from that map. Require input rules allowing TCP/UDP 53 only for an admitted tuple, after management rules but before private-destination and terminal `br-lan` drops. Test expired/missing tuple, wrong MAC, wrong IPv4, wrong mark, wrong interface, stale generation manifest, nft failure and daemon crash without refresh. Ensure external UDP forward remains terminally dropped.

Add route-manager tests for exact owned `ip rule` masks/tables, a single default route through the verified PPP interface, pre-existing foreign rule/table rejection, no main-table fallback, read-back verification, rollback after partial failure and idempotent removal.

- [ ] **Step 2: Verify RED**

```sh
sh scripts/test-guardian-terminal-policy.sh
sh scripts/test-proxypool-guard.sh
PROXYPOOL_TEST_FOCUS_ACTIVATION_HELPER=1 sh scripts/test-firewall-defaults.sh
```

Expected: FAIL because dynamic sets have no timeout and router DNS tuple does not exist.

- [ ] **Step 3: Implement guardian schema and authorizer**

Add exact timeout flags/defaults and update the runtime verifier schema. OpenWrt authorizer validates DTOs, renders one bounded nft transaction to stdin, checks exit status, then reads back exact elements. The authorizer keeps generation in a root-only ownership manifest and refuses to refresh an older generation; nft timeout is the crash-safe stale-state boundary.

RouteManager installs and verifies an exact `fwmark 0x005aNNNN/0x00ffffff` rule and a dedicated per-policy table containing only the route required for the verified PPP device. Publish the policy-mark map and forwarding tuples only after route verification; on failure revoke nft authorization first, then remove the owned rule/table. It must never build a shell command, replace static guardian chains or use the main routing table as fallback.

- [ ] **Step 4: Verify GREEN**

Run the focused commands, Go nft tests, then the complete firewall defaults and guardian matrices.

- [ ] **Step 5: Commit**

```sh
git add proxypool-core/files/proxypool-guard.nft proxypool-core/files/proxypool-firewall-transaction proxypool-core/src/proxypoold/internal/platform/openwrt/nft* proxypool-core/src/proxypoold/internal/platform/openwrt/route* scripts/test-guardian-terminal-policy.sh scripts/test-proxypool-guard.sh scripts/test-firewall-defaults.sh
git commit -m "feat: publish expiring v2 traffic leases"
```

### Task 6: Per-Node DNS Listener and L2TP-Bound DoH

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/dnsproxy/server.go`
- Create: `proxypool-core/src/proxypoold/internal/dnsproxy/server_test.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/openwrt/dial.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/openwrt/dial_test.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/controller.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/scheduler.go`
- Modify: `proxypool-core/files/dns-manager.sh`
- Modify: `scripts/test-dns-fail-closed.sh`
- Modify: `proxypool-core/Makefile`

**Interfaces:**
- Produces `dnsproxy.Server.SetBindings(map[netip.Addr]dnsproxy.NodeChannel)` and `Run(context.Context) error` for UDP/TCP 53.
- `NodeChannel.Resolve(context.Context, []byte) ([]byte, error)` sends DNS wire messages through a per-node HTTPS transport whose dialer is bound to the verified L2TP interface.
- `dns-manager.sh` transactionally sets the unique dnsmasq section to `port=0` and `noresolv=1`, deletes all explicit upstream servers, restarts and verifies dnsmasq; DHCP remains active while dnsmasq no longer owns port 53.

- [ ] **Step 1: Write failing DNS tests**

Run real UDP and TCP listeners on loopback. Cover source-IP mapping, unbound source refusal, cross-node cache separation, DNS ID mismatch, truncated/oversize frames, DoH status/content-type errors, TLS server-name/bootstrap behavior, interface-bind failure, deadline, cancellation and removal of a node channel while queries are active. Assert no fallback call reaches the default resolver/dialer. Extend shell tests so dnsmasq DNS relinquishment is an atomic `port=0` transition, DHCP stays running, restart/read-back failure closes router DNS admission, and no restore path re-enables WAN DNS.

- [ ] **Step 2: Verify RED**

```sh
cd proxypool-core/src/proxypoold
go test ./internal/dnsproxy ./internal/platform/openwrt -run 'DNS|DoH|Dial' -count=1
```

Expected: FAIL because DNS packages do not exist.

- [ ] **Step 3: Implement minimal DNS/DoH path**

First make `dns-manager.sh` persist and verify `port=0`, `noresolv=1` and no explicit server on the unique dnsmasq section; restart failure stops dnsmasq and keeps guardian DNS admission closed. Only after that verified handoff may proxypoold bind router address `192.168.9.1` TCP/UDP 53. Validate DNS wire length and transaction ID without logging query contents. Use standard `net/http` DoH POST with an injected transport; production dial uses `SO_BINDTODEVICE` for the current verified PPP interface and dials the configured bootstrap IP with configured TLS ServerName. Do not call `net.DefaultResolver` or read resolv.conf.

- [ ] **Step 4: Verify GREEN and DNS fail-closed regression**

Run focused Go tests, `scripts/test-dns-fail-closed.sh`, and `go test -race ./internal/dnsproxy ./internal/engine`.

- [ ] **Step 5: Commit**

```sh
git add proxypool-core/src/proxypoold/internal/dnsproxy proxypool-core/src/proxypoold/internal/platform/openwrt/dial* proxypool-core/src/proxypoold/internal/engine proxypool-core/files/dns-manager.sh scripts/test-dns-fail-closed.sh proxypool-core/Makefile
git commit -m "feat: resolve client DNS through assigned nodes"
```

### Task 7: Shared xl2tpd/netifd Adapter

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/platform/openwrt/l2tp.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/openwrt/l2tp_test.go`
- Create: `proxypool-core/files/proxypool-netifd-event`
- Create: `scripts/test-v2-l2tp-adapter.sh`
- Modify: `proxypool-core/Makefile`

**Interfaces:**
- Implements `platform.NodeAdapter` for `model.ProtocolL2TP`.
- Dynamic interface name is a deterministic bounded `ppv2NNNN`; ownership snapshot binds node ID, policy ID, generation, interface name, endpoint, creation boot ID and exact config digest.
- An iface hotplug helper installed as `/etc/hotplug.d/iface/98-proxypool-v2-event` filters exact owned `ppv2*` netifd interface events and sends a bounded notification to `proxypoold`; it performs no nft/route/UCI mutation. The daemon also treats exact ubus status polling as authoritative, so missed hotplug notifications cannot create authorization.

- [ ] **Step 1: Write failing adapter tests**

Fixture the exact ubus calls for `network add_dynamic`, `network.interface.<name> status` and `network del_dynamic`. Cover domain/bootstrap endpoint, username/password control bytes, interface-name collisions, existing foreign interface, wrong l3_device, stale generation, missing PPP address, shared xl2tpd disappearance, stop timeout and hotplug-event spoofing. Prove no call disables/restarts xl2tpd directly and no global chap-secrets or PPP hook is written.

- [ ] **Step 2: Verify RED**

```sh
cd proxypool-core/src/proxypoold
go test ./internal/platform/openwrt -run 'L2TP' -count=1
cd ../../..
sh scripts/test-v2-l2tp-adapter.sh
```

Expected: FAIL because the adapter and event helper are absent.

- [ ] **Step 3: Implement netifd adapter**

Bootstrap-resolve the configured node server before setup and submit the exact IP in a dynamic netifd configuration with `proto=l2tp`, credentials, `ipv6=0`, bounded keepalive/MTU and strictly allowlisted `pppd_options`. The OpenWrt 23.05 script itself writes `nodefaultroute` and `usepeerdns`; do not invent unsupported `defaultroute`/`peerdns` protocol fields. Since dnsmasq has already been verified at `port=0`/`noresolv=1`, peer DNS cannot become a client fallback. Rely on the official protocol script to add/remove LACs in the shared xl2tpd. Verify ubus state, `l3_device=l2tp-<interface>`, IPv4 address, exact owned policy rule/table and ownership digest before returning a session.

- [ ] **Step 4: Verify GREEN and package safety**

Run adapter tests, `scripts/test-package-safety-integration.sh`, IPK inspectors and the kernel/LAN isolation contracts.

- [ ] **Step 5: Commit**

```sh
git add proxypool-core/src/proxypoold/internal/platform/openwrt/l2tp* proxypool-core/files/proxypool-netifd-event scripts/test-v2-l2tp-adapter.sh proxypool-core/Makefile
git commit -m "feat: connect l2tp nodes through shared netifd"
```

### Task 8: End-to-End Live Assembly and LuCI Test Controls

**Files:**
- Modify: `proxypool-core/src/proxypoold/cmd/proxypoold/main.go`
- Modify: `proxypool-core/files/proxypool.init`
- Modify: `proxypool-core/Makefile`
- Modify: `luci-app-proxypool/luasrc/controller/proxypool.lua`
- Modify: `luci-app-proxypool/luasrc/view/proxypool/main.htm`
- Modify: `luci-app-proxypool/htdocs/luci-static/resources/proxypool-global.js`
- Create: `scripts/test-v2-live-integration.sh`
- Modify: `scripts/test-host.sh`
- Modify: `docs/hardware/phase1-gl-mt6000-four-round-handoff.md`

**Interfaces:**
- Live init assembles controller, durable store, scheduler, device source, lease manager, DNS server, L2TP adapter and nft authorizer.
- LuCI calls `proxypoolctl` with strict JSON over the Unix socket; it never invokes manager scripts.

- [ ] **Step 1: Write failing live integration tests**

Use real daemon/controller/scheduler with fake OpenWrt executables. Exercise: discovered device list, bind, L2TP start, netifd up, DNS readiness, online lease, TCP authorization, daemon SIGKILL lease expiry, restart recovery, L2TP failure/backoff, manual reconnect coalescing and unbind/stop. LuCI source tests must reject any direct UCI/process/network mutation and require explicit job/error rendering.

- [ ] **Step 2: Verify RED**

```sh
sh scripts/test-v2-live-integration.sh
sh scripts/test-luci-package-source-safety.sh
sh scripts/test-proxypool-init.sh
```

Expected: FAIL because init and LuCI still expose shadow/quarantine-only behavior.

- [ ] **Step 3: Implement live wiring and minimal UI**

Make test builds start `proxypoold --live`; retain a clearly named diagnostics-only shadow command. LuCI adds discovered-device selection, bind/unbind, node connect/reconnect/stop and a single job-progress poller. Return HTTP 409/422/503 with structured daemon error codes; never report success before the persisted job exists.

- [ ] **Step 4: Full verification**

Run:

```sh
sh scripts/test-host.sh
cd proxypool-core/src/proxypoold
go test -race ./... -count=1
go vet ./...
cd ../../..
git diff --check
```

Then run pinned OpenWrt target nft checks, SDK core/LuCI IPK inspectors and the full-source GL-MT6000 build workflow. On hardware, first validate one device/one L2TP node: management remains available, DNS and public TCP exit IP use the node, external UDP fails, and stopping/killing the node produces no WAN fallback.

- [ ] **Step 5: Commit**

```sh
git add proxypool-core luci-app-proxypool scripts docs/hardware
git commit -m "feat: enable live v2 l2tp test dataplane"
```

## Deferred Follow-On Plans

This plan intentionally delivers the smallest real, safe hardware milestone first: one discovered device through one live L2TP node. After this vertical slice passes one-node hardware validation:

1. Implement SOCKS5 and SLP adapters against the same `NodeAdapter`, DNS and authorizer interfaces.
2. Add single-request 40～60 node import/export and expose full persistent batch progress.
3. Run protocol-mixed scale, shared-xl2tpd crash and 12～24 hour stability rounds.

These follow-ons do not require reopening the safety architecture or enabling V1.
