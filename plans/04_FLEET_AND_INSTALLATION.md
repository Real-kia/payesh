# 04 — Fleet identity, transport and installation

Status: not started. Planning baseline: 2026-09-08.
Risk/assignment guidance: Hard; security review required.
Master milestone: M3.

Read [shared context](00_SHARED_CONTEXT.md) and [the package guide](README.md) before acting. This document is a future build assignment template, not permission to start coding in the documentation-only session.

## Prerequisites and ownership

**Requires:** 01 contracts and artifact trust primitive; 03 durable ingestion/local operations.

**Owns:** Enrollment, node transport, browser auth/setup, installer/SSH services, service definitions and capabilities.

Inspect actual repository conventions before selecting source paths. The lead owns shared contracts, schema ordering, dependency locks and integration unless explicitly delegated. A dependency may be replaced by an isolated test fixture while developing, but not for final integration acceptance.

## Work to implement

1. Implement protected owner setup/session access and separate node identities, enrollment CA, certificate renewal/revocation and one-controller ownership.
2. Implement node-initiated channel, durable acknowledgements/deduplication, bounded spool/gaps, priority/backpressure and heartbeat independent of sampling.
3. Implement direct role-selective installer and hub-assisted password/key SSH installation using common preflights. Verify host keys, keep credentials transient and require measurement arrival before success.
4. Reuse authenticated artifact verification now. The later updater adds recovery transactions; waiting for it does not justify unsigned installation.
5. Implement only necessary typed helper operations, systemd/OpenRC definitions and safe HTTPS/proxy/tunnel access. Preserve occupied ports, unrelated services and firewall state.
6. Persist job results and reject stale/conflicting reconnect commands. Send job/capability/error fixtures to 02.
7. Test 20-node protocol/load behavior with an honestly described topology; distinguish simulated agents from actual VPS/kernel tests.

## Acceptance gates

- Role inventories are correct; repeated/partial install preserves identity/data and reports failure accurately.
- Test wrong/revoked/expired identity, clones/competing hub, duplicate connections, restart/ack loss/retransmission/out-of-order data and spool overflow.
- SSH tests: password/encrypted key, changed host key, custom port, sudo failure, interrupted upload/full disk. No retained credentials.
- Owner setup cannot be claimed by unauthenticated visitor; node credentials cannot query other nodes or admin routes.

## Out of scope

SSH password vault, arbitrary remote admin, actual role-history migration (10), OS updates and production testing.

Reference extracts below sometimes mention neighboring packages. They explain integration requirements; they do not expand this worker's editable scope. Read-only inspection of neighboring interfaces is expected.

## Handoff and stop condition

Write `docs/handoffs/04.md` using the shared-context handoff checklist: baseline/files, implemented behavior, changed/consumed interfaces, actual test commands/results, measured or unmeasured costs, limitations, and next-package inputs. Request lead acceptance. Do not mark the full milestone/product complete or continue into another package on your own. If a prerequisite is missing, report exactly which contract, implementation or test environment is required.

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
