package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

const MaxRolloutTargets = 1000

var (
	ErrUpdateNotPaused    = errors.New("updater: update job is not paused")
	ErrUpdateWindowClosed = errors.New("the fleet update window closed before all targets started")
)

// RolloutOptions are opt-in controls for a single fleet rollout.
type RolloutOptions struct {
	Canary         int           `json:"canary,omitempty"`
	MaxFailPct     *int          `json:"max_fail_pct,omitempty"`
	SkipUpToDate   bool          `json:"skip_up_to_date,omitempty"`
	UpdateWindow   *UpdateWindow `json:"update_window,omitempty"`
	StaggerSeconds int           `json:"stagger_seconds,omitempty"`
}

type UpdateWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type rolloutPolicy struct {
	RolloutOptions
	WindowStart      string `json:"window_start,omitempty"`
	WindowEnd        string `json:"window_end,omitempty"`
	AcknowledgedWave int    `json:"acknowledged_wave,omitempty"`
}

func (p rolloutPolicy) windowEnd() time.Time {
	if p.UpdateWindow != nil {
		return p.UpdateWindow.End
	}
	return time.Time{}
}

// Parent jobs have no delivery target. Their private payload keeps the
// rollout policy alongside the job revision without a schema migration.
func (p rolloutPolicy) store(job *contracts.Job) {
	args, _ := json.Marshal(p)
	job.Action = &contracts.ActionRequest{Protocol: contracts.HelperProtocol, RequestID: job.ID, Action: "update.rollout", TargetServerID: contracts.ServerID(job.ID), IdempotencyKey: job.IdempotencyKey, Target: "fleet", Arguments: args, Deadline: job.ExpiresAt}
}

func policyFor(job contracts.Job) (rolloutPolicy, error) {
	var policy rolloutPolicy
	if job.Action == nil {
		return policy, nil
	}
	err := json.Unmarshal(job.Action.Arguments, &policy)
	return policy, err
}

func (o RolloutOptions) valid(now, expires time.Time, count int) bool {
	if o.Canary < 0 || o.Canary > count || o.StaggerSeconds < 0 || o.StaggerSeconds > 86400 {
		return false
	}
	if o.MaxFailPct != nil && (*o.MaxFailPct < 0 || *o.MaxFailPct > 100) {
		return false
	}
	w := o.UpdateWindow
	return w == nil || (!w.Start.IsZero() && w.Start.Before(w.End) && w.End.After(now) && w.Start.Before(expires))
}

func alreadyOnRelease(server contracts.Server, release string) bool {
	return (server.Role == "node" || server.Role == "hub") &&
		strings.TrimPrefix(server.Version, "v") == strings.TrimPrefix(release, "v")
}

// failedWave only considers completed waves before the next pending target.
// Acknowledging a wave lets the worker proceed without retrying failed children.
func (p rolloutPolicy) failedWave(children []contracts.Job) (int, int, int) {
	if p.MaxFailPct == nil {
		return 0, 0, 0
	}
	pending := len(children)
	for i, child := range children {
		if !terminal(child.State) {
			pending = i
			break
		}
	}
	if pending == len(children) {
		return 0, 0, 0
	}
	for wave, start := 1, 0; start < pending; wave++ {
		size := maxUpdateTargets
		if wave == 1 && p.Canary > 0 {
			size = p.Canary
		}
		end := start + size
		if end > pending {
			break
		}
		if wave > p.AcknowledgedWave {
			failed, total := 0, 0
			for _, child := range children[start:end] {
				if child.State == contracts.JobSkipped {
					continue
				}
				total++
				if child.State != contracts.JobSucceeded {
					failed++
				}
			}
			if failed*100 > *p.MaxFailPct*total {
				return wave, failed, total
			}
		}
		start = end
	}
	return 0, 0, 0
}

func (s *Scheduler) pauseIfNeeded(ctx context.Context, parent contracts.Job, children []contracts.Job, progress uint8) (contracts.Job, bool, error) {
	policy, err := policyFor(parent)
	if err != nil {
		return parent, false, err
	}
	wave, failed, total := policy.failedWave(children)
	if wave == 0 {
		return parent, false, nil
	}
	policy.AcknowledgedWave = wave
	policy.store(&parent)
	reason := updateError("wave_failure_threshold", fmt.Sprintf("Wave %d had %d failures across %d targets. Resume to continue with the remaining targets.", wave, failed, total), false)
	job, err := s.Store.TransitionRolloutJob(ctx, parent, contracts.JobPaused, progress, reason, s.now())
	return job, true, err
}

func (s *Scheduler) ResumePaused(ctx context.Context, id string, revision uint64) (contracts.Job, error) {
	job, found, err := s.Store.GetJob(ctx, id)
	if err != nil {
		return job, err
	}
	if !found || job.Kind != UpdateJobKind {
		return job, ErrUpdateNotFound
	}
	if job.Revision != revision {
		return job, monitoring.ErrJobRevisionConflict
	}
	now := s.now()
	if !UpdateJobResumable(job, now) {
		return job, ErrUpdateNotPaused
	}
	return s.Store.TransitionRolloutJob(ctx, job, contracts.JobRunning, job.Progress, nil, now)
}

// UpdateJobResumable reports whether the job can still advance before its deadline.
func UpdateJobResumable(job contracts.Job, now time.Time) bool {
	if job.Kind != UpdateJobKind || job.State != contracts.JobPaused || job.Action == nil || !job.ExpiresAt.After(now) {
		return false
	}
	policy, err := policyFor(job)
	return err == nil && (policy.UpdateWindow == nil || now.Before(policy.UpdateWindow.End))
}

func rolloutProgress(children []contracts.Job) uint8 {
	if len(children) == 0 {
		return 0
	}
	done := 0
	for _, child := range children {
		if terminal(child.State) {
			done++
		}
	}
	return uint8(done * 100 / len(children))
}
