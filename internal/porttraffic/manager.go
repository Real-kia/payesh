package porttraffic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

const (
	ModuleID   = "port-traffic"
	PolicyKind = "port-traffic-scope"
)

// ScopeParameters is the durable desired configuration consumed by the
// eventual privileged Port Traffic module. Pending is intentional: storing a
// scope never claims that nftables rules have been installed.
type ScopeParameters struct {
	Version uint  `json:"version"`
	Scope   Scope `json:"scope"`
}

type Manager struct {
	Store *monitoring.Store
	mu    sync.Mutex
}

func (m *Manager) List(ctx context.Context, serverID contracts.ServerID) ([]contracts.ControlPolicy, error) {
	if m == nil || m.Store == nil {
		return nil, errors.New("port traffic store is not configured")
	}
	return m.Store.ListControlPolicies(ctx, serverID, ModuleID)
}

// Configure stores one validated desired scope. The CAS protects concurrent
// browser edits and the duplicate scan prevents two IDs selecting one kernel
// counter. Existing rows in non-pending states are still treated as desired
// configuration; kernel activation is deliberately outside this method.
func (m *Manager) Configure(ctx context.Context, serverID contracts.ServerID, scope Scope, expectedRevision uint64) (contracts.ControlPolicy, error) {
	if m == nil || m.Store == nil {
		return contracts.ControlPolicy{}, errors.New("port traffic store is not configured")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !validPortTrafficServer(serverID) {
		return contracts.ControlPolicy{}, errors.New("invalid server_id")
	}
	if err := scope.Validate(); err != nil {
		return contracts.ControlPolicy{}, err
	}
	current, err := m.Store.GetControlPolicy(ctx, serverID, ModuleID, "local-port", scope.ID)
	if err != nil {
		return contracts.ControlPolicy{}, err
	}
	if current.Revision != expectedRevision {
		return contracts.ControlPolicy{}, monitoring.ErrControlPolicyRevisionConflict
	}
	policies, err := m.Store.ListControlPolicies(ctx, serverID, ModuleID)
	if err != nil {
		return contracts.ControlPolicy{}, err
	}
	for _, policy := range policies {
		if policy.TargetName == scope.ID && policy.TargetKind == "local-port" {
			continue
		}
		if policy.State == contracts.ControlPolicyReverted {
			continue
		}
		var parameters ScopeParameters
		if json.Unmarshal(policy.Parameters, &parameters) != nil {
			return contracts.ControlPolicy{}, fmt.Errorf("decode stored scope %s", policy.ID)
		}
		if parameters.Scope.ID == scope.ID {
			return contracts.ControlPolicy{}, fmt.Errorf("scope %q already exists", scope.ID)
		}
		if sameScope(parameters.Scope, scope) {
			return contracts.ControlPolicy{}, fmt.Errorf("scope %q selects the same counter as %q", scope.ID, parameters.Scope.ID)
		}
	}
	parameters, err := json.Marshal(ScopeParameters{Version: 1, Scope: scope})
	if err != nil {
		return contracts.ControlPolicy{}, err
	}
	previousKey := ""
	if current.Revision > 0 && current.State != contracts.ControlPolicyReverted && current.State != contracts.ControlPolicyFailed {
		var previous ScopeParameters
		if json.Unmarshal(current.Parameters, &previous) == nil {
			previousKey = scopeKey(previous.Scope)
		}
	}
	return m.Store.TransitionControlPolicyWithUniqueKey(ctx, serverID, ModuleID, "local-port", scope.ID, expectedRevision, contracts.ControlPolicy{
		Kind: PolicyKind, State: contracts.ControlPolicyPending, Parameters: parameters,
	}, previousKey, scopeKey(scope))
}

func (m *Manager) Revert(ctx context.Context, serverID contracts.ServerID, scopeID string, expectedRevision uint64) (contracts.ControlPolicy, error) {
	if m == nil || m.Store == nil {
		return contracts.ControlPolicy{}, errors.New("port traffic store is not configured")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !validPortTrafficServer(serverID) || !safeScopeID(scopeID) {
		return contracts.ControlPolicy{}, errors.New("invalid port traffic scope identity")
	}
	current, err := m.Store.GetControlPolicy(ctx, serverID, ModuleID, "local-port", scopeID)
	if err != nil {
		return contracts.ControlPolicy{}, err
	}
	if current.Revision != expectedRevision {
		return contracts.ControlPolicy{}, monitoring.ErrControlPolicyRevisionConflict
	}
	if current.State == contracts.ControlPolicyReverted {
		return contracts.ControlPolicy{}, errors.New("port traffic scope is already reverted")
	}
	var parameters ScopeParameters
	if err := json.Unmarshal(current.Parameters, &parameters); err != nil {
		return contracts.ControlPolicy{}, fmt.Errorf("decode stored scope %s", current.ID)
	}
	return m.Store.TransitionControlPolicyWithUniqueKey(ctx, serverID, ModuleID, "local-port", scopeID, expectedRevision, contracts.ControlPolicy{
		Kind: PolicyKind, State: contracts.ControlPolicyReverted, Parameters: current.Parameters,
	}, scopeKey(parameters.Scope), scopeKey(parameters.Scope))
}

// RevertAllForServer is used by module lifecycle cleanup. It first marks all
// desired scopes reverted through the durable CAS, then the runtime can remove
// the owned nft table independently.
func (m *Manager) RevertAllForServer(ctx context.Context, serverID contracts.ServerID) error {
	if m == nil || m.Store == nil {
		return errors.New("port traffic store is not configured")
	}
	policies, err := m.Store.ListControlPolicies(ctx, serverID, ModuleID)
	if err != nil {
		return err
	}
	for _, policy := range policies {
		if policy.State == contracts.ControlPolicyReverted {
			continue
		}
		var parameters ScopeParameters
		if err := json.Unmarshal(policy.Parameters, &parameters); err != nil {
			return fmt.Errorf("decode stored scope %s: %w", policy.ID, err)
		}
		if _, err := m.Revert(ctx, serverID, policy.TargetName, policy.Revision); err != nil {
			return err
		}
	}
	return nil
}

func sameScope(a, b Scope) bool {
	if a.Path == "" {
		a.Path = LocalPath
	}
	if b.Path == "" {
		b.Path = LocalPath
	}
	return a.Protocol == b.Protocol && a.Interface == b.Interface && a.LocalPort == b.LocalPort && a.Direction == b.Direction && a.Tuple == b.Tuple && a.Path == b.Path
}

func scopeKey(scope Scope) string {
	if scope.Path == "" {
		scope.Path = LocalPath
	}
	return fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%s\x00%s", scope.Protocol, scope.Interface, scope.LocalPort, scope.Direction, scope.Tuple, scope.Path)
}

func validPortTrafficServer(serverID contracts.ServerID) bool {
	return len(serverID) >= 16 && len(serverID) <= 128 && safeScopeID(string(serverID))
}

func safeScopeID(value string) bool { return interfacePattern.MatchString(value) }
