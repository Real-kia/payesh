# Project state

Updated: 2026-09-12

Latest verification on 2026-09-12 passed the full Go race suite, `go vet`,
OpenAPI contract check, web type/build check, shell syntax checks, Linux
amd64/arm64 build matrix, init-definition checks, and disposable
install/upgrade/recovery/uninstall acceptance. A fresh 10-second monitoring
soak inserted 101/101 samples with zero ingest, prune, or storage errors;
retention was observed and teardown was clean. This is smoke evidence, not a
replacement for the outstanding long-duration soak.

## Current milestone

Package 09 (Bandwidth Controls) code implementation is complete in the working
tree. It includes explicit warn/throttle/block policies, distinct ingress and
egress tc paths, trusted management exclusions, ownership-safe rollback,
durable policy/audit state, a 15-second quota worker, independent watchdog,
optional module daemon, authenticated proxy wiring, and init definitions.
Ubuntu 20.04 and the rebuilt Ubuntu 22.04 tests prove real tc
apply/verify/revert, foreign-root refusal, management-failure rollback,
watchdog expiry recovery, daemon socket/database startup, and init-file
validation. The reusable runner additionally passed collector, SQLite
ingest/query, authenticated local API, and detected the Ubuntu 22 cgroup-v2
CPU controller. The module-socket stage passed in the latest full runner. A
focused nftables rerun found and fixed Ubuntu 22.04 nftables 1.0.2 omitting
persisted ownership comments from JSON; exact text fallbacks preserve the
fail-closed boundary, and real apply/snapshot/remove passed with cleanup
confirmed. The opt-in throughput probe also reached the
host but reported unsupported because its same-namespace veth topology bypassed
the shaped interface. A 2026-09-12 split-profile rerun again passed the real
tc backend after the slow SSH link was handled with bounded profile uploads.
The runner now records bounded measured throughput and durable-ledger quota
sample overshoot, but a clean-capability host run and collector-driven kernel
quota attribution are still required. Persistent Payesh service lifecycle
and load evidence remain. The host itself was rebooted and
recovered over SSH in 13 seconds on 2026-09-12.

Package 07 (Port Traffic) now has a runnable implementation checkpoint. Its
checkpoint defines bounded local TCP/UDP service-port scopes, explicit
interface/direction/original-versus-translated tuple identity, declarative
Payesh-owned IPv4/IPv6 rule plans, forwarded-path classification,
reset/reload-aware integer counter deltas, authenticated desired-scope API,
database-level unique scope claims, durable observations, and a privileged
reconciliation daemon with systemd/OpenRC definitions. Privileged nftables
execution is available on the rebuilt Ubuntu 22 host. Real guarded
apply/snapshot/remove now passes there, including the nftables 1.0.2 text
fallback and post-test cleanup. The NAT/bridge/container/tunnel correctness
matrix remains.

Package 04 (Fleet identity, transport and installation) remains incomplete.
`payesh-install` now has preflight plus an explicit role-selective installer
for already verified local artifacts, with retry state, service accounts,
directories, and systemd/OpenRC definitions. The TLS 1.3 WebSocket listener
and agent now provide protected single-use bootstrap, authenticated ingestion,
in-band renewal, reconnect, durable spool replay, and durable leased actions.
The agent advertises actions only when its privileged-helper socket is set.
Signed manifest-selected fetching exists in `internal/updater`; authenticated
update plans now durably enqueue through `updater.Scheduler`. `internal/install/ssh.go` now
provides a host-key-verified, transient-credential hub-assisted installer
boundary that delegates to the direct installer and requires enrollment plus
measurement-arrival callbacks. The browser API now durably enqueues update and
SSH-install jobs; SSH credentials remain process-memory-only and are never
stored in the jobs table. Disposable-Linux SSH acceptance remains.
Browser-mode startup now runs bounded durable update and installation worker
passes. The default server intentionally has no release signing anchor or SSH
artifact/callback configuration, so unavailable work settles explicitly rather
than claiming execution.
The Ubuntu 22 core acceptance profile now passes real collection, stable
identity with fresh epochs, SQLite ingest/query, and authenticated API startup
against the populated database. The earlier one-off SIGSEGV did not reproduce
locally across 20 launches or in the fresh remote profile; the later discovery
of truncated/in-flight uploaded executables explains the unreliable earlier
run, so it is no longer tracked as a product crash. On 2026-09-12 the supplied Ubuntu 22 host also
completed an authorized reboot and returned to SSH in 13 seconds with
`systemd=running`. A separate uniquely named Payesh systemd unit using a
persistent disposable path survived restart and reboot and passed `/healthz`;
its unit and state were removed. On 2026-09-12 a real two-host bootstrap and
ingestion acceptance passed with the disposable Ubuntu 22.04 amd64 host acting
as hub and the shared Debian 12 arm64 host running a temporary foreground node.
The node consumed a single-use enrollment job over TLS, persisted its client
identity, and the hub durably stored sequence zero for
`server-debian-node-0001`. Both hosts were cleaned afterward; no persistent
unit or Payesh state remained on the shared host. Live renewal and action
delivery acceptance remain unrun. The local two-node transport gate is now
covered by the opt-in `PAYESH_LIVE_TRANSPORT_ACCEPTANCE=1` test: it uses an
ephemeral port, bootstraps two independent certificates, reconnects both
agents over TLS, and verifies per-node durable ingestion. The default suite
skips the listener-owning test in restricted/shared environments.
Browser owner/session/throttle state and the
enrollment authority are now restart-safe in SQLite. Persisted enrollment
state includes the CA key/certificate, pending pairing-token digests, issued
certificate ownership and revocations; it excludes cleartext pairing tokens
and node private keys. Enrollment HTTP/job production and durable node job
consumption are wired. Live automated renewal/recovery and direct/SSH
installer acceptance remain. Spool eviction gaps are persisted ahead of retained samples
so a restart cannot lose the coverage evidence needed for cumulative replay.

Package 10 now has a composable update/recovery/migration checkpoint: signed
manifest anti-replay, bounded verified fetching and safe staging, atomic
activation with external journal rollback, consistent SQLite backup, an
independent watchdog service, filtered single-server and encrypted full-hub
history transfer, and durable hub-first fleet scheduling. Security review
removed predictable entropy fallbacks and standardized migration encryption on
bounded PBKDF2-HMAC-SHA256 plus AES-GCM. Schema migrations now have contiguous
version gates, transactional execution, and destructive-step backup approval;
role transitions have a leased durable phase journal with live-tail and
one-controller fail-closed boundaries. Still missing are a production signing
anchor and executor, concrete schema entries and cutover hook adapters,
multi-host role-transition acceptance, and real old-to-new signed
upgrade/recovery acceptance.

Package 11 has an initial release-readiness checkpoint. Local tooling builds
deterministic unsigned Linux amd64/arm64 archives, emits a release manifest and
SHA256SUMS, validates archive inventory/safety, refuses destructive overwrite,
and publishes a complete candidate by atomic rename. Quickstart, recovery,
uninstall, contributor, and known-limitations guides exist. This is not a
signed or published release, and performance/security/usability matrices plus
clean-host release acceptance remain. The direct installer now has a guarded
role-specific `--uninstall` command with data retention by default, and
`scripts/install-acceptance.sh` exercises disposable install/upgrade/failure-
recovery/resume/uninstall cycles. A disposable systemd unit also survived
restart and reboot with `/healthz` passing, but a signed release installed on a
fresh clean host remains a separate gate.

The dashboard retains explicit fixture-preview mode, while production mode now
uses authenticated server/detail/metrics/traffic/log APIs plus the implemented
owner setup/session, label, enrollment/revocation, durable job/cancellation,
update, and SSH-install routes. Requests support cancellation, four-server
bounded enrichment, bounded job polling, partial-load warnings, and
loading/empty/retry/session-expired states. Browser accessibility, visual
acceptance, and live mutation acceptance remain.

Package 08 (CPU Controls) now has a runnable module checkpoint: millicore quota math,
a cgroup v2 file-format adapter with external ownership-record enforcement and
parent-quota detection, PID-reuse-safe process identity verification, a
preview/apply/verify/revert lifecycle, a shared `ControlPolicy` store meant
for package 09 to reuse, a cgroup hierarchy/runtime adapter, optional authenticated
Unix-socket API, server proxy wiring, and systemd/OpenRC definitions. The base
server still returns 503 when the optional CPU socket is not configured.
The real CPU-load probe now passes on the rebuilt Ubuntu host inside a
transient systemd `Delegate=yes` unit: the adapter safely propagates the CPU
controller through its owned hierarchy and measured 201274 microseconds of CPU
usage at a 200m quota over 900 milliseconds. Broader service/OpenRC scenarios
remain. Package 06 (signed
optional-module framework) Checkpoint B implemented: curated catalog,
JCS/Ed25519 manifest trust verification, safe archive staging, atomic
activation, an explicit install/activation-separated lifecycle state machine,
durable per-server store, browser routes, and a typed root-owned
`payesh-privd` module-invoke boundary wired into the server when configured.
Not yet accepted: no provisioned production signing key, complete module
teardown, and only
synthetic in-process archive fixtures tested. The OpenAPI contract mismatch
found while building package 08 (async `policyId`/`Job`-wrapped
`/policies`+`/modules` in the package-01 sketch vs. the synchronous,
idempotency-keyed routes actually implemented in 05/06/08) has been
reconciled: `api/openapi.yaml`/`scripts/contract-check.mjs` now describe the
real routes; see `docs/handoffs/06.md` and `docs/handoffs/08.md`. Package 05
(traffic allowances, alerts, and incidents) Checkpoint A has completed its
internal Critical/Major review and fix cycles but is likewise not yet accepted
as the full package milestone because external acceptance work remains.

## Completed

- Package 03 Checkpoint A: Linux/local monitoring collectors, SQLite history,
  retention, rollups, logs, and bounded ingestion. Ubuntu 20.04 acceptance now
  covers real `/proc`/`/sys` collection, the agent-to-ingest stream, SQLite
  query/API reads, and redacted log capture; init/pressure evidence remains.
- Package 04 Checkpoint A: restart-safe SQLite-backed browser owner/session/
  throttle state; a persistent enrollment CA with pending-token digests,
  certificate ownership and revocations; node identity/protocol validation;
  one-live-connection hub; a bounded persistent job store with operation
  idempotency, revision-CAS transitions/cancellation, and authenticated job
  read/cancel routes; and installer preflight checks.
- Package 05 Checkpoint A: durable calendar traffic periods and allowances,
  interface/direction accounting, bounded forecasts, revision-safe/idempotent
  configuration, timestamp/coverage alert evaluation, maintenance and grouped
  incidents, bounded notification delivery, authenticated browser routes,
  telemetry/liveness integration, and a durable post-process replay queue with
  exactly-once traffic charging. Counter intervals are split transactionally
  across calendar/DST boundaries and explicit owner-started period cutovers;
  deferred streams cannot starve ready streams behind them. Forecasts use a
  24-hour-first window with a bounded seven-day fallback and fail closed for
  incomplete period continuity. Historical traffic-period policy snapshots are
  immutable during delayed usage processing, and queued work pins the relevant
  server's period history until it drains. Alert rules have durable effective
  policy boundaries, and legacy generic traffic starters are reconciled away.
  Fresh collector epochs mark sequence-zero usage uncertain when an earlier
  epoch exists; explicit period cutovers close predecessors atomically.
  Forecast reads retain a bounded newest tail and tolerate normal 5s/10s
  cadence edges. Notification cadence commits only after successful delivery,
  and durable rule creation/reconciliation caps enabled and disabled rows.
- Package 07 initial checkpoint: validated non-additive local service-port
  accounting scopes, original/translated tuple rule planning with ownership
  markers, guarded nftables apply/read/remove, and stable coverage/reset-aware
  counter deltas.
- Package 09 implementation: interface/local-port tc enforcement, trusted
  management-flow discovery, ownership-gated apply/verification, durable local
  rollback, shared-ledger quota evaluation, atomic policy audit records,
  module API/proxy wiring, and systemd/OpenRC runtime definitions.

## Current architecture decisions

- SQLite is the durable boundary for raw samples, traffic periods, alert state,
  histories, incidents, maintenance, configuration revisions, and idempotency.
- Raw ingestion commits first. A store-level post-commit observer processes
  newly inserted samples; a gap-only commit also wakes queued work that the gap
  makes processable. Observer failure never causes a durable sample to be
  acknowledged twice, and is surfaced as bounded pending derived work.
- Derived processing is serialized per processor and alert state transitions
  are serialized per engine. Traffic processing requires a durable sequence
  prefix from zero (or explicit coverage gaps) before mutating alert/usage
  state; usage is charged once through a durable server/epoch/sequence/
  allowance ledger.
- Traffic configuration and generated warning-rule reconciliation are committed
  in one transaction with compare-and-swap revision and idempotency checks.
- Period previews and durable period records preserve selected interface sets;
  configuration rejects canonical metric-name collisions and the reserved
  aggregate `billing` interface.
- Alert state, history, and incident transitions are committed atomically.
- Asynchronous notification failures are observable and retryable: queue
  admission or terminal provider failure leaves the durable notification clock
  unchanged, while a successful worker advances it conditionally. Failure
  diagnostics are bounded and secret-free.
- Maintenance and outage suppression retain a durable suppression timestamp so
  reminders remain cadence-bounded and the first unsuppressed observation can
  notify promptly. Explicitly selected interfaces remain observable even when
  the display-only interface cap is reached.
- Collector epoch replay authority is bounded and fail-closed: exact retired
  epoch markers are retained up to the configured metadata authority limit;
  known streams remain usable at saturation while unseen opaque epochs are
  rejected until an operator raises `MetadataMaxRows` through a later prune.
- Browser sessions use transport-aware cookie security; trusted proxy addresses
  are required before forwarded client addresses affect throttling.
- Package 05 traffic is explicitly a host-side estimate, not provider billing;
  control-policy actions remain outside this package.
- Package 06 never imports an optional module's implementation and never
  accepts a request-provided download URL; only the hardcoded catalog names
  eligible module IDs. A manifest's Ed25519 signature (over its RFC-8785-
  subset canonical JSON) and its archive's declared size/SHA-256 are verified
  before any byte is written to disk. Install and activation are separate
  states; a completed install always lands on installed-disabled. Archive
  extraction rejects every symlink/hardlink entry outright, all path
  traversal/absolute paths, and any entry that would exceed the manifest's
  declared unpacked size. Module lifecycle rows use the same compare-and-swap
  revision and idempotency-key pattern as package-05 traffic configuration.
- Package 08 CPU quotas are always a budget for an explicitly named,
  Payesh-owned dedicated cgroup, never "the whole server": every write is
  refused unless Payesh previously created the target directory (an
  ownership record on ordinary durable storage makes that checkable), and an inherited stricter
  ancestor quota is detected and reported rather than silently promised away.
  A process-group target's identity is re-verified via `/proc` start-time
  immediately before every Apply. Existing processes must already be members
  of their dedicated group; Apply never moves them implicitly. Every Apply/Revert reads the quota back and durably records
  `failed` — never a false "applied"/"reverted" — on any mismatch, and Revert
  retains and restores the original pre-Payesh quota across repeated edits and
  refuses to overwrite an externally changed quota.
  `ControlPolicy` (contracts) and its store are intentionally control-kind-
  generic so package 09 (Bandwidth Controls) reuses the same CAS/idempotency
  machinery instead of re-deriving it.

## Known risks / unrun acceptance work

- Known-volume Linux traffic accounting, provider-billing comparison, external
  outage observation, real Telegram/webhook endpoint tests, certificate
  renewal/action delivery, and 24-hour resource soaks remain. Real two-host TLS
  enrollment and monitoring ingestion are complete.
- The post-process queue and traffic ledger are durable and bounded. Long-lived
  server/follow-ingest processes run a bounded periodic post-process retry
  worker; a dedicated operational metric for prolonged derived-work pressure
  remains a future integration concern.
- A saturated collector-epoch retirement authority intentionally pauses new
  opaque epochs until its configured bound is raised; this is a fail-closed
  operational condition, not silent replay weakening.
- Package-04 durable enrollment-job transition wiring and authenticated
  WebSocket bootstrap/ingestion have real two-host evidence. Automated
  certificate renewal/recovery, action delivery, and SSH/direct installer
  execution remain outstanding, as do package-03 systemd/OpenRC, journald, rotation, and
  disk-pressure acceptance plus package-04's remaining Linux gates and real
  browser/assistive-tech validation.
- Package 06 now has a typed `payesh-privd` Unix-socket helper and server-side
  executor wiring guarded by `PAYESH_PRIVD_SOCKET`. The helper allowlists
  module IDs/operations, validates clean paths and deadlines, and never accepts
  shell commands from callers. Installs still fail closed when no real health
  callback is configured; remote-node lifecycle actions also fail closed rather
  than changing hub-local files. No production
  module-signing key is provisioned (the server's trust registry is empty by
  default, so installs fail closed on "unknown signing key" until an operator
  configures one). Only synthetic in-process archive fixtures were tested, not
  a real signed official module release on a real Linux host. Module lifecycle
  CAS transitions now commit redacted audit rows atomically; production key
  provisioning and real signed-release acceptance remain outstanding.
- Package 08 now has a passing real CPU-load enforcement result. On the rebuilt
  Ubuntu 22 host, a systemd-delegated disposable unit retained the owned
  hierarchy and enforced 200m at 201274 microseconds of CPU usage over 900ms.
  Tests also exercise file-format/ownership/CAS logic against a stand-in.
  `payesh run` now starts an unmanaged workload in its own Payesh-owned
  dedicated group without a shell, while read-only target discovery exists via
  `payesh cpu-services`. The base server intentionally exposes only a 503
  placeholder until the authenticated helper/module host supplies the service.
  `pidfd`-based
  identity is documented but not implemented — only the portable `/proc`
  start-time check is.

## Verification

The current workspace passes `go test ./...`, `go test -race ./...`, `go vet
./...`, `make lint`, `make build`, `make build-matrix`, `make web-check`, and
`make contract-check`, including the final disabled-rule, forecast-continuity,
retirement-authority, effective-at, and delayed-period identity regressions,
plus the `internal/trust`, `internal/modules`, and `internal/cpucontrol`
suites (JCS canonicalization, Ed25519 verification, archive-safety,
lifecycle-transition, and manager/HTTP integration tests; cgroup v2
file-format/ownership behavior, PID-reuse-safe identity verification, and the
CPU-controls preview/apply/revert lifecycle). In this sandbox, Go commands use
`GOCACHE=/private/tmp/payesh-go-cache`.

## Next milestone

For package 08: run real CPU-load enforcement tests on a disposable Linux
host, exercise shared-group/parent-quota/PID-reuse/restart/OpenRC scenarios
for real; the local `payesh run` workflow and target discovery are implemented.
For package 06: provision a
production release-signing key, and run acceptance tests against real signed
module release artifacts on a disposable Linux host (the `api/openapi.yaml`
contract mismatch has been reconciled — see `docs/handoffs/06.md`/`08.md`).
For package 05: request package acceptance
with the external Linux/provider/observer/notification and soak evidence
called out above. Package 09 next needs only the real Linux acceptance matrix
and signed-module deployment evidence described in `docs/handoffs/09.md`.
