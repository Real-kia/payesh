# Release checklist

Use this checklist for each candidate. A feature is complete only when its
implementation and applicable acceptance evidence are both complete. A preview
may explicitly defer capabilities; a production release must document its scope
and close every gate for that scope.

Record the candidate revision, artifact digests, environment, exact command,
result, skipped tests and reasons, and evidence location for each gate. Store
raw evidence privately; publish only sanitized release notes. A skipped test,
short soak, fixture, or historical result is not a fresh acceptance result.

## 1. Define the supported scope

- [ ] Specify standalone/hub/node roles, architectures, distributions, init
  systems, and optional packages supported by this release.
- [ ] Declare limitations for remote package execution, fleet updates, role
  transitions, provider billing estimates, and unsupported control capabilities.
- [ ] Match Settings, Quickstart, API/OpenAPI, node protocol, and release docs to
  implemented behavior. Contract validation alone does not prove all routes are
  documented.

## Implementation gaps identified by source review

These require code changes, rather than additional evidence alone:

- [ ] Wire the database schema registry into actual store startup and update
  preflight. Refuse newer/invalid schemas before changing the database; advance
  each migration version with its data changes and verify a restorable backup
  before destructive steps. Exercise interrupted migration and downgrade refusal
  against real SQLite files. Store startup now uses the registry, with actual
  SQLite refusal, atomic version/data and verified-backup regressions. Signed
  candidate compatibility is wired before worker preparation, with exact index
  binding, expiry rechecks and installer capability checks. Recovery snapshots
  count toward pressure and capture is page bounded. Fresh installed-host
  migration and interruption acceptance remain open. The latest native
  standalone baseline exposed concurrent fresh-store initialization failure;
  the source fix passes repeated concurrency, storage pragma and cancellation
  tests. The rebuilt baseline subsequently passed isolated native systemd
  collection and cleanup on both architectures. A further actual-SQLite review
  reproduced interruption between fresh schema creation and vacuum setup that
  leaves incremental cleanup disabled after reopen. A durable fresh-initialization
  marker now makes that configuration recoverable; real SQLite cancellation,
  restart and concurrent-finalizer regressions pass. Verify the rebuilt native
  interruption path before this gate closes; baseline collection does not prove
  interruption or signed update recovery.
- [ ] Wire distributed role-cutover production adapters for history transfer,
  write freeze, authority switch, revocation and confirmation. The tested generic
  coordinator and local role conversion do not establish distributed execution.
  Opt-in migration-v2 snapshots now preserve retained replay and billing state;
  tail reconciliation requires an unchanged imported baseline and refuses lost
  authority. Actual SQLite regressions pass. Per-server ingest and command
  fences, a destination that verifies the frozen state before activating, and a
  crash-resumable store-level orchestrator pass locally and natively on both
  architectures, as do certificate, enrollment-token and node-identity fences and
  an authenticated mutual-TLS peer with resumable chunked transfer (over loopback),
  and the `payesh cutover` command set was run natively with the real binary in a
  no-network sandbox, and the signed systemd and OpenRC fixtures were re-run on the
  final schema-7 build. One real two-host run passed (arm64 to amd64 over the internet, throwaway test
  server, digests matched, source relinquished). Network failure injection, support
  for artifacts above 64 MiB remain required, and the peer's service definitions
  (`deploy/systemd/payesh-cutover-peer.service`, `deploy/openrc/payesh-cutover-peer`)
  both passed a real start/respawn/restart/stop run (OpenRC on Alpine, systemd on
  arm64).
- [ ] Complete authenticated remote optional-package installation/execution,
  including privileged kernel ownership and rollback on supported nodes.
- [x] Align Process Monitoring source-based installation on a local hub with
  the manager’s existing hub support. Signed local, URL and GitHub fixtures pass;
  invalid signatures, remote/nonlocal targets and other hub modules are refused.
  This source check does not establish deployed package lifecycle acceptance.
- [x] Authenticate legacy SSH bootstrap executables before the transport probe
  or installer runs. Pins derive from verifier-approved local artifacts and
  arrive over the verified SSH connection. Altered installer/core/agent fixtures
  are refused before execution. Hub/standalone roles use verified upload; uploaded
  executables are checked before probe/preflight and the trusted installer
  receives the web-directory tree pin. Fresh live installer acceptance remains
  required for the rebuilt candidate. Fallback now requires the exact safe
  terminal bootstrap result; uncertain launch/status/deadline and failed or
  unverifiable execution refuse uploads. An actual paused-download regression
  reproduced the former concurrent overwrite and passed after the fix. Refusal
  does not establish that a remote process has terminated or undo an already
  executing installation.
- [x] Refuse node SSH installation when the transport URL is missing. The
  reachability check requires an enrolled identity and hub trust anchor;
  non-node roles do not require a node probe. Helper and orchestration regressions
  cover the missing endpoint. Live tests require explicit transport inputs.

## 2. Verify the source candidate

```sh
go test ./cmd/... ./internal/... ./scripts/...
PAYESH_LIVE_TRANSPORT_ACCEPTANCE=1 go test -race ./cmd/... ./internal/... ./scripts/...
make lint
make contract-check
make web-check
make install-acceptance
make monitoring-soak-ci
make build-matrix
git diff --check
```

- [ ] All applicable checks pass with socket access; review skipped tests.
- [ ] CI executes frontend tests and opt-in local transport integration tests.
- [ ] Core and all shipped modules build for Linux amd64 and arm64.
- [ ] No internal notes, secrets, runtime state, real host details, or local
  absolute paths enter staged content or packaged artifacts.

## 3. Prove installation and independent ports on Linux

Use a disposable hub and at least two disposable nodes. Open the intended hub
ports before testing; nodes need outbound access to the transport endpoint.

- [ ] Fresh standalone/hub installation, owner login, HTTPS activation, and node
  enrollment deliver actual monitoring samples.
- [ ] Both node download/bootstrap and hub upload/fallback installation paths
  succeed when the TLS node endpoint is reachable.
- [ ] Block the node transport port, use untrusted TLS, and break DNS in separate
  runs: node installation fails before services are applied, with useful errors
  and no secrets in logs. Restore access and verify retry succeeds.
- [ ] Changing dashboard port leaves node endpoint and ingestion unchanged.
- [ ] Changing node port migrates an online node; verify samples continue without
  duplicate charging and the new endpoint survives node/hub restarts.
- [ ] Keep one node offline during migration, bring it back on the old endpoint,
  and verify it migrates before retirement becomes available.
- [ ] An older agent is reported as requiring an update; upgrading it completes
  migration. Old endpoints remain available until this is resolved.
- [ ] Occupied target port, blocked target port, invalid TLS, and failed endpoint
  persistence retain a working connection and show an error. Inject file and
  directory sync failures; endpoint migration and retirement must not report
  success before state is durable.
- [ ] Retirement is refused while any non-revoked node is pending. After all
  nodes migrate, retirement closes old node listeners and legacy dashboard node
  access; restart does not reopen them.
- [ ] Existing installations using dashboard node routes retain compatibility
  during upgrade and successfully migrate to the separate node port.
- [ ] Validate the advertised URL through actual firewall/NAT/proxy rules.
  Firewall rules are operator-managed, not changed by Payesh.

## 4. Verify security and browser behavior

- [ ] Login/logout, account password resets, session expiry/revocation,
  read-only mutations, owner-only functions, CSRF, and trusted proxy handling.
  Revoke a session while Settings notifications or storage is open: polling
  must show sign-in and stop protected requests. A failed logout must keep the
  active session visible with an error and allow a successful retry.
- [ ] Enrollment token reuse/expiry, revoked certificates, TLS renewal,
  unavailable certificate, and reconnect after network loss.
- [ ] Managed HTTPS issuance/renewal on the supported deployment path; test real
  DNS-01 if advertised. HTTPS login produces secure cookies.
- [ ] On desktop and mobile, verify enrollment, fleet/detail views, charts,
  logs, settings, migration errors, package lifecycle, and update progress.
  Fresh localhost browser checks with actual TLS agents/API pass occupied-port,
  offline pending, retirement refusal, returning-node migration, retirement and
  saved-endpoint restart states. Existing sample rows remain unchanged and
  desktop/mobile layouts fit. Physical fleet and the other flows remain open.
- [ ] Verify empty/offline/error/retry states, keyboard navigation, focus,
  contrast, reduced motion, and screen-reader announcements.

## 5. Verify services, data, and recovery

- [ ] Systemd and OpenRC install/start/stop/restart/reboot on supported hosts;
  include amd64 and arm64 and distinguish image smoke from service acceptance.
- [ ] Actual hub plus agents run for 24 hours with collection, queries, derived
  processing, retention, and notifications active. Record data loss, ingestion
  errors, latency, DB/WAL size, CPU, RSS, and I/O.
- [ ] Run the separate store soak and a representative 30-minute resource
  benchmark with optional modules disabled/enabled. A store-only soak does not
  measure the running fleet.
- [ ] Exercise disk-full/storage pressure, log rotation, process interruption,
  backup restore, and post-restart gap/replay handling on disposable storage.
- [ ] Update an installed release to the candidate; simulate corrupt downloads,
  failed health checks, interrupted activation, and watchdog rollback.
  Current-source isolated signed-systemd fixtures (disposable test trust,
  nested systemd namespaces on real arm64 and amd64 hosts) now pass signed
  success, failed-health rollback with the failed generation preserved, and
  expired-request recovery with zero downloads. Each case ran to a protected
  terminal result with exact cleanup, installed services and a running endurance
  unit unchanged. An earlier failure of the success case was a fixture race: the
  update worker deliberately exits after a committed update and systemd restarts
  it after five seconds, so the fixture now waits for the service instead of
  checking instantly. The same four cases (success, failed-health rollback,
  expired-request recovery and a hard kill of the worker during activation) also
  passed on both architectures for a schema-7 candidate upgrading from a schema-6
  baseline, including rollback of the migrated database to schema 6. A hard kill of the whole worker control group during
  activation is also recovered on both architectures (baseline restored, failed
  generation preserved, collection continued). Still open: other interruption
  phases, resource faults, host reboot or power loss, production signing trust, and exercising the browser
  update route end to end through these fixtures. The route itself uses the
  protected worker transaction with durable bounded intent; authenticated API and
  full race regressions pass.
- [ ] For selected-fleet updates, verify hub-first ordering, one-node-at-a-time
  execution, offline/incompatible/mixed outcomes, expiry, replay, and controller
  restarts. Node delivery acceptance alone must not complete a job: confirm its
  durable worker result, exact freshly installed version/service health, and
  authenticated hello after restart. Reject preview-mode fleet updates.
- [ ] Exercise the installed root-worker path with real managed binaries,
  configuration, services and SQLite data. Failed post-install health must
  restore the exact previous version, configuration, units, ownership and data;
  interrupted prepared/activating/restoring phases must recover, and interruption
  after commit must not repeat activation. Incomplete restore retains intent.
- [ ] Before rollback, retain candidate-interval database/WAL/SHM, spool and replay
  identity files. Inject unsafe files, insufficient space, archive tampering,
  and interruption between preservation and restoration; no restore may proceed
  without verified retained evidence. Inject a partial service-start failure after
  the restored generation accepts a sample; recovery must retry startup without
  copying the previous snapshot again or losing that sample. Review how preserved samples will be
  reconciled; retaining an archive does not automatically restore those samples
  to the running dashboard. Older restoring journals without preserved data
  require operator recovery rather than recapturing partially restored files.
- [ ] Verify root-protected role/init metadata and authoritative result storage;
  forged service-account state/results must not bypass verification or report
  success. Existing installations missing protected scope fail closed.
- [ ] Reject oversized snapshots, insufficient database/restore headroom and
  exhausted retained archive budgets before services stop. Concurrent growth
  must remain bounded; capture failure restarts old services. Record terminal
  archive cleanup; never remove active or unresolved recovery snapshots.
- [ ] Cancel a rollout immediately before transport dispatch and verify no queued
  derivative activation leases. Test expiry/revocation and preserve delivery of
  ordinary module actions. Report separately any node activation already started.
- [ ] Convert a disposable hub/standalone to node: verify reachability before
  mutation, inject service/write failure, and interrupt recovery. Restore prior
  managed files and active services while preserving database/logs/unrelated
  data; reject unsafe or incomplete recovery journals.
- [ ] Confirm data-preserving uninstall, explicit data removal, repeated
  uninstall, and absence of unexpected units/listeners/owned kernel state.

## 6. Verify optional packages and accounting

- [ ] Process package signature rejection, install/enable/disable/remove,
  unexpected-exit recovery, restart restoration, proc permission limits, and
  unavailable remote target behavior.
- [ ] CPU quota enforcement/revert with PID reuse and cgroup delegation.
- [ ] Bandwidth enforcement/management exclusions, watchdog rollback, measured
  throughput, and collector-driven quota overshoot under load.
- [ ] Port Traffic accounting on every advertised topology: host, forwarding,
  NAT, bridge/container, or tunnel; foreign kernel state remains untouched.
- [ ] Provider billing comparison uses the same interfaces, direction, period,
  reset boundary, and units, with an agreed tolerance and visible coverage gaps.
- [ ] Fresh notification delivery, outage/retry/recovery, maintenance suppression,
  and duplicate prevention use an authorized test destination.

Network topology, tc, nftables and throughput acceptance automatically run
inside fresh outer network and mount namespaces, with a private namespace mount
directory. If isolation is unavailable, they fail closed rather than changing
host forwarding or kernel rules. Namespace bridge/NAT tests do not prove Docker
integration or measured Port Traffic accounting. CPU acceptance separately uses
a disposable delegated systemd unit; it must not alter the root hierarchy.

## 7. Build and approve the release

- [ ] Reproducible archives contain the exact tested web/core/module artifacts;
  validate checksums and manifest inventory.
- [ ] For a production core release, provision an external signing key and
  distribute the reviewed public trust anchor. Verify signatures at the actual
  installation/update boundary; adjacent checksums alone are insufficient.
- [ ] Publish the canonical manifest signature and the release/key-ID-bound
  `SHA256SUMS.sig` covering the exact archives and bootstrap script. Verify that
  Go authenticates `install.sh` before execution and the trusted shell verifies
  archives before extraction. Missing anchor/signature, wrong key, altered
  script/index/archive, and cross-version replay must fail closed.
- [ ] Configure production trust only through root-managed worker environment
  and protected public-anchor files/directories. Test both systemd and OpenRC;
  the service-account environment must not select preview mode or a new anchor.
- [ ] Record how the initial bootstrap script and public anchor were obtained
  independently. A first unsigned `curl | sh` is not independent authentication.
- [ ] Validate a signed clean-host install and installed-version upgrade against
  the exact candidate artifacts, including rollback and data compatibility.
- [ ] Check preview versus production labeling and release notes against the
  evidence. Explicitly list deferred capabilities and skipped environments.
- [ ] Owner approves publication separately. Building or reviewing a candidate
  does not authorize tagging or pushing it.

## Current evidence checkpoint — 2026-10-04

The current source remains a preview awaiting production acceptance. The table
records observed scopes; it does not check broader gates above automatically.

| Area | Verified scope | Remaining acceptance |
| --- | --- | --- |
| Source checks | Full current-source Go race suite with local TLS transport enabled passed again after the later recovery fixes. Vet, frontend check with zero warnings, four frontend tests and contract checks also passed at the fresh review checkpoint. Lint/vet, contracts, frontend check/build and four frontend tests passed at their preceding recorded checkpoints. | Remote CI/publication require separate authorization; rerun applicable checks after further source changes. |
| Linux build matrix | Six core tools and four optional modules built for each of amd64/arm64; all 20 ELF architectures verified. | Exact release packaging/signing and supported-host lifecycle. |
| Migration persistence | Five native Linux tests plus three subcases per architecture passed, without skips; failure paths retain listeners/fallback and refuse acknowledgement/retirement. | Real matched-fleet ports, firewalls, offline/older agents, restarts and physical power loss. |
| Real fleet transport fixture | Exact candidate hub and two agents passed TLS enrollment, dashboard bind independence, occupied-port refusal, online/offline migration, pending retirement refusal, durable restarts and sample/identity continuity on both architectures. Disposable loopback cleanup and installed Payesh preservation passed. A separate bounded node-to-hub TCP probe reached the dashboard port and intended transport port through the real network, then removed its listener. | Full TLS/WebSocket through the real firewall/NAT, older agents, existing legacy dashboard routes, Settings installer flow and physical host restarts. Arm64 strict controller failed because an unrelated already-restarting container drifted; scoped fleet checks passed. |
| Historical agent and dashboard transport | Both native architectures passed a real v0.2.18 source-built server/agent on dashboard TLS, candidate server upgrade with continued old-agent collection and update-required status, retirement refusal, pending restart, identity-preserving agent upgrade and migration, retirement of dashboard node routes and the old listener, and persistent migration after restart. Each retained 15 unique samples across three contiguous collector epochs. Task cleanup, installed services and running endurance units were preserved. This scenario also passed against the later rebuilt agent/server on both architectures. | These pinned offline historical-source test builds establish process compatibility, not owner-signed release provenance or actual installed-host upgrade/firewall/boot acceptance. AMD strict preservation passed; ARM strict comparison failed solely because an unrelated container already restarting incremented its restart count. The rebuilt CLI still requires fresh native worker-recovery acceptance; historical transport checks exercise the rebuilt agent/server. The browser-tested web remains unchanged. |
| Systemd services | The recorded candidate’s generated agent/server units passed isolated ingestion, health/dashboard, owner/reader sessions, stop/restart and cleanup on both architectures. Installed service snapshots stayed unchanged. | Fresh generated-unit runtime checks for the rebuilt recovery generation, actual installed candidate and host boot/reboot. |
| OpenRC services | Current-source candidate passed native Alpine Docker OpenRC clean install, service start/stop/restart, ingestion, health/dashboard and data-preserving uninstall on both architectures; task containers/images were removed and existing containers and installed Payesh services stayed unchanged. | Host boot/reboot and signed OpenRC role lifecycle on the current source. |
| Recovery | The pre-recovery-fix candidate passed disposable node/standalone filesystem installation, retained failed-generation data and restart-retry safeguards on both architectures. | Fixed-source interruption acceptance is recorded below; node/hub activation and power loss remain open. Retained candidate-interval samples require manual reconciliation. |
| Authentication/UI | Built-browser Settings401 shows sign-in and stops protected polling; logout503 retains the active session and permits successful retry. Desktop/mobile dialog focus and accessibility-tree checks passed. The final staged web ran with an actual server/API and live collector: owner login/logout, detail CPU/memory charts, two users, read-only controls, all five loaded Settings sections with no overflow at five widths and dark mobile rendering were checked. Exact fixture cleanup and installed-service preservation passed. Local checks also covered empty-fleet onboarding and the single-method SSH form. | Live installer/migration and package/update flows, screen-reader and assistive-technology acceptance remain open. |
| Test-signed CLI bootstrap | Earlier candidate: seven clean-container cases passed per architecture using disposable trust. Shell rejected altered checksum/signature/archive/wrong-key inputs; separate validator checks rejected altered bootstrap/manifest before invocation. | Exact-current owner-signed service-role installation, upgrade and rollback. Test keys do not establish production trust. |
| Test-signed root worker | Current-source standalone role passed six native Alpine/OpenRC cases per architecture using disposable trust and a synthetic previous version: signed upgrade committed exact target binaries and restarted the worker; injected health failure rolled back and retained candidate samples; invalid signature, expired request, unsafe trust anchor and unsigned preview all refused activation. Task cleanup and installed Payesh preservation passed. On amd64 the six cases ran as two runs, with a fixture wait for request removal added between them. | Owner-controlled production trust, host boot/power loss, storage pressure during activation and a live fleet. The arm64 strict whole-host comparison failed only because an unrelated container was already restarting; scoped Payesh checks passed. |
| Test-signed hub/node workers | Current-source actual generated OpenRC root workers passed hub and node upgrade plus worker-health rollback on both architectures (four cases each) using disposable trust, a synthetic previous version, loopback TLS enrollment with a real peer hub for nodes, retained identity and collection after each terminal result. All cases and task cleanup passed and installed Payesh services were unchanged. On arm64 the strict whole-host comparison failed only because an unrelated container was already restarting; scoped Payesh checks passed. | Owner-controlled production trust, standalone-role signed OpenRC rerun on this source, host boot/power loss, actual live fleet and activation-time storage exhaustion. |
| Actual low-space preflight | Current-source generated standalone OpenRC root worker on both architectures refused a valid signed intent on a real private 32 MiB backup filesystem before any snapshot, archive download, activation or service stop. The worker first authenticates the release metadata (checksum index, manifest and installer, five small files) and then checks free space; no release archive is fetched on refusal. Original worker/agent/server processes, artifacts, identity/config and all original metric rows were retained; collection continued. | Activation-time disk exhaustion, failed restore/publication, storage pressure across supported roles/init systems and physical storage faults. Arm64 strict controller failed due to unrelated preexisting container restart drift; scoped checks passed. |
| Interrupted activation | Before the later recovery fixes, pinned native OpenRC fixtures passed activating and restarting worker SIGKILL on both arm64 and amd64. All four verified correlated rollback and request removal, restored baseline executable generation, archived candidate samples, continued collection, and a distinct fresh signed update that committed and restarted into the target executable. Restarting preserved rows accepted after restoration without another stop. Independent task cleanup and installed Payesh preservation passed. | Fresh acceptance for the rebuilt recovery generation, other interruption phases, systemd, host reboot/power loss and production trust. Both amd64 strict host results passed; arm64 strict host results failed only because an unrelated preexisting container drifted. |
| Kernel controls | Earlier isolated Linux probes exercised CPU quota, traffic shaping/watchdog, scoped nftables, packet/byte accounting and network topologies, with foreign host state preserved. | Full signed optional-package lifecycle and provider-aligned billing. Namespace probes do not establish Docker integration or every topology. |
| Optional process module | Current-source disposable native Linux lifecycle tests passed on amd64 and arm64: signed staging, actual samples, unexpected exit, temporary 503, a different restarted PID serving 200, unchanged enabled revision, restoration, disable and removal. Local module/process-monitor race tests passed too. Native fixtures used a disclosed pinned prebuilt module for the exact test build invocation; installed services/endurance PIDs were unchanged and task containers removed. | Broader proc permission limits, full signed lifecycle for other modules and accounting requirements remain open. AMD strict preservation passed; ARM strict comparison failed solely for an unrelated preexisting restarting container's state drift. |
| Resource/endurance | Earlier bounded 30-minute synthetic candidate baseline passed with optional modules disabled. The 24-hour store soak completed 86,400 attempted/inserted samples with zero duplicates, ingest/prune/storage errors, observed retention, 288 prune runs and clean teardown. A fixed-candidate isolated amd64 fleet smoke ran two TLS agents for 120 measured seconds: 62 samples, 168 authenticated queries with zero errors, a drained derived queue, traffic ledger/rollups, six verified local HTTPS alert receipts, resource sampling and clean task teardown. The isolated fleet runs exceeded 24 hours on both architectures, with zero query errors and completed cleanup, but both failed the coverage-gap assertion. Installed services remained unchanged. | Diagnose gap reasons, distinguish normal retention from ingestion loss, and obtain a passing fresh run; representative optional-module load remains open. These failed runs do not close the gate. |

Approved root-only backups were verified on both original servers. They do not
replace the fresh stopped-boundary snapshot required for activation. The
fixed-source candidate with the final current UI has been privately staged
and independently hash-verified on both servers in new root-only directories.
All twelve native core binaries, both ELF architectures, the current web digest
and complete archive inventories matched the locally rebuilt pair. The previous
stages and installed services stayed unchanged. Earlier acceptance applies only
to the source scope it exercised; the fixed worker interruption cases above
exercise the new core binary directly. Production activation and owner signing
trust remain open. The final web build was placed in new root-only directories
on both original servers after local and remote pin checks. Its populated
browser flows are recorded above. After native module acceptance, the later
process-start observer was included in a frozen current-source build. New
root-only stages on both servers passed bounded chunk transfer, assembled
archive hashes, all seven artifact pins and preservation checks. Only the
server executable changed; the browser-tested web and five other cores stayed
identical. Runtime evidence remains scoped to the inputs it exercised. No Git
staging, commit, push, tag or publication was performed.

A later worker review corrected recovery intent loss after authorization expiry
or missing trust, and restored-result handling after completed rollback. Focused
regressions and the complete current-source race suite with local TLS acceptance
passed, as did vet. Independent rebuilt-candidate staging passed on both native
architectures. Both rebuilt native workers passed all three fresh disposable OpenRC recovery
cases: expired intent after interruption, restoration repair with missing-trust
preservation, and completed rollback reconciliation without trust followed by
worker reload. Each case retained candidate samples, restored baseline collection
and completed a distinct fresh signed retry. Independent watchdog cleanup passed
and installed Payesh services and endurance processes were preserved. AMD64’s
overall controller passed; ARM64’s overall controller failed solely because an
unrelated preexisting container restarted during the run. Strict duplicate-worker
checks remained enabled; the earlier duplicate observation did not recur and its
cause remains unproven. Other interruption phases, systemd recovery, host
boot/power loss and production trust remain open.
Shared package changes alter the CLI, agent and server binaries, so earlier
runtime checks remain scoped to their recorded inputs.

The subsequent browser review fixed installer retry handling: a rejected enqueue
now retains the already-created server, and retrying a failed installation opens
that server’s installation form. The UI marks installation as running only after
the job is accepted. Final-bundle Chromium checks verified unchanged server count
and identity after an actual enqueue rejection, plus retry routing using a
simulated terminal job. All five Settings controls remain fully visible when
focused by keyboard at narrow mobile widths. Real SSH installation and update
execution are separate acceptance gates. The refreshed UI is packaged locally and privately staged on both servers;
independent final inventory, artifact hash and service preservation checks passed. No live activation occurred. Earlier native fixtures retain their
recorded web generation.

The release signer now normalizes manifest timestamps consistently with the
verifier before signing. Regression tests and actual signer-to-validator CLI
checks cover UTC offset spelling, fractional trailing zeros, and nonzero offsets.
Native signing helpers were rebuilt and independently verified on both
architectures. Fresh disposable systemd baselines ran on both architectures:
real systemd PID 1 started the generated agent/server units and root worker;
collection advanced from two to eight samples. Independent checks verified
process ownership, executables and isolated namespaces. Both baselines stopped
cleanly, with task units, cgroups and namespaces absent and installed services
and ongoing endurance tests unchanged. These baselines used the recorded earlier
core build and submitted no signed update; they do not establish signed systemd
activation, rollback or recovery. Fresh OpenRC recovery cases and watchdog
cleanup completed on both architectures, with the preservation distinction
recorded above.

The latest source review fixed the SSH schema check to use installation’s sudo
privileges. Installer and release/update race checks and lint passed afterward;
disposable installation, the short store soak and both Linux build architectures
passed their recorded scopes. The current schema startup, authenticated candidate
preflight, bounded recovery snapshots and storage UI differ from earlier native
fixtures. Fresh signed systemd lifecycle evidence for this exact generation is
still required; building pinned native inputs does not establish activation.
