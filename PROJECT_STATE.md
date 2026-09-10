# Project state

Updated: 2026-09-10

## Current milestone

Package 08 (CPU Controls) — Checkpoint A implemented: millicore quota math,
a real cgroup v2 file-format adapter with ownership-marker enforcement and
parent-quota detection, PID-reuse-safe process identity verification, a
preview/apply/verify/revert lifecycle, a shared `ControlPolicy` store meant
for package 09 to reuse, and a module-disable cleanup hook. Not yet accepted:
no real CPU-load test has run on any Linux host. Package 06 (signed
optional-module framework) Checkpoint A implemented: curated catalog,
JCS/Ed25519 manifest trust verification, safe archive staging, atomic
activation, an explicit install/activation-separated lifecycle state machine,
durable per-server store, and browser routes. Not yet accepted: no real
`payesh-privd` wiring, no provisioned production signing key, and only
synthetic in-process archive fixtures tested. An explicit, reported OpenAPI
contract mismatch (async `policyId`/`Job`-wrapped `/policies`+`/modules` in
the package-01 sketch vs. the synchronous, idempotency-keyed routes actually
implemented in 05/06/08) needs lead reconciliation before either package is
accepted; see `docs/handoffs/06.md` and `docs/handoffs/08.md`. Package 05
(traffic allowances, alerts, and incidents) Checkpoint A has completed its
internal Critical/Major review and fix cycles but is likewise not yet accepted
as the full package milestone because external acceptance work remains.

## Completed

- Package 03 Checkpoint A: Linux/local monitoring collectors, SQLite history,
  retention, rollups, logs, and bounded ingestion.
- Package 04 Checkpoint A: authenticated browser flow, CA-owned enrollment,
  node identity/revocation, protocol validation, one-live-connection hub, and
  installer preflight checks.
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
  refused unless this process itself created the target directory (an
  ownership-marker file makes that checkable), and an inherited stricter
  ancestor quota is detected and reported rather than silently promised away.
  A process-group target's identity is re-verified via `/proc` start-time
  immediately before every Apply, so a reused PID can never inherit a stale
  policy. Every Apply/Revert reads the quota back and durably records
  `failed` — never a false "applied"/"reverted" — on any mismatch, and Revert
  always restores exactly what the matching Apply found beforehand.
  `ControlPolicy` (contracts) and its store are intentionally control-kind-
  generic so package 09 (Bandwidth Controls) reuses the same CAS/idempotency
  machinery instead of re-deriving it.

## Known risks / unrun acceptance work

- Linux kernel traffic instrumentation, provider-billing comparison, external
  outage observation, real Telegram/webhook endpoint tests, disposable-fleet
  integration, and 24-hour resource soaks remain unrun on this macOS host.
- The post-process queue and traffic ledger are durable and bounded. Long-lived
  server/follow-ingest processes run a bounded periodic post-process retry
  worker; a dedicated operational metric for prolonged derived-work pressure
  remains a future integration concern.
- A saturated collector-epoch retirement authority intentionally pauses new
  opaque epochs until its configured bound is raised; this is a fail-closed
  operational condition, not silent replay weakening.
- Full package-03/package-04 acceptance gates and real browser/assistive-tech
  validation remain outstanding.
- Package 06 has no real `payesh-privd` wiring yet (that binary is still an
  unimplemented stub), so Enable/Disable only flip durable state today — they
  do not start/stop a real process or tear down kernel rules. No production
  module-signing key is provisioned (the server's trust registry is empty by
  default, so installs fail closed on "unknown signing key" until an operator
  configures one). Only synthetic in-process archive fixtures were tested, not
  a real signed official module release on a real Linux host. Audit-event
  persistence does not exist for any package yet.
- Package 08 has no real CPU-load enforcement test — this development host
  has no cgroupfs at all, so `cgroupRootDir()`'s default `/sys/fs/cgroup`
  points nowhere real here; tests exercise the file-format/ownership/CAS
  logic against a temp-directory stand-in instead, which is real and correct
  file-format behavior but cannot prove kernel enforcement. `payesh run`
  (local CLI helper to place an unmanaged workload in its own dedicated
  group) and target discovery (enumerating existing systemd/OpenRC services)
  are not implemented; a caller must name a target explicitly. `pidfd`-based
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
for real, and add the `payesh run` CLI workflow and target discovery. For
package 06: wire real `payesh-privd module.invoke` actions for
enable/disable/health-check, provision a production release-signing key, add
durable audit-event persistence, and run acceptance tests against real signed
module release artifacts on a disposable Linux host. Before accepting either,
reconcile the reported `api/openapi.yaml` contract mismatch (see
`docs/handoffs/06.md`/`08.md`). For package 05: request package acceptance
with the external Linux/provider/observer/notification and soak evidence
called out above. After all three, connect the remaining
real-data/external-observer seams (and packages 07, 09) without expanding
traffic into automatic control policy ahead of an explicit owner-selected
policy.
