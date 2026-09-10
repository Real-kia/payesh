# Shared context — every implementing AI reads this

Planning baseline: 2026-09-08. This is a specification, not implemented software. Read this file together with your assigned package, repository instructions, and the actual approved contracts. Start with [the package guide](README.md).

## Product and scope

Payesh is open-source Linux VPS monitoring for beginners with one or multiple servers. Resource lightness comes first: CPU, RAM, disk writes, storage, network, and browser costs. Simple setup and a modern, accessible dashboard come next. GitHub popularity is an ambition, not a guarantee.

Confirmed: standalone/hub/node and CLI-only operation; history-preserving role transitions; resources, network usage, traffic allowances, bounded existing-service logs and incident snapshots; direct and hub-assisted SSH installation; configurable sampling/retention; optional modules actually downloaded only when requested. Full v1 includes Port Traffic, CPU Controls, and Bandwidth Controls. Installing an extra never silently activates enforcement.

Deferred: RAM controls, per-process bandwidth control, OS updates/reboots, arbitrary remote administration, continuous centralized application-log archives, container-specific integrations, probes/status pages, MCP, AI diagnosis, SaaS, and multi-tenant roles. Container/forwarded traffic still matters for accounting correctness. An API or modern chart alone is not a unique advantage.

Use UI/UX Pro Max in the authorized UI development phase only; it is not a VPS runtime dependency. Keep owner-facing chat short and MEMORY.md below roughly 500 words.

## Authority and coordination

1. This documentation task does not authorize application implementation. A later explicit build assignment authorizes only its stated scope.
2. The lead reads the full [master plan](../PLAN.md). Each worker reads this file, its full package including reference extracts, applicable repository instructions, approved interface/schema definitions, and dependency handoffs.
3. Confirmed owner requirements outrank engineering defaults. The master is the canonical detailed specification; package extracts are snapshots. If code, contracts, or documents disagree, report the exact conflict to the lead. Do not silently choose whichever is easier.
4. Foundation must turn the plan's resource families into concrete contracts. Proposed paths below are conventions until the actual repository is inspected. Do not invent another architecture, schema, unit convention, or API to make your package easier.
5. Before editing, identify a baseline revision, assigned files, dependencies, and tests. Use separate worktrees/branches when practical. In a shared working tree, edit only explicitly assigned non-overlapping files; never reset another worker's changes. Check the repository root before any Git operation.
6. The lead owns shared contracts, schema migration ordering, dependency/lockfile changes, routing, CI, and integration. A worker proposes shared changes with consumers/tests identified; the lead applies or explicitly delegates them. The UI owner implements browser components; backend workers provide documented endpoints, fixtures, and integration requests.
7. A package can contain reference material owned by another package. Reading it does not authorize implementing that other package. Stop after your assigned acceptance gates and handoff; do not automatically continue to another milestone.
8. Missing inputs: identify the missing contract or dependency precisely. Continue independent work only if useful; label any fixtures and keep them out of production paths. Never claim a mocked dependency is working production integration.
9. A package is not accepted just because it compiles or its author says it is done. The lead reviews the diff, runs relevant integration tests, and records acceptance or remaining gaps.

## Shared semantic invariants

- Standard sampling 15 seconds; economy 60 seconds; configurable 5–3600 seconds. Heartbeats and enforcement have independent cadences.
- Stable server ID, new collector epoch for each agent start, sequence within epoch. Never identify a server by hostname/IP alone. Source and receipt timestamps are separate; monotonic deltas calculate rates.
- Missing/unsupported/stale is not zero. Store integer bytes; large 64-bit JSON quantities use decimal strings. CPU policies use integer millicores; bandwidth policies use integer bits/second.
- Calendar billing periods are timezone-aware. Reboot/update must not reset usage. Counter gaps/resets expose uncertainty, not invented zero traffic.
- One active hub per node. Node initiates the authenticated bidirectional connection. Acknowledgements mean durable acceptance; retries must not double-count.
- Logs are untrusted events, not numeric samples. Existing logs remain on their source; bounded snapshots are not a complete archive. No arbitrary privileged file reads.
- Controls are separate from warnings. Preview, validate, apply, verify, audit, and revert. Protect management connectivity; refuse unsafe targets or ownership conflicts.
- No arbitrary shell endpoint, remote script runner, third-party executable catalog, or automatic OS upgrades. Do not operate on production servers or publish/provision paid resources without explicit authorization.
- CPU/network kernel behavior requires disposable Linux tests. A macOS host or mocks cannot prove enforcement. Unrun tests remain unverified.

## Cross-cutting reference extracts

The following are copied from master sections 5, 13, 16, and 17. They apply to every package. Targets are not achieved measurements. The lead must synchronize these extracts after an approved master-plan change.

## 5. Architecture and distribution layout

Default stack:

- Go for agents, hub/server, CLI, installer helpers, and optional module executables.
- SQLite for standalone/hub data, using a pinned [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) version for builds without a production C toolchain requirement.
- Svelte 5, TypeScript, and Vite for the dashboard; [uPlot](https://github.com/leeoniya/uPlot) for time-series charts.
- Pin supported toolchain and dependency versions when scaffolding. Commit dependency lockfiles. Production machines need no Node.js, Python, Docker, Redis, or separate database service.

Separate distributable artifacts, sharing internal packages:

| Artifact | Responsibility |
| --- | --- |
| `payesh-agent` | Collect metrics, bounded existing-log access, node connection, small offline spool. No UI files or SQLite dependency in its build graph. |
| `payesh-server` | Standalone/hub API, authentication, history, alerts, jobs, fleet management, serving installed static UI files. |
| `payesh` | On-demand CLI for status, configuration, setup, modules, updates, and role transitions. Not another always-running process. |
| `payesh-privd` | Small privileged helper for approved installation/update/service operations and invoking installed control modules. |
| Web asset archive | Versioned static files, only downloaded for a web role. |
| Optional module archives | Separately built feature executables and metadata. Not compiled into or bundled with the basic install. |

The privileged helper is part of the management foundation, not a license to ship all optional control implementations in the core. Its base action set is narrow. Module-specific code is supplied only by verified installed modules.

Run agent/server under dedicated unprivileged accounts. Communicate with the local helper through a permission-controlled Unix socket and typed requests. The helper validates paths, target identities, supported actions, and module ownership/version. It must never provide “execute this shell string” as an API.

Default host paths: root-owned configuration in `/etc/payesh`, mutable application data in `/var/lib/payesh`, versioned binaries/assets in `/opt/payesh/releases`, and sockets in `/run/payesh`. Do not scatter runtime files into the source checkout. Use restrictive permissions for secrets and credential material.

Suggested source organization: `cmd/` for executables, `internal/` for shared services/adapters, `web/` for UI, `modules/` for separately built extras, `api/` for OpenAPI, and `docs/` for operating and compatibility documentation. Keep optional modules out of core artifact dependency graphs.

### Role behavior

| Role | Agent | Server/history | UI | Privileged helper |
| --- | --- | --- | --- | --- |
| Standalone | Yes | Yes | Yes | Yes |
| Hub | Optional local agent | Yes | Yes | Yes |
| Node | Yes | No; bounded spool only | No | Yes |
| CLI-only standalone | Yes | Local-only history/API | No | Yes |

“Standalone has everything” means everything in the basic standalone experience, not preinstalled extras. The hub's optional local agent prevents an otherwise unmonitored hub from being mistaken for a monitored server.

## 13. Public interfaces and minimum data model

Use REST JSON under `/api/v1`, documented by OpenAPI. Browser updates use a bounded event stream; node transport uses the separate versioned WebSocket protocol. Use the same backend operations for CLI and UI so behavior does not diverge.

Initial API resource families:

| Resource | Required operations |
| --- | --- |
| Session/setup | One-time setup, login/logout, owner recovery through local CLI, session expiry. |
| Servers | List/detail, labels, enrollment/revocation, capabilities, connection/freshness state. |
| Metrics/traffic | Time-range queries, resolution selection, allowances, period totals, coverage. |
| Log sources/entries | Discover allowed sources, bounded query/live tail, cursor pagination. |
| Alerts/snapshots | Rules, current state, history, maintenance windows, incident detail. |
| Modules | Catalog, per-server status, install/enable/disable/remove jobs. |
| Policies | Preview, apply, inspect effective state, revert; only for installed eligible modules. |
| Updates/jobs | Available release, preflight, selected-machine rollout, job status/cancellation/recovery. |
| Settings/backups | Retention/budget settings, export/import, role transition jobs. |

Minimum shared records:

- `Server`: stable ID, name, role, architecture/platform, capabilities, version, last heartbeat, configuration revision.
- `MetricSample`: server ID, collector epoch, sequence, observation/receipt timestamps, typed values, units, validity/coverage.
- `Rollup`: server/metric/bucket, observed duration/count, min/max/weighted mean or valid counter delta as appropriate.
- `TrafficPeriod`: server/scope, timezone/bounds, allowance, direction, counted bytes, continuity status.
- `LogSource` and `LogEntry`: source identity, timestamp, optional severity, text, cursor, truncation/redaction indicators.
- `AlertRule`, `AlertState`, `IncidentSnapshot`, `ModuleInstallation`, `ControlPolicy`, `Job`, and `AuditEvent`.

API conventions:

- Store timestamps in UTC; use RFC 3339 timestamps externally and explicit display/billing timezone settings.
- Preserve integer byte counters and policy units. Serialize 64-bit values that may exceed JavaScript's safe integer range as decimal strings; do not lose billing precision in JSON parsing.
- Errors contain a stable code, a user-facing explanation, optional field errors, retryability, and correlation ID. Do not return secrets or raw shell traces to the browser.
- Limit time ranges, pagination, filter lengths, body sizes, subscriptions, and execution deadlines. Parameterize database queries and invoke external programs with argument arrays, not interpolated shell commands.
- Mutating jobs require an idempotency key and expected revision where appropriate. Repeated Apply/Install clicks must not create duplicate policies or parallel installers.
- Return a job identifier for asynchronous actions. Persist progress and final failure/recovery details; do not expose internal implementation noise as the primary user explanation.
- Scope API tokens to read operations by default; separately grant configuration/module/control operations. A future MCP adapter will use the read-only API without requiring the core to depend on an AI service.

Do not turn this into a general-purpose query language, workflow engine, shell console, or multi-tenant authorization platform for v1.

## 16. Security and operational boundaries

Remote installation and control make the hub a privileged management system. Implement these protections as part of the product, not as optional polish:

- Password hashing with a modern maintained algorithm, secure/HttpOnly/SameSite session cookies, CSRF protection, session expiry, and login throttling.
- A one-time setup secret, no default password, no public unauthenticated log/metric/control endpoints, and a documented local recovery path.
- Per-node identity/certificate revocation, authenticated enrollment, replay-resistant jobs, and separation between browser and node routes.
- Node certificates identify a specific node; one compromised node cannot query another server's history or execute hub-admin actions.
- Redacted structured audit events for policy, enrollment, module, update, and role changes. Audit history has its own clear retention behavior inside the configured budget.
- No arbitrary shell execution or arbitrary privileged file reads through APIs. Logs and SSH output are untrusted data.
- Browser Content Security Policy, escaped log rendering, secure download headers, and dependency/version pinning.
- Explicit owner-configured network targets for SSH/webhooks and bounded requests. Private IPs are legitimate for this product; require authenticated owner intent instead of creating an unauthenticated proxy or broadly exposing internal services.
- Encrypted sensitive exports and restrictive local key permissions. Do not send data, telemetry, or diagnostics to an external service by default.
- Installation/removal must preserve unrelated firewall rules, service configuration, and data. Provide a safe uninstall path; removing retained history is a separate explicit choice.

A compromised authorized hub owner can intentionally manage enrolled machines. Do not claim this trust model protects nodes from every malicious action by their legitimate controller; limit the supported action surface and document the trust boundary honestly.

## 17. Performance budgets and measurement procedure

These are release targets, not measured results. Never put them in marketing as facts until reproducible tests demonstrate them.

| Scenario | Initial target |
| --- | --- |
| Basic node, standard 15-second collection, no live logs/modules | Combined Payesh resident RAM under 50 MiB; average CPU under 1% of one vCPU. |
| Hub, 20 basic nodes, one ordinary dashboard viewer | Combined Payesh server-side resident RAM under 250 MiB. Record CPU and p95 query latency. |
| Normal chart queries over retained history | p95 under 500 ms on the documented test machine, excluding network RTT. |
| Initial dashboard JS+CSS | At most 300 KiB compressed; lazy chunks measured separately and kept justified. |
| Managed storage | Configured live-data limits enforced, with no unbounded WAL/log/spool growth. |
| Disabled/uninstalled module | No module process/polling; confirm whether any kernel state remains and remove it when disabled. |

Measure on a documented low-end Linux VPS: one vCPU, 512 MiB RAM for the node; a documented 1–2 vCPU/1 GiB machine for the hub. Also test ARM64. Report kernel/distribution, vCPU model, metric count, sampling interval, viewer count, duration, build revision, and enabled modules.

Run at least a 30-minute steady-state measurement after warmup, plus a 24-hour storage/retention soak. Measure the entire Payesh process tree, actual disk writes, monitoring bytes, open connections, and browser memory/CPU. Record module overhead separately under idle and high packet/process load.

For traffic modules, also compare whole-system/kernel CPU and workload throughput/latency with the same traffic and modules disabled/enabled. Kernel packet processing is real overhead even when no userspace process appears busy. Exercise relevant offload paths and report unsupported accounting arrangements.

Test 5-second and hourly collection as well as defaults. Collection frequency must not change heartbeat health or secretly disable quota enforcement. No hidden-tab polling storm, per-chart independent duplicate queries, per-sample subprocess explosion, or unbounded cardinality is acceptable.

If a target is missed, investigate and record the cause. Do not hide the cost in an uncounted helper, lower measurement frequency only for the benchmark, or relax targets silently.

## Required handoff

Write a short record under `docs/handoffs/<package-id>.md` during implementation. Do not create a fictitious completion record in advance.

- Package ID, baseline/final revision if available, and actual files changed.
- Implemented behavior, explicit omissions, platform restrictions, and remaining fixtures.
- Contracts consumed/changed; affected consumers; migration/configuration impact.
- Exact test commands and outcomes, including failed, skipped, and unrun checks.
- Resource measurements with environment/duration, or clearly state not measured.
- Security/privilege/rollback review findings and recovery instructions where applicable.
- What the next package can rely on; unresolved blockers; requested integration changes.

Update the lead's `docs/IMPLEMENTATION_STATUS.md` only through its assigned owner. That status file is created when implementation starts. “Ready for review,” “accepted,” and “blocked” must remain distinct.
