# ProxyPool L2TP Recovery Closure Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans and superpowers:test-driven-development task by task.

**Goal:** Make L2TP cleanup, health recovery, WAN recovery and 40–60 node jobs bounded and automatic before producing another GL-MT6000 firmware.

**Architecture:** Keep the V2 single-writer daemon, shared netifd/xl2tpd, node-bound DNS and expiring nft authorization. Add one bounded scheduler health supervisor, separate operation-job outcomes from long-lived retry intent, and ship a pinned bounded netifd L2TP protocol script in the full firmware overlay.

**Tech stack:** Go 1.20, POSIX shell, OpenWrt 23.05.3 netifd/xl2tpd/PPP, nftables, ubus, GL-MT6000.

### Task 1: Establish the failing recovery tests

**Files:**
- Modify: `proxypool-core/src/proxypoold/internal/engine/scheduler_test.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/jobs_test.go`
- Create: `scripts/test-l2tp-netifd-bounded.sh`
- Modify: `scripts/test-host.sh`

- [ ] Add a scheduler test proving an online session with a missed hotplug is revoked and requeued after a failed periodic probe.
- [ ] Add a 60-node job test proving retryable first-attempt failures do not leave the original import job running.
- [ ] Add executable shell fixtures proving wedged `xl2tpd-control` and a persistent PPP interface cannot block teardown forever.
- [ ] Run focused tests on the compile server and record the expected RED failures.

### Task 2: Ship bounded OpenWrt L2TP lifecycle

**Files:**
- Create: `files/lib/netifd/proto/l2tp.sh`
- Modify: `scripts/test-image-files.sh`
- Modify: `scripts/test-release-contracts.sh`

- [ ] Implement exact-interface timeout, stale-LAC cleanup and bounded TERM/KILL teardown.
- [ ] Verify the script behavior using fake control, proc and sysfs boundaries rather than source greps.
- [ ] Verify the full-image staging preserves the executable override and package-only builds do not claim to contain it.

### Task 3: Add periodic composite health supervision

**Files:**
- Modify: `proxypool-core/src/proxypoold/internal/platform/contracts.go`
- Modify: `proxypool-core/src/proxypoold/internal/live/gates.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/scheduler.go`
- Modify: matching Go tests

- [ ] Add a narrow health-verifier contract for adapter, route and DNS checks.
- [ ] Drive it from one bounded scheduler ticker with per-protocol concurrency limits.
- [ ] On failure revoke authorization first, close remaining gates, clean the owned L2TP session and enqueue independent recovery.
- [ ] Cover stale generations, shutdown races, shared daemon loss and missed hotplug.

### Task 4: Bound operation jobs and preserve background recovery

**Files:**
- Modify: `proxypool-core/src/proxypoold/internal/engine/jobs.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/scheduler.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/controller.go`
- Modify: matching Go and Round 3 tests

- [ ] Record a retryable failed attempt as a terminal outcome for the originating user job without changing the node runtime from `backoff`.
- [ ] Create a fresh `system.recover` job for later retry and keep terminal jobs immutable.
- [ ] Restore timers/jobs after daemon restart without immediate retry storms.
- [ ] Prove 60 unreachable nodes finish the import job and continue bounded background recovery independently.

### Task 5: Add authoritative WAN supervision

**Files:**
- Create/modify: OpenWrt WAN status adapter and tests
- Modify: `proxypool-core/src/proxypoold/cmd/proxypoold/main.go`
- Modify: `proxypool-core/src/proxypoold/internal/engine/controller.go`
- Modify: netifd event helper tests if needed

- [ ] Treat unreadable/down WAN as unavailable and pause new attempts.
- [ ] Detect the down-to-up edge and enqueue bounded recovery for waiting nodes.
- [ ] Verify missed hotplug is repaired by polling and no WAN state expands client authorization.

### Task 6: Verification and firmware

- [ ] Run focused RED/GREEN cycles on the compile server for every task.
- [ ] Run `scripts/test-host.sh`, `go test -race -count=1 ./...`, `go vet ./...`, shell syntax checks and `git diff --check`.
- [ ] Build and inspect the SDK packages.
- [ ] Commit and push only after all package gates pass.
- [ ] Build the full firmware once, verify SHA256SUMS and download the unique GL-MT6000 sysupgrade image.
- [ ] Provide the exact local path and a staged 1/5/20/40–60 node hardware test checklist.
