package monitoring

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

var (
	ErrJobNotFound            = errors.New("job_not_found")
	ErrJobRevisionConflict    = errors.New("job_revision_conflict")
	ErrJobNotCancellable      = errors.New("job_not_cancellable")
	ErrJobIdempotencyConflict = errors.New("job_idempotency_conflict")
	ErrEnrollmentJobInvalid   = errors.New("enrollment_job_invalid")
	ErrEnrollmentJobExpired   = errors.New("enrollment_job_expired")
	ErrJobLeaseLost           = errors.New("job_lease_lost")
	ErrActionJobNotFound      = errors.New("action_job_not_found")
)

const MaxDurableJobs = 10000

// CreateJob durably creates a bounded job. A retry with the same operation
// identity returns the original row; reuse for different input fails closed.
func (s *Store) CreateJob(ctx context.Context, job contracts.Job, requestHash string, now time.Time) (contracts.Job, bool, error) {
	if err := validateJob(job, requestHash, now); err != nil {
		return contracts.Job{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return contracts.Job{}, false, err
	}
	defer tx.Rollback()

	existing, found, err := getJobByOperationTx(ctx, tx, job.Kind, job.TargetServerID, job.IdempotencyKey)
	if err != nil {
		return contracts.Job{}, false, err
	}
	if found {
		var storedHash string
		if err := tx.QueryRowContext(ctx, `SELECT request_hash FROM jobs WHERE id=?`, existing.ID).Scan(&storedHash); err != nil {
			return contracts.Job{}, false, err
		}
		if storedHash != requestHash {
			return contracts.Job{}, false, ErrJobIdempotencyConflict
		}
		return existing, false, nil
	}
	if job.TargetServerID != "" {
		if err := requireServerCommandAuthorityTx(ctx, tx, job.TargetServerID); err != nil {
			return contracts.Job{}, false, err
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&count); err != nil {
		return contracts.Job{}, false, err
	}
	if count >= MaxDurableJobs {
		return contracts.Job{}, false, ErrStoragePressure
	}
	errorJSON, err := marshalJobError(job.Error)
	if err != nil {
		return contracts.Job{}, false, err
	}
	actionJSON, err := marshalAction(job.Action)
	if err != nil {
		return contracts.Job{}, false, err
	}
	resultJSON, err := marshalActionResult(job.Result)
	if err != nil {
		return contracts.Job{}, false, err
	}
	stamp := FormatPersistedTime(now)
	_, err = tx.ExecContext(ctx, `INSERT INTO jobs(id,kind,state,revision,idempotency_key,request_hash,target_server_id,expires_at,cancel_requested,progress,error_json,action_json,result_json,lease_token,lease_expires_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		job.ID, job.Kind, string(job.State), strconv.FormatUint(job.Revision, 10), job.IdempotencyKey, requestHash, nullableServerID(job.TargetServerID), FormatPersistedTime(job.ExpiresAt), jobBoolInt(job.CancelRequested), job.Progress, errorJSON, actionJSON, resultJSON, nil, nil, stamp, stamp)
	if err != nil {
		return contracts.Job{}, false, fmt.Errorf("insert job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return contracts.Job{}, false, err
	}
	return job, true, nil
}

func (s *Store) GetJob(ctx context.Context, id string) (contracts.Job, bool, error) {
	if !validJobText(id) {
		return contracts.Job{}, false, nil
	}
	return scanJob(s.db.QueryRowContext(ctx, jobSelect+` WHERE id=?`, id))
}

// ListJobsByKind returns the bounded durable history for one job kind. It is
// intentionally a read-only primitive: workers must still claim work with a
// revision compare-and-swap before doing anything external.
func (s *Store) ListJobsByKind(ctx context.Context, kind string) ([]contracts.Job, error) {
	if !validJobText(kind) {
		return nil, errors.New("invalid job kind")
	}
	rows, err := s.db.QueryContext(ctx, jobSelect+` WHERE kind=? ORDER BY created_at,id LIMIT ?`, kind, MaxDurableJobs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := make([]contracts.Job, 0)
	for rows.Next() {
		job, found, scanErr := scanJob(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if found {
			jobs = append(jobs, job)
		}
	}
	return jobs, rows.Err()
}

// ListActiveJobsForServer identifies durable work that must be serialized
// against an update. The update worker uses this instead of an in-memory lock,
// so a second process and a restarted process observe the same exclusion.
func (s *Store) ListActiveJobsForServer(ctx context.Context, serverID contracts.ServerID, excludeID string) ([]contracts.Job, error) {
	if !validStoreServerID(serverID) || (excludeID != "" && !validJobText(excludeID)) {
		return nil, errors.New("invalid active job query")
	}
	query := jobSelect + ` WHERE target_server_id=? AND state IN (?,?,?,?)`
	args := []any{string(serverID), string(contracts.JobQueued), string(contracts.JobRunning), string(contracts.JobCancelling), string(contracts.JobRecoveryRequired)}
	if excludeID != "" {
		query += ` AND id<>?`
		args = append(args, excludeID)
	}
	query += ` ORDER BY created_at,id LIMIT 256`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := make([]contracts.Job, 0)
	for rows.Next() {
		job, found, scanErr := scanJob(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if found {
			jobs = append(jobs, job)
		}
	}
	return jobs, rows.Err()
}

// GetJobByOperation returns the durable job and its request hash for an
// idempotent producer preflight. The hash is never part of the API response;
// callers use it only to distinguish replay from key reuse.
func (s *Store) GetJobByOperation(ctx context.Context, kind string, serverID contracts.ServerID, idempotencyKey string) (contracts.Job, string, bool, error) {
	if !validJobText(kind) || !validJobText(idempotencyKey) || (!validStoreServerID(serverID) && serverID != "") {
		return contracts.Job{}, "", false, errors.New("invalid job operation identity")
	}
	var requestHash string
	job, found, err := scanJob(s.db.QueryRowContext(ctx, jobSelect+` WHERE kind=? AND target_server_id=? AND idempotency_key=?`, kind, string(serverID), idempotencyKey))
	if err != nil || !found {
		return job, "", found, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT request_hash FROM jobs WHERE id=?`, job.ID).Scan(&requestHash); err != nil {
		return contracts.Job{}, "", false, err
	}
	return job, requestHash, true, nil
}

// ClaimEnrollmentJob atomically moves one queued enrollment job to running.
// The pairing token is intentionally absent: the node presents it out of
// band to the transport consumer, while this durable boundary only stores
// bounded state and CAS revisions.
func (s *Store) ClaimEnrollmentJob(ctx context.Context, jobID string, serverID contracts.ServerID, expectedRevision uint64, now time.Time) (contracts.Job, error) {
	if !validJobText(jobID) || !validStoreServerID(serverID) || now.IsZero() || expectedRevision == ^uint64(0) {
		return contracts.Job{}, ErrEnrollmentJobInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return contracts.Job{}, err
	}
	defer tx.Rollback()
	job, found, err := scanJob(tx.QueryRowContext(ctx, jobSelect+` WHERE id=?`, jobID))
	if err != nil {
		return contracts.Job{}, err
	}
	if !found {
		return contracts.Job{}, ErrJobNotFound
	}
	if job.Kind != "enrollment" || job.TargetServerID != serverID || job.Revision != expectedRevision || job.State != contracts.JobQueued {
		return contracts.Job{}, ErrEnrollmentJobInvalid
	}
	if err := requireServerCommandAuthorityTx(ctx, tx, serverID); err != nil {
		return contracts.Job{}, err
	}
	if !job.ExpiresAt.After(now) {
		return contracts.Job{}, ErrEnrollmentJobExpired
	}
	nextRevision := expectedRevision + 1
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state=?,revision=?,progress=?,updated_at=? WHERE id=? AND revision=? AND kind=? AND target_server_id=?`, string(contracts.JobRunning), strconv.FormatUint(nextRevision, 10), 5, FormatPersistedTime(now), jobID, strconv.FormatUint(expectedRevision, 10), "enrollment", string(serverID))
	if err != nil {
		return contracts.Job{}, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return contracts.Job{}, ErrJobRevisionConflict
	}
	if err := tx.Commit(); err != nil {
		return contracts.Job{}, err
	}
	job.State, job.Revision, job.Progress = contracts.JobRunning, nextRevision, 5
	job.CancelRequested = false
	return job, nil
}

// TransitionJob performs the worker-side compare-and-swap state update.
func (s *Store) TransitionJob(ctx context.Context, id string, expectedRevision uint64, state contracts.JobState, progress uint8, jobError *contracts.Error, now time.Time) (contracts.Job, error) {
	if !validJobText(id) || !validJobState(state) || progress > 100 || now.IsZero() {
		return contracts.Job{}, errors.New("invalid job transition")
	}
	errorJSON, err := marshalJobError(jobError)
	if err != nil {
		return contracts.Job{}, err
	}
	next := expectedRevision + 1
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET state=?,revision=?,progress=?,error_json=?,updated_at=? WHERE id=? AND revision=?`, string(state), strconv.FormatUint(next, 10), progress, errorJSON, FormatPersistedTime(now), id, strconv.FormatUint(expectedRevision, 10))
	if err != nil {
		return contracts.Job{}, err
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		if _, found, getErr := s.GetJob(ctx, id); getErr != nil {
			return contracts.Job{}, getErr
		} else if !found {
			return contracts.Job{}, ErrJobNotFound
		}
		return contracts.Job{}, ErrJobRevisionConflict
	}
	job, _, err := s.GetJob(ctx, id)
	return job, err
}

// RequestJobCancellation records an idempotent cancellation result and uses
// the job revision as a compare-and-swap guard.
func (s *Store) RequestJobCancellation(ctx context.Context, id, idempotencyKey string, expectedRevision uint64, now time.Time) (contracts.Job, error) {
	if !validJobText(id) || !validJobText(idempotencyKey) || now.IsZero() {
		return contracts.Job{}, errors.New("invalid cancellation request")
	}
	hashBytes := sha256.Sum256([]byte(strconv.FormatUint(expectedRevision, 10)))
	requestHash := hex.EncodeToString(hashBytes[:])
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return contracts.Job{}, err
	}
	defer tx.Rollback()
	var storedHash, resultJSON string
	err = tx.QueryRowContext(ctx, `SELECT request_hash,result_json FROM job_cancellation_requests WHERE job_id=? AND idempotency_key=?`, id, idempotencyKey).Scan(&storedHash, &resultJSON)
	if err == nil {
		if storedHash != requestHash {
			return contracts.Job{}, ErrJobIdempotencyConflict
		}
		var job contracts.Job
		if err := json.Unmarshal([]byte(resultJSON), &job); err != nil {
			return contracts.Job{}, fmt.Errorf("decode cancellation result: %w", err)
		}
		return job, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return contracts.Job{}, err
	}
	job, found, err := scanJob(tx.QueryRowContext(ctx, jobSelect+` WHERE id=?`, id))
	if err != nil {
		return contracts.Job{}, err
	}
	if !found {
		return contracts.Job{}, ErrJobNotFound
	}
	if job.Revision != expectedRevision {
		return contracts.Job{}, ErrJobRevisionConflict
	}
	if terminalJobState(job.State) {
		return contracts.Job{}, ErrJobNotCancellable
	}
	job.Revision++
	job.CancelRequested = true
	if job.State == contracts.JobQueued {
		job.State = contracts.JobCancelled
		job.Progress = 100
	} else {
		job.State = contracts.JobCancelling
	}
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state=?,revision=?,cancel_requested=1,progress=?,updated_at=? WHERE id=? AND revision=?`, string(job.State), strconv.FormatUint(job.Revision, 10), job.Progress, FormatPersistedTime(now), id, strconv.FormatUint(expectedRevision, 10))
	if err != nil {
		return contracts.Job{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return contracts.Job{}, ErrJobRevisionConflict
	}
	// Atomically block queued or leased derivative activation before the
	// cancellation is visible. The transport dispatcher cannot race a later
	// scheduler tick and send new work for this cancelled rollout.
	if job.Kind == "update" || job.Kind == "update-node" {
		_, err = tx.ExecContext(ctx, `UPDATE jobs SET cancel_requested=1,state=CASE WHEN state=? THEN ? ELSE ? END,revision=CAST(CAST(revision AS INTEGER)+1 AS TEXT),updated_at=? WHERE kind='core-update-action' AND state IN (?,?) AND json_extract(action_json,'$.arguments.job_id') IN (SELECT id FROM jobs WHERE id=? OR (kind='update-node' AND json_extract(action_json,'$.arguments.parent_job_id')=?))`, string(contracts.JobQueued), string(contracts.JobCancelled), string(contracts.JobCancelling), FormatPersistedTime(now), string(contracts.JobQueued), string(contracts.JobRunning), job.ID, job.ID)
		if err != nil {
			return contracts.Job{}, err
		}
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		return contracts.Job{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_cancellation_requests(job_id,idempotency_key,request_hash,result_json,created_at) VALUES(?,?,?,?,?)`, id, idempotencyKey, requestHash, string(encoded), FormatPersistedTime(now)); err != nil {
		return contracts.Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return contracts.Job{}, err
	}
	return job, nil
}

const jobSelect = `SELECT id,kind,state,revision,idempotency_key,target_server_id,expires_at,cancel_requested,progress,error_json,action_json,result_json FROM jobs`

type rowScanner interface{ Scan(...any) error }

func scanJob(row rowScanner) (contracts.Job, bool, error) {
	var job contracts.Job
	var state, revision, expiresAt string
	var target sql.NullString
	var cancel int
	var errorJSON, actionJSON, resultJSON sql.NullString
	err := row.Scan(&job.ID, &job.Kind, &state, &revision, &job.IdempotencyKey, &target, &expiresAt, &cancel, &job.Progress, &errorJSON, &actionJSON, &resultJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.Job{}, false, nil
	}
	if err != nil {
		return contracts.Job{}, false, err
	}
	job.State = contracts.JobState(state)
	job.TargetServerID = contracts.ServerID(target.String)
	job.CancelRequested = cancel != 0
	job.Revision, err = strconv.ParseUint(revision, 10, 64)
	if err == nil {
		job.ExpiresAt, err = time.Parse(persistedTimeLayout, expiresAt)
	}
	if err == nil && errorJSON.Valid {
		job.Error = &contracts.Error{}
		err = json.Unmarshal([]byte(errorJSON.String), job.Error)
	}
	if err == nil && actionJSON.Valid && actionJSON.String != "" {
		job.Action = &contracts.ActionRequest{}
		err = json.Unmarshal([]byte(actionJSON.String), job.Action)
		if err == nil {
			err = job.Action.Validate(time.Unix(0, 0).UTC())
		}
	}
	if err == nil && resultJSON.Valid && resultJSON.String != "" {
		job.Result = &contracts.ActionResponse{}
		err = json.Unmarshal([]byte(resultJSON.String), job.Result)
	}
	if err != nil || !validJobState(job.State) || job.Progress > 100 {
		return contracts.Job{}, false, errors.New("invalid persisted job")
	}
	return job, true, nil
}

func getJobByOperationTx(ctx context.Context, tx *sql.Tx, kind string, serverID contracts.ServerID, key string) (contracts.Job, bool, error) {
	return scanJob(tx.QueryRowContext(ctx, jobSelect+` WHERE kind=? AND target_server_id=? AND idempotency_key=?`, kind, string(serverID), key))
}

func validateJob(job contracts.Job, requestHash string, now time.Time) error {
	if !validJobText(job.ID) || !validJobText(job.Kind) || !validJobText(job.IdempotencyKey) || requestHash == "" || len(requestHash) > 128 || !validJobState(job.State) || job.Revision == ^uint64(0) || job.Progress > 100 || now.IsZero() || !job.ExpiresAt.After(now) || (!validStoreServerID(job.TargetServerID) && job.TargetServerID != "") {
		return errors.New("invalid job")
	}
	return nil
}

func validJobText(value string) bool { return value != "" && len(value) <= 128 }
func validJobState(state contracts.JobState) bool {
	switch state {
	case contracts.JobQueued, contracts.JobRunning, contracts.JobCancelling, contracts.JobSucceeded, contracts.JobFailed, contracts.JobCancelled, contracts.JobRecoveryRequired:
		return true
	default:
		return false
	}
}
func terminalJobState(state contracts.JobState) bool {
	return state == contracts.JobSucceeded || state == contracts.JobFailed || state == contracts.JobCancelled || state == contracts.JobRecoveryRequired
}
func nullableServerID(id contracts.ServerID) any {
	return string(id)
}
func jobBoolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
func marshalJobError(jobError *contracts.Error) (any, error) {
	if jobError == nil {
		return nil, nil
	}
	data, err := json.Marshal(jobError)
	if err != nil {
		return nil, err
	}
	if len(data) > contracts.MaxEnvelopeBytes {
		return nil, errors.New("job error exceeds bound")
	}
	return string(data), nil
}

func marshalAction(action *contracts.ActionRequest) (any, error) {
	if action == nil {
		return nil, nil
	}
	if err := action.Validate(time.Unix(0, 0).UTC()); err != nil {
		return nil, err
	}
	data, err := json.Marshal(action)
	if err != nil {
		return nil, err
	}
	if len(data) > contracts.MaxEnvelopeBytes {
		return nil, errors.New("job action exceeds bound")
	}
	return string(data), nil
}

func marshalActionResult(result *contracts.ActionResponse) (any, error) {
	if result == nil {
		return nil, nil
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if len(data) > contracts.MaxEnvelopeBytes {
		return nil, errors.New("job result exceeds bound")
	}
	return string(data), nil
}

// JobLease is the short-lived delivery ownership held by one authenticated
// node connection. The token is never exposed through the browser job API.
type JobLease struct {
	Job       contracts.Job
	Action    contracts.ActionRequest
	Token     string
	ExpiresAt time.Time
}

// LeaseNextActionJob atomically claims the oldest queued action for a node.
// An expired running lease is reclaimable after disconnect; the action's
// idempotency key lets a node safely return the same result on redelivery.
func (s *Store) LeaseNextActionJob(ctx context.Context, serverID contracts.ServerID, now time.Time, leaseTTL time.Duration) (JobLease, bool, error) {
	if !validStoreServerID(serverID) || now.IsZero() || leaseTTL <= 0 || leaseTTL > 24*time.Hour {
		return JobLease{}, false, errors.New("invalid action lease request")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return JobLease{}, false, err
	}
	defer tx.Rollback()
	if err := requireServerCommandAuthorityTx(ctx, tx, serverID); err != nil {
		return JobLease{}, false, err
	}
	// Recheck rollout authorization while claiming delivery. A producer may
	// persist a derivative after a concurrent cancellation transaction has
	// already cascaded to existing derivatives; such new intent is never leased.
	// The guard also applies to expired-lease redelivery and fails closed for
	// orphaned, mismatched or revoked core actions. Ordinary module actions
	// do not acquire arbitrary dependencies on other jobs.
	row := tx.QueryRowContext(ctx, jobSelect+` WHERE target_server_id=? AND kind<> 'update-node' AND action_json IS NOT NULL AND expires_at>? AND cancel_requested=0 AND (state=? OR (state=? AND lease_expires_at IS NOT NULL AND lease_expires_at<=?))
	AND (
	 (kind<>'core-update-action' AND json_extract(action_json,'$.action')<>'core.update')
	 OR (kind='core-update-action' AND json_extract(action_json,'$.action')='core.update' AND EXISTS (
	  SELECT 1 FROM jobs child JOIN jobs parent ON parent.id=json_extract(child.action_json,'$.arguments.parent_job_id')
	  JOIN servers node ON node.id=child.target_server_id
	  WHERE child.id=json_extract(jobs.action_json,'$.arguments.job_id')
	   AND child.kind='update-node' AND parent.kind='update'
	   AND child.target_server_id=jobs.target_server_id
	   AND json_extract(child.action_json,'$.arguments.release')=json_extract(jobs.action_json,'$.arguments.release')
	   AND child.state='running' AND parent.state IN ('queued','running')
	   AND child.cancel_requested=0 AND parent.cancel_requested=0
	   AND child.expires_at>? AND parent.expires_at>?
	   AND node.connection_state<>'revoked'
	 ))
	) ORDER BY created_at,id LIMIT 1`, string(serverID), FormatPersistedTime(now), string(contracts.JobQueued), string(contracts.JobRunning), FormatPersistedTime(now), FormatPersistedTime(now), FormatPersistedTime(now))
	job, found, err := scanJob(row)
	if err != nil || !found {
		return JobLease{}, found, err
	}
	if job.Action == nil {
		return JobLease{}, false, errors.New("persisted action job has no action")
	}
	token, err := newLeaseToken()
	if err != nil {
		return JobLease{}, false, err
	}
	leaseExpires := now.Add(leaseTTL).UTC()
	nextRevision := job.Revision + 1
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state=?,revision=?,lease_token=?,lease_expires_at=?,updated_at=? WHERE id=? AND revision=? AND (state=? OR (state=? AND lease_expires_at IS NOT NULL AND lease_expires_at<=?))`, string(contracts.JobRunning), strconv.FormatUint(nextRevision, 10), token, FormatPersistedTime(leaseExpires), FormatPersistedTime(now), job.ID, strconv.FormatUint(job.Revision, 10), string(contracts.JobQueued), string(contracts.JobRunning), FormatPersistedTime(now))
	if err != nil {
		return JobLease{}, false, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return JobLease{}, false, ErrJobRevisionConflict
	}
	if err := tx.Commit(); err != nil {
		return JobLease{}, false, err
	}
	job.State, job.Revision = contracts.JobRunning, nextRevision
	return JobLease{Job: job, Action: *job.Action, Token: token, ExpiresAt: leaseExpires}, true, nil
}

// CompleteActionJob commits the node result only while the delivery lease is
// still owned by the caller. Replaying a completed result returns the durable
// original job, making lost response ACKs safe across reconnects.
func (s *Store) CompleteActionJob(ctx context.Context, jobID, leaseToken string, response contracts.ActionResponse, now time.Time) (contracts.Job, error) {
	if !validJobText(jobID) || leaseToken == "" || now.IsZero() || response.RequestID == "" {
		return contracts.Job{}, errors.New("invalid action completion")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return contracts.Job{}, err
	}
	defer tx.Rollback()
	job, found, err := scanJob(tx.QueryRowContext(ctx, jobSelect+` WHERE id=?`, jobID))
	if err != nil {
		return contracts.Job{}, err
	}
	if !found {
		return contracts.Job{}, ErrJobNotFound
	}
	if job.Result != nil {
		return job, nil
	}
	if job.TargetServerID != "" {
		if err := requireServerCommandAuthorityTx(ctx, tx, job.TargetServerID); err != nil {
			return contracts.Job{}, err
		}
	}
	if job.Action == nil || job.Action.RequestID != response.RequestID {
		return contracts.Job{}, errors.New("action response request mismatch")
	}
	var storedLease string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(lease_token,'') FROM jobs WHERE id=?`, jobID).Scan(&storedLease); err != nil {
		return contracts.Job{}, err
	}
	if storedLease != leaseToken {
		return contracts.Job{}, ErrJobLeaseLost
	}
	resultJSON, err := marshalActionResult(&response)
	if err != nil {
		return contracts.Job{}, err
	}
	nextState := contracts.JobFailed
	progress := uint8(100)
	if response.Accepted {
		nextState = contracts.JobSucceeded
	}
	nextRevision := job.Revision + 1
	var errorJSON any
	if response.Error != nil {
		errorJSON, err = marshalJobError(response.Error)
		if err != nil {
			return contracts.Job{}, err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state=?,revision=?,progress=?,error_json=?,result_json=?,lease_token=NULL,lease_expires_at=NULL,updated_at=? WHERE id=? AND revision=? AND lease_token=?`, string(nextState), strconv.FormatUint(nextRevision, 10), progress, errorJSON, resultJSON, FormatPersistedTime(now), jobID, strconv.FormatUint(job.Revision, 10), leaseToken)
	if err != nil {
		return contracts.Job{}, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return contracts.Job{}, ErrJobLeaseLost
	}
	if err := tx.Commit(); err != nil {
		return contracts.Job{}, err
	}
	job.State, job.Revision, job.Progress, job.Error, job.Result = nextState, nextRevision, progress, response.Error, &response
	return job, nil
}

// CompleteActionJobForServer resolves a response by request id and the
// authenticated server target. The lease token remains internal to the hub;
// an authenticated node cannot forge another node's response.
func (s *Store) CompleteActionJobForServer(ctx context.Context, serverID contracts.ServerID, response contracts.ActionResponse, now time.Time) (contracts.Job, error) {
	if !validStoreServerID(serverID) || response.RequestID == "" {
		return contracts.Job{}, errors.New("invalid action response")
	}
	rows, err := s.db.QueryContext(ctx, jobSelect+` WHERE target_server_id=? AND action_json IS NOT NULL AND result_json IS NULL AND state IN (?,?) ORDER BY created_at,id LIMIT 256`, string(serverID), string(contracts.JobRunning), string(contracts.JobCancelling))
	if err != nil {
		return contracts.Job{}, err
	}
	var matching contracts.Job
	for rows.Next() {
		job, _, scanErr := scanJob(rows)
		if scanErr != nil {
			_ = rows.Close()
			return contracts.Job{}, scanErr
		}
		if job.Action != nil && job.Action.RequestID == response.RequestID {
			matching = job
			break
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return contracts.Job{}, err
	}
	_ = rows.Close()
	if matching.ID != "" {
		var token string
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(lease_token,'') FROM jobs WHERE id=?`, matching.ID).Scan(&token); err != nil {
			return contracts.Job{}, err
		}
		if token == "" {
			return contracts.Job{}, ErrJobLeaseLost
		}
		return s.CompleteActionJob(ctx, matching.ID, token, response, now)
	}
	return contracts.Job{}, ErrActionJobNotFound
}

func newLeaseToken() (string, error) {
	var raw [24]byte
	if _, err := cryptorand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
