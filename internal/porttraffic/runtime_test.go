package porttraffic

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

type runtimeBackend struct {
	applyCalls  int
	removeCalls int
	scopes      []Scope
	generation  string
	counters    []Counter
	err         error
}

func (b *runtimeBackend) Apply(_ context.Context, scopes []Scope, generation string) error {
	b.applyCalls++
	b.scopes = append([]Scope(nil), scopes...)
	b.generation = generation
	return b.err
}
func (b *runtimeBackend) Remove(context.Context) error { b.removeCalls++; return b.err }
func (b *runtimeBackend) Snapshot(context.Context) ([]Counter, error) {
	return append([]Counter(nil), b.counters...), b.err
}

func TestRuntimeReconcilesPendingPersistsCountersAndTearsDown(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-port-runtime-01")
	if err := store.UpsertServer(ctx, contracts.Server{ID: serverID, Name: "local", Role: "standalone", Platform: "linux", Architecture: "amd64", ConnectionState: "connected", FreshnessState: "fresh"}); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Store: store}
	scope := Scope{ID: "web", Protocol: TCP, Interface: "eth0", LocalPort: 443, Direction: Inbound, Tuple: TranslatedTuple}
	if _, err := manager.Configure(ctx, serverID, scope, 0); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	backend := &runtimeBackend{counters: []Counter{{ScopeID: "web", Bytes: 10, Packets: 1, Generation: "g1", ObservedAt: t0}}}
	runtime, err := NewRuntime(RuntimeOptions{Store: store, ServerID: serverID, Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Tick(ctx, t0); err != nil {
		t.Fatal(err)
	}
	if backend.applyCalls != 1 || len(backend.scopes) != 1 {
		t.Fatalf("apply calls=%d scopes=%+v", backend.applyCalls, backend.scopes)
	}
	policy, err := store.GetControlPolicy(ctx, serverID, ModuleID, "local-port", "web")
	if err != nil || policy.State != contracts.ControlPolicyApplied {
		t.Fatalf("policy=%+v err=%v", policy, err)
	}
	observations, err := store.ListPortTrafficObservations(ctx, serverID)
	if err != nil || len(observations) != 1 || observations[0].Bytes != 10 || observations[0].Continuity != "uncertain" {
		t.Fatalf("observations=%+v err=%v", observations, err)
	}
	backend.counters[0].Bytes = 25
	backend.counters[0].Packets = 2
	backend.counters[0].ObservedAt = t0.Add(time.Second)
	if err := runtime.Tick(ctx, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if backend.applyCalls != 1 {
		t.Fatalf("unchanged desired scopes reapplied: %d", backend.applyCalls)
	}
	observations, err = store.ListPortTrafficObservations(ctx, serverID)
	if err != nil || observations[0].Continuity != "complete" || observations[0].Bytes != 25 {
		t.Fatalf("updated observations=%+v err=%v", observations, err)
	}
	policy, err = store.GetControlPolicy(ctx, serverID, ModuleID, "local-port", "web")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Revert(ctx, serverID, "web", policy.Revision); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Tick(ctx, t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if backend.removeCalls != 1 {
		t.Fatalf("owned table was not removed: %d", backend.removeCalls)
	}
}

func TestRuntimeLeavesPendingWhenNftApplyFails(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-port-runtime-02")
	if err := store.UpsertServer(ctx, contracts.Server{ID: serverID, Name: "local", Role: "standalone", Platform: "linux", Architecture: "amd64", ConnectionState: "connected", FreshnessState: "fresh"}); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Store: store}
	if _, err := manager.Configure(ctx, serverID, Scope{ID: "web", Protocol: TCP, Interface: "eth0", LocalPort: 443, Direction: Inbound, Tuple: TranslatedTuple}, 0); err != nil {
		t.Fatal(err)
	}
	backend := &runtimeBackend{err: errors.New("nft unavailable")}
	runtime, err := NewRuntime(RuntimeOptions{Store: store, ServerID: serverID, Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Tick(ctx, time.Now()); err == nil {
		t.Fatal("failed nft apply was accepted")
	}
	policy, err := store.GetControlPolicy(ctx, serverID, ModuleID, "local-port", "web")
	if err != nil || policy.State != contracts.ControlPolicyPending {
		t.Fatalf("failed apply changed policy=%+v err=%v", policy, err)
	}
}
