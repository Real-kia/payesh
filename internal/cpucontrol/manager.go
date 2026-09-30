package cpucontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
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
// design requires before any Apply.
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
	// LocalServerID binds direct cgroup work to this installation. Role labels
	// in fleet rows are not sufficient proof that a target is the local host.
	LocalServerID contracts.ServerID
	Now           func() time.Time
	// PrepareService must bind a service to group through a supervisor-aware
	// adapter (for example systemd over the privileged helper). A nil adapter
	// fails closed; creating an empty cgroup is never considered enforcement.
	PrepareService func(ctx context.Context, target Target, group GroupPath) error
	// LifecycleMu is shared with modules.Manager. Apply takes a read lock while
	// module enable/disable/remove take the write lock, preventing a policy from
	// appearing between the disable cleanup sweep and its state transition.
	LifecycleMu *sync.RWMutex
	mu          sync.Mutex
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

func (m *Manager) PreviewForServer(ctx context.Context, serverID contracts.ServerID, target Target, millicores uint64, totalCores int) (Preview, error) {
	if err := m.ensureLocalEnabled(ctx, serverID); err != nil {
		return Preview{}, err
	}
	return m.Preview(target, millicores, totalCores)
}

// Apply runs validate → require a dedicated group (refusing an unsafe shared
// one) → verify process identity for a process-group target → write the
// quota → read it back to confirm → persist. A verification mismatch is
// recorded as failed, never reported as success.
func (m *Manager) Apply(ctx context.Context, serverID contracts.ServerID, target Target, millicores uint64, expectedRevision uint64) (contracts.ControlPolicy, error) {
	if m.LifecycleMu != nil {
		m.LifecycleMu.RLock()
		defer m.LifecycleMu.RUnlock()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ensureLocalEnabled(ctx, serverID); err != nil {
		return contracts.ControlPolicy{}, err
	}
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
	currentMillicores, currentUnlimited, err := m.FS.ReadQuota(group)
	if err != nil {
		return contracts.ControlPolicy{}, err
	}
	previousMillicores, previousUnlimited := currentMillicores, currentUnlimited
	// Updating a Payesh policy retains the original pre-Payesh baseline and
	// first checks that nobody changed the effective value behind our back.
	if current.State == contracts.ControlPolicyApplied {
		var prior QuotaParameters
		if err := json.Unmarshal(current.Parameters, &prior); err != nil {
			return contracts.ControlPolicy{}, fmt.Errorf("cpucontrol: stored policy parameters are invalid: %w", err)
		}
		if currentUnlimited != prior.Unlimited || (!prior.Unlimited && currentMillicores != prior.Millicores) {
			return contracts.ControlPolicy{}, errors.New("cpucontrol: effective quota changed externally; revalidation is required")
		}
		previousMillicores, previousUnlimited = prior.PreviousMillicores, prior.PreviousUnlimited
	}
	if err := m.FS.EnsureDedicatedGroup(group); err != nil {
		return m.persistApplyFailure(ctx, serverID, target, current, err)
	}
	if target.Kind == TargetKindProcessGroup {
		// Existing processes are never moved implicitly. The local `payesh run`
		// or privileged target-preparation path must have created the dedicated
		// group and placed this exact process there before an API policy applies.
		contained, err := m.FS.ContainsProcess(group, target.Process.PID)
		if err != nil || !contained {
			if err == nil {
				err = errors.New("cpucontrol: process is not already present in its dedicated Payesh cgroup")
			}
			return m.persistApplyFailure(ctx, serverID, target, current, err)
		}
		ok, err := VerifyIdentity(m.procRoot(), *target.Process)
		if err != nil || !ok {
			if err == nil {
				err = errors.New("cpucontrol: process identity changed during target verification")
			}
			return m.persistApplyFailure(ctx, serverID, target, current, err)
		}
	} else {
		if m.PrepareService == nil {
			return m.persistApplyFailure(ctx, serverID, target, current, errors.New("cpucontrol: supervisor service adapter is not configured"))
		}
		if err := m.PrepareService(ctx, target, group); err != nil {
			return m.persistApplyFailure(ctx, serverID, target, current, err)
		}
		empty, err := m.FS.IsEmpty(group)
		if err != nil || empty {
			if err == nil {
				err = errors.New("cpucontrol: service cgroup has no member processes")
			}
			return m.persistApplyFailure(ctx, serverID, target, current, err)
		}
	}
	if err := m.FS.WriteQuota(group, millicores, false); err != nil {
		_ = m.FS.WriteQuota(group, currentMillicores, currentUnlimited)
		return m.persistApplyFailure(ctx, serverID, target, current, err)
	}
	readBackMillicores, readBackUnlimited, err := m.FS.ReadQuota(group)
	if err != nil {
		_ = m.FS.WriteQuota(group, currentMillicores, currentUnlimited)
		return m.persistApplyFailure(ctx, serverID, target, current, err)
	}
	if readBackUnlimited || readBackMillicores != millicores {
		_ = m.FS.WriteQuota(group, currentMillicores, currentUnlimited)
		return m.persistApplyFailure(ctx, serverID, target, current, fmt.Errorf("cpucontrol: effective quota %dm did not match the requested %dm after apply", readBackMillicores, millicores))
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
	policy, err := m.Store.TransitionControlPolicy(ctx, serverID, ModuleID, target.Kind, target.Name, current.Revision, contracts.ControlPolicy{
		Kind: PolicyKind, State: contracts.ControlPolicyApplied, Parameters: parameters,
	})
	if err != nil {
		// A durable conflict must not leave an unrecorded kernel side effect.
		_ = m.FS.WriteQuota(group, currentMillicores, currentUnlimited)
		return contracts.ControlPolicy{}, err
	}
	return policy, nil
}

func (m *Manager) ensureLocalEnabled(ctx context.Context, serverID contracts.ServerID) error {
	server, found, err := m.Store.GetServer(ctx, serverID)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("cpucontrol: server not found")
	}
	if server.Role != "standalone" {
		return errors.New("cpucontrol: remote node execution is unavailable until the authenticated node helper is configured")
	}
	if m.LocalServerID == "" || serverID != m.LocalServerID {
		return errors.New("cpucontrol: target is not the configured local installation")
	}
	installation, err := m.Store.GetModuleInstallation(ctx, serverID, ModuleID)
	if err != nil {
		return err
	}
	if installation.State != contracts.ModuleEnabled {
		return fmt.Errorf("cpucontrol: module must be enabled before applying a policy (current state %s)", installation.State)
	}
	return nil
}

func (m *Manager) persistApplyFailure(ctx context.Context, serverID contracts.ServerID, target Target, current contracts.ControlPolicy, applyErr error) (contracts.ControlPolicy, error) {
	// A failed edit must not replace an existing applied record with a failed
	// row and thereby make its still-active baseline impossible to revert.
	if current.State == contracts.ControlPolicyApplied {
		return contracts.ControlPolicy{}, applyErr
	}
	if _, err := m.Store.TransitionControlPolicy(ctx, serverID, ModuleID, target.Kind, target.Name, current.Revision, contracts.ControlPolicy{
		Kind: PolicyKind, State: contracts.ControlPolicyFailed, Error: &contracts.Error{Code: "apply_failed", Message: applyErr.Error()},
	}); err != nil {
		return contracts.ControlPolicy{}, err
	}
	return contracts.ControlPolicy{}, applyErr
}

// Revert restores the quota Apply found before it ran (or removes the limit
// entirely if there was none), verifies the restoration, and persists
// "reverted". A failed policy with retained recovery parameters may also be
// retried after an operator restores/revalidates the expected effective state.
func (m *Manager) Revert(ctx context.Context, serverID contracts.ServerID, target Target, expectedRevision uint64) (contracts.ControlPolicy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.revertLocked(ctx, serverID, target, expectedRevision)
}

func (m *Manager) revertLocked(ctx context.Context, serverID contracts.ServerID, target Target, expectedRevision uint64) (contracts.ControlPolicy, error) {
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
	if current.State != contracts.ControlPolicyApplied && current.State != contracts.ControlPolicyFailed {
		return contracts.ControlPolicy{}, fmt.Errorf("cpucontrol: only an applied or recovery-required failed policy can be reverted, current state is %s", current.State)
	}
	var parameters QuotaParameters
	if err := json.Unmarshal(current.Parameters, &parameters); err != nil {
		return contracts.ControlPolicy{}, fmt.Errorf("cpucontrol: stored policy parameters are invalid: %w", err)
	}
	group := GroupPath(parameters.GroupPath)
	actualMillicores, actualUnlimited, err := m.FS.ReadQuota(group)
	if err != nil {
		return m.persistFailureWithParameters(ctx, serverID, target, current.Revision, current.Parameters, err)
	}
	if actualUnlimited != parameters.Unlimited || (!parameters.Unlimited && actualMillicores != parameters.Millicores) {
		return m.persistFailureWithParameters(ctx, serverID, target, current.Revision, current.Parameters,
			errors.New("cpucontrol: effective quota changed externally; refusing to overwrite administrator configuration"))
	}
	if err := m.FS.WriteQuota(group, parameters.PreviousMillicores, parameters.PreviousUnlimited); err != nil {
		return m.persistFailureWithParameters(ctx, serverID, target, current.Revision, current.Parameters, err)
	}
	readBackMillicores, readBackUnlimited, err := m.FS.ReadQuota(group)
	if err != nil {
		return m.persistFailureWithParameters(ctx, serverID, target, current.Revision, current.Parameters, err)
	}
	if readBackUnlimited != parameters.PreviousUnlimited || (!parameters.PreviousUnlimited && readBackMillicores != parameters.PreviousMillicores) {
		return m.persistFailureWithParameters(ctx, serverID, target, current.Revision, current.Parameters, errors.New("cpucontrol: effective quota did not match the restored value after revert"))
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
	m.mu.Lock()
	defer m.mu.Unlock()
	policies, err := m.Store.ListControlPolicies(ctx, serverID, ModuleID)
	if err != nil {
		return err
	}
	for _, policy := range policies {
		if policy.State != contracts.ControlPolicyApplied && policy.State != contracts.ControlPolicyFailed {
			continue
		}
		if policy.State == contracts.ControlPolicyFailed {
			var parameters QuotaParameters
			if err := json.Unmarshal(policy.Parameters, &parameters); err != nil || parameters.GroupPath == "" {
				continue // a rejected first apply produced no kernel state to clean up
			}
		}
		if _, err := m.revertLocked(ctx, serverID, Target{Kind: policy.TargetKind, Name: policy.TargetName}, policy.Revision); err != nil {
			return fmt.Errorf("cpucontrol: revert %s/%s: %w", policy.TargetKind, policy.TargetName, err)
		}
	}
	return nil
}

func (m *Manager) persistFailureWithParameters(ctx context.Context, serverID contracts.ServerID, target Target, expectedRevision uint64, parameters json.RawMessage, applyErr error) (contracts.ControlPolicy, error) {
	if _, err := m.Store.TransitionControlPolicy(ctx, serverID, ModuleID, target.Kind, target.Name, expectedRevision, contracts.ControlPolicy{
		Kind: PolicyKind, State: contracts.ControlPolicyFailed, Parameters: parameters,
		Error: &contracts.Error{Code: "apply_failed", Message: applyErr.Error()},
	}); err != nil {
		return contracts.ControlPolicy{}, err
	}
	return contracts.ControlPolicy{}, applyErr
}
