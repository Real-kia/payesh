# 11 — Integration, security, performance and release readiness

Status: not started. Planning baseline: 2026-09-08.
Risk/assignment guidance: Lead/reviewer; bounded docs tasks can use lower-cost model.
Master milestone: M7; integrate after every package.

Read [shared context](00_SHARED_CONTEXT.md) and [the package guide](README.md) before acting. This document is a future build assignment template, not permission to start coding in the documentation-only session.

## Prerequisites and ownership

**Requires:** All required packages and UI production checkpoints. Plan tests earlier.

**Owns:** Lead integration/status/CI/shared migrations; final docs and local release artifacts.

Inspect actual repository conventions before selecting source paths. The lead owns shared contracts, schema ordering, dependency locks and integration unless explicitly delegated. A dependency may be replaced by an isolated test fixture while developing, but not for final integration acceptance.

## Work to implement

1. Integrate packages one at a time against approved contracts; review cross-package changes and regressions instead of blindly combining outputs.
2. Track M0–M7 separately from worker completion: M1 previews, M3 real UI, M5 three optional modules, M6 actual failure recovery.
3. Exercise novice journeys: install one server, add another, investigate metrics/logs, set allowance, install/preview/revert modules, survive disconnection, update and change role without data loss.
4. Run exact Linux/architecture/security/performance matrix; distinguish unit/fixture tests, logical-agent load and real kernel/VM evidence.
5. Measure whole process tree, kernel overhead, disk writes/network bytes, hidden-tab work, bundle size and 24-hour retention with environment/configuration/build/duration.
6. Write actual quickstart, uninstall, HTTPS/SSH, retention, backup/recovery, API and contributor guides; recheck competitor claims before marketing.
7. Prepare screenshots/demo, release notes, checksums/signatures and benchmarks; protect signing keys and leave publication subject to authorization.

## Acceptance gates

- Every required scenario has evidence or explicit blocker; mocks cannot verify enforcement.
- Clean Linux installation uses real signed artifacts with correct role inventories and absent optional extras.
- Resolve critical security/data-loss/connectivity issues and investigate budget misses; do not silently relax targets or market unmeasured results.
- Docs match observed behavior; memory stays compact; prepared is not published.

## Out of scope

Deferred features or calling the monitoring-only foundation the complete v1.

Reference extracts below sometimes mention neighboring packages. They explain integration requirements; they do not expand this worker's editable scope. Read-only inspection of neighboring interfaces is expected.

## Handoff and stop condition

Write `docs/handoffs/11.md` using the shared-context handoff checklist: baseline/files, implemented behavior, changed/consumed interfaces, actual test commands/results, measured or unmeasured costs, limitations, and next-package inputs. Request lead acceptance. Do not mark the full milestone/product complete or continue into another package on your own. If a prerequisite is missing, report exactly which contract, implementation or test environment is required.

## Detailed reference requirements

These are relevant verbatim extracts from [the master plan](../PLAN.md) at the planning baseline. Together with shared context, they are part of this assignment's requirements, not optional background. The lead synchronizes affected copies when the master changes.

## 18. Ordered milestones and concrete completion gates

All milestones below are required for the full v1. Keep a progress checklist in `docs/IMPLEMENTATION_STATUS.md` during implementation, with links to evidence. Do not mark future work done based on this plan's existence.

### M0 — Grounding and repository foundation

- Inspect the remote repository and local changes before creating scaffolding; do not overwrite existing work or initialize a conflicting repository.
- Record dependency/toolchain versions, support matrix, MIT license default, data-layout conventions, and artifact names.
- Scaffold the Go workspace, separate build targets, frontend build, lint/type checks, and CI. Do not add unrelated frameworks.
- Gate: the empty core targets build for x86-64/ARM64; the node dependency graph contains neither frontend files nor the SQLite server driver.

### M1 — Design foundation and previews

- Install/read the pinned UI/UX Pro Max development skill and produce the Payesh design system.
- Implement realistic fixture-based previews for onboarding, overview, and server details/logs, with empty/error/mobile states.
- Gate: visual checks in section 12 pass and the bundle budget is measured. Fixtures are isolated and labelled; production data wiring remains an explicit later task.

### M2 — Local monitoring, storage, logs, and CLI

- Implement collectors, timestamp/counter semantics, SQLite history, rollups, storage limits, basic log adapters, snapshots, and local API/CLI.
- Support standard/economy settings, config persistence, restart recovery, and live-data freshness.
- Gate: a disposable Linux machine supplies real metrics/logs; interval changes, reboot, log rotation, downsampling, and disk-budget tests pass. No UI-only mocks count.

### M3 — Roles, fleet communication, and installers

- Implement authenticated enrollment, node spooling, hub queries, role-specific artifact installation, and systemd/OpenRC definitions.
- Add direct installation and SSH password/key installation from the hub, including preflights and progress.
- Wire the UI to real APIs; connect a 20-node test fleet.
- Gate: nodes install no UI/server storage, reconnect and deduplicate correctly, identity errors are rejected, and a failed SSH install stays visibly failed.

### M4 — Alerts and usable daily monitoring

- Implement alert state, maintenance, Telegram/webhook delivery, billing periods, quota forecasts, snapshots, and linked chart/log workflows.
- Gate: timestamp-based durations, duplicates/retries, missing data, notification recovery, and monthly boundaries pass tests. A novice can find an unhealthy server and related evidence without a query language.

### M5 — Optional modules and resource enforcement

- Implement signed module distribution/jobs and the separately downloaded Port Traffic, CPU Controls, and Bandwidth Controls modules.
- Include previews, exclusions, local persistence, cleanup, and independent connectivity rollback.
- Gate: real CPU/network workloads demonstrate effective limits in isolated VMs; conflicting qdiscs, shared groups/ports, unsupported kernels, failed removal, and management connectivity are handled correctly.

### M6 — Updates, migration, and recovery

- Implement signed release manifests, compatibility negotiation, rolling updates, independent watchdog recovery, backups, and all role transitions.
- Gate: upgrade from an older real build, deliberately break startup/migration, recover automatically, and verify history/policies. Test an offline node and interrupted transfer.

### M7 — Release readiness

- Run the compatibility/performance/security/usability matrix. Produce installation, recovery, uninstall, API, module, data-retention, and known-limitations documentation.
- Create screenshots, a short demo, honest benchmarks, release notes, checksums/signatures, and contributor instructions. Provide an example small-fleet setup.
- Gate: all required capability checks have recorded results, no critical known failure is hidden, and a clean machine can follow the published instructions using actual built artifacts.
- Prepare release artifacts locally/through the authorized CI workflow; publishing remains subject to the owner's actual authorization in the implementation session.

## 19. Edge-case and acceptance checklist

Use meaningful unit tests for arithmetic/state transitions and Linux integration tests for kernel behavior. Mocks cannot prove a traffic cap or cgroup policy works. Browser tests should exercise real API contracts; visual fixtures are appropriate only for layout-state coverage.

A macOS development host is not sufficient to validate Linux controls. Use disposable local Linux VMs or explicitly authorized test hosts. Do not provision paid cloud resources without authorization. Multiple logical agents may exercise 20-node protocol/load behavior in a controlled environment, but label that topology honestly and keep actual Linux collector/control tests separate from simulated telemetry.

| Area | Required scenarios / expected result |
| --- | --- |
| Installation | Fresh/repeated/partial install; wrong architecture; unsupported init; package lock; no sudo; full disk; occupied ports. Preserve data and report the failing stage. |
| SSH | Password, encrypted key, changed host key, timeout, disconnected upload, sudo failure. No credentials in persistence or logs. |
| Identity | Duplicate hostname/IP, cloned filesystem identity, two connections, revoked certificate, expired enrollment token. Never silently merge or enroll twice. |
| Data delivery | Hub restart, disconnect, ack loss, retransmission, out-of-order batch, bounded spool overflow. Deduplicate and show coverage gaps. |
| Time | Clock jumps, timezone changes, DST, leap years, reset day 31, late samples. Correct intervals and billing period identity. |
| Metric arithmetic | Counter resets, missing values, CPU hotplug, wrapped/large integers, disk replacement, interface rename. No negative usage or fake zeros. |
| Retention | Sparse/hourly samples, rollup boundaries, cap exhaustion, large WAL, failed cleanup, long uptime. Stay bounded and keep correct totals. |
| Logs | Rotation/truncation, permission denial, huge/multiline entries, invalid UTF-8, secrets, hostile HTML, path traversal, cancellation. Safe bounded rendering/querying. |
| Alerts | Sustained vs brief spike, sparse coverage, threshold oscillation, duplicate notifications, maintenance/recovery, webhook outage. Correct state and bounded retries. |
| Port accounting | TCP/UDP, IPv4/IPv6, both directions, NAT, bridge/forwarded container traffic, proxy shared ports, rule reload. No duplicate totals or false app attribution. |
| CPU | Dedicated/shared cgroups, parent stricter limit, process exit/PID reuse, service restart, unsupported permissions. Correct target or explicit refusal. |
| Bandwidth | Existing third-party qdisc, low speed, quota crossing/reset, reboot, SSH on custom port, shared management port, hub loss. Keep management/recovery and show accounting uncertainty. |
| Modules | Tampered/wrong-version archive, extraction escape, partial download/install, cancellation, disable/remove with active policy. Preserve working state and recoverability. |
| Updates | Healthy old→new, incompatible node, broken binary, failed migration, power loss, insufficient backup space, stale browser assets. Restore a compatible complete old state. |
| Role changes | Each supported transition, unavailable old hub, duplicate import, interrupted cutover, fleet still attached. Preserve identity/history and one active controller. |
| Browser | Slow network, expired session, refresh during job, mobile/zoom, huge labels, empty/stale/unsupported data, keyboard/reduced motion. No hidden failures or inaccessible actions. |
| Resource use | 1/20 nodes, 5/15/60/3600-second sampling, log flood, slow disk, high packet rate, hidden tabs, disabled modules. Bounded costs and honest benchmark results. |

Include unit tests for rate deltas, weighted rollups, quota period calculations, idempotency, alert hysteresis, version compatibility, and policy/job state transitions. Use disposable VMs for changes to firewall, cgroups, network shaping, system services, and reboot recovery. Never test these destructively on the owner's working server.

## 20. Documentation and future expansion rules

Keep this handbook and a short implementation-status file current. Keep `MEMORY.md` under roughly 500 words by replacing outdated notes, not appending a transcript.

The README should explain who Payesh is for, what the core installs, available roles, optional downloads, an actual quickstart, screenshots, measured costs, and important limitations. Avoid “better than Grafana” without a narrowly scoped reproducible comparison.

Prepare operating guides for HTTPS/proxy access, SSH installation, log retention, failed updates, backups/restores, node detachment, capability limitations, and safe uninstall. Document API compatibility and the module trust model for contributors.

Future OS-maintenance modules must have a separate specification for package updates, reboot policy, SSH recovery, and authorization. Do not reuse the Payesh updater as an arbitrary system-upgrade mechanism. Future MCP starts read-only over the existing API; it does not gain control powers automatically. Future continuous logging needs a separately measured storage/traffic budget.

Additional technical references used during planning: [SQLite deployment suitability](https://www.sqlite.org/whentouse.html), [Go release policy](https://go.dev/doc/devel/release), [Svelte documentation](https://svelte.dev/docs/svelte/overview), and [ACME client support](https://github.com/go-acme/lego). Read current primary documentation before implementation instead of relying on this plan for library-version details.
