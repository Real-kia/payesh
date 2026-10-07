package updater

// This file contains orchestration only. Downloading, staging, activation,
// migration, and recovery remain the independently testable primitives in
// this package. In particular, a nil Executor never means "remote update
// succeeded"; it produces an explicit durable executor_unavailable result.

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

const (
	UpdateJobKind     = "update"
	UpdateNodeJobKind = "update-node"
	maxUpdateTargets  = 20
	// DefaultWorkerInterval bounds recovery latency without creating a busy
	// loop when a release executor is unavailable.
	DefaultWorkerInterval = 5 * time.Second
)

var (
	ErrUpdateNotFound       = errors.New("updater: update job not found")
	ErrUpdateExpired        = errors.New("updater: update authorization expired")
	ErrUpdateConflict       = errors.New("updater: update conflicts with another durable job")
	ErrUpdateExecutorAbsent = errors.New("updater: no update executor is configured")
)

// Executor performs one already-authorized target update. Implementations
// should make Execute idempotent for the supplied JobID: a process can stop
// after activation but before its durable completion CAS, and the scheduler
// will safely retry that child after restart. The scheduler does not invoke
// SSH, shell, or any remote activation by itself.
type Executor interface {
	Execute(context.Context, Execution) error
}

// ExecutorFunc adapts a function to Executor.
type ExecutorFunc func(context.Context, Execution) error

func (f ExecutorFunc) Execute(ctx context.Context, execution Execution) error {
	return f(ctx, execution)
}

// Execution is the narrow, injectable handoff to a local updater or an
// authenticated node transport implementation.
type Execution struct {
	JobID    string
	ParentID string
	Release  string
	Target   contracts.Server
}

// Target describes one owner-selected machine. Compatible and Reason are
// supplied by the release preflight. The scheduler also enforces connection
// state itself immediately before execution, so stale preflight results do
// not silently turn into an attempted offline update.
type Target struct {
	Server     contracts.Server
	Compatible bool
	Reason     string
}

// Plan is an explicit owner action. Notifications and scheduled checks do not
// call Schedule; only a plan submitted by the owner creates update jobs.
type Plan struct {
	Release        string
	IdempotencyKey string
	Targets        []Target
	ExpiresAt      time.Time
}

// Scheduler persists one parent update job and one child per selected target.
// Parent and child transitions are CAS-protected in monitoring.Store, making
// concurrent workers and process restarts converge without an in-memory lock.
type Scheduler struct {
	Store    *monitoring.Store
	Executor Executor
	Now      func() time.Time
	// IDGenerator is injectable for tests and must return an error when
	// entropy is unavailable. The default generator never falls back to a
	// predictable timestamp-based identity.
	IDGenerator func(prefix string) (string, error)
}

// NewScheduler is a convenience constructor for services that wire the
// executor at startup. The executor may be nil; nil is deliberately an
// honest unavailable result as documented on Scheduler.
func NewScheduler(store *monitoring.Store, executor Executor) *Scheduler {
	return &Scheduler{Store: store, Executor: executor}
}

// ScheduleUpdate is a descriptive alias for Schedule for API/service callers.
func (s *Scheduler) ScheduleUpdate(ctx context.Context, plan Plan) (contracts.Job, error) {
	return s.Schedule(ctx, plan)
}

// Resume is a descriptive alias for RunOnce, intended for process-start
// recovery loops that scan durable update parents before accepting new work.
func (s *Scheduler) Resume(ctx context.Context, parentID string) (contracts.Job, error) {
	return s.RunOnce(ctx, parentID)
}

// ProcessPending advances each non-terminal update parent once. It is a
// bounded, restart-safe worker pass; jobs remain authoritative in SQLite.
func (s *Scheduler) ProcessPending(ctx context.Context) error {
	if s == nil || s.Store == nil {
		return errors.New("updater: scheduler store is required")
	}
	jobs, err := s.Store.ListJobsByKind(ctx, UpdateJobKind)
	if err != nil {
		return err
	}
	var firstErr error
	for _, job := range jobs {
		if terminal(job.State) {
			continue
		}
		if _, runErr := s.RunOnce(ctx, job.ID); runErr != nil && !errors.Is(runErr, ErrUpdateConflict) {
			// Continue processing other parents. A single offline/conflicting
			// target must not starve independent durable work.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if firstErr == nil {
				firstErr = runErr
			}
		}
	}
	return firstErr
}

// StartWorker starts one serialized durable worker and returns a completion
// channel that closes after ctx cancellation. The callback receives bounded
// diagnostic errors and must not mutate job state concurrently.
func (s *Scheduler) StartWorker(ctx context.Context, interval time.Duration, onError func(error)) <-chan struct{} {
	done := make(chan struct{})
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 || interval > time.Hour {
		interval = DefaultWorkerInterval
	}
	go func() {
		defer close(done)
		run := func() {
			if err := s.ProcessPending(ctx); err != nil && ctx.Err() == nil && onError != nil {
				onError(err)
			}
		}
		run()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
	return done
}

// Schedule creates an idempotent parent job and durable child jobs. Children
// are inserted hub-first and then in stable server-ID order. Ineligible,
// offline, and incompatible targets are terminal child jobs with explicit
// error codes, not omitted machines and not fake successes.
func (s *Scheduler) Schedule(ctx context.Context, plan Plan) (contracts.Job, error) {
	if s == nil || s.Store == nil {
		return contracts.Job{}, errors.New("updater: scheduler store is required")
	}
	now := s.now()
	if err := validatePlan(plan, now); err != nil {
		return contracts.Job{}, err
	}
	plan.Targets = orderedTargets(plan.Targets)
	hash, err := planHash(plan)
	if err != nil {
		return contracts.Job{}, err
	}
	parent, storedHash, found, err := s.Store.GetJobByOperation(ctx, UpdateJobKind, "", plan.IdempotencyKey)
	if err != nil {
		return contracts.Job{}, err
	}
	if found {
		if storedHash != hash {
			return contracts.Job{}, monitoring.ErrJobIdempotencyConflict
		}
		if err := s.ensureChildren(ctx, parent, plan); err != nil {
			return contracts.Job{}, err
		}
		return parent, nil
	}
	parentID, err := s.newID("update")
	if err != nil {
		return contracts.Job{}, fmt.Errorf("create update job identity: %w", err)
	}
	parent = contracts.Job{ID: parentID, Kind: UpdateJobKind, State: contracts.JobQueued, IdempotencyKey: plan.IdempotencyKey, ExpiresAt: plan.ExpiresAt}
	parent, _, err = s.Store.CreateJob(ctx, parent, hash, now)
	if err != nil {
		if raced, racedHash, racedFound, lookupErr := s.Store.GetJobByOperation(ctx, UpdateJobKind, "", plan.IdempotencyKey); lookupErr == nil && racedFound {
			if racedHash != hash {
				return contracts.Job{}, monitoring.ErrJobIdempotencyConflict
			}
			if ensureErr := s.ensureChildren(ctx, raced, plan); ensureErr != nil {
				return contracts.Job{}, ensureErr
			}
			return raced, nil
		}
		return contracts.Job{}, err
	}
	if err := s.ensureChildren(ctx, parent, plan); err != nil {
		return contracts.Job{}, err
	}
	return parent, nil
}

func (s *Scheduler) ensureChildren(ctx context.Context, parent contracts.Job, plan Plan) error {
	now := s.now()
	for index, target := range plan.Targets {
		childID, err := s.newID(fmt.Sprintf("update-node-%02d", index))
		if err != nil {
			return fmt.Errorf("create update target %s identity: %w", target.Server.ID, err)
		}
		child := contracts.Job{ID: childID, Kind: UpdateNodeJobKind, State: contracts.JobQueued, IdempotencyKey: childKey(parent.ID, target.Server.ID), TargetServerID: target.Server.ID, ExpiresAt: plan.ExpiresAt}
		arguments, _ := json.Marshal(struct {
			Parent  string `json:"parent_job_id"`
			Release string `json:"release"`
		}{Parent: parent.ID, Release: plan.Release})
		// Keeping the release and parent identity in the private action payload
		// makes a resumed child self-describing without adding a new shared
		// database column. Action is never emitted by the browser job API.
		child.Action = &contracts.ActionRequest{Protocol: contracts.HelperProtocol, RequestID: child.ID, Action: "update", TargetServerID: target.Server.ID, IdempotencyKey: child.IdempotencyKey, Target: "payesh-agent", Arguments: arguments, Deadline: plan.ExpiresAt}
		if reason, code := targetSkipReason(target, now); reason != "" {
			child.State = contracts.JobFailed
			child.Progress = 100
			child.Error = &contracts.Error{Code: code, Message: reason, Retryable: code == "offline"}
		}
		childHash := childRequestHash(parent.ID, plan.Release, target, index)
		if _, _, err := s.Store.CreateJob(ctx, child, childHash, now); err != nil {
			// A replay may race another worker inserting this exact child. The
			// operation key and request hash make that insert idempotent.
			if existing, storedHash, found, lookupErr := s.Store.GetJobByOperation(ctx, UpdateNodeJobKind, target.Server.ID, child.IdempotencyKey); lookupErr == nil && found && storedHash == childHash {
				_ = existing
				continue
			}
			return fmt.Errorf("create update target %s: %w", target.Server.ID, err)
		}
	}
	return nil
}

// RunOnce advances one parent by at most one target. Callers may invoke this
// from a durable worker loop. A target in running state is retried after a
// restart; the Executor contract requires idempotent activation by JobID.
func (s *Scheduler) RunOnce(ctx context.Context, parentID string) (contracts.Job, error) {
	if s == nil || s.Store == nil || parentID == "" {
		return contracts.Job{}, errors.New("updater: scheduler and parent job are required")
	}
	parent, found, err := s.Store.GetJob(ctx, parentID)
	if err != nil {
		return contracts.Job{}, err
	}
	if !found || parent.Kind != UpdateJobKind {
		return contracts.Job{}, ErrUpdateNotFound
	}
	children, err := s.children(ctx, parent.ID)
	if err != nil {
		return contracts.Job{}, err
	}
	if parent.State == contracts.JobCancelled || parent.State == contracts.JobCancelling {
		for _, child := range children {
			if err := s.cancelCoreAction(ctx, child); err != nil {
				return parent, err
			}
			if terminal(child.State) {
				continue
			}
			if child.State == contracts.JobQueued {
				_, _ = s.Store.TransitionJob(ctx, child.ID, child.Revision, contracts.JobCancelled, 100, nil, s.now())
			} else if child.State == contracts.JobRunning {
				_, _ = s.Store.RequestJobCancellation(ctx, child.ID, "scheduler-cancel-"+parent.ID, child.Revision, s.now())
			} else if child.State == contracts.JobCancelling {
				_, _ = s.Store.TransitionJob(ctx, child.ID, child.Revision, contracts.JobCancelled, 100, nil, s.now())
			}
		}
		return s.finishParent(ctx, parent, children, true)
	}
	if terminal(parent.State) {
		return parent, nil
	}
	if !parent.ExpiresAt.After(s.now()) {
		for _, child := range children {
			if err := s.cancelCoreAction(ctx, child); err != nil {
				return parent, err
			}
		}
		failed, transitionErr := s.Store.TransitionJob(ctx, parent.ID, parent.Revision, contracts.JobFailed, 100, updateError("expired", ErrUpdateExpired.Error(), false), s.now())
		if transitionErr != nil {
			return contracts.Job{}, transitionErr
		}
		return failed, ErrUpdateExpired
	}

	// Claiming the parent prevents two workers from attempting children at the
	// same time. A parent already running is resumed, not reset.
	if parent.State == contracts.JobQueued {
		parent, err = s.transition(ctx, parent, contracts.JobRunning, parent.Progress, nil)
		if err != nil {
			return contracts.Job{}, err
		}
	}
	children, gated, err := s.enforceHubGate(ctx, children)
	if err != nil {
		return contracts.Job{}, err
	}
	if gated {
		return s.finishParent(ctx, parent, children, false)
	}
	for _, child := range children {
		if terminal(child.State) {
			continue
		}
		if child.State == contracts.JobCancelling || child.CancelRequested {
			if err := s.cancelCoreAction(ctx, child); err != nil {
				return parent, err
			}
			_, _ = s.Store.TransitionJob(ctx, child.ID, child.Revision, contracts.JobCancelled, 100, nil, s.now())
			continue
		}
		if child.State != contracts.JobQueued && child.State != contracts.JobRunning {
			continue
		}
		server, found, getErr := s.Store.GetServer(ctx, child.TargetServerID)
		if getErr != nil {
			return contracts.Job{}, getErr
		}
		if !found {
			return s.failChildAndFinish(ctx, parent, children, child, "incompatible", "selected server no longer exists", false)
		}
		if reason, code := currentSkipReason(server, s.now()); reason != "" && !(child.State == contracts.JobRunning && code == "offline" && server.ConnectionState != "revoked") {
			return s.failChildAndFinish(ctx, parent, children, child, code, reason, code == "offline")
		}
		active, queryErr := s.Store.ListActiveJobsForServer(ctx, server.ID, child.ID)
		if queryErr != nil {
			return contracts.Job{}, queryErr
		}
		conflict := false
		for _, job := range active {
			// The transport action owned by this child is part of the update,
			// not an independent operation that should block its reconciliation.
			if job.ID == CoreUpdateActionID(child.ID) && job.Kind == "core-update-action" && job.Action != nil && job.Action.Action == "core.update" {
				continue
			}
			conflict = true
		}
		if conflict {
			// Leave the child queued. This is durable serialization: the next
			// worker pass can run it once the conflicting operation completes.
			return parent, ErrUpdateConflict
		}
		claimed, claimErr := s.Store.TransitionJob(ctx, child.ID, child.Revision, contracts.JobRunning, child.Progress, nil, s.now())
		if claimErr != nil {
			return contracts.Job{}, claimErr
		}
		if err := ctx.Err(); err != nil {
			// Preserve running ownership on interruption. A subsequent worker
			// pass retries this idempotent child instead of recording a false
			// failure caused only by the worker shutting down.
			return claimed, err
		}
		if s.Executor == nil {
			return s.completeChild(ctx, parent, children, claimed, updateError("executor_unavailable", ErrUpdateExecutorAbsent.Error(), true))
		}
		execErr := s.Executor.Execute(ctx, Execution{JobID: claimed.ID, ParentID: parent.ID, Release: releaseForChild(claimed), Target: server})
		if errors.Is(execErr, ErrExecutionPending) {
			return parent, nil
		}
		if errors.Is(execErr, context.Canceled) || errors.Is(execErr, context.DeadlineExceeded) {
			return claimed, execErr
		}
		if execErr != nil {
			code, retryable := "update_failed", false
			message := "update execution failed"
			var offline *OfflineError
			var incompatible *IncompatibleError
			var transient *RetryableError
			switch {
			case errors.As(execErr, &offline):
				code, retryable, message = "offline", true, offline.Error()
			case errors.As(execErr, &incompatible):
				code, message = "incompatible", incompatible.Error()
			case errors.As(execErr, &transient):
				code, retryable = "update_retryable", true
				message = execErr.Error()
			default:
				message = execErr.Error()
			}
			return s.completeChild(ctx, parent, children, claimed, updateError(code, message, retryable))
		}
		return s.completeChild(ctx, parent, children, claimed, nil)
	}
	return s.finishParent(ctx, parent, children, false)
}

// enforceHubGate makes the hub a rollout prerequisite, not merely the first
// item in a queue. If a selected hub reaches a terminal non-success state,
// every later target is durably blocked and no node executor can run.
func (s *Scheduler) enforceHubGate(ctx context.Context, children []contracts.Job) ([]contracts.Job, bool, error) {
	for index, child := range children {
		server, found, err := s.Store.GetServer(ctx, child.TargetServerID)
		if err != nil {
			return nil, false, err
		}
		if !found || server.Role != "hub" || child.State == contracts.JobSucceeded || !terminal(child.State) {
			continue
		}
		code, message := "hub_update_failed", "hub update failed; remaining targets were blocked"
		if child.State == contracts.JobCancelled {
			code, message = "hub_update_blocked", "hub update was cancelled; remaining targets were blocked"
		}
		for next := index + 1; next < len(children); next++ {
			if terminal(children[next].State) {
				continue
			}
			blocked, transitionErr := s.Store.TransitionJob(ctx, children[next].ID, children[next].Revision, contracts.JobFailed, 100, updateError(code, message, false), s.now())
			if transitionErr != nil {
				return nil, false, transitionErr
			}
			children[next] = blocked
		}
		return children, true, nil
	}
	return children, false, nil
}

// Run drains one parent in sequence. Context cancellation leaves a running
// child durable and a later invocation can safely resume it.
func (s *Scheduler) Run(ctx context.Context, parentID string) (contracts.Job, error) {
	for {
		job, err := s.RunOnce(ctx, parentID)
		if err != nil && !errors.Is(err, ErrUpdateConflict) {
			return job, err
		}
		if terminal(job.State) {
			return job, nil
		}
		if errors.Is(err, ErrUpdateConflict) {
			return job, err
		}
	}
}

// OfflineError and IncompatibleError let an injected executor preserve
// explicit result semantics without coupling it to transport implementations.
type OfflineError struct{ Reason string }

func (e *OfflineError) Error() string {
	if e == nil || e.Reason == "" {
		return "target is offline"
	}
	return e.Reason
}

type IncompatibleError struct {
	Reason string
	Err    error
}

func (e *IncompatibleError) Error() string {
	if e == nil || e.Reason == "" {
		return "target is incompatible"
	}
	return e.Reason
}
func (e *IncompatibleError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// RetryableError preserves transient source/transport failures for a worker
// policy. The scheduler records the failure while allowing the durable child
// to be retried by a later pass; permanent trust and compatibility failures
// remain ordinary terminal errors.
type RetryableError struct{ Err error }

func (e *RetryableError) Error() string {
	if e == nil || e.Err == nil {
		return "retryable update failure"
	}
	return e.Err.Error()
}
func (e *RetryableError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (s *Scheduler) children(ctx context.Context, parentID string) ([]contracts.Job, error) {
	all, err := s.Store.ListJobsByKind(ctx, UpdateNodeJobKind)
	if err != nil {
		return nil, err
	}
	prefix := parentID + ":"
	children := make([]contracts.Job, 0, maxUpdateTargets)
	for _, job := range all {
		if strings.HasPrefix(job.IdempotencyKey, prefix) {
			children = append(children, job)
		}
	}
	// The sequence prefix is part of the durable child identity, so ordering is
	// retained even when a restart or concurrent producer changes timestamps.
	sort.SliceStable(children, func(i, j int) bool { return children[i].ID < children[j].ID })
	return children, nil
}

func (s *Scheduler) completeChild(ctx context.Context, parent contracts.Job, children []contracts.Job, child contracts.Job, jobErr *contracts.Error) (contracts.Job, error) {
	state, progress := contracts.JobSucceeded, uint8(100)
	if jobErr != nil {
		state = contracts.JobFailed
	}
	completed, err := s.Store.TransitionJob(ctx, child.ID, child.Revision, state, progress, jobErr, s.now())
	if err != nil {
		return contracts.Job{}, err
	}
	for i := range children {
		if children[i].ID == completed.ID {
			children[i] = completed
		}
	}
	return s.finishParent(ctx, parent, children, false)
}

func (s *Scheduler) failChildAndFinish(ctx context.Context, parent contracts.Job, children []contracts.Job, child contracts.Job, code, message string, retryable bool) (contracts.Job, error) {
	if err := s.cancelCoreAction(ctx, child); err != nil {
		return parent, err
	}
	return s.completeChild(ctx, parent, children, child, updateError(code, message, retryable))
}

func (s *Scheduler) finishParent(ctx context.Context, parent contracts.Job, children []contracts.Job, cancelling bool) (contracts.Job, error) {
	if len(children) == 0 {
		failed, err := s.transition(ctx, parent, contracts.JobFailed, 100, updateError("empty_selection", "no update targets were selected", false))
		return failed, err
	}
	done, successes, failures := 0, 0, 0
	for _, child := range children {
		if terminal(child.State) {
			done++
		}
		if child.State == contracts.JobSucceeded {
			successes++
		}
		if child.State == contracts.JobFailed {
			failures++
		}
	}
	progress := uint8(done * 100 / len(children))
	if done < len(children) {
		if progress > 99 {
			progress = 99
		}
		updated, err := s.transition(ctx, parent, parent.State, progress, nil)
		return updated, err
	}
	if cancelling {
		updated, err := s.transition(ctx, parent, contracts.JobCancelled, 100, nil)
		return updated, err
	}
	if failures > 0 || successes != len(children) {
		error := updateError("partial_failure", fmt.Sprintf("%d of %d selected targets updated", successes, len(children)), false)
		updated, err := s.transition(ctx, parent, contracts.JobFailed, 100, error)
		return updated, err
	}
	updated, err := s.transition(ctx, parent, contracts.JobSucceeded, 100, nil)
	return updated, err
}

func (s *Scheduler) transition(ctx context.Context, job contracts.Job, state contracts.JobState, progress uint8, jobErr *contracts.Error) (contracts.Job, error) {
	if terminal(job.State) && job.State == state {
		return job, nil
	}
	return s.Store.TransitionJob(ctx, job.ID, job.Revision, state, progress, jobErr, s.now())
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func validatePlan(plan Plan, now time.Time) error {
	if !ValidRelease(plan.Release) || plan.IdempotencyKey == "" || len(plan.IdempotencyKey) > 128 || len(plan.Targets) == 0 || len(plan.Targets) > maxUpdateTargets || now.IsZero() || !plan.ExpiresAt.After(now) || plan.ExpiresAt.Sub(now) > 30*24*time.Hour {
		return errors.New("updater: invalid update plan")
	}
	seen := map[contracts.ServerID]bool{}
	for _, target := range plan.Targets {
		if target.Server.ID == "" || seen[target.Server.ID] {
			return errors.New("updater: update targets must be unique")
		}
		seen[target.Server.ID] = true
	}
	return nil
}

func orderedTargets(targets []Target) []Target {
	copyTargets := append([]Target(nil), targets...)
	sort.SliceStable(copyTargets, func(i, j int) bool {
		hi := copyTargets[i].Server.Role == "hub"
		hj := copyTargets[j].Server.Role == "hub"
		if hi != hj {
			return hi
		}
		return copyTargets[i].Server.ID < copyTargets[j].Server.ID
	})
	return copyTargets
}

func targetSkipReason(target Target, now time.Time) (string, string) {
	if !target.Compatible {
		if target.Reason != "" {
			return target.Reason, "incompatible"
		}
		return "target failed compatibility preflight", "incompatible"
	}
	return currentSkipReason(target.Server, now)
}
func currentSkipReason(server contracts.Server, now time.Time) (string, string) {
	if server.ConnectionState == "disconnected" || server.ConnectionState == "never-connected" || server.ConnectionState == "revoked" {
		return "target is offline", "offline"
	}
	if server.Role != "hub" && server.Role != "node" {
		return "server role cannot receive a fleet update", "incompatible"
	}
	if now.IsZero() {
		return "scheduler clock is invalid", "incompatible"
	}
	return "", ""
}

func terminal(state contracts.JobState) bool {
	return state == contracts.JobSucceeded || state == contracts.JobFailed || state == contracts.JobCancelled || state == contracts.JobRecoveryRequired
}
func updateError(code, message string, retryable bool) *contracts.Error {
	return &contracts.Error{Code: code, Message: message, Retryable: retryable}
}
func planHash(plan Plan) (string, error) {
	// Server records contain heartbeat/freshness fields that legitimately
	// change between an HTTP retry. Idempotency is for the owner-selected
	// operation, not a transient snapshot of those fields.
	type stableTarget struct {
		ServerID   contracts.ServerID `json:"server_id"`
		Compatible bool               `json:"compatible"`
		Reason     string             `json:"reason,omitempty"`
	}
	targets := make([]stableTarget, 0, len(plan.Targets))
	for _, target := range orderedTargets(plan.Targets) {
		targets = append(targets, stableTarget{ServerID: target.Server.ID, Compatible: target.Compatible, Reason: target.Reason})
	}
	body, err := json.Marshal(struct {
		Release        string         `json:"release"`
		IdempotencyKey string         `json:"idempotency_key"`
		Targets        []stableTarget `json:"targets"`
		ExpiresAt      time.Time      `json:"expires_at"`
	}{plan.Release, plan.IdempotencyKey, targets, plan.ExpiresAt})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}
func childRequestHash(parent, release string, target Target, index int) string {
	body, _ := json.Marshal(struct {
		Parent, Release string
		Target          contracts.ServerID
		Index           int
	}{parent, release, target.Server.ID, index})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
func childKey(parent string, target contracts.ServerID) string { return parent + ":" + string(target) }
func releaseForChild(job contracts.Job) string {
	if job.Action != nil && len(job.Action.Arguments) > 0 {
		var payload struct {
			Release string `json:"release"`
		}
		if json.Unmarshal(job.Action.Arguments, &payload) == nil {
			return payload.Release
		}
	}
	return ""
}
func newUpdateJobID(prefix string) string {
	var raw [10]byte
	if _, err := cryptorand.Read(raw[:]); err != nil {
		return ""
	}
	return prefix + "-" + hex.EncodeToString(raw[:])
}

func (s *Scheduler) newID(prefix string) (string, error) {
	if s.IDGenerator != nil {
		id, err := s.IDGenerator(prefix)
		if err != nil {
			return "", err
		}
		if id == "" || len(id) > 128 {
			return "", errors.New("generated job identity is invalid")
		}
		return id, nil
	}
	id := newUpdateJobID(prefix)
	if id == "" {
		return "", errors.New("secure job identity generation failed")
	}
	return id, nil
}

// ValidRelease is the scheduler-facing release identity check shared with
// manifest validation. It intentionally accepts only semantic versions.
func ValidRelease(version string) bool { return semverPattern.MatchString(version) }

// Cancelling an orchestration job must also prevent its queued transport intent
// from being leased. Already delivered activation is reconciled by its worker;
// cancellation does not claim to undo a running installation.
func (s *Scheduler) cancelCoreAction(ctx context.Context, child contracts.Job) error {
	id := CoreUpdateActionID(child.ID)
	for attempt := 0; attempt < 3; attempt++ {
		action, found, err := s.Store.GetJob(ctx, id)
		if err != nil {
			return err
		}
		if !found || terminal(action.State) || action.CancelRequested {
			return nil
		}
		if action.Kind != "core-update-action" || action.TargetServerID != child.TargetServerID {
			return errors.New("update action identity mismatch")
		}
		_, err = s.Store.RequestJobCancellation(ctx, id, "rollout-cancel-"+child.ID, action.Revision, s.now())
		if errors.Is(err, monitoring.ErrJobRevisionConflict) {
			continue
		}
		return err
	}
	return monitoring.ErrJobRevisionConflict
}
