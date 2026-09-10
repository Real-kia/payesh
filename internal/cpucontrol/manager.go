package cpucontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

// QuotaParameters is the ControlPolicy.Parameters payload for PolicyKind.
// PreviousMillicores/PreviousUnlimited capture the state Apply found before
// writing a new quota, so Revert can restore exactly that (not a guessed
// default), matching "preserve and restore the previous quota rather than
// resetting all resource settings."
type QuotaParameters struct {
	GroupPath          string `json:"group_path"`
	Millicores         uint64 `json:"millicores,string"`
	Unlimited          bool   `json:"unlimited"`
	PreviousMillicores uint64 `json:"previous_millicores,string"`
	PreviousUnlimited  bool   `json:"previous_unlimited"`
	ProcessStartTicks  uint64 `json:"process_start_ticks,string,omitempty"`
}

// Preview is the read-only "show existing state and expected effect" step
// PLAN.md section 10 requires before any Apply.
type Preview struct {
	CurrentMillicores  uint64 `json:"current_millicores"`
	CurrentUnlimited   bool   `json:"current_unlimited"`
	ProposedMillicores uint64 `json:"proposed_millicores"`
	Description        string `json:"description"`
	// ParentStricter is true when an ancestor cgroup already enforces a
	// tighter quota than ProposedMillicores would request — the requested
	// policy would not actually take effect at the level shown.
	ParentStricter   bool   `json:"parent_stricter,omitempty"`
	ParentMillicores uint64 `json:"parent_millicores,omitempty"`
}

// Manager implements the package-08 CPU Controls lifecycle: preview, apply
// (validate → write → verify → persist), and revert. It never moves an
// unrelated process into a group and never writes into a cgroup this process
// did not create (enforced by FSCgroup's ownership marker).
type Manager struct {
	Store    *monitoring.Store
	FS       CgroupFS
	ProcRoot string
	Now      func() time.Time
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now().UTC()
}

func (m *Manager) procRoot() string {
	if m.ProcRoot != "" {
		return m.ProcRoot
	}
	return "/proc"
}

// Preview reports the current effective quota and what applying millicores
// would change, including whether a stricter ancestor quota would make the
// requested value misleading. totalCores is caller-supplied (from whatever
// the caller already knows about the server) purely to phrase Description;
// zero is accepted and simply omits the whole-server percentage.
func (m *Manager) Preview(target Target, millicores uint64, totalCores int) (Preview, error) {
	if err := target.validateKindName(); err != nil {
		return Preview{}, err
	}
	if err := ValidateMillicores(millicores); err != nil {
		return Preview{}, err
	}
	group := target.GroupPath()
	currentMillicores, currentUnlimited, err := m.FS.ReadQuota(group)
	if err != nil {
		return Preview{}, err
	}
	parentMillicores, parentUnlimited, parentFound, err := m.FS.ParentQuota(group)
	if err != nil {
		return Preview{}, err
	}
	preview := Preview{
		CurrentMillicores: currentMillicores, CurrentUnlimited: currentUnlimited,
		ProposedMillicores: millicores, Description: Describe(millicores, totalCores),
	}
	if parentFound && !parentUnlimited && parentMillicores < millicores {
		preview.ParentStricter, preview.ParentMillicores = true, parentMillicores
	}
	return preview, nil
}

// Apply runs validate → require a dedicated group (refusing an unsafe shared
// one) → verify process identity for a process-group target → write the
// quota → read it back to confirm → persist. A verification mismatch is
// recorded as failed, never reported as success.
func (m *Manager) Apply(ctx context.Context, serverID contracts.ServerID, target Target, millicores uint64, expectedRevision uint64) (contracts.ControlPolicy, error) {
	if err := target.validateIdentity(); err != nil {
		return contracts.ControlPolicy{}, err
	}
	if err := ValidateMillicores(millicores); err != nil {
		return contracts.ControlPolicy{}, err
	}
	if target.Kind == TargetKindProcessGroup {
		ok, err := VerifyIdentity(m.procRoot(), *target.Process)
		if err != nil {
			return contracts.ControlPolicy{}, err
		}
		if !ok {
			return contracts.ControlPolicy{}, errors.New("cpucontrol: process identity could not be verified (exited, or its PID was reused)")
		}
	}
	current, err := m.Store.GetControlPolicy(ctx, serverID, ModuleID, target.Kind, target.Name)
	if err != nil {
		return contracts.ControlPolicy{}, err
	}
	if current.Revision != expectedRevision {
		return contracts.ControlPolicy{}, monitoring.ErrControlPolicyRevisionConflict
	}

	group := target.GroupPath()
	previousMillicores, previousUnlimited, err := m.FS.ReadQuota(group)
	if err != nil {
		return contracts.ControlPolicy{}, err
	}
	if err := m.FS.EnsureDedicatedGroup(group); err != nil {
		return m.persistFailure(ctx, serverID, target, current.Revision, err)
	}
	if err := m.FS.WriteQuota(group, millicores, false); err != nil {
		return m.persistFailure(ctx, serverID, target, current.Revision, err)
	}
	readBackMillicores, readBackUnlimited, err := m.FS.ReadQuota(group)
	if err != nil {
		return m.persistFailure(ctx, serverID, target, current.Revision, err)
	}
	if readBackUnlimited || readBackMillicores != millicores {
		return m.persistFailure(ctx, serverID, target, current.Revision, fmt.Errorf("cpucontrol: effective quota %dm did not match the requested %dm after apply", readBackMillicores, millicores))
	}

	var startTicks uint64
	if target.Process != nil {
		startTicks = target.Process.StartTicks
	}
	parameters, err := json.Marshal(QuotaParameters{
		GroupPath: string(group), Millicores: millicores, Unlimited: false,
		PreviousMillicores: previousMillicores, PreviousUnlimited: previousUnlimited,
		ProcessStartTicks: startTicks,
	})
	if err != nil {
		return contracts.ControlPolicy{}, err
	}
	return m.Store.TransitionControlPolicy(ctx, serverID, ModuleID, target.Kind, target.Name, current.Revision, contracts.ControlPolicy{
		Kind: PolicyKind, State: contracts.ControlPolicyApplied, Parameters: parameters,
	})
}

func (m *Manager) persistFailure(ctx context.Context, serverID contracts.ServerID, target Target, expectedRevision uint64, applyErr error) (contracts.ControlPolicy, error) {
	if _, err := m.Store.TransitionControlPolicy(ctx, serverID, ModuleID, target.Kind, target.Name, expectedRevision, contracts.ControlPolicy{
		Kind: PolicyKind, State: contracts.ControlPolicyFailed, Error: &contracts.Error{Code: "apply_failed", Message: applyErr.Error()},
	}); err != nil {
		return contracts.ControlPolicy{}, err
	}
	return contracts.ControlPolicy{}, applyErr
}

// Revert restores the quota Apply found before it ran (or removes the limit
// entirely if there was none), verifies the restoration, and persists
// "reverted". It is only legal against an "applied" policy.
func (m *Manager) Revert(ctx context.Context, serverID contracts.ServerID, target Target, expectedRevision uint64) (contracts.ControlPolicy, error) {
	if err := target.validateKindName(); err != nil {
		return contracts.ControlPolicy{}, err
	}
	current, err := m.Store.GetControlPolicy(ctx, serverID, ModuleID, target.Kind, target.Name)
	if err != nil {
		return contracts.ControlPolicy{}, err
	}
	if current.Revision != expectedRevision {
		return contracts.ControlPolicy{}, monitoring.ErrControlPolicyRevisionConflict
	}
	if current.State != contracts.ControlPolicyApplied {
		return contracts.ControlPolicy{}, fmt.Errorf("cpucontrol: only an applied policy can be reverted, current state is %s", current.State)
	}
	var parameters QuotaParameters
	if err := json.Unmarshal(current.Parameters, &parameters); err != nil {
		return contracts.ControlPolicy{}, fmt.Errorf("cpucontrol: stored policy parameters are invalid: %w", err)
	}
	group := GroupPath(parameters.GroupPath)
	if err := m.FS.WriteQuota(group, parameters.PreviousMillicores, parameters.PreviousUnlimited); err != nil {
		return m.persistFailure(ctx, serverID, target, current.Revision, err)
	}
	readBackMillicores, readBackUnlimited, err := m.FS.ReadQuota(group)
	if err != nil {
		return m.persistFailure(ctx, serverID, target, current.Revision, err)
	}
	if readBackUnlimited != parameters.PreviousUnlimited || (!parameters.PreviousUnlimited && readBackMillicores != parameters.PreviousMillicores) {
		return m.persistFailure(ctx, serverID, target, current.Revision, errors.New("cpucontrol: effective quota did not match the restored value after revert"))
	}
	return m.Store.TransitionControlPolicy(ctx, serverID, ModuleID, target.Kind, target.Name, current.Revision, contracts.ControlPolicy{
		Kind: PolicyKind, State: contracts.ControlPolicyReverted, Parameters: current.Parameters,
	})
}

// RevertAllForServer reverts every currently applied CPU policy on a server.
// It is intended as the CPU Controls module's disable hook (see
// modules.Manager.DeactivateHooks): "disabling ... must first revert its
// active policies and verify cleanup" before the module is allowed to move
// to installed-disabled. A single failure stops the sweep and is returned so
// the module stays enabled rather than reporting a false success.
func (m *Manager) RevertAllForServer(ctx context.Context, serverID contracts.ServerID) error {
	policies, err := m.Store.ListControlPolicies(ctx, serverID, ModuleID)
	if err != nil {
		return err
	}
	for _, policy := range policies {
		if policy.State != contracts.ControlPolicyApplied {
			continue
		}
		if _, err := m.Revert(ctx, serverID, Target{Kind: policy.TargetKind, Name: policy.TargetName}, policy.Revision); err != nil {
			return fmt.Errorf("cpucontrol: revert %s/%s: %w", policy.TargetKind, policy.TargetName, err)
		}
	}
	return nil
}
