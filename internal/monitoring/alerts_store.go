package monitoring

// This file contains the durable package-05 state seams. Keeping the SQL in
// monitoring preserves the single SQLite connection, retention budget, and
// foreign-key ownership established by package 03; the traffic and alert
// engines only depend on these typed methods.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

var ErrAlertStateConflict = errors.New("alert state was concurrently updated")

type AlertRulePage struct {
	Items      []contracts.AlertRule `json:"items"`
	NextCursor string                `json:"next_cursor,omitempty"`
}

type AlertStatePage struct {
	Items      []contracts.AlertState `json:"items"`
	NextCursor string                 `json:"next_cursor,omitempty"`
}

type AlertHistoryPage struct {
	Items      []contracts.AlertHistoryEvent `json:"events"`
	NextCursor string                        `json:"next_cursor,omitempty"`
}

type MaintenanceWindowPage struct {
	Items      []contracts.MaintenanceWindow `json:"items"`
	NextCursor string                        `json:"next_cursor,omitempty"`
}

// TrafficAllowanceChange is the durable hand-off between an old calendar
// schedule and a newly proposed one. The allowance amount may change
// immediately, while the reset-day/timezone boundary takes effect at
// EffectiveAt (the end of the existing period).
type TrafficAllowanceChange struct {
	ServerID    contracts.ServerID
	Scope       string
	Direction   string
	EffectiveAt time.Time
	Previous    contracts.TrafficAllowance
	Proposed    contracts.TrafficAllowance
}

type TrafficAllowanceRequestRecord struct {
	RequestHash string
	ResultJSON  []byte
}

// ListTrafficAllowances returns the bounded set of accounting configurations
// for one server. It is used by the ingestion adapter to apply every selected
// allowance to a newly accepted counter sample.
func (s *Store) ListTrafficAllowances(ctx context.Context, serverID contracts.ServerID) ([]contracts.TrafficAllowance, error) {
	if !validStoreServerID(serverID) {
		return nil, errors.New("server_id must be a bounded URL-safe identifier")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT scope,direction,interfaces_json,allowance_bytes,reset_day,timezone,warnings_json FROM traffic_allowances WHERE server_id=? ORDER BY scope,direction LIMIT ?`, string(serverID), MaxTrafficAllowances+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	allowances := make([]contracts.TrafficAllowance, 0, 64)
	for rows.Next() {
		var allowance contracts.TrafficAllowance
		var interfacesJSON, bytesText, warningsJSON string
		var resetDay int
		if err := rows.Scan(&allowance.Scope, &allowance.Direction, &interfacesJSON, &bytesText, &resetDay, &allowance.Timezone, &warningsJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(interfacesJSON), &allowance.Interfaces); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(warningsJSON), &allowance.WarningPercentages); err != nil {
			return nil, err
		}
		var parseErr error
		allowance.AllowanceBytes, parseErr = strconv.ParseUint(bytesText, 10, 64)
		if parseErr != nil || resetDay < 1 || resetDay > 31 {
			return nil, errors.New("invalid persisted traffic allowance")
		}
		allowance.ResetDay = uint8(resetDay)
		if err := allowance.Validate(); err != nil {
			return nil, err
		}
		allowances = append(allowances, allowance)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(allowances) > MaxTrafficAllowances {
		return nil, ErrTrafficAllowanceLimit
	}
	return allowances, nil
}

func (s *Store) UpsertTrafficAllowance(ctx context.Context, serverID contracts.ServerID, allowance contracts.TrafficAllowance) error {
	if !validStoreServerID(serverID) {
		return errors.New("server_id must be a bounded URL-safe identifier")
	}
	if err := allowance.Validate(); err != nil {
		return err
	}
	interfaces, err := json.Marshal(allowance.Interfaces)
	if err != nil {
		return err
	}
	warnings, err := json.Marshal(allowance.WarningPercentages)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM traffic_allowances WHERE server_id=?`, string(serverID)).Scan(&count); err != nil {
		_ = tx.Rollback()
		return err
	}
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM traffic_allowances WHERE server_id=? AND scope=? AND direction=?)`, string(serverID), allowance.Scope, allowance.Direction).Scan(&existing); err != nil {
		_ = tx.Rollback()
		return err
	}
	if existing == 0 && count >= MaxTrafficAllowances {
		_ = tx.Rollback()
		return ErrTrafficAllowanceLimit
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO traffic_allowances(server_id,scope,direction,interfaces_json,allowance_bytes,reset_day,timezone,warnings_json) VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(server_id,scope,direction) DO UPDATE SET interfaces_json=excluded.interfaces_json,allowance_bytes=excluded.allowance_bytes,reset_day=excluded.reset_day,timezone=excluded.timezone,warnings_json=excluded.warnings_json`,
		string(serverID), allowance.Scope, allowance.Direction, string(interfaces), strconv.FormatUint(allowance.AllowanceBytes, 10), allowance.ResetDay, allowance.Timezone, string(warnings)); err != nil {
		_ = tx.Rollback()
		return err
	}
	// The legacy/store-level setup API has no clock parameter. Treat its value
	// as the baseline policy; runtime owner edits use the timestamped traffic
	// manager path and append later versions.
	if _, err := tx.ExecContext(ctx, `INSERT INTO traffic_allowance_versions(server_id,scope,direction,effective_at,interfaces_json,allowance_bytes,reset_day,timezone,warnings_json) VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(server_id,scope,direction,effective_at) DO UPDATE SET interfaces_json=excluded.interfaces_json,allowance_bytes=excluded.allowance_bytes,reset_day=excluded.reset_day,timezone=excluded.timezone,warnings_json=excluded.warnings_json`,
		string(serverID), allowance.Scope, allowance.Direction, FormatPersistedTime(time.Time{}), string(interfaces), strconv.FormatUint(allowance.AllowanceBytes, 10), allowance.ResetDay, allowance.Timezone, string(warnings)); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) GetTrafficAllowance(ctx context.Context, serverID contracts.ServerID, scope, direction string) (contracts.TrafficAllowance, bool, error) {
	if !validStoreServerID(serverID) || !isSafeMetricName(scope) || (direction != "inbound" && direction != "outbound" && direction != "combined") {
		return contracts.TrafficAllowance{}, false, errors.New("invalid traffic allowance identity")
	}
	var allowance contracts.TrafficAllowance
	var interfaces, bytes, warnings string
	var resetDay int
	err := s.db.QueryRowContext(ctx, `SELECT scope,direction,interfaces_json,allowance_bytes,reset_day,timezone,warnings_json FROM traffic_allowances WHERE server_id=? AND scope=? AND direction=?`, string(serverID), scope, direction).
		Scan(&allowance.Scope, &allowance.Direction, &interfaces, &bytes, &resetDay, &allowance.Timezone, &warnings)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.TrafficAllowance{}, false, nil
	}
	if err != nil {
		return contracts.TrafficAllowance{}, false, err
	}
	if err := json.Unmarshal([]byte(interfaces), &allowance.Interfaces); err != nil {
		return contracts.TrafficAllowance{}, false, err
	}
	if err := json.Unmarshal([]byte(warnings), &allowance.WarningPercentages); err != nil {
		return contracts.TrafficAllowance{}, false, err
	}
	allowance.AllowanceBytes, err = strconv.ParseUint(bytes, 10, 64)
	if err != nil {
		return contracts.TrafficAllowance{}, false, err
	}
	if resetDay < 0 || resetDay > 255 {
		return contracts.TrafficAllowance{}, false, errors.New("invalid persisted traffic reset day")
	}
	allowance.ResetDay = uint8(resetDay)
	return allowance, true, allowance.Validate()
}

// SaveTrafficAllowanceChange records a schedule transition. It intentionally
// stores only bounded JSON/config fields and never a caller-owned pointer, so
// the pending decision survives process restarts and retries.
func (s *Store) SaveTrafficAllowanceChange(ctx context.Context, change TrafficAllowanceChange) error {
	if !validStoreServerID(change.ServerID) || !isSafeMetricName(change.Scope) || change.Direction == "" || change.EffectiveAt.IsZero() {
		return errors.New("invalid traffic allowance change identity")
	}
	if change.Previous.Scope != change.Scope || change.Previous.Direction != change.Direction || change.Proposed.Scope != change.Scope || change.Proposed.Direction != change.Direction {
		return errors.New("traffic allowance change scope or direction mismatch")
	}
	if err := change.Previous.Validate(); err != nil {
		return err
	}
	if err := change.Proposed.Validate(); err != nil {
		return err
	}
	previous, err := json.Marshal(change.Previous)
	if err != nil {
		return err
	}
	proposed, err := json.Marshal(change.Proposed)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO traffic_allowance_changes(server_id,scope,direction,effective_at,previous_allowance_json,proposed_allowance_json) VALUES(?,?,?,?,?,?)
ON CONFLICT(server_id,scope,direction) DO UPDATE SET effective_at=excluded.effective_at,previous_allowance_json=excluded.previous_allowance_json,proposed_allowance_json=excluded.proposed_allowance_json`,
		string(change.ServerID), change.Scope, change.Direction, FormatPersistedTime(change.EffectiveAt), string(previous), string(proposed))
	return err
}

func (s *Store) GetTrafficAllowanceChange(ctx context.Context, serverID contracts.ServerID, scope, direction string) (TrafficAllowanceChange, bool, error) {
	if !validStoreServerID(serverID) || !isSafeMetricName(scope) || direction == "" || len(direction) > 32 {
		return TrafficAllowanceChange{}, false, errors.New("invalid traffic allowance change identity")
	}
	var change TrafficAllowanceChange
	var effective, previous, proposed string
	err := s.db.QueryRowContext(ctx, `SELECT effective_at,previous_allowance_json,proposed_allowance_json FROM traffic_allowance_changes WHERE server_id=? AND scope=? AND direction=?`, string(serverID), scope, direction).Scan(&effective, &previous, &proposed)
	if errors.Is(err, sql.ErrNoRows) {
		return TrafficAllowanceChange{}, false, nil
	}
	if err != nil {
		return TrafficAllowanceChange{}, false, err
	}
	change.ServerID, change.Scope, change.Direction = serverID, scope, direction
	change.EffectiveAt, err = time.Parse(time.RFC3339Nano, effective)
	if err != nil {
		return TrafficAllowanceChange{}, false, err
	}
	if err := json.Unmarshal([]byte(previous), &change.Previous); err != nil {
		return TrafficAllowanceChange{}, false, err
	}
	if err := json.Unmarshal([]byte(proposed), &change.Proposed); err != nil {
		return TrafficAllowanceChange{}, false, err
	}
	if change.Previous.Scope != scope || change.Previous.Direction != direction || change.Proposed.Scope != scope || change.Proposed.Direction != direction {
		return TrafficAllowanceChange{}, false, errors.New("persisted traffic allowance change identity mismatch")
	}
	if err := change.Previous.Validate(); err != nil {
		return TrafficAllowanceChange{}, false, err
	}
	if err := change.Proposed.Validate(); err != nil {
		return TrafficAllowanceChange{}, false, err
	}
	return change, true, nil
}

func (s *Store) DeleteTrafficAllowanceChange(ctx context.Context, serverID contracts.ServerID, scope, direction string) error {
	if !validStoreServerID(serverID) || !isSafeMetricName(scope) || direction == "" || len(direction) > 32 {
		return errors.New("invalid traffic allowance change identity")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM traffic_allowance_changes WHERE server_id=? AND scope=? AND direction=?`, string(serverID), scope, direction)
	return err
}

func (s *Store) GetTrafficAllowanceRequest(ctx context.Context, serverID contracts.ServerID, idempotencyKey string) (TrafficAllowanceRequestRecord, bool, error) {
	if !validStoreServerID(serverID) || idempotencyKey == "" || len(idempotencyKey) > 128 {
		return TrafficAllowanceRequestRecord{}, false, errors.New("invalid traffic allowance request identity")
	}
	var record TrafficAllowanceRequestRecord
	err := s.db.QueryRowContext(ctx, `SELECT request_hash,result_json FROM traffic_allowance_requests WHERE server_id=? AND idempotency_key=?`, string(serverID), idempotencyKey).Scan(&record.RequestHash, &record.ResultJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return TrafficAllowanceRequestRecord{}, false, nil
	}
	if err != nil {
		return TrafficAllowanceRequestRecord{}, false, err
	}
	return record, true, nil
}

func (s *Store) SaveTrafficAllowanceRequest(ctx context.Context, serverID contracts.ServerID, idempotencyKey, requestHash string, resultJSON []byte) error {
	if !validStoreServerID(serverID) || idempotencyKey == "" || len(idempotencyKey) > 128 || requestHash == "" || len(requestHash) > 128 || len(resultJSON) == 0 || len(resultJSON) > contracts.MaxEnvelopeBytes {
		return errors.New("invalid traffic allowance request record")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO traffic_allowance_requests(server_id,idempotency_key,request_hash,result_json,created_at) VALUES(?,?,?,?,?)`, string(serverID), idempotencyKey, requestHash, string(resultJSON), FormatPersistedTime(time.Now()))
	return err
}

// AddTrafficUsage creates the period identity if necessary and atomically adds
// a delta. It is deliberately separate from UpsertTrafficPeriod so two node
// samples cannot lose an update through a read/modify/write race.
func (s *Store) AddTrafficUsage(ctx context.Context, serverID contracts.ServerID, period contracts.TrafficPeriod, delta uint64, continuity string) (contracts.TrafficPeriod, error) {
	if !validStoreServerID(serverID) {
		return contracts.TrafficPeriod{}, errors.New("server_id must be a bounded URL-safe identifier")
	}
	if err := period.Validate(); err != nil {
		return contracts.TrafficPeriod{}, err
	}
	if continuity != "complete" && continuity != "gap" && continuity != "uncertain" {
		return contracts.TrafficPeriod{}, errors.New("invalid traffic continuity")
	}
	if ^uint64(0)-period.CountedBytes < delta {
		return contracts.TrafficPeriod{}, errors.New("traffic usage exceeds uint64")
	}
	protected, err := s.isProtectedTrafficPeriod(ctx, serverID, period)
	if err != nil {
		return contracts.TrafficPeriod{}, err
	}
	if !protected {
		if err := s.ensureWritable(ctx); err != nil {
			return contracts.TrafficPeriod{}, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return contracts.TrafficPeriod{}, err
	}
	rollback := func(err error) (contracts.TrafficPeriod, error) {
		_ = tx.Rollback()
		return contracts.TrafficPeriod{}, err
	}
	from, to := FormatPersistedTime(period.From), FormatPersistedTime(period.To)
	interfaces, err := json.Marshal(period.Interfaces)
	if err != nil {
		return rollback(err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO traffic_periods(server_id,scope,period_start,period_end,timezone,allowance_bytes,direction,counted_bytes,continuity,interfaces_json) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(server_id,scope,period_start,direction) DO NOTHING`,
		string(serverID), period.Scope, from, to, period.Timezone, strconv.FormatUint(period.AllowanceBytes, 10), period.Direction, "0", period.Continuity, string(interfaces))
	if err != nil {
		return rollback(err)
	}
	var existingTo, existingTimezone, existingAllowance, existingCounted, existingContinuity, existingInterfaces string
	err = tx.QueryRowContext(ctx, `SELECT period_end,timezone,allowance_bytes,counted_bytes,continuity,interfaces_json FROM traffic_periods WHERE server_id=? AND scope=? AND period_start=? AND direction=?`, string(serverID), period.Scope, from, period.Direction).Scan(&existingTo, &existingTimezone, &existingAllowance, &existingCounted, &existingContinuity, &existingInterfaces)
	if err != nil {
		return rollback(err)
	}
	// Every period policy field is historical identity. Usage ingestion may
	// increment accounting and degrade continuity, but it cannot relabel an
	// existing row with current allowance configuration.
	period.To, err = time.Parse(time.RFC3339Nano, existingTo)
	if err != nil {
		return rollback(err)
	}
	period.Timezone = existingTimezone
	period.AllowanceBytes, err = strconv.ParseUint(existingAllowance, 10, 64)
	if err != nil {
		return rollback(errors.New("invalid traffic allowance bytes"))
	}
	if err := json.Unmarshal([]byte(existingInterfaces), &period.Interfaces); err != nil {
		return rollback(err)
	}
	counted, err := strconv.ParseUint(existingCounted, 10, 64)
	if err != nil {
		return rollback(err)
	}
	if ^uint64(0)-counted < delta {
		return rollback(errors.New("traffic usage exceeds uint64"))
	}
	counted += delta
	mergedContinuity := mergeTrafficContinuity(existingContinuity, continuity)
	_, err = tx.ExecContext(ctx, `UPDATE traffic_periods SET counted_bytes=?,continuity=? WHERE server_id=? AND scope=? AND period_start=? AND direction=?`,
		strconv.FormatUint(counted, 10), mergedContinuity, string(serverID), period.Scope, from, period.Direction)
	if err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return contracts.TrafficPeriod{}, err
	}
	period.CountedBytes = counted
	period.Continuity = mergedContinuity
	return period, nil
}

func mergeTrafficContinuity(left, right string) string {
	if left == "gap" || right == "gap" {
		return "gap"
	}
	if left == "uncertain" || right == "uncertain" {
		return "uncertain"
	}
	return "complete"
}

func validStoreServerID(serverID contracts.ServerID) bool {
	if len(serverID) < 16 || len(serverID) > 128 {
		return false
	}
	for _, r := range string(serverID) {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func (s *Store) SaveAlertRule(ctx context.Context, rule contracts.AlertRule) error {
	if err := rule.Validate(); err != nil {
		return err
	}
	if rule.CreatedAt.IsZero() {
		rule.CreatedAt = time.Now().UTC()
	}
	if rule.EffectiveAt.IsZero() {
		rule.EffectiveAt = rule.CreatedAt
	}
	if !rule.Enabled {
		// Disabling a rule may append recovery history and update its grouped
		// incident. Make the retention-pressure check before opening the write
		// transaction so all of those durable changes remain atomic.
		if err := s.ensureWritable(ctx); err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := saveAlertRuleTxWithDisable(ctx, tx, rule, time.Now().UTC()); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateAlertRule(ctx context.Context, rule contracts.AlertRule) error {
	if err := rule.Validate(); err != nil {
		return err
	}
	if rule.CreatedAt.IsZero() {
		rule.CreatedAt = time.Now().UTC()
	}
	if rule.EffectiveAt.IsZero() {
		rule.EffectiveAt = rule.CreatedAt
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := createAlertRule(ctx, tx, rule); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// createAlertRule is insert-only: a racing request must never overwrite the
// durable policy attached to an idempotency key.
func createAlertRule(ctx context.Context, executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, rule contracts.AlertRule) error {
	var exists int
	if err := executor.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM alert_rules WHERE id=?)`, rule.ID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if err := ensureAlertRuleCapacity(ctx, executor, rule.ServerID); err != nil {
			return err
		}
	}
	var server any
	if rule.ServerID != "" {
		server = string(rule.ServerID)
	}
	result, err := executor.ExecContext(ctx, `INSERT INTO alert_rules(id,name,expression,server_id,enabled,duration_seconds,recovery_threshold,reminder_seconds,group_key,idempotency_key,created_at,effective_at,disable_reason) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, rule.ID, rule.Name, rule.Expression, server, boolInt(rule.Enabled), rule.DurationSeconds, nullableFloat(rule.RecoveryThreshold), rule.ReminderSeconds, rule.GroupKey, rule.IdempotencyKey, FormatPersistedTime(rule.CreatedAt), FormatPersistedTime(rule.EffectiveAt), rule.DisableReason)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 0 {
		return err
	}
	var existing contracts.AlertRule
	var serverText string
	var enabled int
	var recovery sql.NullFloat64
	var created string
	var effective string
	err = executor.QueryRowContext(ctx, `SELECT id,name,expression,COALESCE(server_id,''),enabled,duration_seconds,recovery_threshold,reminder_seconds,COALESCE(group_key,''),idempotency_key,created_at,COALESCE(effective_at,created_at),COALESCE(disable_reason,'') FROM alert_rules WHERE id=?`, rule.ID).Scan(&existing.ID, &existing.Name, &existing.Expression, &serverText, &enabled, &existing.DurationSeconds, &recovery, &existing.ReminderSeconds, &existing.GroupKey, &existing.IdempotencyKey, &created, &effective, &existing.DisableReason)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("alert rule creation raced and was not retained")
	}
	if err != nil {
		return err
	}
	existing.ServerID = contracts.ServerID(serverText)
	existing.Enabled = enabled != 0
	existing.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return err
	}
	existing.EffectiveAt, err = time.Parse(time.RFC3339Nano, effective)
	if err != nil {
		return err
	}
	if recovery.Valid {
		value := recovery.Float64
		existing.RecoveryThreshold = &value
	}
	if !alertRulesEquivalent(existing, rule) {
		return errors.New("alert rule idempotency conflict")
	}
	return nil
}

func alertRulesEquivalent(a, b contracts.AlertRule) bool {
	if a.ID != b.ID || a.Name != b.Name || a.Expression != b.Expression || a.ServerID != b.ServerID || a.Enabled != b.Enabled || a.DurationSeconds != b.DurationSeconds || a.ReminderSeconds != b.ReminderSeconds || a.GroupKey != b.GroupKey || a.IdempotencyKey != b.IdempotencyKey {
		return false
	}
	if a.RecoveryThreshold == nil || b.RecoveryThreshold == nil {
		return a.RecoveryThreshold == nil && b.RecoveryThreshold == nil
	}
	return *a.RecoveryThreshold == *b.RecoveryThreshold
}

// SaveAlertRuleTx is the transaction-backed form used when an alert rule is
// part of a larger configuration mutation. Keeping the SQL adapter here lets
// callers commit rule policy and its owning revision/idempotency record as one
// unit without opening a second SQLite connection.
func (s *Store) SaveAlertRuleTx(ctx context.Context, tx *sql.Tx, rule contracts.AlertRule) error {
	if tx == nil {
		return errors.New("alert rule transaction is required")
	}
	if err := rule.Validate(); err != nil {
		return err
	}
	if rule.CreatedAt.IsZero() {
		rule.CreatedAt = time.Now().UTC()
	}
	if rule.EffectiveAt.IsZero() {
		rule.EffectiveAt = rule.CreatedAt
	}
	return saveAlertRuleTxWithDisable(ctx, tx, rule, time.Now().UTC())
}

func saveAlertRuleTxWithDisable(ctx context.Context, tx *sql.Tx, rule contracts.AlertRule, at time.Time) error {
	existing, found, err := (&Store{}).GetAlertRuleTx(ctx, tx, rule.ID)
	if err != nil {
		return err
	}
	if found {
		if existing.ServerID != rule.ServerID && rule.ServerID != "" {
			if err := ensureAlertRuleServerCapacity(ctx, tx, rule.ServerID); err != nil {
				return err
			}
		}
		if alertRulePolicyChanged(existing, rule) || !existing.Enabled && rule.Enabled {
			rule.EffectiveAt = at.UTC()
			if rule.Enabled {
				if err := terminalizeAlertStatesTx(ctx, tx, rule.ID, at, "rule_policy_changed"); err != nil {
					return err
				}
			}
		} else {
			rule.EffectiveAt = existing.EffectiveAt
		}
	} else if err := ensureAlertRuleCapacity(ctx, tx, rule.ServerID); err != nil {
		return err
	}
	if !rule.Enabled {
		if err := terminalizeAlertStatesTx(ctx, tx, rule.ID, at, "rule_disabled"); err != nil {
			return err
		}
	}
	return saveAlertRule(ctx, tx, rule)
}

func ensureAlertRuleCapacity(ctx context.Context, executor interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, serverID contracts.ServerID) error {
	var total int
	if err := executor.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_rules`).Scan(&total); err != nil {
		return err
	}
	if total >= MaxAlertRules {
		return fmt.Errorf("%w: maximum %d total rules (enabled and disabled)", ErrAlertRuleLimit, MaxAlertRules)
	}
	if serverID != "" {
		return ensureAlertRuleServerCapacity(ctx, executor, serverID)
	}
	return nil
}

func ensureAlertRuleServerCapacity(ctx context.Context, executor interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, serverID contracts.ServerID) error {
	var serverTotal int
	if err := executor.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_rules WHERE server_id=?`, string(serverID)).Scan(&serverTotal); err != nil {
		return err
	}
	if serverTotal >= MaxAlertRulesPerServer {
		return fmt.Errorf("%w: maximum %d rules for server %s (enabled and disabled)", ErrAlertRuleLimit, MaxAlertRulesPerServer, serverID)
	}
	return nil
}

func alertRulePolicyChanged(left, right contracts.AlertRule) bool {
	if left.Expression != right.Expression || left.ServerID != right.ServerID || left.DurationSeconds != right.DurationSeconds || left.ReminderSeconds != right.ReminderSeconds || left.GroupKey != right.GroupKey {
		return true
	}
	if left.RecoveryThreshold == nil || right.RecoveryThreshold == nil {
		return left.RecoveryThreshold != nil || right.RecoveryThreshold != nil
	}
	return *left.RecoveryThreshold != *right.RecoveryThreshold
}

// disableAlertStatesTx terminalizes every pending/firing state owned by a
// disabled rule in the same transaction as the policy update. Recovery events
// and grouped-incident closure therefore cannot be lost between a successful
// rule PATCH and a later evaluator pass.
func terminalizeAlertStatesTx(ctx context.Context, tx *sql.Tx, ruleID string, at time.Time, reason string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,rule_id,COALESCE(server_id,''),state,pending_since,firing_since,recovered_at,last_observation,last_value,last_notified_at,last_suppressed_at,pending_recovery_at,COALESCE(incident_id,''),COALESCE(precision_warning,''),revision FROM alert_states WHERE rule_id=? ORDER BY id`, ruleID)
	if err != nil {
		return err
	}
	states := make([]contracts.AlertState, 0, 4)
	for rows.Next() {
		state, scanErr := scanAlertState(rows)
		if scanErr != nil {
			_ = rows.Close()
			return scanErr
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	at = at.UTC()
	for _, state := range states {
		if reason == "rule_policy_changed" {
			state.LastObservation = nil
			state.LastValue = nil
			state.PrecisionWarning = ""
		}
		if state.State != "pending" && state.State != "firing" {
			if reason == "rule_policy_changed" {
				if err := saveAlertStateTx(ctx, tx, state); err != nil {
					return err
				}
			}
			continue
		}
		previousState := state.State
		state.State = "recovered"
		state.PendingSince = nil
		state.FiringSince = nil
		state.RecoveredAt = alertTimePtr(at)
		state.LastNotifiedAt = nil
		state.LastSuppressedAt = nil
		state.PendingRecoveryAt = nil
		state.PrecisionWarning = ""
		if err := saveAlertStateTx(ctx, tx, state); err != nil {
			return err
		}
		event := contracts.AlertHistoryEvent{
			ID:         terminalAlertEventID(state.ID, at, reason),
			AlertID:    state.RuleID,
			ServerID:   state.ServerID,
			State:      "recovered",
			OccurredAt: at,
			Reason:     reason,
			Value:      state.LastValue,
			IncidentID: state.IncidentID,
		}
		if err := appendAlertHistoryTx(ctx, tx, event); err != nil {
			return err
		}
		if state.IncidentID == "" {
			continue
		}
		incident, found, incidentErr := getIncidentTx(ctx, tx, state.IncidentID)
		if incidentErr != nil {
			return incidentErr
		}
		if !found {
			continue
		}
		incident.Events = appendIncidentEvent(incident.Events, event)
		if previousState == "pending" && incident.State == "recovered" {
			// A stale pending state may still point at a grouped incident from a
			// previous evaluation. Let saveIncidentTx recompute its terminal
			// state from durable sibling states below.
			incident.State = "open"
			incident.EndedAt = nil
		}
		if err := saveIncidentTx(ctx, tx, incident, state.ID); err != nil {
			return err
		}
	}
	return nil
}

func getIncidentTx(ctx context.Context, tx *sql.Tx, id string) (contracts.IncidentSnapshot, bool, error) {
	row := tx.QueryRowContext(ctx, `SELECT id,COALESCE(server_id,''),group_key,state,started_at,ended_at,summary,events_json,evidence_json,truncated FROM incidents WHERE id=?`, id)
	incident, err := scanIncident(row)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.IncidentSnapshot{}, false, nil
	}
	return incident, err == nil, err
}

func appendIncidentEvent(events []contracts.AlertHistoryEvent, event contracts.AlertHistoryEvent) []contracts.AlertHistoryEvent {
	updated := append(append([]contracts.AlertHistoryEvent(nil), events...), event)
	if len(updated) > 200 {
		updated = updated[len(updated)-200:]
	}
	return updated
}

func terminalAlertEventID(stateID string, at time.Time, reason string) string {
	h := sha256.Sum256([]byte(stateID + "\x00" + reason + "\x00" + at.UTC().Format(time.RFC3339Nano)))
	return "event-" + hex.EncodeToString(h[:16])
}

func alertTimePtr(value time.Time) *time.Time { return &value }

func saveAlertRule(ctx context.Context, executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, rule contracts.AlertRule) error {
	var server any
	if rule.ServerID != "" {
		server = string(rule.ServerID)
	}
	_, err := executor.ExecContext(ctx, `INSERT INTO alert_rules(id,name,expression,server_id,enabled,duration_seconds,recovery_threshold,reminder_seconds,group_key,idempotency_key,created_at,effective_at,disable_reason) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name,expression=excluded.expression,server_id=excluded.server_id,enabled=excluded.enabled,duration_seconds=excluded.duration_seconds,recovery_threshold=excluded.recovery_threshold,reminder_seconds=excluded.reminder_seconds,group_key=excluded.group_key,effective_at=excluded.effective_at,disable_reason=excluded.disable_reason`,
		rule.ID, rule.Name, rule.Expression, server, boolInt(rule.Enabled), rule.DurationSeconds, nullableFloat(rule.RecoveryThreshold), rule.ReminderSeconds, rule.GroupKey, rule.IdempotencyKey, FormatPersistedTime(rule.CreatedAt), FormatPersistedTime(rule.EffectiveAt), rule.DisableReason)
	return err
}

func (s *Store) GetAlertRule(ctx context.Context, id string) (contracts.AlertRule, bool, error) {
	if id == "" || len(id) > 128 {
		return contracts.AlertRule{}, false, errors.New("invalid alert rule id")
	}
	row := s.db.QueryRowContext(ctx, `SELECT id,name,expression,COALESCE(server_id,''),enabled,duration_seconds,recovery_threshold,reminder_seconds,COALESCE(group_key,''),idempotency_key,created_at,COALESCE(effective_at,created_at),COALESCE(disable_reason,'') FROM alert_rules WHERE id=?`, id)
	rule, err := scanAlertRule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.AlertRule{}, false, nil
	}
	return rule, err == nil, err
}

// GetAlertRuleTx reads a rule through an existing transaction. It is kept
// separate from GetAlertRule because a SQLite store intentionally allows only
// one open connection; using the regular DB handle while a transaction is
// active could otherwise block forever.
func (s *Store) GetAlertRuleTx(ctx context.Context, tx *sql.Tx, id string) (contracts.AlertRule, bool, error) {
	if tx == nil {
		return contracts.AlertRule{}, false, errors.New("alert rule transaction is required")
	}
	if id == "" || len(id) > 128 {
		return contracts.AlertRule{}, false, errors.New("invalid alert rule id")
	}
	row := tx.QueryRowContext(ctx, `SELECT id,name,expression,COALESCE(server_id,''),enabled,duration_seconds,recovery_threshold,reminder_seconds,COALESCE(group_key,''),idempotency_key,created_at,COALESCE(effective_at,created_at),COALESCE(disable_reason,'') FROM alert_rules WHERE id=?`, id)
	rule, err := scanAlertRule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.AlertRule{}, false, nil
	}
	return rule, err == nil, err
}

// ListAlertRulesByServerTx returns a bounded rule set for transaction-backed
// reconciliation. The limit is intentionally caller-selected so an invalid
// database cannot turn a configuration request into an unbounded scan.
func (s *Store) ListAlertRulesByServerTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, limit int) ([]contracts.AlertRule, error) {
	if tx == nil {
		return nil, errors.New("alert rule transaction is required")
	}
	if !validStoreServerID(serverID) || limit < 1 || limit > MaxAlertRulesPerServer {
		return nil, errors.New("invalid alert rule transaction query")
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,name,expression,COALESCE(server_id,''),enabled,duration_seconds,recovery_threshold,reminder_seconds,COALESCE(group_key,''),idempotency_key,created_at,COALESCE(effective_at,created_at),COALESCE(disable_reason,'') FROM alert_rules WHERE server_id=? ORDER BY id LIMIT ?`, string(serverID), limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rules := make([]contracts.AlertRule, 0, limit)
	for rows.Next() {
		if len(rules) == limit {
			return nil, errors.New("alert rule count exceeds transaction bound")
		}
		rule, scanErr := scanAlertRule(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return rules, nil
}

func (s *Store) ListAlertRules(ctx context.Context, limit int, cursor string) (AlertRulePage, error) {
	limit = clampLimit(limit)
	query := `SELECT id,name,expression,COALESCE(server_id,''),enabled,duration_seconds,recovery_threshold,reminder_seconds,COALESCE(group_key,''),idempotency_key,created_at,COALESCE(effective_at,created_at),COALESCE(disable_reason,'') FROM alert_rules`
	args := []any{}
	if cursor != "" {
		if len(cursor) > 128 {
			return AlertRulePage{}, errors.New("invalid alert rule cursor")
		}
		query += ` WHERE id > ?`
		args = append(args, cursor)
	}
	query += ` ORDER BY id LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return AlertRulePage{}, err
	}
	defer rows.Close()
	page := AlertRulePage{Items: make([]contracts.AlertRule, 0, limit)}
	for rows.Next() {
		rule, err := scanAlertRule(rows)
		if err != nil {
			return AlertRulePage{}, err
		}
		if len(page.Items) == limit {
			page.NextCursor = page.Items[len(page.Items)-1].ID
			break
		}
		page.Items = append(page.Items, rule)
	}
	return page, rows.Err()
}

func (s *Store) SaveAlertState(ctx context.Context, state contracts.AlertState) error {
	if err := state.Validate(); err != nil {
		return err
	}
	return s.withAlertStateRevision(ctx, state, func(tx *sql.Tx) error { return saveAlertStateTx(ctx, tx, state) })
}

// ErrAlertRuleChanged tells an evaluator that its rule snapshot is no longer
// the durable policy. Callers should discard the result and retry from the
// current rule list; committing an observation against an edited or disabled
// rule would resurrect stale alert state after an owner mutation.
var ErrAlertRuleChanged = errors.New("alert rule changed during evaluation")

// SaveAlertStateForRule is the rule-guarded form used by the evaluator for
// state-only transitions (pending/uncertain observations). The rule check and
// state write share one SQLite transaction, so a concurrent PATCH cannot race
// a stale evaluator into creating or advancing state after the policy has
// changed.
func (s *Store) SaveAlertStateForRule(ctx context.Context, rule contracts.AlertRule, state contracts.AlertState) error {
	if err := rule.Validate(); err != nil {
		return err
	}
	if err := state.Validate(); err != nil {
		return err
	}
	if rule.ID != state.RuleID || !rule.Enabled {
		return ErrAlertRuleChanged
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := ensureAlertRuleSnapshotTx(ctx, tx, rule); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := saveAlertStateTx(ctx, tx, state); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) withAlertStateRevision(ctx context.Context, state contracts.AlertState, save func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := save(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// SaveAlertEvaluation commits the state transition and its durable history /
// incident mutation as one unit. Alert evaluation must not advance the state
// machine if the corresponding evidence cannot be retained (for example when
// the history budget is under storage pressure).
func (s *Store) SaveAlertEvaluation(ctx context.Context, state contracts.AlertState, event *contracts.AlertHistoryEvent, incident *contracts.IncidentSnapshot) error {
	return s.saveAlertEvaluation(ctx, nil, state, event, incident)
}

// SaveAlertEvaluationForRule is the rule-guarded form used by the evaluator
// for transitions that also append history/incidents. The durable rule is
// re-read in the same transaction as the state/history write; stale enabled
// snapshots therefore fail closed when a concurrent PATCH or generated-rule
// reconciliation commits first.
func (s *Store) SaveAlertEvaluationForRule(ctx context.Context, rule contracts.AlertRule, state contracts.AlertState, event *contracts.AlertHistoryEvent, incident *contracts.IncidentSnapshot) error {
	if err := rule.Validate(); err != nil {
		return err
	}
	if rule.ID != state.RuleID || !rule.Enabled {
		return ErrAlertRuleChanged
	}
	return s.saveAlertEvaluation(ctx, &rule, state, event, incident)
}

// CommitAlertNotificationCadence records a successful asynchronous delivery
// only while the alert is still the same firing incident and its cadence clock
// still has the value observed by the enqueueing evaluation. A late worker can
// therefore never overwrite a recovery or a newer delivery attempt.
func (s *Store) CommitAlertNotificationCadence(ctx context.Context, stateID, incidentID string, previous *time.Time, deliveredAt time.Time) (bool, error) {
	if stateID == "" || len(stateID) > 128 || incidentID == "" || len(incidentID) > 128 || deliveredAt.IsZero() {
		return false, errors.New("invalid alert notification cadence update")
	}
	expected := nullableTime(previous)
	result, err := s.db.ExecContext(ctx, `UPDATE alert_states SET last_notified_at=?,revision=revision+1
WHERE id=? AND state='firing' AND incident_id=? AND ((last_notified_at IS NULL AND ? IS NULL) OR last_notified_at=?)`,
		FormatPersistedTime(deliveredAt), stateID, incidentID, expected, expected)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

// CommitAlertRecoveryDelivery clears the durable retry marker only if this is
// still the same recovered incident. Late workers cannot consume recovery for
// a reopened alert or a newer recovery transition.
func (s *Store) CommitAlertRecoveryDelivery(ctx context.Context, stateID, incidentID string, recoveredAt time.Time) (bool, error) {
	if stateID == "" || len(stateID) > 128 || incidentID == "" || len(incidentID) > 128 || recoveredAt.IsZero() {
		return false, errors.New("invalid alert recovery delivery update")
	}
	timestamp := FormatPersistedTime(recoveredAt)
	result, err := s.db.ExecContext(ctx, `UPDATE alert_states SET pending_recovery_at=NULL,revision=revision+1
WHERE id=? AND state='recovered' AND incident_id=? AND pending_recovery_at=?`, stateID, incidentID, timestamp)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (s *Store) saveAlertEvaluation(ctx context.Context, rule *contracts.AlertRule, state contracts.AlertState, event *contracts.AlertHistoryEvent, incident *contracts.IncidentSnapshot) error {
	if err := state.Validate(); err != nil {
		return err
	}
	if event != nil {
		if err := event.Validate(); err != nil {
			return err
		}
	}
	if incident != nil {
		bounded, err := boundIncident(*incident)
		if err != nil {
			return err
		}
		*incident = bounded
	}
	if event != nil || incident != nil {
		if err := s.ensureWritable(ctx); err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rollback := func(err error) error {
		_ = tx.Rollback()
		return err
	}
	if rule != nil {
		if err := ensureAlertRuleSnapshotTx(ctx, tx, *rule); err != nil {
			return rollback(err)
		}
	}
	if err := saveAlertStateTx(ctx, tx, state); err != nil {
		return rollback(err)
	}
	if event != nil {
		if err := appendAlertHistoryTx(ctx, tx, *event); err != nil {
			return rollback(err)
		}
	}
	if incident != nil {
		recoveringStateID := ""
		if event != nil && event.State == "recovered" {
			recoveringStateID = state.ID
		}
		if err := saveIncidentTx(ctx, tx, *incident, recoveringStateID); err != nil {
			return rollback(err)
		}
	}
	return tx.Commit()
}

func ensureAlertRuleSnapshotTx(ctx context.Context, tx *sql.Tx, expected contracts.AlertRule) error {
	actual, found, err := (&Store{}).GetAlertRuleTx(ctx, tx, expected.ID)
	if err != nil {
		return err
	}
	if !found || !alertRuleSnapshotMatches(actual, expected) {
		return ErrAlertRuleChanged
	}
	return nil
}

// AlertRuleSnapshotCurrent validates a caller-held rule before evaluation.
// The guarded write repeats this check transactionally, closing the race
// between this read and the eventual state/history commit.
func (s *Store) CurrentAlertRuleSnapshot(ctx context.Context, expected contracts.AlertRule) (contracts.AlertRule, bool, error) {
	actual, found, err := s.GetAlertRule(ctx, expected.ID)
	if err != nil || !found {
		return contracts.AlertRule{}, false, err
	}
	return actual, alertRuleSnapshotMatches(actual, expected), nil
}

func alertRuleSnapshotMatches(actual, expected contracts.AlertRule) bool {
	expectedEffectiveAt := expected.EffectiveAt
	if expectedEffectiveAt.IsZero() {
		expectedEffectiveAt = expected.CreatedAt
	}
	effectiveMatches := expectedEffectiveAt.IsZero() || actual.EffectiveAt.Equal(expectedEffectiveAt)
	// Disabled rules are valid snapshots for the evaluator: Evaluate must be
	// able to observe the durable disabled state and return rule_disabled
	// without attempting a state write. State/evaluation save paths separately
	// reject disabled rules, while alertRulesEquivalent still detects a stale
	// enabled/disabled transition.
	return actual.DisableReason == expected.DisableReason && effectiveMatches && alertRulesEquivalent(actual, expected)
}

func saveAlertStateTx(ctx context.Context, tx *sql.Tx, state contracts.AlertState) error {
	if state.Revision == ^uint64(0) {
		return errors.New("alert state revision is exhausted")
	}
	var server any
	if state.ServerID != "" {
		server = string(state.ServerID)
	}
	var existingRevision uint64
	readErr := tx.QueryRowContext(ctx, `SELECT revision FROM alert_states WHERE id=?`, state.ID).Scan(&existingRevision)
	if errors.Is(readErr, sql.ErrNoRows) {
		_, err := tx.ExecContext(ctx, `INSERT INTO alert_states(id,rule_id,server_id,state,pending_since,firing_since,recovered_at,last_observation,last_value,last_notified_at,last_suppressed_at,pending_recovery_at,incident_id,precision_warning,revision) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			state.ID, state.RuleID, server, state.State, nullableTime(state.PendingSince), nullableTime(state.FiringSince), nullableTime(state.RecoveredAt), nullableTime(state.LastObservation), nullableFloat(state.LastValue), nullableTime(state.LastNotifiedAt), nullableTime(state.LastSuppressedAt), nullableTime(state.PendingRecoveryAt), state.IncidentID, state.PrecisionWarning, state.Revision+1)
		if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: alert_states.id") {
			return ErrAlertStateConflict
		}
		return err
	}
	if readErr != nil {
		return readErr
	}
	if existingRevision != state.Revision {
		return ErrAlertStateConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE alert_states SET rule_id=?,server_id=?,state=?,pending_since=?,firing_since=?,recovered_at=?,last_observation=?,last_value=?,last_notified_at=?,last_suppressed_at=?,pending_recovery_at=?,incident_id=?,precision_warning=?,revision=? WHERE id=? AND revision=?`,
		state.RuleID, server, state.State, nullableTime(state.PendingSince), nullableTime(state.FiringSince), nullableTime(state.RecoveredAt), nullableTime(state.LastObservation), nullableFloat(state.LastValue), nullableTime(state.LastNotifiedAt), nullableTime(state.LastSuppressedAt), nullableTime(state.PendingRecoveryAt), state.IncidentID, state.PrecisionWarning, state.Revision+1, state.ID, state.Revision)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrAlertStateConflict
	}
	return nil
}

func appendAlertHistoryTx(ctx context.Context, tx *sql.Tx, event contracts.AlertHistoryEvent) error {
	var server any
	if event.ServerID != "" {
		server = string(event.ServerID)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO alert_history(id,alert_id,server_id,state,occurred_at,reason,value,incident_id) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, event.ID, event.AlertID, server, event.State, FormatPersistedTime(event.OccurredAt), event.Reason, nullableFloat(event.Value), event.IncidentID)
	return err
}

func saveIncidentTx(ctx context.Context, tx *sql.Tx, incident contracts.IncidentSnapshot, recoveringStateID string) error {
	if recoveringStateID != "" {
		var siblingFiring int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM alert_states WHERE incident_id=? AND state='firing' AND id<>?)`, incident.ID, recoveringStateID).Scan(&siblingFiring); err != nil {
			return err
		}
		if siblingFiring != 0 {
			// The state transition being committed is one member of a grouped
			// incident; a sibling still firing keeps the group open.
			incident.State = "open"
			incident.EndedAt = nil
		} else {
			incident.State = "recovered"
			if incident.EndedAt == nil {
				// The caller may have observed a sibling race and supplied an
				// open snapshot. Use the recovery event timestamp when rebuilding
				// the terminal state rather than leaving a recovered incident
				// without an end time.
				for _, event := range incident.Events {
					if event.State == "recovered" && !event.OccurredAt.IsZero() && (incident.EndedAt == nil || event.OccurredAt.After(*incident.EndedAt)) {
						ended := event.OccurredAt.UTC()
						incident.EndedAt = &ended
					}
				}
			}
		}
	}
	// Re-read the durable incident inside the write transaction and merge its
	// bounded evidence before replacing it. Separate engine instances may have
	// built snapshots from the same prior read; blindly writing either snapshot
	// would lose a sibling alert's history.
	var existingStarted, existingSummary, existingEvents, existingEvidence string
	var existingEnded sql.NullString
	var existingState string
	var existingTruncated int
	readErr := tx.QueryRowContext(ctx, `SELECT state,started_at,ended_at,summary,events_json,evidence_json,truncated FROM incidents WHERE id=?`, incident.ID).Scan(&existingState, &existingStarted, &existingEnded, &existingSummary, &existingEvents, &existingEvidence, &existingTruncated)
	if readErr == nil {
		var priorEvents []contracts.AlertHistoryEvent
		var priorEvidence []string
		if err := json.Unmarshal([]byte(existingEvents), &priorEvents); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(existingEvidence), &priorEvidence); err != nil {
			return err
		}
		seenEvents := make(map[string]struct{}, len(priorEvents)+len(incident.Events))
		for _, event := range priorEvents {
			seenEvents[event.ID] = struct{}{}
		}
		mergedEvents := append([]contracts.AlertHistoryEvent(nil), priorEvents...)
		for _, event := range incident.Events {
			if _, ok := seenEvents[event.ID]; !ok {
				mergedEvents = append(mergedEvents, event)
				seenEvents[event.ID] = struct{}{}
			}
		}
		seenEvidence := make(map[string]struct{}, len(priorEvidence)+len(incident.Evidence))
		for _, item := range priorEvidence {
			seenEvidence[item] = struct{}{}
		}
		mergedEvidence := append([]string(nil), priorEvidence...)
		for _, item := range incident.Evidence {
			if _, ok := seenEvidence[item]; !ok {
				mergedEvidence = append(mergedEvidence, item)
				seenEvidence[item] = struct{}{}
			}
		}
		incident.Events = mergedEvents
		incident.Evidence = mergedEvidence
		incident.Truncated = incident.Truncated || existingTruncated != 0
		if parsed, err := time.Parse(time.RFC3339Nano, existingStarted); err == nil && parsed.Before(incident.StartedAt) {
			incident.StartedAt = parsed
		}
	} else if !errors.Is(readErr, sql.ErrNoRows) {
		return readErr
	}
	var boundErr error
	incident, boundErr = boundIncident(incident)
	if boundErr != nil {
		return boundErr
	}
	var server any
	if incident.ServerID != "" {
		server = string(incident.ServerID)
	}
	events, err := json.Marshal(incident.Events)
	if err != nil {
		return err
	}
	evidence, err := json.Marshal(incident.Evidence)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO incidents(id,server_id,group_key,state,started_at,ended_at,summary,events_json,evidence_json,truncated) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET state=excluded.state,ended_at=excluded.ended_at,summary=excluded.summary,events_json=excluded.events_json,evidence_json=excluded.evidence_json,truncated=excluded.truncated`, incident.ID, server, incident.GroupKey, incident.State, FormatPersistedTime(incident.StartedAt), nullableTime(incident.EndedAt), incident.Summary, string(events), string(evidence), boolInt(incident.Truncated))
	return err
}

func boundIncident(incident contracts.IncidentSnapshot) (contracts.IncidentSnapshot, error) {
	// Merge paths can briefly contain the previous 200 events plus a new
	// event. Trim by count before validation so a bounded concurrent update is
	// retained instead of being rejected merely because two writers raced.
	if len(incident.Events) > 200 {
		incident.Events = append([]contracts.AlertHistoryEvent(nil), incident.Events[len(incident.Events)-200:]...)
		incident.Truncated = true
	}
	if len(incident.Evidence) > 200 {
		incident.Evidence = append([]string(nil), incident.Evidence[len(incident.Evidence)-200:]...)
		incident.Truncated = true
	}
	if err := incident.Validate(); err != nil {
		return contracts.IncidentSnapshot{}, err
	}
	for _, event := range incident.Events {
		if err := event.Validate(); err != nil {
			return contracts.IncidentSnapshot{}, err
		}
	}
	const maxIncidentPayloadBytes = 256 << 10
	for {
		events, eventsErr := json.Marshal(incident.Events)
		if eventsErr != nil {
			return contracts.IncidentSnapshot{}, eventsErr
		}
		evidence, evidenceErr := json.Marshal(incident.Evidence)
		if evidenceErr != nil {
			return contracts.IncidentSnapshot{}, evidenceErr
		}
		if len(events)+len(evidence) <= maxIncidentPayloadBytes {
			return incident, nil
		}
		incident.Truncated = true
		if len(incident.Evidence) > 0 {
			incident.Evidence = incident.Evidence[:len(incident.Evidence)-1]
			continue
		}
		if len(incident.Events) > 0 {
			incident.Events = incident.Events[1:]
			continue
		}
		return contracts.IncidentSnapshot{}, errors.New("incident snapshot exceeds 256 KiB")
	}
}

func (s *Store) GetAlertState(ctx context.Context, id string) (contracts.AlertState, bool, error) {
	if id == "" || len(id) > 128 {
		return contracts.AlertState{}, false, errors.New("invalid alert state id")
	}
	row := s.db.QueryRowContext(ctx, `SELECT id,rule_id,COALESCE(server_id,''),state,pending_since,firing_since,recovered_at,last_observation,last_value,last_notified_at,last_suppressed_at,pending_recovery_at,COALESCE(incident_id,''),COALESCE(precision_warning,''),revision FROM alert_states WHERE id=?`, id)
	state, err := scanAlertState(row)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.AlertState{}, false, nil
	}
	return state, err == nil, err
}

// HasFiringAlertStateForIncident reports whether another alert still owns an
// open firing condition. Incident recovery is grouped: one member recovering
// must not close an incident while a sibling rule remains firing.
func (s *Store) HasFiringAlertStateForIncident(ctx context.Context, incidentID, excludeStateID string) (bool, error) {
	if incidentID == "" || len(incidentID) > 128 {
		return false, errors.New("invalid incident id")
	}
	if len(excludeStateID) > 128 {
		return false, errors.New("invalid alert state id")
	}
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM alert_states WHERE incident_id=? AND state='firing' AND id<>?)`, incidentID, excludeStateID).Scan(&exists)
	return exists == 1, err
}

func (s *Store) ListAlertStates(ctx context.Context, serverID contracts.ServerID, limit int, cursor string) (AlertStatePage, error) {
	limit = clampLimit(limit)
	query := `SELECT id,rule_id,COALESCE(server_id,''),state,pending_since,firing_since,recovered_at,last_observation,last_value,last_notified_at,last_suppressed_at,pending_recovery_at,COALESCE(incident_id,''),COALESCE(precision_warning,''),revision FROM alert_states`
	args := []any{}
	if serverID != "" {
		if !validStoreServerID(serverID) {
			return AlertStatePage{}, errors.New("invalid server id")
		}
		query += ` WHERE server_id=?`
		args = append(args, string(serverID))
	}
	if cursor != "" {
		if len(cursor) > 128 {
			return AlertStatePage{}, errors.New("invalid alert state cursor")
		}
		if len(args) == 0 {
			query += ` WHERE id > ?`
		} else {
			query += ` AND id > ?`
		}
		args = append(args, cursor)
	}
	query += ` ORDER BY id LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return AlertStatePage{}, err
	}
	defer rows.Close()
	page := AlertStatePage{Items: make([]contracts.AlertState, 0, limit)}
	for rows.Next() {
		state, err := scanAlertState(rows)
		if err != nil {
			return AlertStatePage{}, err
		}
		if len(page.Items) == limit {
			page.NextCursor = page.Items[len(page.Items)-1].ID
			break
		}
		page.Items = append(page.Items, state)
	}
	return page, rows.Err()
}

func (s *Store) AppendAlertHistory(ctx context.Context, event contracts.AlertHistoryEvent) error {
	if err := event.Validate(); err != nil {
		return err
	}
	if err := s.ensureWritable(ctx); err != nil {
		return err
	}
	var server any
	if event.ServerID != "" {
		server = string(event.ServerID)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO alert_history(id,alert_id,server_id,state,occurred_at,reason,value,incident_id) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, event.ID, event.AlertID, server, event.State, FormatPersistedTime(event.OccurredAt), event.Reason, nullableFloat(event.Value), event.IncidentID)
	return err
}

func (s *Store) ListAlertHistory(ctx context.Context, from, to time.Time, limit int, cursor string) (AlertHistoryPage, error) {
	limit = clampLimit(limit)
	decoded, err := decodeAlertHistoryCursor(cursor)
	if err != nil {
		return AlertHistoryPage{}, err
	}
	query := `SELECT id,alert_id,COALESCE(server_id,''),state,occurred_at,COALESCE(reason,''),value,COALESCE(incident_id,'') FROM alert_history WHERE 1=1`
	args := []any{}
	if !from.IsZero() {
		query += ` AND occurred_at >= ?`
		args = append(args, FormatPersistedTime(from))
	}
	if !to.IsZero() {
		query += ` AND occurred_at <= ?`
		args = append(args, FormatPersistedTime(to))
	}
	if decoded.OccurredAt != "" {
		cursorTime, err := time.Parse(time.RFC3339Nano, decoded.OccurredAt)
		if err != nil {
			return AlertHistoryPage{}, errors.New("invalid alert history cursor")
		}
		persistedCursor := FormatPersistedTime(cursorTime)
		query += ` AND (occurred_at > ? OR (occurred_at = ? AND id > ?))`
		args = append(args, persistedCursor, persistedCursor, decoded.ID)
	}
	query += ` ORDER BY occurred_at,id LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return AlertHistoryPage{}, err
	}
	defer rows.Close()
	page := AlertHistoryPage{Items: make([]contracts.AlertHistoryEvent, 0, limit)}
	for rows.Next() {
		event, err := scanAlertHistory(rows)
		if err != nil {
			return AlertHistoryPage{}, err
		}
		if len(page.Items) == limit {
			last := page.Items[len(page.Items)-1]
			page.NextCursor = encodeAlertHistoryCursor(alertHistoryCursor{OccurredAt: FormatPersistedTime(last.OccurredAt), ID: last.ID})
			break
		}
		page.Items = append(page.Items, event)
	}
	return page, rows.Err()
}

type alertHistoryCursor struct {
	OccurredAt string `json:"occurred_at"`
	ID         string `json:"id"`
}

func encodeAlertHistoryCursor(cursor alertHistoryCursor) string {
	b, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeAlertHistoryCursor(value string) (alertHistoryCursor, error) {
	if value == "" {
		return alertHistoryCursor{}, nil
	}
	if len(value) > 256 {
		return alertHistoryCursor{}, errors.New("invalid alert history cursor")
	}
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return alertHistoryCursor{}, errors.New("invalid alert history cursor")
	}
	var cursor alertHistoryCursor
	if err := json.Unmarshal(b, &cursor); err != nil || cursor.OccurredAt == "" || cursor.ID == "" || len(cursor.ID) > 128 {
		return alertHistoryCursor{}, errors.New("invalid alert history cursor")
	}
	if _, err := time.Parse(time.RFC3339Nano, cursor.OccurredAt); err != nil {
		return alertHistoryCursor{}, errors.New("invalid alert history cursor")
	}
	return cursor, nil
}

func (s *Store) SaveMaintenanceWindow(ctx context.Context, window contracts.MaintenanceWindow) error {
	if err := window.Validate(); err != nil {
		return err
	}
	serverIDs := window.ServerIDs
	if serverIDs == nil {
		serverIDs = []contracts.ServerID{}
	}
	ids, err := json.Marshal(serverIDs)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO maintenance_windows(id,idempotency_key,starts_at,ends_at,server_ids_json,reason) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, window.ID, window.IdempotencyKey, FormatPersistedTime(window.StartsAt), FormatPersistedTime(window.EndsAt), string(ids), window.Reason)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 0 {
		return err
	}
	existing, found, err := s.GetMaintenanceWindowByIdempotency(ctx, window.IdempotencyKey)
	if err != nil {
		return err
	}
	if !found || existing.ID != window.ID || !existing.StartsAt.Equal(window.StartsAt) || !existing.EndsAt.Equal(window.EndsAt) || existing.Reason != window.Reason || len(existing.ServerIDs) != len(window.ServerIDs) {
		return errors.New("maintenance window idempotency conflict")
	}
	for i := range existing.ServerIDs {
		if existing.ServerIDs[i] != window.ServerIDs[i] {
			return errors.New("maintenance window idempotency conflict")
		}
	}
	return nil
}

func (s *Store) GetMaintenanceWindowByIdempotency(ctx context.Context, key string) (contracts.MaintenanceWindow, bool, error) {
	if key == "" || len(key) > 128 {
		return contracts.MaintenanceWindow{}, false, errors.New("invalid maintenance idempotency key")
	}
	row := s.db.QueryRowContext(ctx, `SELECT id,idempotency_key,starts_at,ends_at,server_ids_json,reason FROM maintenance_windows WHERE idempotency_key=?`, key)
	window, err := scanMaintenanceWindow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.MaintenanceWindow{}, false, nil
	}
	return window, err == nil, err
}

func (s *Store) ListMaintenanceWindows(ctx context.Context, limit int, cursor string) (MaintenanceWindowPage, error) {
	limit = clampLimit(limit)
	query := `SELECT id,idempotency_key,starts_at,ends_at,server_ids_json,reason FROM maintenance_windows`
	args := []any{}
	if cursor != "" {
		if len(cursor) > 128 {
			return MaintenanceWindowPage{}, errors.New("invalid maintenance cursor")
		}
		query += ` WHERE id > ?`
		args = append(args, cursor)
	}
	query += ` ORDER BY id LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return MaintenanceWindowPage{}, err
	}
	defer rows.Close()
	page := MaintenanceWindowPage{Items: make([]contracts.MaintenanceWindow, 0, limit)}
	for rows.Next() {
		window, err := scanMaintenanceWindow(rows)
		if err != nil {
			return MaintenanceWindowPage{}, err
		}
		if len(page.Items) == limit {
			page.NextCursor = page.Items[len(page.Items)-1].ID
			break
		}
		page.Items = append(page.Items, window)
	}
	return page, rows.Err()
}

func (s *Store) IsMaintenance(ctx context.Context, serverID contracts.ServerID, at time.Time) (bool, error) {
	if !validStoreServerID(serverID) || at.IsZero() {
		return false, errors.New("invalid maintenance query")
	}
	instant := FormatPersistedTime(at)
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM maintenance_windows WHERE starts_at <= ? AND ends_at > ? AND (server_ids_json='[]' OR EXISTS(SELECT 1 FROM json_each(server_ids_json) WHERE value=?)))`, instant, instant, string(serverID)).Scan(&exists)
	return exists == 1, err
}

func (s *Store) SaveIncident(ctx context.Context, incident contracts.IncidentSnapshot) error {
	var err error
	incident, err = boundIncident(incident)
	if err != nil {
		return err
	}
	if err := s.ensureWritable(ctx); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := saveIncidentTx(ctx, tx, incident, ""); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) GetIncident(ctx context.Context, id string) (contracts.IncidentSnapshot, bool, error) {
	if id == "" || len(id) > 128 {
		return contracts.IncidentSnapshot{}, false, errors.New("invalid incident id")
	}
	row := s.db.QueryRowContext(ctx, `SELECT id,COALESCE(server_id,''),group_key,state,started_at,ended_at,summary,events_json,evidence_json,truncated FROM incidents WHERE id=?`, id)
	incident, err := scanIncident(row)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.IncidentSnapshot{}, false, nil
	}
	return incident, err == nil, err
}

func nullableTime(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return FormatPersistedTime(*value)
}

func parseNullableTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid || value.String == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func scanAlertRule(row interface{ Scan(...any) error }) (contracts.AlertRule, error) {
	var rule contracts.AlertRule
	var server string
	var enabled int
	var recovery sql.NullFloat64
	var created, effective string
	if err := row.Scan(&rule.ID, &rule.Name, &rule.Expression, &server, &enabled, &rule.DurationSeconds, &recovery, &rule.ReminderSeconds, &rule.GroupKey, &rule.IdempotencyKey, &created, &effective, &rule.DisableReason); err != nil {
		return contracts.AlertRule{}, err
	}
	rule.ServerID = contracts.ServerID(server)
	rule.Enabled = enabled != 0
	if recovery.Valid {
		value := recovery.Float64
		rule.RecoveryThreshold = &value
	}
	var err error
	rule.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return contracts.AlertRule{}, err
	}
	rule.EffectiveAt, err = time.Parse(time.RFC3339Nano, effective)
	return rule, err
}

func scanAlertState(row interface{ Scan(...any) error }) (contracts.AlertState, error) {
	var state contracts.AlertState
	var server, pending, firing, recovered, observed, notified, suppressed, pendingRecovery sql.NullString
	var value sql.NullFloat64
	if err := row.Scan(&state.ID, &state.RuleID, &server, &state.State, &pending, &firing, &recovered, &observed, &value, &notified, &suppressed, &pendingRecovery, &state.IncidentID, &state.PrecisionWarning, &state.Revision); err != nil {
		return contracts.AlertState{}, err
	}
	state.ServerID = contracts.ServerID(server.String)
	var err error
	if state.PendingSince, err = parseNullableTime(pending); err != nil {
		return contracts.AlertState{}, err
	}
	if state.FiringSince, err = parseNullableTime(firing); err != nil {
		return contracts.AlertState{}, err
	}
	if state.RecoveredAt, err = parseNullableTime(recovered); err != nil {
		return contracts.AlertState{}, err
	}
	if state.LastObservation, err = parseNullableTime(observed); err != nil {
		return contracts.AlertState{}, err
	}
	if state.LastNotifiedAt, err = parseNullableTime(notified); err != nil {
		return contracts.AlertState{}, err
	}
	if state.LastSuppressedAt, err = parseNullableTime(suppressed); err != nil {
		return contracts.AlertState{}, err
	}
	if state.PendingRecoveryAt, err = parseNullableTime(pendingRecovery); err != nil {
		return contracts.AlertState{}, err
	}
	if value.Valid {
		v := value.Float64
		state.LastValue = &v
	}
	return state, nil
}

func scanAlertHistory(row interface{ Scan(...any) error }) (contracts.AlertHistoryEvent, error) {
	var event contracts.AlertHistoryEvent
	var server, occurred, reason, incident sql.NullString
	var value sql.NullFloat64
	if err := row.Scan(&event.ID, &event.AlertID, &server, &event.State, &occurred, &reason, &value, &incident); err != nil {
		return contracts.AlertHistoryEvent{}, err
	}
	event.ServerID = contracts.ServerID(server.String)
	event.Reason, event.IncidentID = reason.String, incident.String
	var err error
	event.OccurredAt, err = time.Parse(time.RFC3339Nano, occurred.String)
	if err != nil {
		return contracts.AlertHistoryEvent{}, err
	}
	if value.Valid {
		v := value.Float64
		event.Value = &v
	}
	return event, nil
}

func scanMaintenanceWindow(row interface{ Scan(...any) error }) (contracts.MaintenanceWindow, error) {
	var window contracts.MaintenanceWindow
	var starts, ends, ids string
	if err := row.Scan(&window.ID, &window.IdempotencyKey, &starts, &ends, &ids, &window.Reason); err != nil {
		return contracts.MaintenanceWindow{}, err
	}
	var err error
	window.StartsAt, err = time.Parse(time.RFC3339Nano, starts)
	if err != nil {
		return contracts.MaintenanceWindow{}, err
	}
	window.EndsAt, err = time.Parse(time.RFC3339Nano, ends)
	if err != nil {
		return contracts.MaintenanceWindow{}, err
	}
	if err := json.Unmarshal([]byte(ids), &window.ServerIDs); err != nil {
		return contracts.MaintenanceWindow{}, err
	}
	return window, nil
}

func scanIncident(row interface{ Scan(...any) error }) (contracts.IncidentSnapshot, error) {
	var incident contracts.IncidentSnapshot
	var server, started, events, evidence string
	var ended sql.NullString
	var truncated int
	if err := row.Scan(&incident.ID, &server, &incident.GroupKey, &incident.State, &started, &ended, &incident.Summary, &events, &evidence, &truncated); err != nil {
		return contracts.IncidentSnapshot{}, err
	}
	incident.ServerID = contracts.ServerID(server)
	var err error
	incident.StartedAt, err = time.Parse(time.RFC3339Nano, started)
	if err != nil {
		return contracts.IncidentSnapshot{}, err
	}
	if ended.Valid && ended.String != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, ended.String)
		if parseErr != nil {
			return contracts.IncidentSnapshot{}, parseErr
		}
		incident.EndedAt = &parsed
	}
	if err := json.Unmarshal([]byte(events), &incident.Events); err != nil {
		return contracts.IncidentSnapshot{}, err
	}
	if err := json.Unmarshal([]byte(evidence), &incident.Evidence); err != nil {
		return contracts.IncidentSnapshot{}, err
	}
	incident.Truncated = truncated != 0
	return incident, nil
}
