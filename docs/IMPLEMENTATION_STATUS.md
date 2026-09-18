# Implementation status

Updated: 2026-09-18

For the current verified command results and the only remaining external
acceptance requirements, see [`ACCEPTANCE_STATUS.md`](ACCEPTANCE_STATUS.md).
The package handoffs below are historical implementation evidence; they are not
the authority for current acceptance status when they conflict with that file.

This file records implementation evidence, not planning intent. The repository
started as planning documents only. Package 01 / milestone M0 is **ready for review**
and is not accepted. Package 02 / milestone M1 has a **production API-connected dashboard plus an explicit fixture preview**;
it is not an accepted milestone. Package 03 / milestone
M2 now has a **local-monitoring Checkpoint A**; core Ubuntu Linux collector,
SQLite ingest/query, log capture, and local API acceptance is evidenced, while
distribution/init and pressure testing remain pending. Package 04 remains at
Checkpoint A awaiting its acceptance gates. A side-effect-free
`payesh-install` preflight CLI now exposes the existing platform/role and
kernel/tooling capability decision in human or JSON form and fails closed
before credentials or artifact transfer.
Package 04 browser authentication, enrollment-authority persistence, its
generic durable job store/read/cancel API, authenticated enrollment job
production, and durable out-of-band token consumption are now restart-safe.
An opt-in TLS 1.3 WebSocket node listener and agent client now provide protected
bootstrap, certificate-authenticated ingestion, reconnect, heartbeat, durable
spool replay, cumulative acknowledgement, in-band renewal, and leased durable
actions. The direct installer applies already verified local artifacts
idempotently and installs systemd/OpenRC definitions. Signed artifact fetching
remains incomplete. Authenticated `/api/v1/updates` requests now create
idempotent durable scheduler plans, and `/api/v1/installations` (plus the
per-server alias) creates durable SSH-install jobs without persisting
credentials. The hub-assisted installer boundary now requires a
caller-provided artifact verifier, rejects unsafe local paths and symlinks,
verifies known-host fingerprints, uses transient password/key credentials,
transfers verified role artifacts, delegates to the direct installer, and fails closed
until enrollment and measurement-arrival callbacks complete. Disposable Ubuntu
live SSH acceptance now passes pinned host-key verification, optional local
source binding, atomic rsync transfer with SCP fallback, remote SHA-256
verification, node systemd activation, preservation-first uninstall, explicit
data removal, and repeated uninstall.
Browser-mode server startup now runs bounded durable update and SSH-install
worker passes. The default process has no release signing anchor or SSH
artifact/callback configuration: new SSH submissions return
`install_executor_unavailable`, and any pre-existing unavailable update rows
settle with `executor_unavailable` instead of remaining queued indefinitely.
The direct installer also exposes an exact-path `--uninstall` operation that
stops/disables only the selected role's services, preserves data by default,
and requires `--remove-data` for database/config/log deletion. A disposable
clean-host lifecycle gate covers first install, artifact replacement, failed
verification recovery, persisted resume state, and both uninstall modes;
live signed clean-host rollout remains environment-dependent.
Public access now has a reusable guarded helper. With a resolving domain it
checks listener ownership, installs Caddy (including its official repository
fallback on Debian/Ubuntu), writes only a Payesh-marked configuration, enables
secure browser cookies through the Payesh environment file when present, and
delegates issuance/renewal to Caddy. Without a domain it leaves the hub on
loopback and prints an encrypted SSH-tunnel workflow with an explicit warning.
Real Ubuntu 22 acceptance passed Let's Encrypt issuance, HTTP redirect, TLS
1.3, correct SAN verification, reverse proxying, and idempotent re-apply.
Reusable one-shot notification acceptance commands also passed an externally
routed HTTPS/HMAC webhook and the supplied Telegram test bot/chat.
Package 05 Checkpoint A has completed its internal Critical/Major review and fix
cycles, but still awaits the package acceptance evidence listed below. Package 06
now has a **module-framework Checkpoint B** (catalog, trust verification, safe
archive staging, lifecycle, durable store, HTTP routes, and a typed root-owned
`payesh-privd` module-invoke boundary wired into the server when configured); it
is not accepted and has no provisioned production signing key yet.
Package 08 now has a **CPU Controls runtime checkpoint** (quota math, real cgroup v2
file-format adapter, PID-reuse-safe process identity, preview/apply/revert
lifecycle, durable shared `ControlPolicy` store, a cgroup hierarchy/runtime,
Unix-socket API/proxy, and systemd/OpenRC definitions). The rebuilt Ubuntu
22.04.1 host now passes real 200m CPU enforcement inside a disposable
systemd-delegated unit (201274 microseconds used over 900ms); broader service
and OpenRC acceptance remain. Package 07 (Port Traffic)
now has an accounting contract, guarded nftables adapter, forwarded-path
classification, atomic unique-scope/idempotency claims, durable counter
observations, a reconciliation daemon, init definitions, and an opt-in
Linux acceptance probe. On 2026-09-12 the Ubuntu 22 nftables 1.0.2 profile
passed guarded apply/snapshot/remove and confirmed removal of temporary owned
state.
Package 09 (Bandwidth Controls) implementation is present: tc enforcement,
trusted management exclusions, durable policy/audit state, quota worker,
independent watchdog, optional module daemon, authenticated proxy routes, and
service definitions. Ubuntu 20.04 and the rebuilt Ubuntu 22.04.1 isolated-
interface runs prove tc apply/verify/revert and both rollback paths. The
reusable runner also passed collector, SQLite ingest/query, and authenticated
local API on the rebuilt host before reaching the nftables ownership guard.
The module-socket stage passed in the latest full runner; the nftables probe
safely stopped at a foreign-table ownership guard. The acceptance runner now
has bounded isolated-veth throughput evidence output (target rate, observed
bytes, elapsed time, effective rate, and rate overshoot) and a durable-ledger
quota-sample overshoot probe. Those probes are opt-in and still require a
clean-capability host run; collector-driven kernel quota attribution,
reboot/full service lifecycle, and whole-process overhead evidence remain.

## Completed in this session

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
- Recorded the comment-by-comment resolution in
  `docs/reviews/01-foundation-review-resolution.md`.
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
  wrapping. Design decisions and the handoff are recorded under
  `design-system/payesh/MASTER.md` and `docs/handoffs/02.md`.
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
  The review resolution is recorded in
  `docs/reviews/03-local-monitoring-review-resolution.md` and the handoff in
  `docs/handoffs/03.md`.
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
  and Linux acceptance remain outstanding. See `docs/handoffs/04.md`.

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
  accepted 4,280/4,280 samples without ingest/prune/storage errors. See
  `docs/handoffs/05.md`.
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
  no real official module release artifact). See `docs/handoffs/06.md`.
  This session also fixed two pre-existing build-breaking regressions in
  `internal/monitoring/store.go` (a `*time.Time` dereference bug and a stale
  `RollupPage{}` return from an earlier edit) that left `go build ./...`
  failing before any package-06 work began.
- Package-08 Checkpoint A adds CPU Controls: integer-millicore quota math
  matching PLAN.md's worked "1 core = 25% of a 4-vCPU server" example; a real
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
  schemas). See `docs/handoffs/08.md`.

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
Production registry entries, cutover hook adapters, an authenticated remote
executor, signing-anchor operations, and multi-host acceptance remain
unimplemented. See
`docs/handoffs/10.md`.

Package 11 now has an initial local release-readiness slice: the
`release-package` tool cross-builds reproducible Linux amd64/arm64 archives for
all core and optional-module executables (and available web assets), emits a
`payesh.release.v1` manifest plus `SHA256SUMS`, and `release-validate` checks
archive safety, sizes, digests, inventory, and (when given an explicit public
key anchor) detached signatures. `release-sign` now performs the explicit
owner-invoked offline JCS + Ed25519 signing step using an external owner-only
private key; no production signing key is generated or stored here. The
default bundle remains intentionally unsigned (`signing_key_id=unavailable-local`)
and no official publication is claimed. Quickstart, update/recovery,
uninstall, contributor, and known-limitations guides are recorded in `docs/`.
Full live clean-host installation, signed publication/production rollout execution,
measured resource benchmarks, and final v1 acceptance remain outstanding. See
`docs/handoffs/11.md`.

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
GOCACHE=/private/tmp/payesh-go-cache go test ./...
GOCACHE=/private/tmp/payesh-go-cache go test -race ./...
GOCACHE=/private/tmp/payesh-go-cache go vet ./...
GOCACHE=/private/tmp/payesh-go-cache make lint
GOCACHE=/private/tmp/payesh-go-cache make build
GOCACHE=/private/tmp/payesh-go-cache make build-matrix
make contract-check
make web-check
```

Results on the development host: all Go tests passed, `make lint` passed,
the amd64/arm64 Linux matrix built, the OpenAPI contract check passed, and the
frontend pin/lockfile check passed. Race tests and cross-compiled Linux
monitoring test binaries also passed. CI configuration includes
protected-route, lint, service-definition, and distro/init checks. The
default Go cache location is restricted in this sandbox, so `GOCACHE` must be
set to a writable temporary directory here.

These checks cover compilation, monitoring arithmetic/storage behavior, static
contracts, and the frontend shell build. In addition, the 2026-09-11 Ubuntu
20.04 run exercised real `/proc`/`/sys` collection, persisted SQLite ingest and
queries, the follow-mode agent pipeline, bounded/redacted log capture, and the
authenticated local API; temporary state was removed. Real systemd-managed
restart, OpenRC, journald, rotation-under-load, and disk-pressure acceptance
still require the disposable environments described in `docs/handoffs/03.md`.

The package-02 Svelte design system and fixture previews still build. Default
production mode reads authenticated server/detail/metrics/traffic/log APIs and
now also consumes the implemented setup/session, label, enrollment/revocation,
durable job/cancellation, update, and SSH-install routes with cancellation,
bounded polling, visible partial failures, empty/retry/session-expired states,
and decimal-string counters. Browser visual screenshots, live mutation
acceptance, and manual assistive-technology checks remain unrun. This is not
production completion.

## Next authorized work

Review and accept the remaining package-03 init/pressure evidence in a
disposable Linux matrix, then
continue package-04 with live transport and SSH-installer acceptance. Durable
installer-job orchestration is now implemented. Run live browser mutation acceptance
against those seams, then run the committed CI
workflow and focused security/schema review before accepting M0/M2. For
package 06, the next inputs are a provisioned production release-signing key
and Linux acceptance tests against real
signed module release artifacts rather than only synthetic in-process
fixtures — see `docs/handoffs/06.md`. For package 08, the next inputs are
shared-group/parent-quota/PID-reuse/restart/OpenRC acceptance scenarios; real
CPU-load enforcement now passes in the delegated Ubuntu 22 runner. The
`payesh run` workflow and target discovery are implemented — see `docs/handoffs/08.md`. The
`api/openapi.yaml` mismatch against packages 05/06/08 has been reconciled
(see above); a fresh review pass should still confirm the reconciled
document against real client usage before acceptance. Do not describe this
checkpoint as a complete monitoring product.
