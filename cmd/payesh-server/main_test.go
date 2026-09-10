package main

import (
	"context"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/alerts"
	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func TestLoopbackListenAddress(t *testing.T) {
	for _, address := range []string{"127.0.0.1:8787", "[::1]:8787", "localhost:8787"} {
		if !loopbackListenAddress(address) {
			t.Fatalf("loopback address rejected: %s", address)
		}
	}
	for _, address := range []string{"0.0.0.0:8787", ":8787", "192.0.2.10:8787", "[::]:8787"} {
		if loopbackListenAddress(address) {
			t.Fatalf("non-loopback address accepted: %s", address)
		}
	}
}

func TestEvaluateUnreachableCreatesLivenessAlert(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-liveness-012345")
	at := time.Now().UTC().Add(time.Second)
	lastHeartbeat := at.Add(-2 * time.Minute)
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Liveness", Role: "node", Architecture: "amd64", Platform: "linux", LastHeartbeat: &lastHeartbeat, ConnectionState: "connected", FreshnessState: "fresh"}); err != nil {
		t.Fatal(err)
	}
	engine, err := alerts.NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RefreshServerStates(ctx, at); err != nil {
		t.Fatal(err)
	}
	if err := evaluateUnreachable(ctx, store, engine, at); err != nil {
		t.Fatal(err)
	}
	if err := evaluateUnreachable(ctx, store, engine, at.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	page, err := store.ListAlertStates(ctx, serverID, 200, "")
	if err != nil {
		t.Fatal(err)
	}
	firing := false
	for _, state := range page.Items {
		if state.State == "firing" {
			firing = true
			break
		}
	}
	if !firing {
		t.Fatalf("liveness evaluation did not fire an alert: %+v", page.Items)
	}
}
