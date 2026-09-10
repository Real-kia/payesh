package traffic

import (
	"context"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/alerts"
	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func TestPruneProtectsQueuedSamplesUntilTrafficProcessingCompletes(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-queued-retention-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Queued retention", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertTrafficAllowance(ctx, serverID, allowance(1, "UTC")); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.January, 10, 12, 0, 0, 0, time.UTC)
	first := contracts.MetricSample{ServerID: serverID, CollectorEpoch: "queued-retention-epoch-01", Sequence: 0, ObservedAt: base, ReceivedAt: base, Values: map[string]float64{}, Counters: map[string]string{"net.billing.rx_bytes": "100", "net.billing.tx_bytes": "0"}, Validity: map[string]string{"net.billing.rx_bytes": "valid", "net.billing.tx_bytes": "valid"}}
	if _, err := store.IngestSamples(ctx, serverID, []contracts.MetricSample{first}, nil); err != nil {
		t.Fatal(err)
	}

	engine, err := alerts.NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	processor, err := NewProcessor(store, engine)
	if err != nil {
		t.Fatal(err)
	}
	// Process and acknowledge the predecessor first. The later queued sample
	// must still keep this already-acknowledged raw row available for its
	// counter delta.
	if err := processor.ObserveIngestion(ctx, nil); err != nil {
		t.Fatal(err)
	}
	second := contracts.MetricSample{ServerID: serverID, CollectorEpoch: "queued-retention-epoch-01", Sequence: 1, ObservedAt: base.Add(time.Minute), ReceivedAt: base.Add(time.Minute), Values: map[string]float64{}, Counters: map[string]string{"net.billing.rx_bytes": "250", "net.billing.tx_bytes": "0"}, Validity: map[string]string{"net.billing.rx_bytes": "valid", "net.billing.tx_bytes": "valid"}}
	if _, err := store.IngestSamples(ctx, serverID, []contracts.MetricSample{second}, nil); err != nil {
		t.Fatal(err)
	}
	// The second sample is still queued. Retention must leave both rows
	// available, including the acknowledged predecessor needed for sequence 1.
	stats, err := store.Prune(ctx, base.Add(48*time.Hour), monitoring.RetentionPolicy{FullResolutionAge: time.Hour, BatchSize: 200})
	if err != nil {
		t.Fatal(err)
	}
	if stats.MetricSamples != 0 {
		t.Fatalf("retention deleted queued raw samples: %+v", stats)
	}
	page, err := store.QueryMetrics(ctx, serverID, time.Time{}, time.Time{}, 10, "")
	if err != nil || len(page.Samples) != 2 {
		t.Fatalf("queued samples were not retained: samples=%d err=%v", len(page.Samples), err)
	}

	if err := processor.ObserveIngestion(ctx, nil); err != nil {
		t.Fatal(err)
	}
	trafficPage, err := store.QueryTrafficPeriods(ctx, serverID, time.Time{}, time.Time{}, "host", 10, "")
	if err != nil || len(trafficPage.Periods) != 1 || trafficPage.Periods[0].CountedBytes != 150 {
		t.Fatalf("delayed queue processing lost predecessor delta: periods=%+v err=%v", trafficPage.Periods, err)
	}

	// Once processing acknowledges both queue entries, a later prune can delete
	// their raw payloads and leave replay tombstones behind.
	stats, err = store.Prune(ctx, base.Add(48*time.Hour), monitoring.RetentionPolicy{FullResolutionAge: time.Hour, BatchSize: 200})
	if err != nil {
		t.Fatal(err)
	}
	if stats.MetricSamples != 2 {
		t.Fatalf("acknowledged samples were not eligible for retention: %+v", stats)
	}
	trafficPage, err = store.QueryTrafficPeriods(ctx, serverID, time.Time{}, time.Time{}, "host", 10, "")
	if err != nil || len(trafficPage.Periods) != 1 || trafficPage.Periods[0].CountedBytes != 150 {
		t.Fatalf("traffic period changed after raw retention: periods=%+v err=%v", trafficPage.Periods, err)
	}
}
