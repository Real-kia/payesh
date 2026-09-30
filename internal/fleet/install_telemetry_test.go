package fleet

import (
	"context"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func TestInstallationRequiresReceivedTelemetry(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	server := contracts.Server{ID: "server-install-telemetry-01", Name: "node", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	svc, _ := NewInstallService(store)
	svc.Now = func() time.Time { return now }
	opts := svc.optionsFor(installRequest{ServerID: string(server.ID), Role: "node"})
	if err := opts.Enroll(ctx); err != nil {
		t.Fatal(err)
	}
	actual, _, err := store.GetServer(ctx, server.ID)
	if err != nil || actual.LastHeartbeat != nil || actual.ConnectionState != "never-connected" {
		t.Fatalf("enrollment fabricated telemetry: %+v, %v", actual, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := opts.VerifyMeasurements(canceled); err == nil {
		t.Fatal("completed without telemetry")
	}
	sample := contracts.MetricSample{ServerID: server.ID, CollectorEpoch: "install-telemetry-epoch", Sequence: 1, ObservedAt: now, ReceivedAt: now, Values: map[string]float64{"cpu.utilization": 25}}
	if _, err := store.IngestSamples(ctx, server.ID, []contracts.MetricSample{sample}, nil); err != nil {
		t.Fatal(err)
	}
	if err := opts.VerifyMeasurements(ctx); err != nil {
		t.Fatalf("received sample rejected: %v", err)
	}
	// A stale sample from a previous installation cannot prove the new agent is live.
	if err := svc.waitForMeasurements(canceled, server.ID, now.Add(time.Second)); err == nil {
		t.Fatal("stale telemetry accepted")
	}
}
