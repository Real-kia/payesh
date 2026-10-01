package monitoring

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTrafficRangeRetainedTotalsAndMissingHours(t *testing.T) {
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
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rollups := []Rollup{}
	for i := 0; i < 3; i++ {
		for _, metric := range []string{"net.billing.rx_bytes", "net.billing.tx_bytes", "net.eth0.rx_bytes"} {
			r := Rollup{ServerID: server.ID, Metric: metric, BucketStart: base.Add(time.Duration(i) * time.Hour), BucketSeconds: 3600, SampleCount: 2, CounterDelta: "18446744073709551615", Coverage: "complete"}
			if i == 1 && metric == "net.billing.rx_bytes" {
				r.CounterDelta = ""
				r.Coverage = "uncertain"
			}
			rollups = append(rollups, r)
		}
	}
	if err := store.PutRollups(ctx, rollups); err != nil {
		t.Fatal(err)
	}
	r, err := store.QueryTrafficRange(ctx, server.ID, base, base.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if r.DownloadHours != 1 || r.UploadHours != 2 || r.RequestedHours != 2 || *r.DownloadBytes != "18446744073709551615" || *r.UploadBytes != "36893488147419103230" || *r.TotalBytes != "55340232221128654845" {
		t.Fatalf("incorrect bounded totals: %+v", r)
	}
	r, err = store.QueryTrafficRange(ctx, server.ID, base.Add(10*time.Hour), base.Add(11*time.Hour))
	if err != nil || r.TotalBytes != nil || r.DownloadBytes != nil || r.UploadBytes != nil {
		t.Fatalf("missing history became zero: %+v %v", r, err)
	}
	for _, bounds := range [][2]time.Time{{base, base}, {base, base.Add(91 * 24 * time.Hour)}, {base.Add(time.Minute), base.Add(time.Hour)}} {
		if _, err := store.QueryTrafficRange(ctx, server.ID, bounds[0], bounds[1]); err == nil {
			t.Fatal("invalid range accepted")
		}
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO rollup_rebuild_queue(server_id,bucket_start,bucket_seconds) VALUES(?,?,3600)`, string(server.ID), FormatPersistedTime(base)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.QueryTrafficRange(ctx, server.ID, base, base.Add(time.Hour)); !errors.Is(err, ErrRollupsPending) {
		t.Fatalf("pending history presented as current: %v", err)
	}
}
