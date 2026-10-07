package monitoring

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

// ErrControlPolicyRevisionConflict mirrors ErrModuleRevisionConflict for the
// shared control-policy CAS.
var ErrControlPolicyRevisionConflict = errors.New("control_policy_revision_conflict")
var ErrControlPolicyUniqueConflict = errors.New("control_policy_unique_key_conflict")
var ErrControlPolicyRequestInProgress = errors.New("control_policy_request_in_progress")
var ErrControlPolicyRequestConflict = errors.New("control_policy_request_conflict")

func controlPolicyID(serverID contracts.ServerID, moduleID, targetKind, targetName string) string {
	return string(serverID) + ":" + moduleID + ":" + targetKind + ":" + targetName
}

// GetControlPolicy returns the durable per-target policy, or a synthetic
// "pending" row at revision 0 if this exact target has never had a policy
// requested — the same "always CAS from a known starting point" convention
// GetModuleInstallation uses.
func (s *Store) GetControlPolicy(ctx context.Context, serverID contracts.ServerID, moduleID, targetKind, targetName string) (contracts.ControlPolicy, error) {
	if !validStoreServerID(serverID) || !isSafeMetricName(moduleID) {
		return contracts.ControlPolicy{}, errors.New("invalid control policy identity")
	}
	if targetKind != "service" && targetKind != "process-group" && targetKind != "interface" && targetKind != "local-port" {
		return contracts.ControlPolicy{}, errors.New("target_kind must be service, process-group, interface, or local-port")
	}
	if targetName == "" || len(targetName) > 256 {
		return contracts.ControlPolicy{}, errors.New("target_name must be 1..256 characters")
	}
	row := s.db.QueryRowContext(ctx, `SELECT kind,state,parameters_json,revision,updated_at,error_json FROM control_policies WHERE server_id=? AND module_id=? AND target_kind=? AND target_name=?`,
		string(serverID), moduleID, targetKind, targetName)
	policy, err := scanControlPolicy(row, serverID, moduleID, targetKind, targetName)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.ControlPolicy{
			ID: controlPolicyID(serverID, moduleID, targetKind, targetName), ServerID: serverID, ModuleID: moduleID,
			TargetKind: targetKind, TargetName: targetName, State: contracts.ControlPolicyPending,
			Parameters: json.RawMessage(`{}`), Revision: 0, UpdatedAt: time.Now().UTC(),
		}, nil
	}
	return policy, err
}

// ListControlPolicies returns every control policy ever requested for a
// server, optionally filtered to one module (empty moduleID lists all).
func (s *Store) ListControlPolicies(ctx context.Context, serverID contracts.ServerID, moduleID string) ([]contracts.ControlPolicy, error) {
	if !validStoreServerID(serverID) {
		return nil, errors.New("server_id must be a bounded URL-safe identifier")
	}
	query := `SELECT module_id,target_kind,target_name,kind,state,parameters_json,revision,updated_at,error_json FROM control_policies WHERE server_id=?`
	args := []any{string(serverID)}
	if moduleID != "" {
		if !isSafeMetricName(moduleID) {
			return nil, errors.New("module_id is invalid")
		}
		query += ` AND module_id=?`
		args = append(args, moduleID)
	}
	query += ` ORDER BY module_id ASC, target_kind ASC, target_name ASC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	policies := make([]contracts.ControlPolicy, 0, 8)
	for rows.Next() {
		var moduleID, targetKind, targetName, kind, state, parametersJSON, updatedAt string
		var revision int64
		var errorJSON sql.NullString
		if err := rows.Scan(&moduleID, &targetKind, &targetName, &kind, &state, &parametersJSON, &revision, &updatedAt, &errorJSON); err != nil {
			return nil, err
		}
		policy, err := buildControlPolicy(serverID, moduleID, targetKind, targetName, kind, state, parametersJSON, revision, updatedAt, errorJSON)
		if err != nil {
			return nil, err
		}
		policies = append(policies, policy)
	}
	return policies, rows.Err()
}

func scanControlPolicy(row *sql.Row, serverID contracts.ServerID, moduleID, targetKind, targetName string) (contracts.ControlPolicy, error) {
	var kind, state, parametersJSON, updatedAt string
	var revision int64
	var errorJSON sql.NullString
	if err := row.Scan(&kind, &state, &parametersJSON, &revision, &updatedAt, &errorJSON); err != nil {
		return contracts.ControlPolicy{}, err
	}
	return buildControlPolicy(serverID, moduleID, targetKind, targetName, kind, state, parametersJSON, revision, updatedAt, errorJSON)
}

func buildControlPolicy(serverID contracts.ServerID, moduleID, targetKind, targetName, kind, state, parametersJSON string, revision int64, updatedAt string, errorJSON sql.NullString) (contracts.ControlPolicy, error) {
	parsedUpdatedAt, err := time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return contracts.ControlPolicy{}, err
	}
	policy := contracts.ControlPolicy{
		ID: controlPolicyID(serverID, moduleID, targetKind, targetName), ServerID: serverID, ModuleID: moduleID,
		Kind: kind, TargetKind: targetKind, TargetName: targetName, State: contracts.ControlPolicyState(state),
		Parameters: json.RawMessage(parametersJSON), Revision: uint64(revision), UpdatedAt: parsedUpdatedAt,
	}
	if errorJSON.Valid && errorJSON.String != "" {
		var policyError contracts.Error
		if err := json.Unmarshal([]byte(errorJSON.String), &policyError); err != nil {
			return contracts.ControlPolicy{}, err
		}
		policy.Error = &policyError
	}
	return policy, nil
}

// TransitionControlPolicy atomically compare-and-swaps the durable policy
// row, mirroring TransitionModuleInstallation: one INSERT .. ON CONFLICT ..
// DO UPDATE .. WHERE revision=? statement handles both "create the first
// row" and "advance an existing row" without a separate read-then-write race
// window.
func (s *Store) TransitionControlPolicy(ctx context.Context, serverID contracts.ServerID, moduleID, targetKind, targetName string, expectedRevision uint64, next contracts.ControlPolicy) (contracts.ControlPolicy, error) {
	return s.transitionControlPolicy(ctx, serverID, moduleID, targetKind, targetName, expectedRevision, next, "", "", false)
}

// TransitionControlPolicyWithUniqueKey extends the normal policy CAS with an
// atomic SQLite claim for a package-defined identity (Port Traffic uses its
// protocol/interface/port/direction/tuple/path tuple). previousUniqueKey is
// released only as part of the same transaction. A reverted transition
// releases the new claim as well.
func (s *Store) TransitionControlPolicyWithUniqueKey(ctx context.Context, serverID contracts.ServerID, moduleID, targetKind, targetName string, expectedRevision uint64, next contracts.ControlPolicy, previousUniqueKey, uniqueKey string) (contracts.ControlPolicy, error) {
	if uniqueKey == "" {
		return contracts.ControlPolicy{}, errors.New("control policy unique key is required")
	}
	return s.transitionControlPolicy(ctx, serverID, moduleID, targetKind, targetName, expectedRevision, next, previousUniqueKey, uniqueKey, true)
}

func (s *Store) transitionControlPolicy(ctx context.Context, serverID contracts.ServerID, moduleID, targetKind, targetName string, expectedRevision uint64, next contracts.ControlPolicy, previousUniqueKey, uniqueKey string, useUniqueKey bool) (contracts.ControlPolicy, error) {
	if expectedRevision == ^uint64(0) {
		return contracts.ControlPolicy{}, errors.New("control policy revision is exhausted")
	}
	next.ServerID, next.ModuleID, next.TargetKind, next.TargetName = serverID, moduleID, targetKind, targetName
	next.ID = controlPolicyID(serverID, moduleID, targetKind, targetName)
	next.Revision = expectedRevision + 1
	next.UpdatedAt = time.Now().UTC()
	if len(next.Parameters) == 0 {
		next.Parameters = json.RawMessage(`{}`)
	}
	if err := next.Validate(); err != nil {
		return contracts.ControlPolicy{}, err
	}
	var errorJSON any
	if next.Error != nil {
		encoded, err := json.Marshal(next.Error)
		if err != nil {
			return contracts.ControlPolicy{}, err
		}
		errorJSON = string(encoded)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return contracts.ControlPolicy{}, err
	}
	defer tx.Rollback()
	if err := requireServerCommandAuthorityTx(ctx, tx, serverID); err != nil {
		return contracts.ControlPolicy{}, err
	}
	if useUniqueKey && next.State != contracts.ControlPolicyReverted && next.State != contracts.ControlPolicyFailed {
		result, err := tx.ExecContext(ctx, `INSERT INTO control_policy_unique_keys(server_id,module_id,unique_key,target_kind,target_name) VALUES(?,?,?,?,?) ON CONFLICT(server_id,module_id,unique_key) DO UPDATE SET target_kind=excluded.target_kind,target_name=excluded.target_name WHERE control_policy_unique_keys.target_kind=? AND control_policy_unique_keys.target_name=?`, string(serverID), moduleID, uniqueKey, targetKind, targetName, targetKind, targetName)
		if err != nil {
			return contracts.ControlPolicy{}, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return contracts.ControlPolicy{}, err
		}
		if changed != 1 {
			return contracts.ControlPolicy{}, ErrControlPolicyUniqueConflict
		}
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO control_policies(server_id,module_id,target_kind,target_name,kind,state,parameters_json,revision,updated_at,error_json) VALUES(?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(server_id,module_id,target_kind,target_name) DO UPDATE SET kind=excluded.kind,state=excluded.state,parameters_json=excluded.parameters_json,revision=excluded.revision,updated_at=excluded.updated_at,error_json=excluded.error_json
WHERE control_policies.revision=?`,
		string(serverID), moduleID, targetKind, targetName, next.Kind, string(next.State), string(next.Parameters), int64(next.Revision), FormatPersistedTime(next.UpdatedAt), errorJSON, int64(expectedRevision))
	if err != nil {
		return contracts.ControlPolicy{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return contracts.ControlPolicy{}, err
	}
	if changed != 1 {
		return contracts.ControlPolicy{}, ErrControlPolicyRevisionConflict
	}
	if useUniqueKey {
		if previousUniqueKey != "" && previousUniqueKey != uniqueKey {
			if _, err := tx.ExecContext(ctx, `DELETE FROM control_policy_unique_keys WHERE server_id=? AND module_id=? AND unique_key=? AND target_kind=? AND target_name=?`, string(serverID), moduleID, previousUniqueKey, targetKind, targetName); err != nil {
				return contracts.ControlPolicy{}, err
			}
		}
		if next.State == contracts.ControlPolicyReverted || next.State == contracts.ControlPolicyFailed {
			if _, err := tx.ExecContext(ctx, `DELETE FROM control_policy_unique_keys WHERE server_id=? AND module_id=? AND unique_key=? AND target_kind=? AND target_name=?`, string(serverID), moduleID, uniqueKey, targetKind, targetName); err != nil {
				return contracts.ControlPolicy{}, err
			}
		}
	}
	auditID := fmt.Sprintf("%s:%s:%d", next.ID, next.State, next.Revision)
	resultName := "success"
	if next.State == contracts.ControlPolicyFailed {
		resultName = "failure"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events(id,occurred_at,actor_type,action,target_type,target_id,result,revision,redacted) VALUES(?,?,?,?,?,?,?,?,1)`,
		auditID, FormatPersistedTime(next.UpdatedAt), "system", "control-policy."+string(next.State), "control-policy", next.ID, resultName, int64(next.Revision)); err != nil {
		return contracts.ControlPolicy{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM audit_events WHERE id IN (SELECT id FROM audit_events ORDER BY occurred_at DESC,id DESC LIMIT -1 OFFSET ?)`, MaxAuditEvents); err != nil {
		return contracts.ControlPolicy{}, err
	}
	if err := tx.Commit(); err != nil {
		return contracts.ControlPolicy{}, err
	}
	return next, nil
}

// ControlPolicyRequestRecord is the stored outcome of one idempotent
// preview/apply/revert request, mirroring ModuleLifecycleRequestRecord.
type ControlPolicyRequestRecord struct {
	RequestHash string
	ResultJSON  string
}

// ClaimControlPolicyRequest reserves an idempotency key in one SQLite write.
// A placeholder result means another request owns the work; callers must not
// execute the operation a second time. The claim is intentionally separate
// from the policy CAS because module operations may perform kernel work before
// their final durable result is known.
func (s *Store) ClaimControlPolicyRequest(ctx context.Context, serverID contracts.ServerID, moduleID, targetKind, targetName, idempotencyKey, requestHash string) (ControlPolicyRequestRecord, bool, error) {
	if !validStoreServerID(serverID) || idempotencyKey == "" || len(idempotencyKey) > 128 || requestHash == "" {
		return ControlPolicyRequestRecord{}, false, errors.New("invalid control policy request claim")
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO control_policy_requests(server_id,module_id,target_kind,target_name,idempotency_key,request_hash,result_json,created_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, string(serverID), moduleID, targetKind, targetName, idempotencyKey, requestHash, `{}`, FormatPersistedTime(time.Now()))
	if err != nil {
		return ControlPolicyRequestRecord{}, false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return ControlPolicyRequestRecord{}, false, err
	}
	if changed == 1 {
		return ControlPolicyRequestRecord{RequestHash: requestHash, ResultJSON: `{}`}, true, nil
	}
	record, found, err := s.GetControlPolicyRequest(ctx, serverID, moduleID, targetKind, targetName, idempotencyKey)
	if err != nil || !found {
		return record, false, err
	}
	if record.RequestHash != requestHash {
		return ControlPolicyRequestRecord{}, false, ErrControlPolicyRequestConflict
	}
	return record, false, nil
}

// CompleteControlPolicyRequest fills a previously claimed placeholder without
// allowing a completed result to be overwritten by a concurrent retry.
func (s *Store) CompleteControlPolicyRequest(ctx context.Context, serverID contracts.ServerID, moduleID, targetKind, targetName, idempotencyKey, requestHash string, resultJSON []byte) error {
	if !validStoreServerID(serverID) || idempotencyKey == "" || len(idempotencyKey) > 128 || requestHash == "" || len(resultJSON) == 0 || len(resultJSON) > contracts.MaxEnvelopeBytes {
		return errors.New("invalid control policy request result")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE control_policy_requests SET result_json=? WHERE server_id=? AND module_id=? AND target_kind=? AND target_name=? AND idempotency_key=? AND request_hash=? AND result_json=?`, string(resultJSON), string(serverID), moduleID, targetKind, targetName, idempotencyKey, requestHash, `{}`)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrControlPolicyRequestInProgress
	}
	return nil
}

// AbandonControlPolicyRequest removes a claim when the operation failed
// before producing a durable result, allowing a caller to retry safely.
func (s *Store) AbandonControlPolicyRequest(ctx context.Context, serverID contracts.ServerID, moduleID, targetKind, targetName, idempotencyKey, requestHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM control_policy_requests WHERE server_id=? AND module_id=? AND target_kind=? AND target_name=? AND idempotency_key=? AND request_hash=? AND result_json=?`, string(serverID), moduleID, targetKind, targetName, idempotencyKey, requestHash, `{}`)
	return err
}

func (s *Store) GetControlPolicyRequest(ctx context.Context, serverID contracts.ServerID, moduleID, targetKind, targetName, idempotencyKey string) (ControlPolicyRequestRecord, bool, error) {
	if !validStoreServerID(serverID) || idempotencyKey == "" || len(idempotencyKey) > 128 {
		return ControlPolicyRequestRecord{}, false, errors.New("invalid control policy request identity")
	}
	var record ControlPolicyRequestRecord
	err := s.db.QueryRowContext(ctx, `SELECT request_hash,result_json FROM control_policy_requests WHERE server_id=? AND module_id=? AND target_kind=? AND target_name=? AND idempotency_key=?`,
		string(serverID), moduleID, targetKind, targetName, idempotencyKey).Scan(&record.RequestHash, &record.ResultJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ControlPolicyRequestRecord{}, false, nil
	}
	if err != nil {
		return ControlPolicyRequestRecord{}, false, err
	}
	return record, true, nil
}

func (s *Store) SaveControlPolicyRequest(ctx context.Context, serverID contracts.ServerID, moduleID, targetKind, targetName, idempotencyKey, requestHash string, resultJSON []byte) error {
	if !validStoreServerID(serverID) || idempotencyKey == "" || len(idempotencyKey) > 128 || requestHash == "" || len(resultJSON) == 0 || len(resultJSON) > contracts.MaxEnvelopeBytes {
		return errors.New("invalid control policy request record")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO control_policy_requests(server_id,module_id,target_kind,target_name,idempotency_key,request_hash,result_json,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		string(serverID), moduleID, targetKind, targetName, idempotencyKey, requestHash, string(resultJSON), FormatPersistedTime(time.Now()))
	return err
}
