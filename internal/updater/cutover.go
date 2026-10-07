package updater

import (
	"context"
	"crypto/rand"
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

const cutoverLeaseDuration = 5 * time.Minute
const cutoverRecoveryTimeout = 30 * time.Second

// The companion table belongs to the independent cutover journal. Production
// callers must not use the monitoring database as an unversioned journal.
func ensureCutoverJournal(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS role_cutovers (id TEXT PRIMARY KEY, request_json BLOB NOT NULL, phase TEXT NOT NULL, last_error TEXT NOT NULL, updated_at TEXT NOT NULL, busy_until TEXT)`); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS role_cutover_leases (id TEXT PRIMARY KEY, owner_token TEXT NOT NULL, generation INTEGER NOT NULL, deadline_ns INTEGER NOT NULL)`)
	return err
}

// RunRoleCutover creates or resumes an independent cutover journal. Hooks must
// be idempotent. Successful pre-switch rollback restarts at preflight; failed
// rollback and uncertain authority changes require operator recovery. Leases
// fence journal completion, not external side effects: production adapters
// must also enforce their own durable authority fences. Lease owner/generation
// are currently journal-private; future adapters must receive and validate a
// durable authority generation before performing external side effects.
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
	if err := createCutover(ctx, journal, req, now()); err != nil {
		return RoleCutoverRecord{}, err
	}
	record, _, err := loadCutover(ctx, journal, req.ID)
	if err != nil {
		return record, err
	}
	if record.Request != req {
		return record, errors.New("updater: cutover idempotency conflict")
	}
	if !validCutoverPhase(record.Phase) {
		return record, errors.New("updater: invalid persisted role cutover phase")
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
		{CutoverCleanup, CutoverComplete, hooks.Cleanup, true},
	}
	for _, step := range phases {
		if record.Phase != step.phase {
			continue
		}
		lease, err := claimCutoverPhase(ctx, journal, req.ID, step.phase, now())
		if err != nil {
			return record, err
		}
		// Persist hook outcomes even when the initiating request has been canceled.
		// Recovery is bounded and retains context values, never the caller's deadline.
		persist := func(next RoleCutoverRecord) (RoleCutoverRecord, error) {
			recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), cutoverRecoveryTimeout)
			defer cancel()
			if err := completeCutoverPhase(recovery, journal, lease, next, now()); err != nil {
				stored, found, loadErr := loadCutover(recovery, journal, req.ID)
				if loadErr == nil && found {
					return stored, err
				}
				return record, errors.Join(err, loadErr)
			}
			next.UpdatedAt = lease.completedAt
			return next, nil
		}
		if step.run == nil && !(step.phase == CutoverCleanup && !req.CleanupSource) {
			next := record
			next.LastError = fmt.Sprintf("updater: role cutover hook %s is required", step.phase)
			stored, saveErr := persist(next)
			return stored, errors.Join(errors.New(next.LastError), saveErr)
		}
		var hookErr error
		if !(step.phase == CutoverCleanup && !req.CleanupSource) {
			hookErr = step.run(ctx, req)
		}
		if hookErr != nil {
			recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), cutoverRecoveryTimeout)
			// Revalidate ownership before rollback side effects. An expired owner may
			// neither undo a successor's work nor publish its own error over that work.
			renewErr := renewCutoverLease(recovery, journal, lease, now())
			if renewErr != nil {
				stored, found, loadErr := loadCutover(recovery, journal, req.ID)
				cancel()
				if loadErr == nil && found {
					return stored, errors.Join(renewErr, hookErr)
				}
				return record, errors.Join(renewErr, hookErr, loadErr)
			}
			next := record
			next.LastError = hookErr.Error()
			if step.phase == CutoverCleanup {
				// Destination was already confirmed; cleanup alone remains retryable.
			} else if step.afterSwitch || hooks.RollbackBeforeSwitch == nil {
				next.Phase = CutoverRecovery
			} else {
				// Persist intent before rollback can invalidate completed
				// prerequisites. A crash during rollback must never resume tail
				// transfer or switch authority with an unfrozen source.
				pending := next
				pending.Phase = CutoverRecovery
				pending.LastError += "; rollback pending"
				if checkpointErr := checkpointCutoverRollback(recovery, journal, lease, pending, now()); checkpointErr != nil {
					stored, found, loadErr := loadCutover(recovery, journal, req.ID)
					cancel()
					if loadErr == nil && found {
						return stored, errors.Join(hookErr, checkpointErr)
					}
					return record, errors.Join(hookErr, checkpointErr, loadErr)
				}
				record = pending
				if rollbackErr := hooks.RollbackBeforeSwitch(recovery, req); rollbackErr != nil {
					next.Phase = CutoverRecovery
					next.LastError += "; rollback: " + rollbackErr.Error()
				} else {
					next.Phase = CutoverPreflight
				}
			}
			cancel()
			// Rollback may have consumed its entire deadline. Journal the
			// result under a fresh bounded context so cancellation cannot leave
			// an invalidated pre-switch phase eligible for ordinary retry.
			next, saveErr := persist(next)
			if saveErr != nil {
				return next, errors.Join(hookErr, saveErr)
			}
			return next, fmt.Errorf("updater: role cutover %s: %w", step.phase, hookErr)
		}
		next := record
		next.Phase, next.LastError = step.next, ""
		record, err = persist(next)
		if err != nil {
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

func createCutover(ctx context.Context, db *sql.DB, req RoleCutoverRequest, at time.Time) error {
	if at.IsZero() {
		return errors.New("updater: cutover clock returned zero time")
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO role_cutovers(id,request_json,phase,last_error,updated_at,busy_until) VALUES(?,?,?,?,?,NULL) ON CONFLICT(id) DO NOTHING`, req.ID, encoded, string(CutoverPreflight), "", at.UTC().Format(time.RFC3339Nano))
	return err
}

type cutoverLease struct {
	id, owner   string
	phase       CutoverPhase
	generation  int64
	completedAt time.Time
}

var errCutoverLeaseLost = errors.New("updater: role cutover lease lost or expired")

func claimCutoverPhase(ctx context.Context, db *sql.DB, id string, phase CutoverPhase, at time.Time) (*cutoverLease, error) {
	if at.IsZero() {
		return nil, errors.New("updater: cutover clock returned zero time")
	}
	lease := &cutoverLease{id: id, phase: phase, owner: rand.Text()}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	until := at.Add(cutoverLeaseDuration)
	result, err := tx.ExecContext(ctx, `UPDATE role_cutovers SET busy_until=? WHERE id=? AND phase=? AND (
  EXISTS(SELECT 1 FROM role_cutover_leases WHERE id=? AND deadline_ns<=?) OR
  (NOT EXISTS(SELECT 1 FROM role_cutover_leases WHERE id=?) AND (busy_until IS NULL OR julianday(busy_until)<=julianday(?))))`, until.UTC().Format(time.RFC3339Nano), id, string(phase), id, at.UnixNano(), id, at.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("updater: claim role cutover phase: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed != 1 {
		return nil, errors.New("updater: role cutover phase is already running or changed")
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO role_cutover_leases(id,owner_token,generation,deadline_ns) VALUES(?,?,1,?)
  ON CONFLICT(id) DO UPDATE SET owner_token=excluded.owner_token,generation=generation+1,deadline_ns=excluded.deadline_ns
  WHERE generation<9223372036854775807 RETURNING generation`, id, lease.owner, until.UnixNano()).Scan(&lease.generation)
	if err != nil {
		return nil, fmt.Errorf("updater: claim role cutover lease generation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return lease, nil
}

func renewCutoverLease(ctx context.Context, db *sql.DB, lease *cutoverLease, at time.Time) error {
	if at.IsZero() {
		return errors.New("updater: cutover clock returned zero time")
	}
	result, err := db.ExecContext(ctx, `UPDATE role_cutover_leases SET deadline_ns=? WHERE id=? AND owner_token=? AND generation=? AND deadline_ns>? AND EXISTS(SELECT 1 FROM role_cutovers WHERE id=? AND phase=?)`, at.Add(cutoverLeaseDuration).UnixNano(), lease.id, lease.owner, lease.generation, at.UnixNano(), lease.id, string(lease.phase))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errCutoverLeaseLost
	}
	return nil
}

func completeCutoverPhase(ctx context.Context, db *sql.DB, lease *cutoverLease, record RoleCutoverRecord, at time.Time) error {
	if at.IsZero() {
		return errors.New("updater: cutover clock returned zero time")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE role_cutovers SET phase=?,last_error=?,updated_at=?,busy_until=NULL WHERE id=? AND phase=? AND EXISTS(SELECT 1 FROM role_cutover_leases WHERE id=? AND owner_token=? AND generation=? AND deadline_ns>?)`, string(record.Phase), record.LastError, at.UTC().Format(time.RFC3339Nano), lease.id, string(lease.phase), lease.id, lease.owner, lease.generation, at.UnixNano())
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errCutoverLeaseLost
	}
	if _, err := tx.ExecContext(ctx, `UPDATE role_cutover_leases SET deadline_ns=0 WHERE id=? AND owner_token=? AND generation=?`, lease.id, lease.owner, lease.generation); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	lease.completedAt = at.UTC()
	return nil
}

// checkpointCutoverRollback retains the current lease while durably making
// interrupted rollback fail closed. Completion then uses this new expected phase.
func checkpointCutoverRollback(ctx context.Context, db *sql.DB, lease *cutoverLease, record RoleCutoverRecord, at time.Time) error {
	if at.IsZero() {
		return errors.New("updater: cutover clock returned zero time")
	}
	result, err := db.ExecContext(ctx, `UPDATE role_cutovers SET phase=?,last_error=?,updated_at=? WHERE id=? AND phase=? AND EXISTS(SELECT 1 FROM role_cutover_leases WHERE id=? AND owner_token=? AND generation=? AND deadline_ns>?)`, string(CutoverRecovery), record.LastError, at.UTC().Format(time.RFC3339Nano), lease.id, string(lease.phase), lease.id, lease.owner, lease.generation, at.UnixNano())
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errCutoverLeaseLost
	}
	lease.phase = CutoverRecovery
	return nil
}
