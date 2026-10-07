package webupdate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestFleetIntentRetainedDuringInterruptionAndCompletedOnce(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	req := Request{JobID: "update-node-fleet-test01", Version: "1.2.0", Deadline: now.Add(time.Hour)}
	if err := SubmitFleet(dir, req, now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, ok, err := Take(dir)
		if err != nil || !ok || got.JobID != req.JobID {
			t.Fatalf("take=%+v ok=%v err=%v", got, ok, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	installs := 0
	_, err := RunFleet(ctx, dir, req, "1.0.0", func(context.Context, string) error { installs++; cancel(); return context.Canceled }, func(context.Context, string) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interruption=%v", err)
	}
	if _, ok, err := Take(dir); err != nil || !ok {
		t.Fatalf("interruption lost intent: %v %v", ok, err)
	}
	// A new worker sees the newly installed release and health-checks without
	// reactivating. Durable per-job completion survives a producer restart.
	healthChecks := 0
	done, err := RunFleet(context.Background(), dir, req, "1.2.0", func(context.Context, string) error { installs++; return nil }, func(context.Context, string) error { healthChecks++; return nil })
	if err != nil || !done || installs != 1 || healthChecks != 1 {
		t.Fatalf("done=%v err=%v installs=%d health=%d", done, err, installs, healthChecks)
	}
	if _, err := os.Stat(filepath.Join(dir, RequestFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed intent was not removed")
	}
	if err := SubmitFleet(dir, req, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := Take(dir); ok {
		t.Fatal("completed replay queued another activation")
	}
	result, err := FleetResult(dir, req.JobID)
	if err != nil || result.State != StateSucceeded || result.Target != req.Version {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	req.Version = "1.3.0"
	if err := SubmitFleet(dir, req, time.Now()); err == nil {
		t.Fatal("job identity reused for a different release")
	}
}

func TestFleetWorkerFailureExpiredAndDowngradeAreDurable(t *testing.T) {
	for _, kind := range []string{"install", "health", "expired", "downgrade"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Now().UTC()
			req := Request{JobID: "update-fleet-failure-01", Version: "1.2.0", Deadline: now.Add(time.Hour)}
			if err := SubmitFleet(dir, req, now); err != nil {
				t.Fatal(err)
			}
			installed := "1.0.0"
			if kind == "downgrade" {
				installed = "2.0.0"
			}
			if kind == "expired" {
				req.Deadline = now.Add(-time.Second)
			}
			done, err := RunFleet(context.Background(), dir, req, installed, func(context.Context, string) error {
				if kind == "install" {
					return errors.New("untrusted release")
				}
				return nil
			}, func(context.Context, string) error {
				if kind == "health" {
					return errors.New("new service unhealthy")
				}
				return nil
			})
			if done || err == nil {
				t.Fatalf("done=%v err=%v", done, err)
			}
			result, err := FleetResult(dir, req.JobID)
			if err != nil || result.State != StateFailed {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestCoreActionRequiresFinalMatchingVersionAndRejectsCallerPaths(t *testing.T) {
	dir := t.TempDir()
	id := contracts.ServerID("node-core-action-test01")
	now := time.Now().UTC()
	req := Request{JobID: "update-core-action-test01", Version: "1.2.0", Deadline: now.Add(time.Hour)}
	if err := SubmitFleet(dir, req, now); err != nil {
		t.Fatal(err)
	}
	if _, err := RunFleet(context.Background(), dir, req, "1.0.0", func(context.Context, string) error { return nil }, func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(contracts.CoreUpdateIntent{JobID: req.JobID, Release: req.Version})
	action := contracts.ActionRequest{Protocol: contracts.HelperProtocol, RequestID: "core-action-test01", Action: "core.update", TargetServerID: id, Target: "payesh-core", IdempotencyKey: "core-action-test01", Arguments: body, Deadline: req.Deadline}
	handler := CoreActionHandler(dir, id, "1.2.0", nil)
	result := handler(context.Background(), action)
	if !result.Accepted || result.CoreUpdate == nil || result.CoreUpdate.JobID != req.JobID {
		t.Fatalf("result=%+v", result)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result = CoreActionHandler(dir, id, "1.0.0", nil)(ctx, action)
	if result.Accepted {
		t.Fatal("old node version accepted worker result")
	}
	action.Arguments = []byte(`{"job_id":"update-core-action-test01","release":"1.2.0","path":"/tmp/arbitrary"}`)
	if handler(context.Background(), action).Accepted {
		t.Fatal("caller path accepted")
	}
	action.Arguments = body
	action.TargetServerID = "different-node-test01"
	if handler(context.Background(), action).Accepted {
		t.Fatal("wrong node target accepted")
	}
}

func TestServiceInboxForgedResultDoesNotSkipInstallation(t *testing.T) {
	dir := t.TempDir()
	req := Request{JobID: "forged-inbox-result01", Version: "1.2.0", Deadline: time.Now().Add(time.Hour)}
	forged := Status{JobID: req.JobID, Target: req.Version, State: StateSucceeded, UpdatedAt: time.Now()}
	if err := writeJSON(dir, fleetResultName(req.JobID), forged, 0644); err != nil {
		t.Fatal(err)
	}
	if err := SubmitFleet(dir, req, time.Now()); err != nil {
		t.Fatal(err)
	}
	installs := 0
	health := 0
	done, err := RunFleet(context.Background(), dir, req, "1.0.0", func(context.Context, string) error { installs++; return nil }, func(context.Context, string) error { health++; return nil })
	if err != nil || !done || installs != 1 || health != 1 {
		t.Fatalf("forged result bypassed worker: done=%v err=%v install=%d health=%d", done, err, installs, health)
	}
}

func TestIncompleteRollbackRetainsIntentForRecovery(t *testing.T) {
	dir := t.TempDir()
	req := Request{JobID: "incomplete-restore-01", Version: "1.2.0", Deadline: time.Now().Add(time.Hour)}
	if err := SubmitFleet(dir, req, time.Now()); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("candidate health failed")
	done, err := RunFleet(context.Background(), dir, req, "1.0.0", func(context.Context, string) error { return nil }, func(context.Context, string) error {
		return InstallationRollbackError(cause, errors.New("service restart failed"))
	})
	if done || !errors.Is(err, ErrRollbackIncomplete) || !errors.Is(err, cause) {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if _, ok, err := Take(dir); err != nil || !ok {
		t.Fatalf("lost recovery intent: %v %v", ok, err)
	}
	if _, err := FleetResult(dir, req.JobID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete recovery marked terminal: %v", err)
	}
}

func TestCompleteFleetRollbackRetainsIntentOnConflictsAndWriteFailure(t *testing.T) {
	for _, tc := range []string{"target-conflict", "success-conflict", "result-write-failure", "status-write-failure"} {
		t.Run(tc, func(t *testing.T) {
			dir := t.TempDir()
			req := Request{JobID: "rollback-completion", Version: "1.2.0", Deadline: time.Now().Add(-time.Minute)}
			if err := SubmitFleet(dir, req, req.Deadline.Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(dir, RequestFile))
			if err != nil {
				t.Fatal(err)
			}
			var existing Status
			switch tc {
			case "target-conflict", "success-conflict":
				existing = Status{JobID: req.JobID, Target: "1.2.0", State: StateFailed}
				if tc == "target-conflict" {
					existing.Target = "1.3.0"
				} else {
					existing.State = StateSucceeded
				}
				if err := publishFleetResult(dir, existing); err != nil {
					t.Fatal(err)
				}
			case "result-write-failure":
				if err := os.Mkdir(fleetResultsDir(dir), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(fleetResultsDir(dir), fleetResultName(req.JobID)), 0755); err != nil {
					t.Fatal(err)
				}
			case "status-write-failure":
				if err := os.Remove(filepath.Join(dir, StatusFile)); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(dir, StatusFile), 0755); err != nil {
					t.Fatal(err)
				}
			}
			done, err := CompleteFleetRollback(dir, req)
			if done || err == nil || errors.Is(err, ErrInstallationRestored) {
				t.Fatalf("failed completion claimed restoration: %v %v", done, err)
			}
			after, err := os.ReadFile(filepath.Join(dir, RequestFile))
			if err != nil || string(after) != string(before) {
				t.Fatalf("failed completion lost intent: %q %v", after, err)
			}
			if tc == "target-conflict" || tc == "success-conflict" {
				result, err := FleetResult(dir, req.JobID)
				if err != nil || result.Target != existing.Target || result.State != existing.State {
					t.Fatalf("conflicting result overwritten: %+v %v", result, err)
				}
			}
		})
	}
}
