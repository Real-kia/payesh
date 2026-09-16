# 09 — Bandwidth caps and quota enforcement

Status: implementation complete in the working tree. The optional module
binary, authenticated Unix-socket API integration, tc executor, trusted
management discovery, durable policy/audit state, 15-second quota worker, and
independent rollback watchdog are present. An isolated Ubuntu 20.04 kernel
test now covers tc apply/verify/revert, foreign-root refusal, management-failure
rollback, and expired watchdog recovery. Measured throughput/overshoot,
crash/reboot, full service lifecycle, and overhead evidence remains outstanding.
Planning baseline: 2026-09-08.
Risk/assignment guidance: Very hard; strongest available reviewer and real Linux tests.
Master milestone: M5 module.

Read [shared context](00_SHARED_CONTEXT.md) and [the package guide](README.md) before acting. This document is a future build assignment template, not permission to start coding in the documentation-only session.

## Prerequisites and ownership

**Requires:** 06 lifecycle; 07 classifiers/counters; 05 period/usage semantics; 04 management identity/jobs.

**Owns:** Separate Bandwidth Controls module, local policy/checkpoint state and independent connectivity rollback.

Inspect actual repository conventions before selecting source paths. The lead owns shared contracts, schema ordering, dependency locks and integration unless explicitly delegated. A dependency may be replaced by an isolated test fixture while developing, but not for final integration acceptance.

## Work to implement

1. Implement distinct incoming/outgoing kernel paths for selected interface workload traffic and local TCP/UDP ports.
2. Offer explicit warn/throttle/block choices with scope, readable units, exclusions and revert. Installation or alert crossing never silently blocks.
3. Reuse shared period/usage identity; do not invent a divergent quota ledger. Local enforcement/checkpoints remain independent of sampling and hub connectivity.
4. Discover SSH/management flows including custom ports. Refuse when proxies/shared ports prevent safe separation; exemptions mean a workload cap, not every packet.
5. Detect qdisc/filter/firewall ownership and refuse incompatible trees instead of overwriting them.
6. Arm independent two-minute local rollback before connectivity-affecting apply. Commit only after local verification and management round trip; recover without hub/dashboard.
7. Serialize jobs, preserve recovery components and revert only owned changes, including when another administrator changes network state afterward.

## Acceptance gates

- Real workloads validate incoming/outgoing speed, per-port targeting and quota actions with documented overshoot/accuracy.
- Test custom/shared management ports, tiny speed, foreign qdisc, hub loss, stale jobs, reboot/crash, quota resets and accounting gaps.
- Deliberately break management confirmation and prove local watchdog recovery.
- Failed cleanup retains recovery; foreign state survives; measure kernel/whole-system overhead under load.

## Out of scope

Per-process bandwidth, whole-firewall replacement, silent blocking, billing-grade crash guarantees or production tests.

Reference extracts below sometimes mention neighboring packages. They explain integration requirements; they do not expand this worker's editable scope. Read-only inspection of neighboring interfaces is expected.

## Handoff and stop condition

Write `docs/handoffs/09.md` using the shared-context handoff checklist: baseline/files, implemented behavior, changed/consumed interfaces, actual test commands/results, measured or unmeasured costs, limitations, and next-package inputs. Request lead acceptance. Do not mark the full milestone/product complete or continue into another package on your own. If a prerequisite is missing, report exactly which contract, implementation or test environment is required.

## Detailed reference requirements

These are relevant verbatim extracts from [the master plan](../PLAN.md) at the planning baseline. Together with shared context, they are part of this assignment's requirements, not optional background. The lead synchronizes affected copies when the master changes.

## 10. Resource controls: behavior and failure cases

The distinction between observation and enforcement must remain obvious. A notification threshold does not automatically authorize throttling or blocking. Installing a module does not enable any limit.

Each control follows: select target → show existing state and expected effect → validate compatibility/management exclusions → apply → verify effective state → record an audit event. A failed verification is not “success.” Every policy has a visible disable/revert action.

### Bandwidth Controls

Use Linux [traffic control](https://man7.org/linux/man-pages/man8/tc.8.html) with nftables classification where needed. Distinguish outgoing shaping from incoming policing/redirection. Input and output are not mechanically identical operations.

- Support workload traffic on selected external interfaces and selected local TCP/UDP ports. Per-process bandwidth attribution/control is deferred; CPU process support does not imply it exists.
- Let users choose speed limits independently for incoming and outgoing traffic. Store integer bits/second; display human-friendly Mbps.
- Quota actions are explicitly selected: warn only, throttle to a specified speed, or block selected workload traffic after the allowance. No default blocking.
- Enforce installed policies locally on the node, so a hub outage does not remove them. Kernel counters/quotas should drive critical byte-limit behavior rather than waiting for the owner's potentially hourly graph sample interval.
- Persist period identity and usage checkpoints. After an unexpected crash, restore the last known period/usage, flag any accounting gap, and do not silently reset a full monthly allowance. Exact billing-grade enforcement across lost counters is not promised.
- Define and test a separate local enforcement/checkpoint cadence, default 15 seconds, where userspace evaluation is necessary. The UI documents possible threshold overshoot during that interval. Graph frequency is not this cadence.
- Reserve/exempt identifiable SSH and Payesh management traffic. The resulting cap is a workload cap, not an assertion that every packet on the machine is constrained. Show exclusions in the policy preview.
- If shared ports/proxies prevent management traffic from being safely distinguished, reject the conflicting policy with a clear reason. Do not blindly assume SSH is always port 22.
- Detect existing qdisc/filter/firewall owners. Integrate only where ownership is understood; otherwise refuse with a diagnostic. Never flush nftables or replace a third-party qdisc tree indiscriminately.
- For a connectivity-affecting change, arm a local two-minute rollback watchdog before applying. Cancel it only after local verification and a successful management round trip. It must work when the dashboard or hub is unavailable.
- Track only Payesh's changes for rollback. Do not restore an old full firewall snapshot over someone else's subsequent changes.

Disabling/removing a control module must first revert its active policies and verify cleanup. If cleanup fails, keep the component available and display recovery instructions; do not remove the executable needed to recover.

### Traffic allowances

- Per server: selected interface(s), outbound-only or combined traffic, allowance in bytes, billing reset day, billing timezone, and warning percentages. Default warnings: 80%, 90%, and 100%.
- Use UTC unless the owner selects a billing timezone. If a reset day is 29–31 and absent that month, use that month's last day. Compute successive periods explicitly; do not use “30 days” as a month.
- Store the period identity with usage. An ordinary reboot or software update must never reset the allowance.
- Changing allowance size affects the active period immediately; changing billing schedule takes effect at the next existing boundary unless the owner explicitly starts a new period. Preview this behavior.
- Forecast only after sufficient observations, using an explicitly labelled recent-rate estimate. Default: at least 24 hours of usable data, with up to 7 days for the recent daily average. Hide misleading predictions when coverage is poor.
- Traffic is a host-side estimate, not the provider's authoritative bill. Incoming traffic may have been billed before Linux drops it.

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
