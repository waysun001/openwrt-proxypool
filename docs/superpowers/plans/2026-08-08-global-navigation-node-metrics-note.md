# ProxyPool Global Navigation, Node Metrics and Note Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在全部 LuCI 管理页面提供 ProxyPool 导航，并在节点管理中加入本次连接流量、实时速率和中文备注。

**Architecture:** 备注作为向后兼容的可选 UCI 节点字段进入现有单写者事务；流量由调度器按当前 session generation 从 sysfs 采样，只进入内存状态摘要；全局导航由独立 `luci-theme-proxypool` 主题提供，不修改 Bootstrap 软件包文件。现有 LuCI 页面通过状态 API 每三秒读取新的非敏感字段。

**Tech Stack:** Go 1.20、OpenWrt 23.05.3、LuCI Lua/ucode 模板、原生 JavaScript、POSIX shell、OpenWrt SDK/完整源码构建。

## Global Constraints

- 最多 60 个节点。
- 流量只统计本次成功连接，断线、重连、服务重启和路由器重启后归零。
- 流量不写 UCI、不写运行时快照、不增加配置 revision、不创建任务。
- 下行为接口 `rx_bytes`，上行为接口 `tx_bytes`；后端约 1 秒采样，LuCI 约 3 秒轮询。
- 备注最长 200 个 Unicode 字符，去除首尾空白，禁止换行和控制字符。
- 旧配置缺少 `option note` 时必须继续有效；批量导入格式保持不变。
- 修改名称或备注不得触发节点重连；连接参数变化继续沿用现有调度器。
- 主题不得覆盖或修改 `luci-theme-bootstrap` 所属文件；安装失败和卸载时保持可用的 Bootstrap 回退。
- 不改变 LAN/WiFi 隔离、本地 WAN 禁止、UDP 禁止、管理白名单和 fail-closed 行为。

---

### Task 1: Add the backward-compatible node note field

**Files:**
- Modify: `proxypool-core/src/proxypoold/internal/model/config.go`
- Modify: `proxypool-core/src/proxypoold/internal/model/validate.go`
- Modify: `proxypool-core/src/proxypoold/internal/model/validate_test.go`
- Modify: `proxypool-core/src/proxypoold/internal/config/codec.go`
- Modify: `proxypool-core/src/proxypoold/internal/config/codec_test.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/engine.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/engine_test.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/controller.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/controller_test.go`

**Interfaces:**
- Produces: `model.Node.Note string`.
- Produces: `DesiredNodeSummary.Note string` with JSON name `note`.
- Produces: `nodeSaveParams.Note string` with JSON name `note`.
- Produces: optional UCI `option note`; canonical encoding always writes it.

- [ ] **Step 1: Write failing model and codec tests**

Add table cases that accept `"香港 01"`, reject 201 runes, reject `"line1\nline2"`, reject `"a\u0007b"`, and prove a fixture with no `note` decodes to `Node.Note == ""`. Add a round-trip assertion:

```go
cfg := validConfig()
node := cfg.Nodes["node_1"]
node.Note = "香港住宅节点"
cfg.Nodes[node.ID] = node
var encoded bytes.Buffer
if err := config.Encode(&encoded, cfg); err != nil { t.Fatal(err) }
if !strings.Contains(encoded.String(), "option note '香港住宅节点'") { t.Fatal(encoded.String()) }
decoded, err := config.Decode(strings.NewReader(encoded.String()))
if err != nil { t.Fatal(err) }
if got := decoded.Nodes[node.ID].Note; got != node.Note { t.Fatalf("note=%q", got) }
```

- [ ] **Step 2: Run focused tests and verify RED**

Run:

```sh
cd proxypool-core/src/proxypoold
go test ./internal/model ./internal/config
```

Expected: compilation fails because `model.Node.Note` does not exist.

- [ ] **Step 3: Implement the model and optional codec field**

Add `Note string` to `model.Node`, include the non-secret note in `Node.String()`, and validate canonical text with:

```go
const MaxNodeNoteRunes = 200

func validNodeNote(note string) bool {
	if note != strings.TrimSpace(note) || utf8.RuneCountInString(note) > MaxNodeNoteRunes {
		return false
	}
	for _, character := range note {
		if unicode.IsControl(character) { return false }
	}
	return true
}
```

Add `note` to `nodeOptions` but not `nodeRequiredOptions`, leave `testdata/v2-valid.uci` without the option as the backward-compatibility fixture, decode missing values as empty, and write `option note` in canonical output.

- [ ] **Step 4: Add failing controller tests for transport and no reconnect**

Add tests that save a Chinese note, observe it in `status.get`, and update only `name`/`note` on an online node without submitting a queued node job:

```go
response := controller.Handle(ctx, controllerRequest("note-save", "node.save",
	`{"node_id":"node_a","name":"香港 A","note":"微信专用","protocol":"l2tp","enabled":true,`+
	`"server":"vpn.example.com","port":1701,"username":"","password":"","expires_at":"","expected_revision":3}`))
assertControllerSuccess(t, response)
if got := scheduler.submittedCount(); got != 0 { t.Fatalf("submitted=%d", got) }
```

Also assert 201 runes, newline and control characters return `invalid_request` or `invalid_config` before persistence.

- [ ] **Step 5: Run controller tests and verify RED**

Run:

```sh
cd proxypool-core/src/proxypoold
go test ./internal/engine -run 'NodeSave|StatusSummary|Note' -count=1
```

Expected: note is absent or rejected by strict parameter decoding.

- [ ] **Step 6: Implement controller note handling**

Trim `params.Note`, assign it to `model.Node.Note`, expose it in `DesiredNodeSummary`, and add:

```go
func sameNodeDataplane(left, right model.Node) bool {
	return left.ID == right.ID && left.Protocol == right.Protocol &&
		left.Enabled == right.Enabled && left.DeletePending == right.DeletePending &&
		left.Server == right.Server && left.Port == right.Port &&
		left.Username == right.Username && left.Password == right.Password &&
		left.SLPToken == right.SLPToken && left.SLPTransport == right.SLPTransport &&
		left.SLPObfs == right.SLPObfs && left.SLPObfsKey == right.SLPObfsKey &&
		left.SLPInsecure == right.SLPInsecure && left.PolicyID == right.PolicyID &&
		equalOptionalTime(left.ExpiresAt, right.ExpiresAt)
}
```

Only add the node ID to the scheduled job when creating an enabled node, changing enabled state, or `sameNodeDataplane(previous, node)` is false. A metadata-only mutation still commits exactly one desired revision and returns a successful mutation result with no affected nodes.

- [ ] **Step 7: Run note tests and commit**

Run:

```sh
cd proxypool-core/src/proxypoold
go test ./internal/model ./internal/config ./internal/engine -count=1
```

Expected: PASS.

Commit:

```sh
git add proxypool-core/src/proxypoold/internal/model \
  proxypool-core/src/proxypoold/internal/config \
  proxypool-core/src/proxypoold/internal/engine
git commit -m "feat: add node notes without reconnecting"
```

---

### Task 2: Build the generation-safe in-memory traffic collector

**Files:**
- Modify: `proxypool-core/src/proxypoold/internal/platform/contracts.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/openwrt/traffic.go`
- Create: `proxypool-core/src/proxypoold/internal/platform/openwrt/traffic_test.go`
- Create: `proxypool-core/src/proxypoold/internal/engine/traffic.go`
- Create: `proxypool-core/src/proxypoold/internal/engine/traffic_test.go`

**Interfaces:**
- Produces: `platform.InterfaceCounters{RXBytes uint64, TXBytes uint64}`.
- Produces: `platform.InterfaceTrafficReader.ReadInterfaceCounters(interfaceName string) (platform.InterfaceCounters, error)`.
- Produces: `engine.TrafficSnapshot` with download/upload totals, rates and RFC3339 `SampledAt string`.
- Produces: `trafficTracker.Begin`, `End`, `Sample` and `Snapshot` methods keyed by node ID and generation.

- [ ] **Step 1: Write failing sysfs reader tests**

Use `t.TempDir()` as a fake `/sys/class/net` tree. Create `l2tp-ppv20001/statistics/rx_bytes` containing `"123\n"` and `tx_bytes` containing `"456\n"`; assert the reader returns both. Reject `../escape`, missing files, negative text and non-decimal text.

- [ ] **Step 2: Run the reader test and verify RED**

Run:

```sh
cd proxypool-core/src/proxypoold
go test ./internal/platform/openwrt -run InterfaceTraffic -count=1
```

Expected: the reader types do not exist.

- [ ] **Step 3: Implement the bounded sysfs reader**

Validate the interface with a conservative Linux name expression and read only regular files under the configured root:

```go
var interfaceNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)

func (reader *SysfsTrafficReader) ReadInterfaceCounters(name string) (platform.InterfaceCounters, error) {
	if !interfaceNamePattern.MatchString(name) { return platform.InterfaceCounters{}, errors.New("invalid interface") }
	rx, err := readUintFile(filepath.Join(reader.root, name, "statistics", "rx_bytes"))
	if err != nil { return platform.InterfaceCounters{}, err }
	tx, err := readUintFile(filepath.Join(reader.root, name, "statistics", "tx_bytes"))
	if err != nil { return platform.InterfaceCounters{}, err }
	return platform.InterfaceCounters{RXBytes: rx, TXBytes: tx}, nil
}
```

No external process or shell command is allowed.

- [ ] **Step 4: Write failing tracker tests**

Cover initial zero baseline, normal deltas, a 2.5-second interval, counter decrease, interface read gap, new generation reset, old generation ignored, `End` reset and 60 independent nodes. Example:

```go
tracker.Begin("node_a", 7, "l2tp-ppv20001")
tracker.Sample("node_a", 7, counters(100, 200), epoch)
tracker.Sample("node_a", 7, counters(350, 300), epoch.Add(2*time.Second))
got := tracker.Snapshot("node_a")
if got.DownloadBytes != 250 || got.UploadBytes != 100 { t.Fatalf("%#v", got) }
if got.DownloadBytesPerSecond != 125 || got.UploadBytesPerSecond != 50 { t.Fatalf("%#v", got) }
```

- [ ] **Step 5: Run tracker tests and verify RED**

Run:

```sh
cd proxypool-core/src/proxypoold
go test ./internal/engine -run Traffic -count=1
```

Expected: collector symbols are undefined.

- [ ] **Step 6: Implement the tracker**

Keep all state behind one mutex. `Begin` replaces any prior node session with zero totals and no baseline. `Sample` only accepts the exact generation; a first or decreased counter re-establishes baseline with zero rate; normal samples use real elapsed time. `End` deletes only when generation matches. Return value snapshots must be copies.

- [ ] **Step 7: Run collector tests and commit**

Run:

```sh
cd proxypool-core/src/proxypoold
go test ./internal/platform/openwrt ./internal/engine -run 'Traffic|InterfaceTraffic' -count=1
```

Expected: PASS.

Commit:

```sh
git add proxypool-core/src/proxypoold/internal/platform \
  proxypool-core/src/proxypoold/internal/engine/traffic.go \
  proxypool-core/src/proxypoold/internal/engine/traffic_test.go
git commit -m "feat: collect per-session interface traffic"
```

---

### Task 3: Integrate traffic collection with scheduler lifecycle and status

**Files:**
- Modify: `proxypool-core/src/proxypoold/internal/engine/scheduler.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/scheduler_test.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/controller.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/controller_test.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/engine.go`
- Modify: `proxypool-core/src/proxypoold/cmd/proxypoold/main.go`
- Modify: `proxypool-core/src/proxypoold/cmd/proxypoold/main_test.go`

**Interfaces:**
- Produces: `NewSchedulerWithTraffic(controller, adapter, trafficReader, config, gates...)` while retaining `NewScheduler(...)` for existing callers.
- Produces: scheduler method `Traffic(nodeID string) TrafficSnapshot`.
- Extends: `RuntimeNodeSummary.Traffic TrafficSnapshot` with JSON name `traffic`.
- Extends: `SchedulerConfig.TrafficSampleInterval time.Duration`, defaulting to one second and set to 10 milliseconds only in focused tests.

- [ ] **Step 1: Write failing scheduler lifecycle tests**

Use a fake reader with per-interface counters and `TrafficSampleInterval: 10 * time.Millisecond`. Assert successful `EventValidated` begins a zero session, samples become visible, `takeSession`/stop removes metrics, and generation 8 cannot reuse generation 7 totals. Assert a reader error leaves connection state online and reports zero speed.

- [ ] **Step 2: Write failing status API tests**

Attach a fake scheduler that implements both `Submit(Job)` and `Traffic(nodeID)` and assert:

```json
"traffic":{"download_bytes":1024,"upload_bytes":512,"download_bytes_per_second":256,"upload_bytes_per_second":128}
```

is present under the matching runtime node while the runtime snapshot serialization still contains no `traffic` key.

- [ ] **Step 3: Run focused tests and verify RED**

Run:

```sh
cd proxypool-core/src/proxypoold
go test ./internal/engine ./cmd/proxypoold -run 'Traffic|Status' -count=1
```

Expected: scheduler/status integration does not exist.

- [ ] **Step 4: Implement scheduler sampling**

Keep the existing constructor as a nil-reader wrapper and add the new constructor. In `Run`, create a ticker only when a reader exists; on each tick copy the current sessions under `scheduler.mu`, release the lock, read counters, then call `tracker.Sample`. `putSession` calls `tracker.Begin`; `takeSession`, deletion and shutdown call `tracker.End` with the owned generation.

Never hold `scheduler.mu` while reading sysfs or calling controller methods.

- [ ] **Step 5: Expose traffic in live status only**

Add a private optional reporter interface:

```go
type trafficReporter interface { Traffic(string) TrafficSnapshot }
```

`AttachScheduler` records it when supported. `handleStatus` copies the traffic snapshot into each `RuntimeNodeSummary`. Do not add traffic to `NodeStatus` or `RuntimeSnapshot`.

- [ ] **Step 6: Wire the OpenWrt reader**

In live startup, construct `openwrtplatform.NewSysfsTrafficReader("/sys/class/net")` and pass it to `NewSchedulerWithTraffic`. Keep shadow/read-only mode unchanged.

- [ ] **Step 7: Run race and lifecycle tests, then commit**

Run:

```sh
cd proxypool-core/src/proxypoold
go test ./internal/engine ./cmd/proxypoold -count=1
go test -race ./internal/engine -run 'Traffic|Scheduler' -count=1
```

Expected: PASS with no race report.

Commit:

```sh
git add proxypool-core/src/proxypoold/internal/engine \
  proxypool-core/src/proxypoold/cmd/proxypoold
git commit -m "feat: publish live node traffic metrics"
```

---

### Task 4: Add note and traffic presentation to LuCI

**Files:**
- Modify: `luci-app-proxypool/luasrc/controller/proxypool.lua`
- Modify: `luci-app-proxypool/tests/test_controller.mjs`
- Modify: `luci-app-proxypool/luasrc/view/proxypool/main.htm`
- Modify: `luci-app-proxypool/htdocs/luci-static/resources/proxypool-v2.js`
- Modify: `luci-app-proxypool/htdocs/luci-static/resources/proxypool-v2.css`
- Modify: `luci-app-proxypool/tests/ui/proxypool-v2.test.mjs`

**Interfaces:**
- Consumes: status `desired.nodes[].note` and `runtime.nodes[].traffic`.
- Produces: controller form field `note` bounded to 800 UTF-8 bytes before backend rune validation.
- Produces: JS helpers `formatBytes(value)` and `formatRate(value)`.

- [ ] **Step 1: Write failing Lua bridge and form tests**

Assert `node_save_params` forwards a normal Chinese note, rejects embedded newline/control bytes, and rejects inputs over the bridge byte bound. Assert the generated request uses `note` and keeps password handling unchanged.

- [ ] **Step 2: Write failing UI model tests**

Add cases that verify:

```js
assert.equal(api.formatBytes(0), '0 B');
assert.equal(api.formatBytes(1536), '1.5 KB');
assert.equal(api.formatRate(1048576), '1 MB/s');
assert.equal(reduced.nodes[0].traffic.download_bytes, 2048);
assert.equal(api.sanitizedExport(status).desired.nodes[0].note, '微信专用');
```

Also verify `validateNodeForm()` trims and forwards the note, rejects 201 Unicode code points and a newline, and rendering uses `textContent` rather than `innerHTML` for `<img onerror=...>`.

- [ ] **Step 3: Run LuCI tests and verify RED**

Run:

```sh
node --test luci-app-proxypool/tests/test_controller.mjs
node --test luci-app-proxypool/tests/ui/proxypool-v2.test.mjs
```

Expected: note and traffic assertions fail.

- [ ] **Step 4: Implement bridge and state mapping**

In the Lua controller, reject CR/LF and ASCII control bytes before returning `note`. In `reduceState`, copy a normalized zero traffic object from the observed runtime node. Export `note` in sanitized JSON.

Implement byte formatting with a 1024 base, one decimal only when needed, finite nonnegative input clamping, and maximum unit TB.

- [ ] **Step 5: Implement the form and compact node cells**

Add `<input name="note" maxlength="200">`. Fill it in `openNodeEditor`, submit it through `validateNodeForm`, and render the note as a secondary line below node name.

Change the node table to two compact traffic columns:

```html
<th>累计流量</th><th>实时速度</th>
```

Each cell renders down/up on separate lines. Update empty-row `colSpan` to 11 and add responsive CSS so notes wrap without pushing action buttons off-screen.

- [ ] **Step 6: Run LuCI tests and commit**

Run:

```sh
node --test luci-app-proxypool/tests/test_controller.mjs
node --test luci-app-proxypool/tests/ui/proxypool-v2.test.mjs
```

Expected: PASS.

Commit:

```sh
git add luci-app-proxypool/luasrc/controller/proxypool.lua \
  luci-app-proxypool/luasrc/view/proxypool/main.htm \
  luci-app-proxypool/htdocs/luci-static/resources/proxypool-v2.js \
  luci-app-proxypool/htdocs/luci-static/resources/proxypool-v2.css \
  luci-app-proxypool/tests
git commit -m "feat: show node notes and live traffic"
```

---

### Task 5: Package the ProxyPool global LuCI theme

**Files:**
- Create: `luci-theme-proxypool/Makefile`
- Create: `luci-theme-proxypool/root/etc/uci-defaults/30_luci-theme-proxypool`
- Create: `luci-theme-proxypool/ucode/template/themes/proxypool/header.ut`
- Create: `luci-theme-proxypool/ucode/template/themes/proxypool/footer.ut`
- Create: `luci-theme-proxypool/ucode/template/themes/proxypool/sysauth.ut`
- Create: `luci-theme-proxypool/htdocs/luci-static/proxypool/proxypool-global.css`
- Create: `luci-theme-proxypool/htdocs/luci-static/proxypool/proxypool-global.js`
- Create: `scripts/test-theme-source-safety.sh`
- Create: `scripts/inspect-theme-ipk.sh`
- Create: `scripts/test-inspect-theme-ipk.sh`
- Modify: `luci-app-proxypool/Makefile`
- Modify: `luci-app-proxypool/luasrc/view/proxypool/main.htm`
- Modify: `luci-app-proxypool/luasrc/view/proxypool/locked.htm`
- Delete: `luci-app-proxypool/htdocs/luci-static/resources/proxypool-global.css`
- Delete: `luci-app-proxypool/htdocs/luci-static/resources/proxypool-global.js`
- Modify: `scripts/test-luci-package-source-safety.sh`
- Modify: `scripts/inspect-luci-ipk.sh`
- Modify: `scripts/test-inspect-luci-ipk.sh`
- Modify: `scripts/test-host.sh`

**Interfaces:**
- Produces: LuCI theme registration `luci.themes.ProxyPool=/luci-static/proxypool`.
- Produces: active `luci.main.mediaurlbase=/luci-static/proxypool` only after required files exist.
- Produces: one `#proxypool-global-menu` from the theme header on every non-blank LuCI page.

- [ ] **Step 1: Write failing source safety tests**

The test must require all three ucode templates, six navigation links, `aria-label`, `aria-current`, keyboard focus styling, status counters, mobile CSS, exact 0644/0755 packaging declarations, and an uninstall fallback. It must reject `sed -i`, writes into `/usr/share/ucode/luci/template/themes/bootstrap`, writes into `/www/luci-static/bootstrap`, and any symbolic-link payload.

Update the LuCI app safety test to require that `main.htm` no longer embeds `proxypool-global-menu` or loads global assets, preventing a duplicate bar.

- [ ] **Step 2: Run theme source tests and verify RED**

Run:

```sh
sh scripts/test-theme-source-safety.sh
sh scripts/test-luci-package-source-safety.sh
```

Expected: theme package is missing and the app still embeds the bar.

- [ ] **Step 3: Create the theme from the pinned Bootstrap templates**

Start from OpenWrt 23.05.3 LuCI commit `b07cf9dcfc37e021e5619a41c847e63afbd5d34a` Bootstrap `header.ut`, `footer.ut` and `sysauth.ut`, retaining Apache-2.0 notices. Use `/luci-static/bootstrap/cascade.css`, `/luci-static/bootstrap/mobile.css` and `/luci-static/bootstrap/favicon.png` directly; the theme package depends on `luci-theme-bootstrap` and does not copy its files.

Inside the existing `{% if (!blank_page): %}` block, render the ProxyPool navigation with `dispatcher.build_url()` links for:

```text
admin/services/proxypool
admin/network
admin/network/wireless
admin/system
admin/system/flash
admin/system/reboot
```

Set active classes from `ctx.request_path`. Load the theme-owned CSS and deferred JS on all non-blank pages. The JS fetches `admin/services/proxypool/api/read?action=status` and silently leaves `-` counters if unavailable.

- [ ] **Step 4: Implement safe activation and removal**

The uci-default must test regular files before changing UCI:

```sh
theme_root=${IPKG_INSTROOT:-}/usr/share/ucode/luci/template/themes/proxypool
asset_root=${IPKG_INSTROOT:-}/www/luci-static/proxypool
[ -f "$theme_root/header.ut" ] && [ -f "$theme_root/footer.ut" ] &&
  [ -f "$asset_root/proxypool-global.css" ] && [ -f "$asset_root/proxypool-global.js" ] || exit 0
uci -q set luci.themes.ProxyPool='/luci-static/proxypool'
uci -q set luci.main.mediaurlbase='/luci-static/proxypool'
uci -q commit luci
```

The package `postrm` switches to `/luci-static/bootstrap` only when the removed theme is active, removes `luci.themes.ProxyPool`, commits, and clears LuCI caches. Pin the uci-default to 0755 and templates/assets to 0644.

- [ ] **Step 5: Remove the page-local navigation**

Make `luci-app-proxypool` depend on `luci-theme-proxypool`. Remove the local navigation markup/assets from `main.htm`. Convert `locked.htm` to use `<%+header%>` and `<%+footer%>` with an explicit Chinese service state instead of the old fullscreen loading illusion, so it also receives global navigation.

- [ ] **Step 6: Add failing and passing IPK fixture tests**

`inspect-theme-ipk.sh` must require package name `luci-theme-proxypool`, architecture `all`, dependencies `luci-base` and `luci-theme-bootstrap`, an exact regular-file payload, expected modes, no symlinks, no Bootstrap-owned paths, and the safe uci-default/postrm contracts. `test-inspect-theme-ipk.sh` builds one good fixture and corrupts each contract individually.

Run:

```sh
sh scripts/test-theme-source-safety.sh
sh scripts/test-luci-package-source-safety.sh
sh scripts/test-inspect-theme-ipk.sh
sh scripts/test-inspect-luci-ipk.sh
```

Expected: PASS.

- [ ] **Step 7: Commit the theme package**

Commit:

```sh
git add luci-theme-proxypool luci-app-proxypool scripts
git commit -m "feat: add global ProxyPool LuCI theme"
```

---

### Task 6: Extend package and firmware release contracts

**Files:**
- Modify: `.github/workflows/build-fast.yml`
- Modify: `.github/workflows/build.yml`
- Modify: `config/gl-mt6000.config`
- Modify: `scripts/test-release-contracts.sh`
- Modify: `scripts/test-kernel-isolation-contract.sh`
- Modify: `scripts/test-host.sh`
- Modify: `proxypool-core/Makefile`
- Modify: `luci-app-proxypool/Makefile`

**Interfaces:**
- Produces: three package artifacts: core, LuCI app and LuCI theme.
- Produces: full firmware config containing `CONFIG_PACKAGE_luci-theme-proxypool=y`.

- [ ] **Step 1: Write failing release-contract assertions**

Require both workflows to copy/install/build/inspect the theme package. Require the fast artifact to include one unique theme IPK and the full firmware evidence to include the same. Require `config/gl-mt6000.config` to select the theme while retaining Bootstrap as the fallback dependency.

- [ ] **Step 2: Run release tests and verify RED**

Run:

```sh
sh scripts/test-release-contracts.sh
sh scripts/test-kernel-isolation-contract.sh
```

Expected: workflows and firmware config do not yet mention the theme.

- [ ] **Step 3: Update versions and workflows**

Bump core and LuCI app package releases once, set the theme package version to match the feature release, and update the SDK flow to:

```sh
cp -r proxypool-core luci-app-proxypool luci-theme-proxypool sdk/package/proxypool/
make package/proxypool/luci-theme-proxypool/clean
make package/proxypool/luci-theme-proxypool/compile V=s -j1
sh ./scripts/inspect-theme-ipk.sh "${theme_packages[0]}"
```

Update full source feed copying, forced feed installation, `.config` proof, package inspection and evidence copying for the exact theme package.

- [ ] **Step 4: Run static release and host gates**

Run:

```sh
sh scripts/test-release-contracts.sh
sh scripts/test-kernel-isolation-contract.sh
sh scripts/test-host.sh
```

Expected: PASS.

- [ ] **Step 5: Commit release integration**

Commit:

```sh
git add .github/workflows config scripts proxypool-core/Makefile \
  luci-app-proxypool/Makefile luci-theme-proxypool/Makefile
git commit -m "build: package global theme in firmware"
```

---

### Task 7: Verify packages, build the GL-MT6000 firmware and collect evidence

**Files:**
- Generated outside Git: `package-evidence/<short-commit>/`
- Generated outside Git: `firmware-evidence/<short-commit>/`

**Interfaces:**
- Consumes: the exact committed source SHA from Tasks 1-6.
- Produces: verified aarch64 core IPK, all-architecture LuCI/theme IPKs and one unique GL-MT6000 squashfs sysupgrade image.

- [ ] **Step 1: Run clean local verification**

Run:

```sh
git diff --check
sh scripts/test-host.sh
cd proxypool-core/src/proxypoold
go test ./... -count=1
go test -race ./internal/engine ./internal/platform/openwrt -count=1
```

Expected: every command exits 0 and race output contains no warning.

- [ ] **Step 2: Run the cached package build on M1068**

Export the exact Git commit with file mode 0022 to the dedicated build account, refresh the local package feed, and clean-build only `proxypool-core`, `luci-app-proxypool`, and `luci-theme-proxypool`. Run all three IPK inspectors and regenerate `SHA256SUMS` under `package-evidence/<short-commit>/`.

Expected: exactly one package of each name, core architecture `aarch64_cortex-a53`, LuCI/theme architecture `all`, and all checksum verification succeeds.

- [ ] **Step 3: Run the incremental full firmware build on M1068**

Use the pinned OpenWrt 23.05.3 source and pinned feed commits already cached on M1068. Refresh the `src-link proxypool` feed before `make defconfig`, assert all three packages equal `y`, then build with available cores. Do not run a second full build unless the package SHA changed or the current full build failed with a code-related error.

- [ ] **Step 4: Verify and download the firmware evidence**

Require exactly one:

```text
*glinet_gl-mt6000*squashfs-sysupgrade.bin
```

Verify server-side and local `SHA256SUMS`, preserve build logs, `.config`, feed commits and kernel-patch hashes, then copy the artifact to `firmware-evidence/<short-commit>/`.

- [ ] **Step 5: Final repository and artifact audit**

Run:

```sh
git status --short
git log -1 --format='%H'
```

Confirm the evidence directory short hash matches that full commit, the worktree has no accidental files, and report the exact local sysupgrade path plus SHA256 to the user.
