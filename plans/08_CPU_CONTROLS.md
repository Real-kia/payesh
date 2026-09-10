# 08 — CPU controls for safe workload targets

Status: not started. Planning baseline: 2026-09-08.
Risk/assignment guidance: Hard; cgroup/privilege review.
Master milestone: M5 module.

Read [shared context](00_SHARED_CONTEXT.md) and [the package guide](README.md) before acting. This document is a future build assignment template, not permission to start coding in the documentation-only session.

## Prerequisites and ownership

**Requires:** 06 lifecycle/helper ABI; 04 identity/capabilities/jobs. May run alongside 07 with non-overlapping files.

**Owns:** Separate CPU Controls module and cgroup/systemd/OpenRC adapters; CLI edits via assigned CLI owner.

Inspect actual repository conventions before selecting source paths. The lead owns shared contracts, schema ordering, dependency locks and integration unless explicitly delegated. A dependency may be replaced by an isolated test fixture while developing, but not for final integration acceptance.

## Work to implement

1. Implement target discovery, millicore preview/validation, effective quota inspection and restoration of changed CPU properties only.
2. Distinguish dedicated groups from unsafe shared groups; verify PID start identity/pidfd so PID reuse cannot inherit stale policy.
3. Handle persistent service versus temporary process-group lifetime, stricter parent quotas and external configuration changes.
4. Provide local payesh run workflow for a dedicated group when safe; coordinate CLI integration. Do not create a remote arbitrary-command endpoint.
5. Implement audit/revert/restart and failed-cleanup recovery through 06; preserve unrelated supervisor/resource settings.
6. Explain cores versus whole-server percentage; workload budgets do not change provider allocation or constrain all kernel work.

## Acceptance gates

- Real CPU-load tests show effective quota and restoration on disposable Linux.
- Test shared-group refusal, parent quotas, children, PID reuse/exit, restart, permissions and OpenRC limitations.
- Failed target verification is not success; management services and unrelated settings survive.
- Disabled/uninstalled state and measured costs meet lifecycle requirements.

## Out of scope

RAM limits, arbitrary remote execution, moving unrelated processes or provider-vCPU resizing.

Reference extracts below sometimes mention neighboring packages. They explain integration requirements; they do not expand this worker's editable scope. Read-only inspection of neighboring interfaces is expected.

## Handoff and stop condition

Write `docs/handoffs/08.md` using the shared-context handoff checklist: baseline/files, implemented behavior, changed/consumed interfaces, actual test commands/results, measured or unmeasured costs, limitations, and next-package inputs. Request lead acceptance. Do not mark the full milestone/product complete or continue into another package on your own. If a prerequisite is missing, report exactly which contract, implementation or test environment is required.

## Detailed reference requirements

These are relevant verbatim extracts from [the master plan](../PLAN.md) at the planning baseline. Together with shared context, they are part of this assignment's requirements, not optional background. The lead synchronizes affected copies when the master changes.

## 10. Resource controls: behavior and failure cases

The distinction between observation and enforcement must remain obvious. A notification threshold does not automatically authorize throttling or blocking. Installing a module does not enable any limit.

Each control follows: select target → show existing state and expected effect → validate compatibility/management exclusions → apply → verify effective state → record an audit event. A failed verification is not “success.” Every policy has a visible disable/revert action.

### CPU Controls

- Use cgroup v2 CPU quotas and systemd resource properties where applicable. The [kernel cgroup documentation](https://www.kernel.org/doc/html/latest/admin-guide/cgroup-v2.html) explains the underlying controls; the UI must express their effect understandably.
- Let the owner select an allowance in CPU cores, for example “at most 1 core, equivalent to 25% of this 4-vCPU server.” API values use integer millicores. Never confuse 100% of one core with 100% of the whole machine.
- Target an identifiable service or dedicated process cgroup. Preserve and restore the previous quota rather than resetting all resource settings.
- For existing processes, require a dedicated, safely controllable cgroup. Verify process identity using start time/pidfd where available; a reused PID must not inherit a previous process's policy.
- Do not automatically move unrelated processes into a shared group or rewrite their supervisor configuration. If an unmanaged workload lacks a dedicated group, explain the limitation and provide a local `payesh run` workflow that starts that workload in its own group. Arbitrary remote command execution is not introduced by this local CLI workflow.
- Service policies persist across restarts. Temporary process-group policies expire when their workload ends. Parent/child processes within the group share its allowance.
- On OpenRC, use dedicated cgroup ownership where available; report unsupported service arrangements rather than applying limits to an entire shared cgroup.
- “Whole VPS CPU limit” means a budget for explicitly selected workloads, with operating-system and management services excluded. It cannot change a hosting provider's vCPU allocation or impose a magic cap on all kernel work.
- Detect inherited stricter quotas and report effective limits. Preserve other administrators' configuration; a policy whose target changed externally requires revalidation.

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
