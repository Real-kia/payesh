package monitoring

import (
	"context"
	"testing"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestGetModuleInstallationDefaultsToUnavailable(t *testing.T) {
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
	installation, err := store.GetModuleInstallation(ctx, server.ID, "port-traffic")
	if err != nil {
		t.Fatal(err)
	}
	if installation.State != contracts.ModuleUnavailable || installation.Revision != 0 {
		t.Fatalf("expected unavailable/0, got %+v", installation)
	}
}

func TestTransitionModuleInstallationCreatesThenAdvancesRevision(t *testing.T) {
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
	created, err := store.TransitionModuleInstallation(ctx, server.ID, "port-traffic", 0, contracts.ModuleInstallation{Version: "1.0.0", State: contracts.ModuleDownloading})
	if err != nil {
		t.Fatal(err)
	}
	if created.Revision != 1 || created.State != contracts.ModuleDownloading {
		t.Fatalf("unexpected first transition: %+v", created)
	}
	advanced, err := store.TransitionModuleInstallation(ctx, server.ID, "port-traffic", 1, contracts.ModuleInstallation{Version: "1.0.0", State: contracts.ModuleVerifying})
	if err != nil {
		t.Fatal(err)
	}
	if advanced.Revision != 2 || advanced.State != contracts.ModuleVerifying {
		t.Fatalf("unexpected second transition: %+v", advanced)
	}
	fetched, err := store.GetModuleInstallation(ctx, server.ID, "port-traffic")
	if err != nil {
		t.Fatal(err)
	}
	if fetched.Revision != 2 || fetched.State != contracts.ModuleVerifying {
		t.Fatalf("unexpected persisted state: %+v", fetched)
	}
}

func TestTransitionModuleInstallationRejectsStaleAndDuplicateCreateRevision(t *testing.T) {
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
	if _, err := store.TransitionModuleInstallation(ctx, server.ID, "port-traffic", 0, contracts.ModuleInstallation{Version: "1.0.0", State: contracts.ModuleDownloading}); err != nil {
		t.Fatal(err)
	}
	// A second "create at revision 0" must not silently overwrite the row.
	if _, err := store.TransitionModuleInstallation(ctx, server.ID, "port-traffic", 0, contracts.ModuleInstallation{Version: "1.0.0", State: contracts.ModuleDownloading}); err != ErrModuleRevisionConflict {
		t.Fatalf("expected ErrModuleRevisionConflict, got %v", err)
	}
	// A stale expected revision must not apply either.
	if _, err := store.TransitionModuleInstallation(ctx, server.ID, "port-traffic", 99, contracts.ModuleInstallation{Version: "1.0.0", State: contracts.ModuleVerifying}); err != ErrModuleRevisionConflict {
		t.Fatalf("expected ErrModuleRevisionConflict, got %v", err)
	}
}

func TestModuleLifecycleRequestIdempotencyRoundTrip(t *testing.T) {
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
	if _, found, err := store.GetModuleLifecycleRequest(ctx, server.ID, "port-traffic", "key-1"); err != nil || found {
		t.Fatalf("expected no record yet, found=%v err=%v", found, err)
	}
	if err := store.SaveModuleLifecycleRequest(ctx, server.ID, "port-traffic", "key-1", "hash-1", []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	record, found, err := store.GetModuleLifecycleRequest(ctx, server.ID, "port-traffic", "key-1")
	if err != nil || !found {
		t.Fatalf("expected stored record, found=%v err=%v", found, err)
	}
	if record.RequestHash != "hash-1" || record.ResultJSON != `{"ok":true}` {
		t.Fatalf("unexpected record: %+v", record)
	}
	// A repeated key is a caller-level idempotency replay, not a schema
	// uniqueness violation the caller should see: writing it again must fail
	// so the HTTP layer's "check-then-insert" race is detectable.
	if err := store.SaveModuleLifecycleRequest(ctx, server.ID, "port-traffic", "key-1", "hash-1", []byte(`{"ok":true}`)); err == nil {
		t.Fatal("expected duplicate idempotency key insert to fail")
	}
}
