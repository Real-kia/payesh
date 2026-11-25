package monitoring

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestFleetTrafficOptInAndHourlyCoverage(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "fleet.db")
	store, err := OpenStore(ctx, path, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"server-fleet-0001", "server-fleet-0002"} {
		server := testServer()
		server.ID = contracts.ServerID(id)
		if err := store.EnsureServer(ctx, server); err != nil {
			t.Fatal(err)
		}
		var rollups []Rollup
		for offset := 0; offset < 4; offset++ {
			rollups = append(rollups, Rollup{ServerID: server.ID, Metric: "net.billing.rx_bytes",
				BucketStart: base.Add(time.Duration(offset) * time.Hour), BucketSeconds: 3600,
				SampleCount: 2, CounterDelta: "18446744073709551615", Coverage: "complete"})
		}
		if err := store.PutRollups(ctx, rollups); err != nil {
			t.Fatal(err)
		}
	}
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	read := func(now time.Time) FleetTrafficHistory {
		t.Helper()
		history, err := store.FleetTrafficHistory(ctx, now)
		check(err)
		return history
	}
	check(store.captureFleetTraffic(ctx, base.Add(time.Hour)))
	if history := read(base.Add(time.Hour)); history.Enabled || len(history.Hours) != 0 {
		t.Fatalf("recorded before opt-in: %+v", history)
	}
	check(store.SetFleetTrafficEnabled(ctx, true, base.Add(time.Hour+time.Minute)))
	check(store.Close())
	store, err = OpenStore(ctx, path, StoreOptions{})
	check(err)

	now := base.Add(3*time.Hour + 5*time.Minute)
	check(store.captureFleetTraffic(ctx, now))
	check(store.captureFleetTraffic(ctx, now))
	history := read(now)
	if !history.Enabled || len(history.Hours) != 1 {
		t.Fatalf("expected a single hour after enabling: %+v", history)
	}
	hour := history.Hours[0]
	if !hour.Start.Equal(base.Add(2*time.Hour)) || hour.DownloadBytes == nil ||
		*hour.DownloadBytes != "36893488147419103230" || hour.DownloadServers != 2 ||
		hour.UploadBytes != nil || hour.UploadServers != 0 || hour.ServerCount != 2 {
		t.Fatalf("incorrect fleet totals or coverage: %+v", hour)
	}
	check(store.SetFleetTrafficEnabled(ctx, false, now))
	now = now.Add(time.Hour)
	check(store.captureFleetTraffic(ctx, now))
	if history := read(now); history.Enabled || len(history.Hours) != 1 {
		t.Fatalf("disabling should keep previous hours only: %+v", history)
	}
	now = now.Add(fleetTrafficRetention)
	check(store.captureFleetTraffic(ctx, now))
	if history := read(now); len(history.Hours) != 0 {
		t.Fatalf("expired history still returned: %+v", history)
	}
}
