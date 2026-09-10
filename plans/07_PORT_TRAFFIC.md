# 07 — Per-port traffic accounting

Status: not started. Planning baseline: 2026-09-08.
Risk/assignment guidance: Hard; Linux networking review.
Master milestone: M5 module.

Read [shared context](00_SHARED_CONTEXT.md) and [the package guide](README.md) before acting. This document is a future build assignment template, not permission to start coding in the documentation-only session.

## Prerequisites and ownership

**Requires:** 06 lifecycle/ABI; 03 counter storage; 04 capabilities. May run alongside 08 in separate files.

**Owns:** Separate Port Traffic module, owned kernel counters/classifiers and tests.

Inspect actual repository conventions before selecting source paths. The lead owns shared contracts, schema ordering, dependency locks and integration unless explicitly delegated. A dependency may be replaced by an isolated test fixture while developing, but not for final integration acceptance.

## Work to implement

1. Define protocol/interface/local-port/direction scopes and original/translated tuple behavior. Remote destination ports are not locally hosted service ports.
2. Use counters/classification, not payload capture. Verify NAT, bridges/veth, forwarding, IPv6, tunnels and relevant offload paths with real traffic.
3. Count once per declared scope; overlapping totals are not automatically additive. Shared proxy ports do not identify individual applications.
4. Export stable counter/coverage/reset semantics. No reconstruction of pre-enablement usage.
5. Create/remove only owned rules; refuse unsupported ownership/network arrangements.
6. Provide 09 stable classifier/counter contracts instead of requiring it to guess private nftables layout; document UI fields for 02.

## Acceptance gates

- Known-volume TCP/UDP IPv4/IPv6 bidirectional tests document byte/header/offload scope and tolerance.
- NAT/container bridge/proxy/port reuse/reload/reboot/reset tests show no double-counting or false attribution.
- Lifecycle preserves foreign firewall state; measure userspace and kernel overhead under representative load.
- Show unsupported arrangements and gaps; do not claim exact provider billing.

## Out of scope

Payload capture, process attribution, enforcement or container management.

Reference extracts below sometimes mention neighboring packages. They explain integration requirements; they do not expand this worker's editable scope. Read-only inspection of neighboring interfaces is expected.

## Handoff and stop condition

Write `docs/handoffs/07.md` using the shared-context handoff checklist: baseline/files, implemented behavior, changed/consumed interfaces, actual test commands/results, measured or unmeasured costs, limitations, and next-package inputs. Request lead acceptance. Do not mark the full milestone/product complete or continue into another package on your own. If a prerequisite is missing, report exactly which contract, implementation or test environment is required.

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

### Port Traffic module

The initial scope is selected local TCP/UDP service ports and their corresponding inbound/outbound traffic. Remote destination port 443 used by an outbound client is not automatically the same thing as a service listening locally on 443.

Use kernel counters/classification, not payload capture. [nftables counters](https://wiki.nftables.org/wiki-nftables/index.php/Counters) provide a foundation; implementation must validate both directions and connection identity.

- Distinguish protocol, selected interface, local port, and direction in accounting keys.
- Follow original and translated tuples where NAT/forwarding is involved. Count a packet once per declared accounting scope, not at every hook/interface it crosses.
- Shared ports and reverse proxies cannot always distinguish applications. Show port-level aggregates; do not label those values as individual application usage without additional evidence.
- Handle port reuse, sockets closing, IPv6, forwarded container traffic, VPN/tunnel visibility, counter resets, and rule reloads.
- Create only Payesh-owned rules/counters; do not flush another application's firewall state.
- A newly enabled module cannot reconstruct traffic from before installation. Show its actual coverage start and gaps.

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
