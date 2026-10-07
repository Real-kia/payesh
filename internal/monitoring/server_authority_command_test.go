package monitoring

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func commandFenceFixture(t *testing.T) (*Store, contracts.Server, time.Time) {
	t.Helper()
	store, server := openAuthorityStore(t)
	return store, server, time.Date(2026, 9, 12, 13, 0, 0, 0, time.UTC)
}

func commandFenceJob(server contracts.Server, suffix string, now time.Time) contracts.Job {
	action := contracts.ActionRequest{Protocol: contracts.HelperProtocol, RequestID: "action-request-" + suffix, Action: "service.restart", TargetServerID: server.ID, Target: "payesh-agent", IdempotencyKey: "action-idem-" + suffix, Deadline: now.Add(time.Hour), Arguments: []byte(`{"unit":"payesh-agent"}`)}
	return contracts.Job{ID: "action-job-" + suffix, Kind: "update", State: contracts.JobQueued, IdempotencyKey: action.IdempotencyKey, TargetServerID: server.ID, ExpiresAt: now.Add(time.Hour), Action: &action}
}

func freezeForCommandTest(t *testing.T, store *Store, server contracts.Server) {
	t.Helper()
	if _, err := store.InitializeServerAuthority(context.Background(), server.ID, "owner-hub-aaaa"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FreezeServerAuthority(context.Background(), AuthorityTransition{ServerID: server.ID, CutoverID: "cutover-0001", RequestDigest: authorityDigest("a"), FrontierDigest: authorityDigest("b")}); err != nil {
		t.Fatal(err)
	}
}

func TestActiveAuthorityStillAllowsCommands(t *testing.T) {
	ctx := context.Background()
	store, server, now := commandFenceFixture(t)
	if _, err := store.InitializeServerAuthority(ctx, server.ID, "owner-hub-aaaa"); err != nil {
		t.Fatal(err)
	}
	job := commandFenceJob(server, "0123456789", now)
	if _, _, err := store.CreateJob(ctx, job, "hash-active", now); err != nil {
		t.Fatalf("active owner could not create a job: %v", err)
	}
	if _, ok, err := store.LeaseNextActionJob(ctx, server.ID, now, time.Minute); err != nil || !ok {
		t.Fatalf("active owner could not lease: ok=%v err=%v", ok, err)
	}
	if _, err := store.AdvanceServerConfigurationRevision(ctx, server.ID, 0); err != nil {
		t.Fatalf("active owner could not advance configuration: %v", err)
	}
}

func TestFrozenServerRefusesJobCreationAndLeasing(t *testing.T) {
	ctx := context.Background()
	store, server, now := commandFenceFixture(t)
	if _, err := store.InitializeServerAuthority(ctx, server.ID, "owner-hub-aaaa"); err != nil {
		t.Fatal(err)
	}
	queued := commandFenceJob(server, "queued000001", now)
	if _, _, err := store.CreateJob(ctx, queued, "hash-queued", now); err != nil {
		t.Fatal(err)
	}
	freezeForCommandTest2(t, store, server)
	if _, _, err := store.CreateJob(ctx, commandFenceJob(server, "afterfreeze1", now), "hash-after", now); !errors.Is(err, ErrServerNotAuthoritative) {
		t.Fatalf("frozen server accepted a new job: %v", err)
	}
	if lease, ok, err := store.LeaseNextActionJob(ctx, server.ID, now, time.Minute); !errors.Is(err, ErrServerNotAuthoritative) || ok || lease.Token != "" {
		t.Fatalf("frozen server leased a queued job: lease=%+v ok=%v err=%v", lease, ok, err)
	}
	var state string
	if err := store.db.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id=?`, queued.ID).Scan(&state); err != nil || state != string(contracts.JobQueued) {
		t.Fatalf("a refused lease changed the queued job: state=%q err=%v", state, err)
	}
}

// freezeForCommandTest2 freezes an already-initialized server.
func freezeForCommandTest2(t *testing.T, store *Store, server contracts.Server) {
	t.Helper()
	if _, err := store.FreezeServerAuthority(context.Background(), AuthorityTransition{ServerID: server.ID, CutoverID: "cutover-0001", RequestDigest: authorityDigest("a"), FrontierDigest: authorityDigest("b")}); err != nil {
		t.Fatal(err)
	}
}

func TestOldOwnerCannotCompleteALeasedActionAfterFreeze(t *testing.T) {
	ctx := context.Background()
	store, server, now := commandFenceFixture(t)
	if _, err := store.InitializeServerAuthority(ctx, server.ID, "owner-hub-aaaa"); err != nil {
		t.Fatal(err)
	}
	job := commandFenceJob(server, "leased000001", now)
	created, _, err := store.CreateJob(ctx, job, "hash-leased", now)
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := store.LeaseNextActionJob(ctx, server.ID, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	freezeForCommandTest2(t, store, server)
	response := contracts.ActionResponse{RequestID: job.Action.RequestID, Accepted: true, Revision: 2}
	if _, err := store.CompleteActionJob(ctx, created.ID, lease.Token, response, now.Add(time.Second)); !errors.Is(err, ErrServerNotAuthoritative) {
		t.Fatalf("a frozen server applied an action result: %v", err)
	}
	var state, result string
	if err := store.db.QueryRowContext(ctx, `SELECT state, COALESCE(result_json,'') FROM jobs WHERE id=?`, created.ID).Scan(&state, &result); err != nil {
		t.Fatal(err)
	}
	if state == string(contracts.JobSucceeded) || result != "" {
		t.Fatalf("refused completion still changed the job: state=%q result=%q", state, result)
	}
	if _, err := store.CompleteActionJobForServer(ctx, server.ID, response, now.Add(time.Second)); err == nil {
		t.Fatal("completion by request id bypassed the authority fence")
	}
}

func TestFrozenServerRefusesDesiredStateChanges(t *testing.T) {
	ctx := context.Background()
	store, server, _ := commandFenceFixture(t)
	freezeForCommandTest(t, store, server)
	params, _ := json.Marshal(map[string]any{"quota_percent": 50})
	if _, err := store.TransitionControlPolicy(ctx, server.ID, "cpu-controls", "service", "nginx.service", 0, contracts.ControlPolicy{Kind: "cpu-quota", State: contracts.ControlPolicyApplied, Parameters: params}); !errors.Is(err, ErrServerNotAuthoritative) {
		t.Fatalf("frozen server changed a control policy: %v", err)
	}
	if _, err := store.TransitionModuleInstallation(ctx, server.ID, "port-traffic", 0, contracts.ModuleInstallation{Version: "1.0.0", State: contracts.ModuleDownloading}); !errors.Is(err, ErrServerNotAuthoritative) {
		t.Fatalf("frozen server changed a module installation: %v", err)
	}
	if _, err := store.AdvanceServerConfigurationRevision(ctx, server.ID, 0); !errors.Is(err, ErrServerNotAuthoritative) {
		t.Fatalf("frozen server advanced configuration: %v", err)
	}
	var rows int
	if err := store.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM control_policies WHERE server_id=?)+(SELECT COUNT(*) FROM module_installations WHERE server_id=?)`, string(server.ID), string(server.ID)).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refused changes left durable rows: rows=%d err=%v", rows, err)
	}
}

func TestRelinquishedServerKeepsRefusingCommands(t *testing.T) {
	ctx := context.Background()
	store, server, now := commandFenceFixture(t)
	freezeForCommandTest(t, store, server)
	if _, err := store.RelinquishServerAuthority(ctx, AuthorityTransition{ServerID: server.ID, CutoverID: "cutover-0001", RequestDigest: authorityDigest("a"), FrontierDigest: authorityDigest("b")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateJob(ctx, commandFenceJob(server, "relinquish01", now), "hash-rel", now); !errors.Is(err, ErrServerNotAuthoritative) {
		t.Fatalf("relinquished server accepted a job: %v", err)
	}
}

func TestUnownedServerKeepsLegacyCommands(t *testing.T) {
	ctx := context.Background()
	store, server, now := commandFenceFixture(t)
	if _, _, err := store.CreateJob(ctx, commandFenceJob(server, "legacy000001", now), "hash-legacy", now); err != nil {
		t.Fatalf("a server without an authority row lost legacy job creation: %v", err)
	}
	if _, err := store.AdvanceServerConfigurationRevision(ctx, server.ID, 0); err != nil {
		t.Fatalf("legacy configuration advance: %v", err)
	}
}

func TestRequireServerAuthorityReportsOwnership(t *testing.T) {
	ctx := context.Background()
	store, server, _ := commandFenceFixture(t)
	if err := store.RequireServerAuthority(ctx, server.ID); err != nil {
		t.Fatalf("an unowned server must stay usable: %v", err)
	}
	if _, err := store.InitializeServerAuthority(ctx, server.ID, "owner-hub-aaaa"); err != nil {
		t.Fatal(err)
	}
	if err := store.RequireServerAuthority(ctx, server.ID); err != nil {
		t.Fatalf("an active owner must pass: %v", err)
	}
	freezeForCommandTest2(t, store, server)
	if err := store.RequireServerAuthority(ctx, server.ID); !errors.Is(err, ErrServerNotAuthoritative) {
		t.Fatalf("a frozen server passed the ownership check: %v", err)
	}
}

func TestFrozenServerRefusesEnrollmentClaims(t *testing.T) {
	ctx := context.Background()
	store, server, now := commandFenceFixture(t)
	if _, err := store.InitializeServerAuthority(ctx, server.ID, "owner-hub-aaaa"); err != nil {
		t.Fatal(err)
	}
	job, created, err := store.CreateJob(ctx, contracts.Job{ID: "enrollment-fence-0123", Kind: "enrollment", State: contracts.JobQueued, IdempotencyKey: "enroll-fence-1", TargetServerID: server.ID, ExpiresAt: now.Add(time.Hour)}, "hash-enroll", now)
	if err != nil || !created {
		t.Fatalf("create enrollment job: %+v created=%v err=%v", job, created, err)
	}
	freezeForCommandTest2(t, store, server)
	if _, err := store.ClaimEnrollmentJob(ctx, job.ID, server.ID, job.Revision, now.Add(time.Second)); !errors.Is(err, ErrServerNotAuthoritative) {
		t.Fatalf("a frozen server claimed an enrollment: %v", err)
	}
	var state string
	if err := store.db.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id=?`, job.ID).Scan(&state); err != nil || state != string(contracts.JobQueued) {
		t.Fatalf("a refused claim changed the job: state=%q err=%v", state, err)
	}
}
