package updater

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

type storeActionProducer struct{ store *monitoring.Store }

func (p storeActionProducer) CreateActionJob(ctx context.Context, job contracts.Job, request contracts.ActionRequest, hash string, now time.Time) (contracts.Job, bool, error) {
	job.Action = &request
	return p.store.CreateJob(ctx, job, hash, now)
}

func TestFleetExecutorDurablePendingRestartAndVerifiedOutcome(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store := schedulerStore(t)
	node := schedulerServer("node-remote-update-001", "node", "connected")
	node.Capabilities = append(node.Capabilities, "core-update", "actions")
	if err := store.EnsureServer(ctx, node); err != nil {
		t.Fatal(err)
	}
	executor := &FleetExecutor{Store: store, Producer: storeActionProducer{store: store}}
	scheduler := NewScheduler(store, executor)
	parent, err := scheduler.Schedule(ctx, Plan{Release: "1.2.0", IdempotencyKey: "remote-update-plan", Targets: []Target{{Server: node, Compatible: true}}, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.RunOnce(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	children, err := scheduler.children(ctx, parent.ID)
	if err != nil || len(children) != 1 || children[0].State != contracts.JobRunning {
		t.Fatalf("children=%+v err=%v", children, err)
	}
	child := children[0]
	action, found, err := store.GetJob(ctx, CoreUpdateActionID(child.ID))
	if err != nil || !found || action.State != contracts.JobQueued || action.Action.Action != "core.update" {
		t.Fatalf("action=%+v err=%v", action, err)
	}
	// A restarted worker reconciles its own action rather than conflicting with it.
	resumed := NewScheduler(store, &FleetExecutor{Store: store, Producer: storeActionProducer{store: store}})
	node.ConnectionState = "disconnected"
	if err := store.UpsertServer(ctx, node); err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.RunOnce(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	child, _, _ = store.GetJob(ctx, child.ID)
	if child.State != contracts.JobRunning {
		t.Fatal("disconnect during activation falsely failed update")
	}
	lease, ok, err := store.LeaseNextActionJob(ctx, node.ID, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("lease=%+v ok=%v err=%v", lease, ok, err)
	}
	_, err = store.CompleteActionJob(ctx, lease.Job.ID, lease.Token, contracts.ActionResponse{RequestID: lease.Action.RequestID, Accepted: true, CoreUpdate: &contracts.CoreUpdateResult{JobID: child.ID, Release: "1.2.0", State: "succeeded"}}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.RunOnce(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	child, _, _ = store.GetJob(ctx, child.ID)
	if child.State != contracts.JobRunning {
		t.Fatal("result without restarted version was marked complete")
	}
	node.Version = "1.2.0"
	node.ConnectionState = "connected"
	if err := store.UpsertServer(ctx, node); err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.RunOnce(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	child, _, _ = store.GetJob(ctx, child.ID)
	if child.State != contracts.JobSucceeded {
		t.Fatalf("child=%+v", child)
	}
	if _, err := resumed.RunOnce(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	parent, _, _ = store.GetJob(ctx, parent.ID)
	if parent.State != contracts.JobSucceeded {
		t.Fatalf("parent=%+v", parent)
	}
	actions, err := store.ListJobsByKind(ctx, "core-update-action")
	if err != nil || len(actions) != 1 {
		t.Fatalf("duplicate activation action: %d err=%v", len(actions), err)
	}
}

func TestFleetExecutorRejectsDeliveryOnlyOrMismatchedActivation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *contracts.CoreUpdateResult
	}{
		{"delivery accepted", nil}, {"wrong job", &contracts.CoreUpdateResult{JobID: "wrong-job", Release: "1.2.0", State: "succeeded"}}, {"wrong release", &contracts.CoreUpdateResult{JobID: "child-remote-test01", Release: "1.3.0", State: "succeeded"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			store := schedulerStore(t)
			node := schedulerServer("node-remote-check-001", "node", "connected")
			node.Version = "1.2.0"
			node.Capabilities = []string{"core-update", "actions"}
			if err := store.EnsureServer(ctx, node); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.CreateJob(ctx, contracts.Job{ID: "parent-remote-test01", Kind: UpdateJobKind, State: contracts.JobRunning, IdempotencyKey: "parent-remote-test01", ExpiresAt: now.Add(time.Hour)}, "parenthash", now); err != nil {
				t.Fatal(err)
			}
			child, _, err := store.CreateJob(ctx, contracts.Job{ID: "child-remote-test01", Kind: UpdateNodeJobKind, State: contracts.JobRunning, TargetServerID: node.ID, IdempotencyKey: "child-remote-test01", ExpiresAt: now.Add(time.Hour), Action: &contracts.ActionRequest{Protocol: contracts.HelperProtocol, RequestID: "child-remote-test01", Action: "update", TargetServerID: node.ID, IdempotencyKey: "child-remote-test01", Target: "payesh-agent", Arguments: []byte(`{"parent_job_id":"parent-remote-test01","release":"1.2.0"}`), Deadline: now.Add(time.Hour)}}, "childhash", now)
			if err != nil {
				t.Fatal(err)
			}
			exec := &FleetExecutor{Store: store, Producer: storeActionProducer{store: store}}
			intent := Execution{JobID: child.ID, Release: "1.2.0", Target: node}
			if err := exec.Execute(ctx, intent); !errors.Is(err, ErrExecutionPending) {
				t.Fatalf("queue err=%v", err)
			}
			lease, ok, err := store.LeaseNextActionJob(ctx, node.ID, now, time.Minute)
			if err != nil || !ok {
				t.Fatalf("lease err=%v ok=%v", err, ok)
			}
			if _, err = store.CompleteActionJob(ctx, lease.Job.ID, lease.Token, contracts.ActionResponse{RequestID: lease.Action.RequestID, Accepted: true, CoreUpdate: tc.result}, now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := exec.Execute(ctx, intent); err == nil || errors.Is(err, ErrExecutionPending) {
				t.Fatalf("unverified activation accepted: %v", err)
			}
		})
	}
}

func TestFleetCancellationPreventsQueuedDerivativeActivation(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store := schedulerStore(t)
	node := schedulerServer("node-cancel-fleet-001", "node", "connected")
	node.Capabilities = []string{"core-update", "actions"}
	if err := store.EnsureServer(ctx, node); err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(store, &FleetExecutor{Store: store, Producer: storeActionProducer{store: store}})
	parent, err := scheduler.Schedule(ctx, Plan{Release: "1.2.0", IdempotencyKey: "cancel-fleet-plan", Targets: []Target{{Server: node, Compatible: true}}, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.RunOnce(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	parent, _, err = store.GetJob(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.RequestJobCancellation(ctx, parent.ID, "cancel-parent", parent.Revision, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if lease, ok, err := store.LeaseNextActionJob(ctx, node.ID, time.Now().UTC(), time.Minute); err != nil || ok {
		t.Fatalf("activation leased between cancellation and scheduler tick: lease=%+v ok=%v err=%v", lease, ok, err)
	}
	if _, err = scheduler.RunOnce(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	if lease, ok, err := store.LeaseNextActionJob(ctx, node.ID, time.Now().UTC(), time.Minute); err != nil || ok {
		t.Fatalf("cancelled parent still permits activation: lease=%+v found=%v err=%v", lease, ok, err)
	}
	actions, err := store.ListJobsByKind(ctx, "core-update-action")
	if err != nil || len(actions) != 1 || actions[0].State != contracts.JobCancelled {
		t.Fatalf("actions=%+v err=%v", actions, err)
	}
}

func TestFleetOrchestrationChildIsNeverTransportDeliverable(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store := schedulerStore(t)
	node := schedulerServer("node-child-private-001", "node", "connected")
	node.Capabilities = []string{"core-update", "actions"}
	if err := store.EnsureServer(ctx, node); err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(store, &FleetExecutor{Store: store, Producer: storeActionProducer{store: store}})
	if _, err := scheduler.Schedule(ctx, Plan{Release: "1.2.0", IdempotencyKey: "private-child-plan", Targets: []Target{{Server: node, Compatible: true}}, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if lease, ok, err := store.LeaseNextActionJob(ctx, node.ID, now, time.Minute); err != nil || ok {
		t.Fatalf("orchestration child leaked into transport: lease=%+v found=%v err=%v", lease, ok, err)
	}
}

func TestRevokedRunningRolloutCancelsQueuedActivation(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store := schedulerStore(t)
	node := schedulerServer("node-revoke-fleet-001", "node", "connected")
	node.Capabilities = []string{"core-update", "actions"}
	if err := store.EnsureServer(ctx, node); err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(store, &FleetExecutor{Store: store, Producer: storeActionProducer{store: store}})
	parent, err := scheduler.Schedule(ctx, Plan{Release: "1.2.0", IdempotencyKey: "revoke-fleet-plan", Targets: []Target{{Server: node, Compatible: true}}, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = scheduler.RunOnce(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	if err = store.MarkServerRevoked(ctx, node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = scheduler.RunOnce(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	if lease, ok, err := store.LeaseNextActionJob(ctx, node.ID, time.Now(), time.Minute); err != nil || ok {
		t.Fatalf("revoked rollout leaves queued activation: %+v %v %v", lease, ok, err)
	}
}

type beforeCreateActionProducer struct {
	store  *monitoring.Store
	before func(context.Context) error
}

func (p beforeCreateActionProducer) CreateActionJob(ctx context.Context, job contracts.Job, request contracts.ActionRequest, hash string, now time.Time) (contracts.Job, bool, error) {
	if err := p.before(ctx); err != nil {
		return contracts.Job{}, false, err
	}
	job.Action = &request
	return p.store.CreateJob(ctx, job, hash, now)
}

func TestCancellationOrRevocationBeforeDerivativeCreationPreventsImmediateDelivery(t *testing.T) {
	for _, boundary := range []string{"parent cancellation", "child cancellation", "revocation"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			store := schedulerStore(t)
			node := schedulerServer("node-create-race-001", "node", "connected")
			node.Capabilities = []string{"core-update", "actions"}
			if err := store.EnsureServer(ctx, node); err != nil {
				t.Fatal(err)
			}
			executor := &FleetExecutor{Store: store}
			scheduler := NewScheduler(store, executor)
			parent, err := scheduler.Schedule(ctx, Plan{Release: "1.2.0", IdempotencyKey: "creation-race-plan", Targets: []Target{{Server: node, Compatible: true}}, ExpiresAt: now.Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			executor.Producer = beforeCreateActionProducer{store: store, before: func(ctx context.Context) error {
				if boundary == "revocation" {
					return store.MarkServerRevoked(ctx, node.ID)
				}
				id := parent.ID
				if boundary == "child cancellation" {
					children, err := scheduler.children(ctx, parent.ID)
					if err != nil {
						return err
					}
					id = children[0].ID
				}
				job, _, err := store.GetJob(ctx, id)
				if err != nil {
					return err
				}
				_, err = store.RequestJobCancellation(ctx, id, "cancel-before-create", job.Revision, time.Now().UTC())
				return err
			}}
			if _, err := scheduler.RunOnce(ctx, parent.ID); err != nil {
				t.Fatal(err)
			}
			// No scheduler reconciliation runs between cancellation and delivery.
			if lease, ok, err := store.LeaseNextActionJob(ctx, node.ID, time.Now().UTC(), time.Minute); err != nil || ok {
				t.Fatalf("unauthorized newly created derivative delivered: %+v ok=%v err=%v", lease, ok, err)
			}
		})
	}
}
