# ProxyPool V2 Phase 6 Stability and Release Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 完成 12～24 小时稳定性、重启/备份/sysupgrade/回退验证，在全部四轮硬件门通过后删除 V1 运行路径并生成可发布固件。

**Architecture:** 发布门以自动采样和 WAN/bridge 抓包证据为准，不以页面显示代替。升级前后用 schema/revision/节点/设备摘要对比；V1 删除是最后一个独立可回滚提交，发布仍保留上一版固件和配置恢复说明。

**Tech Stack:** OpenWrt 23.05.3 SDK/ImageBuilder、GL-MT6000、procd/ubus/nft/ip/tcpdump、POSIX shell、GitHub Actions、sysupgrade/opkg。

## Global Constraints

- 继承路线图全部约束；前 1～3 轮真机报告必须通过才能开始第四轮。
- 第四轮使用 40～50 个常用有效节点持续 12～24 小时。
- 任何泄漏、终端互访、永久 connecting、无界资源增长或升级后绑定丢失都阻断发布。
- 不在 soak 中把日志持续写入 flash；采样写 `/tmp`，结束时打包下载。
- 删除 V1 代码前创建已验证的 V1 固件、V1 配置备份和 V2->V1 导出。
- release 固件只面向已验证的 GL-MT6000/OpenWrt 23.05.3 组合。

---

### Task 1: 建立资源采样、网络泄漏和不变量检查器

**Files:**
- Create: `scripts/device-test/monitor-proxypool.sh`
- Create: `scripts/device-test/assert-invariants.sh`
- Create: `scripts/device-test/tests/test-monitor.sh`
- Modify: `scripts/test-host.sh`

**Interfaces:**
- Produces: timestamped CSV/JSONL resource samples, invariant failure events, bounded `/tmp` output.

- [ ] **Step 1: Write fixture-driven shell tests**

Feed fake `/proc`, `ubus`, `nft -j` and `ip -j` output. Assert parser records daemon/xl2tpd RSS, goroutines, FD, process count, online/state counts, nft generation, route count and WAN leak counters; missing command becomes an explicit error sample, not zero.

- [ ] **Step 2: Verify RED**

```bash
bash scripts/device-test/tests/test-monitor.sh
```

- [ ] **Step 3: Implement bounded sampling**

Sample every 60 seconds to `/tmp/proxypool/soak/metrics.csv`, rotate at 8 MiB and retain two files. `assert-invariants.sh` checks: base drop present, no unauthorized LAN->WAN counter increase, no duplicate policy IDs/tables/listeners, all online nodes have expected adapter+route+nft+DNS generation, and no orphan ProxyPool process.

- [ ] **Step 4: Define objective resource gate**

After a 60-minute warm-up, fail if RSS, goroutine or FD count shows monotonic growth in 10 consecutive hourly samples or final value exceeds warm-up median by both 20% and a fixed floor (RSS 16 MiB, FD 64, goroutine 32). A temporary spike that returns below the threshold is recorded but not failed.

- [ ] **Step 5: Verify and commit**

```bash
./scripts/test-host.sh
bash scripts/device-test/tests/test-monitor.sh
git diff --check
git add scripts/device-test scripts/test-host.sh
git commit -m "test: add stability and network invariant monitors"
```

### Task 2: 验证配置备份、恢复、sysupgrade 和 V1 回退

**Files:**
- Rewrite: `proxypool-core/files/backup.sh`
- Create: `proxypool-core/src/proxypoold/internal/config/backup_manifest.go`
- Create: `proxypool-core/src/proxypoold/internal/config/backup_manifest_test.go`
- Create: `scripts/device-test/backup-restore.sh`
- Create: `docs/testing/backup-upgrade-rollback.md`
- Modify: `proxypool-core/Makefile`

**Interfaces:**
- Produces: checksummed backup manifest, restore preflight, documented V1/V2 recovery artifacts.

- [ ] **Step 1: Write manifest and tamper tests**

Manifest includes schema version, config revision, SHA-256 for ProxyPool/DHCP/firewall/wireless relevant files, node count, device count and pending-binding count; it excludes secret values. A changed/missing file rejects restore before any write.

- [ ] **Step 2: Verify RED**

```bash
cd proxypool-core/src/proxypoold
go test ./internal/config -run TestBackupManifest -v
```

- [ ] **Step 3: Implement backup/restore transaction**

Backup archives only explicit paths, mode `0600`, with manifest and V1 recovery export. Restore extracts to a new temp directory, rejects absolute/`..` entries and symlinks, verifies hashes/schema, applies permanent fail-closed, stops V2, atomically replaces configs, then restarts reconciliation. Failure retains fail-closed and original files.

- [ ] **Step 4: Add sysupgrade preservation and rollback checklist**

Package conffiles and sysupgrade backup list must preserve `/etc/config/proxypool`, migration marker and required DHCP/firewall/wireless sections. Checklist records old firmware SHA-256, new firmware SHA-256, backup SHA-256, GL-MT6000 recovery method and a tested V2->V1 export; it never embeds credentials in Markdown.

- [ ] **Step 5: Verify and commit**

```bash
./scripts/test-host.sh
git diff --check
git add proxypool-core scripts/device-test/backup-restore.sh docs/testing/backup-upgrade-rollback.md
git commit -m "feat: harden backup upgrade and rollback flow"
```

### Task 3: 执行第四轮 12～24 小时稳定性和升级测试

**Files:**
- Create: `scripts/device-test/round4-soak.sh`
- Create: `docs/testing/round4-soak-template.md`
- Create: `docs/testing/release-acceptance.md`

**Interfaces:**
- Produces: one reproducible Round 4 report and signed-off acceptance checklist.

- [ ] **Step 1: Implement guarded soak orchestrator**

Preflight requires Round 1～3 report IDs/checksums, 40～50 enabled nodes, at least one L2TP/SOCKS5/SLP node when available, config backup, free `/tmp` space and correct model/version. It starts monitor/invariant checker, records baseline, and installs traps that stop capture without changing network policy.

- [ ] **Step 2: Define fault schedule**

During soak: one WAN outage/recovery, one shared xl2tpd TERM, one proxypoold TERM, one firewall reload, one network reload and several individual node failures. Each event records recovery duration and device authorization counters. Do not automate router reboot until current logs are downloaded.

- [ ] **Step 3: Execute passive soak and analyze**

Run 12～24 hours. Report node attempt distributions, longest recovery, RSS/goroutine/FD trend, orphan count, DNS SERVFAIL/fallback counters, UDP/IPv6 drop counters and any invariant failure. Credentials are redacted before leaving the router.

- [ ] **Step 4: Execute reboot, backup/restore and sysupgrade cases**

After soak capture: reboot and verify staged restoration; export backup, factory-safe restore test as agreed, then sysupgrade with config preservation. Compare pre/post manifest counts and sample every binding. Finally perform or dry-run the documented V1 rollback on the test device, never the only production device without recovery access.

- [ ] **Step 5: Commit templates and record the actual report separately**

```bash
git add scripts/device-test/round4-soak.sh docs/testing
git commit -m "test: add final soak and upgrade acceptance gate"
```

Reports containing server addresses are stored outside Git or sanitized before commit.

### Task 4: 在全部硬件门通过后删除 V1 运行路径

**Files:**
- Delete: `proxypool-core/files/proxypool.sh`
- Delete: `proxypool-core/files/l2tp-manager.sh`
- Delete: `proxypool-core/files/socks5-manager.sh`
- Delete: `proxypool-core/files/slp-manager.sh`
- Delete: `proxypool-core/files/firewall.sh`
- Delete: `proxypool-core/files/status.sh`
- Delete: `proxypool-core/files/watchdog.sh`
- Delete: `proxypool-core/files/timeout-check.sh`
- Delete: `proxypool-core/files/timeout-rotate.sh`
- Delete: `proxypool-core/files/dns-manager.sh`
- Modify: `proxypool-core/files/ppp-up.sh`
- Modify: `proxypool-core/files/ppp-down.sh`
- Modify: `proxypool-core/files/proxypool.init`
- Modify: `proxypool-core/Makefile`
- Modify: `luci-app-proxypool/root/etc/uci-defaults/luci-proxypool`
- Modify: `files/etc/uci-defaults/98-proxypool-wireless`
- Modify: `files/etc/uci-defaults/99-proxypool-lan`
- Modify: `.github/workflows/build-fast.yml`
- Modify: `.github/workflows/build.yml`

**Interfaces:**
- Produces: one V2 runtime path with no legacy cron/background/firewall writers.

- [ ] **Step 1: Add a failing legacy-reference gate**

Extend `scripts/test-host.sh` to reject runtime references to deleted scripts, `setsid`, watchdog cron, direct PPP firewall rebuild, `firewall.@zone[1]`, V1 start branch and `runtime_backend=v1`. Permit legacy names only inside migration tests/fixtures and historical docs.

- [ ] **Step 2: Verify the gate fails before deletion**

```bash
./scripts/test-host.sh
```

Expected: FAIL listing current Makefile/init/controller/script references.

- [ ] **Step 3: Delete legacy files and simplify lifecycle**

Remove installs/calls/cron entries. PPP hooks become notification-only. Init always starts shared system xl2tpd as needed and `proxypoold`; stop revokes dynamic authorization through daemon with a deadline, then retains persistent base rules. Rewrite uci-defaults to named sections and remove positional firewall-zone modifications.

- [ ] **Step 4: Make release workflows build-only unless tagged**

Both workflows run host tests, SDK package builds and image build. Artifact upload occurs for manual/branch builds; GitHub Release occurs only for an explicit version tag. Firmware manifest includes package versions, Git commit and SHA-256.

- [ ] **Step 5: Verify and commit deletion independently**

```bash
./scripts/test-host.sh
bash tests/integration/fail_closed_test.sh
git diff --check
# SDK and ImageBuilder full build
git add -A proxypool-core luci-app-proxypool files .github/workflows scripts/test-host.sh
git commit -m "refactor: remove legacy proxypool runtime writers"
```

If any gate fails, revert only this deletion commit and fix without re-enabling V1 and V2 simultaneously on hardware.

### Task 5: 更新用户文档并生成发布候选

**Files:**
- Rewrite: `README.md`
- Create: `docs/ARCHITECTURE_V2.md`
- Create: `docs/OPERATIONS.md`
- Create: `docs/TROUBLESHOOTING.md`
- Create: `docs/SECURITY_BOUNDARIES.md`
- Modify: `docs/GLOBAL_MENU_INSTALL.md`
- Modify: `docs/IP_LOCATION_SETUP.md`
- Modify: `docs/SLP_PROTOCOL.md`

**Interfaces:**
- Produces: installation, binding, operation, failure semantics, rollback and security-limit documentation matching shipped behavior.

- [ ] **Step 1: Write documentation acceptance checklist**

The docs must state: device connects by DHCP first; no manual MAC; unbound/offline means no internet; LuCI allowance; terminal isolation; 60-node max; shared xl2tpd recovery; TCP-only SOCKS5/SLP; UDP/IPv6 limitations; expected微信 limitations; migration/backup/rollback; diagnostic redaction; downstream-switch non-goal.

- [ ] **Step 2: Replace V1 screenshots/commands and unsafe advice**

Remove instructions that call manager scripts, rebuild firewall manually, add `ppp-+` to positional zones, restore system DNS for clients or enable local WAN fallback. Document only `proxypoolctl`, LuCI and supported init operations.

- [ ] **Step 3: Run final verification from a clean checkout**

```bash
./scripts/test-host.sh
bash tests/integration/fail_closed_test.sh
git diff --check
# Fresh OpenWrt 23.05.3 SDK/ImageBuilder:
make package/proxypool/proxypool-core/compile V=s -j1
make package/proxypool/luci-app-proxypool/compile V=s -j1
make image PROFILE=glinet_gl-mt6000 ...
```

Confirm resulting image contains V2 binaries/include/hooks, contains none of the deleted runtime scripts, and its package manifest matches the tested candidate.

- [ ] **Step 4: Commit docs and tag only after acceptance sign-off**

```bash
git add README.md docs
git commit -m "docs: publish proxypool v2 operations guide"
git tag -a v2.0.0-rc1 -m "ProxyPool V2 release candidate"
```

Do not push/tag a final `v2.0.0` until the user approves the Round 4 report and release candidate checksums.

## Phase 6 Exit Gate

- [ ] Round 4 runs 12～24 hours with 40～50 nodes and no invariant failure or unbounded resource trend.
- [ ] WAN/xl2tpd/proxypoold/firewall/network failures recover automatically while clients remain fail-closed.
- [ ] Reboot、备份恢复、sysupgrade preserve every node/device/pending binding and policy ID.
- [ ] V1 recovery firmware/export is verified before V1 runtime files are removed.
- [ ] Clean SDK/ImageBuilder build and full automated suites pass after V1 deletion.
- [ ] Final documentation states all supported behaviors and limitations; release requires explicit user sign-off.
