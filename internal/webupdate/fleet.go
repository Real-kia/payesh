package webupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/updater"
)

func validFleetRequest(req Request) bool {
	if len(req.JobID) == 0 || len(req.JobID) > 128 || !updater.ValidRelease(req.Version) || req.Deadline.IsZero() {
		return false
	}
	for _, c := range req.JobID {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func fleetResultName(jobID string) string {
	sum := sha256.Sum256([]byte(jobID))
	return "update-result-" + hex.EncodeToString(sum[:16]) + ".json"
}

func fleetResultsDir(dir string) string {
	if dir == DefaultDir {
		return "/var/lib/payesh-updates/results"
	}
	return filepath.Join(dir, "root-results")
}
func publishFleetResult(dir string, status Status) error {
	path := fleetResultsDir(dir)
	if err := safeDirectory(path, 0755, dir == DefaultDir); err != nil {
		return err
	}
	return writeJSON(path, fleetResultName(status.JobID), status, 0644)
}
func FleetResult(dir, jobID string) (Status, error) {
	var status Status
	path := filepath.Join(fleetResultsDir(dir), fleetResultName(jobID))
	if err := safeExistingParents(path, dir == DefaultDir); err != nil {
		return status, err
	}
	err := readJSON(path, &status)
	if err == nil && status.JobID != jobID {
		return Status{}, errors.New("fleet result identity mismatch")
	}
	return status, err
}

// SubmitFleet preserves exact job intent across producer and worker restarts.
// Final per-job results prevent a replay from activating a completed release.
func SubmitFleet(dir string, req Request, now time.Time) error {
	if !validFleetRequest(req) {
		return errors.New("invalid fleet update intent")
	}
	submitMu.Lock()
	defer submitMu.Unlock()
	if result, err := FleetResult(dir, req.JobID); err == nil {
		if result.Target != req.Version {
			return errors.New("fleet job was already used for another release")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var existing Request
	if err := readJSON(filepath.Join(dir, RequestFile), &existing); err == nil {
		if existing.JobID == req.JobID && existing.Version == req.Version && existing.Deadline.Equal(req.Deadline) {
			return nil
		}
		return ErrBusy
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !req.Deadline.After(now) {
		return errors.New("fleet update authorization expired")
	}
	if status, err := ReadStatus(dir); err == nil && status.Active(now) {
		return ErrBusy
	}
	req.RequestedAt = now.UTC()
	if err := WriteStatus(dir, Status{State: StateQueued, JobID: req.JobID, Target: req.Version, UpdatedAt: now.UTC()}); err != nil {
		return err
	}
	if err := writeJSON(dir, RequestFile, req, 0600); err != nil {
		_ = WriteStatus(dir, Status{State: StateFailed, JobID: req.JobID, Target: req.Version, UpdatedAt: now.UTC(), Message: "could not queue fleet update"})
		return err
	}
	return nil
}

// CompleteFleetRollback records verified local restoration independently of
// the expired authorization to activate a release. The root caller must have
// completed InstallationTransaction.Recover or validated its durable
// rolled-back journal before invoking it.
func CompleteFleetRollback(dir string, req Request) (bool, error) {
	if !validFleetRequest(req) {
		return false, errors.New("invalid fleet rollback intent")
	}
	if result, err := FleetResult(dir, req.JobID); err == nil {
		if result.Target != req.Version || result.State != StateFailed {
			return false, InstallationRollbackError(errors.New("fleet rollback result conflicts with recovery"), errors.New("terminal result requires operator review"))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return finishFleet(dir, req, StateFailed, InstallationRollbackError(errors.New("activation was interrupted"), nil))
}

func finishFleet(dir string, req Request, state string, failure error) (bool, error) {
	message := ""
	if failure != nil {
		message = "fleet update failed; inspect the root worker log"
	}
	result := Status{State: state, JobID: req.JobID, Target: req.Version, Message: message, UpdatedAt: time.Now().UTC()}
	if errors.Is(failure, ErrInstallationRestored) {
		result.Rollback = "restored"
	}
	if err := publishFleetResult(dir, result); err != nil {
		return false, err
	}
	if err := WriteStatus(dir, result); err != nil {
		return false, err
	}
	if err := os.Remove(filepath.Join(dir, RequestFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return state == StateSucceeded, failure
}

// RunFleet retains requests during activation and interruption. The root
// caller supplies signed installation and installed-version/service health
// checks; caller-controlled paths and executable arguments are never accepted.
func RunFleet(ctx context.Context, dir string, req Request, installedVersion string, install func(context.Context, string) error, health func(context.Context, string) error) (bool, error) {
	if !validFleetRequest(req) || install == nil || health == nil {
		return false, errors.New("fleet worker configuration or intent is invalid")
	}
	if result, err := FleetResult(dir, req.JobID); err == nil {
		if result.Target != req.Version {
			return false, errors.New("fleet job release mismatch")
		}
		if result.State == StateSucceeded {
			if err := health(ctx, req.Version); err != nil {
				result.State = StateFailed
				result.Message = "previous fleet completion failed its current health check"
				result.UpdatedAt = time.Now().UTC()
				if publishErr := publishFleetResult(dir, result); publishErr != nil {
					return false, publishErr
				}
				if statusErr := WriteStatus(dir, result); statusErr != nil {
					return false, statusErr
				}
				return false, err
			}
		}
		_ = os.Remove(filepath.Join(dir, RequestFile))
		return result.State == StateSucceeded, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	finish := func(state string, failure error) (bool, error) {
		return finishFleet(dir, req, state, failure)
	}

	if !req.Deadline.After(time.Now()) {
		return finish(StateFailed, errors.New("fleet update authorization expired"))
	}
	if err := WriteStatus(dir, Status{State: StateRunning, JobID: req.JobID, Target: req.Version, UpdatedAt: time.Now().UTC()}); err != nil {
		return false, err
	}
	current := strings.TrimPrefix(strings.TrimSpace(installedVersion), "v")
	compare := -1
	if updater.ValidRelease(current) {
		var err error
		compare, err = updater.CompareReleases(current, req.Version)
		if err != nil {
			return finish(StateFailed, err)
		}
	}
	if compare > 0 {
		return finish(StateFailed, errors.New("fleet update downgrade refused"))
	}
	deadline := req.Deadline
	if max := time.Now().Add(25 * time.Minute); deadline.After(max) {
		deadline = max
	}
	workCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if compare < 0 {
		if err := install(workCtx, req.Version); err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			if errors.Is(err, ErrRollbackIncomplete) {
				return false, err
			}
			return finish(StateFailed, err)
		}
	}
	if err := health(workCtx, req.Version); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if errors.Is(err, ErrRollbackIncomplete) {
			return false, err
		}
		return finish(StateFailed, err)
	}
	return finish(StateSucceeded, nil)
}

// ExecuteLocal submits a root worker intent and waits for correlated completion
// and the freshly started server version. Queueing alone is never success.
func ExecuteLocal(ctx context.Context, dir, installedVersion string, execution updater.Execution, deadline time.Time) error {
	req := Request{JobID: execution.JobID, Version: execution.Release, Deadline: deadline}
	if err := SubmitFleet(dir, req, time.Now().UTC()); err != nil {
		if errors.Is(err, ErrBusy) {
			return updater.ErrExecutionPending
		}
		return err
	}
	result, err := FleetResult(dir, req.JobID)
	if errors.Is(err, os.ErrNotExist) {
		return updater.ErrExecutionPending
	}
	if err != nil {
		return err
	}
	if result.State != StateSucceeded {
		return errors.New("local core update failed; inspect the root worker log")
	}
	if result.Target != execution.Release {
		return errors.New("local core update result mismatch")
	}
	if strings.TrimPrefix(installedVersion, "v") != execution.Release {
		return updater.ErrExecutionPending
	}
	return nil
}

// CoreActionHandler only queues typed intent to the installed root worker.
// Its wait runs asynchronously in transport so samples continue during updates.
func CoreActionHandler(dir string, identity contracts.ServerID, installedVersion string, fallback func(context.Context, contracts.ActionRequest) contracts.ActionResponse) func(context.Context, contracts.ActionRequest) contracts.ActionResponse {
	return func(ctx context.Context, request contracts.ActionRequest) contracts.ActionResponse {
		if request.Action != "core.update" {
			if fallback != nil {
				return fallback(ctx, request)
			}
			return contracts.ActionResponse{RequestID: request.RequestID, Error: &contracts.Error{Code: "action_unsupported", Message: "node action is not configured"}}
		}
		failure := func(code string) contracts.ActionResponse {
			return contracts.ActionResponse{RequestID: request.RequestID, Error: &contracts.Error{Code: code, Message: "core update did not complete; inspect the node root worker"}}
		}
		if request.TargetServerID != identity || request.Protocol != contracts.HelperProtocol || request.Target != "payesh-core" || request.Validate(time.Now().UTC()) != nil {
			return failure("invalid_core_update")
		}
		var intent contracts.CoreUpdateIntent
		decoder := json.NewDecoder(strings.NewReader(string(request.Arguments)))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&intent) != nil {
			return failure("invalid_core_update")
		}
		var trailing any
		if decoder.Decode(&trailing) != io.EOF {
			return failure("invalid_core_update")
		}
		req := Request{JobID: intent.JobID, Version: intent.Release, Deadline: request.Deadline}
		if err := SubmitFleet(dir, req, time.Now().UTC()); err != nil {
			return failure("core_update_queue_failed")
		}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			result, err := FleetResult(dir, req.JobID)
			if err == nil {
				if result.Target != req.Version || result.State != StateSucceeded {
					return failure("core_update_failed")
				}
				if strings.TrimPrefix(installedVersion, "v") == req.Version {
					return contracts.ActionResponse{RequestID: request.RequestID, Accepted: true, CoreUpdate: &contracts.CoreUpdateResult{JobID: req.JobID, Release: req.Version, State: StateSucceeded}}
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return failure("core_update_status_failed")
			}
			select {
			case <-ctx.Done():
				return failure("core_update_interrupted")
			case <-ticker.C:
			}
		}
	}
}
