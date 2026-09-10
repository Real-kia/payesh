package monitoring

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestGetControlPolicyDefaultsToPending(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	policy, err := store.GetControlPolicy(ctx, server.ID, "cpu-controls", "service", "nginx.service")
	if err != nil {
		t.Fatal(err)
	}
	if policy.State != contracts.ControlPolicyPending || policy.Revision != 0 {
		t.Fatalf("expected pending/0, got %+v", policy)
	}
}

func TestTransitionControlPolicyCreatesThenAdvances(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(map[string]any{"millicores": "1000"})
	applied, err := store.TransitionControlPolicy(ctx, server.ID, "cpu-controls", "service", "nginx.service", 0, contracts.ControlPolicy{Kind: "cpu-quota", State: contracts.ControlPolicyApplied, Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	if applied.Revision != 1 || applied.State != contracts.ControlPolicyApplied {
		t.Fatalf("unexpected first transition: %+v", applied)
	}
	reverted, err := store.TransitionControlPolicy(ctx, server.ID, "cpu-controls", "service", "nginx.service", 1, contracts.ControlPolicy{Kind: "cpu-quota", State: contracts.ControlPolicyReverted, Parameters: params})
	if err != nil {
		t.Fatal(err)
	}
	if reverted.Revision != 2 || reverted.State != contracts.ControlPolicyReverted {
		t.Fatalf("unexpected second transition: %+v", reverted)
	}
	fetched, err := store.GetControlPolicy(ctx, server.ID, "cpu-controls", "service", "nginx.service")
	if err != nil {
		t.Fatal(err)
	}
	if fetched.Revision != 2 || fetched.State != contracts.ControlPolicyReverted {
		t.Fatalf("unexpected persisted state: %+v", fetched)
	}
}

func TestTransitionControlPolicyRejectsStaleRevision(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionControlPolicy(ctx, server.ID, "cpu-controls", "service", "nginx.service", 0, contracts.ControlPolicy{Kind: "cpu-quota", State: contracts.ControlPolicyApplied}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionControlPolicy(ctx, server.ID, "cpu-controls", "service", "nginx.service", 0, contracts.ControlPolicy{Kind: "cpu-quota", State: contracts.ControlPolicyApplied}); err != ErrControlPolicyRevisionConflict {
		t.Fatalf("expected ErrControlPolicyRevisionConflict, got %v", err)
	}
}

func TestListControlPoliciesFiltersByModule(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionControlPolicy(ctx, server.ID, "cpu-controls", "service", "nginx.service", 0, contracts.ControlPolicy{Kind: "cpu-quota", State: contracts.ControlPolicyApplied}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionControlPolicy(ctx, server.ID, "bandwidth-controls", "service", "nginx.service", 0, contracts.ControlPolicy{Kind: "bandwidth-cap", State: contracts.ControlPolicyApplied}); err != nil {
		t.Fatal(err)
	}
	cpuOnly, err := store.ListControlPolicies(ctx, server.ID, "cpu-controls")
	if err != nil {
		t.Fatal(err)
	}
	if len(cpuOnly) != 1 || cpuOnly[0].ModuleID != "cpu-controls" {
		t.Fatalf("unexpected filtered list: %+v", cpuOnly)
	}
	all, err := store.ListControlPolicies(ctx, server.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 policies total, got %d", len(all))
	}
}

func TestControlPolicyRequestIdempotencyRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveControlPolicyRequest(ctx, server.ID, "cpu-controls", "service", "nginx.service", "key-1", "hash-1", []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	record, found, err := store.GetControlPolicyRequest(ctx, server.ID, "cpu-controls", "service", "nginx.service", "key-1")
	if err != nil || !found {
		t.Fatalf("expected stored record, found=%v err=%v", found, err)
	}
	if record.RequestHash != "hash-1" {
		t.Fatalf("unexpected record: %+v", record)
	}
}
