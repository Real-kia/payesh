# Data model v1 (contract)

SQLite is owned by `payesh-server` and later local standalone storage. The agent
build graph must not depend on SQLite. All timestamps are UTC RFC 3339 values;
display and billing timezones are explicit settings.

## Entities

| Entity | Identity and required semantics |
| --- | --- |
| `servers` | Stable random ID, role, platform/architecture, capabilities, version, heartbeat, config revision. |
| `metric_samples` | Server + collector epoch + sequence; observed/received timestamps; typed values; validity/coverage. |
| `rollups` | Server/metric/UTC bucket; count, observed duration, min/max/weighted mean or valid counter delta. |
| `traffic_periods` | Server/scope, immutable selected-interface snapshot, timezone-aware bounds, allowance, direction, counted bytes, continuity status. Package 03 persists these periods independently from graph rollups; package 05 owns calendar-boundary calculation. Delayed usage may update counted bytes/continuity but never relabels the policy snapshot. |
| `traffic_allowances`, `traffic_allowance_changes`, `traffic_allowance_requests` | Owner-selected interface/direction allowance, warning percentages, and restart-safe idempotent schedule transitions. A schedule edit records the old calendar and effective boundary; the amount may change in the active period. |
| `log_sources`, `log_entries` | Validated source identity/cursor, bounded text, timestamp/severity, truncation/redaction flags. |
| `alert_rules`, `alert_states`, `alert_history`, `maintenance_windows`, `incidents` | Editable timestamp/coverage-based rules with durable `effective_at` policy boundaries, pending/firing/recovered state, durable transition history, notification suppression windows, and bounded grouped incident evidence. Rule rows are capped at 20,000 total and 4,096 per server, including disabled rows. |
| `post_process_queue`, `collector_epoch_metadata`, `collector_epoch_retirements` | Bounded durable derived-work replay queue and collector-epoch lifecycle metadata. Queued samples pin required traffic history; exact retired epoch markers are retained up to a singleton authority limit and unseen epochs fail closed at saturation. |
| `module_installations`, `control_policies` | Signed module/version state and policy revision/effective state. |
| `jobs` | Durable state, idempotency key, target, expiry, progress, cancellation and recovery details. |
| `audit_events` | `id`, UTC `occurred_at`, actor type/optional ID, action, target type/optional ID, result, decimal-string revision, redaction flag, and optional correlation ID; independently retained within budget. |

Counters and byte quantities are unsigned 64-bit values (maximum
18446744073709551615). JSON APIs serialize
values that may exceed JavaScript's safe integer range as decimal strings,
including sequence numbers, configuration revisions, job revisions, and
coverage-gap bounds.
Missing, stale, unsupported and uncertain readings are distinct from zero.
Collectors may encode integer-valued capacity gauges (for example
`memory.*_bytes` and `disk.root.*`) in the decimal-string counter map to retain
raw precision; rollup implementations must aggregate those fields as gauges,
not monotonic deltas.

Traffic forecasts are host-side recent-rate estimates, never provider-authoritative
usage. The API checks a 24-hour window first and may use a bounded seven-day
fallback; forecasts are available only with at least 24 hours of usable
observations and 80% interval coverage. A period whose durable continuity is
`gap` or `uncertain` is not forecastable; counter resets, overflow, invalid
observations, and intervals longer than 24 hours lower coverage and can hide the
estimate. Alert
state transitions use observation timestamps (not a fixed sample count), retain
precision warnings for sparse sampling, and suppress delivery during maintenance
or a known node outage without stopping collection. Collector epoch changes at a
sequence-zero baseline degrade the active traffic period to `uncertain` when an
earlier epoch is known; explicit period restarts close the predecessor in the
same transaction. Configured alert delivery advances cadence only after a
successful worker send; failed admission/delivery remains retryable.

The contract fixtures in `internal/contracts/testdata/` cover a >53-bit
counter, an evicted sequence gap, an expired durable job, and a partial-fleet
result. They are test inputs only and are not served as production telemetry.

## Ownership and invariants

Only the server writes fleet/history tables. Agent spool data is local and
bounded to 32 MiB. Mutating jobs require an idempotency key and, when applicable,
an expected configuration revision. A unique constraint on `(server_id,
collector_epoch, sequence)` makes ACK retries harmless. Deleting expired data
must never remove identity keys, active policies, configuration, or current
billing-period totals. Queued derived work also protects the historical period
identity needed to process its delayed sample. Migrations are forward-only with a recorded schema
version and a verified backup before destructive transformation.

All wire-level 64-bit sequence and revision fields are decimal strings (Go
implementations may use `uint64` internally with string JSON tags). A job is
serialized per target machine and transitions through explicit queued, running,
cancelling, terminal, or recovery-required states; cancellation and expiry are
recorded outcomes, never silent success.

Metric samples retain source and receipt timestamps even when the source clock
is ahead. Consumers expose that condition as `source-clock-ahead` uncertainty;
it is not converted into a zero or discarded as a validation error.

Audit events use the shared `AuditEvent` record and the API `AuditEvent` schema.
`actor_type` is one of `owner`, `node`, `local-cli`, or `system`; `result` is
one of `succeeded`, `failed`, or `denied`. The event's target type/ID identify
the affected server, module, policy, release, backup, or role transition. Event
details are intentionally not a free-form archival payload: secrets, private
keys, tokens, and raw command output are never valid audit fields. `redacted`
means a user-visible record omitted sensitive operational detail.
