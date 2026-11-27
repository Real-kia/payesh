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
		server.Role = "node"
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
	taggedID := contracts.ServerID("server-fleet-0001")
	_, err = store.SetServerTags(ctx, taggedID, []string{"Edge", "West"})
	check(err)
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
	assertTag := func(now time.Time, tag string) {
		t.Helper()
		filtered, err := store.FleetTrafficHistoryByTag(ctx, now, tag)
		check(err)
		if len(filtered.Hours) != 1 {
			t.Fatalf("expected one saved hour for %s: %+v", tag, filtered)
		}
		point := filtered.Hours[0]
		if point.DownloadBytes == nil || *point.DownloadBytes != "18446744073709551615" ||
			point.DownloadServers != 1 || point.ServerCount != 1 || point.UploadBytes != nil {
			t.Fatalf("incorrect saved totals for %s: %+v", tag, point)
		}
	}
	assertTag(now, "EDGE")
	assertTag(now, "west")
	server, found, err := store.GetServer(ctx, taggedID)
	check(err)
	if !found {
		t.Fatal("tagged server missing before deletion")
	}
	check(store.DeleteServer(ctx, taggedID, server.ConfigurationRevision))
	assertTag(now, "edge")
	history = read(now)
	if len(history.Hours) != 1 || history.Hours[0].DownloadBytes == nil ||
		*history.Hours[0].DownloadBytes != "36893488147419103230" ||
		history.Hours[0].ServerCount != 2 || len(history.Tags) != 2 || history.Tags[0] != "edge" {
		t.Fatalf("server deletion changed saved history: %+v", history)
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
	filtered, err := store.FleetTrafficHistoryByTag(ctx, now, "edge")
	check(err)
	if len(filtered.Hours) != 0 || len(read(now).Tags) != 0 {
		t.Fatalf("expired tagged history still returned: %+v", filtered)
	}
}

func TestFleetTrafficFiveMinuteCoverageAndRetention(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "fleet.db"), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	server := testServer()
	check(store.EnsureServer(ctx, server))
	_, err = store.SetServerTags(ctx, server.ID, []string{"Edge"})
	check(err)
	for _, start := range []time.Time{base, base.Add(5 * time.Minute), base.Add(10 * time.Minute)} {
		check(store.PutRollups(ctx, []Rollup{{
			ServerID: server.ID, Metric: "net.billing.rx_bytes", BucketStart: start,
			BucketSeconds: MidRollupSeconds, CounterDelta: "2700000000", SampleCount: 2, Coverage: "complete",
		}}))
	}
	check(store.SetFleetTrafficEnabled(ctx, true, base.Add(time.Minute)))
	read := func(now time.Time, tag string) FleetTrafficHistory {
		t.Helper()
		history, err := store.FleetTrafficHistoryByTag(ctx, now, tag)
		check(err)
		return history
	}
	now := base.Add(10 * time.Minute)
	check(store.captureFleetTraffic(ctx, now))
	if history := read(now, ""); len(history.FiveMinutes) != 0 {
		t.Fatalf("included a bucket before opt-in or before the grace period: %+v", history)
	}
	now = now.Add(time.Minute)
	check(store.captureFleetTraffic(ctx, now))
	check(store.captureFleetTraffic(ctx, now))
	for _, tag := range []string{"", "EDGE"} {
		history := read(now, tag)
		if len(history.Hours) != 0 || len(history.FiveMinutes) != 1 ||
			history.FiveMinuteEnd == nil || !history.FiveMinuteEnd.Equal(base.Add(10*time.Minute)) {
			t.Fatalf("expected one five-minute bucket for %q: %+v", tag, history)
		}
		point := history.FiveMinutes[0]
		if !point.Start.Equal(base.Add(5*time.Minute)) || point.DownloadBytes == nil ||
			*point.DownloadBytes != "2700000000" || point.DownloadServers != 1 ||
			point.ServerCount != 1 || point.UploadBytes != nil {
			t.Fatalf("incorrect fine-resolution point for %q: %+v", tag, point)
		}
	}
	check(store.SetFleetTrafficEnabled(ctx, false, now))
	now = now.Add(5 * time.Minute)
	check(store.captureFleetTraffic(ctx, now))
	if history := read(now, ""); history.Enabled || len(history.FiveMinutes) != 1 {
		t.Fatalf("disabling should keep the previously recorded bucket only: %+v", history)
	}
	now = now.Add(24 * time.Hour)
	check(store.captureFleetTraffic(ctx, now))
	if history := read(now, "edge"); len(history.FiveMinutes) != 0 {
		t.Fatalf("expired five-minute bucket was returned: %+v", history)
	}
	var saved int
	check(store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM fleet_traffic_five_minutes").Scan(&saved))
	if saved != 0 {
		t.Fatalf("retention left %d expired five-minute rows", saved)
	}
}
