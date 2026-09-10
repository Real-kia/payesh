# 01 — Foundation and shared contracts

Status: ready for review (M0 scaffold; not accepted). Planning baseline: 2026-09-08.
Risk/assignment guidance: Hard decisions; bounded scaffolding afterward.
Master milestone: M0.

Read [shared context](00_SHARED_CONTEXT.md) and [the package guide](README.md) before acting. The M0 scaffold and contract fixtures are present; security/schema review and lead acceptance are still required.

## Prerequisites and ownership

**Requires:** None. Start here.

**Owns:** Lead: scaffolding, api/, shared types, dependencies/lockfiles, CI and decisions.

Inspect actual repository conventions before selecting source paths. The lead owns shared contracts, schema ordering, dependency locks and integration unless explicitly delegated. A dependency may be replaced by an isolated test fixture while developing, but not for final integration acceptance.

## Work to implement

1. Inspect the actual repository root, remote and changes. This notes directory is inside an enclosing Git repository; do not stage unrelated parent files or initialize a conflicting repository. Confirm the intended Payesh repository before scaffolding.
2. Pin current supported Go/frontend/dependency versions, exact Linux/architecture CI matrix, MIT license default and separate role artifacts. Keep SQLite/UI out of the agent build graph and optional implementations out of core.
3. Turn the plan's resource families into approved OpenAPI operations/schemas, node envelopes, typed helper/module requests, release trust format and database model. Workers must not invent independent endpoints.
4. Specify units, errors, capabilities, idempotency/revisions, cancellation, durable jobs and per-machine serialization across module/control/update/role actions. Include fixtures for large counters, gaps, expired jobs and partial-fleet results.
5. Define shared authenticated artifact verification early; installers need real signed verification before acceptance. Define backup/migration/acknowledgement boundaries before ingestion work.
6. Proposed contract files: api/openapi.yaml plus docs/contracts/NODE_PROTOCOL.md, HELPER_PROTOCOL.md, MODULE_PROTOCOL.md, RELEASE_FORMAT.md and DATA_MODEL.md. Adapt names once after inspecting repository conventions.
7. Create docs/IMPLEMENTATION_STATUS.md and decisions/file ownership when implementation starts. Supply buildable boundaries without implementing all features.

## Acceptance gates

- Both architectures build; lint/type/contract validation runs; agent graph excludes server storage/frontend.
- Record approved contract revision, schema/migration owner and worker boundaries. Unimplemented APIs are explicitly unimplemented, not successful dummy responses.
- Security-sensitive contracts receive focused review before consumer implementation.

## Out of scope

Full collectors, dashboard, installers, enforcement and updater implementation.

Reference extracts below sometimes mention neighboring packages. They explain integration requirements; they do not expand this worker's editable scope. Read-only inspection of neighboring interfaces is expected.

## Handoff and stop condition

Write `docs/handoffs/01.md` using the shared-context handoff checklist: baseline/files, implemented behavior, changed/consumed interfaces, actual test commands/results, measured or unmeasured costs, limitations, and next-package inputs. Request lead acceptance. Do not mark the full milestone/product complete or continue into another package on your own. If a prerequisite is missing, report exactly which contract, implementation or test environment is required.

## Detailed reference requirements

These are relevant verbatim extracts from [the master plan](../PLAN.md) at the planning baseline. Together with shared context, they are part of this assignment's requirements, not optional background. The lead synchronizes affected copies when the master changes.

## 4. Platforms and capability detection

Default initial support matrix:

- x86-64 and ARM64.
- Ubuntu 22.04 LTS and newer LTS releases; Debian 12 and newer stable releases.
- Fedora's supported stable releases; Rocky Linux and AlmaLinux 9/10 families.
- Alpine 3.22 and newer supported stable releases, with OpenRC.
- Establish an exact version-pinned CI matrix at implementation start from vendor-supported releases. Include the oldest supported baseline and current stable representative for each family; record what was actually tested. Do not claim all past/future versions work.
- RHEL compatibility may share the RPM-family installer, but do not advertise RHEL certification without direct testing. Arch and other distributions are best-effort binary users initially, not silently added release gates.

Detect distribution, architecture, init system, package manager, kernel, cgroups, network-control facilities, virtualization restrictions, disk space, and existing ports before installing components. Support the vendor kernels shipped by the test matrix using feature probes, not a blanket kernel-number cutoff: [Rocky Linux's version guide](https://wiki.rockylinux.org/rocky/version/) includes 5.14-based kernels in the 9 family. Resource controls require the appropriate kernel features and permissions; CPU controls use cgroup v2.

On restricted containers/VPSs without those permissions, monitoring should still work where possible. The panel must show “unavailable on this server” with the specific reason. Do not install useless dependencies, force a reboot, change the host's cgroup mode, or weaken SELinux to pretend the feature works.

Provide systemd units and OpenRC service definitions. Read existing journals on systemd systems and explicitly discovered/configured log files on other systems. Absence of journald must not crash the agent.
