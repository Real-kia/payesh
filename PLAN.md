# Payesh: implementation handbook

Planning baseline: 2026-09-08. Repository: https://github.com/Real-kia/payesh

This is a build specification for a future implementation session, not a claim that the software exists. At the time this document was created, the local workspace contained project notes only. No application, design skill, deployment, or GitHub changes were created as part of writing this plan.

For smaller AI assignments, start with [the build-package guide](plans/README.md). It divides implementation into 11 bounded packages with mandatory shared context, dependencies, review levels, detailed requirement extracts, and handoff prompts. This full handbook remains the canonical reference.

Quick navigation: [Requirements](#1-product-purpose-and-non-negotiables) · [Architecture](#5-architecture-and-distribution-layout) · [UI design](#12-uiux-design-workflow-including-uiux-pro-max) · [Updates](#15-releases-upgrades-compatibility-and-rollback) · [Milestones](#18-ordered-milestones-and-concrete-completion-gates) · [Edge-case tests](#19-edge-case-and-acceptance-checklist) · [Handoff prompt](#21-copyable-handoff-prompt)

## 0. How the implementing AI should use this document

1. The lead or single implementing AI reads `MEMORY.md`, this entire document, and applicable repository instructions. A bounded worker instead reads `plans/00_SHARED_CONTEXT.md`, its complete assigned package including reference extracts, applicable instructions, approved contracts, and dependency handoffs. Inspect the actual repository; it may have changed since planning.
2. The lead implements and accepts milestones in section 18 in order, using the package guide for safe parallel boundaries. A bounded worker stops at its assigned package/checkpoint and handoff. A milestone is finished only when its acceptance checks pass. A working dashboard with simulated values is not a finished monitoring product.
3. Preserve the confirmed requirements below. Engineering defaults are supplied so you can proceed without asking the owner to choose libraries or invent API contracts. Change a default only when concrete evidence shows a problem; document the reason and replacement.
4. Keep communication with the owner short. Put detailed decisions, test results, and limitations in project documentation. Do not repeatedly paste this plan into chat.
5. Preserve unrelated changes. Do not publish, deploy, message other people, or run resource controls on real production servers just because this plan mentions those activities. Use disposable Linux test machines for privileged tests.
6. Do not quietly skip hard requirements, label missing capabilities as implemented, or substitute mock data for real measurements. If a platform cannot support a feature, report that capability accurately.
7. The full v1 includes optional CPU and bandwidth enforcement. It is acceptable to develop these after the monitoring foundation, but not to call the foundation alone the completed v1.

### Requirement status

| Status | Meaning |
| --- | --- |
| Confirmed | Explicit owner preference or answer from the planning conversation. Preserve it. |
| Default | Engineering choice supplied by this plan to make implementation actionable. It is not a separately negotiated owner requirement. |
| Deferred | Deliberately outside v1. Do not expand into it while required work remains. |

## 1. Product purpose and non-negotiables

**Payesh is simple, lightweight Linux VPS monitoring with optional resource controls.** Its first audience is a VPS owner with one or several servers, including people who find monitoring stacks and command-line setup difficult.

Confirmed requirements:

- Low CPU, RAM, storage, disk-write, and monitoring-network overhead are the highest priority. A small download alone is insufficient.
- Installation and routine use must be simple. Provide modern, polished, responsive charts and a clear web interface.
- Monitor resources, network rates, traffic totals, and useful logs.
- Support standalone, hub, and node roles, with transitions between them. Also support CLI-only use.
- Nodes must not download or install dashboard assets or other components they do not need.
- Extra features must actually be downloaded and installed on request, from a panel button or equivalent CLI command. Merely hiding preinstalled functionality is not enough.
- v1 must include downloadable port traffic, CPU-control, and bandwidth-control capabilities.
- Provide basic existing-service-log access in the core, configurable collection intervals, and automatic retention/cleanup of Payesh-owned data.
- Allow a hub to install nodes over SSH with a password or key, as well as direct one-command installation on the target machine.
- Design version compatibility, upgrades, and rollback from the beginning. Default behavior is to notify about a release and let the owner update selected machines.
- Support multiple Linux distribution families at launch. Test for 1–20 VPSs; do not impose a commercial server-count cap.
- Use UI/UX Pro Max as a development design aid, alongside visual and usability verification.
- Keep the conversation memory compact, roughly 500 words maximum. This file holds the detailed specification.

GitHub popularity is an ambition, not a guarantee. The intended advantage is the combination of approachable setup, understandable server problems, traffic budgets, and optional controls. Do not claim that charts, logs, APIs, or MCP are new inventions.

## 2. Existing projects and evidence for the direction

This is a documentation comparison, not an independent performance benchmark. Features and issue statuses may change; recheck before publishing comparisons.

| Project | Existing strengths | What to learn / Payesh positioning |
| --- | --- | --- |
| [Grafana ecosystem](https://grafana.com/oss/) | Dashboards and alerting, with complementary metric and log collection/storage projects such as Prometheus, Alloy, and Loki | Provide a ready-made VPS experience without asking beginners to assemble a stack or write dashboard queries. |
| [Beszel](https://beszel.dev/) | Lightweight multi-server monitoring, resource history, container statistics, alerts, backups, and REST API | Closest competitor. Simplicity alone will not distinguish Payesh; traffic budgets, port detail, and optional controls are the focus. |
| [Netdata](https://github.com/netdata/netdata) | Detailed real-time collection, automatic discovery, alerts, logs, and multi-node monitoring | Learn automatic discovery and incident context. Its open-source agent and separately licensed UI/cloud must not be described as one uniformly open-source product. |
| [Uptime Kuma](https://github.com/louislam/uptime-kuma) | Service/website availability checks, notifications, and status pages | Learn approachable setup and understandable health states. Full synthetic monitoring is not v1 scope here. |
| [Dozzle](https://dozzle.dev/) | Live container logs, search, multi-host views, charts, alerts, and MCP | Logs beside metrics already exist. Payesh should also fit ordinary Linux services and VPS traffic concerns. |
| [Cockpit](https://cockpit-project.org/) | Browser-based Linux administration for beginners and experienced operators | Learn from visual management, but keep v1 focused on monitoring and specified controls. |
| [btop](https://github.com/aristocratos/btop) | Terminal resource charts and process inspection | Learn information clarity. Payesh adds persistence and optional centralized access. |

Specific demand signals reviewed:

- [Beszel issue #2056](https://github.com/henrygd/beszel/issues/2056) requested monthly bandwidth quotas, configurable billing reset dates, and warnings for VPS fleets.
- [Beszel PR #2266](https://github.com/henrygd/beszel/pull/2266) proposes network probes. Availability checks are already being pursued by competitors, not an uncontested opportunity.
- [Beszel PR #2257](https://github.com/henrygd/beszel/pull/2257), listed in the project's PR index, concerns configurable retention.
- A [beginner VPS discussion](https://www.reddit.com/r/VPS/comments/1lk5r89/server_monitoring_which_service_to_choose/) asks for approachable resource monitoring and login-related logs; another [discussion about monitoring complexity](https://www.reddit.com/r/selfhosted/comments/1npobk0/newbie_needs_monitoring_feeling_overwhelmed/) describes difficulty assembling metrics and logging components. These are anecdotes, not market-size measurements.
- [Beszel's API](https://beszel.dev/guide/rest-api) and [Netdata's MCP support](https://github.com/netdata/netdata/blob/master/docs/netdata-ai/mcp/README.md) mean API/MCP alone is not a distinguishing claim.

## 3. Scope: core, downloadable v1 modules, and later work

| Capability | Basic installation | Downloadable in v1 | Deferred |
| --- | --- | --- | --- |
| CPU/RAM/swap/load/disk/interface metrics, uptime | Yes | — | — |
| Historical charts, fleet view, traffic budgets | Yes, on standalone/hub | — | — |
| Read existing Linux service logs; incident snapshots | Yes, bounded and on demand | — | — |
| Alerts, maintenance pauses, Telegram/webhooks | Yes | — | — |
| REST API and basic CLI | Yes | — | — |
| Per-port byte totals and rates | No | Port Traffic | — |
| CPU limits for supported services/process groups | No | CPU Controls | — |
| Interface/port bandwidth speed caps and quota actions | No | Bandwidth Controls | — |
| RAM limits | No | No | Yes |
| OS package updates, reboots, arbitrary remote administration | No | No | Yes |
| Continuous central archive of application logs | No | No | Yes |
| Docker/Podman-specific logs and resource controls | No | No | Yes |
| MCP adapter and AI-generated diagnoses | No | No | Yes |
| Website/ping/DNS probes and public status pages | No | No | Yes |
| High-availability hubs, Kubernetes, billing, hosted SaaS | No | No | Yes |

Container traffic passing through a host still matters to port-accounting correctness; deferring a Docker integration is not permission to double-count that traffic. Mark unsupported network arrangements rather than inventing attribution.

Lightweight incident explanations in v1 are deterministic summaries of evidence, such as “memory pressure and a service restart occurred in this interval.” They must not assert a root cause from correlation alone.

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

## 6. Installation, onboarding, and secure access

### Direct installation

1. Offer a short documented bootstrap command and a downloadable installer for users who want to inspect it. No placeholder release URLs may ship in the final README.
2. Detect the target and show role selection. Explicit role flags allow unattended installation.
3. Preflight platform capabilities, existing installation, available disk, occupied ports, and required privileges.
4. Download only role-required artifacts; verify release metadata and artifact integrity. Do not compile on the VPS.
5. Create service accounts, directories, service definitions, and restrictive credentials. Re-running the same installer must converge on the existing installation without deleting data or generating a second node identity.
6. Start services and show actual health results. A failed service is not a successful install.
7. Web roles complete onboarding in the browser. Node enrollment commands include a short-lived, single-use pairing token and the expected hub identity/fingerprint.

The first downloaded installer depends on the trustworthiness of its delivery channel. Do not pretend that a checksum downloaded alongside an untrusted installer independently authenticates that installer. Subsequent software trust is anchored by a release-signing key shipped in the installed binaries.

### Web onboarding

- Default to one owner account in v1; one owner can manage the whole fleet. Defer multi-tenant roles.
- Account creation requires the one-time bootstrap secret. Simply visiting an exposed setup page must not allow takeover.
- Guide the owner through access, collection/retention presets, notification setup, and optional server enrollment. Offer defaults and a skip path for nonessential steps.
- Use HTTPS for public dashboards. Support domain certificates and public-IP certificates with automated ACME renewal. [Let's Encrypt documents public IP certificates](https://letsencrypt.org/2026/01/15/6day-and-ip-general-availability.html); validate the selected ACME client's support and renewal behavior during implementation.
- Certificate validation still needs reachable challenge endpoints. If ports are occupied or blocked, offer existing-reverse-proxy configuration or a documented SSH tunnel. Do not silently stop nginx, publish credentials over HTTP, or replace a valid certificate with an untrusted fallback.
- Private/local mode binds to loopback unless deliberately configured otherwise. Existing reverse proxy support must validate trusted proxy addresses and WebSocket forwarding.
- Self-host necessary fonts/assets. Dashboard operation after installation must not depend on a CDN, third-party analytics, or an AI provider.

### Hub-assisted SSH installation

- Collect address, SSH port, username, and password or private key/passphrase. Support root login or sudo with the required credential flow; report the exact stage of failure.
- Verify known host keys. For a new host, show its fingerprint for confirmation; a changed key is an explicit failure, not an automatic acceptance.
- Keep credentials only for the installation job. Do not retain passwords/private keys in the database, logs, local storage, exception messages, or job command strings. Limit their in-memory lifetime.
- Transfer verified artifacts, perform the same idempotent installer steps, enroll the node, then verify real measurements reach the hub.
- After enrollment, use the authenticated agent connection for module installation and Payesh updates. Do not require SSH credentials to remain stored for routine management.
- Timeouts, failed sudo, unavailable package repositories, full disks, and partial uploads need clear job status and safe retry. A partially installed node must not appear healthy.

## 7. Hub/node protocol and disconnected operation

Use one persistent TLS WebSocket opened by each node to the hub. Direction is chosen for reconnect ownership and operational consistency, not a claim that outbound connections inherently use less CPU.

- Authenticate nodes with an internal enrollment CA and individual node certificates. Authenticate the hub using a pinned enrollment trust anchor. Dashboard browser certificates and internal node identity are separate concerns.
- Rotate internal node certificates automatically while a valid authenticated connection exists; default validity is 90 days, with renewal beginning 30 days before expiry. Detect expiring trust material. An expired/revoked identity requires an authenticated recovery/re-enrollment path, never a silent certificate-validation bypass.
- The node endpoint must enforce node authentication even when ordinary browser routes do not require a client certificate.
- Exchange protocol version, software version, capabilities, enabled modules, and effective configuration at connection time.
- The hub sends collection/configuration requests and typed jobs through the established channel. The connection is bidirectional despite being opened by the node.
- Keep one active hub per node in v1. Reject competing enrollments and simultaneous identity reuse; never merge machines merely because hostname or IP matches.
- Use a stable random server ID and a new collector-epoch ID at each agent start. Identify samples with server ID, epoch, and monotonically increasing sequence. A reboot ID alone does not distinguish two agent starts in one boot.
- Store source observation time and hub receipt time. Use monotonic elapsed time for rates. Large clock skew must be visible rather than silently rewriting history.
- Default heartbeat: 30 seconds. Mark a node unreachable after 90 seconds without a valid heartbeat. Heartbeats remain independent of a user selecting hourly metric sampling.
- Reconnect with jittered exponential backoff, capped at 60 seconds. Do not busy-loop on DNS, certificate, or authentication failures.
- Acknowledge measurements after durable acceptance, not before storage. Deduplicate retransmissions. Buffer at most 32 MiB on a node, using batched writes and an oldest-first eviction policy with explicit gap reporting.
- Prioritize acknowledgements, heartbeats, and cancellation over log streams or downloads. Bound queues and stream concurrency; a slow consumer must not cause unlimited memory growth.
- The hub owns fleet alerts. A disconnected node keeps existing local control policies active and keeps collecting within its buffer; it does not send duplicate fleet notifications.
- Avoid executing old management commands unexpectedly on reconnection. Commands include target identity, expiry, and expected configuration revision; expired/conflicting jobs fail for explicit retry.

## 8. Metrics, traffic accounting, and data semantics

### Core measurements

Collect CPU utilization and available steal/iowait signals, memory available/used, swap, load, uptime, disk capacity/inodes, disk I/O, and per-interface byte/error/drop counters. Distinguish unavailable readings from zero. Read kernel-provided data efficiently; do not launch many external commands every sample.

- Start with host-level bounded series. Per-process discovery is on demand; do not store every PID as a permanent metric dimension.
- Exclude loopback and virtual interfaces from the suggested billing total, but show discovered interfaces and let owners select the authoritative external interface(s).
- Keep host interface totals as the billing-estimate baseline. Never sum all bridge, veth, tunnel, and physical interfaces together by default.
- Preserve bytes as integer counts. Clearly distinguish bytes, bits/second, decimal GB/TB, and binary GiB/TiB. Default traffic allowances to decimal GB/TB; storage displays use MiB/GiB.
- On counter decrease/reset, start a new baseline and mark uncertainty. Do not create negative rates or huge rollover spikes.
- Network gaps do not automatically mean zero usage. A cumulative counter can recover a delta if continuity is known; do not claim exact historical timing inside the gap.

### Traffic allowances

- Per server: selected interface(s), outbound-only or combined traffic, allowance in bytes, billing reset day, billing timezone, and warning percentages. Default warnings: 80%, 90%, and 100%.
- Use UTC unless the owner selects a billing timezone. If a reset day is 29–31 and absent that month, use that month's last day. Compute successive periods explicitly; do not use “30 days” as a month.
- Store the period identity with usage. An ordinary reboot or software update must never reset the allowance.
- Changing allowance size affects the active period immediately; changing billing schedule takes effect at the next existing boundary unless the owner explicitly starts a new period. Preview this behavior.
- Forecast only after sufficient observations, using an explicitly labelled recent-rate estimate. Default: at least 24 hours of usable data, with up to 7 days for the recent daily average. Hide misleading predictions when coverage is poor.
- Traffic is a host-side estimate, not the provider's authoritative bill. Incoming traffic may have been billed before Linux drops it.

### Port Traffic module

The initial scope is selected local TCP/UDP service ports and their corresponding inbound/outbound traffic. Remote destination port 443 used by an outbound client is not automatically the same thing as a service listening locally on 443.

Use kernel counters/classification, not payload capture. [nftables counters](https://wiki.nftables.org/wiki-nftables/index.php/Counters) provide a foundation; implementation must validate both directions and connection identity.

- Distinguish protocol, selected interface, local port, and direction in accounting keys.
- Follow original and translated tuples where NAT/forwarding is involved. Count a packet once per declared accounting scope, not at every hook/interface it crosses.
- Shared ports and reverse proxies cannot always distinguish applications. Show port-level aggregates; do not label those values as individual application usage without additional evidence.
- Handle port reuse, sockets closing, IPv6, forwarded container traffic, VPN/tunnel visibility, counter resets, and rule reloads.
- Create only Payesh-owned rules/counters; do not flush another application's firewall state.
- A newly enabled module cannot reconstruct traffic from before installation. Show its actual coverage start and gaps.

## 9. Storage, logs, cleanup, and alert behavior

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

### Alerts

- Starter rules: CPU above 90% for 5 minutes, available RAM below 10% for 5 minutes, disk/inodes above 90%, traffic thresholds, and unreachable nodes. Make rules editable and clearly show enabled defaults.
- Evaluate using sample timestamps and coverage, not a fixed count of samples. If sampling is slower than a rule's duration, warn about reduced detection precision; do not claim continuous evidence from one point.
- Use pending/firing/recovered state, hysteresis, grouping, deduplication, and bounded retries. Default sustained-alert reminders: at most once per hour; recovery is a separate message.
- Maintenance pauses suppress notifications, not collection. Group symptoms during a server outage to avoid dozens of secondary alerts.
- Keep control-policy events distinct from monitoring alerts. Record who changed a limit and its result.
- A standalone server cannot report its own complete power/network outage without another observer. The UI/docs must explain this; do not simulate external coverage.

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

## 12. UI/UX design workflow, including UI/UX Pro Max

The owner explicitly requested [UI/UX Pro Max](https://github.com/nextlevelbuilder/ui-ux-pro-max-skill) as a design aid. Add it to the development environment in the UI milestone. It is not a runtime dependency, server module, or requirement for Payesh users.

The upstream project supplies design guidance and a generated installation for different coding assistants. Use its current documented installer, pin the selected version, and preserve required data/scripts. Do not copy only a `SKILL.md` while leaving its referenced resources behind. The package currently documents `ui-ux-pro-max-cli`; the old `uipro-cli` name is stale. Use the appropriate assistant target or its universal target, and inspect the dry-run first. Record the exact package version/source revision for reproducibility.

Read the installed skill's complete instructions. Use its dashboard design-system workflow and Svelte guidance. Save the project-specific design decisions under `design-system/payesh/MASTER.md`, with page exceptions only when justified. Follow actual generated resource paths rather than assuming a Claude-specific directory. Python/Node used for design/build tooling must not enter a production server dependency list.

Payesh-specific design brief:

> Design a modern, clear VPS monitoring dashboard for beginners managing 1–20 servers. Prioritize server health, readable numbers, synchronized resource/traffic charts, understandable logs, and safe optional controls. Use a restrained neutral palette with teal accents, accessible light/dark modes, mobile-friendly layouts, and low motion. Prefer clear labels and predictable navigation. Avoid decorative visuals that obscure monitoring data or add significant runtime work.

Treat generated recommendations as suggestions. Do not import an animation library, remote fonts, or a large component suite simply because a search result suggests it. The skill does not establish performance or accessibility compliance by itself.

### UI defaults and screens

- Use CSS variables for semantic colors, spacing, typography, borders, radii, and chart series. Neutral backgrounds, teal primary actions, and distinct warning/error meanings are the provisional visual direction.
- Use a system sans-serif stack and tabular numerals; a system monospace stack for logs/identifiers. Self-host any later custom font and justify its cost.
- Follow system light/dark preference initially; persist an explicit owner choice. Respect reduced-motion preferences. Keep routine transitions short and avoid background animations.
- Desktop navigation: Overview, Alerts, Extras, Settings. Server details contain Metrics, Traffic, Logs, and installed Controls. Collapse the fleet layer gracefully for one standalone server.
- First prototypes: onboarding, fleet overview, and server details with linked charts/logs. Use realistic fixture data, clearly marked as fixtures, for design work only.
- Present concrete previews for owner feedback. If design choices are delegated, proceed using this brief; do not block unrelated backend work waiting for cosmetic preferences.
- Overview shows unhealthy servers first, freshness, a few resource values, and monthly traffic progress. Advanced metrics appear on demand rather than turning the first page into a wall of charts.
- Distinguish healthy, pending, unreachable, stale, installing, failed, disabled, and unsupported states with text/icons as well as color.
- Charts support shared time selection, units, tooltips, gaps, and min/max context. Fetch an appropriate resolution for the displayed interval; never send millions of raw points to the browser.
- Virtualize long logs/lists, cap rendered chart points, lazy-load detailed screens, and pause nonessential subscriptions in hidden tabs. Selecting Live must not permanently enable high-frequency polling after leaving the screen.
- Use inline validation and progress for setup/modules/updates. Destructive control actions need a consequence preview and clear revert action. Normal navigation should not be interrupted by unnecessary confirmation dialogs.
- Preserve enough local UI state for refresh/back navigation. Do not put passwords, keys, API tokens, or log content in shareable URLs.

Visual acceptance: check widths 375, 768, and 1440 pixels, light/dark modes, 200% zoom, keyboard navigation, visible focus, screen-reader labels, contrast, reduced motion, long hostnames/IPv6 addresses, zero/huge values, empty fleets, partial failures, and slow networks. Labels must survive translated/long text even though English is the v1 interface language.

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

The detailed extracts in `plans/` are copies of this canonical handbook, not independent specifications. When an approved requirement changes, synchronize the affected packages and shared context in the same documentation change. The lead coordinates shared interfaces and accepts integration; package completion alone does not imply product completion.

The README should explain who Payesh is for, what the core installs, available roles, optional downloads, an actual quickstart, screenshots, measured costs, and important limitations. Avoid “better than Grafana” without a narrowly scoped reproducible comparison.

Prepare operating guides for HTTPS/proxy access, SSH installation, log retention, failed updates, backups/restores, node detachment, capability limitations, and safe uninstall. Document API compatibility and the module trust model for contributors.

Future OS-maintenance modules must have a separate specification for package updates, reboot policy, SSH recovery, and authorization. Do not reuse the Payesh updater as an arbitrary system-upgrade mechanism. Future MCP starts read-only over the existing API; it does not gain control powers automatically. Future continuous logging needs a separately measured storage/traffic budget.

Additional technical references used during planning: [SQLite deployment suitability](https://www.sqlite.org/whentouse.html), [Go release policy](https://go.dev/doc/devel/release), [Svelte documentation](https://svelte.dev/docs/svelte/overview), and [ACME client support](https://github.com/go-acme/lego). Read current primary documentation before implementation instead of relying on this plan for library-version details.

## 21. Copyable handoff prompt

This prompt is for the lead/single implementer with broader milestone authorization. Bounded workers must use the package-specific prompt in [plans/README.md](plans/README.md) and stop after their assigned handoff.

> Read MEMORY.md and the complete PLAN.md, then inspect the repository and applicable instructions. Implement the next incomplete milestone in order, preserving confirmed requirements and using the supplied defaults. Keep optional modules absent from base installs and keep UI assets absent from nodes. Do not confuse metric sampling with log events, a warning with enforcement, or a process starting with a healthy update. Use real Linux integration tests for privileged behavior and report measured performance, not guesses. Preserve unrelated work, keep the progress file accurate, and communicate briefly. Do not publish or operate on production servers without session authorization. When a milestone passes, continue with the next authorized milestone rather than declaring the whole product finished.
