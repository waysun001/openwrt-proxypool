# ProxyPool V2 Phase 2 Shared L2TP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 用 netifd + 单个共享 xl2tpd 实现最多 60 个 L2TP 节点的有界连接、准确状态、分批启动和共享 daemon 自动恢复，并完成第一轮真机门。

**Architecture:** 每个节点映射到一个稳定短 netifd logical interface；OpenWrt 标准 L2TP handler 通过共享 xl2tpd control socket 管理 LAC。adapter 只产生观察结果，engine 用 node/config/xl2tpd generation 拒绝过期事件，并在共享 daemon 重启后默认每批并行恢复 4 个节点。

**Tech Stack:** Go 1.20、ubus/netifd、OpenWrt 23.05 `l2tp.sh`、xl2tpd、pppd、procd、Go fake runner、GL-MT6000 真机。

## Global Constraints

- 继承路线图全部约束；本阶段不开放任何终端数据面。
- L2TP 单次建连 deadline 默认 60 秒，正常停止 deadline 10 秒；任何 wait loop 必须响应 context。
- logical interface 使用 `p` + 节点 ID SHA-256 前 8 位；实际 PPP 接口从 netifd status 读取，不猜测。
- `defaultroute=0`、`peerdns=0`、IPv6 关闭，不能修改 main routing table 或系统 DNS。
- 共享 xl2tpd crash 先撤销（本阶段为空实现）授权，再按默认并发 4 恢复。
- 第一轮测试期间 V1 与 V2 L2TP 不同时运行；测试脚本先应用临时 LAN->WAN 拒绝，再停 V1。

---

### Task 1: 建立可测试的平台命令和 netifd client

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/platform/runner.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/fake_runner_test.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/netifd.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/netifd_test.go`

**Interfaces:**
- Produces: `Runner.Run(ctx, name, args, stdin)`, `Runner.Stream`, `Netifd.AddDynamic`, `Up`, `Down`, `RemoveDynamic`, `Status`, `Events`.

- [ ] **Step 1: Write argv and JSON parsing tests**

Prove untrusted server/user/password remain JSON stdin and never enter a shell command. Cover ubus nonzero exit, invalid JSON, missing l3 device, IPv4 extraction and canceled streaming event subscription.

```go
type Runner interface {
    Run(context.Context, string, []string, []byte) ([]byte, error)
    Stream(context.Context, string, []string) (io.ReadCloser, error)
}
```

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/platform -run TestNetifd -v
```

- [ ] **Step 3: Implement ubus calls without `sh -c`**

Use executable `ubus`, argv `call network add_dynamic` and JSON stdin. `AddDynamic` sends:

```json
{"name":"p12345678","config":{"proto":"l2tp","server":"vpn.example:1701","username":"u","password":"p","defaultroute":"0","peerdns":"0","ipv6":"0"}}
```

`Status` calls `network.interface.<name> status` and returns `Up`, `Pending`, `L3Device`, IPv4 addresses and protocol error strings.

- [ ] **Step 4: Implement event stream and cancel behavior**

Parse `ubus listen` objects into a typed `NetifdEvent`; reconnect stream with bounded backoff when ubus restarts. Stream goroutine must exit within one second of context cancellation.

- [ ] **Step 5: Verify and commit**

```bash
cd proxypool-core/src/proxypoold
go test -race ./internal/platform
git diff --check
git add internal/platform
git commit -m "feat: add safe netifd platform client"
```

### Task 2: 定义 adapter contract 和稳定 L2TP identity

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/adapter/adapter.go`
- Create: `proxypool-core/src/proxypoold/internal/adapter/fake_test.go`
- Create: `proxypool-core/src/proxypoold/internal/adapter/l2tp/identity.go`
- Create: `proxypool-core/src/proxypoold/internal/adapter/l2tp/identity_test.go`

**Interfaces:**
- Consumes: `model.Node`, `model.Protocol`, netifd status.
- Produces: `adapter.Adapter`, `adapter.Observed`, `adapter.Event`, `l2tp.InterfaceName(nodeID)`.

- [ ] **Step 1: Write identity and interface-length tests**

```go
func TestInterfaceNameIsStableAndFitsLinux(t *testing.T) {
    got := l2tp.InterfaceName("node-with-a-very-long-id")
    if len("l2tp-"+got) > 15 { t.Fatalf("too long: %q", got) }
    if got != l2tp.InterfaceName("node-with-a-very-long-id") { t.Fatal("not stable") }
}
```

Also assert two representative IDs do not collide and invalid empty IDs fail validation before naming.

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/adapter/... -run TestInterfaceName -v
```

- [ ] **Step 3: Implement the canonical adapter types**

`Observed` contains logical name, actual L3 device, IPv4, PID/generation metadata and opaque adapter facts. `Event` contains node ID, node generation, daemon generation, kind, timestamp and structured error.

- [ ] **Step 4: Implement stable short names**

Return lowercase `p` plus the first eight hex characters of SHA-256(node ID). Keep a startup reverse-map collision check across all configured nodes; a collision returns `invalid_config` instead of silently reusing an interface.

- [ ] **Step 5: Verify and commit**

```bash
cd proxypool-core/src/proxypoold
go test -race ./internal/adapter/...
git add internal/adapter
git commit -m "feat: define protocol adapter contract"
```

### Task 3: 实现共享 xl2tpd generation watcher 和 L2TP lifecycle

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/adapter/l2tp/generation.go`
- Create: `proxypool-core/src/proxypoold/internal/adapter/l2tp/generation_test.go`
- Create: `proxypool-core/src/proxypoold/internal/adapter/l2tp/adapter.go`
- Create: `proxypool-core/src/proxypoold/internal/adapter/l2tp/adapter_test.go`

**Interfaces:**
- Consumes: `platform.Netifd`, `platform.Runner`, `adapter.Adapter`.
- Produces: L2TP `Start`, `Inspect`, `Probe`, `Stop`, daemon generation events.

- [ ] **Step 1: Write lifecycle tests with a scripted fake netifd**

Cover: successful pending->up with IPv4, up without IPv4 until timeout, auth error, context cancellation, stop disappearance, stop timeout, stale netifd event and shared PID change.

```go
func TestStartTimesOutWhenPPPHasNoIPv4(t *testing.T) {
    clock := newFakeClock()
    a := newAdapter(netifdAlwaysPending(), clock, 60*time.Second)
    _, err := a.Start(ctx, node, 9)
    assertCode(t, err, "connect_timeout")
}
```

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/adapter/l2tp -run 'TestStart|TestStop|TestGeneration' -v
```

- [ ] **Step 3: Implement start/inspect/stop with deadlines**

`Start` creates/replaces only its stable dynamic interface, calls up, consumes matching events and polls status as a lost-event fallback. It succeeds only with `up=true`, nonempty L3 device and IPv4. `Stop` calls down/remove and returns `stop_timeout` after 10 seconds; it never loops forever.

- [ ] **Step 4: Implement shared daemon generation**

Generation fingerprint is `(pid, /proc/<pid>/stat starttime)`. PID reuse with a different starttime is a restart. Missing daemon is a generation loss event. Do not kill/restart shared xl2tpd from a single-node `Stop`; escalation is owned by engine recovery.

- [ ] **Step 5: Implement interface-bound TCP probe**

Invoke `curl` as argv, never shell, with `--interface <actual-l3-device>`, fixed connect/max timeouts and a configurable HTTPS probe URL. Probe failure returns `probe_failed`; it never installs a default route or uses a hostname bootstrap through client DNS.

- [ ] **Step 6: Verify and commit**

```bash
cd proxypool-core/src/proxypoold
go test -race ./internal/adapter/l2tp
git diff --check
git add internal/adapter/l2tp
git commit -m "feat: manage l2tp through shared netifd runtime"
```

### Task 4: 将 L2TP adapter 接入 engine、协议并发和过期事件保护

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/engine/coordinator.go`
- Create: `proxypool-core/src/proxypoold/internal/engine/coordinator_test.go`
- Create: `proxypool-core/src/proxypoold/internal/engine/scheduler.go`
- Create: `proxypool-core/src/proxypoold/internal/engine/scheduler_test.go`
- Create: `proxypool-core/src/proxypoold/internal/api/node_methods.go`
- Create: `proxypool-core/src/proxypoold/internal/api/node_methods_test.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/engine.go`
- Modify: `proxypool-core/src/proxypoold/cmd/proxypoolctl/main.go`

**Interfaces:**
- Consumes: adapter contract, jobs/state/retry from Phase 1.
- Produces: bounded L2TP reconciliation with default concurrency 4.

- [ ] **Step 1: Write scheduler tests**

Start 50 fake nodes and prove active `Start` calls never exceed 4, one timeout releases a slot, auth failure does not retry, transient failure schedules backoff, config save cancels generation N and queues N+1.

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/engine -run 'TestScheduler|TestCoordinator' -v
```

- [ ] **Step 3: Implement per-protocol semaphore and serial result application**

Worker goroutines perform adapter I/O only. They send typed results to one coordinator channel; only coordinator mutates node state/jobs. Acquire/release semaphore with context so canceled tasks do not leak capacity.

- [ ] **Step 4: Add explicit validation stage**

State order is `queued -> starting -> validating -> online`. Adapter `Start` returning PPP IPv4 only advances to validating; `Probe` must pass before online. Because dataplane is still no-op, status labels these nodes `online_control_plane_only=true` and binds no device.

- [ ] **Step 5: Add the stable L2TP node control methods**

Implement `node.save`, `node.delete` and `node.action` (`start|stop|reconnect`) with expected revision and jobs. Before Phase 4, save accepts only L2TP and returns `unsupported` for SOCKS5/SLP. Extend `proxypoolctl` to read node JSON from stdin, so credentials never appear in argv/history. The Round 1 lab daemon uses a separate `0600` config/socket under `/tmp/proxypool-lab/` and refuses to start if pointed at `/etc/config/proxypool`.

- [ ] **Step 6: Verify and commit**

```bash
cd proxypool-core/src/proxypoold
go test -race ./internal/engine ./internal/adapter/l2tp ./internal/api
git add internal/engine internal/api cmd/proxypoolctl
git commit -m "feat: schedule bounded l2tp reconciliation"
```

### Task 5: 实现共享 xl2tpd crash 的全局撤销和分批恢复

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/engine/recovery.go`
- Create: `proxypool-core/src/proxypoold/internal/engine/recovery_test.go`
- Create: `proxypool-core/src/proxypoold/internal/dataplane/reconciler.go`
- Create: `proxypool-core/src/proxypoold/internal/dataplane/noop.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/engine.go`

**Interfaces:**
- Produces: `dataplane.Reconciler`, no-op implementation, xl2tpd generation recovery sequence.

- [ ] **Step 1: Write crash fan-out tests**

For 50 online L2TP nodes, a daemon generation loss must call `RevokeNode` for all affected nodes before any new `Start`; states become recovering; after a new generation, starts are bounded at 4. SOCKS/SLP fake nodes remain unchanged.

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/engine -run TestSharedL2TPRecovery -v
```

- [ ] **Step 3: Implement recovery barrier**

Record the failed xl2tpd generation, revoke affected nodes, wait until procd presents a distinct live generation, then enqueue nodes in stable ID order with jitter. Ignore repeated loss events for the same generation.

- [ ] **Step 4: Test daemon restart reconstruction**

On `proxypoold` restart, call adapter `Inspect` for desired enabled nodes. Objects whose logical name or generation cannot be proven are stopped/removed before a fresh start. Never adopt a PPP interface solely because its name shares a prefix.

- [ ] **Step 5: Verify and commit**

```bash
cd proxypool-core/src/proxypoold
go test -race ./internal/engine ./internal/dataplane
git add internal/engine internal/dataplane
git commit -m "feat: recover shared xl2tpd generations safely"
```

### Task 6: 包装共享服务、PPP 只通知 hook 和第一轮真机脚本

**Files:**
- Modify: `proxypool-core/files/proxypool.init`
- Modify: `proxypool-core/Makefile`
- Create: `proxypool-core/files/proxypool-event.sh`
- Modify: `proxypool-core/files/ppp-up.sh`
- Modify: `proxypool-core/files/ppp-down.sh`
- Create: `scripts/device-test/lib.sh`
- Create: `scripts/device-test/round1-l2tp.sh`
- Create: `docs/testing/round1-l2tp.md`

**Interfaces:**
- Produces: procd-managed system xl2tpd cooperation, notification-only PPP events, reproducible Round 1 evidence bundle.

- [ ] **Step 1: Write shell static tests**

Add checks to `scripts/test-host.sh` proving V2 branches of PPP hooks only call `proxypoolctl event ppp` and never call `firewall.sh`, `ip route`, `nft`, `xl2tpd` or background `setsid`. Verify `proxypool-event.sh` whitelists `IFNAME`, `IPLOCAL`, `IPREMOTE`, `IPPARAM` and discards all other environment fields.

- [ ] **Step 2: Verify tests fail before hook changes**

```bash
./scripts/test-host.sh
```

Expected: FAIL on current direct firewall rebuild calls.

- [ ] **Step 3: Update packaging and init**

In V2 L2TP lab/active mode, enable and start the package-provided `/etc/init.d/xl2tpd`; never create one daemon per node. procd continues to supervise `proxypoold`. Install event helper and hooks with executable permissions.

- [ ] **Step 4: Implement Round 1 script with safe preflight**

Script refuses to run unless target reports GL-MT6000, a backup path exists, `runtime_backend=v2_shadow`, and the operator types the generated one-time confirmation string. It applies a temporary named nft table that rejects LAN->WAN before stopping V1, tests batches 5/20/40-50, captures timestamps/PID generations/RSS/FD/interface counts, kills shared xl2tpd once, waits for terminal states, then restores the selected backend. Trap cleanup must preserve fail-closed if interrupted.

Upload `scripts/device-test/lib.sh` and `round1-l2tp.sh` to `/tmp/proxypool-test/` on the router for execution; test scripts are development artifacts and are not installed into production firmware.

The operator supplies nodes through a local `0600` JSON file piped to `proxypoolctl`; the script never prints credentials, copies them into the report or passes them in argv. The lab config/socket under `/tmp/proxypool-lab/` are removed at cleanup.

- [ ] **Step 5: Run automated verification and build**

```bash
./scripts/test-host.sh
git diff --check
# SDK:
make package/proxypool/proxypool-core/compile V=s -j1
```

- [ ] **Step 6: Commit, then execute hardware gate with the user**

```bash
git add proxypool-core scripts/device-test docs/testing
git commit -m "test: add shared l2tp hardware recovery gate"
```

Do not mark Phase 2 complete until the real report contains no infinite connecting state, a bad account does not block the queue, and shared daemon recovery works at the agreed node scale.

## Phase 2 Exit Gate

- [ ] Host tests cover success, no-IP timeout, auth failure, stop timeout, stale event, daemon generation change and 50-node concurrency.
- [ ] OpenWrt package uses the single system xl2tpd and standard netifd L2TP handler.
- [ ] First hardware round passes at 5, 20 and available 40～50 valid nodes, or records a concrete blocker and uses the documented multi-instance engineering fallback.
- [ ] No terminal device traffic is authorized in this phase; temporary lab isolation is removed only after V1 is safely restored.
