# ProxyPool Cold-Image State Convergence Design

## Problem

ProxyPool currently preserves both user configuration and generated control
state across `sysupgrade`. Older releases can therefore restore a selector,
backend-ownership marker, or cleanup write-ahead log that was valid for an
earlier state machine but is not a valid tuple for the new activator. The
activator correctly fails closed, but it has no complete cold-image recovery
path, so the router can remain permanently unable to start ProxyPool.

The field failure from release `2.0.0-9` is the concrete regression case:

- the ROM selector is canonical V1 and the ROM contains only an `image`
  activation request;
- the effective overlay selector is a preserved pre-selector/invalid file;
- a preserved backend ownership marker is also present;
- the new boot has no ProxyPool process, socket, runtime directory, or procd
  instance;
- activation stops at `selector and persistent backend ownership are
  inconsistent`.

The prior repair only accepted an invalid selector when no ownership marker
existed. That was an incomplete state model.

## Goals

1. Make a cold firmware boot converge every provably safe legacy or V2 state
   to one canonical committed V2 state.
2. Keep contradictory, corrupt, live, or same-boot package states fail-closed.
3. Stop carrying generated transaction/control state into future firmware
   images.
4. Preserve all user-owned ProxyPool configuration and backups.
5. Produce diagnostics that identify the rejected state class without logging
   credentials or configuration contents.

## Non-goals

- Do not weaken live package-upgrade admission rules.
- Do not infer safety from missing PIDs alone.
- Do not delete or replace corrupt/symlinked state paths.
- Do not change the L2TP data plane, firewall policy, device isolation policy,
  or LuCI behavior in this change.
- Do not silently prefer V1 over a proven V2 ownership transaction.

## State Inputs

The activator bases its decision on these independent inputs:

- request: `image` or a boot-bound package request;
- selector classification: `missing`, `v1`, `v2_shadow`, or `unknown`;
- activated ownership: empty, `v1`, or `v2_shadow`;
- cleanup WAL: empty, `v1`, or `v2_shadow`;
- legacy configuration classification;
- V2 configuration classification;
- boot-local process, socket, runtime-directory, transition, and snapshot
  evidence;
- procd service state.

Unsafe marker paths, malformed marker values, malformed requests, or unknown
procd state remain terminal failures before any mutation.

## Cold-Image Safety Boundary

Compatibility convergence is available only when all of the following hold:

1. the request is exactly `image`;
2. every boot-local ProxyPool evidence path is absent;
3. procd proves the service is absent;
4. persistent marker paths and values are structurally safe;
5. the selected lineage has a configuration that the corresponding strict
   classifier accepts.

This boundary proves that an earlier firmware's userspace and kernel-only
runtime crossed a reboot. It does not claim that arbitrary current-boot
package state is safe.

### Boot-bound package requests

The existing package-upgrade path remains available after its requested boot
ID differs from the current boot ID. It uses the same boot-local/procd absence
proof and the same lineage validation, but it accepts only canonical
`missing`/`v1` selector states already admitted by the release-9 transaction
order. An `unknown` selector is never repaired for a package request. A
same-boot request continues to return `reboot_required` without mutation.

## Convergence Matrix

### V1 lineage: migrate once

Treat the state as stopped V1 and run the existing migration transaction when:

- selector is `missing`, `v1`, or `unknown`;
- ownership/WAL is one of:
  - empty / empty;
  - `v1` / empty;
  - `v1` / `v1`;
- the legacy configuration strictly classifies as V1.

The `unknown` selector variant is restricted to an `image` request. Canonical
`missing` and `v1` variants may also use an eligible different-boot package
request.

After migration succeeds and the V2 result strictly classifies, retire a V1
cleanup WAL if present, atomically publish V2 ownership, atomically publish the
canonical V2 selector, and finally retire the image request. No durable input
is changed before migration and V2 validation succeed.

### V2 lineage: repair without migration

Treat the state as an interrupted or stopped V2 transaction when:

- ownership is `v2_shadow`;
- selector is `missing`, `v1`, `v2_shadow`, or `unknown`;
- cleanup WAL is empty or `v2_shadow`;
- the V2 configuration strictly classifies as V2.

The `unknown` selector variant is restricted to an `image` request. Canonical
selector variants may repair an eligible interrupted package transaction.

Do not run V1 migration in this lineage. Retire a same-backend V2 cleanup WAL
if present, atomically publish/repair the canonical V2 selector, preserve V2
ownership, and finally retire the image request.

The already committed tuple `v2_shadow / v2_shadow / empty` remains an
idempotent no-op apart from safely retiring a valid queued request.

### Terminal contradictions

Continue to fail closed for all other tuples, including:

- selector V2 with V1 or empty ownership;
- V1 ownership with a V2 cleanup WAL;
- V2 ownership with a V1 cleanup WAL;
- cleanup WAL without matching ownership;
- a required lineage configuration that fails strict classification;
- any boot-local or procd evidence;
- invalid selector recovery requested by a live/same-boot package request;
- unsafe paths, malformed markers, or malformed requests.

## Sysupgrade Ownership Policy

The package keep list will preserve only durable user data:

- `/etc/config/proxypool_v2`;
- `/etc/proxypool/migration-v1.json`;
- `/etc/proxypool/backups/`.

It will no longer preserve generated control and transaction state:

- `/etc/config/proxypool_runtime`;
- `/etc/proxypool/activated-backend`;
- `/etc/proxypool/cleanup-required`;
- `/etc/proxypool/v2-activation-request`;
- `/etc/proxypool/firewall-transaction`;
- `/etc/proxypool/wireless-quarantine`.

`/etc/config/proxypool` remains the package conffile and is preserved by the
normal OpenWrt conffile mechanism. A new firmware supplies a canonical ROM
selector and a fresh ROM `image` request. The convergence matrix remains
necessary for the first upgrade from older releases because their old keep
list creates the backup before the new firmware is installed.

Removing firewall and wireless transaction files from the sysupgrade keep
list does not relax runtime isolation: the new image must re-establish the
custom-kernel, fw4, and guardian safety proofs before ProxyPool starts.

## Diagnostics

On a rejected tuple, the activator will log only normalized classifications:
selector class, ownership class, cleanup class, request class, and the failed
safety gate. It must never log node endpoints, usernames, passwords, raw UCI
content, or migration payloads.

## Test Design

The activation integration test will become a table-driven matrix covering:

- the exact field regression: unknown selector + V1 owner + image request;
- V1 owner with matching cleanup WAL;
- V2 owner with unknown/V1/missing selector, with and without matching WAL;
- completed V2 idempotence;
- every cross-backend contradiction;
- cleanup-without-owner states;
- invalid lineage configurations;
- package-request rejection for compatibility-only recovery;
- each boot-local evidence path and present/unknown procd state;
- failure ordering proving no selector, owner, WAL, or request mutation occurs
  before successful validation;
- exactly one migration for V1 lineage and zero migrations for V2 repair.

Image staging, IPK inspection, release-contract, and package-safety tests will
assert the reduced keep list and the presence of the ROM selector/request.
The full host suite and integrated fail-closed suite must pass before a package
or firmware build starts.

## Release and Hardware Verification

- bump `proxypool-core` from release 9 to release 10;
- build and inspect the aarch64 Cortex-A53 core IPK and matching LuCI IPK;
- reuse the pinned OpenWrt 23.05.3 full-source build and custom MT7531 kernel
  verification;
- inspect the final rootfs for release 10, the exact activator hash, canonical
  ROM selector, fresh image request, and absence of ROM ownership/WAL markers;
- verify GL-MT6000 firmware metadata and SHA256;
- download the unique sysupgrade image and evidence bundle locally.

Hardware boot remains the final environment check, but the firmware will not
be handed off until every state-machine, package, rootfs, and kernel gate above
has produced fresh passing evidence.
