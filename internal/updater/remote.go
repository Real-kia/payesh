package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

// ErrExecutionPending means intent is durable but final healthy activation has
// not been confirmed. It must never be translated into a successful update.
var ErrExecutionPending = errors.New("updater: activation is pending")

type ActionProducer interface {
	CreateActionJob(context.Context, contracts.Job, contracts.ActionRequest, string, time.Time) (contracts.Job, bool, error)
}

type FleetExecutor struct {
	Store         *monitoring.Store
	Producer      ActionProducer
	LocalServerID contracts.ServerID
	Local         Executor
	Now           func() time.Time
}

func CoreUpdateActionID(jobID string) string {
	sum := sha256.Sum256([]byte(jobID))
	return "core-update-" + hex.EncodeToString(sum[:16])
}

func (e *FleetExecutor) Execute(ctx context.Context, execution Execution) error {
	if e == nil || e.Store == nil {
		return errors.New("updater: fleet executor is not configured")
	}
	if execution.Target.ID == e.LocalServerID {
		if e.Local == nil {
			return ErrUpdateExecutorAbsent
		}
		return e.Local.Execute(ctx, execution)
	}
	if e.Producer == nil {
		return ErrUpdateExecutorAbsent
	}
	supported := false
	for _, capability := range execution.Target.Capabilities {
		if capability == "core-update" {
			supported = true
		}
	}
	if !supported {
		return &IncompatibleError{Reason: "node must be upgraded to support signed core updates"}
	}
	child, found, err := e.Store.GetJob(ctx, execution.JobID)
	if err != nil {
		return err
	}
	if !found || child.TargetServerID != execution.Target.ID || !ValidRelease(execution.Release) {
		return errors.New("updater: update child identity is invalid")
	}
	now := time.Now().UTC()
	if e.Now != nil {
		now = e.Now().UTC()
	}
	id := CoreUpdateActionID(execution.JobID)
	action, found, err := e.Store.GetJob(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		body, _ := json.Marshal(contracts.CoreUpdateIntent{JobID: execution.JobID, Release: execution.Release})
		request := contracts.ActionRequest{Protocol: contracts.HelperProtocol, RequestID: id, Action: "core.update", TargetServerID: execution.Target.ID, IdempotencyKey: id, ExpectedRevision: execution.Target.ConfigurationRevision, Target: "payesh-core", Arguments: body, Deadline: child.ExpiresAt}
		encoded, _ := json.Marshal(request)
		hash := sha256.Sum256(encoded)
		action, _, err = e.Producer.CreateActionJob(ctx, contracts.Job{ID: id, Kind: "core-update-action", State: contracts.JobQueued, IdempotencyKey: id, TargetServerID: execution.Target.ID, ExpiresAt: child.ExpiresAt}, request, hex.EncodeToString(hash[:]), now)
		if err != nil {
			return err
		}
	}
	switch action.State {
	case contracts.JobFailed, contracts.JobCancelled, contracts.JobRecoveryRequired:
		if action.Error != nil {
			return fmt.Errorf("node core update failed: %s", action.Error.Code)
		}
		return errors.New("node core update did not complete")
	case contracts.JobSucceeded:
		if action.Result == nil || !action.Result.Accepted || action.Result.CoreUpdate == nil {
			return errors.New("node core update has no confirmed activation result")
		}
		result := action.Result.CoreUpdate
		if result.JobID != execution.JobID || result.Release != execution.Release || result.State != "succeeded" {
			return errors.New("node core update result does not match authorized intent")
		}
		// A correlated result alone is insufficient: require the restarted node's
		// authenticated hello to advertise the exact installed release as well.
		current, ok, err := e.Store.GetServer(ctx, execution.Target.ID)
		if err != nil {
			return err
		}
		if !ok || current.Version != execution.Release || current.ConnectionState != "connected" {
			return ErrExecutionPending
		}
		return nil
	default:
		return ErrExecutionPending
	}
}
