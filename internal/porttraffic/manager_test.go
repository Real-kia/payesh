package porttraffic

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func portTrafficManager(t *testing.T) (*Manager, contracts.ServerID) {
	t.Helper()
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	serverID := contracts.ServerID("server-port-traffic-01")
	if err := store.UpsertServer(ctx, contracts.Server{ID: serverID, Name: "local", Role: "standalone", Platform: "linux", Architecture: "amd64", ConnectionState: "connected", FreshnessState: "fresh"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionModuleInstallation(ctx, serverID, ModuleID, 0, contracts.ModuleInstallation{Version: "1.0.0", State: contracts.ModuleEnabled}); err != nil {
		t.Fatal(err)
	}
	return &Manager{Store: store}, serverID
}

func TestPortTrafficConfigureUsesPendingCASAndRejectsDuplicateCounter(t *testing.T) {
	manager, serverID := portTrafficManager(t)
	ctx := context.Background()
	scope := Scope{ID: "web", Protocol: TCP, Interface: "eth0", LocalPort: 443, Direction: Inbound, Tuple: TranslatedTuple}
	policy, err := manager.Configure(ctx, serverID, scope, 0)
	if err != nil {
		t.Fatal(err)
	}
	if policy.State != contracts.ControlPolicyPending || policy.Revision != 1 {
		t.Fatalf("policy=%+v", policy)
	}
	var parameters ScopeParameters
	if err := json.Unmarshal(policy.Parameters, &parameters); err != nil || parameters.Version != 1 || !sameScope(parameters.Scope, scope) {
		t.Fatalf("parameters=%s err=%v", policy.Parameters, err)
	}
	if _, err := manager.Configure(ctx, serverID, scope, 0); !errors.Is(err, monitoring.ErrControlPolicyRevisionConflict) {
		t.Fatalf("stale revision error=%v", err)
	}
	duplicate := scope
	duplicate.ID = "web-copy"
	if _, err := manager.Configure(ctx, serverID, duplicate, 0); err == nil {
		t.Fatal("duplicate counter configuration was accepted")
	}
	reverted, err := manager.Revert(ctx, serverID, scope.ID, 1)
	if err != nil || reverted.State != contracts.ControlPolicyReverted || reverted.Revision != 2 {
		t.Fatalf("reverted=%+v err=%v", reverted, err)
	}
}
