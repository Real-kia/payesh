package traffic

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/alerts"
	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

// Mirrors the endurance harness: every retention gap must be covered by a
// billing/combined traffic tombstone once a Prune call has returned.
func TestRetentionGapsAreCoveredByTrafficTombstonesAfterPrune(t *testing.T) {
	t.Run("all samples carry counters", func(t *testing.T) { retentionGapRepro(t, false, 0) })
	t.Run("first sample has no counters", func(t *testing.T) { retentionGapRepro(t, true, 0) })
	// The endurance fixture configures the allowance after the first samples
	// arrive. Those earlier samples were never billed, so traffic tombstones
	// legitimately begin at the first billed sequence, not at the gap start.
	t.Run("allowance configured after first samples", func(t *testing.T) { retentionGapRepro(t, false, 3) })
}

func retentionGapRepro(t *testing.T, firstSampleWithoutCounters bool, unbilledPrefix int) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine, err := alerts.NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	processor, err := NewProcessor(store, engine)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.January, 10, 0, 0, 0, 0, time.UTC)
	servers := []contracts.ServerID{"server-gap-repro-0001", "server-gap-repro-0002"}
	for _, id := range servers {
		if err := store.EnsureServer(ctx, contracts.Server{ID: id, Name: string(id), Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
			t.Fatal(err)
		}
	}
	configure := func() {
		for _, id := range servers {
			a := allowance(1, "UTC")
			a.Scope = "billing"
			if err := store.UpsertTrafficAllowance(ctx, id, a); err != nil {
				t.Fatal(err)
			}
		}
	}
	if unbilledPrefix == 0 {
		configure()
	}
	const total = 800
	step := 50
	for sequence := 0; sequence < total; sequence += step {
		width := step
		if unbilledPrefix > 0 && sequence == 0 {
			width = unbilledPrefix
		}
		for _, id := range servers {
			batch := make([]contracts.MetricSample, 0, width)
			for s := sequence; s < sequence+width; s++ {
				at := base.Add(time.Duration(s) * 5 * time.Second)
				rx := strconv.Itoa(1000 + s*10)
				counters := map[string]string{"net.billing.rx_bytes": rx, "net.billing.tx_bytes": "0"}
				validity := map[string]string{"net.billing.rx_bytes": "valid", "net.billing.tx_bytes": "valid"}
				if s == 0 && firstSampleWithoutCounters {
					counters, validity = map[string]string{}, map[string]string{}
				}
				batch = append(batch, contracts.MetricSample{ServerID: id, CollectorEpoch: contracts.CollectorEpoch("gap-repro-" + string(id[len(id)-4:]) + "-epoch"), Sequence: uint64(s), ObservedAt: at, ReceivedAt: at, Values: map[string]float64{},
					Counters: counters, Validity: validity})
			}
			if _, err := store.IngestSamples(ctx, id, batch, nil); err != nil {
				t.Fatal(err)
			}
		}
		if err := processor.ObserveIngestion(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if unbilledPrefix > 0 && sequence == 0 {
			configure()
			step = 50
			sequence = unbilledPrefix - step
		}
	}
	// Prune in several steps, as the hub does every few minutes, and check after each.
	for step := 1; step <= 4; step++ {
		now := base.Add(24*time.Hour + time.Duration(step*100*5)*time.Second)
		if _, err := store.Prune(ctx, now, monitoring.RetentionPolicy{FullResolutionAge: 24 * time.Hour, BatchSize: 25}); err != nil {
			t.Fatal(err)
		}
		if err := store.WithTransaction(ctx, func(tx *sql.Tx) error { return checkGapCoverage(ctx, tx, step, uint64(unbilledPrefix)) }); err != nil {
			t.Fatal(err)
		}
	}
}

func checkGapCoverage(ctx context.Context, tx *sql.Tx, step int, firstBilled uint64) error {
	rows, err := tx.QueryContext(ctx, `SELECT server_id,collector_epoch,from_sequence,to_sequence FROM coverage_gaps WHERE reason='retention'`)
	if err != nil {
		return err
	}
	type gap struct {
		server, epoch string
		from, to      uint64
	}
	var gaps []gap
	for rows.Next() {
		var g gap
		var f, e string
		if err := rows.Scan(&g.server, &g.epoch, &f, &e); err != nil {
			return err
		}
		g.from, _ = strconv.ParseUint(f, 10, 64)
		g.to, _ = strconv.ParseUint(e, 10, 64)
		gaps = append(gaps, g)
	}
	rows.Close()
	if len(gaps) == 0 {
		return fmt.Errorf("step %d: prune produced no retention gaps", step)
	}
	for _, g := range gaps {
		tr, err := tx.QueryContext(ctx, `SELECT from_sequence,to_sequence FROM traffic_usage_tombstones WHERE server_id=? AND collector_epoch=? AND scope='billing' AND direction='combined' ORDER BY CAST(from_sequence AS INTEGER)`, g.server, g.epoch)
		if err != nil {
			return err
		}
		frontier := g.from
		if frontier < firstBilled {
			frontier = firstBilled
		}
		var seen []string
		for tr.Next() {
			var f, e string
			if err := tr.Scan(&f, &e); err != nil {
				return err
			}
			seen = append(seen, f+"-"+e)
			lo, _ := strconv.ParseUint(f, 10, 64)
			hi, _ := strconv.ParseUint(e, 10, 64)
			if hi < frontier {
				continue
			}
			if lo > frontier {
				break
			}
			if hi+1 > frontier {
				frontier = hi + 1
			}
		}
		tr.Close()
		if frontier < g.to {
			return fmt.Errorf("step %d: gap %s/%s [%d,%d) not covered by billing tombstones %v (frontier %d)", step, g.server, g.epoch, g.from, g.to, seen, frontier)
		}
	}
	return nil
}
