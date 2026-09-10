package monitoring

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

// ErrControlPolicyRevisionConflict mirrors ErrModuleRevisionConflict for the
// shared control-policy CAS.
var ErrControlPolicyRevisionConflict = errors.New("control_policy_revision_conflict")

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
	if targetKind != "service" && targetKind != "process-group" {
		return contracts.ControlPolicy{}, errors.New("target_kind must be service or process-group")
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
	result, err := s.db.ExecContext(ctx, `
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
	return next, nil
}

// ControlPolicyRequestRecord is the stored outcome of one idempotent
// preview/apply/revert request, mirroring ModuleLifecycleRequestRecord.
type ControlPolicyRequestRecord struct {
	RequestHash string
	ResultJSON  string
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
