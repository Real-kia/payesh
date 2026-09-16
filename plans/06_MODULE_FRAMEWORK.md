# 06 — Signed optional-module framework

Status: Checkpoint B implemented and safety-review fixes applied (see `docs/handoffs/06.md`);
typed `payesh-privd` wiring is now present, while a provisioned production signing key and
Linux/real-archive acceptance remain outstanding. Planning baseline: 2026-09-08.
Risk/assignment guidance: Hard; supply-chain/privilege review.
Master milestone: M5 foundation.

Read [shared context](00_SHARED_CONTEXT.md) and [the package guide](README.md) before acting. This document is a future build assignment template, not permission to start coding in the documentation-only session.

## Prerequisites and ownership

**Requires:** 01 manifests/helper/jobs; 04 verified installation and authenticated node jobs; coordinate 10 compatibility.

**Owns:** Curated catalog, dependencies, artifact staging, module states/jobs and module-host/helper boundary.

Inspect actual repository conventions before selecting source paths. The lead owns shared contracts, schema ordering, dependency locks and integration unless explicitly delegated. A dependency may be replaced by an isolated test fixture while developing, but not for final integration acceptance.

## Work to implement

1. Implement per-server eligibility and install/enable/disable/remove lifecycle with full dependency/disk/privilege previews.
2. Reuse trust verifier; nodes independently verify hub-relayed artifacts. Only catalog-approved signed modules, not request-provided executable URLs.
3. Validate archive paths/symlinks/sizes, stage atomically, install only named prerequisites and perform real bounded health checks.
4. Finalize module ABI and cleanup/recovery hooks for 07–09. Serialize conflicting module/control/update/role jobs and handle cancellation.
5. Keep install separate from activation. Failed cleanup retains recovery executables; disable tears down relevant kernel/background work.
6. Provide UI state/cost/job fixtures; resource estimates require measurements.

## Acceptance gates

- Base artifacts/dependency graphs contain no optional implementations or executables.
- Test tampered/stale/incompatible metadata, extraction escape, interruption, no disk, package lock/repository failure and cancellation.
- Test module proves lifecycle and failed-cleanup recovery; test artifacts cannot enter production catalog.
- Framework acceptance alone is not M5 completion; all official modules must pass integrated lifecycle tests.

## Out of scope

Arbitrary executable marketplace/scripts, shared OS-package autoremove or hidden preinstallation.

Reference extracts below sometimes mention neighboring packages. They explain integration requirements; they do not expand this worker's editable scope. Read-only inspection of neighboring interfaces is expected.

## Handoff and stop condition

Write `docs/handoffs/06.md` using the shared-context handoff checklist: baseline/files, implemented behavior, changed/consumed interfaces, actual test commands/results, measured or unmeasured costs, limitations, and next-package inputs. Request lead acceptance. Do not mark the full milestone/product complete or continue into another package on your own. If a prerequisite is missing, report exactly which contract, implementation or test environment is required.

## Detailed reference requirements

These are relevant verbatim extracts from [the master plan](../PLAN.md) at the planning baseline. Together with shared context, they are part of this assignment's requirements, not optional background. The lead synchronizes affected copies when the master changes.

## 11. Optional-module lifecycle and trust

Ship an explicit curated catalog of official Payesh modules. A general third-party executable marketplace is deferred.

Module metadata must describe ID, version, compatible core/protocol range, supported OS/architecture/capabilities, dependencies, compressed/unpacked size, signing information, required privileges, and install/verify/remove behavior. Resource-cost estimates must come from release benchmarks, not guessed numbers.

State model: unavailable, available, downloading, verifying, installing, installed-disabled, enabled, updating, removing, or failed. Preserve the previous working state on failed upgrades. A download completing does not mean installation succeeded.

Installation sequence:

1. Select one or several servers and show each server's eligibility.
2. Resolve dependencies and show the complete action/space requirements. Bandwidth Controls depends on the relevant traffic-accounting components; list that dependency instead of silently bundling every extra.
3. Fetch a signed manifest and matching artifact. The hub may relay/cache artifacts to nodes with limited outbound access; nodes independently verify them.
4. Verify signature, checksum, declared sizes, platform, and compatibility before executing anything. Reject unsafe archive paths, symlink escapes, oversized extraction, and unexpected executables.
5. Stage on the target filesystem, install only the named prerequisites through its supported package manager, then atomically activate the component.
6. Run a bounded health/capability check; show success only after the running component is confirmed.
7. Keep installation and activation separate. The owner then configures a feature or control policy.

Cancel safely between stages. Package-manager locks, missing repositories, interrupted connections, old cached manifests, and insufficient disk must be recoverable failures. Persist job results so a browser refresh does not lose progress.

Disabled modules stop their userspace work. Kernel rules also need explicit teardown where appropriate; “zero overhead” must not be claimed while counters/hooks remain active. On removal, keep policy history/audit records but stop the feature. Do not automatically remove system packages that may now be used by other software.

Never use a request-provided URL as permission to download and execute arbitrary code. The catalog and trust policy define eligible release artifacts.

## 15. Releases, upgrades, compatibility, and rollback

### Release artifacts and compatibility

- Use semantic versions for Payesh releases and a separately declared node protocol major version. Publish binaries, UI assets, modules, checksums, a signed manifest, release notes, and upgrade requirements through GitHub Releases.
- The core installer includes a pinned public release-signing trust anchor. Keep signing private keys out of the repository and ordinary pull-request CI. Document key rotation and revocation.
- Authenticate release metadata as well as archives; record accepted metadata versions and validity. Reject stale/replayed downgrade instructions. Deliberate restoration of the locally retained last-known-good release is a separately recorded recovery action, not a general downgrade bypass.
- UI assets must match their server release. Core/module bundles declare their supported combinations; default to installing the matching module release during a core update.
- Within protocol v1, retain compatibility with previously released v1 agents through capability negotiation. Never send a new action to a node that did not advertise support.
- A major protocol change needs a migration path before release; reject unsupported combinations before changing a live installation.
- Use hashed asset filenames and cache-aware deployment. An old browser tab should be told to reload when incompatible with the new API, not fail mysteriously.

### Default update behavior

- Check at most daily with jitter and a bounded request. Show the available version, release notes, relevant module updates, and machine eligibility.
- Default: notify only. “Update selected servers” is explicit owner action. Offer opt-in scheduled patch updates; feature/major upgrades remain explicit.
- Sequence fleet upgrades hub first, then one node at a time. Keep incompatible/offline nodes visible as skipped/pending; do not call the whole fleet successful when only some machines changed.
- Revalidate delayed jobs before execution. If their authorization/preconditions expired, require a new job rather than unexpectedly upgrading a machine weeks later.

### Per-machine upgrade transaction

1. Verify compatibility, free staging/backup space, pending jobs, module state, and writable storage.
2. Download and authenticate all required artifacts before stopping working services. A network failure leaves the old version running.
3. Create a consistent configuration/database backup. Keep an upgrade journal outside the candidate application database.
4. Start a small independent updater/watchdog with the old release path and recovery instructions. The new application must not be responsible for recovering from its own failure to start.
5. Activate the staged release atomically and run transactional/explicit migrations. Serialize upgrade against role changes and conflicting control/module jobs.
6. Check API health, database access, agent reconnection, installed-module health, and representative metric ingestion. Process existence alone is insufficient.
7. Commit success and retain one last-known-good release/backup. Resume normal acknowledgements and jobs after the transition is stable.
8. On failure, restore compatible old binaries/assets/modules and required database/configuration state, then restart and report the failed attempt.

Do not roll back a binary against an incompatible newer database schema. Do not acknowledge samples that cannot survive the chosen rollback boundary; replay buffered samples after recovery. The watchdog must also recover after interrupted activation/reboot.

Database migrations are versioned and tested. A destructive migration requires a restorable pre-migration backup and explicit disk headroom. Automatically restoring Payesh does not mean downgrading unrelated OS packages; report any prerequisite packages that remain installed.

Policies should survive ordinary Payesh restarts/upgrades. Validate their effective state after recovery. Limit updater backups/cache retention and expose their temporary disk requirements. Updating Payesh does not authorize `apt upgrade`, `dnf upgrade`, a distribution upgrade, or an OS reboot.
