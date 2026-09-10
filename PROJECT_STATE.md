# Project state

Updated: 2026-09-10

## Current milestone

Package 05 (traffic allowances, alerts, and incidents) — Checkpoint A has
completed its internal Critical/Major review and fix cycles. It is not yet
accepted as the full package milestone because external acceptance work remains.

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

## Verification

The current workspace passes `go test ./...`, `go test -race ./...`, `go vet
./...`, `make lint`, `make build`, `make build-matrix`, `make web-check`, and
`make contract-check`, including the final disabled-rule, forecast-continuity,
retirement-authority, effective-at, and delayed-period identity regressions.
In this sandbox, Go commands use `GOCACHE=/private/tmp/payesh-go-cache`.

## Next milestone

Complete the fresh final Part 5 review, then request package acceptance with
the external Linux/provider/observer/notification and soak evidence called out
above. After acceptance, connect the remaining real-data/external-observer
seams without expanding traffic into automatic control policy.
