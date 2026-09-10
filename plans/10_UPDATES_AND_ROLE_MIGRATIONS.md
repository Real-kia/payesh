# 10 — Updates, recovery and role/history migration

Status: not started. Planning baseline: 2026-09-08.
Risk/assignment guidance: Very hard; strongest available reviewer for data safety.
Master milestone: M6; design interfaces in 01.

Read [shared context](00_SHARED_CONTEXT.md) and [the package guide](README.md) before acting. This document is a future build assignment template, not permission to start coding in the documentation-only session.

## Prerequisites and ownership

**Requires:** 03 backup/history; 04 roles/trust/install jobs; 06–09 module/policy compatibility. Review design early, accept against actual builds.

**Owns:** Updater/independent watchdog, backups and role cutover; shared schema changes remain lead-coordinated.

Inspect actual repository conventions before selecting source paths. The lead owns shared contracts, schema ordering, dependency locks and integration unless explicitly delegated. A dependency may be replaced by an isolated test fixture while developing, but not for final integration acceptance.

## Work to implement

1. Reuse artifact trust and implement metadata anti-replay, core/protocol/module compatibility, UI cache matching and key rotation.
2. Implement notify/manual defaults and explicit scheduled-patch opt-in; hub first then one node at a time with offline/incompatible results visible.
3. Stage verified artifacts with explicit backup headroom and consistent snapshots; keep recovery journal outside candidate database.
4. Independent watchdog restores matching binaries/UI/modules/database/configuration after failed startup/migration/activation. Durable acknowledgement boundary must prevent rollback erasing acknowledged samples.
5. Implement every specified role transition with stable identity, deduplicated history/live tail, source preservation, interruption recovery and one-controller cutover.
6. Encrypt full-hub exports; exclude other-node secrets/CA keys from single-server exports. Unavailable old hub cannot yield history stored only there.
7. Serialize conflicting jobs, verify policies after recovery and report remaining prerequisite packages honestly. Coordinate UI progress/recovery through 02.

## Acceptance gates

- Upgrade actual older build; deliberately fail binary/startup/migration/activation and test no space, bad/stale metadata and incompatible modules.
- Verify matching old DB/binary after rollback, effective policies and sample survival/replay across acknowledgement boundary.
- Exercise all transition rows below, attached fleet, unavailable old hub, duplicate import and interruption.
- One authority after cutover; no source deletion before verified transfer; web/CLI-only switch preserves history.

## Out of scope

OS upgrades/reboots, full-machine backup guarantee, HA hubs, paid provisioning or publishing without authorization.

Reference extracts below sometimes mention neighboring packages. They explain integration requirements; they do not expand this worker's editable scope. Read-only inspection of neighboring interfaces is expected.

## Handoff and stop condition

Write `docs/handoffs/10.md` using the shared-context handoff checklist: baseline/files, implemented behavior, changed/consumed interfaces, actual test commands/results, measured or unmeasured costs, limitations, and next-package inputs. Request lead acceptance. Do not mark the full milestone/product complete or continue into another package on your own. If a prerequisite is missing, report exactly which contract, implementation or test environment is required.

## Detailed reference requirements

These are relevant verbatim extracts from [the master plan](../PLAN.md) at the planning baseline. Together with shared context, they are part of this assignment's requirements, not optional background. The lead synchronizes affected copies when the master changes.

## 14. Role transitions and history migration

Treat a role change as a preflighted job with backup, transfer, verification, cutover, and recoverable cleanup. Do not implement it as “delete the database and reinstall.”

| Transition | Required result |
| --- | --- |
| Standalone → hub | Existing server ID/history/settings remain; enable fleet management without resetting local collection. |
| Standalone → node | Enroll into the destination hub, import retained server history/policies, verify continuity, then stop local server/UI. |
| Node → standalone | Download core server/UI components, preserve identity/local policies, optionally retrieve retained history from the previous hub, then revoke the old hub's authority. |
| Hub → another hub | Transfer full fleet state/trust/configuration/history with protected export, re-point nodes in batches, and retire the old controller after verification. |
| Web standalone ↔ CLI-only | Install/remove UI assets and change listener exposure without deleting history or changing server identity. |

- A hub still managing nodes cannot become a node or be removed until its fleet is migrated or explicitly detached. Show affected servers.
- Export a consistent database snapshot using SQLite's backup facilities; copying only a live database file while ignoring WAL is not sufficient.
- Imports use stable identities and sample deduplication keys. Transfer historical ranges plus the live tail around cutover without double counting.
- Keep the source copy until verification succeeds. State the disk cost and allow cleanup only after the destination is confirmed.
- Full hub exports contain authority/credentials and must be encrypted. A single-server history export must not include other nodes' secrets or the hub CA private key.
- Moving a node to a different authority requires explicit trust replacement. The former hub must not retain control through an unnoticed second connection.
- If the old hub is unavailable, a node can become standalone using local state, but cannot recreate history that existed only on that hub. Make the missing range explicit.
- Interrupted transfers must be resumable or safely restartable. Avoid simultaneous active writers/controllers during final cutover.

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
