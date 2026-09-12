package monitoring

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestDurableJobIdempotencyAndCancellationSurviveRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jobs.db")
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	store, err := OpenStore(ctx, path, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	job := contracts.Job{ID: "job_0123456789abcdef", Kind: "enrollment", State: contracts.JobQueued, Revision: 0, IdempotencyKey: "request-1", TargetServerID: "server-0123456789abcdef", ExpiresAt: now.Add(10 * time.Minute)}
	created, wasCreated, err := store.CreateJob(ctx, job, "request-hash-1", now)
	if err != nil || !wasCreated || created.ID != job.ID {
		t.Fatalf("create job: created=%v job=%+v err=%v", wasCreated, created, err)
	}
	replayed, wasCreated, err := store.CreateJob(ctx, contracts.Job{ID: "different-job-id", Kind: job.Kind, State: contracts.JobQueued, IdempotencyKey: job.IdempotencyKey, TargetServerID: job.TargetServerID, ExpiresAt: job.ExpiresAt}, "request-hash-1", now)
	if err != nil || wasCreated || replayed.ID != job.ID {
		t.Fatalf("idempotent create: created=%v job=%+v err=%v", wasCreated, replayed, err)
	}
	if _, _, err := store.CreateJob(ctx, job, "different-hash", now); !errors.Is(err, ErrJobIdempotencyConflict) {
		t.Fatalf("conflicting idempotency key: %v", err)
	}
	cancelled, err := store.RequestJobCancellation(ctx, job.ID, "cancel-1", 0, now.Add(time.Second))
	if err != nil || cancelled.State != contracts.JobCancelled || !cancelled.CancelRequested || cancelled.Revision != 1 || cancelled.Progress != 100 {
		t.Fatalf("cancel queued job: job=%+v err=%v", cancelled, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenStore(ctx, path, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loaded, found, err := store.GetJob(ctx, job.ID)
	if err != nil || !found || loaded.State != contracts.JobCancelled || loaded.Revision != 1 {
		t.Fatalf("load after restart: found=%v job=%+v err=%v", found, loaded, err)
	}
	replayedCancellation, err := store.RequestJobCancellation(ctx, job.ID, "cancel-1", 0, now.Add(2*time.Second))
	if err != nil || replayedCancellation.Revision != 1 || replayedCancellation.State != contracts.JobCancelled {
		t.Fatalf("replay cancellation: job=%+v err=%v", replayedCancellation, err)
	}
}

func TestJobCancellationRejectsStaleRevisionAndTerminalJob(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	job := contracts.Job{ID: "job_abcdef0123456789", Kind: "update", State: contracts.JobRunning, Revision: 4, IdempotencyKey: "update-1", ExpiresAt: now.Add(time.Hour), Progress: 50}
	if _, _, err := store.CreateJob(ctx, job, "hash", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestJobCancellation(ctx, job.ID, "cancel-stale", 3, now); !errors.Is(err, ErrJobRevisionConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	succeeded, err := store.TransitionJob(ctx, job.ID, 4, contracts.JobSucceeded, 100, nil, now.Add(time.Second))
	if err != nil || succeeded.State != contracts.JobSucceeded {
		t.Fatalf("complete job: job=%+v err=%v", succeeded, err)
	}
	if _, err := store.RequestJobCancellation(ctx, job.ID, "cancel-complete", 5, now.Add(2*time.Second)); !errors.Is(err, ErrJobNotCancellable) {
		t.Fatalf("terminal cancellation: %v", err)
	}
}

func TestClaimEnrollmentJobUsesCASAndDoesNotStoreToken(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	server := testServer()
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	job, created, err := store.CreateJob(ctx, contracts.Job{ID: "enrollment-claim-012345", Kind: "enrollment", State: contracts.JobQueued, IdempotencyKey: "enroll-key-1", TargetServerID: server.ID, ExpiresAt: now.Add(time.Minute)}, "hash-only", now)
	if err != nil || !created {
		t.Fatalf("create enrollment job: %+v created=%v err=%v", job, created, err)
	}
	claimed, err := store.ClaimEnrollmentJob(ctx, job.ID, server.ID, job.Revision, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if claimed.State != contracts.JobRunning || claimed.Revision != 1 || claimed.Progress != 5 {
		t.Fatalf("unexpected claim: %+v", claimed)
	}
	if _, err := store.ClaimEnrollmentJob(ctx, job.ID, server.ID, job.Revision, now.Add(2*time.Second)); err != ErrEnrollmentJobInvalid {
		t.Fatalf("expected second claim to be rejected, got %v", err)
	}
	var stored string
	if err := store.db.QueryRowContext(ctx, `SELECT request_hash FROM jobs WHERE id=?`, job.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "hash-only" {
		t.Fatalf("unexpected stored request data: %q", stored)
	}
}

func TestActionJobLeaseReclaimAndIdempotentCompletion(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 12, 13, 0, 0, 0, time.UTC)
	server := testServer()
	action := contracts.ActionRequest{Protocol: contracts.HelperProtocol, RequestID: "action-request-012345", Action: "service.restart", TargetServerID: server.ID, Target: "payesh-agent", IdempotencyKey: "action-idem-012345", Deadline: now.Add(time.Hour), Arguments: []byte(`{"unit":"payesh-agent"}`)}
	job := contracts.Job{ID: "action-job-0123456789", Kind: "update", State: contracts.JobQueued, IdempotencyKey: action.IdempotencyKey, TargetServerID: server.ID, ExpiresAt: now.Add(time.Hour), Action: &action}
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	created, _, err := store.CreateJob(ctx, job, "action-hash", now)
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := store.LeaseNextActionJob(ctx, server.ID, now, time.Minute)
	if err != nil || !ok || lease.Action.RequestID != action.RequestID || lease.Job.State != contracts.JobRunning {
		t.Fatalf("lease=%+v ok=%v err=%v", lease, ok, err)
	}
	if _, ok, err := store.LeaseNextActionJob(ctx, server.ID, now.Add(30*time.Second), time.Minute); err != nil || ok {
		t.Fatalf("leased job was duplicated before expiry: ok=%v err=%v", ok, err)
	}
	reclaimed, ok, err := store.LeaseNextActionJob(ctx, server.ID, now.Add(2*time.Minute), time.Minute)
	if err != nil || !ok || reclaimed.Token == lease.Token {
		t.Fatalf("expired lease was not reclaimed: lease=%+v ok=%v err=%v", reclaimed, ok, err)
	}
	response := contracts.ActionResponse{RequestID: action.RequestID, Accepted: true, Revision: 2}
	completed, err := store.CompleteActionJob(ctx, created.ID, reclaimed.Token, response, now.Add(3*time.Minute))
	if err != nil || completed.State != contracts.JobSucceeded || completed.Result == nil {
		t.Fatalf("complete action: job=%+v err=%v", completed, err)
	}
	replay, err := store.CompleteActionJob(ctx, created.ID, "stale-token", response, now.Add(4*time.Minute))
	if err != nil || replay.State != contracts.JobSucceeded || replay.Result == nil {
		t.Fatalf("idempotent result replay: job=%+v err=%v", replay, err)
	}
}
