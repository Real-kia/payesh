# Implementation status

Updated: 2026-10-04

The source is a working preview, not an accepted production v1. Implementation,
automated verification, and operational acceptance are separate claims. Use the
[release checklist](RELEASE_CHECKLIST.md) for the evidence required before a
release; the dated package notes below describe earlier checkpoints.

## Current implementation

- Core monitoring includes Linux collection, SQLite history/rollups, bounded
  logs, retention, storage settings, traffic periods, forecasts, alerts,
  incidents, and notification adapters.
- Browser authentication includes durable accounts/sessions, CSRF protection,
  owner-only account management, and read/edit permissions. Owner and read-only
  sessions can revoke their own session; expired-cookie and replay tests pass.
  A failed logout keeps the session visible with an error and permits retry.
  Shared confirmations use native modal focus handling, and the sign-in screen
  hides background controls from keyboard and browser accessibility trees.
  Desktop/mobile Chromium checks passed; full assistive-technology acceptance
  remains open. Settings polling recognizes session expiry and shows sign-in;
  periodic protected requests pause while signed out.
- Installer retries reuse the existing server after enqueue rejection or a
  failed job. Pending status is retained until enqueue succeeds. Final-bundle
  Chromium checks verified an actual enqueue rejection without duplicate
  server creation; terminal-job retry routing used simulated API responses.
  Mobile Settings focus scrolling keeps all five controls visible at narrow
  widths. Real SSH installation and browser updater execution remain gates.
- CLI-only installation supports hosts without an init manager and records
  that state in protected installation metadata. Service roles require systemd
  or OpenRC; fleet activation rejects CLI-only installations.
- Fleet transport includes TLS WebSocket enrollment, identity renewal,
  revocation, reconnect/spool replay, and typed durable actions. Dashboard
  access and node transport now use independent ports. Capable agents verify
  and persist port migrations; old endpoints remain until guarded retirement.
  Endpoint and hub state sync file contents and directory entries before
  success. Persistence failures refuse acknowledgement or retirement.
- SSH installation includes staged artifact checks, pinned host keys, transient
  credentials, download/upload fallback, and a node-side TLS reachability check
  before service installation. Latest transport regression tests passed locally
  and in disposable Linux amd64/arm64 loopback fixtures. Matched two-host deployment,
  firewall and installer acceptance remain required.
- Optional packages include Port Traffic, CPU Controls, Bandwidth Controls, and
  Advanced Process Monitoring. Process Monitoring has a pinned signed package;
  remote-node package execution remains unavailable.
- The dashboard can request local and selected-fleet core updates. Server startup
  now wires a durable executor that delivers authenticated typed update intent
  to node-owned root workers. Intent/result records survive restarts; success
  requires the exact installed release and required service health, followed
  by the node's authenticated hello. Offline, incompatible, failed and pending
  targets remain explicit. This implementation has automated regression
  coverage; owner-signed multi-host rollout and recovery acceptance remain open.
  Parent cancellation atomically prevents queued derivative activation, and
  transport leasing excludes private rollout orchestration records. Activation
  already started on a node may continue after cancellation.
- Fleet root workers use protected installation role/init metadata and
  root-owned authoritative results outside the service-writable intent inbox.
  Installed-layout rollback restores managed binaries, web assets, configuration,
  service definitions, installation state and a consistent SQLite backup after
  failed activation or health checks. Recovery journals survive interruption;
  incomplete restore retains intent. Before restoration, rollback retains the
  failed generation's raw SQLite database/WAL/SHM, default agent spool and node
  replay identities in a private, bounded archive. Recovery verifies its hashes
  and refuses restoration when capture fails. This preserves recovery evidence;
  automatic reconciliation into the restored database is not implemented.
  Storage preflight runs before stopping
  services, with bounded copying, a 2 GiB snapshot ceiling, an 8 GiB retained
  archive budget and restore/database headroom. Completed archives require
  operator cleanup; active or unresolved snapshots are never auto-deleted.
- Explicit local hub/standalone-to-node conversion verifies destination
  reachability and snapshot size/headroom before mutation, and journals managed
  binaries, configuration, identity and service definitions for recovery.
  Failure or restart recovery restores the prior role and its previously active services while
  preserving database, logs and unrelated data. Distributed role-cutover
  orchestration and remote optional packages remain incomplete.
- Shell installation and CLI/root-worker updates default to production signature
  verification using an externally configured Ed25519 public anchor and key ID.
  The offline owner signer signs both the canonical JSON manifest and a
  release/key-ID-bound checksum index containing the bootstrap script. The Go
  updater authenticates that script before execution; the shell authenticates
  archives before extraction/execution. Legacy unsigned releases require
  explicitly labeled preview mode. Test-signed fixtures pass; the actual owner
  anchor, signed publication and clean-host lifecycle are still external gates.
  The signer normalizes timestamps to the same representation as the verifier;
  timestamp regression tests and real signer/validator CLI checks pass.
  The signed process package does not establish trust for the core release.

Fresh source evidence through 2026-10-03 includes focused signature/bootstrap and
fleet worker and installed-layout recovery/storage/cancellation tests, the full
product race suite with local transport acceptance enabled, disposable Linux
transport regression, and the Linux
amd64/arm64 build matrix: six core tools plus four optional modules per
architecture. The refreshed arm64 synthetic candidate resource baseline completed
30 minutes with zero sampler errors, verified 15-second sample ingestion and
clean child shutdown; optional modules were disabled and resource caps applied.
Disposable real-systemd callbacks around activation/recovery also passed on
both architectures, with previous fixture data restored and failed-generation
samples retained. Exact generated agent/server units subsequently passed isolated
systemd ingestion, session and stop/restart checks on both architectures. A
native Alpine container exposed and verified fixes for OpenRC preparation
ownership and environment loading; clean installation, collection, HTTP health,
restart and data-preserving uninstall passed on amd64 and arm64. These runs isolate filesystem and
networking and do not establish host boot or live fleet lifecycle. The store-only
24-hour soak completed with 86,400 inserted samples, zero ingest/prune/storage
errors and observed retention. Separate actual-fleet runs later exceeded 24 hours
on both architectures but failed their coverage-gap assertion. They are not
passing fleet-endurance evidence. These checks do not
accept a production release.

Before the later recovery fixes, pinned signed root-worker verification passed six disposable
native OpenRC standalone-role cases on each architecture. The actual generated root service and
real Go/curl TLS clients installed exact target artifacts, committed a correlated
job result and restarted into the target CLI. A supervisor-status fault exercised
worker-health rollback, preserved the first two original metric rows, and retained
candidate-period samples plus the full transaction baseline in the failed-generation
archive. Four negative cases rejected invalid signatures, expired requests, unsafe
anchors and valid preview mode. Test-only trust and a synthetic
previous version bound this evidence. Task resources were removed and installed
Payesh services stayed unchanged. The arm64 strict controller failed because an
unrelated, already restarting container drifted; all scoped worker cases passed.
Production trust, other service roles, real fleet activation, interruption/storage
pressure and host reboot/power-loss acceptance remain open.

A later source review fixed recovery intent loss when authorization expires or
release trust becomes unavailable, and restored-result handling after rollback.
The complete race suite with local transport acceptance and focused regressions
passed for that source. Its rebuilt CLI, agent and server have been independently
verified in private staging on both architectures. Fresh native recovery tests
remain pending; earlier worker results apply to their recorded build.

UI review inspected the existing live desktop/mobile interface and local
candidate fixtures after responsive navigation, focus and chart/theme fixes.
The 390-pixel layout was checked for overflow. These observations do not prove
live candidate deployment, full workflow or assistive-technology acceptance.

Remaining operational evidence includes supported-host service/boot compatibility, browser
interaction/accessibility, current managed TLS and firewall deployments,
long-duration retention/resource pressure, and provider/topology accounting.
See [known limitations](KNOWN_LIMITATIONS.md) for scope boundaries.

## Historical implementation evidence

### Foundation and package checkpoints

- Recorded a Go 1.26 module and four separate build targets: `payesh-agent`,
  `payesh-server`, `payesh`, and `payesh-privd`.
- Added a dependency-free shared contracts package with protocol/version,
  sample, error, and durable-job request types.
- Added the initial REST/OpenAPI contract under `/api/v1`.
- Added node, helper, module, release-trust, and data-model contracts.
- Added a Makefile with format/vet/test checks and Linux amd64/arm64 build
  matrix.
- Added typed durable-job/action records, decimal-string 64-bit wire fields,
  validation helpers, and fixtures for large counters, gaps, expired jobs, and
  partial-fleet results.
- Added a proposed ownership ADR and a committed npm lockfile with CI `npm ci`
  verification.
- Incorporated review fixes: protected-route session authentication and 401
  responses, all planned API resource families, bounded query pagination/range,
  idempotent job cancellation, explicit source-clock skew semantics, aligned
  helper targets, and CI lint enforcement.
- Second review fixes add bounded uint64 validation, required metric values,
  explicit rollups/traffic allowances/live logs/alert history/maintenance,
  complete module and policy safety flows, update preflight and machine
  selection, backup/role-transition contracts, full release metadata, required
  job expiry, and the declared distribution-image CI matrix.
- Recorded the exact CI representatives and test scope in
  `docs/support/LINUX_MATRIX.md`.
- Added a minimal Vite/Svelte shell with `svelte-check`, a real frontend build
  target, and YAML-parsing OpenAPI/reference validation in CI.
- Added CSRF requirements for browser mutations, page/cursor bounds for list
  resources, strict release-field alignment, typed node/helper payloads, and
  explicit request/response byte limits.
- Added the minimal Svelte/Vite shell, `svelte-check`, real `web-check` build
  target, parsed OpenAPI reference checking, and distro smoke execution of the
  Linux agent artifact.
- Completed package 02 Checkpoint A with responsive onboarding, fleet overview,
  and server metrics/traffic/logs previews. Fixtures are loaded only through a
  development/explicit-preview adapter; production builds render an API-needed
  state instead of fabricated data. The preview covers all eight server states,
  capability-gated uPlot range selection, local navigation/theme persistence,
  validated access and optional enrollment steps, bounded log metadata, and
  explicit unavailable states. The second review pass also fixed chart data
  boundaries, retry behavior, contrast, readable time axes, and long-name
  wrapping. Design decisions are recorded under
  `design-system/payesh/MASTER.md`.
- Third foundation review fixes separate node-observed samples from hub-stamped
  receipt records; specify JCS/Ed25519 detached release signatures; validate
  module protocol/hello inventories and coverage gaps; make build loops
  fail-fast; correct high-range uint64 validation; and add server
  label/freshness, audit-event, and login-throttle API contracts.
- Implemented package-03 Checkpoint A: Linux proc/syscall collectors for CPU,
  memory/swap, load/uptime, disk capacity/inodes/I/O, and network counters;
  hub-stamped SQLite ingestion with deduplication, coverage gaps, rollups,
  traffic periods, persisted rollup queries, cursor queries, retention/storage-
  pressure checks, bounded file/journal logs and live tails, protected local API
  reads, CLI commands, and systemd/OpenRC service definitions. Omitted agent
  epoch flags generate a fresh epoch per process start; ingestion materializes
  affected minute/hour rollups and preserves cross-bucket counter intervals.
  Late endpoint buckets are recomputed; raw ingestion acknowledgements remain
  successful when derived writes are pressured, with a durable rebuild queue
  for recovery. Current billing periods remain writable under history pressure,
  all CPU counter components are checked for resets, partial log lines retain
  their cursor, and routine agent samples are excluded from service logs.
  The collector is SQLite-free so the agent build graph remains independent.
  Capacity counters are classified as gauges for rollups, agent server IDs are
  persisted per installation, logical interfaces are excluded from billing,
  retention deletes the exact rows selected for gap creation, and coverage-gap
  pagination is independent from raw sample pagination. Store-level log
  redaction, NULL-heartbeat decoding, post-decode receipt stamping, explicit
  missing-field validity, and same-inode rewrite fingerprints are covered by
  regression tests.
  The installed agent service now feeds a bounded streaming ingest process,
  the server schedules bounded retention/prune work, adjacent-bucket gauge
  weighting is preserved, rollup failures are surfaced as retryable, raw
  coverage includes validity-only metrics and gaps, authoritative billing
  interfaces are configurable, and filtered/persisted log capture is exposed
  through the CLI.
  Service registration is insert-only during bootstrap, ingestion refreshes
  heartbeat/freshness, architecture defaults to the running binary, storage
  pressure evicts eligible history before the ceiling with WAL/incremental
  vacuum recovery, and on-demand file-log queries enforce the one-MiB page
  bound. Coverage gaps are scoped to requested raw sample ranges.
  Retention now emits only contiguous deleted-sequence gaps and preserves
  unknown timing bounds. Freshness derives from heartbeat age, with follow-mode
  ingestion sending an independent 15-second heartbeat. Managed storage counts
  configured operational-log directories; historical file cursors resume at
  source offsets, journald units support bounded live tails, and raw metric
  pages enforce a byte-aware continuation bound.
- Package-04 Checkpoint A adds SQLite-backed restart-safe browser owner setup/
  login/logout with PBKDF2 password records, transport-aware session cookies
  and bounded, expiring CSRF/login throttling (with trusted reverse-proxy
  client keys); auth persistence keeps only password/session digests and
  bounded CSRF/throttle metadata, prevents setup from reopening after restart,
  and fails closed on corrupt/oversized state; CA-owned single-use enrollment
  tokens with one certificate per server now persist their CA key/certificate,
  pending token digests, certificate ownership and revocations in the same
  restricted store (without retaining token cleartext or node private keys);
  node certificates support revocation that closes live channels; a
  one-active-connection hub ingestion boundary with compatible hello
  validation, sequence-zero-anchored contiguous durable acknowledgements and
  bounded typed connection jobs; a 10,000-row durable job store with
  operation idempotency, revision-CAS transitions and cancellation, plus
  authenticated job read/cancel routes; and side-effect-free role/platform/init preflight checks
  against the pinned release matrix that require systemd or OpenRC for service
  roles. This is not the
  package-04 acceptance gate: live enrollment bootstrap/transport wiring, authenticated
  TLS/WebSocket connection wiring, automated renewal/recovery orchestration, SSH/direct installers,
  and Linux acceptance remain outstanding.

- Package-05 Checkpoint A adds calendar/timezone traffic allowances with
  durable period identity and restart-safe schedule transitions, selected
  interface/direction counter deltas, 24-hour-first bounded as-of recent-rate
  forecasts with a seven-day fallback,
  compare-and-swap configuration revisions, editable
  timestamp/coverage-based alert rules, hysteresis and sparse-sampling
  precision warnings, maintenance/outage notification suppression with durable
  reminder cadence, grouped bounded incident snapshots, HTTPS webhook/Telegram
  delivery with bounded retries, authenticated browser routes for
  traffic/rules/history/maintenance/incidents, and durable post-process replay
  plus exactly-once traffic charging. Forecasts fail closed when a target
  period's continuity is `gap` or `uncertain`; historical allowance/interface
  snapshots are preserved during delayed processing and queued work pins
  period history until it drains. Derived processing is sequence-frontier
  aware, gap-only batches wake deferred work, and concurrent drains/state
  transitions are serialized. Complete counter intervals are split across
  calendar/DST boundaries and explicit owner-started periods transactionally;
  deferred identities are skipped so ready streams make progress. Alert rules
  carry durable effective-at boundaries; legacy generic traffic starters are
  disabled and terminalized; a bounded retirement authority rejects unseen
  epochs at saturation while preserving exact replay markers. This is not the
  package-05 acceptance gate:
  precision-safe provider billing comparison tooling now exists, but the
  authoritative provider total and 24-hour resource-soak evidence remain
  unrun. A real
  Linux file source passed bounded metric/log incident snapshot persistence.
  Real Telegram and signed HTTPS webhook endpoints passed;
  an external HTTPS outage/recovery run passed two deliberate 503 responses,
  bounded retry/backoff, and third-attempt recovery. A 121-second Ubuntu retention soak
  accepted 4,280/4,280 samples without ingest/prune/storage errors.
  Fresh collector epochs mark sequence-zero usage uncertain when prior epoch
  metadata exists; explicit period restarts close active predecessors
  atomically. Forecast reads retain a bounded newest tail and accept ordinary
  5s/10s cadence edges. Notification cadence advances only after successful
  delivery, and alert-rule creation/reconciliation is bounded at 20,000 total
  and 4,096 per-server rows, including disabled rules.

- Package-06 Checkpoint B adds the signed optional-module framework plus a
  typed root-owned `payesh-privd` module-invoke boundary wired into the server:
  RFC-8785-subset JCS canonicalizer and Ed25519 detached-signature verifier
  (`internal/trust`), the curated three-module catalog with per-server
  eligibility checks, safe tar.gz archive staging (checksum/size verified
  before extraction; absolute paths, traversal, and every symlink/hardlink
  entry rejected; atomic activation preserving the previous release),
  an explicit lifecycle state machine that keeps install separate from
  activation and preserves the previous working state on a failed update,
  and a durable per-server `module_installations`/`module_lifecycle_requests`
  store with the same compare-and-swap-revision and idempotency-key
  conventions as the package-05 traffic endpoint. Browser routes expose the
  catalog, per-server status, and install/enable/disable/remove. Signed
  metadata is freshness-, catalog-version-, core/protocol-, dependency-, and
  privilege-checked; missing health execution and remote-node execution fail
  closed. This is not
  the package-06 acceptance gate: no production release-signing key is
  provisioned,
  audit-event persistence now covers module lifecycle transitions atomically,
  and only synthetic in-process archive fixtures were exercised (no real Linux host,
  no real official module release artifact).
  This session also fixed two pre-existing build-breaking regressions in
  `internal/monitoring/store.go` (a `*time.Time` dereference bug and a stale
  `RollupPage{}` return from an earlier edit) that left `go build ./...`
  failing before any package-06 work began.
- Package-08 Checkpoint A adds CPU Controls: integer-millicore quota math
  matching the worked "1 core = 25% of a 4-vCPU server" example; a real
  cgroup v2 `cpu.max`/`cgroup.procs` file-format adapter (`internal/cpucontrol`,
  `FSCgroup`) that refuses to write into any directory Payesh did not create
  itself (an ownership record outside cgroupfs distinguishes a Payesh-created dedicated
  group from a pre-existing shared one) and walks ancestors to detect an
  inherited stricter quota; `/proc/<pid>/stat`-based process-identity
  verification so a reused PID cannot inherit a stale policy; a
  preview→apply→verify→revert `Manager` that records a verification mismatch
  as durably `failed` rather than success, retains the original pre-Payesh
  baseline across edits, and refuses to overwrite external quota changes; a shared `contracts.ControlPolicy`
  record and `control_policies`/`control_policy_requests` store designed for
  package 09 (Bandwidth Controls) to reuse; isolated HTTP adapters; and a
  `modules.Manager.DeactivateHooks` mechanism so disabling cpu-controls first
  reverts its active policies (a failing cleanup blocks the disable). The
  OpenAPI contract mismatch discovered while building this package (the
  package-01 foundation sketch's async `policyId`/`Job`-wrapped `/policies`
  and `/modules` families vs. the synchronous, idempotency-keyed,
  target-addressed routes actually implemented for packages 05/06/08) has
  been reconciled: `api/openapi.yaml` and `scripts/contract-check.mjs` now
  describe the real routes/schemas (new `/servers/{serverId}/modules/{moduleId}/install`,
  `/servers/{serverId}/cpu-policies...`; corrected `Module`/`ModuleInstallation`/`Policy`
  schemas).

## Verification

Package 10 has an initial signed-manifest, artifact-verification, anti-replay,
and external activation-journal checkpoint. Healthy activation commits only
after a supplied health check; failure or an interrupted activation restores
the prior complete release. Consistent SQLite snapshots include committed WAL
state and pass an integrity check. Verified release fetching/staging and an
independent recovery watchdog are also implemented. Role/history migration
primitives now provide consistent filtered exports, encrypted full-hub
artifacts, validated transactional imports, stable sample deduplication, and
an external idempotency journal. Durable hub-first/one-node-at-a-time
sequencing now records explicit offline/incompatible/conflict/executor results,
blocks node rollout after hub failure, and resumes through CAS-protected jobs.
`HTTPReleaseSource` and `ReleaseExecutor` now connect signed metadata fetching
to verified download, safe staging, caller-supplied backup/health checks, and
the existing transactional activation path for one local installation. An
exact schema registry now rejects unsupported versions, advances each step
transactionally, and requires a verified backup before destructive migrations.
The durable role-cutover journal orders backup/history/live-tail transfer and
one-controller authority switching, prevents concurrent phase execution, and
fails closed to operator recovery after an uncertain authority switch.
The current authenticated remote executor and root-worker handoff described
above extend this historical checkpoint. Store startup now has production schema registry entries; authenticated candidate
compatibility at the update boundary is being integrated. Distributed
role-cutover hook adapters, owner signing-anchor operations and owner-signed
multi-host acceptance remain incomplete.

Package 11 now has an initial local release-readiness slice: the
`release-package` tool cross-builds reproducible Linux amd64/arm64 archives for
all core and optional-module executables (and available web assets), emits a
`payesh.release.v1` manifest plus `SHA256SUMS` and the bootstrap script, and `release-validate` checks
archive safety, sizes, digests, inventory, and (when given an explicit public
key anchor) detached signatures. `release-sign` now performs the explicit
owner-invoked offline JCS + Ed25519 signing step using an external owner-only
private key and additionally signs the bootstrap checksum envelope; no production signing key is generated or stored here. The
default bundle remains intentionally unsigned (`signing_key_id=unavailable-local`)
and no official publication is claimed. Quickstart, update/recovery,
uninstall, contributor, and known-limitations guides are recorded in `docs/`.
Full live clean-host installation, signed publication/production rollout execution,
measured resource benchmarks, and final v1 acceptance remain outstanding.

The 2026-09-12 Ubuntu 22 core profile now passes real collector,
identity/epoch, SQLite ingestion/query, and authenticated populated-database
API startup. The earlier one-off SIGSEGV did not reproduce locally or remotely
after transfer integrity was enforced; truncated/in-flight artifacts were
observed during the two-host work and are treated as the cause of the invalid
earlier evidence. The tc backend also passed. A later focused CPU profile passed real quota enforcement inside a
transient `Delegate=yes` service, and the adapter now refuses nested groups
below a foreign `payesh` parent. A focused Port Traffic run exposed that
nftables 1.0.2 retains table/counter ownership
comments but omits them from JSON. Exact text fallbacks were added for that
version; guarded apply/snapshot/remove then passed, and the owned table plus
temporary directory were confirmed absent after cleanup. No foreign
cgroup/table was mutated.

Run from the repository root:

```text
go test ./cmd/... ./internal/... ./scripts/...
PAYESH_LIVE_TRANSPORT_ACCEPTANCE=1 go test -race ./cmd/... ./internal/... ./scripts/...
go vet ./cmd/... ./internal/... ./scripts/...
make lint
make build
make build-matrix
make contract-check
make web-check
```

Results on the development host: all Go tests passed, `make lint` passed,
the amd64/arm64 Linux matrix built, the OpenAPI contract check passed, and the
frontend pin/lockfile check passed. Race tests and cross-compiled Linux
monitoring test binaries also passed. CI configuration includes
protected-route, lint, service-definition, and distro/init checks. The
Go cache location can be changed with `GOCACHE` when the local environment
requires a writable temporary directory. Untracked local experiments are not
part of the product package test scope.

These checks cover compilation, monitoring arithmetic/storage behavior, static
contracts, and the frontend shell build. In addition, the 2026-09-11 Ubuntu
20.04 run exercised real `/proc`/`/sys` collection, persisted SQLite ingest and
queries, the follow-mode agent pipeline, bounded/redacted log capture, and the
authenticated local API; temporary state was removed. Real systemd-managed
restart, OpenRC, journald, rotation-under-load, and disk-pressure acceptance
still require the disposable environments described in the support matrix.

The package-02 Svelte design system and fixture previews still build. Default
production mode reads authenticated server/detail/metrics/traffic/log APIs and
now also consumes the implemented setup/session, label, enrollment/revocation,
durable job/cancellation, update, and SSH-install routes with cancellation,
bounded polling, visible partial failures, empty/retry/session-expired states,
and decimal-string counters. At that checkpoint browser screenshots had not
been collected. The current UI observations are described above; full live
candidate mutation and manual assistive-technology acceptance remain open.
This is not production completion.

## Next authorized work

Finish the agreed candidate deployment and backup/recovery checks, then verify
independent dashboard/node ports and both installer paths across real hosts.
Retest the browser against that deployed candidate. Review final resource and
endurance reports after their full bounded runs rather than promoting initial
observations to completed evidence.

The release owner must provide an independently distributed production public
anchor/key ID and publish owner-signed candidate assets. Exercise the actual
root-worker installed-layout update path with failure, interruption, rollback,
restart, mixed-fleet and fresh-version confirmation. Complete supported OpenRC
and distribution/architecture service acceptance, optional kernel control
scenarios, authoritative billing comparison and focused security/schema review.
Role-transition and remote-package capabilities must be completed or excluded
explicitly from supported release scope. The release checklist defines the
remaining gates; this checkpoint is not a completed production product.

The latest Settings refinement uses a consistent mobile section menu, distinct
dashboard/node port labels and a responsive account layout. Browser navigation
and overflow checks passed for all five sections from320px through1440px,
including a corrected tablet-width account-row overflow. Dark-mode and keyboard
checks passed; frontend validation, four tests and the production web build
passed. These newest fixture UI changes have not been activated on production;
prior server lifecycle evidence applies to its separately verified candidate.

An isolated native Linux fixture now runs the actual candidate hub and two
agents on both architectures. TLS enrollment, dashboard bind independence,
online/offline transport migration, guarded retirement, durable restarts and
stored sample/identity continuity passed. Installed Payesh services remained
unchanged and task cleanup passed. The arm64 strict controller failed solely
because an unrelated container was already restarting; retain that failure
alongside the scoped pass. Loopback fixtures do not establish physical firewall,
older-agent, host reboot or production deployment acceptance.

The latest UI is now packaged in a separate immutable local candidate for both
architectures. All twelve core binary hashes match the earlier tested candidate;
new web bytes, native ELF architectures, seven artifact pins and exact archive
inventories were independently verified. Earlier server stages and evidence
were preserved. The new package is now staged and independently hash-verified in separate
root-only directories on both servers. Activation and deployed browser
acceptance remain open.

Fresh recovery acceptance for the rebuilt worker completed all three disposable
OpenRC cases on both architectures: expired authorization after interruption,
failed restoration with trust temporarily unavailable, and reconciliation of a
completed rollback without trust. Each verified retained candidate data, restored
collection, worker generation reload and a new signed retry. Installed Payesh
services and endurance units were preserved, with independent cleanup. AMD64’s
controller passed; ARM64’s strict controller failed solely for an unrelated
preexisting container’s restart drift. These tests use disposable signing trust
and do not establish systemd recovery or physical power-loss acceptance.

The subsequent installer retry and mobile keyboard-focus fixes were verified
against the final browser bundle. Their new private candidate stages passed
independent exact inventory, native ELF, hash and installed-service/endurance
preservation checks on both architectures. No production activation occurred;
older native runtime evidence retains its earlier web scope.

The subsequent source review fixed local-hub Process Monitoring source installs
and legacy SSH executable authentication. Signed package transport/replay and
refusal regressions pass. Generated SSH shell regressions reject altered
installer, core and node-agent bytes before execution; uploaded executables are
checked before probe/preflight. Full roles use verified upload with a trusted web
tree pin. The complete source race suite with local TLS acceptance and vet passed;
additional focused installer race checks passed. New Linux candidate archives
were rebuilt with the unchanged final browser bundle. These changes alter the
server and installer executables, so earlier native runtime evidence remains
scoped to its recorded artifacts until fresh acceptance.

The latest review fixed the SSH schema check to use the same sudo privileges
and password input as installation. The regression verifies that a supported
sudo account checks the protected database before preflight or installation,
without exposing the password in commands. Installer, release, CLI and update
race tests pass after this correction, as do lint and the disposable installer
lifecycle. Both Linux core architectures were rebuilt from the reviewed source.
The short store soak passed; the separate native fleet runs exceeded 24 hours
but failed their coverage-gap assertion. Installed services remained unchanged
and fixture cleanup completed. Production endurance and current installed-host
signed update acceptance remain open.

The subsequent installer review removed a missing-transport-URL bypass: node
SSH installation now refuses absent endpoints rather than skipping the node-side
TLS reachability check. Live SSH acceptance requires an explicit endpoint and
real enrolled identity/trust files. The complete current-source race suite with
local TLS transport enabled, vet and contract checks passed after this change;
both Linux candidate architectures were rebuilt and their source fingerprint,
executable pins and archive inventories independently checked. This is source
and build evidence; fresh installed-host acceptance remains open.

Native standalone acceptance subsequently exposed concurrent fresh-database
initialization and WAL transition contention. Initialization now rechecks
metadata under SQLite’s writer lock; migrations recheck their version before
applying a step. Fresh databases retain incremental vacuum, and WAL setup
uses a bounded cancellable retry. Repeated real concurrent opens preserve all
committed samples and verify storage pragmas and cancellation. The full source
race suite with local TLS transport, lint and both Linux build architectures
passed after these fixes. Fresh native lifecycle acceptance remains required.

The cutover journal now uses immutable request creation, owned generations and
compare-and-swap completion. It records rollback intent before side effects and
uses bounded recovery contexts independent of caller cancellation. Regressions
and independent race review pass. These changes protect journal state; concrete
distributed transfer, authority fencing and production adapters remain open.

The rebuilt startup baseline subsequently passed isolated native systemd checks
on both Linux architectures: exact pinned executables, mapped service users,
root update worker, continuing collection and removal of fixture units, cgroups
and guest namespaces were verified. Installed services remained unchanged.
This baseline used the browser bundle frozen with that candidate and submitted
no signed update. A further actual-SQLite review reproduced a fresh-initialization
interruption window: schema creation commits before vacuum setup, and reopening
can leave incremental cleanup disabled. A temporary initialization marker now
commits with fresh schema creation and survives until incremental vacuum is
verified. Real SQLite cancellation/restart, concurrent-finalizer and unchanged
legacy-vacuum regressions pass. These baseline results precede that further fix
and do not close signed update or native interruption acceptance.

A fresh browser review verified actual API responses on desktop and narrow mobile
layouts. Account forms now explain the minimum password length; unavailable SSH
installation explains that the server was added and how to retry. Frontend checks,
build and tests pass. These screenshots used disposable synthetic server states;
successful remote installation and connected-node migration flows remain open.


Retained-state transfer now has an opt-in migration-v2 primitive for complete
schema-6 snapshots. It transfers replay frontiers, retired epochs, raw and traffic
tombstones, charged-sequence ledgers, pending derived work and allowance history.
Tail reconciliation requires the exact previously imported artifact and an
unchanged scoped destination; replacement is transactional and preserves
unrelated servers. The default v1 API remains compatible. Artifacts remain
limited to 64 MiB, exclude browser credentials and executable fleet authority,
and refuse incompatible global retirement saturation. This is a prerequisite;
production peer adapters, chunked transfer and distributed authority fencing
remain unfinished. Independent review reproduced lost replay/charging authority
on a successor; monotonic-authority refusal and valid compaction regressions now
pass. Validation also distinguishes half-open coverage gaps from inclusive
tombstones and independently verifies the maximum sequence point. The complete
source race suite with local TLS acceptance passes after these corrections.

Fresh isolated signed-systemd success cases subsequently failed on both Linux
architectures before reaching the installer health checkpoint. Worker journals
reported rollback and failure; the precise installer cause needs further
diagnosis. Fixture units, cgroups and guest namespaces were removed and live
service snapshots remained unchanged. These are failed update acceptance results,
despite the earlier successful baseline collection. They also precede the further
startup recovery and migration fixes described above; rebuilt native acceptance
remains required.


The final reviewed source passes the complete race suite with local TLS transport.
Its fresh isolated systemd baseline runs on both architectures with verified
executable pins, service identities and continuing samples; no signed update was
submitted in this baseline. The corrected 120-second fleet smoke passed on AMD64,
including host preservation and watchdog cleanup, and a new 24-hour run is active.
ARM64's fixture passed, but its overall smoke failed the container-preservation
check because a pre-existing unhealthy container restarted. Prior observations
also show restart churn; causation remains unresolved and the failed overall
result is retained. The short smoke does not establish completed retention or
production endurance. Signed update, native interruption and full distributed
cutover acceptance remain open.

A further logging-enabled rerun of the reviewed signed-systemd candidate failed
on both architectures. Private worker diagnostics identify extraction and copy
failures caused by the fixture's 128 MiB temporary filesystem: its retained
update payload requires approximately 142–146 MiB before filesystem overhead.
The worker reported restoration of the previous installation. Exact task cleanup
and installed-service preservation passed; these failures do not establish
successful update or fully verified rollback. A storage-only fixture correction
and fresh acceptance are required, with the original deadlines retained.

A fresh route review found that browser updates still dispatched through a legacy
worker branch without the installation transaction. Browser requests now receive
a durable operation identity and bounded authorization; historical requests are
promoted without extending their original expiry. All worker inbox activations
use the protected transaction, installed-version/service health checks and
recovery path. An authenticated API regression fails before the fix and passes
after it. The full source race suite with local TLS passes after this correction
and the migrated-endpoint diagnostic fix. Fresh native acceptance of this source
remains required.

Fresh local browser acceptance also exercised the actual hub API with two TLS
agents. An occupied target port retained the working endpoint. One offline node
remained pending, and retirement was refused by both the UI and authenticated
API. Returning that node completed migration; retirement committed, and an agent
restarted with its original configured URL used the saved new endpoint. All 40
baseline sample rows retained their hashes while collection continued. Desktop
and 320-pixel mobile checks found no horizontal overflow or page errors, and
owned processes and private fixture data were removed. This used synthetic host
metric files and prepared TLS identities on localhost; it does not establish
physical firewall/NAT, installer enrollment, older-agent or power-loss acceptance.
The browser run did not independently probe the retired listening socket.

## Current status (2026-10-05)

Store schema 7 adds per-server ingest authority. A new 6-to-7 registered
migration creates `server_authority` and `server_authority_transitions`; it
preserves all existing rows and does not assign authority implicitly. A server
without an authority row keeps legacy ingest. Authority is created only by an
explicit initialization, and `FreezeServerAuthority` and
`RelinquishServerAuthority` move it monotonically, bind each transition to one
cutover and immutable request digest, and are idempotent for exact replays.
Ingest checks the record inside its write transaction, so a committed freeze
serializes with every ingest: batches acknowledged before the freeze are in the
final tail and later batches fail without any durable effect. The same check
fences command and desired-state writes inside their own transactions: job
creation, action leasing and result completion, control-policy and module
installation transitions, and configuration-revision changes all fail with a
not-authoritative error once a server is frozen or relinquished. Enrollment
claims are fenced the same way. Outside the store, a hub that no longer owns a
server also refuses to renew its certificate, to mint an enrollment token and to
issue a node identity for the installer, using a point-in-time ownership check
before the certificate authority acts. The cutover's revoke phase can run an
idempotent callback to revoke the old hub's identity for the moved server after the
source has relinquished; a failure there stops the journal in operator recovery. A freeze computes a
deterministic digest of the server's retained ingest state (samples, coverage
gaps and retired epoch identifiers) inside its own transaction. A destination
store activates authority only when the state it holds hashes to exactly that
frontier, so a missing or extra tail refuses activation and leaves no authority
behind; the activated generation is the source's final generation plus one.
An integration test moves a server through the real migration-v2 export and
import path and confirms ownership of writes moves from source to destination.

A store-level cutover orchestrator now drives that sequence through the durable
role-cutover journal with idempotent hooks: preflight, a verified SQLite backup,
the retained-range transfer, destination verification, source freeze, the frozen
tail transfer, destination activation, source relinquish, confirmation and an
optional cleanup of transient artifacts. Every exported artifact is persisted
atomically before use, so a crash injected after the side effects of each phase
resumes from that phase once the lease expires and reaches the same state. A
failure before the switch resumes the source with a recorded, generation-advancing
abort that burns the cutover identifier; a retry uses a new identifier and imports
over the baseline the failed attempt left. The destination is reached through a
small peer interface with a local-store implementation. An authenticated network
peer now implements that interface: the destination serves TLS 1.3 with mutual
authentication, and a client is authorized only by an expiring grant naming one
client certificate fingerprint, one server and one cutover; the server takes
neither the server nor the cutover from the request. The client pins the
destination certificate. Artifacts move in hash-verified chunks that resume from
whatever the peer already holds, are inspected to confirm they contain only the
granted server (encrypted full-hub artifacts are refused), and are then imported
with the existing replay-safe migration; activation goes through the store's
frontier verification. A full cutover over this peer passes locally and on both
native architectures over loopback. Transfers remain bounded by the existing
64 MiB artifact limit. `payesh cutover` now wraps this as operator commands
(identity, grant, serve, run, status; see [server cutover](SERVER_CUTOVER.md)):
grants are one per client certificate, expire within 24 hours, live in a private
file that the serving process re-reads on every request, and fail closed when the
file is corrupt or readable by others. The real binary completed a cutover, and
refused a client with no grant and a client with a wrong pin, on both Linux
architectures in a sandbox with no outside network. Unit tests cover
explicit initialization, replay and conflict handling, the migration from a
schema-6 file, and a concurrent freeze-versus-ingest race. The migration-v2
snapshot allowlist deliberately excludes authority rows. The command fence,
destination activation, orchestrator, authenticated peer and operator commands
are described above and in [server cutover](SERVER_CUTOVER.md). A schema-7 build
frozen from the final source, including all of them, has passed native
acceptance of the upgrade path on both Linux architectures: a real schema-6 database with collected samples
migrated to schema 7 with identical rows and an intact integrity check; the
schema-6 binary refuses the migrated file, and restoring the pre-upgrade copy
makes it work again; and through the real signed update worker in isolated
systemd fixtures (disposable test trust) the 6-to-7 upgrade committed, a failed
health check after the migration rolled back to schema 6 with the failed
generation preserved, and both a hard kill of the worker during activation and an
expired request recovered cleanly. The OpenRC fixtures also passed for that final
schema-7 build on both architectures in containers with disposable trust: clean
install and lifecycle, the signed hub, node and standalone worker cases
(upgrade, health rollback, signature, expiry, unsafe trust anchor and preview
refusal) and refusal on low space. On arm64 the strict whole-host comparison
failed only because an unrelated container was already restarting. A long-running
endurance run and production signing trust have not been repeated for schema 7.

Signed-update acceptance has since passed on the schema-6 candidate in isolated
systemd and OpenRC fixtures on both architectures with disposable test trust: signed
install success, failed-health rollback, expired-request recovery, a hard kill of
the worker during activation, hub, node and standalone OpenRC worker cases, and
refusal on low space before any snapshot or archive download. These fixtures do
not establish production signing trust, host reboot or power loss, activation-time
disk exhaustion, or a completed 24-hour endurance result. See the release
checklist for the exact residual gaps.

Installing a node over SSH was exercised through the dashboard against a
disposable Alpine (OpenRC, busybox) target using key authentication, and the node
connected and reported fresh telemetry. That run found and fixed two defects.
The executable-staging check copied `/bin/true`, which is a busybox symlink and
fails under another name, so staging was rejected on Alpine; it now probes with a
small script written into the directory. The node configuration step ran as
`sudo` followed by a multi-line script, which elevated only the first line, so a
non-root SSH user got no identity, trust or environment files and the node never
connected; the script is now run as one `sudo sh -c` command, and OpenRC hosts
restart `payesh-agent` after configuration. Regression tests cover both.

The cutover commands now open the journal and the source database with a 10-second
busy timeout. Before this, a concurrent reader such as `payesh cutover status`
could make a running cutover fail immediately with a locked-database error and
exit mid-phase; the run could then be resumed, but the failure was avoidable. A
regression test holds a competing write lock and confirms the writer waits.

The first full 24-hour fleet endurance run on the schema-6 candidate (amd64, two
agents, real binaries on loopback) did not pass. It ran 86,755 seconds with 116,389
queries and no query errors, services and containers unchanged and independent
cleanup passing, then failed its own consistency check in the last seconds: a
retention coverage gap was not matched by a billing traffic tombstone. Retention
begins pruning after 24 hours, so this was the first time that check saw real
pruning. The two schema-7 24-hour runs (amd64 and arm64) failed identically at the
same point. The cause is a test-fixture bug: the fixture configured the billing
allowance after each server's first few samples, so those samples were never billed
and have no traffic-ledger rows. Traffic tombstones therefore start at the first billed
sequence (for example 3-99 for a gap of 0-100) while the fixture demanded coverage
from the gap start. A regression test in the traffic package reproduces this shape
and shows the product records tombstones for every billed sample. The fixture now
requires traffic coverage only from the first billed sequence. These runs are failures
of the fixture check, not passes; a corrected 24-hour run is required before the
endurance gate can be called closed. The corrected fixture then ran about 16-17 hours on each architecture with 0 query
errors and unchanged host services and containers before the operator stopped it
early; that is a partial soak, not a completed 24-hour run.
