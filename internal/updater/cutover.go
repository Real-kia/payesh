package updater

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type CutoverPhase string

const (
	CutoverPreflight CutoverPhase = "preflight"
	CutoverBackup    CutoverPhase = "backup"
	CutoverTransfer  CutoverPhase = "transfer"
	CutoverVerify    CutoverPhase = "verify"
	CutoverFreeze    CutoverPhase = "freeze-source"
	CutoverTail      CutoverPhase = "transfer-live-tail"
	CutoverSwitch    CutoverPhase = "switch-authority"
	CutoverRevoke    CutoverPhase = "revoke-old-authority"
	CutoverConfirm   CutoverPhase = "confirm-destination"
	CutoverCleanup   CutoverPhase = "cleanup-source"
	CutoverComplete  CutoverPhase = "completed"
	CutoverRecovery  CutoverPhase = "recovery-required"
)

type RoleCutoverRequest struct {
	ID, SourceRole, DestinationRole string
	SourceID, DestinationID         string
	ManagedNodes                    int
	FleetDisposition                string // migrated or detached
	CleanupSource                   bool
}

type RoleCutoverRecord struct {
	Request   RoleCutoverRequest `json:"request"`
	Phase     CutoverPhase       `json:"phase"`
	LastError string             `json:"last_error,omitempty"`
	UpdatedAt time.Time          `json:"updated_at"`
}

// RoleCutoverHooks are idempotent phase operations. Transfer performs the
// retained-range import; TransferTail repeats export/import after Freeze.
// Cleanup is deliberately separate and runs only after confirmation.
type RoleCutoverHooks struct {
	Preflight, Backup, Transfer, Verify, Freeze, TransferTail func(context.Context, RoleCutoverRequest) error
	SwitchAuthority, RevokeOldAuthority, Confirm, Cleanup     func(context.Context, RoleCutoverRequest) error
	RollbackBeforeSwitch                                      func(context.Context, RoleCutoverRequest) error
}

func validateRoleCutover(req RoleCutoverRequest) error {
	validRole := func(v string) bool { return v == "standalone" || v == "hub" || v == "node" || v == "cli-only" }
	if req.ID == "" || len(req.ID) > 128 || req.SourceID == "" || req.DestinationID == "" || !validRole(req.SourceRole) || !validRole(req.DestinationRole) || req.SourceRole == req.DestinationRole || req.ManagedNodes < 0 {
		return errors.New("updater: invalid role cutover request")
	}
	if req.SourceRole == "hub" && req.DestinationRole == "node" && req.ManagedNodes > 0 && req.FleetDisposition != "migrated" && req.FleetDisposition != "detached" {
		return errors.New("updater: hub still manages nodes; migrate or explicitly detach the fleet first")
	}
	return nil
}

func ensureCutoverJournal(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS role_cutovers (id TEXT PRIMARY KEY, request_json BLOB NOT NULL, phase TEXT NOT NULL, last_error TEXT NOT NULL, updated_at TEXT NOT NULL, busy_until TEXT)`)
	return err
}

// RunRoleCutover creates or resumes a cutover journal. Failures before the
// authority switch may be rolled back and retried. Failures at/after the switch
// become recovery-required: the orchestrator never re-enables the old writer,
// preserving the one-controller invariant.
func RunRoleCutover(ctx context.Context, journal *sql.DB, req RoleCutoverRequest, hooks RoleCutoverHooks, now func() time.Time) (RoleCutoverRecord, error) {
	if journal == nil || now == nil {
		return RoleCutoverRecord{}, errors.New("updater: cutover journal and clock are required")
	}
	if err := validateRoleCutover(req); err != nil {
		return RoleCutoverRecord{}, err
	}
	if err := ensureCutoverJournal(ctx, journal); err != nil {
		return RoleCutoverRecord{}, fmt.Errorf("updater: create cutover journal: %w", err)
	}
	record, found, err := loadCutover(ctx, journal, req.ID)
	if err != nil {
		return record, err
	}
	if found && record.Request != req {
		return record, errors.New("updater: cutover idempotency conflict")
	}
	if found && !validCutoverPhase(record.Phase) {
		return record, errors.New("updater: invalid persisted role cutover phase")
	}
	if !found {
		record = RoleCutoverRecord{Request: req, Phase: CutoverPreflight}
		if err := saveCutover(ctx, journal, record, now()); err != nil {
			return record, err
		}
	}
	if record.Phase == CutoverComplete {
		return record, nil
	}
	if record.Phase == CutoverRecovery {
		return record, errors.New("updater: role cutover requires operator recovery")
	}

	phases := []struct {
		phase, next CutoverPhase
		run         func(context.Context, RoleCutoverRequest) error
		afterSwitch bool
	}{
		{CutoverPreflight, CutoverBackup, hooks.Preflight, false},
		{CutoverBackup, CutoverTransfer, hooks.Backup, false},
		{CutoverTransfer, CutoverVerify, hooks.Transfer, false},
		{CutoverVerify, CutoverFreeze, hooks.Verify, false},
		{CutoverFreeze, CutoverTail, hooks.Freeze, false},
		{CutoverTail, CutoverSwitch, hooks.TransferTail, false},
		{CutoverSwitch, CutoverRevoke, hooks.SwitchAuthority, true},
		{CutoverRevoke, CutoverConfirm, hooks.RevokeOldAuthority, true},
		{CutoverConfirm, CutoverCleanup, hooks.Confirm, true},
	}
	for _, step := range phases {
		if record.Phase != step.phase {
			continue
		}
		claimed, err := claimCutoverPhase(ctx, journal, req.ID, step.phase, now())
		if err != nil {
			return record, err
		}
		if !claimed {
			return record, errors.New("updater: role cutover phase is already running")
		}
		if step.run == nil {
			_ = saveCutover(ctx, journal, record, now())
			return record, fmt.Errorf("updater: role cutover hook %s is required", step.phase)
		}
		if err := step.run(ctx, req); err != nil {
			record.LastError = err.Error()
			if step.afterSwitch {
				record.Phase = CutoverRecovery
			} else if hooks.RollbackBeforeSwitch != nil {
				if rollbackErr := hooks.RollbackBeforeSwitch(ctx, req); rollbackErr != nil {
					record.LastError += "; rollback: " + rollbackErr.Error()
				}
			}
			_ = saveCutover(ctx, journal, record, now())
			return record, fmt.Errorf("updater: role cutover %s: %w", step.phase, err)
		}
		record.Phase, record.LastError = step.next, ""
		if err := saveCutover(ctx, journal, record, now()); err != nil {
			return record, err
		}
	}
	if record.Phase == CutoverCleanup {
		claimed, err := claimCutoverPhase(ctx, journal, req.ID, CutoverCleanup, now())
		if err != nil {
			return record, err
		}
		if !claimed {
			return record, errors.New("updater: role cutover cleanup is already running")
		}
		if req.CleanupSource {
			if hooks.Cleanup == nil {
				return record, errors.New("updater: source cleanup hook is required")
			}
			if err := hooks.Cleanup(ctx, req); err != nil {
				record.LastError = err.Error()
				_ = saveCutover(ctx, journal, record, now())
				return record, fmt.Errorf("updater: cleanup confirmed source: %w", err)
			}
		}
		record.Phase, record.LastError = CutoverComplete, ""
		if err := saveCutover(ctx, journal, record, now()); err != nil {
			return record, err
		}
	}
	return record, nil
}

func validCutoverPhase(phase CutoverPhase) bool {
	switch phase {
	case CutoverPreflight, CutoverBackup, CutoverTransfer, CutoverVerify, CutoverFreeze, CutoverTail, CutoverSwitch, CutoverRevoke, CutoverConfirm, CutoverCleanup, CutoverComplete, CutoverRecovery:
		return true
	default:
		return false
	}
}

func loadCutover(ctx context.Context, db *sql.DB, id string) (RoleCutoverRecord, bool, error) {
	var encoded []byte
	var phase, last, updated string
	err := db.QueryRowContext(ctx, `SELECT request_json,phase,last_error,updated_at FROM role_cutovers WHERE id=?`, id).Scan(&encoded, &phase, &last, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return RoleCutoverRecord{}, false, nil
	}
	if err != nil {
		return RoleCutoverRecord{}, false, err
	}
	var req RoleCutoverRequest
	stamp, parseErr := time.Parse(time.RFC3339Nano, updated)
	if jsonErr := json.Unmarshal(encoded, &req); jsonErr != nil || parseErr != nil {
		return RoleCutoverRecord{}, false, errors.New("updater: invalid persisted role cutover")
	}
	return RoleCutoverRecord{Request: req, Phase: CutoverPhase(phase), LastError: last, UpdatedAt: stamp}, true, nil
}

func saveCutover(ctx context.Context, db *sql.DB, record RoleCutoverRecord, at time.Time) error {
	encoded, err := json.Marshal(record.Request)
	if err != nil {
		return err
	}
	stamp := at.UTC()
	if stamp.IsZero() {
		return errors.New("updater: cutover clock returned zero time")
	}
	_, err = db.ExecContext(ctx, `INSERT INTO role_cutovers(id,request_json,phase,last_error,updated_at,busy_until) VALUES(?,?,?,?,?,NULL) ON CONFLICT(id) DO UPDATE SET phase=excluded.phase,last_error=excluded.last_error,updated_at=excluded.updated_at,busy_until=NULL`, record.Request.ID, encoded, string(record.Phase), record.LastError, stamp.Format(time.RFC3339Nano))
	return err
}

func claimCutoverPhase(ctx context.Context, db *sql.DB, id string, phase CutoverPhase, at time.Time) (bool, error) {
	if at.IsZero() {
		return false, errors.New("updater: cutover clock returned zero time")
	}
	// A crashed owner may be retried after this bounded lease. Every hook is
	// required to be idempotent; authority-step failures still enter the
	// recovery-required state rather than attempting an unsafe rollback.
	nowText := at.UTC().Format(time.RFC3339Nano)
	until := at.UTC().Add(5 * time.Minute).Format(time.RFC3339Nano)
	result, err := db.ExecContext(ctx, `UPDATE role_cutovers SET busy_until=? WHERE id=? AND phase=? AND (busy_until IS NULL OR busy_until<=?)`, until, id, string(phase), nowText)
	if err != nil {
		return false, fmt.Errorf("updater: claim role cutover phase: %w", err)
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}
