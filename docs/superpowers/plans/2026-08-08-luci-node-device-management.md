# ProxyPool LuCI Chinese Node Device Management Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver a Chinese-only LuCI status experience with a reliable top navigation bar and node-centric searchable, multi-device binding, unbinding, and reassignment.

**Architecture:** Keep English machine codes in the Go API and runtime, but translate them through centralized pure JavaScript presentation helpers. Add a revision-protected `device.bindings.replace` method that replaces one node's device membership atomically, updates all DHCP reservations in one transaction, and schedules affected old nodes before the target node. The LuCI modal computes a minimal preview from the current device snapshot and submits the complete selected membership for the target node.

**Tech Stack:** Go 1.x, OpenWrt netifd/uci/dnsmasq integration, Lua LuCI controller, ES5-compatible browser JavaScript, Node.js built-in test runner, POSIX package/integration scripts.

## Global Constraints

- Ordinary LuCI UI text shows Chinese only; raw English codes remain in API responses, sanitized exports, and diagnostic artifacts.
- One modal submission handles at most 60 devices and changes the desired configuration revision exactly once.
- Cross-node reassignment requires a Chinese confirmation listing the source and target node.
- LAN/WiFi discovery remains automatic; no manual MAC input is added.
- Fail-closed isolation, local-WAN prohibition, IPv6 prohibition, UDP prohibition, and management-page allowlisting are unchanged.
- Existing `device.bind` and `device.unbind` methods remain compatible.
- A full firmware build starts only after local/remote tests and the fast package build pass.

---

### Task 1: Atomic DHCP Reservation Replacement

**Files:**
- Modify: `proxypool-core/src/proxypoold/internal/platform/contracts.go`
- Modify: `proxypool-core/src/proxypoold/internal/platform/openwrt/devices.go`
- Test: `proxypool-core/src/proxypoold/internal/platform/openwrt/devices_test.go`

**Interfaces:**
- Consumes: `model.Device`, `platform.CommandRunner`, `platform.DeviceSource`.
- Produces: `LeaseManager.Replace(context.Context, before, after []model.Device, revision uint64) error`.

- [ ] **Step 1: Write failing behavior tests**

Add table-driven tests that use the real OpenWrt lease manager with the existing recording command runner. Hand-derive the expected command behavior:

```go
func TestLeaseManagerReplaceCommitsAndReloadsOnce(t *testing.T) {
    before := []model.Device{configuredDevice("00:11:22:33:44:55", "192.168.9.10")}
    after := []model.Device{configuredDevice("00:11:22:33:44:66", "192.168.9.11")}
    // Assert one `uci commit dhcp`, one dnsmasq reload, old owned section
    // deleted, and new exact section written.
}

func TestLeaseManagerReplaceRejectsConflictWithoutCommit(t *testing.T) {
    // Existing non-ProxyPool host owns the requested MAC/IP.
    // Assert error and no commit/reload.
}
```

Also cover duplicate input IDs, unconfirmed new devices, ownership mismatch, stage failure, commit failure, reload failure with exact touched-section restoration, and context cancellation.

- [ ] **Step 2: Run the focused test remotely and verify RED**

Sync the worktree to an unprivileged temporary directory on M1068 and run:

```sh
go test ./internal/platform/openwrt -run 'TestLeaseManagerReplace' -count=1
```

Expected: compilation failure because `Replace` is absent.

- [ ] **Step 3: Implement one-transaction replacement**

Extend the interface and OpenWrt implementation:

```go
type LeaseManager interface {
    Apply(context.Context, model.Device, uint64) error
    Remove(context.Context, model.Device, uint64) error
    Replace(context.Context, []model.Device, []model.Device, uint64) error
}
```

`Replace` must normalize sorted maps keyed by device ID, inventory DHCP once, validate all removals and additions before mutation, confirm every newly reserved device, stage every delete/set, verify the staged projection, commit once, reload once, and restore the exact prior touched sections if a post-commit operation fails. Empty changes return success without UCI calls.

- [ ] **Step 4: Run focused and package-level Go tests**

```sh
go test ./internal/platform/openwrt -run 'TestLeaseManager' -count=1
go test ./internal/platform/... -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```sh
git add proxypool-core/src/proxypoold/internal/platform/contracts.go \
  proxypool-core/src/proxypoold/internal/platform/openwrt/devices.go \
  proxypool-core/src/proxypoold/internal/platform/openwrt/devices_test.go
git commit -m "feat: replace device leases atomically"
```

### Task 2: Revision-Protected Node Membership API

**Files:**
- Modify: `proxypool-core/src/proxypoold/internal/api/server.go`
- Modify: `proxypool-core/src/proxypoold/cmd/proxypoold/main.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/controller.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/scheduler.go`
- Test: `proxypool-core/src/proxypoold/internal/api/server_test.go`
- Test: `proxypool-core/src/proxypoold/internal/engine/controller_test.go`
- Test: `proxypool-core/src/proxypoold/internal/engine/scheduler_test.go`

**Interfaces:**
- Consumes: `LeaseManager.Replace` from Task 1.
- Produces: JSON-RPC method `device.bindings.replace` with params `{node_id:string, device_ids:string[], expected_revision:uint64}` and standard `mutationResult`.

- [ ] **Step 1: Write failing controller tests**

Add literal request/response fixtures proving:

```go
request := controllerRequest("replace-1", "device.bindings.replace",
    `{"node_id":"node_b","device_ids":["device_a","device_c"],"expected_revision":3}`)
```

The success test begins with `device_a` on `node_a`, `device_b` on `node_b`, and an unconfigured confirmed `device_c`. It expects exactly `device_a` and `device_c` enabled on `node_b`, `device_b` disabled/unbound, revision `4`, one lease replacement, and a job ordered as `node_a`, then `node_b`.

Add rejection tests for duplicate device IDs, 61 IDs, unknown target, disabled target, unknown/unconfirmed device, stale revision, malformed arrays, unknown JSON fields, and storage/lease failure. Each rejection must reload the store and assert revision and all device assignments are unchanged. Add replay coverage asserting the same request ID returns the original job/revision without another lease transaction.

- [ ] **Step 2: Verify RED remotely**

```sh
go test ./internal/engine -run 'TestControllerReplaceDeviceBindings' -count=1
```

Expected: FAIL with `unknown_method` or missing handler.

- [ ] **Step 3: Implement strict params and atomic mutation**

Add:

```go
type replaceDeviceBindingsParams struct {
    NodeID           string   `json:"node_id"`
    DeviceIDs        []string `json:"device_ids"`
    ExpectedRevision *uint64  `json:"expected_revision"`
}
```

The handler deduplicates and bounds IDs, loads one confirmed discovery snapshot, constructs `next` without mutating `current`, removes target-node devices absent from the requested set, migrates or creates selected devices, validates target/global enablement, invokes `leaseManager.Replace` with all previously reserved and resulting reserved devices, performs one store `Replace`, and creates `device.bindings.replace`. Affected old nodes are sorted and the target node is appended last.

If desired-store replacement fails after DHCP replacement succeeds, call `leaseManager.Replace` again with the resulting and original reserved-device sets under a bounded rollback context. Return success only when both desired configuration and DHCP reservation projection agree on the new revision; otherwise return an internal error and retain fail-closed runtime behavior.

- [ ] **Step 4: Make the scheduler process the batch in safe order**

Treat `device.bindings.replace` jobs like multi-node `device.bind` jobs: run node progress sequentially in job order. For this job kind, online affected nodes reconnect/refresh so their device authorization sets are replaced; the target runs last to prevent a migrated device from being authorized by two nodes simultaneously.

- [ ] **Step 5: Register the method and verify tests**

Register the exact method in `defaultMethods` and `liveControlMethods`, then run:

```sh
go test ./internal/api ./internal/engine -count=1
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```sh
git add proxypool-core/src/proxypoold/internal/api/server.go \
  proxypool-core/src/proxypoold/cmd/proxypoold/main.go \
  proxypool-core/src/proxypoold/internal/engine/controller.go \
  proxypool-core/src/proxypoold/internal/engine/scheduler.go \
  proxypool-core/src/proxypoold/internal/api/server_test.go \
  proxypool-core/src/proxypoold/internal/engine/controller_test.go \
  proxypool-core/src/proxypoold/internal/engine/scheduler_test.go
git commit -m "feat: replace node device bindings in one transaction"
```

### Task 3: LuCI Batch API Bridge

**Files:**
- Modify: `luci-app-proxypool/luasrc/controller/proxypool.lua`
- Test: `luci-app-proxypool/tests/test_controller.mjs`

**Interfaces:**
- Consumes: `device.bindings.replace` from Task 2.
- Produces: LuCI write action `bindings_replace` accepting repeated/form-encoded device IDs as one bounded comma-free JSON-safe list.

- [ ] **Step 1: Write a failing controller contract test**

Extend the executable Lua-controller harness to post:

```text
action=bindings_replace
node_id=node_b
device_ids_json=["device_a","device_c"]
expected_revision=3
```

Assert the RPC receives exactly:

```json
{"node_id":"node_b","device_ids":["device_a","device_c"],"expected_revision":3}
```

Add invalid JSON, non-array, duplicate, bad ID, and more-than-60 rejection cases that return HTTP 400 without an RPC call.

- [ ] **Step 2: Run and verify RED**

```sh
node --test luci-app-proxypool/tests/test_controller.mjs
```

Expected: FAIL because `bindings_replace` is unknown.

- [ ] **Step 3: Implement exact parsing**

Register `bindings_replace = "device.bindings.replace"`. Decode `device_ids_json` with `luci.jsonc.parse`, require a dense array of 0–60 unique `exact_id` strings, validate `node_id` and `expected_revision`, and return the strict RPC params. Do not accept object keys, nulls, numbers, or trailing data.

- [ ] **Step 4: Run controller and RPC tests**

```sh
node --test luci-app-proxypool/tests/test_controller.mjs
lua luci-app-proxypool/tests/test_rpc.lua
```

Expected: PASS.

- [ ] **Step 5: Commit**

```sh
git add luci-app-proxypool/luasrc/controller/proxypool.lua \
  luci-app-proxypool/tests/test_controller.mjs
git commit -m "feat: expose batch device binding to LuCI"
```

### Task 4: Chinese Presentation and Device Selection Model

**Files:**
- Modify: `luci-app-proxypool/htdocs/luci-static/resources/proxypool-v2.js`
- Test: `luci-app-proxypool/tests/ui/proxypool-v2.test.mjs`

**Interfaces:**
- Produces pure helpers `stateLabel(kind, value)`, `errorLabel(code)`, `jobKindLabel(kind)`, `boundDevicesByNode(nodes, devices)`, `deviceBindingRows(devices, nodes, targetNodeID, query)`, and `buildBindingReplacement(originalRows, selectedIDs, revision)`.

- [ ] **Step 1: Write failing localization tests**

Use literal assertions for every runtime state and representative error classes:

```js
assert.equal(ui.stateLabel('node', 'backoff'), '等待重试');
assert.equal(ui.errorLabel('dataplane_failed'), '网络通道建立失败');
assert.equal(ui.stateLabel('job', 'running'), '执行中');
assert.equal(ui.errorLabel('future_code'), '未知错误');
assert.equal(ui.stateLabel('node', 'future_state'), '未知状态');
```

Update job-summary expectations so no visible string contains `probe_failed`, `running`, or raw job kinds.

- [ ] **Step 2: Verify RED locally**

```powershell
node --test luci-app-proxypool/tests/ui/proxypool-v2.test.mjs
```

Expected: FAIL because the presentation helpers are absent.

- [ ] **Step 3: Implement centralized maps**

Add frozen lookup objects for node/job/diagnostic states, error codes, job kinds, and job steps. Return Chinese fallbacks for unknown values. Keep raw state values on view models only for CSS classes and API payloads; every text node must use the label helpers.

- [ ] **Step 4: Write failing binding-model tests**

Fixtures must include unbound, target-bound, and other-node-bound devices. Assert search by hostname, both IP fields, normalized MAC, ingress, and node name. Assert rows expose Chinese ownership labels and that a selection change produces:

```js
{
  changed: true,
  device_ids: ['device_keep', 'device_new', 'device_move'],
  migrations: [{ device_id: 'device_move', device_name: '手机', from_node: '节点 A', to_node: '节点 B' }],
  expected_revision: 9
}
```

Also assert deselection unbinds target membership, unselected other-node devices remain unchanged, hidden search results retain selection, and an unchanged selection reports `changed: false`.

- [ ] **Step 5: Implement the minimal pure model and verify GREEN**

Implement deterministic sorting by hostname/IP/MAC, case-insensitive trimmed search, and complete target membership payloads. Run:

```powershell
node --test luci-app-proxypool/tests/ui/proxypool-v2.test.mjs
```

Expected: PASS.

- [ ] **Step 6: Commit**

```sh
git add luci-app-proxypool/htdocs/luci-static/resources/proxypool-v2.js \
  luci-app-proxypool/tests/ui/proxypool-v2.test.mjs
git commit -m "feat: localize statuses and model device selection"
```

### Task 5: Navigation, Bound Device List, and Searchable Modal

**Files:**
- Modify: `luci-app-proxypool/luasrc/view/proxypool/main.htm`
- Modify: `luci-app-proxypool/htdocs/luci-static/resources/proxypool-v2.js`
- Modify: `luci-app-proxypool/htdocs/luci-static/resources/proxypool-v2.css`
- Modify: `luci-app-proxypool/htdocs/luci-static/resources/proxypool-global.js`
- Modify: `luci-app-proxypool/htdocs/luci-static/resources/proxypool-global.css`
- Test: `luci-app-proxypool/tests/ui/proxypool-v2.test.mjs`
- Test: `luci-app-proxypool/tests/test_controller.mjs`

**Interfaces:**
- Consumes: the pure model from Task 4 and LuCI `bindings_replace` from Task 3.
- Produces: accessible `#pp-v2-binding-modal`, search input, device checkbox list, migration summary, and a page-local `#proxypool-global-menu`.

- [ ] **Step 1: Write failing DOM/template behavior tests**

Extend the existing lightweight DOM fixture so rendering two nodes and three devices proves that:

- each node row shows bound device name, selected IP, and MAC;
- “绑定设备” opens the target modal;
- typing filters visible rows without clearing checked IDs;
- ownership badges identify the current and other node in Chinese;
- save is disabled for no change and during submission;
- a migration invokes `confirm()` with source and target names;
- confirmed save emits one `bindings_replace` request with the complete selected membership;
- successful submit closes the modal and triggers polling; failure preserves it.

Add a template route test that renders `main.htm` and checks one navigation landmark with the four expected links, without modifying the shared LuCI header.

- [ ] **Step 2: Verify RED locally**

```powershell
node --test luci-app-proxypool/tests/ui/proxypool-v2.test.mjs \
  luci-app-proxypool/tests/test_controller.mjs
```

Expected: FAIL because the modal/navigation elements and event handlers are absent.

- [ ] **Step 3: Add page-local navigation and modal markup**

Load `proxypool-global.css` and `proxypool-global.js` from `main.htm`, pass LuCI-generated URLs through `data-*` attributes, and add a `<nav>` fallback that remains useful with JavaScript disabled. Do not set `pp-wide-layout` and do not hide the stock LuCI navigation.

Add the binding modal with search input, scrollable checkbox list, selected count, migration summary, cancel, and save controls. Include `aria-modal`, an explicit label, and a polite live region.

- [ ] **Step 4: Render devices and wire modal behavior**

Add an “已绑定设备” cell and “绑定设备” action per node. Render every bound device as a wrapping item. Maintain selected IDs independently of the current search result. Before migrating, build Chinese lines in the form `手机：节点 A → 节点 B`; cancel leaves all state unchanged. Submit `device_ids_json: JSON.stringify(model.device_ids)` through one `mutate('bindings_replace', ...)` call.

- [ ] **Step 5: Add responsive styling**

Keep the modal within 90vh, use a sticky search/summary area, make each checkbox row readable at phone widths, wrap bound-device items, and give status/ownership badges semantic colors without relying on color alone.

- [ ] **Step 6: Run LuCI tests and commit**

```powershell
node --test luci-app-proxypool/tests/ui/proxypool-v2.test.mjs \
  luci-app-proxypool/tests/test_controller.mjs
```

Expected: PASS.

```sh
git add luci-app-proxypool/luasrc/view/proxypool/main.htm \
  luci-app-proxypool/htdocs/luci-static/resources/proxypool-v2.js \
  luci-app-proxypool/htdocs/luci-static/resources/proxypool-v2.css \
  luci-app-proxypool/htdocs/luci-static/resources/proxypool-global.js \
  luci-app-proxypool/htdocs/luci-static/resources/proxypool-global.css \
  luci-app-proxypool/tests/ui/proxypool-v2.test.mjs \
  luci-app-proxypool/tests/test_controller.mjs
git commit -m "feat: manage devices directly from node list"
```

### Task 6: Release Gates, Package Version, and Delivery

**Files:**
- Modify: `proxypool-core/Makefile`
- Modify: `luci-app-proxypool/Makefile`
- Modify: `scripts/inspect-luci-ipk.sh` to require the page-local global assets and binding-modal markers in the packaged main view
- Test: existing scripts under `scripts/` and `tests/`

**Interfaces:**
- Produces versioned `aarch64_cortex-a53` core and matching `all` LuCI IPKs, followed by a GL-MT6000 sysupgrade image only after package success.

- [ ] **Step 1: Bump package releases**

Increment the core and LuCI package releases exactly once so opkg cannot retain cached pre-feature assets.

- [ ] **Step 2: Run complete source verification**

Local:

```powershell
node --test luci-app-proxypool/tests/ui/proxypool-v2.test.mjs \
  luci-app-proxypool/tests/test_controller.mjs
```

M1068:

```sh
go test ./... -count=1
./scripts/test-luci-package-source-safety.sh
./scripts/test-v2-live-integration.sh
./scripts/test-v2-phase5-gates.sh
./scripts/test-release-contracts.sh
```

Expected: all commands PASS with no skipped binding/localization assertions.

- [ ] **Step 3: Build and inspect fast packages on M1068**

Use the persistent OpenWrt SDK/download/ccache directories. Run the repository package workflow command, then execute `scripts/inspect-core-ipk.sh` and `scripts/inspect-luci-ipk.sh` against the resulting IPKs. Verify core architecture `aarch64_cortex-a53`, LuCI architecture `all`, matching dependencies, resource loads, executable modes, and checksums.

- [ ] **Step 4: Commit release metadata and push**

```sh
git add proxypool-core/Makefile luci-app-proxypool/Makefile scripts/inspect-luci-ipk.sh
git commit -m "build: release Chinese device management UI"
git push origin codex/proxypool-v2-phase1
```

- [ ] **Step 5: Build firmware only after package success**

Trigger the self-hosted/full GL-MT6000 firmware workflow for the exact verified commit. Download the artifact into `firmware-evidence/<short-commit>/`, verify `SHA256SUMS`, and identify the unique `*glinet_gl-mt6000*squashfs-sysupgrade.bin`.

- [ ] **Step 6: Report delivery evidence**

Report the exact commit, passing test groups, IPK paths, firmware path and SHA256. Do not describe the feature as complete until the verified artifacts exist.
