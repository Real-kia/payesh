package updater

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func schedulerStore(t *testing.T) *monitoring.Store {
	t.Helper()
	store, err := monitoring.OpenStore(context.Background(), ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func schedulerServer(id, role, connection string) contracts.Server {
	return contracts.Server{ID: contracts.ServerID(id), Name: id, Role: role, Architecture: "amd64", Platform: "linux", Version: "1.0.0", Capabilities: []string{"updates"}, ConnectionState: connection, FreshnessState: "fresh"}
}

func TestSchedulerHubFirstOneNodeAtATimeAndDurableResults(t *testing.T) {
	ctx := context.Background()
	store := schedulerStore(t)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	hub := schedulerServer("hub-scheduler-test", "hub", "connected")
	node := schedulerServer("node-scheduler-test", "node", "connected")
	for _, server := range []contracts.Server{hub, node} {
		if err := store.EnsureServer(ctx, server); err != nil {
			t.Fatal(err)
		}
	}
	var calls []string
	scheduler := &Scheduler{Store: store, Now: func() time.Time { return now }, Executor: ExecutorFunc(func(_ context.Context, execution Execution) error {
		calls = append(calls, string(execution.Target.ID))
		return nil
	})}
	parent, err := scheduler.Schedule(ctx, Plan{Release: "1.1.0", IdempotencyKey: "schedule-test-1", Targets: []Target{{Server: node, Compatible: true}, {Server: hub, Compatible: true}}, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.RunOnce(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{string(hub.ID)}) {
		t.Fatalf("first worker pass calls=%v, want hub only", calls)
	}
	if _, err := scheduler.RunOnce(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{string(hub.ID), string(node.ID)}) {
		t.Fatalf("second worker pass calls=%v, want one node after hub", calls)
	}
	completed, found, err := store.GetJob(ctx, parent.ID)
	if err != nil || !found {
		t.Fatalf("parent lookup found=%v err=%v", found, err)
	}
	if completed.State != contracts.JobSucceeded {
		t.Fatalf("parent state=%s, want succeeded", completed.State)
	}
	// A replay after a completed parent is a no-op and cannot activate twice.
	if _, err := scheduler.RunOnce(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("replay made executor calls=%v", calls)
	}
}

func TestSchedulerProcessPendingSettlesUnavailableExecutor(t *testing.T) {
	ctx := context.Background()
	store := schedulerStore(t)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	server := schedulerServer("worker-unavailable-target", "node", "connected")
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	scheduler := &Scheduler{Store: store, Now: func() time.Time { return now }}
	parent, err := scheduler.Schedule(ctx, Plan{Release: "1.2.3", IdempotencyKey: "worker-unavailable-1", Targets: []Target{{Server: server, Compatible: true}}, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.ProcessPending(ctx); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.ProcessPending(ctx); err != nil {
		t.Fatal(err)
	}
	settled, found, err := store.GetJob(ctx, parent.ID)
	if err != nil || !found {
		t.Fatalf("parent found=%v err=%v", found, err)
	}
	if settled.State != contracts.JobFailed {
		t.Fatalf("parent state=%s, want failed", settled.State)
	}
	children, err := store.ListJobsByKind(ctx, UpdateNodeJobKind)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].Error == nil || children[0].Error.Code != "executor_unavailable" {
		t.Fatalf("children=%+v", children)
	}
}

func TestSchedulerRecordsOfflineIncompatibleAndExecutorUnavailable(t *testing.T) {
	ctx := context.Background()
	store := schedulerStore(t)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	offline := schedulerServer("offline-scheduler-1", "node", "disconnected")
	incompatible := schedulerServer("incompatible-sched", "node", "connected")
	good := schedulerServer("good-scheduler-01", "node", "connected")
	for _, server := range []contracts.Server{offline, incompatible, good} {
		if err := store.EnsureServer(ctx, server); err != nil {
			t.Fatal(err)
		}
	}
	scheduler := &Scheduler{Store: store, Now: func() time.Time { return now }}
	parent, err := scheduler.Schedule(ctx, Plan{Release: "1.1.0", IdempotencyKey: "schedule-test-2", Targets: []Target{
		{Server: offline, Compatible: true},
		{Server: incompatible, Compatible: false, Reason: "requires protocol v2"},
		{Server: good, Compatible: true},
	}, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := scheduler.RunOnce(ctx, parent.ID); err != nil && !errors.Is(err, ErrUpdateExecutorAbsent) {
			// executor_unavailable is recorded as a child result, not returned
			// as a worker failure; this branch is for unexpected errors only.
			t.Fatal(err)
		}
	}
	children, err := store.ListJobsByKind(ctx, UpdateNodeJobKind)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[contracts.ServerID]contracts.Job{}
	for _, child := range children {
		if len(child.IdempotencyKey) > len(parent.ID) && child.IdempotencyKey[:len(parent.ID)] == parent.ID {
			seen[child.TargetServerID] = child
		}
	}
	if seen[offline.ID].Error == nil || seen[offline.ID].Error.Code != "offline" {
		t.Fatalf("offline result=%+v", seen[offline.ID].Error)
	}
	if seen[incompatible.ID].Error == nil || seen[incompatible.ID].Error.Code != "incompatible" {
		t.Fatalf("incompatible result=%+v", seen[incompatible.ID].Error)
	}
	if seen[good.ID].Error == nil || seen[good.ID].Error.Code != "executor_unavailable" {
		t.Fatalf("executor result=%+v", seen[good.ID].Error)
	}
	parent, _, _ = store.GetJob(ctx, parent.ID)
	if parent.State != contracts.JobFailed || parent.Error == nil || parent.Error.Code != "partial_failure" {
		t.Fatalf("parent result=%+v", parent)
	}
}

func TestSchedulerWaitsForConflictingDurableJob(t *testing.T) {
	ctx := context.Background()
	store := schedulerStore(t)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	server := schedulerServer("conflict-scheduler-1", "node", "connected")
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	conflict, _, err := store.CreateJob(ctx, contracts.Job{ID: "module-conflict-job", Kind: "module", State: contracts.JobQueued, IdempotencyKey: "module-conflict-key", TargetServerID: server.ID, ExpiresAt: now.Add(time.Hour)}, "module-request", now)
	if err != nil {
		t.Fatal(err)
	}
	blocked := true
	scheduler := &Scheduler{Store: store, Now: func() time.Time { return now }, Executor: ExecutorFunc(func(context.Context, Execution) error {
		if blocked {
			t.Fatal("executor ran while conflicting job queued")
		}
		return nil
	})}
	parent, err := scheduler.Schedule(ctx, Plan{Release: "1.1.0", IdempotencyKey: "schedule-test-3", Targets: []Target{{Server: server, Compatible: true}}, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.RunOnce(ctx, parent.ID); !errors.Is(err, ErrUpdateConflict) {
		t.Fatalf("conflict error=%v", err)
	}
	// Completing the conflicting job durably releases the target.
	if _, err := store.TransitionJob(ctx, conflict.ID, conflict.Revision, contracts.JobSucceeded, 100, nil, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	blocked = false
	if _, err := scheduler.RunOnce(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerResumesRunningChildAfterWorkerCancellation(t *testing.T) {
	ctx := context.Background()
	store := schedulerStore(t)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	server := schedulerServer("restart-scheduler-1", "node", "connected")
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	parent, err := (&Scheduler{Store: store, Now: func() time.Time { return now }, Executor: ExecutorFunc(func(context.Context, Execution) error { return context.Canceled })}).Schedule(ctx, Plan{Release: "1.1.0", IdempotencyKey: "restart-schedule-1", Targets: []Target{{Server: server, Compatible: true}}, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Scheduler{Store: store, Now: func() time.Time { return now }, Executor: ExecutorFunc(func(context.Context, Execution) error { return context.Canceled })}).RunOnce(ctx, parent.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("first worker error=%v, want cancellation", err)
	}
	children, err := store.ListJobsByKind(ctx, UpdateNodeJobKind)
	if err != nil || len(children) != 1 || children[0].State != contracts.JobRunning {
		t.Fatalf("running child after interruption: jobs=%+v err=%v", children, err)
	}
	resumed := &Scheduler{Store: store, Now: func() time.Time { return now }, Executor: ExecutorFunc(func(context.Context, Execution) error { return nil })}
	if _, err := resumed.RunOnce(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	completed, _, err := store.GetJob(ctx, parent.ID)
	if err != nil || completed.State != contracts.JobSucceeded {
		t.Fatalf("resumed parent=%+v err=%v", completed, err)
	}
}

func TestSchedulerCancellationPropagatesToChildren(t *testing.T) {
	ctx := context.Background()
	store := schedulerStore(t)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	server := schedulerServer("cancel-scheduler-1", "node", "connected")
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	scheduler := &Scheduler{Store: store, Now: func() time.Time { return now }, Executor: ExecutorFunc(func(context.Context, Execution) error { t.Fatal("cancelled update executed"); return nil })}
	parent, err := scheduler.Schedule(ctx, Plan{Release: "1.1.0", IdempotencyKey: "cancel-schedule-1", Targets: []Target{{Server: server, Compatible: true}}, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := store.RequestJobCancellation(ctx, parent.ID, "cancel-request-1", parent.Revision, now.Add(time.Second))
	if err != nil || cancelled.State != contracts.JobCancelled {
		t.Fatalf("parent cancellation=%+v err=%v", cancelled, err)
	}
	if _, err := scheduler.RunOnce(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	children, err := store.ListJobsByKind(ctx, UpdateNodeJobKind)
	if err != nil || len(children) != 1 || children[0].State != contracts.JobCancelled {
		t.Fatalf("cancelled child: jobs=%+v err=%v", children, err)
	}
}

func TestSchedulerFailsClosedWhenJobIdentityEntropyFails(t *testing.T) {
	ctx := context.Background()
	store := schedulerStore(t)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	server := schedulerServer("entropy-scheduler-1", "node", "connected")
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	scheduler := &Scheduler{Store: store, Now: func() time.Time { return now }, IDGenerator: func(string) (string, error) { return "", errors.New("entropy unavailable") }}
	if _, err := scheduler.Schedule(ctx, Plan{Release: "1.1.0", IdempotencyKey: "entropy-schedule-1", Targets: []Target{{Server: server, Compatible: true}}, ExpiresAt: now.Add(time.Hour)}); err == nil {
		t.Fatal("schedule unexpectedly succeeded without secure identity entropy")
	}
	jobs, err := store.ListJobsByKind(ctx, UpdateJobKind)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("failed identity generation left parent jobs: %+v", jobs)
	}
}

func TestSchedulerHubFailureBlocksRemainingNodes(t *testing.T) {
	ctx := context.Background()
	store := schedulerStore(t)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	hub := schedulerServer("hub-gate-scheduler", "hub", "connected")
	node := schedulerServer("node-gate-scheduler", "node", "connected")
	for _, server := range []contracts.Server{hub, node} {
		if err := store.EnsureServer(ctx, server); err != nil {
			t.Fatal(err)
		}
	}
	var calls []contracts.ServerID
	scheduler := &Scheduler{Store: store, Now: func() time.Time { return now }, Executor: ExecutorFunc(func(_ context.Context, execution Execution) error {
		calls = append(calls, execution.Target.ID)
		if execution.Target.ID == hub.ID {
			return &IncompatibleError{Reason: "hub module set is incompatible"}
		}
		return nil
	})}
	parent, err := scheduler.Schedule(ctx, Plan{Release: "1.1.0", IdempotencyKey: "hub-gate-schedule-1", Targets: []Target{{Server: node, Compatible: true}, {Server: hub, Compatible: true}}, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.RunOnce(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.RunOnce(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []contracts.ServerID{hub.ID}) {
		t.Fatalf("node executor ran after hub failure: calls=%v", calls)
	}
	children, err := store.ListJobsByKind(ctx, UpdateNodeJobKind)
	if err != nil {
		t.Fatal(err)
	}
	var nodeJob contracts.Job
	for _, child := range children {
		if child.TargetServerID == node.ID {
			nodeJob = child
		}
	}
	if nodeJob.State != contracts.JobFailed || nodeJob.Error == nil || nodeJob.Error.Code != "hub_update_failed" {
		t.Fatalf("blocked node result=%+v", nodeJob)
	}
	completed, _, err := store.GetJob(ctx, parent.ID)
	if err != nil || completed.State != contracts.JobFailed {
		t.Fatalf("parent after hub failure=%+v err=%v", completed, err)
	}
}
