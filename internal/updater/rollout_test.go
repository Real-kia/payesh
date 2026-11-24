package updater

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestRolloutWavesPauseAndResumeAfterRestart(t *testing.T) {
	ctx := context.Background()
	store := schedulerStore(t)
	now := time.Now().UTC()
	var targets []Target
	for i := 22; i >= 0; i-- {
		server := schedulerServer(fmt.Sprintf("node-rollout-test-%03d", i), "node", "connected")
		if err := store.EnsureServer(ctx, server); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, Target{Server: server, Compatible: true})
	}
	var calls []contracts.ServerID
	execute := ExecutorFunc(func(_ context.Context, execution Execution) error {
		calls = append(calls, execution.Target.ID)
		if execution.Target.ID == targets[0].Server.ID || execution.Target.ID == targets[1].Server.ID {
			return errors.New("test activation failed")
		}
		return nil
	})
	scheduler := &Scheduler{Store: store, Executor: execute, Now: func() time.Time { return now }}
	threshold := 0
	plan := Plan{Release: "1.2.0", IdempotencyKey: "rollout-waves", Targets: targets, ExpiresAt: now.Add(time.Hour),
		RolloutOptions: RolloutOptions{Canary: 1, MaxFailPct: &threshold}}
	parent, err := scheduler.Schedule(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	job, err := scheduler.Run(ctx, parent.ID)
	if err != nil || job.State != contracts.JobPaused || len(calls) != 1 || calls[0] != targets[0].Server.ID {
		t.Fatalf("canary job=%+v calls=%v err=%v", job, calls, err)
	}
	restarted := &Scheduler{Store: store, Executor: execute, Now: scheduler.Now}
	paused, err := restarted.RunOnce(ctx, job.ID)
	if err != nil || paused.Revision != job.Revision || len(calls) != 1 {
		t.Fatalf("paused job changed after restart: %+v calls=%v err=%v", paused, calls, err)
	}
	if _, err := restarted.ResumePaused(ctx, job.ID, job.Revision-1); err == nil {
		t.Fatal("stale revision resumed the job")
	}
	if _, err := restarted.ResumePaused(ctx, job.ID, job.Revision); err != nil {
		t.Fatal(err)
	}
	job, err = restarted.Run(ctx, job.ID)
	if err != nil || job.State != contracts.JobPaused || len(calls) != 21 {
		t.Fatalf("second wave job=%+v calls=%v err=%v", job, calls, err)
	}
	if _, err := restarted.ResumePaused(ctx, job.ID, job.Revision); err != nil {
		t.Fatal(err)
	}
	job, err = restarted.Run(ctx, job.ID)
	if err != nil || job.State != contracts.JobFailed || job.Error == nil || job.Error.Code != "partial_failure" {
		t.Fatalf("final job=%+v err=%v", job, err)
	}
	want := make([]contracts.ServerID, len(targets))
	for i, target := range targets {
		want[i] = target.Server.ID
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v, want %v", calls, want)
	}
}

func TestRolloutWindowStaggerAndVersionSkip(t *testing.T) {
	ctx := context.Background()
	store := schedulerStore(t)
	now := time.Now().UTC()
	var targets []Target
	for i := 0; i < 4; i++ {
		server := schedulerServer(fmt.Sprintf("node-window-test-%03d", i), "node", "connected")
		if i == 0 {
			server.Version = "1.2.0"
		}
		// The agent build and installed release are independent.
		server.Capabilities = append(server.Capabilities, "node-version:1.2.0")
		if err := store.EnsureServer(ctx, server); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, Target{Server: server, Compatible: true})
	}
	var calls []contracts.ServerID
	scheduler := &Scheduler{Store: store, Now: func() time.Time { return now }, Executor: ExecutorFunc(func(_ context.Context, e Execution) error {
		calls = append(calls, e.Target.ID)
		return nil
	})}
	window := &UpdateWindow{Start: now.Add(time.Minute), End: now.Add(2 * time.Minute)}
	parent, err := scheduler.Schedule(ctx, Plan{Release: "1.2.0", IdempotencyKey: "window", Targets: targets, ExpiresAt: now.Add(time.Hour),
		RolloutOptions: RolloutOptions{SkipUpToDate: true, UpdateWindow: window, StaggerSeconds: 30}})
	if err != nil {
		t.Fatal(err)
	}
	run := func(at time.Time, wantCalls int) contracts.Job {
		t.Helper()
		now = at
		job, err := scheduler.RunOnce(ctx, parent.ID)
		if err != nil || len(calls) != wantCalls {
			t.Fatalf("at %v: job=%+v calls=%v err=%v", at, job, calls, err)
		}
		return job
	}
	run(now, 0)
	run(window.Start, 1)
	run(window.Start.Add(29*time.Second), 1)
	run(window.Start.Add(30*time.Second), 2)
	job := run(window.End, 2)
	if job.State != contracts.JobFailed || job.Error.Code != "update_window_closed" {
		t.Fatalf("window outcome=%+v", job)
	}
	children, err := store.ListUpdateTargets(ctx, parent.ID)
	if err != nil || len(children) != 4 || children[0].State != contracts.JobSkipped || children[3].State != contracts.JobFailed {
		t.Fatalf("children=%+v err=%v", children, err)
	}
	if !reflect.DeepEqual(calls, []contracts.ServerID{targets[1].Server.ID, targets[2].Server.ID}) {
		t.Fatalf("unexpected activations: %v", calls)
	}
}

func TestRolloutRegistrationIsAtomic(t *testing.T) {
	ctx := context.Background()
	store := schedulerStore(t)
	now := time.Now().UTC()
	server := schedulerServer("node-atomic-valid", "node", "connected")
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	invalid := schedulerServer("z", "node", "connected")
	scheduler := NewScheduler(store, nil)
	_, err := scheduler.Schedule(ctx, Plan{Release: "1.2.0", IdempotencyKey: "atomic-rollout",
		Targets: []Target{{Server: server, Compatible: true}, {Server: invalid, Compatible: true}}, ExpiresAt: now.Add(time.Hour)})
	if err == nil {
		t.Fatal("expected invalid target to prevent registration")
	}
	for _, kind := range []string{UpdateJobKind, UpdateNodeJobKind} {
		jobs, err := store.ListJobsByKind(ctx, kind)
		if err != nil || len(jobs) != 0 {
			t.Fatalf("%s: persisted jobs=%+v err=%v", kind, jobs, err)
		}
	}
}

func TestRolloutsOnSameNodeWaitForActiveUpdate(t *testing.T) {
	ctx := context.Background()
	store := schedulerStore(t)
	now := time.Now().UTC()
	node := schedulerServer("node-rollout-overlap", "node", "connected")
	if err := store.EnsureServer(ctx, node); err != nil {
		t.Fatal(err)
	}
	finish := false
	var calls []string
	scheduler := &Scheduler{Store: store, Now: func() time.Time { return now }, Executor: ExecutorFunc(func(_ context.Context, execution Execution) error {
		calls = append(calls, execution.ParentID)
		if !finish {
			return ErrExecutionPending
		}
		return nil
	})}
	plan := Plan{Release: "1.2.0", IdempotencyKey: "rollout-first", DefaultExpiry: true,
		Targets: []Target{{Server: node, Compatible: true}}, ExpiresAt: now.Add(time.Hour)}
	first, err := scheduler.Schedule(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	plan.ExpiresAt = plan.ExpiresAt.Add(time.Second)
	replay, err := scheduler.Schedule(ctx, plan)
	if err != nil || replay.ID != first.ID {
		t.Fatalf("default expiry replay=%+v err=%v", replay, err)
	}
	plan.IdempotencyKey = "rollout-second"
	second, err := scheduler.Schedule(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.RunOnce(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.RunOnce(ctx, second.ID); !errors.Is(err, ErrUpdateConflict) {
		t.Fatalf("expected active update conflict, got %v", err)
	}
	finish = true
	for _, job := range []contracts.Job{first, second} {
		completed, err := scheduler.RunOnce(ctx, job.ID)
		if err != nil || completed.State != contracts.JobSucceeded {
			t.Fatalf("completed=%+v err=%v", completed, err)
		}
	}
	if !reflect.DeepEqual(calls, []string{first.ID, first.ID, second.ID}) {
		t.Fatalf("out-of-order activations: %v", calls)
	}
}

func TestRolloutFailureThresholdIsStrictAndExcludesSkips(t *testing.T) {
	children := []contracts.Job{
		{State: contracts.JobFailed}, {State: contracts.JobSucceeded},
		{State: contracts.JobSkipped}, {State: contracts.JobQueued},
	}
	threshold := 50
	policy := rolloutPolicy{RolloutOptions: RolloutOptions{Canary: 3, MaxFailPct: &threshold}}
	if wave, _, _ := policy.failedWave(children); wave != 0 {
		t.Fatalf("paused at exactly the threshold: wave=%d", wave)
	}
	threshold = 49
	if wave, failed, total := policy.failedWave(children); wave != 1 || failed != 1 || total != 2 {
		t.Fatalf("got wave=%d failures=%d total=%d", wave, failed, total)
	}
}
