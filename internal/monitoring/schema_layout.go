package monitoring

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func reconcileStoreSchema(ctx context.Context, tx *sql.Tx) error {
	// Capture this before CREATE TABLE. Only databases that genuinely predate
	// temporal allowance history need a synthetic baseline; ordinary reopen
	// must never make a pre-creation sample retroactively chargeable.
	var allowanceVersionsPreexisting int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='traffic_allowance_versions'`).Scan(&allowanceVersionsPreexisting); err != nil {
		return fmt.Errorf("inspect traffic allowance version schema: %w", err)
	}
	const schema = `
CREATE TABLE IF NOT EXISTS storage_settings (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1), max_bytes INTEGER NOT NULL,
 sample_seconds INTEGER NOT NULL, pressure_seconds INTEGER NOT NULL,
 adaptive INTEGER NOT NULL, notifications INTEGER NOT NULL, revision INTEGER NOT NULL,
 pressure_state TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS storage_notifications (
 id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, message TEXT NOT NULL,
 created_at TEXT NOT NULL, is_read INTEGER NOT NULL DEFAULT 0, event_key TEXT UNIQUE NOT NULL
);
CREATE TABLE IF NOT EXISTS browser_auth_state (
  singleton INTEGER PRIMARY KEY CHECK(singleton=1),
  state_json BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS fleet_identity_state (
  singleton INTEGER PRIMARY KEY CHECK(singleton=1),
  state_json BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS jobs (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  state TEXT NOT NULL,
  revision TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  target_server_id TEXT NOT NULL DEFAULT '',
  expires_at TEXT NOT NULL,
  cancel_requested INTEGER NOT NULL DEFAULT 0,
  progress INTEGER NOT NULL DEFAULT 0,
  error_json TEXT,
  action_json TEXT,
  result_json TEXT,
  lease_token TEXT,
  lease_expires_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(kind, target_server_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS jobs_expiry ON jobs(expires_at, state, id);
CREATE TABLE IF NOT EXISTS job_cancellation_requests (
  job_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY(job_id, idempotency_key),
  FOREIGN KEY(job_id) REFERENCES jobs(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS servers (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  address TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL,
  architecture TEXT NOT NULL,
  platform TEXT NOT NULL,
  capabilities_json TEXT NOT NULL,
  version TEXT NOT NULL,
  last_heartbeat TEXT,
  connection_state TEXT NOT NULL,
  freshness_state TEXT NOT NULL,
  freshness_reason TEXT,
  configuration_revision TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS metric_samples (
  server_id TEXT NOT NULL,
  collector_epoch TEXT NOT NULL,
  sequence TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  received_at TEXT NOT NULL,
  timestamp_uncertainty TEXT,
  values_json TEXT NOT NULL,
  counters_json TEXT,
  units_json TEXT,
  validity_json TEXT,
  PRIMARY KEY (server_id, collector_epoch, sequence),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS metric_samples_time ON metric_samples(server_id, observed_at, sequence);
CREATE TABLE IF NOT EXISTS coverage_gaps (
  server_id TEXT NOT NULL,
  collector_epoch TEXT NOT NULL,
  from_sequence TEXT NOT NULL,
  to_sequence TEXT NOT NULL,
  reason TEXT NOT NULL,
  from_observed_at TEXT,
  to_observed_at TEXT,
  PRIMARY KEY (server_id, collector_epoch, from_sequence, to_sequence, reason),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS coverage_gaps_frontier ON coverage_gaps(server_id, collector_epoch, length(from_sequence), from_sequence, length(to_sequence), to_sequence);
CREATE INDEX IF NOT EXISTS metric_samples_sequence_range ON metric_samples(server_id, collector_epoch, length(sequence), sequence);
CREATE TABLE IF NOT EXISTS metric_rollups (
  server_id TEXT NOT NULL,
  metric TEXT NOT NULL,
  bucket_start TEXT NOT NULL,
  bucket_seconds INTEGER NOT NULL,
  sample_count INTEGER NOT NULL,
  observed_seconds REAL NOT NULL,
  minimum REAL,
  maximum REAL,
  weighted_mean REAL,
  counter_delta TEXT,
  coverage TEXT NOT NULL,
  PRIMARY KEY (server_id, metric, bucket_start, bucket_seconds),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS metric_rollups_time ON metric_rollups(server_id, bucket_start, bucket_seconds);
CREATE TABLE IF NOT EXISTS rollup_rebuild_queue (
  server_id TEXT NOT NULL,
  bucket_start TEXT NOT NULL,
  bucket_seconds INTEGER NOT NULL,
  PRIMARY KEY (server_id, bucket_start, bucket_seconds),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS traffic_periods (
  server_id TEXT NOT NULL,
  scope TEXT NOT NULL,
  period_start TEXT NOT NULL,
  period_end TEXT NOT NULL,
  timezone TEXT NOT NULL,
  allowance_bytes TEXT NOT NULL,
  direction TEXT NOT NULL,
  counted_bytes TEXT NOT NULL,
  continuity TEXT NOT NULL,
  interfaces_json TEXT NOT NULL DEFAULT '[]',
  PRIMARY KEY (server_id, scope, period_start, direction),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS traffic_periods_time ON traffic_periods(server_id, period_start, scope, direction);
-- Contiguous sequence frontier for O(1) post-commit readiness checks. frontier
-- is the first sequence not covered by a raw sample or explicit gap; max_point
-- records a durable sequence=MaxUint64 sample without overflowing frontier.
CREATE TABLE IF NOT EXISTS sequence_frontiers (
  server_id TEXT NOT NULL,
  collector_epoch TEXT NOT NULL,
  frontier TEXT NOT NULL,
  max_point INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (server_id, collector_epoch),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
-- Last-seen timestamps let retention retire metadata from collector epochs
-- that have stopped producing data. Epoch metadata is deliberately separate
-- from raw samples because samples are deleted on a shorter age policy.
CREATE TABLE IF NOT EXISTS collector_epoch_metadata (
  server_id TEXT NOT NULL,
  collector_epoch TEXT NOT NULL,
  first_seen_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  PRIMARY KEY (server_id, collector_epoch),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS collector_epoch_metadata_last_seen ON collector_epoch_metadata(last_seen_at, server_id, collector_epoch);
-- Retired epochs are a compact authority for replay rejection. Range
-- tombstones may be removed only after this marker is committed; unlike the
-- age-bounded coverage metadata, these markers are retained so a late sample
-- can never reopen a charged usage identity.
CREATE TABLE IF NOT EXISTS collector_epoch_retirements (
  server_id TEXT NOT NULL,
  collector_epoch TEXT NOT NULL,
  retired_at TEXT NOT NULL,
  PRIMARY KEY(server_id,collector_epoch),
  FOREIGN KEY(server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS collector_epoch_retirements_time ON collector_epoch_retirements(retired_at, server_id, collector_epoch);
-- Opaque epoch IDs cannot be summarized by a monotonic cutoff. This singleton
-- therefore bounds exact permanent retirement markers and records the
-- fail-closed state used to reject previously unseen epochs at capacity.
CREATE TABLE IF NOT EXISTS collector_epoch_retirement_authority (
  singleton INTEGER PRIMARY KEY CHECK(singleton=1),
  max_rows INTEGER NOT NULL,
  saturated INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO collector_epoch_retirement_authority(singleton,max_rows,saturated) VALUES(1,10000,0);
-- Per-server ingest authority. A server without a row here keeps legacy
-- (unfenced) ingest; a row is created only by an explicit initialization and
-- every later transition is monotonic, bound to one cutover and durable.
CREATE TABLE IF NOT EXISTS server_authority (
  server_id TEXT PRIMARY KEY,
  generation INTEGER NOT NULL CHECK(generation >= 1),
  owner_id TEXT NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('active','frozen','relinquished')),
  cutover_id TEXT NOT NULL DEFAULT '',
  transition_digest TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  FOREIGN KEY(server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS server_authority_transitions (
  cutover_id TEXT NOT NULL,
  server_id TEXT NOT NULL,
  to_state TEXT NOT NULL CHECK(to_state IN ('frozen','relinquished','activated','aborted')),
  request_digest TEXT NOT NULL,
  frontier_digest TEXT NOT NULL,
  from_generation INTEGER NOT NULL,
  to_generation INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY(cutover_id, server_id, to_state),
  FOREIGN KEY(server_id) REFERENCES servers(id) ON DELETE CASCADE
);
-- Exactly-once usage application key. The raw sample queue may be replayed
-- after an observer crash, but a server/epoch/sequence/allowance tuple can
-- charge its period only once.
CREATE TABLE IF NOT EXISTS traffic_usage_ledger (
  server_id TEXT NOT NULL,
  collector_epoch TEXT NOT NULL,
  sequence TEXT NOT NULL,
  scope TEXT NOT NULL,
  direction TEXT NOT NULL,
  PRIMARY KEY (server_id, collector_epoch, sequence, scope, direction),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
-- Compact, durable replay tombstones for ledger rows whose raw samples have
-- aged out. Ranges are inclusive and preserve exactly-once charging without
-- retaining one ledger row per sample forever.
CREATE TABLE IF NOT EXISTS traffic_usage_tombstones (
  server_id TEXT NOT NULL,
  collector_epoch TEXT NOT NULL,
  scope TEXT NOT NULL,
  direction TEXT NOT NULL,
  from_sequence TEXT NOT NULL,
  to_sequence TEXT NOT NULL,
  PRIMARY KEY (server_id, collector_epoch, scope, direction, from_sequence, to_sequence),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS traffic_usage_tombstones_lookup ON traffic_usage_tombstones(server_id, collector_epoch, scope, direction, from_sequence);
-- Raw-sample retention tombstones prevent a late retransmission from being
-- accepted as a fresh sample (and charged under a newer allowance) after the
-- original detailed row has aged out. Bounds are inclusive.
CREATE TABLE IF NOT EXISTS metric_sample_tombstones (
  server_id TEXT NOT NULL,
  collector_epoch TEXT NOT NULL,
  from_sequence TEXT NOT NULL,
  to_sequence TEXT NOT NULL,
  PRIMARY KEY (server_id, collector_epoch, from_sequence, to_sequence),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS metric_sample_tombstones_lookup ON metric_sample_tombstones(server_id, collector_epoch, from_sequence);
CREATE TABLE IF NOT EXISTS log_sources (
  server_id TEXT NOT NULL,
  id TEXT NOT NULL,
  label TEXT NOT NULL,
  path TEXT NOT NULL,
  PRIMARY KEY (server_id, id),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS log_entries (
  server_id TEXT NOT NULL,
  source_id TEXT NOT NULL,
  cursor TEXT NOT NULL,
  timestamp TEXT NOT NULL,
  severity TEXT,
  text TEXT NOT NULL,
  truncated INTEGER NOT NULL DEFAULT 0,
  redacted INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (server_id, source_id, cursor),
  FOREIGN KEY (server_id, source_id) REFERENCES log_sources(server_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS log_entries_time ON log_entries(server_id, source_id, timestamp, cursor);
CREATE TABLE IF NOT EXISTS traffic_allowances (
  server_id TEXT NOT NULL,
  scope TEXT NOT NULL,
  direction TEXT NOT NULL,
  interfaces_json TEXT NOT NULL,
  allowance_bytes TEXT NOT NULL,
  reset_day INTEGER NOT NULL,
  timezone TEXT NOT NULL,
  warnings_json TEXT NOT NULL,
  PRIMARY KEY (server_id, scope, direction),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
-- Immutable configuration history makes delayed post-processing use the
-- allowance that was effective when a sample was accepted. The current table
-- remains the bounded owner-facing projection and CAS target.
CREATE TABLE IF NOT EXISTS traffic_allowance_versions (
  server_id TEXT NOT NULL,
  scope TEXT NOT NULL,
  direction TEXT NOT NULL,
  effective_at TEXT NOT NULL,
  interfaces_json TEXT NOT NULL,
  allowance_bytes TEXT NOT NULL,
  reset_day INTEGER NOT NULL,
  timezone TEXT NOT NULL,
  warnings_json TEXT NOT NULL,
  PRIMARY KEY (server_id, scope, direction, effective_at),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS traffic_allowance_versions_effective ON traffic_allowance_versions(server_id, scope, direction, effective_at);
-- A schedule edit is kept separately from the current allowance so a server
-- restart cannot accidentally apply the new reset boundary in the middle of
-- an already-counted calendar period. The row is removed atomically when its
-- effective boundary is reached.
CREATE TABLE IF NOT EXISTS traffic_allowance_changes (
  server_id TEXT NOT NULL,
  scope TEXT NOT NULL,
  direction TEXT NOT NULL,
  effective_at TEXT NOT NULL,
  previous_allowance_json TEXT NOT NULL,
  proposed_allowance_json TEXT NOT NULL,
  PRIMARY KEY (server_id, scope, direction),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS traffic_allowance_changes_effective ON traffic_allowance_changes(effective_at, server_id);
CREATE TABLE IF NOT EXISTS traffic_allowance_requests (
  server_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (server_id, idempotency_key),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS traffic_allowance_requests_created ON traffic_allowance_requests(created_at);
CREATE TABLE IF NOT EXISTS alert_rules (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  expression TEXT NOT NULL,
  server_id TEXT,
  enabled INTEGER NOT NULL,
  duration_seconds INTEGER NOT NULL,
  recovery_threshold REAL,
  reminder_seconds INTEGER NOT NULL,
  group_key TEXT,
  idempotency_key TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL,
  effective_at TEXT NOT NULL,
  disable_reason TEXT NOT NULL DEFAULT '',
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
-- Raw samples are acknowledged before package-specific derived work runs.
-- Keep the exact accepted sample here so a crashed observer can replay it
-- without asking the node to resend (which would be deduplicated at ingest).
CREATE TABLE IF NOT EXISTS post_process_queue (
  server_id TEXT NOT NULL,
  collector_epoch TEXT NOT NULL,
  sequence TEXT NOT NULL,
  sample_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (server_id, collector_epoch, sequence),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS post_process_queue_created ON post_process_queue(created_at, server_id);
CREATE INDEX IF NOT EXISTS alert_rules_server ON alert_rules(server_id, id);
CREATE TABLE IF NOT EXISTS alert_states (
  id TEXT PRIMARY KEY,
  rule_id TEXT NOT NULL,
  server_id TEXT,
  state TEXT NOT NULL,
  pending_since TEXT,
  firing_since TEXT,
  recovered_at TEXT,
  last_observation TEXT,
  last_value REAL,
  last_notified_at TEXT,
  last_suppressed_at TEXT,
  pending_recovery_at TEXT,
  incident_id TEXT,
  precision_warning TEXT,
  revision INTEGER NOT NULL DEFAULT 0,
  UNIQUE(rule_id, server_id),
  FOREIGN KEY (rule_id) REFERENCES alert_rules(id) ON DELETE CASCADE,
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS alert_states_server ON alert_states(server_id, state, id);
CREATE TABLE IF NOT EXISTS alert_history (
  id TEXT PRIMARY KEY,
  alert_id TEXT NOT NULL,
  server_id TEXT,
  state TEXT NOT NULL,
  occurred_at TEXT NOT NULL,
  reason TEXT,
  value REAL,
  incident_id TEXT,
  FOREIGN KEY (alert_id) REFERENCES alert_rules(id) ON DELETE CASCADE,
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS alert_history_time ON alert_history(occurred_at, id);
CREATE TABLE IF NOT EXISTS maintenance_windows (
  id TEXT PRIMARY KEY,
  idempotency_key TEXT NOT NULL UNIQUE,
  starts_at TEXT NOT NULL,
  ends_at TEXT NOT NULL,
  server_ids_json TEXT NOT NULL,
  reason TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS maintenance_windows_time ON maintenance_windows(starts_at, ends_at, id);
CREATE TABLE IF NOT EXISTS incidents (
  id TEXT PRIMARY KEY,
  server_id TEXT,
  group_key TEXT NOT NULL,
  state TEXT NOT NULL,
  started_at TEXT NOT NULL,
  ended_at TEXT,
  summary TEXT NOT NULL,
  events_json TEXT NOT NULL,
  evidence_json TEXT NOT NULL,
  truncated INTEGER NOT NULL DEFAULT 0,
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS incidents_server_time ON incidents(server_id, started_at, id);
-- Durable per-server optional-module lifecycle (package 06). revision is a
-- compare-and-swap counter so concurrent install/enable/disable/remove
-- requests for the same module cannot race each other's state transition.
CREATE TABLE IF NOT EXISTS module_installations (
  server_id TEXT NOT NULL,
  module_id TEXT NOT NULL,
  version TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  revision INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  error_json TEXT,
  PRIMARY KEY (server_id, module_id),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS module_installations_server ON module_installations(server_id, module_id);
-- Idempotent lifecycle-request results, mirroring traffic_allowance_requests:
-- a repeated Install/Enable/Disable/Remove click with the same key returns
-- the original outcome instead of running the action twice.
CREATE TABLE IF NOT EXISTS module_lifecycle_requests (
  server_id TEXT NOT NULL,
  module_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (server_id, module_id, idempotency_key),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS module_lifecycle_requests_created ON module_lifecycle_requests(created_at);
-- Shared control-policy record (package 08 CPU Controls today; package 09
-- Bandwidth Controls reuses this same table/shape). One row per server/
-- module/target: a second Apply for the same target is a revision-CAS
-- update to the same row, never a duplicate policy.
CREATE TABLE IF NOT EXISTS control_policies (
  server_id TEXT NOT NULL,
  module_id TEXT NOT NULL,
  target_kind TEXT NOT NULL,
  target_name TEXT NOT NULL,
  kind TEXT NOT NULL,
  state TEXT NOT NULL,
  parameters_json TEXT NOT NULL DEFAULT '{}',
  revision INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  error_json TEXT,
  PRIMARY KEY (server_id, module_id, target_kind, target_name),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS control_policies_server ON control_policies(server_id, state);
CREATE TABLE IF NOT EXISTS control_policy_requests (
  server_id TEXT NOT NULL,
  module_id TEXT NOT NULL,
  target_kind TEXT NOT NULL,
  target_name TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (server_id, module_id, target_kind, target_name, idempotency_key),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS control_policy_requests_created ON control_policy_requests(created_at);
-- Port Traffic's counter identity is not the same as its user-facing scope
-- name.  Keep the identity claim in SQLite so two server processes cannot
-- select the same kernel counter between a read and a later policy write.
CREATE TABLE IF NOT EXISTS control_policy_unique_keys (
  server_id TEXT NOT NULL,
  module_id TEXT NOT NULL,
  unique_key TEXT NOT NULL,
  target_kind TEXT NOT NULL,
  target_name TEXT NOT NULL,
  PRIMARY KEY (server_id, module_id, unique_key),
  UNIQUE (server_id, module_id, target_kind, target_name),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS control_policy_unique_keys_target ON control_policy_unique_keys(server_id,module_id,target_kind,target_name);
-- Durable observations are deliberately separate from control policy
-- parameters: replacing a scope must not make an absolute nft counter look
-- like desired configuration or increase its revision.
CREATE TABLE IF NOT EXISTS port_traffic_observations (
  server_id TEXT NOT NULL,
  scope_id TEXT NOT NULL,
  bytes TEXT NOT NULL,
  packets TEXT NOT NULL,
  generation TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  continuity TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY (server_id, scope_id),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
-- Redacted structured audit records. Policy transitions insert their audit
-- row in the same SQLite transaction as the state change.
CREATE TABLE IF NOT EXISTS audit_events (
  id TEXT PRIMARY KEY,
  occurred_at TEXT NOT NULL,
  actor_type TEXT NOT NULL,
  actor_id TEXT NOT NULL DEFAULT '',
  action TEXT NOT NULL,
  target_type TEXT NOT NULL,
  target_id TEXT NOT NULL DEFAULT '',
  result TEXT NOT NULL,
  revision INTEGER NOT NULL,
  redacted INTEGER NOT NULL DEFAULT 1,
  correlation_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS audit_events_time ON audit_events(occurred_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS audit_events_target ON audit_events(target_id, occurred_at DESC, id DESC);
`
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate sqlite schema: %w", err)
	}
	// These columns were added after the initial durable-job checkpoint. Keep
	// startup compatible with existing stores instead of requiring a destructive
	// migration or silently dropping queued action payloads.
	for _, column := range []struct {
		name string
		ddl  string
	}{
		{name: "action_json", ddl: "ALTER TABLE jobs ADD COLUMN action_json TEXT"},
		{name: "result_json", ddl: "ALTER TABLE jobs ADD COLUMN result_json TEXT"},
		{name: "lease_token", ddl: "ALTER TABLE jobs ADD COLUMN lease_token TEXT"},
		{name: "lease_expires_at", ddl: "ALTER TABLE jobs ADD COLUMN lease_expires_at TEXT"},
	} {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name=?`, column.name).Scan(&count); err != nil {
			return fmt.Errorf("inspect jobs schema: %w", err)
		}
		if count == 0 {
			if _, err := tx.ExecContext(ctx, column.ddl); err != nil {
				return fmt.Errorf("migrate jobs schema: %w", err)
			}
		}
	}
	var disableReasonColumn int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('alert_rules') WHERE name='disable_reason'`).Scan(&disableReasonColumn); err != nil {
		return fmt.Errorf("inspect alert rule schema: %w", err)
	}
	if disableReasonColumn == 0 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE alert_rules ADD COLUMN disable_reason TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate alert rule schema: %w", err)
		}
	}
	var effectiveAtColumn int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('alert_rules') WHERE name='effective_at'`).Scan(&effectiveAtColumn); err != nil {
		return fmt.Errorf("inspect alert rule effective timestamp schema: %w", err)
	}
	if effectiveAtColumn == 0 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE alert_rules ADD COLUMN effective_at TEXT`); err != nil {
			return fmt.Errorf("migrate alert rule effective timestamp schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE alert_rules SET effective_at=created_at WHERE effective_at IS NULL OR effective_at=''`); err != nil {
			return fmt.Errorf("backfill alert rule effective timestamp: %w", err)
		}
	}
	var suppressedColumn int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('alert_states') WHERE name='last_suppressed_at'`).Scan(&suppressedColumn); err != nil {
		return fmt.Errorf("inspect alert state schema: %w", err)
	}
	if suppressedColumn == 0 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE alert_states ADD COLUMN last_suppressed_at TEXT`); err != nil {
			return fmt.Errorf("migrate alert state schema: %w", err)
		}
	}
	var pendingRecoveryColumn int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('alert_states') WHERE name='pending_recovery_at'`).Scan(&pendingRecoveryColumn); err != nil {
		return fmt.Errorf("inspect alert state pending recovery schema: %w", err)
	}
	if pendingRecoveryColumn == 0 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE alert_states ADD COLUMN pending_recovery_at TEXT`); err != nil {
			return fmt.Errorf("migrate alert state pending recovery schema: %w", err)
		}
	}
	var alertStateRevisionColumn int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('alert_states') WHERE name='revision'`).Scan(&alertStateRevisionColumn); err != nil {
		return fmt.Errorf("inspect alert state revision schema: %w", err)
	}
	if alertStateRevisionColumn == 0 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE alert_states ADD COLUMN revision INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("migrate alert state revision schema: %w", err)
		}
	}
	var serverAddressColumn int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('servers') WHERE name='address'`).Scan(&serverAddressColumn); err != nil {
		return fmt.Errorf("inspect server address schema: %w", err)
	}
	if serverAddressColumn == 0 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE servers ADD COLUMN address TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate server address schema: %w", err)
		}
	}
	var periodInterfacesColumn int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('traffic_periods') WHERE name='interfaces_json'`).Scan(&periodInterfacesColumn); err != nil {
		return fmt.Errorf("inspect traffic period schema: %w", err)
	}
	if periodInterfacesColumn == 0 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE traffic_periods ADD COLUMN interfaces_json TEXT NOT NULL DEFAULT '[]'`); err != nil {
			return fmt.Errorf("migrate traffic period schema: %w", err)
		}
		// A legacy database has no way to reconstruct interface selections for
		// closed historical rows. The currently active row can still be
		// recovered from the authoritative allowance without relabelling every
		// historical period with today's configuration.
		now := FormatPersistedTime(time.Now())
		if _, err := tx.ExecContext(ctx, `UPDATE traffic_periods SET interfaces_json=(SELECT interfaces_json FROM traffic_allowances a WHERE a.server_id=traffic_periods.server_id AND a.scope=traffic_periods.scope AND a.direction=traffic_periods.direction) WHERE interfaces_json='[]' AND period_start <= ? AND period_end > ? AND EXISTS(SELECT 1 FROM traffic_allowances a WHERE a.server_id=traffic_periods.server_id AND a.scope=traffic_periods.scope AND a.direction=traffic_periods.direction)`, now, now); err != nil {
			return fmt.Errorf("backfill active traffic period interfaces: %w", err)
		}
	}
	// Older Checkpoint A databases were created before temporal gap bounds
	// existed. ALTER TABLE is kept separate because SQLite has no portable
	// ADD COLUMN IF NOT EXISTS form.
	for _, column := range []string{"from_observed_at", "to_observed_at"} {
		var present int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('coverage_gaps') WHERE name=?`, column).Scan(&present); err != nil {
			return fmt.Errorf("inspect coverage gap schema: %w", err)
		}
		if present == 0 {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE coverage_gaps ADD COLUMN `+column+` TEXT`); err != nil {
				return fmt.Errorf("migrate coverage gap schema: %w", err)
			}
		}
	}
	// Schedule-transition state was introduced after the initial package-05
	// schema. Existing databases may already have the table with an earlier
	// column layout; add the durable JSON column before traffic consumers read
	// it. (New databases create the final layout above.)
	var changesTable int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='traffic_allowance_changes'`).Scan(&changesTable); err != nil {
		return fmt.Errorf("inspect traffic allowance change schema: %w", err)
	}
	if changesTable > 0 {
		var previousJSON, proposedJSON int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('traffic_allowance_changes') WHERE name='previous_allowance_json'`).Scan(&previousJSON); err != nil {
			return fmt.Errorf("inspect traffic allowance change columns: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('traffic_allowance_changes') WHERE name='proposed_allowance_json'`).Scan(&proposedJSON); err != nil {
			return fmt.Errorf("inspect traffic allowance proposed column: %w", err)
		}
		if proposedJSON == 0 {
			return errors.New("traffic allowance migration cannot reconstruct missing proposed schedule column")
		}
		if previousJSON == 0 {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE traffic_allowance_changes ADD COLUMN previous_allowance_json TEXT NOT NULL DEFAULT '{}'`); err != nil {
				return fmt.Errorf("migrate traffic allowance change columns: %w", err)
			}
		}
		var invalidPrevious, invalidProposed int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM traffic_allowance_changes WHERE previous_allowance_json='{}'`).Scan(&invalidPrevious); err != nil {
			return fmt.Errorf("validate traffic allowance history: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM traffic_allowance_changes WHERE proposed_allowance_json='{}'`).Scan(&invalidProposed); err != nil {
			return fmt.Errorf("validate proposed traffic allowance history: %w", err)
		}
		if invalidPrevious != 0 || invalidProposed != 0 {
			return fmt.Errorf("traffic allowance migration cannot reconstruct schedule rows (previous=%d proposed=%d)", invalidPrevious, invalidProposed)
		}
	}
	// Only a database whose version-history table was absent before this
	// migration needs the legacy baseline. Running this on every startup would
	// retroactively authorize samples received before a newly-created policy.
	if allowanceVersionsPreexisting == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO traffic_allowance_versions(server_id,scope,direction,effective_at,interfaces_json,allowance_bytes,reset_day,timezone,warnings_json)
SELECT server_id,scope,direction,?,interfaces_json,allowance_bytes,reset_day,timezone,warnings_json FROM traffic_allowances`, FormatPersistedTime(time.Time{})); err != nil {
			return fmt.Errorf("backfill traffic allowance versions: %w", err)
		}
	}
	if err := backfillCollectorEpochMetadataTx(ctx, tx); err != nil {
		return err
	}
	return nil
}

type persistedTimeColumn struct {
	table  string
	column string
}

// migratePersistedTimes converts every SQL timestamp to one fixed-width UTC
// encoding. SQLite can then compare and index the TEXT values exactly down to
// one nanosecond. The transaction intentionally fails closed on a primary-key
// collision (for example two legacy offset spellings of the same key instant)
// instead of silently discarding either durable row.
func migratePersistedTimesTx(ctx context.Context, tx *sql.Tx) error {
	columns := []persistedTimeColumn{
		{"servers", "last_heartbeat"},
		{"metric_samples", "observed_at"}, {"metric_samples", "received_at"},
		{"coverage_gaps", "from_observed_at"}, {"coverage_gaps", "to_observed_at"},
		{"metric_rollups", "bucket_start"}, {"rollup_rebuild_queue", "bucket_start"},
		{"traffic_periods", "period_start"}, {"traffic_periods", "period_end"},
		{"collector_epoch_metadata", "first_seen_at"}, {"collector_epoch_metadata", "last_seen_at"},
		{"collector_epoch_retirements", "retired_at"},
		{"traffic_allowance_versions", "effective_at"}, {"traffic_allowance_changes", "effective_at"},
		{"traffic_allowance_requests", "created_at"}, {"post_process_queue", "created_at"},
		{"log_entries", "timestamp"},
		{"alert_rules", "created_at"}, {"alert_rules", "effective_at"},
		{"alert_states", "pending_since"}, {"alert_states", "firing_since"},
		{"alert_states", "recovered_at"}, {"alert_states", "last_observation"},
		{"alert_states", "last_notified_at"}, {"alert_states", "last_suppressed_at"},
		{"alert_states", "pending_recovery_at"},
		{"alert_history", "occurred_at"},
		{"maintenance_windows", "starts_at"}, {"maintenance_windows", "ends_at"},
		{"incidents", "started_at"}, {"incidents", "ended_at"},
	}
	for _, target := range columns {
		var present int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, target.table, target.column).Scan(&present); err != nil {
			return fmt.Errorf("inspect %s.%s timestamp column: %w", target.table, target.column, err)
		}
		if present == 0 {
			continue
		}
		var after int64
		for {
			query := `SELECT rowid,` + target.column + ` FROM ` + target.table + ` WHERE rowid>? AND ` + target.column + ` IS NOT NULL AND ` + target.column + `<>'' ORDER BY rowid LIMIT ?`
			rows, err := tx.QueryContext(ctx, query, after, MaxPageItems)
			if err != nil {
				return fmt.Errorf("read %s.%s timestamps: %w", target.table, target.column, err)
			}
			type timestampRow struct {
				rowID int64
				value string
			}
			batch := make([]timestampRow, 0, MaxPageItems)
			for rows.Next() {
				var row timestampRow
				if err := rows.Scan(&row.rowID, &row.value); err != nil {
					_ = rows.Close()
					return fmt.Errorf("scan %s.%s timestamp: %w", target.table, target.column, err)
				}
				batch = append(batch, row)
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return fmt.Errorf("read %s.%s timestamp rows: %w", target.table, target.column, err)
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("close %s.%s timestamp rows: %w", target.table, target.column, err)
			}
			if len(batch) == 0 {
				break
			}
			for _, row := range batch {
				parsed, err := time.Parse(time.RFC3339Nano, row.value)
				if err != nil {
					return fmt.Errorf("parse %s.%s timestamp %q: %w", target.table, target.column, row.value, err)
				}
				canonical := FormatPersistedTime(parsed)
				if canonical != row.value {
					update := `UPDATE ` + target.table + ` SET ` + target.column + `=? WHERE rowid=?`
					if _, err := tx.ExecContext(ctx, update, canonical, row.rowID); err != nil {
						return fmt.Errorf("canonicalize %s.%s timestamp: %w", target.table, target.column, err)
					}
				}
				after = row.rowID
			}
		}
	}

	return nil
}

// backfillCollectorEpochMetadata seeds the last-seen table for databases
// created before epoch metadata was introduced. Legacy rows are treated as
// newly observed so an upgrade never retires replay/coverage state
// immediately; normal ingestion updates the timestamp from then on.
func backfillCollectorEpochMetadataTx(ctx context.Context, tx *sql.Tx) error {
	now := FormatPersistedTime(time.Now())
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO collector_epoch_metadata(server_id,collector_epoch,first_seen_at,last_seen_at)
SELECT server_id,collector_epoch,?,? FROM (
  SELECT DISTINCT server_id,collector_epoch FROM metric_samples
  UNION SELECT DISTINCT server_id,collector_epoch FROM coverage_gaps
  UNION SELECT DISTINCT server_id,collector_epoch FROM sequence_frontiers
  UNION SELECT DISTINCT server_id,collector_epoch FROM traffic_usage_ledger
  UNION SELECT DISTINCT server_id,collector_epoch FROM traffic_usage_tombstones
  UNION SELECT DISTINCT server_id,collector_epoch FROM metric_sample_tombstones
  UNION SELECT DISTINCT server_id,collector_epoch FROM post_process_queue
)`, now, now)
	if err != nil {
		return fmt.Errorf("backfill collector epoch metadata: %w", err)
	}
	return nil
}
