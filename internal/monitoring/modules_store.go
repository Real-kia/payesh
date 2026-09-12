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

// ErrModuleRevisionConflict is returned when a lifecycle transition's
// expected revision no longer matches the durable row, mirroring the traffic
// allowance CAS convention so a stale browser tab cannot silently overwrite a
// concurrent install/enable/disable/remove action.
var ErrModuleRevisionConflict = errors.New("module_revision_conflict")

// GetModuleInstallation returns the durable per-server module lifecycle
// state. A server that has never touched this module has no row; that is
// reported as the synthetic "unavailable" state at revision 0 rather than an
// error, so callers can always CAS from a known starting point.
func (s *Store) GetModuleInstallation(ctx context.Context, serverID contracts.ServerID, moduleID string) (contracts.ModuleInstallation, error) {
	if !validStoreServerID(serverID) {
		return contracts.ModuleInstallation{}, errors.New("server_id must be a bounded URL-safe identifier")
	}
	if !isSafeMetricName(moduleID) {
		return contracts.ModuleInstallation{}, errors.New("module_id is invalid")
	}
	row := s.db.QueryRowContext(ctx, `SELECT version,state,revision,updated_at,error_json FROM module_installations WHERE server_id=? AND module_id=?`, string(serverID), moduleID)
	installation, err := scanModuleInstallation(row, serverID, moduleID)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.ModuleInstallation{ServerID: serverID, ModuleID: moduleID, State: contracts.ModuleUnavailable, Revision: 0, UpdatedAt: time.Now().UTC()}, nil
	}
	return installation, err
}

// ListModuleInstallations returns every module a server has ever touched.
// Modules never staged on this server are not represented; the catalog is
// the source of truth for what is offered.
func (s *Store) ListModuleInstallations(ctx context.Context, serverID contracts.ServerID) ([]contracts.ModuleInstallation, error) {
	if !validStoreServerID(serverID) {
		return nil, errors.New("server_id must be a bounded URL-safe identifier")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT module_id,version,state,revision,updated_at,error_json FROM module_installations WHERE server_id=? ORDER BY module_id ASC`, string(serverID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	installations := make([]contracts.ModuleInstallation, 0, 8)
	for rows.Next() {
		var moduleID, version, state, updatedAt string
		var revision int64
		var errorJSON sql.NullString
		if err := rows.Scan(&moduleID, &version, &state, &revision, &updatedAt, &errorJSON); err != nil {
			return nil, err
		}
		installation, err := buildModuleInstallation(serverID, moduleID, version, state, revision, updatedAt, errorJSON)
		if err != nil {
			return nil, err
		}
		installations = append(installations, installation)
	}
	return installations, rows.Err()
}

func scanModuleInstallation(row *sql.Row, serverID contracts.ServerID, moduleID string) (contracts.ModuleInstallation, error) {
	var version, state, updatedAt string
	var revision int64
	var errorJSON sql.NullString
	if err := row.Scan(&version, &state, &revision, &updatedAt, &errorJSON); err != nil {
		return contracts.ModuleInstallation{}, err
	}
	return buildModuleInstallation(serverID, moduleID, version, state, revision, updatedAt, errorJSON)
}

func buildModuleInstallation(serverID contracts.ServerID, moduleID, version, state string, revision int64, updatedAt string, errorJSON sql.NullString) (contracts.ModuleInstallation, error) {
	parsedUpdatedAt, err := time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}
	installation := contracts.ModuleInstallation{
		ServerID: serverID, ModuleID: moduleID, Version: version,
		State: contracts.ModuleState(state), Revision: uint64(revision), UpdatedAt: parsedUpdatedAt,
	}
	if errorJSON.Valid && errorJSON.String != "" {
		var moduleError contracts.Error
		if err := json.Unmarshal([]byte(errorJSON.String), &moduleError); err != nil {
			return contracts.ModuleInstallation{}, err
		}
		installation.Error = &moduleError
	}
	return installation, nil
}

// TransitionModuleInstallation atomically compare-and-swaps the durable
// lifecycle row from expectedRevision to next, stamping the new revision and
// timestamp itself. A mismatch (including "no row yet" when expectedRevision
// is nonzero, or "row already exists" when it is zero) fails closed with
// ErrModuleRevisionConflict rather than silently reordering two concurrent
// lifecycle requests for the same module.
func (s *Store) TransitionModuleInstallation(ctx context.Context, serverID contracts.ServerID, moduleID string, expectedRevision uint64, next contracts.ModuleInstallation) (contracts.ModuleInstallation, error) {
	return s.transitionModuleInstallation(ctx, serverID, moduleID, expectedRevision, next, "", "")
}

// TransitionModuleInstallationAudited performs the module CAS and its
// redacted lifecycle audit insert in one SQLite transaction. A successful
// state transition therefore cannot become durable without its audit record.
func (s *Store) TransitionModuleInstallationAudited(ctx context.Context, serverID contracts.ServerID, moduleID string, expectedRevision uint64, next contracts.ModuleInstallation, action, result string) (contracts.ModuleInstallation, error) {
	if action == "" || result == "" {
		return contracts.ModuleInstallation{}, errors.New("module audit action and result are required")
	}
	return s.transitionModuleInstallation(ctx, serverID, moduleID, expectedRevision, next, action, result)
}

func (s *Store) transitionModuleInstallation(ctx context.Context, serverID contracts.ServerID, moduleID string, expectedRevision uint64, next contracts.ModuleInstallation, auditAction, auditResult string) (contracts.ModuleInstallation, error) {
	if !validStoreServerID(serverID) {
		return contracts.ModuleInstallation{}, errors.New("server_id must be a bounded URL-safe identifier")
	}
	if !isSafeMetricName(moduleID) {
		return contracts.ModuleInstallation{}, errors.New("module_id is invalid")
	}
	if expectedRevision == ^uint64(0) {
		return contracts.ModuleInstallation{}, errors.New("module revision is exhausted")
	}
	next.ServerID, next.ModuleID = serverID, moduleID
	next.Revision = expectedRevision + 1
	next.UpdatedAt = time.Now().UTC()
	if err := next.Validate(); err != nil {
		return contracts.ModuleInstallation{}, err
	}
	var errorJSON any
	if next.Error != nil {
		encoded, err := json.Marshal(next.Error)
		if err != nil {
			return contracts.ModuleInstallation{}, err
		}
		errorJSON = string(encoded)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
INSERT INTO module_installations(server_id,module_id,version,state,revision,updated_at,error_json) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(server_id,module_id) DO UPDATE SET version=excluded.version,state=excluded.state,revision=excluded.revision,updated_at=excluded.updated_at,error_json=excluded.error_json
WHERE module_installations.revision=?`,
		string(serverID), moduleID, next.Version, string(next.State), int64(next.Revision), FormatPersistedTime(next.UpdatedAt), errorJSON, int64(expectedRevision))
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}
	if changed != 1 {
		return contracts.ModuleInstallation{}, ErrModuleRevisionConflict
	}
	if auditAction != "" {
		auditID := fmt.Sprintf("module:%s:%s:%d", serverID, moduleID, next.Revision)
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events(id,occurred_at,actor_type,action,target_type,target_id,result,revision,redacted) VALUES(?,?,?,?,?,?,?,?,1)`,
			auditID, FormatPersistedTime(next.UpdatedAt), "system", auditAction, "module", fmt.Sprintf("%s/%s", serverID, moduleID), auditResult, int64(next.Revision)); err != nil {
			return contracts.ModuleInstallation{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM audit_events WHERE id IN (SELECT id FROM audit_events ORDER BY occurred_at DESC,id DESC LIMIT -1 OFFSET ?)`, MaxAuditEvents); err != nil {
		return contracts.ModuleInstallation{}, err
	}
	if err := tx.Commit(); err != nil {
		return contracts.ModuleInstallation{}, err
	}
	return next, nil
}

// ModuleLifecycleRequestRecord is the stored outcome of one idempotent
// install/enable/disable/remove request, mirroring
// TrafficAllowanceRequestRecord.
type ModuleLifecycleRequestRecord struct {
	RequestHash string
	ResultJSON  string
}

func (s *Store) GetModuleLifecycleRequest(ctx context.Context, serverID contracts.ServerID, moduleID, idempotencyKey string) (ModuleLifecycleRequestRecord, bool, error) {
	if !validStoreServerID(serverID) || !isSafeMetricName(moduleID) || idempotencyKey == "" || len(idempotencyKey) > 128 {
		return ModuleLifecycleRequestRecord{}, false, errors.New("invalid module lifecycle request identity")
	}
	var record ModuleLifecycleRequestRecord
	err := s.db.QueryRowContext(ctx, `SELECT request_hash,result_json FROM module_lifecycle_requests WHERE server_id=? AND module_id=? AND idempotency_key=?`, string(serverID), moduleID, idempotencyKey).Scan(&record.RequestHash, &record.ResultJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ModuleLifecycleRequestRecord{}, false, nil
	}
	if err != nil {
		return ModuleLifecycleRequestRecord{}, false, err
	}
	return record, true, nil
}

func (s *Store) SaveModuleLifecycleRequest(ctx context.Context, serverID contracts.ServerID, moduleID, idempotencyKey, requestHash string, resultJSON []byte) error {
	if !validStoreServerID(serverID) || !isSafeMetricName(moduleID) || idempotencyKey == "" || len(idempotencyKey) > 128 || requestHash == "" || len(requestHash) > 128 || len(resultJSON) == 0 || len(resultJSON) > contracts.MaxEnvelopeBytes {
		return errors.New("invalid module lifecycle request record")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO module_lifecycle_requests(server_id,module_id,idempotency_key,request_hash,result_json,created_at) VALUES(?,?,?,?,?,?)`,
		string(serverID), moduleID, idempotencyKey, requestHash, string(resultJSON), FormatPersistedTime(time.Now()))
	return err
}
