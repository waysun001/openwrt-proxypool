# ProxyPool V2 Phase 1 Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付默认 shadow、不会修改现有网络的 `proxypoold` 基础包，包括 V2 配置模型、原子配置 store、Unix socket RPC、节点状态机和任务队列。

**Architecture:** 新 daemon 以接口隔离平台副作用；第一阶段所有 adapter 和 dataplane 使用只读/空实现。配置变更、任务和状态转移由单一 engine 协调，外部 I/O 返回结果必须携带 configuration generation。

**Tech Stack:** Go 1.20 标准库、OpenWrt UCI/procd、Unix domain socket、Go `testing`、GitHub Actions、OpenWrt 23.05 `golang-package.mk`。

## Global Constraints

- 目标平台是 OpenWrt 23.05.3 GL-MT6000；节点上限 60。
- daemon 默认 `runtime_backend=v1` 或 `v2_shadow`，第一阶段不得启动/停止现有节点或写 nft/route。
- UCI 是期望状态，daemon 是 V2 运行状态唯一写者。
- RPC request 最大 1 MiB，一连接一请求，Unix socket 权限 `0600`。
- 所有写操作使用 expected revision；冲突返回 `revision_conflict`。
- 密码和 token 不出现在 String、JSON 状态、日志和测试失败 diff 中。

---

### Task 1: 建立 Go module、公共命令入口和 host 测试脚本

**Files:**
- Create: `proxypool-core/src/proxypoold/go.mod`
- Create: `proxypool-core/src/proxypoold/internal/buildinfo/buildinfo.go`
- Create: `proxypool-core/src/proxypoold/internal/buildinfo/buildinfo_test.go`
- Create: `proxypool-core/src/proxypoold/cmd/proxypoold/main.go`
- Create: `proxypool-core/src/proxypoold/cmd/proxypoolctl/main.go`
- Create: `scripts/test-host.sh`
- Modify: `.gitignore`

**Interfaces:**
- Produces: `buildinfo.Version`, `buildinfo.SchemaVersion`, two compilable commands, one canonical host test command.

- [ ] **Step 1: Write the failing build-info test**

```go
func TestSchemaVersionIsV2(t *testing.T) {
    if buildinfo.SchemaVersion != 2 {
        t.Fatalf("SchemaVersion=%d want 2", buildinfo.SchemaVersion)
    }
    if strings.TrimSpace(buildinfo.Version) == "" {
        t.Fatal("Version must not be empty")
    }
}
```

- [ ] **Step 2: Run the focused test and verify RED**

Run:

```bash
cd proxypool-core/src/proxypoold
go test ./internal/buildinfo
```

Expected: FAIL because the module/package does not exist.

- [ ] **Step 3: Add the minimal module and commands**

`go.mod` must contain `module proxypoold` and `go 1.20`. `buildinfo.go` exposes:

```go
var Version = "dev"
const SchemaVersion = 2
```

Both commands must support `--version`; `proxypoold` otherwise exits with a clear “foundation shadow only” message until Task 6 wires the service.

- [ ] **Step 4: Add the canonical test runner**

`scripts/test-host.sh` uses `set -eu`, runs `go test ./...` and `go vet ./...` from the module, then prints explicit SKIP lines for Lua and Node suites that do not exist yet. Add `/build/`, firmware artifacts, host binaries and diagnostic archives to `.gitignore`.

- [ ] **Step 5: Verify GREEN and commit**

```bash
./scripts/test-host.sh
git diff --check
git add .gitignore scripts/test-host.sh proxypool-core/src/proxypoold
git commit -m "test: establish proxypoold host test baseline"
```

Expected: Go test/vet pass; Lua/Node show deliberate SKIP, not command failure.

### Task 2: 定义 V2 期望状态、运行状态和严格验证

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/model/config.go`
- Create: `proxypool-core/src/proxypoold/internal/model/runtime.go`
- Create: `proxypool-core/src/proxypoold/internal/model/errors.go`
- Create: `proxypool-core/src/proxypoold/internal/model/validate.go`
- Create: `proxypool-core/src/proxypoold/internal/model/validate_test.go`

**Interfaces:**
- Produces: `Protocol`, `DesiredConfig`, `GlobalConfig`, `Node`, `Device`, `RuntimeState`, `NodeRuntime`, `DeviceRuntime`, `CodeError`, `Validate(DesiredConfig) error`.

- [ ] **Step 1: Write table-driven validation tests**

Cover exactly: 60 nodes accepted, 61 rejected as `capacity_exceeded`, duplicate MAC, duplicate fixed IP, missing node reference, invalid protocol, invalid port, empty L2TP credentials, invalid MAC, IPv6 bind address and secret redaction.

```go
func TestValidateRejectsSixtyOneNodes(t *testing.T) {
    cfg := validConfigWithNodes(61)
    assertCode(t, model.Validate(cfg), "capacity_exceeded")
}
```

- [ ] **Step 2: Run tests and verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/model -run 'TestValidate|TestSecret' -v
```

Expected: FAIL because model types and `Validate` are undefined.

- [ ] **Step 3: Implement canonical types and validation**

Use `map[string]Node` and `map[string]Device`; normalize MAC with `net.ParseMAC` and IPv4 with `net.ParseIP(...).To4()`. Define errors as:

```go
type GlobalConfig struct {
    Enabled            bool
    RuntimeBackend     string
    MaxNodes           int
    LANDevice          string
    ManagementPorts    []uint16
    L2TPConcurrency    int
    ProxyConcurrency   int
    ConnectTimeout     time.Duration
    StopTimeout        time.Duration
    DoHEndpoints       []DoHEndpoint
}

type Node struct {
    ID, Name           string
    Protocol           Protocol
    Enabled            bool
    Server             string
    Port               uint16
    Username, Password string
    SLPToken           string
    SLPTransport       string
    SLPObfs            bool
    SLPObfsKey         string
    SLPInsecure        bool
    ExpiresAt          *time.Time
    PolicyID           uint16
    Revision           uint64
}

type DoHEndpoint struct {
    URL, BootstrapIP, ServerName string
}

type Device struct {
    ID, MAC, Hostname string
    FixedIPv4         netip.Addr
    NodeID            string
    Enabled           bool
}

type CodeError struct { Code, Message string }
func (e *CodeError) Error() string { return e.Code + ": " + e.Message }
```

`PolicyID` is allocated from 1～60 and immutable; Phase 3 starts using it for marks/routes. `Password`、`SLPToken` 和 `SLPObfsKey` use `json:"-"`; API uses a separate sanitized DTO later. Validate server strings as IP or hostname without invoking DNS.

- [ ] **Step 4: Verify all model tests and race-safety**

```bash
cd proxypool-core/src/proxypoold
go test -race ./internal/model
go vet ./internal/model
```

Expected: PASS; error codes match table exactly.

- [ ] **Step 5: Commit**

```bash
git add proxypool-core/src/proxypoold/internal/model
git commit -m "feat: define validated proxypool v2 model"
```

### Task 3: 实现可往返的 UCI V2 codec 和 revision 原子 store

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/config/codec.go`
- Create: `proxypool-core/src/proxypoold/internal/config/codec_test.go`
- Create: `proxypool-core/src/proxypoold/internal/config/store.go`
- Create: `proxypool-core/src/proxypoold/internal/config/store_test.go`
- Create: `proxypool-core/src/proxypoold/internal/config/testdata/v2-valid.uci`
- Create: `proxypool-core/src/proxypoold/internal/config/testdata/v2-invalid.uci`

**Interfaces:**
- Consumes: `model.DesiredConfig`, `model.Validate`.
- Produces: `Decode(io.Reader)`, `Encode(io.Writer, DesiredConfig)`, `Store.Load`, `Store.Replace`.

- [ ] **Step 1: Write codec and store failure tests**

Tests must prove encode/decode preserves IDs and secrets, produces deterministic node/device ordering, quotes apostrophes safely, rejects malformed sections, rejects stale revision and leaves the original file byte-for-byte unchanged on failed validation.

```go
func TestReplaceRejectsStaleRevision(t *testing.T) {
    _, err := store.Replace(ctx, 7, next)
    assertCode(t, err, "revision_conflict")
    assertFileEquals(t, path, before)
}
```

- [ ] **Step 2: Run focused tests and verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/config -v
```

Expected: FAIL because codec/store do not exist.

- [ ] **Step 3: Implement deterministic codec**

Map UCI sections to `config global 'global'`, `config node '<id>'` and `config device '<id>'`. Encode complete config to a temp file in the same directory, `fsync`, validate by decoding the temp file, compare expected revision, rename atomically, then fsync the directory. File mode is `0600`; every successful replace increments revision exactly once.

- [ ] **Step 4: Add crash and permission cases**

Inject filesystem operations through a narrow internal interface so tests can fail before rename and at rename. Confirm original content remains, temp files are removed, and mode never widens beyond `0600`.

- [ ] **Step 5: Verify and commit**

```bash
cd proxypool-core/src/proxypoold
go test -race ./internal/config
cd ../../..
git diff --check
git add proxypool-core/src/proxypoold/internal/config
git commit -m "feat: add atomic uci v2 config store"
```

### Task 4: 实现有界 Unix socket RPC server/client

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/api/message.go`
- Create: `proxypool-core/src/proxypoold/internal/api/server.go`
- Create: `proxypool-core/src/proxypoold/internal/api/client.go`
- Create: `proxypool-core/src/proxypoold/internal/api/server_test.go`
- Modify: `proxypool-core/src/proxypoold/cmd/proxypoolctl/main.go`

**Interfaces:**
- Produces: `Request`, `Response`, `Handler`, `Server.Serve`, `Client.Call`; newline JSON protocol version 1.

- [ ] **Step 1: Write protocol boundary tests**

Cover good request, unknown version, unknown method, malformed JSON, missing newline, request larger than `1<<20`, handler timeout, response ID echo and socket mode `0600`.

```go
type Handler interface {
    Handle(context.Context, Request) Response
}
```

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/api -run TestServer -v
```

- [ ] **Step 3: Implement one-request-per-connection server**

Use `io.LimitReader(conn, (1<<20)+1)`, read through newline, set read/write deadlines, reject trailing second messages, and remove an existing path only after `Lstat` proves it is a Unix socket. Create parent directory `0755`, socket `0600`.

- [ ] **Step 4: Wire `proxypoolctl call` without shell interpolation**

CLI accepts JSON from stdin:

```bash
printf '%s\n' '{"version":1,"id":"cli","method":"status.get","params":{}}' | proxypoolctl call
```

It prints exactly one JSON response to stdout and errors to stderr; secrets are never included in error formatting.

- [ ] **Step 5: Verify and commit**

```bash
cd proxypool-core/src/proxypoold
go test -race ./internal/api
go test ./cmd/proxypoolctl
git diff --check
git add internal/api cmd/proxypoolctl
git commit -m "feat: add bounded local control protocol"
```

### Task 5: 实现节点状态机、job、generation 和退避 scheduler

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/engine/state_machine.go`
- Create: `proxypool-core/src/proxypoold/internal/engine/state_machine_test.go`
- Create: `proxypool-core/src/proxypoold/internal/engine/jobs.go`
- Create: `proxypool-core/src/proxypoold/internal/engine/jobs_test.go`
- Create: `proxypool-core/src/proxypoold/internal/engine/retry.go`
- Create: `proxypool-core/src/proxypoold/internal/engine/retry_test.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/clock.go`

**Interfaces:**
- Produces: `Machine.Apply(Event)`, `JobStore`, `RetryPolicy.Next`, injectable `Clock`.

- [ ] **Step 1: Write state transition and stale-event tests**

The table must exercise every state. Explicitly prove generation 7 completion cannot change a node already on generation 8, auth failure goes to `failed`, timeout goes to `backoff`, manual reconnect creates a new generation, and stable online resets attempts.

```go
type Event struct {
    NodeID string
    Generation uint64
    Kind EventKind
    Err *model.CodeError
    At time.Time
}
```

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/engine -run 'TestMachine|TestRetry|TestJob' -v
```

- [ ] **Step 3: Implement legal transition table and bounded jobs**

Reject illegal transitions as `internal` errors; never silently mutate. `JobStore` retains the newest 256 jobs and newest 2048 node events in memory, with deterministic eviction. Public DTOs contain error codes/messages but no `Node.Password` or token.

- [ ] **Step 4: Implement retry policy with deterministic jitter source**

Base delays are 5s, 15s, 30s, 60s, doubling to a 5m cap. Inject a `rand.Source` so tests assert bounds; config/auth errors return no automatic retry, `wan_down` waits for a WAN event rather than a timer.

- [ ] **Step 5: Verify race behavior and commit**

```bash
cd proxypool-core/src/proxypoold
go test -race ./internal/engine
git diff --check
git add internal/engine internal/platform/clock.go
git commit -m "feat: add generation-safe runtime state machine"
```

### Task 6: 装配 shadow daemon、procd 包和 CI 门

**Files:**
- Create: `proxypool-core/src/proxypoold/internal/engine/engine.go`
- Create: `proxypool-core/src/proxypoold/internal/engine/engine_test.go`
- Modify: `proxypool-core/src/proxypoold/cmd/proxypoold/main.go`
- Modify: `proxypool-core/Makefile`
- Modify: `proxypool-core/files/proxypool.init`
- Modify: `proxypool-core/files/proxypool.config`
- Create: `.github/workflows/test.yml`
- Modify: `.github/workflows/build-fast.yml`

**Interfaces:**
- Consumes: config store, RPC, job/state packages.
- Produces: bootable `proxypoold` in `v2_shadow`, `status.get`, clean shutdown, host and SDK CI.

- [ ] **Step 1: Write engine reconciliation tests with no-op adapters**

Tests prove shadow startup loads/validates V2 UCI, reports `migration_required` for V1 without modifying it, exposes node/device desired state, never calls mutating platform methods, handles SIGTERM context cancellation, and rebuilds an in-memory reconciliation job after restart.

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/engine -run TestShadow -v
```

- [ ] **Step 3: Wire daemon and procd**

`proxypool.init` must use `USE_PROCD=1`, one instance, stdout/stderr to logd, respawn thresholds, and no cron watchdog. Preserve the V1 start path only when `runtime_backend='v1'`; `v2_shadow` starts daemon with `--shadow` and must not disable or stop system xl2tpd.

```sh
procd_set_param command /usr/sbin/proxypoold --config /etc/config/proxypool --socket /var/run/proxypoold.sock --shadow
procd_set_param respawn 3600 5 5
procd_set_param stdout 1
procd_set_param stderr 1
```

- [ ] **Step 4: Update OpenWrt Go packaging and CI**

Use the 23.05 `golang-package.mk`, build `proxypoold/cmd/proxypoold` and `proxypoold/cmd/proxypoolctl`, install both explicitly, keep the existing C `ip2region_searcher` build in a post-compile hook, and add host Go tests before SDK build. CI must fail on `go test`, `go vet`, `git diff --check` or package build errors.

- [ ] **Step 5: Run full verification**

```bash
./scripts/test-host.sh
git diff --check
# In OpenWrt 23.05.3 SDK:
make package/proxypool/proxypool-core/clean
make package/proxypool/proxypool-core/compile V=s -j1
```

Expected: two target binaries packaged; daemon starts in shadow; V1 behavior unchanged; no network mutations from daemon logs.

- [ ] **Step 6: Commit the phase gate**

```bash
git add proxypool-core .github/workflows scripts/test-host.sh
git commit -m "feat: package proxypoold shadow control plane"
```

## Phase 1 Exit Gate

- [ ] `./scripts/test-host.sh` passes with race-free Go tests.
- [ ] OpenWrt 23.05.3 package builds and contains both binaries with expected paths.
- [ ] Installing with `runtime_backend=v1` leaves existing behavior unchanged.
- [ ] Starting `v2_shadow` creates only socket/log/runtime memory; no netifd, process, nft or route mutation occurs.
- [ ] A malformed V2 config leaves the daemon fail-safe and returns a structured error without exposing secrets.
