# ProxyPool V2 Phase 4 Proxy and Per-Node DNS Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 增加 SOCKS5 和 SLP 的 TCP-only 数据面以及按绑定节点传输的本地 DNS/DoH，并通过第二轮真机测试证明没有 DNS、UDP、IPv6 或本地 WAN 泄漏。

**Architecture:** 每个代理节点拥有稳定本地 listener 和受 daemon 生命周期约束的子进程。bridge policy mark 在 inet prerouting 把终端 TCP 重定向到对应 listener；DNS 53 被重定向到 `proxypoold` 的 1053 listener，按源设备选择 L2TP-bound、SOCKS5 CONNECT 或 SLP local-SOCKS DoH transport。

**Tech Stack:** Go 1.20、redsocks、现有 SLP client、nftables redirect、DNS wire format、HTTPS/DoH、SOCKS5 CONNECT、Linux `SO_BINDTODEVICE`。

## Global Constraints

- 继承路线图约束；SOCKS5/SLP 只承载客户端 TCP，不实现 SOCKS5 UDP ASSOCIATE 或 redudp。SLP 选择 QUIC 时，只允许路由器自身到该节点精确 endpoint 的 UDP 隧道承载。
- 每个 listener 端口由不可变 `PolicyID` 确定并在启动前检查冲突。
- 代理没有 direct/fallback mode；listener/握手/probe 未就绪时不发布设备授权。
- 客户端 DNS 不交给系统上游；节点/DoH 失败时返回 SERVFAIL，不回退本地主 WAN。
- DNS cache 以 node ID + node revision + qname + qtype + qclass 分区。
- 客户端 AAAA 返回 NODATA；外部 UDP 和全部客户端 IPv6 继续由基础规则拒绝。

---

### Task 1: 建立节点子进程 supervisor 和稳定端口分配

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/adapter/process/supervisor.go`
- Create: `proxypool-core/src/proxypoold/internal/adapter/process/supervisor_test.go`
- Create: `proxypool-core/src/proxypoold/internal/adapter/process/ports.go`
- Create: `proxypool-core/src/proxypoold/internal/adapter/process/ports_test.go`

**Interfaces:**
- Produces: `Supervisor.Start`, `Stop`, `Inspect`, `Events`; `TransparentPort`, `SLPSOCKSPort`.

- [ ] **Step 1: Write process lifetime and port tests**

Prove policy IDs 1/60 map to valid non-overlapping ports, occupied port prevents start, child exit emits one event, stop targets only recorded process group, PID reuse is rejected by `/proc/<pid>/stat` starttime, and context cancellation reaps the child.

```go
type ProcessSpec struct {
    NodeID string
    Generation uint64
    Executable string
    Args []string
    ConfigPath string
}
```

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/adapter/process -v
```

- [ ] **Step 3: Implement Linux lifetime binding**

Start with `exec.Cmd`, `Setpgid=true`, `Pdeathsig=SIGTERM`, stdout/stderr to a bounded line scanner feeding structured log events. Store PID + starttime + node generation. Stop sends TERM to the verified process group, waits the configured deadline, then KILLs only the still-matching group.

- [ ] **Step 4: Implement deterministic port ranges**

Use `12000 + PolicyID` for SOCKS5 transparent redsocks, `13000 + PolicyID` for SLP local SOCKS and `14000 + PolicyID` for SLP transparent redsocks. Bind listeners to router-local addresses only; check `/proc/net/tcp*` or bind-probe before writing authorization.

- [ ] **Step 5: Verify and commit**

```bash
cd proxypool-core/src/proxypoold
go test -race ./internal/adapter/process
git add internal/adapter/process
git commit -m "feat: supervise node proxy processes safely"
```

### Task 2: 实现 TCP-only SOCKS5 adapter

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/adapter/socks5/config.go`
- Create: `proxypool-core/src/proxypoold/internal/adapter/socks5/config_test.go`
- Create: `proxypool-core/src/proxypoold/internal/adapter/socks5/adapter.go`
- Create: `proxypool-core/src/proxypoold/internal/adapter/socks5/adapter_test.go`

**Interfaces:**
- Consumes: process supervisor, model Node/PolicyID.
- Produces: SOCKS5 `adapter.Adapter`, exact transparent listener observation.

- [ ] **Step 1: Write config escaping and lifecycle tests**

Cover username/password containing quotes/backslashes/newlines, IPv4/hostname server, no redudp stanza, no direct fallback, listener readiness, SOCKS auth rejection, process exit, stop timeout and stale generation.

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/adapter/socks5 -v
```

- [ ] **Step 3: Render a minimal per-node redsocks config**

Reject newline/control characters before rendering. Config contains one `redsocks` section, `type=socks5`, loopback/local transparent listener, configured remote endpoint and optional auth; it contains no `redudp`, `direct`, alternate proxy or global dns entry. Config files are `0600` in `/var/run/proxypool/nodes/<id>/`.

- [ ] **Step 4: Implement handshake validation**

Before online, connect through the SOCKS endpoint with an internal Go SOCKS5 client and open the configured TCP probe target. Distinguish `auth_failed`, `resolve_failed`, `connect_timeout` and `probe_failed`. Process-alive alone never yields online.

- [ ] **Step 5: Verify and commit**

```bash
cd proxypool-core/src/proxypoold
go test -race ./internal/adapter/socks5 ./internal/adapter/process
git add internal/adapter/socks5
git commit -m "feat: add tcp-only socks5 adapter"
```

### Task 3: 实现 SLP + redsocks adapter

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/adapter/slp/config.go`
- Create: `proxypool-core/src/proxypoold/internal/adapter/slp/config_test.go`
- Create: `proxypool-core/src/proxypoold/internal/adapter/slp/adapter.go`
- Create: `proxypool-core/src/proxypoold/internal/adapter/slp/adapter_test.go`
- Modify: `proxypool-core/Makefile`

**Interfaces:**
- Consumes: process supervisor, existing packaged `/usr/bin/slp-client`, local SOCKS/transparent ports.
- Produces: SLP `adapter.Adapter`, SLP SOCKS dial endpoint for DNS.

- [ ] **Step 1: Write two-process readiness tests**

Cover SLP starts then local SOCKS becomes ready then redsocks starts; either process exiting revokes adapter observation; token never appears in logs; unsupported transport is `invalid_config`; probe flows through local SOCKS, not directly.

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/adapter/slp -v
```

- [ ] **Step 3: Implement sanitized SLP config and startup order**

Write one `0600` YAML/config file per node using a structured encoder or strict scalar quoting. Disable the legacy global DNS proxy. Start SLP on `13000+PolicyID`, wait for an authenticated SOCKS CONNECT probe, then start redsocks on `14000+PolicyID` pointing only to `127.0.0.1:<slp-port>`.

- [ ] **Step 4: Implement combined stop/inspect**

Revoke listener observation before stopping redsocks, then SLP. `Inspect` requires both verified process generations and both expected ports; orphaned halves are stopped and recreated.

- [ ] **Step 5: Verify package and commit**

```bash
cd proxypool-core/src/proxypoold
go test -race ./internal/adapter/slp ./internal/adapter/process
git add internal/adapter/slp proxypool-core/Makefile
git commit -m "feat: add supervised slp tcp adapter"
```

### Task 4: 实现 DNS wire server、AAAA NODATA 和分区缓存

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/dnsproxy/wire.go`
- Create: `proxypool-core/src/proxypoold/internal/dnsproxy/wire_test.go`
- Create: `proxypool-core/src/proxypoold/internal/dnsproxy/cache.go`
- Create: `proxypool-core/src/proxypoold/internal/dnsproxy/cache_test.go`
- Create: `proxypool-core/src/proxypoold/internal/dnsproxy/server.go`
- Create: `proxypool-core/src/proxypoold/internal/dnsproxy/server_test.go`

**Interfaces:**
- Produces: UDP/TCP DNS listener `:1053`, one-question parser, cache, device/node selector hook.

- [ ] **Step 1: Write DNS protocol tests**

Use binary fixtures for A, AAAA, compressed names, malformed pointer loop, multi-question, oversized UDP, TCP length prefix and cache TTL. Assert AAAA returns NOERROR/NODATA with original ID/question and unbound/unknown source returns REFUSED.

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/dnsproxy -run 'TestWire|TestCache|TestServer' -v
```

- [ ] **Step 3: Implement bounded wire handling**

Accept one IN question, cap packet at 4096 bytes and compression pointer depth at 16. Preserve request ID only at response edge; cache stores canonical response with zero ID. Extract minimum positive TTL and cap cache lifetime to 10 minutes; negative cache maximum 60 seconds.

- [ ] **Step 4: Implement source-to-node selection**

UDP `ReadFrom` and TCP remote address map source IPv4 to current `DeviceRuntime.AllowedIPv4`, then node ID/revision. Unknown, unbound or offline returns REFUSED/SERVFAIL without calling any upstream. Listener binds router LAN/loopback port 1053, not public WAN addresses.

- [ ] **Step 5: Verify fuzz boundaries and commit**

Add `FuzzParseQuestion` seed cases and run:

```bash
cd proxypool-core/src/proxypoold
go test -race ./internal/dnsproxy
go test ./internal/dnsproxy -run=FuzzParseQuestion -fuzz=FuzzParseQuestion -fuzztime=10s
git add internal/dnsproxy
git commit -m "feat: add bounded per-device dns server"
```

### Task 5: 实现 L2TP-bound、SOCKS5 和 SLP DoH transports

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/dnsproxy/transport.go`
- Create: `proxypool-core/src/proxypoold/internal/dnsproxy/socks.go`
- Create: `proxypool-core/src/proxypoold/internal/dnsproxy/socks_test.go`
- Create: `proxypool-core/src/proxypoold/internal/dnsproxy/doh.go`
- Create: `proxypool-core/src/proxypoold/internal/dnsproxy/doh_test.go`
- Create: `proxypool-core/src/proxypoold/internal/dnsproxy/bind_linux.go`

**Interfaces:**
- Consumes: node protocol/observed interface, configured DoH URL/bootstrap IP.
- Produces: `Transport.Exchange(ctx, nodeRuntime, dnsWire)`, no-fallback DoH client.

- [ ] **Step 1: Write fake TLS/DoH and SOCKS5 server tests**

Cover no-auth and username/password SOCKS5, domain and IPv4 CONNECT targets, auth reject, invalid reply, partial frames, deadline, HTTP status !=200, content type mismatch, oversized response and TLS server-name validation.

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/dnsproxy -run 'TestSOCKS|TestDoH|TestBound' -v
```

- [ ] **Step 3: Implement minimal SOCKS5 CONNECT dialer**

Support RFC1928 CONNECT and RFC1929 auth only; explicitly return `unsupported` for UDP ASSOCIATE. Honor context deadlines on every read/write. SOCKS5 nodes dial their remote endpoint over router control-plane WAN, then CONNECT DoH; SLP nodes dial the verified local SOCKS port.

- [ ] **Step 4: Implement bound L2TP dialer and DoH POST**

On Linux, `net.Dialer.Control` applies `SO_BINDTODEVICE` to the exact observed PPP interface before connect. DoH uses POST `application/dns-message`, bootstrap IP in TCP address and configured TLS ServerName/Host. It never falls back to `http.DefaultTransport`, system proxy or system DNS.

- [ ] **Step 5: Verify and commit**

```bash
cd proxypool-core/src/proxypoold
go test -race ./internal/dnsproxy
git add internal/dnsproxy
git commit -m "feat: route doh through each assigned node"
```

### Task 6: 发布代理/DNS 动态规则并完成第二轮真机测试

**Files:**
- Modify: `proxypool-core/src/proxypoold/internal/dataplane/device_rules.go`
- Modify: `proxypool-core/src/proxypoold/internal/dataplane/device_rules_test.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/coordinator.go`
- Modify: `proxypool-core/files/proxypool.config`
- Create: `scripts/device-test/round2-isolation-dns.sh`
- Create: `docs/testing/round2-isolation-dns.md`

**Interfaces:**
- Produces: mark->listener redirect, DNS 53->1053 redirect, complete protocol activation and Round 2 evidence.

- [ ] **Step 1: Extend golden/integration tests**

Assert private/link-local/multicast destinations are rejected before proxy redirect; TCP for SOCKS5/SLP redirects only to the matching port; DNS UDP/TCP redirects to 1053; all other UDP drops; IPv6 drops; missing listener/revision drops.

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/dataplane ./internal/engine -run 'TestProxy|TestDNS|TestUDP|TestIPv6' -v
```

- [ ] **Step 3: Wire readiness and revoke order**

For SOCKS5/SLP, publish only after process listener, TCP probe and DoH probe succeed. Revoke nft redirect before stopping DNS namespace/process. DNS cache namespace is deleted on node revision change or offline event.

- [ ] **Step 4: Implement Round 2 capture script**

Collect nft counters and WAN `tcpdump` filters while operator tests LAN-LAN, same/cross Wi-Fi, wired-wireless, unbound, L2TP, SOCKS5, SLP and forced node failure. Explicitly attempt external UDP DNS, QUIC/443, IPv6 and RFC1918. Record LuCI reachability and basic webpage/WeChat text-image results; redact node credentials.

- [ ] **Step 5: Run full verification, commit and execute hardware gate**

```bash
./scripts/test-host.sh
bash tests/integration/fail_closed_test.sh
git diff --check
git add proxypool-core scripts/device-test docs/testing
git commit -m "test: add proxy dns isolation hardware gate"
```

Do not continue if WAN capture contains direct client destinations, external DNS not encapsulated in the assigned node, peer traffic succeeds, or LuCI becomes unreachable.

## Phase 4 Exit Gate

- [ ] SOCKS5 and SLP carry TCP only and have no direct/redudp fallback.
- [ ] L2TP/SOCKS5/SLP DNS reaches DoH only through the assigned node; failure returns SERVFAIL.
- [ ] DNS cache is isolated by node ID and revision; AAAA is NODATA.
- [ ] Round 2 proves LuCI reachable, all terminal pairs isolated, external UDP/IPv6/private access blocked and node loss causes immediate offline.
- [ ] Basic browsing and微信文字/图片 succeed for valid TCP-capable nodes; voice/video are not acceptance requirements.
