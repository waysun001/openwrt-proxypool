# Cold-Image State Convergence Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every provably safe preserved V1 or V2 cold-image state converge to canonical V2 while rejecting contradictory/live states and preventing future sysupgrades from preserving generated control state.

**Architecture:** Keep the existing shell activator and strict classifiers, but split its cold decision into explicit V1-migrate and V2-repair lineages. Gate compatibility-only recovery on an exact `image` request plus absent boot-local/procd evidence, and reduce the sysupgrade keep list to durable user data.

**Tech Stack:** POSIX/OpenWrt shell, UCI classifiers through `proxypoolctl`, OpenWrt IPK/sysupgrade packaging, existing shell integration harness.

## Global Constraints

- Device target remains GL.iNet GL-MT6000 on OpenWrt 23.05.3.
- Client traffic remains fail-closed until firewall and LAN-isolation safety activation is current.
- Package same-boot activation remains forbidden.
- No raw UCI data, endpoints, usernames, or passwords may be logged.
- Full test suite runs once after targeted tests pass; full firmware builds run only after all host/package gates pass.

---

### Task 1: Add the cold-state decision matrix

**Files:**
- Modify: `scripts/test-backend-activation-integration.sh`

**Interfaces:**
- Consumes: `run_activate`, fixture paths under `CASE_ROOT`, strict fake `proxypoolctl` classifications.
- Produces: field-regression and matrix assertions for V1 migration, V2 repair, and terminal contradictions.

- [ ] **Step 1: Add the exact field regression before changing production code**

  Add a case with an unknown/pre-selector file, `image` request, V1 ownership,
  valid legacy V1 config, absent procd, and no boot-local evidence. Assert that
  activation succeeds, migration runs exactly once, selector/owner become V2,
  and the request is retired.

- [ ] **Step 2: Add safe V1 and V2 lineage cases**

  Cover V1 owner with/without V1 WAL; V2 owner with missing, V1, unknown, and
  V2 selector; and V2 owner with/without V2 WAL. Assert one migration for V1
  lineage and zero migration for V2 repair.

- [ ] **Step 3: Add terminal contradiction cases**

  Cover V1/V2 cross-WALs, cleanup without owner, V2 selector without V2 owner,
  invalid lineage config, package request with unknown selector, present and
  unknown procd, and every boot-local evidence path. Assert no durable file is
  changed on failure.

- [ ] **Step 4: Run RED**

  Run: `sh scripts/test-backend-activation-integration.sh`

  Expected: FAIL at the field case because release 9 rejects
  `unknown:v1:` as inconsistent.

### Task 2: Implement explicit V1-migrate and V2-repair convergence

**Files:**
- Modify: `proxypool-core/files/proxypool-backend-activate`

**Interfaces:**
- Consumes: normalized selector/owner/WAL/request classes and existing strict
  `classify`, `select-backend`, `procd-state`, migration, and atomic-write helpers.
- Produces: internal `lineage` value (`v1_migrate` or `v2_repair`) and canonical
  V2 persistent state.

- [ ] **Step 1: Validate request and cold boundary before compatibility decisions**

  Preserve completed-V2 idempotence. For all other states, validate the
  request/boot ID, prove every boot-local path absent, and prove procd absent
  before any selector, owner, WAL, or request mutation.

- [ ] **Step 2: Select the lineage from an explicit case table**

  Image-only V1 lineage accepts selector `missing|v1|unknown` with owner/WAL
  `empty/empty`, `v1/empty`, or `v1/v1`, then requires strict legacy V1.
  Image-only V2 repair accepts V2 ownership, selector
  `missing|v1|v2_shadow|unknown`, WAL `empty|v2_shadow`, then requires strict V2.
  Different-boot package requests keep canonical release-9 states but never
  accept an unknown selector. All other tuples fail with normalized classes.

  The production decision must be equivalent to:

  ```sh
  case "$request_class:$selector:$activated:$cleanup_required" in
    image:missing::|image:v1::|image:unknown::|\
    image:missing:v1:|image:v1:v1:|image:unknown:v1:|\
    image:missing:v1:v1|image:v1:v1:v1|image:unknown:v1:v1)
      lineage=v1_migrate ;;
    image:missing:v2_shadow:|image:v1:v2_shadow:|\
    image:v2_shadow:v2_shadow:|image:unknown:v2_shadow:|\
    image:missing:v2_shadow:v2_shadow|image:v1:v2_shadow:v2_shadow|\
    image:v2_shadow:v2_shadow:v2_shadow|image:unknown:v2_shadow:v2_shadow)
      lineage=v2_repair ;;
    *) fail_with_normalized_state ;;
  esac
  ```

  The existing canonical different-boot package cases are expressed in a
  separate table so the package path cannot inherit `unknown` recovery.

- [ ] **Step 3: Execute only the chosen transaction**

  `v1_migrate` creates/validates V2 through the existing migration and then
  publishes owner/selector. `v2_repair` never calls migration; it validates V2,
  clears a matching V2 WAL after the cold proof, repairs the selector, and
  retires the request. Both publish the request retirement last.

- [ ] **Step 4: Run GREEN**

  Run: `sh scripts/test-backend-activation-integration.sh`

  Expected: `backend cold activation integration: PASS` with the field case,
  complete positive matrix, and negative matrix passing.

### Task 3: Stop preserving generated control state across sysupgrade

**Files:**
- Modify: `proxypool-core/files/proxypool.keep`
- Modify: `scripts/inspect-ipk.sh`
- Modify: `scripts/test-inspect-ipk.sh`
- Modify: `scripts/test-release-contracts.sh`
- Modify: `scripts/test-image-files.sh`

**Interfaces:**
- Consumes: OpenWrt keep.d semantics and ROM `files/` selector/image request.
- Produces: keep list containing only `proxypool_v2`, migration record, and backups.

- [ ] **Step 1: Change fixture expectations first**

  Set the exact expected keep list to:

  ```text
  /etc/config/proxypool_v2
  /etc/proxypool/migration-v1.json
  /etc/proxypool/backups/
  ```

  Add an image-upgrade fixture proving generated selector, owner, WAL, request,
  firewall transaction, and wireless quarantine are not selected by the new
  keep policy while V2 user config and backups remain selected.

- [ ] **Step 2: Run keep-list tests RED**

  Run: `sh scripts/test-inspect-ipk.sh && sh scripts/test-release-contracts.sh && sh scripts/test-image-files.sh`

  Expected: FAIL because production keep.d still contains transient state.

- [ ] **Step 3: Reduce the production keep list**

  Replace `proxypool.keep` with the exact three durable entries above.

- [ ] **Step 4: Run keep-list tests GREEN**

  Run the same command and expect all three scripts to report PASS.

### Task 4: Release gate and regression verification

**Files:**
- Modify: `proxypool-core/Makefile`
- Modify: `scripts/test-release-contracts.sh`

**Interfaces:**
- Produces: `proxypool-core 2.0.0-10`.

- [ ] **Step 1: Bump release expectations and package release to 10**

  Change `proxypool-core/Makefile` to `PKG_RELEASE:=10` and change the exact
  assertion in `scripts/test-release-contracts.sh` to the same value.

- [ ] **Step 2: Run the focused gate**

  Run:

  ```sh
  sh scripts/test-backend-activation-integration.sh
  sh scripts/test-image-files.sh
  sh scripts/test-inspect-ipk.sh
  sh scripts/test-package-safety-integration.sh
  sh scripts/test-release-contracts.sh
  ```

- [ ] **Step 3: Run the full gate once**

  Run: `./scripts/test-host.sh && sh tests/integration/fail_closed_test.sh`

  Expected: exit 0; the optional host nft syntax probe may report its existing
  SKIP, while all contractual gates pass.

- [ ] **Step 4: Review diff, commit, and push**

  Commit implementation as `fix: converge preserved cold backend state` and
  push `codex/proxypool-v2-phase1`.

### Task 5: Build and verify one firmware candidate

**Files:**
- No repository source edits.
- Create build evidence under `firmware-evidence/<short-commit>/`.

**Interfaces:**
- Consumes: exact pushed commit, cached M1068 SDK/full-source trees.
- Produces: inspected release-10 IPKs and one GL-MT6000 sysupgrade image.

- [ ] **Step 1: Build and inspect the core IPK on M1068**

  Clean only the ProxyPool core package, compile with the existing SDK cache,
  and run `scripts/inspect-ipk.sh` plus metadata/version checks.

- [ ] **Step 2: Perform one incremental full-source build**

  Update only the ProxyPool feed source, clean only ProxyPool core, and run the
  cached full build once.

- [ ] **Step 3: Verify the final rootfs and kernel**

  Require release 10, exact activator hash, canonical ROM V1 selector, exact
  ROM `image` request, no ROM ownership/WAL markers, LuCI payload pass, and the
  MT7531 custom-kernel verifier pass.

- [ ] **Step 4: Verify and download the unique firmware**

  Require one `*glinet_gl-mt6000*squashfs-sysupgrade.bin`, extract fwtool
  metadata for `glinet,gl-mt6000`, generate/verify SHA256SUMS on the server,
  download locally, and recompute every checksum before handoff.
