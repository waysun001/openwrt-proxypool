# ProxyPool V2 Phase 5 LuCI, Import, Migration and Diagnostics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 用事务式批量导入和 V2 LuCI 替换浏览器驱动的后台脚本调用，迁移现有节点/IP 绑定，并提供脱敏诊断，完成第三轮 40～60 节点故障恢复测试。

**Architecture:** LuCI controller 通过 nixio Unix socket 直接转发 RPC，所有写操作为 POST。批量文本由 daemon 服务端解析，preview 与 commit 绑定配置 revision；V1 UCI 在临时目录完整迁移并校验后原子替换，旧 IP 在没有活动 lease 时保存为待自动学习绑定。

**Tech Stack:** Go 1.20、LuCI Lua/nixio/jsonc、原生 JavaScript、Node test runner、Lua 5.1 tests、UCI、tar/gzip、OpenWrt logd。

## Global Constraints

- 继承路线图约束；LuCI 不再执行 `setsid`、manager shell、nft、ip、uci 写命令或同步等待拨号。
- GET 仅允许只读；任何保存、启停、删除、绑定、导入和激活操作必须 POST + LuCI session/CSRF 校验。
- 导入请求最大 1 MiB，解析记录最多 60；导入后总节点数最多 60。
- preview 有阻断错误时 commit 不可用；commit 必须匹配 preview hash 和 expected revision。
- 迁移前备份 V1；失败不覆盖原配置。离线旧 IP 绑定等待 DHCP 自动学习，不要求用户输入 MAC。
- 诊断文件只写 `/tmp`、模式 `0600`、有大小/时间限制，并删除所有认证信息。

---

### Task 1: 实现兼容格式的服务端批量 preview/commit

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/importer/parser.go`
- Create: `proxypool-core/src/proxypoold/internal/importer/parser_test.go`
- Create: `proxypool-core/src/proxypoold/internal/importer/preview.go`
- Create: `proxypool-core/src/proxypoold/internal/importer/preview_test.go`
- Create: `proxypool-core/src/proxypoold/internal/importer/testdata/legacy-l2tp.txt`
- Create: `proxypool-core/src/proxypoold/internal/importer/testdata/legacy-socks5.txt`
- Create: `proxypool-core/src/proxypoold/internal/importer/testdata/legacy-slp.txt`

**Interfaces:**
- Produces: `Importer.Preview(ctx, PreviewRequest)`, `Commit(ctx, CommitRequest)`, bounded preview store.

- [ ] **Step 1: Encode current import formats as regression tests**

Exact accepted legacy forms:

```text
L2TP: server|username|password
L2TP: server|port|username|password
L2TP: server|username|password|YYYY-MM-DD
L2TP: server|port|username|password|YYYY-MM-DD
SOCKS5: server|port|username|password|expiry
SLP: server|port|token|quic
```

Also test CRLF, blank lines, malformed ports, duplicate natural key, duplicate within batch, 61 records, total capacity, invalid date and pipe/control characters in secrets.

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/importer -run TestParse -v
```

- [ ] **Step 3: Implement pure parsing and sanitized preview DTOs**

Line errors include 1-based line, stable code and Chinese-safe message. Natural duplicate key is protocol + normalized server + effective port + username; SLP uses a SHA-256 token fingerprint internally and never returns token. Preview rows show masked secret state only.

- [ ] **Step 4: Implement revision-bound commit**

Preview ID stores parsed config delta, SHA-256(raw + protocol + base revision), expiration 10 minutes and maximum 16 concurrent previews. Commit accepts preview ID/hash/expected revision, calls `Store.Replace` once, creates one reconciliation job and consumes preview so it cannot replay.

- [ ] **Step 5: Prove all-or-nothing behavior and commit**

```bash
cd proxypool-core/src/proxypoold
go test -race ./internal/importer ./internal/config
git add internal/importer
git commit -m "feat: add atomic server-side bulk import"
```

### Task 2: 实现 V1->V2 迁移、待学习绑定和回退导出

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/config/migrate.go`
- Create: `proxypool-core/src/proxypoold/internal/config/migrate_test.go`
- Create: `proxypool-core/src/proxypoold/internal/config/v1.go`
- Create: `proxypool-core/src/proxypoold/internal/config/testdata/v1-realistic.uci`
- Create: `proxypool-core/src/proxypoold/internal/config/testdata/v1-duplicate-bind.uci`
- Modify: `proxypool-core/src/proxypoold/internal/model/config.go`
- Modify: `proxypool-core/src/proxypoold/internal/device/inventory.go`
- Create: `proxypool-core/files/proxypool-migrate.sh`

**Interfaces:**
- Produces: `MigrateV1`, `ExportV1`, `PendingBinding`, automatic pending->device conversion.

- [ ] **Step 1: Write realistic migration tests**

Cover L2TP/SOCKS5/SLP fields, enabled flags, expiry, multiple `bind_ip`, current DHCP match, offline IP, duplicate bind IP, node ID collision, policy ID allocation, secret preservation and source file unchanged on every injected failure.

```go
type PendingBinding struct {
    ID string
    LegacyIPv4 netip.Addr
    NodeID string
    CreatedAt time.Time
}
```

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/config -run 'TestMigrate|TestExportV1' -v
```

- [ ] **Step 3: Implement staged migration**

Create `/etc/proxypool/backups/proxypool-v1-<UTC timestamp>.uci` mode `0600`, parse V1 without UCI writes, generate stable IDs/policy IDs, match bind IP against current leases, validate complete V2, encode to same-directory temp, fsync and rename. Migration marker contains source checksum and target revision for idempotence.

- [ ] **Step 4: Implement pending auto-learning transaction**

`DesiredConfig` gains `PendingBindings map[string]PendingBinding`. On DHCP lease event whose IPv4 exactly matches one pending entry, create Device with discovered MAC/fixed IP/node ID and remove pending in one `Replace`; conflicting MAC/IP keeps pending with `duplicate` error visible in status.

- [ ] **Step 5: Implement V1 recovery export and verify**

Export reconstructs `config client` sections and `list bind_ip`; pending bindings are also emitted as bind IP. Round-trip V1->V2->V1 must preserve protocol endpoints, credentials, enabled flags and binding IPs.

```bash
cd proxypool-core/src/proxypoold
go test -race ./internal/config ./internal/device
git add internal/config internal/model internal/device ../../../files/proxypool-migrate.sh
git commit -m "feat: migrate legacy nodes and learn device macs"
```

### Task 3: 实现完整 RPC method handlers 和安全 DTO

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/api/methods.go`
- Create: `proxypool-core/src/proxypoold/internal/api/methods_test.go`
- Create: `proxypool-core/src/proxypoold/internal/api/dto.go`
- Create: `proxypool-core/src/proxypoold/internal/api/dto_test.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/engine.go`

**Interfaces:**
- Consumes: Phase 2 node methods and Phase 3 device methods.
- Produces: remaining stable methods listed in roadmap plus unified sanitized status/node/device/job DTOs.

- [ ] **Step 1: Write authorization-independent method contract tests**

Test invalid params, stale revision, 61st node, save preserving blank password, explicit secret replacement, delete unbinding devices, reconnect generation, bind unknown MAC, job lookup, preview/commit and diagnostics job creation.

- [ ] **Step 2: Write secret leak test before implementation**

Marshal every success/error DTO and search for known password/token values and their URL/base64 encodings. Test failure output itself must use labels, not print the secret.

- [ ] **Step 3: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/api -run 'TestMethod|TestDTO|TestNoSecret' -v
```

- [ ] **Step 4: Implement typed dispatch**

Decode each params object with `DisallowUnknownFields`, enforce required expected revision on writes, return `invalid_request` for extra/missing fields, and create jobs instead of blocking on node I/O. `node.delete` clears device node references in the same config transaction; devices remain offline.

- [ ] **Step 5: Verify and commit**

```bash
cd proxypool-core/src/proxypoold
go test -race ./internal/api ./internal/engine
git add internal/api internal/engine
git commit -m "feat: expose typed proxypool v2 rpc methods"
```

### Task 4: 将 LuCI controller 改成受 CSRF 保护的 RPC bridge

**Files:**
- Create: `luci-app-proxypool/luasrc/model/proxypool_rpc.lua`
- Create: `luci-app-proxypool/tests/test_rpc.lua`
- Create: `luci-app-proxypool/tests/stubs/nixio.lua`
- Rewrite: `luci-app-proxypool/luasrc/controller/proxypool.lua`
- Modify: `luci-app-proxypool/Makefile`
- Modify: `scripts/test-host.sh`

**Interfaces:**
- Consumes: newline JSON RPC socket.
- Produces: LuCI read/write endpoints, HTTP status mapping, no direct system mutations.

- [ ] **Step 1: Write Lua socket bridge tests**

Stub nixio and assert request newline framing, 1 MiB limit, read timeout, response ID check, malformed daemon response -> 502, daemon unavailable -> 503, and no credential logging.

- [ ] **Step 2: Write controller static security tests**

The test reads controller source and rejects `os.execute`, `sys.exec`, `setsid`, `/usr/lib/proxypool/*.sh`, `uci:set` and mutation actions registered as GET. Read methods are GET; write methods use LuCI `post()` dispatch target/session token enforcement.

- [ ] **Step 3: Verify RED against current controller**

```bash
./scripts/test-host.sh
```

Expected: FAIL because current controller executes shell/UCI and mutates via action query parameters.

- [ ] **Step 4: Implement RPC bridge and minimal routes**

Controller renders main/locked/lease pages and exposes separate read/write endpoints. `proxypool_rpc.lua` uses `nixio.socket("unix", "stream")`, connects only `/var/run/proxypoold.sock`, sends one JSON line and reads one bounded response. Map invalid request 400, conflict 409, capacity 422, unavailable 503 and internal 502.

- [ ] **Step 5: Update install paths, test runner and commit**

```bash
./scripts/test-host.sh
git diff --check
git add luci-app-proxypool scripts/test-host.sh
git commit -m "refactor: route luci operations through proxypoold"
```

### Task 5: 重建节点、设备和任务页面状态层

**Files:**
- Rewrite: `luci-app-proxypool/luasrc/view/proxypool/main.htm`
- Create: `luci-app-proxypool/htdocs/luci-static/resources/proxypool-v2.js`
- Create: `luci-app-proxypool/htdocs/luci-static/resources/proxypool-v2.css`
- Create: `luci-app-proxypool/tests/ui/proxypool-v2.test.mjs`
- Modify: `luci-app-proxypool/Makefile`
- Modify: `scripts/test-host.sh`

**Interfaces:**
- Consumes: status/node/device/job RPC DTOs.
- Produces: three-tab LuCI UI with refresh-safe job display and no browser-driven sequential starts.

- [ ] **Step 1: Write pure JS state/reducer tests**

Using Node built-in `node:test`, cover status replacement by revision, stale response ignored, job progress aggregation, reconnect countdown, API error mapping, secret field blank-preserve semantics, HTML escaping and page reload restoring tracked job IDs from sessionStorage.

- [ ] **Step 2: Verify RED**

```bash
node --test luci-app-proxypool/tests/ui/proxypool-v2.test.mjs
```

- [ ] **Step 3: Implement a small HTML shell and testable JS module**

`main.htm` contains tabs/containers/modal roots plus one external script. JS exports pure `reduceState`, `escapeText`, `formatError`, `validateNodeForm` for Node and attaches UI boot only when `document` exists. Render device hostname/MAC/current/fixed IP/access/binding; render node state/error/retry/bound count; render jobs summary and per-node failures.

- [ ] **Step 4: Implement safe operations**

All mutations use POST JSON with revision and CSRF token. Save/reconnect/delete/bind return job IDs and update locally to queued without fabricating online. Poll status at one bounded cadence with AbortController; page close does not cancel jobs. Remove `sequentialConnect`, pending marker files and per-node browser loops.

- [ ] **Step 5: Verify and commit**

```bash
./scripts/test-host.sh
git diff --check
git add luci-app-proxypool scripts/test-host.sh
git commit -m "feat: add device node and job luci views"
```

### Task 6: 增加导入 preview/commit、导出和迁移反馈 UI

**Files:**
- Modify: `luci-app-proxypool/htdocs/luci-static/resources/proxypool-v2.js`
- Modify: `luci-app-proxypool/tests/ui/proxypool-v2.test.mjs`
- Modify: `luci-app-proxypool/luasrc/view/proxypool/main.htm`

**Interfaces:**
- Consumes: `import.preview`, `import.commit`, pending binding DTOs.
- Produces: server-authoritative preview and durable background job tracking.

- [ ] **Step 1: Write import UI tests**

Prove raw text—not browser-parsed credentials—is sent to preview; blocking error disables commit; displayed secret is masked; commit includes preview ID/hash/base revision; 409 forces new preview; successful commit tracks returned job; pending old-IP device shows “等待设备出现”.

- [ ] **Step 2: Verify RED**

```bash
node --test luci-app-proxypool/tests/ui/proxypool-v2.test.mjs
```

- [ ] **Step 3: Implement preview and job UI**

Show line, normalized endpoint, add/update/skip/error and stable error message. Do not include plaintext password/token in DOM after request submission. Commit closes modal only after config transaction succeeds; node connection progress remains in Tasks and survives refresh.

- [ ] **Step 4: Implement sanitized export**

Default export excludes secrets and produces V2 JSON. A separate “包含认证信息” action requires explicit confirmation and POST, streams a one-time download from `/tmp`, then deletes it. Never put credentials in query strings or page HTML.

- [ ] **Step 5: Verify and commit**

```bash
./scripts/test-host.sh
git add luci-app-proxypool
git commit -m "feat: add transactional import and migration ui"
```

### Task 7: 生成有界、脱敏的一键诊断包

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/diagnostics/redact.go`
- Create: `proxypool-core/src/proxypoold/internal/diagnostics/redact_test.go`
- Create: `proxypool-core/src/proxypoold/internal/diagnostics/bundle.go`
- Create: `proxypool-core/src/proxypoold/internal/diagnostics/bundle_test.go`
- Modify: `proxypool-core/src/proxypoold/internal/api/methods.go`
- Modify: `luci-app-proxypool/luasrc/controller/proxypool.lua`

**Interfaces:**
- Produces: async `diagnostics.create`, one-time safe download, automatic expiry.

- [ ] **Step 1: Write adversarial redaction tests**

Fixtures place passwords/tokens in UCI, argv, log lines, JSON, URL userinfo and base64. Assert bundle contains none of the raw known secrets, Cookie, session token or private key markers. Assert per-command 2 MiB and total 16 MiB caps.

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/diagnostics -v
```

- [ ] **Step 3: Implement bounded collectors**

Collect sanitized config summary, status/jobs/events, ubus interface status, PPP/xl2tpd process metadata, owned nft/ip objects, DHCP binding consistency, RSS/FD counts and recent logd entries. Each command uses argv + 5 second deadline and truncation marker.

- [ ] **Step 4: Implement safe archive/download lifecycle**

Create `/tmp/proxypool/diagnostics/<random>.tar.gz` mode `0600`, return opaque ID not arbitrary path, allow authenticated controller download once, verify resolved path stays inside diagnostics root, then delete; periodic cleanup removes files after 15 minutes.

- [ ] **Step 5: Verify and commit**

```bash
cd proxypool-core/src/proxypoold
go test -race ./internal/diagnostics ./internal/api
git add internal/diagnostics internal/api
cd ../../..
git add luci-app-proxypool/luasrc/controller/proxypool.lua
git commit -m "feat: add redacted bounded diagnostics"
```

### Task 8: 完成第三轮批量和故障恢复真机门

**Files:**
- Create: `scripts/device-test/round3-bulk-recovery.sh`
- Create: `scripts/device-test/fixtures/import-60-valid.txt`
- Create: `scripts/device-test/fixtures/import-invalid.txt`
- Create: `docs/testing/round3-bulk-recovery.md`
- Modify: `.github/workflows/test.yml`

**Interfaces:**
- Produces: repeatable 40～60 node batch/failure report and CI secret/static checks.

- [ ] **Step 1: Add CI static gates**

Fail when V2 LuCI contains direct shell/UCI mutations, legacy `sequentialConnect`, GET mutation routes, Go formatting errors, test failure or known fixture secret in a generated diagnostic test archive.

- [ ] **Step 2: Implement fault sequence script**

Run valid import, invalid atomic import, duplicate, 61-node rejection, UI/API polling during refresh, WAN down/up, daemon TERM, xl2tpd TERM, individual proxy process TERM, node edit/reconnect/delete, network reload and firewall reload. Timestamp every request/job/state and save nft/route leak counters.

- [ ] **Step 3: Run complete automated verification**

```bash
./scripts/test-host.sh
bash tests/integration/fail_closed_test.sh
git diff --check
# SDK:
make package/proxypool/proxypool-core/compile V=s -j1
make package/proxypool/luci-app-proxypool/compile V=s -j1
```

- [ ] **Step 4: Commit and execute Round 3 with the user**

```bash
git add scripts/device-test docs/testing .github/workflows/test.yml
git commit -m "test: add bulk import and recovery hardware gate"
```

Pass only if every node reaches online/failed/backoff within its bounded attempt, one failure never blocks remaining jobs, page refresh does not cancel work, stale task results never overwrite edited config and every injected failure remains fail-closed.

## Phase 5 Exit Gate

- [ ] Legacy import formats are preserved but parsed/validated server-side; commit is one atomic revision.
- [ ] Existing V1 nodes, credentials and IP bindings migrate; offline IP waits for DHCP MAC automatically.
- [ ] LuCI writes are POST/CSRF-protected RPC only; no direct network/process/UCI mutation remains.
- [ ] Device, node and task UI accurately reflects daemon state and survives refresh.
- [ ] Diagnostic bundles are size/time bounded and pass adversarial secret scans.
- [ ] Round 3 passes at realistic 40～60 node scale without permanent connecting or manual reconnect dependency.
