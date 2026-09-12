package bandwidth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

const RollbackTimeout = 2 * time.Minute

var (
	ErrForeignNetworkState = errors.New("foreign_network_state")
	ErrVerificationFailed  = errors.New("bandwidth_verification_failed")
)

type Ownership struct {
	Compatible bool   `json:"compatible"`
	Owner      string `json:"owner,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type Checkpoint struct {
	ID        string          `json:"id"`
	Scope     Scope           `json:"scope"`
	Action    Action          `json:"action"`
	Previous  json.RawMessage `json:"previous"`
	ExpiresAt time.Time       `json:"expires_at"`
}

type Kernel interface {
	Inspect(context.Context, Preview) (Ownership, json.RawMessage, error)
	Apply(context.Context, Preview) error
	Verify(context.Context, Preview) error
	RevertOwned(context.Context, Checkpoint) error
}

// Guard must persist the checkpoint and arrange rollback independently of
// the hub connection. Confirm may remove it only after verification and a
// management round trip both succeed.
type Guard interface {
	Arm(context.Context, Checkpoint) error
	Confirm(context.Context, string) error
}

type Manager struct {
	Kernel              Kernel
	Guard               Guard
	ManagementRoundTrip func(context.Context) error
	Store               *monitoring.Store
	LocalServerID       contracts.ServerID
	ManagementDiscovery func(context.Context, Scope) ([]ManagementFlow, error)
	Now                 func() time.Time
	mu                  sync.Mutex
}

func (m *Manager) Apply(ctx context.Context, checkpointID string, request Request, management []ManagementFlow) (Preview, error) {
	return m.executeApply(ctx, checkpointID, request, management, func(Checkpoint, Preview) error { return nil })
}

func (m *Manager) executeApply(ctx context.Context, checkpointID string, request Request, management []ManagementFlow, commit func(Checkpoint, Preview) error) (Preview, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Kernel == nil || m.Guard == nil || m.ManagementRoundTrip == nil {
		return Preview{}, errors.New("bandwidth safety adapters are not configured")
	}
	if !safeName.MatchString(checkpointID) {
		return Preview{}, errors.New("invalid checkpoint id")
	}
	preview, err := BuildPreview(request, management)
	if err != nil {
		return Preview{}, err
	}
	if !preview.AffectsNetwork {
		return preview, nil
	}
	ownership, previous, err := m.Kernel.Inspect(ctx, preview)
	if err != nil {
		return Preview{}, err
	}
	if !ownership.Compatible {
		return Preview{}, fmt.Errorf("%w: %s", ErrForeignNetworkState, ownership.Reason)
	}
	now := time.Now().UTC()
	if m.Now != nil {
		now = m.Now().UTC()
	}
	checkpoint := Checkpoint{ID: checkpointID, Scope: request.Scope, Action: request.Action, Previous: append(json.RawMessage(nil), previous...), ExpiresAt: now.Add(RollbackTimeout)}
	if err := m.Guard.Arm(ctx, checkpoint); err != nil {
		return Preview{}, fmt.Errorf("arm rollback before apply: %w", err)
	}
	rollback := func(cause error) (Preview, error) {
		if revertErr := m.Kernel.RevertOwned(ctx, checkpoint); revertErr != nil {
			return Preview{}, fmt.Errorf("%w; owned rollback also failed: %v", cause, revertErr)
		}
		if confirmErr := m.Guard.Confirm(ctx, checkpoint.ID); confirmErr != nil {
			return Preview{}, fmt.Errorf("%w; rollback succeeded but checkpoint cleanup failed: %v", cause, confirmErr)
		}
		return Preview{}, cause
	}
	if err := m.Kernel.Apply(ctx, preview); err != nil {
		return rollback(err)
	}
	if err := m.Kernel.Verify(ctx, preview); err != nil {
		return rollback(fmt.Errorf("%w: %v", ErrVerificationFailed, err))
	}
	if err := m.ManagementRoundTrip(ctx); err != nil {
		return rollback(fmt.Errorf("management confirmation failed: %w", err))
	}
	if err := commit(checkpoint, preview); err != nil {
		return rollback(fmt.Errorf("persist applied policy: %w", err))
	}
	if err := m.Guard.Confirm(ctx, checkpoint.ID); err != nil {
		// The policy is effective but uncommitted. Leave the guard armed so its
		// independent timeout reverts instead of claiming durable success.
		return Preview{}, fmt.Errorf("confirm rollback guard: %w", err)
	}
	return preview, nil
}

type PolicyParameters struct {
	Request          Request          `json:"request"`
	Checkpoint       Checkpoint       `json:"checkpoint"`
	Exclusions       []ManagementFlow `json:"management_exclusions,omitempty"`
	EnforcementState string           `json:"enforcement_state"`
	PeriodStart      time.Time        `json:"period_start,omitempty"`
	CountedBytes     uint64           `json:"counted_bytes,string,omitempty"`
}

// ApplyPolicy binds the kernel safety transaction to the shared durable
// ControlPolicy CAS. The rollback guard is confirmed only after this commit.
func (m *Manager) PreviewPolicy(ctx context.Context, serverID contracts.ServerID, request Request) (Preview, error) {
	if err := m.ensureLocalEnabled(ctx, serverID); err != nil {
		return Preview{}, err
	}
	management, err := m.discoverManagement(ctx, request.Scope)
	if err != nil {
		return Preview{}, err
	}
	return BuildPreview(request, management)
}

func (m *Manager) ApplyPolicy(ctx context.Context, serverID contracts.ServerID, checkpointID string, request Request, expectedRevision uint64) (contracts.ControlPolicy, error) {
	if err := m.ensureLocalEnabled(ctx, serverID); err != nil {
		return contracts.ControlPolicy{}, err
	}
	management, err := m.discoverManagement(ctx, request.Scope)
	if err != nil {
		return contracts.ControlPolicy{}, err
	}
	targetName := request.Scope.TargetName()
	if request.Action == WarnOnly || request.QuotaBytes > 0 {
		m.mu.Lock()
		defer m.mu.Unlock()
		current, err := m.Store.GetControlPolicy(ctx, serverID, ModuleID, request.Scope.TargetKind, targetName)
		if err != nil {
			return contracts.ControlPolicy{}, err
		}
		if current.Revision != expectedRevision {
			return contracts.ControlPolicy{}, monitoring.ErrControlPolicyRevisionConflict
		}
		preview, err := BuildPreview(request, management)
		if err != nil {
			return contracts.ControlPolicy{}, err
		}
		parameters, err := json.Marshal(PolicyParameters{Request: request, Exclusions: preview.Exclusions, EnforcementState: "waiting"})
		if err != nil {
			return contracts.ControlPolicy{}, err
		}
		return m.Store.TransitionControlPolicy(ctx, serverID, ModuleID, request.Scope.TargetKind, targetName, expectedRevision, contracts.ControlPolicy{Kind: PolicyKind, State: contracts.ControlPolicyApplied, Parameters: parameters})
	}
	current, err := m.Store.GetControlPolicy(ctx, serverID, ModuleID, request.Scope.TargetKind, targetName)
	if err != nil {
		return contracts.ControlPolicy{}, err
	}
	if current.Revision != expectedRevision {
		return contracts.ControlPolicy{}, monitoring.ErrControlPolicyRevisionConflict
	}
	var persisted contracts.ControlPolicy
	_, err = m.executeApply(ctx, checkpointID, request, management, func(checkpoint Checkpoint, preview Preview) error {
		parameters, marshalErr := json.Marshal(PolicyParameters{Request: request, Checkpoint: checkpoint, Exclusions: preview.Exclusions, EnforcementState: "active"})
		if marshalErr != nil {
			return marshalErr
		}
		persisted, marshalErr = m.Store.TransitionControlPolicy(ctx, serverID, ModuleID, request.Scope.TargetKind, targetName, expectedRevision, contracts.ControlPolicy{Kind: PolicyKind, State: contracts.ControlPolicyApplied, Parameters: parameters})
		return marshalErr
	})
	if err != nil {
		return contracts.ControlPolicy{}, err
	}
	return persisted, nil
}

func (m *Manager) discoverManagement(ctx context.Context, scope Scope) ([]ManagementFlow, error) {
	if m.ManagementDiscovery == nil {
		return nil, errors.New("trusted local management-flow discovery is not configured")
	}
	flows, err := m.ManagementDiscovery(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("discover management flows: %w", err)
	}
	return flows, nil
}

func (m *Manager) RevertPolicy(ctx context.Context, serverID contracts.ServerID, scope Scope, expectedRevision uint64) (contracts.ControlPolicy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Kernel == nil || m.Store == nil {
		return contracts.ControlPolicy{}, errors.New("bandwidth adapters are not configured")
	}
	if err := scope.Validate(); err != nil {
		return contracts.ControlPolicy{}, err
	}
	current, err := m.Store.GetControlPolicy(ctx, serverID, ModuleID, scope.TargetKind, scope.TargetName())
	if err != nil {
		return contracts.ControlPolicy{}, err
	}
	if current.Revision != expectedRevision {
		return contracts.ControlPolicy{}, monitoring.ErrControlPolicyRevisionConflict
	}
	if current.State != contracts.ControlPolicyApplied && current.State != contracts.ControlPolicyFailed {
		return contracts.ControlPolicy{}, errors.New("bandwidth policy is not applied")
	}
	var parameters PolicyParameters
	if err := json.Unmarshal(current.Parameters, &parameters); err != nil || parameters.Checkpoint.ID == "" {
		if err == nil && (parameters.Request.Action == WarnOnly || parameters.EnforcementState != "active") {
			return m.Store.TransitionControlPolicy(ctx, serverID, ModuleID, scope.TargetKind, scope.TargetName(), current.Revision, contracts.ControlPolicy{Kind: PolicyKind, State: contracts.ControlPolicyReverted, Parameters: current.Parameters})
		}
		return contracts.ControlPolicy{}, errors.New("stored bandwidth recovery checkpoint is invalid")
	}
	if err := m.Kernel.RevertOwned(ctx, parameters.Checkpoint); err != nil {
		_, _ = m.Store.TransitionControlPolicy(ctx, serverID, ModuleID, scope.TargetKind, scope.TargetName(), current.Revision, contracts.ControlPolicy{Kind: PolicyKind, State: contracts.ControlPolicyFailed, Parameters: current.Parameters, Error: &contracts.Error{Code: "revert_failed", Message: err.Error()}})
		return contracts.ControlPolicy{}, err
	}
	return m.Store.TransitionControlPolicy(ctx, serverID, ModuleID, scope.TargetKind, scope.TargetName(), current.Revision, contracts.ControlPolicy{Kind: PolicyKind, State: contracts.ControlPolicyReverted, Parameters: current.Parameters})
}

// ActivateDuePolicy changes a configured quota policy from waiting to active.
func (m *Manager) ActivateDuePolicy(ctx context.Context, policy contracts.ControlPolicy, parameters PolicyParameters, period contracts.TrafficPeriod) (contracts.ControlPolicy, error) {
	if parameters.EnforcementState != "waiting" || parameters.Request.QuotaBytes == 0 || period.CountedBytes < parameters.Request.QuotaBytes {
		return policy, nil
	}
	parameters.PeriodStart, parameters.CountedBytes = period.From, period.CountedBytes
	if parameters.Request.Action == WarnOnly {
		parameters.EnforcementState = "warned"
		encoded, err := json.Marshal(parameters)
		if err != nil {
			return contracts.ControlPolicy{}, err
		}
		return m.Store.TransitionControlPolicy(ctx, policy.ServerID, ModuleID, policy.TargetKind, policy.TargetName, policy.Revision, contracts.ControlPolicy{Kind: PolicyKind, State: contracts.ControlPolicyApplied, Parameters: encoded})
	}
	checkpointID := "bw-quota-" + strconv.FormatUint(policy.Revision+1, 10) + "-" + strings.ReplaceAll(policy.TargetName, ":", "-")
	if len(checkpointID) > 128 {
		checkpointID = checkpointID[:128]
	}
	var persisted contracts.ControlPolicy
	_, err := m.executeApply(ctx, checkpointID, parameters.Request, parameters.Exclusions, func(checkpoint Checkpoint, _ Preview) error {
		parameters.Checkpoint, parameters.EnforcementState = checkpoint, "active"
		encoded, marshalErr := json.Marshal(parameters)
		if marshalErr != nil {
			return marshalErr
		}
		persisted, marshalErr = m.Store.TransitionControlPolicy(ctx, policy.ServerID, ModuleID, policy.TargetKind, policy.TargetName, policy.Revision, contracts.ControlPolicy{Kind: PolicyKind, State: contracts.ControlPolicyApplied, Parameters: encoded})
		return marshalErr
	})
	return persisted, err
}

func (m *Manager) ResetForNewPeriod(ctx context.Context, policy contracts.ControlPolicy, parameters PolicyParameters, period contracts.TrafficPeriod) (contracts.ControlPolicy, error) {
	if parameters.EnforcementState == "active" {
		if err := m.Kernel.RevertOwned(ctx, parameters.Checkpoint); err != nil {
			return contracts.ControlPolicy{}, err
		}
	}
	parameters.Checkpoint = Checkpoint{}
	parameters.EnforcementState = "waiting"
	parameters.PeriodStart = period.From
	parameters.CountedBytes = period.CountedBytes
	encoded, err := json.Marshal(parameters)
	if err != nil {
		return contracts.ControlPolicy{}, err
	}
	return m.Store.TransitionControlPolicy(ctx, policy.ServerID, ModuleID, policy.TargetKind, policy.TargetName, policy.Revision, contracts.ControlPolicy{Kind: PolicyKind, State: contracts.ControlPolicyApplied, Parameters: encoded})
}

func (m *Manager) MarkAccountingGap(ctx context.Context, policy contracts.ControlPolicy, parameters PolicyParameters, period contracts.TrafficPeriod) (contracts.ControlPolicy, error) {
	if parameters.EnforcementState != "waiting" {
		return policy, nil
	}
	parameters.EnforcementState, parameters.PeriodStart, parameters.CountedBytes = "waiting-gap", period.From, period.CountedBytes
	encoded, err := json.Marshal(parameters)
	if err != nil {
		return contracts.ControlPolicy{}, err
	}
	return m.Store.TransitionControlPolicy(ctx, policy.ServerID, ModuleID, policy.TargetKind, policy.TargetName, policy.Revision, contracts.ControlPolicy{Kind: PolicyKind, State: contracts.ControlPolicyFailed, Parameters: encoded, Error: &contracts.Error{Code: "traffic_accounting_gap", Message: "traffic continuity is incomplete; automatic quota enforcement is paused"}})
}

func (m *Manager) ensureLocalEnabled(ctx context.Context, serverID contracts.ServerID) error {
	if m.Store == nil {
		return errors.New("bandwidth store is not configured")
	}
	server, found, err := m.Store.GetServer(ctx, serverID)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("bandwidth server not found")
	}
	if server.Role != "standalone" || m.LocalServerID == "" || serverID != m.LocalServerID {
		return errors.New("bandwidth remote execution requires the authenticated node helper")
	}
	installation, err := m.Store.GetModuleInstallation(ctx, serverID, ModuleID)
	if err != nil {
		return err
	}
	if installation.State != contracts.ModuleEnabled {
		return fmt.Errorf("bandwidth module must be enabled (current state %s)", installation.State)
	}
	return nil
}

// RevertAllForServer is the module-disable cleanup hook. Any cleanup failure
// stops the sweep so the recovery executable is not removed prematurely.
func (m *Manager) RevertAllForServer(ctx context.Context, serverID contracts.ServerID) error {
	policies, err := m.Store.ListControlPolicies(ctx, serverID, ModuleID)
	if err != nil {
		return err
	}
	for _, policy := range policies {
		if policy.State != contracts.ControlPolicyApplied && policy.State != contracts.ControlPolicyFailed {
			continue
		}
		var parameters PolicyParameters
		if err := json.Unmarshal(policy.Parameters, &parameters); err != nil {
			return fmt.Errorf("decode bandwidth policy %s: %w", policy.ID, err)
		}
		if _, err := m.RevertPolicy(ctx, serverID, parameters.Request.Scope, policy.Revision); err != nil {
			return fmt.Errorf("revert bandwidth policy %s: %w", policy.ID, err)
		}
	}
	return nil
}
