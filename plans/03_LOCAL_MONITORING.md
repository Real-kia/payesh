# 03 — Local metrics, storage, logs and CLI

Status: Checkpoint A implemented in the working tree; Linux/distribution acceptance and
the package-04 transport handoff remain pending. Planning baseline: 2026-09-08.
Risk/assignment guidance: Medium–hard; arithmetic/retention review.
Master milestone: M2.

Read [shared context](00_SHARED_CONTEXT.md) and [the package guide](README.md) before acting. This document is a future build assignment template, not permission to start coding in the documentation-only session.

## Prerequisites and ownership

**Requires:** 01 approved contracts; 02 can proceed independently.

**Owns:** Collectors, history/rollups/retention, log adapters, local API and assigned CLI operations.

Inspect actual repository conventions before selecting source paths. The lead owns shared contracts, schema ordering, dependency locks and integration unless explicitly delegated. A dependency may be replaced by an isolated test fixture while developing, but not for final integration acceptance.

## Work to implement

1. Implement real efficient Linux collectors with agreed identities, timestamp/rate validity and bounded host-level series. Process discovery stays on demand.
2. Implement local API/CLI using shared backend operations and access controls; no public temporary diagnostic bypass.
3. Implement SQLite durable ingestion/deduplication, queries, rollups, configurable retention and protected-state disk-full behavior. Supply monthly-total storage; 05 owns calendar calculations.
4. Implement journal/file queries/tails/cursors, bounded snapshot persistence and rotating Payesh operational logs. 05 owns alert-triggered snapshot orchestration.
5. Provide explicit ingestion/query contracts to 04, counter/coverage storage to 05/07 and consistent backup hooks to 10.
6. Never manufacture minute values from hourly samples, negative usage after reset, or exact historical timing inside uncertain counter gaps.

## Acceptance gates

- Real Linux data reaches local API/CLI; unavailable values are not zero.
- Test agent restart within one boot, reboot, CPU hotplug, large/reset counters, interface rename, clock change, sparse samples and rollup edges.
- Test log rotation/truncation, invalid UTF-8, huge/multiline entries, path/symlink attacks, hostile HTML, permission denial and cancellation with bounded cost.
- Disk/WAL/log/snapshot pressure preserves protected state, reports shortened retention/gaps and keeps live functions responsive where possible. Record measured overhead.

## Out of scope

Fleet installers, notification engine, port instrumentation, limits or continuous central logs.

Reference extracts below sometimes mention neighboring packages. They explain integration requirements; they do not expand this worker's editable scope. Read-only inspection of neighboring interfaces is expected.

## Handoff and stop condition

Write `docs/handoffs/03.md` using the shared-context handoff checklist: baseline/files, implemented behavior, changed/consumed interfaces, actual test commands/results, measured or unmeasured costs, limitations, and next-package inputs. Request lead acceptance. Do not mark the full milestone/product complete or continue into another package on your own. If a prerequisite is missing, report exactly which contract, implementation or test environment is required.

## Detailed reference requirements

These are relevant verbatim extracts from [the master plan](../PLAN.md) at the planning baseline. Together with shared context, they are part of this assignment's requirements, not optional background. The lead synchronizes affected copies when the master changes.

### Core measurements

Collect CPU utilization and available steal/iowait signals, memory available/used, swap, load, uptime, disk capacity/inodes, disk I/O, and per-interface byte/error/drop counters. Distinguish unavailable readings from zero. Read kernel-provided data efficiently; do not launch many external commands every sample.

- Start with host-level bounded series. Per-process discovery is on demand; do not store every PID as a permanent metric dimension.
- Exclude loopback and virtual interfaces from the suggested billing total, but show discovered interfaces and let owners select the authoritative external interface(s).
- Keep host interface totals as the billing-estimate baseline. Never sum all bridge, veth, tunnel, and physical interfaces together by default.
- Preserve bytes as integer counts. Clearly distinguish bytes, bits/second, decimal GB/TB, and binary GiB/TiB. Default traffic allowances to decimal GB/TB; storage displays use MiB/GiB.
- On counter decrease/reset, start a new baseline and mark uncertainty. Do not create negative rates or huge rollover spikes.
- Network gaps do not automatically mean zero usage. A cumulative counter can recover a delta if continuity is known; do not claim exact historical timing inside the gap.

### Collection and retention defaults

| Setting | Default | Required behavior |
| --- | --- | --- |
| Standard sample interval | 15 seconds | Adjustable 5 seconds–1 hour. |
| Economy interval | 60 seconds | Same measurements, less frequent collection. |
| Full-resolution history | 24 hours | Only retain samples actually collected. |
| Minute summaries | Up to age 7 days | Aggregate available samples; do not manufacture minute samples from hourly collection. |
| Hour summaries | Up to age 90 days | Preserve useful summary statistics. |
| Monthly traffic totals | 13 months | Independent from graph downsampling. |
| Payesh events/snapshots | 7 days, 100 MiB | Earliest limit wins; configurable. |
| Total managed data budget | Standalone 512 MiB; hub 2 GiB | Includes live database, WAL, indexes, snapshots, and managed operational logs. |
| Node offline spool | 32 MiB | Stop growth at the cap and report dropped coverage. |

Retention is a maximum age, not a promise that every configured age fits the disk budget. The UI shows effective oldest available data and any shortened retention. Budget settings must not silently delete configuration, active policies, identity keys, or current billing-period totals.

Implement time-weighted gauge summaries with sample counts/coverage, min/max, and average. Sum valid counter deltas for traffic and I/O; do not average byte totals. Align buckets using UTC timestamps. Display gaps; a zero value and a missing sample must look different.

Use batched SQLite transactions, bounded query results, and appropriate time/server indexes. Avoid an immediate fsync per field. A scheduled low-priority worker downsamples and removes expired data in small batches. Cap WAL growth and reclaim reusable/free space without a full blocking VACUUM on every cycle.

Start eviction before the hard storage ceiling. Remove expired/old detailed data first, then oldest snapshots and eligible history. Reserve operational headroom. If protected data plus database overhead cannot fit, suspend new history writes, keep live monitoring/control responsive where possible, and raise a storage error. Do not spin endlessly on “disk full.”

Upgrade staging and backups require separately reported temporary headroom; never silently consume an unbounded second copy under the ordinary data budget.

### Logs are events; metrics are samples

The sample interval controls numeric measurements. Application log events must not be arbitrarily sampled once an hour and presented as a complete log. Existing log retention remains the application's/journal's responsibility.

- Core log access reads existing sources only on demand, with source/time/severity filters, text search, live tail, and readable multiline entries.
- Default request bounds: 200 entries/page and 1 MiB per response. Bound line size and total scanned bytes/time. Truncate with an explicit notice and cancellation, not an unbounded process.
- Use cursors appropriate to journal entries or file identity/offset. Handle rotation, truncation, missing files, permissions, partial lines, invalid UTF-8, and a source disappearing during a query.
- Never interpret log content as HTML, shell commands, or instructions. Redact configured sensitive patterns and obvious credential fields from stored snapshots and diagnostic exports; do not claim redaction is perfect.
- Do not allow arbitrary root-readable paths from an API request. Use discovered or explicitly configured, validated sources; prevent traversal and unsafe symlink changes.
- No continuous central application-log archive in v1. Existing log history is available only while the node/source can be reached and retains it. Make that distinction visible.
- An incident snapshot stores up to 200 relevant entries, at most 256 KiB of log text, plus bounded nearby metric context. It is not a complete forensic archive.
- Label on-demand process information with its collection time. A process seen after an incident is not proof that it consumed resources during the earlier spike.
- Payesh's own structured operational logs rotate locally: default five files of at most 5 MiB each, within the managed storage budget. Routine samples should not produce verbose log lines.

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
