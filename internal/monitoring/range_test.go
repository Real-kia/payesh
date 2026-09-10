package monitoring

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func acknowledgeAllQueuedSamples(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	for {
		pending, err := store.ListPendingPostProcessSamples(ctx, MaxPageItems)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) == 0 {
			return
		}
		for _, sample := range pending {
			if err := store.AcknowledgePostProcessSample(ctx, sample); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestCoverageGapRangesCoalesceNumericallyAndPreserveBounds(t *testing.T) {
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
	epoch := contracts.CollectorEpoch("range-coalesce-epoch-01")
	if err := store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if err := insertCoverageGapTx(ctx, tx, server.ID, epoch, 9007199254740993, 9007199254740994, "transport-disconnect", "2026-09-10T12:00:00Z", "2026-09-10T12:00:01Z"); err != nil {
			return err
		}
		return insertCoverageGapTx(ctx, tx, server.ID, epoch, 9007199254740994, 9007199254740996, "transport-disconnect", "2026-09-10T11:59:00Z", "2026-09-10T12:00:02Z")
	}); err != nil {
		t.Fatal(err)
	}
	var count int
	var fromText, toText, fromObserved, toObserved string
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*),MIN(from_sequence),MAX(to_sequence),MIN(COALESCE(from_observed_at,'')),MAX(COALESCE(to_observed_at,'')) FROM coverage_gaps WHERE server_id=? AND collector_epoch=?`, string(server.ID), string(epoch)).Scan(&count, &fromText, &toText, &fromObserved, &toObserved); err != nil {
		t.Fatal(err)
	}
	if count != 1 || fromText != "9007199254740993" || toText != "9007199254740996" || fromObserved != "2026-09-10T11:59:00Z" || toObserved != "2026-09-10T12:00:02Z" {
		t.Fatalf("coverage gaps were not numerically coalesced: count=%d range=[%s,%s) bounds=[%s,%s]", count, fromText, toText, fromObserved, toObserved)
	}
	if err := store.WithTransaction(ctx, func(tx *sql.Tx) error {
		return insertCoverageGapTx(ctx, tx, server.ID, epoch, 9007199254740996, 9007199254740997, "retention", "", "2026-09-10T12:00:03Z")
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(from_observed_at,''),COALESCE(to_observed_at,'') FROM coverage_gaps WHERE server_id=? AND collector_epoch=?`, string(server.ID), string(epoch)).Scan(&fromObserved, &toObserved); err != nil {
		t.Fatal(err)
	}
	if fromObserved != "" || toObserved != "2026-09-10T12:00:03Z" {
		t.Fatalf("coalescing fabricated an unknown observation bound: [%s,%s]", fromObserved, toObserved)
	}
	if !halfOpenRangesTouch(^uint64(0)-2, ^uint64(0)-1, ^uint64(0)-1, ^uint64(0)) {
		t.Fatal("half-open MaxUint adjacency was rejected")
	}
	if halfOpenRangesTouch(^uint64(0)-1, ^uint64(0), 0, 1) {
		t.Fatal("half-open disjoint MaxUint range was merged")
	}
}

func TestMetricSampleTombstonesCoalesceInclusiveAndMaxUintSafely(t *testing.T) {
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
	epoch := contracts.CollectorEpoch("metric-tombstone-epoch-01")
	if err := store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if err := insertMetricSampleTombstoneTx(ctx, tx, server.ID, epoch, 9007199254740993, 9007199254740993); err != nil {
			return err
		}
		if err := insertMetricSampleTombstoneTx(ctx, tx, server.ID, epoch, 9007199254740994, 9007199254740995); err != nil {
			return err
		}
		return insertMetricSampleTombstoneTx(ctx, tx, server.ID, epoch, 9007199254740996, 9007199254740996)
	}); err != nil {
		t.Fatal(err)
	}
	var count int
	var fromText, toText string
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*),MIN(from_sequence),MAX(to_sequence) FROM metric_sample_tombstones WHERE server_id=? AND collector_epoch=?`, string(server.ID), string(epoch)).Scan(&count, &fromText, &toText); err != nil {
		t.Fatal(err)
	}
	if count != 1 || fromText != "9007199254740993" || toText != "9007199254740996" {
		t.Fatalf("metric tombstones were not coalesced: count=%d range=[%s,%s]", count, fromText, toText)
	}
	max := ^uint64(0)
	if err := store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if err := insertMetricSampleTombstoneTx(ctx, tx, server.ID, epoch, max-1, max-1); err != nil {
			return err
		}
		return insertMetricSampleTombstoneTx(ctx, tx, server.ID, epoch, max, max)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metric_sample_tombstones WHERE server_id=? AND collector_epoch=? AND from_sequence=? AND to_sequence=?`, string(server.ID), string(epoch), "18446744073709551614", "18446744073709551615").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("MaxUint tombstones were not merged safely: count=%d", count)
	}
	if !inclusiveRangesTouch(max-1, max-1, max, max) || inclusiveRangesTouch(max, max, 0, 0) {
		t.Fatal("inclusive MaxUint adjacency guard is incorrect")
	}
}

func TestTrafficUsageTombstonesCoalescePerAllowanceStream(t *testing.T) {
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
	epoch := contracts.CollectorEpoch("traffic-tombstone-epoch-01")
	if err := store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if err := insertTrafficUsageTombstoneTx(ctx, tx, server.ID, epoch, "host", "combined", 9007199254740993, 9007199254740994); err != nil {
			return err
		}
		if err := insertTrafficUsageTombstoneTx(ctx, tx, server.ID, epoch, "host", "combined", 9007199254740995, 9007199254740995); err != nil {
			return err
		}
		return insertTrafficUsageTombstoneTx(ctx, tx, server.ID, epoch, "host", "rx", 9007199254740993, 9007199254740994)
	}); err != nil {
		t.Fatal(err)
	}
	var combinedCount, rxCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM traffic_usage_tombstones WHERE server_id=? AND collector_epoch=? AND scope=? AND direction=?`, string(server.ID), string(epoch), "host", "combined").Scan(&combinedCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM traffic_usage_tombstones WHERE server_id=? AND collector_epoch=? AND scope=? AND direction=?`, string(server.ID), string(epoch), "host", "rx").Scan(&rxCount); err != nil {
		t.Fatal(err)
	}
	if combinedCount != 1 || rxCount != 1 {
		t.Fatalf("traffic tombstones were not bounded per stream: combined=%d rx=%d", combinedCount, rxCount)
	}
	var fromText, toText string
	if err := store.db.QueryRowContext(ctx, `SELECT from_sequence,to_sequence FROM traffic_usage_tombstones WHERE server_id=? AND collector_epoch=? AND scope=? AND direction=?`, string(server.ID), string(epoch), "host", "combined").Scan(&fromText, &toText); err != nil {
		t.Fatal(err)
	}
	if fromText != "9007199254740993" || toText != "9007199254740995" {
		t.Fatalf("traffic tombstone range changed: [%s,%s]", fromText, toText)
	}
}

func TestPruneCoalescesRetentionMetadataAcrossSmallBatches(t *testing.T) {
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
	old := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	samples := make([]contracts.MetricSample, 0, 8)
	for sequence := uint64(0); sequence < 8; sequence++ {
		samples = append(samples, contracts.MetricSample{ServerID: server.ID, CollectorEpoch: "prune-range-epoch-01", Sequence: sequence, ObservedAt: old.Add(time.Duration(sequence) * time.Second), ReceivedAt: old.Add(time.Duration(sequence) * time.Second), Values: map[string]float64{"cpu.utilization": 1}})
	}
	if _, err := store.IngestSamples(ctx, server.ID, samples, nil); err != nil {
		t.Fatal(err)
	}
	acknowledgeAllQueuedSamples(t, ctx, store)
	if _, err := store.Prune(ctx, old.Add(48*time.Hour), RetentionPolicy{FullResolutionAge: time.Hour, BatchSize: 1}); err != nil {
		t.Fatal(err)
	}
	var gaps, tombstones int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM coverage_gaps WHERE server_id=? AND collector_epoch=?`, string(server.ID), "prune-range-epoch-01").Scan(&gaps); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metric_sample_tombstones WHERE server_id=? AND collector_epoch=?`, string(server.ID), "prune-range-epoch-01").Scan(&tombstones); err != nil {
		t.Fatal(err)
	}
	if gaps != 1 || tombstones != 1 {
		t.Fatalf("small prune batches left uncoalesced metadata: gaps=%d tombstones=%d", gaps, tombstones)
	}
}

func TestTrafficLedgerCompactionCoalescesRanges(t *testing.T) {
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
	epoch := contracts.CollectorEpoch("ledger-range-epoch-01")
	if err := store.WithTransaction(ctx, func(tx *sql.Tx) error {
		for sequence := uint64(9007199254740993); sequence <= 9007199254740997; sequence++ {
			if _, err := tx.ExecContext(ctx, `INSERT INTO traffic_usage_ledger(server_id,collector_epoch,sequence,scope,direction) VALUES(?,?,?,?,?)`, string(server.ID), string(epoch), strconv.FormatUint(sequence, 10), "host", "combined"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	compacted, err := store.compactTrafficUsageLedger(ctx)
	if err != nil || compacted != 5 {
		t.Fatalf("ledger compaction failed: compacted=%d err=%v", compacted, err)
	}
	var count int
	var fromText, toText string
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*),MIN(from_sequence),MAX(to_sequence) FROM traffic_usage_tombstones WHERE server_id=? AND collector_epoch=? AND scope=? AND direction=?`, string(server.ID), string(epoch), "host", "combined").Scan(&count, &fromText, &toText); err != nil {
		t.Fatal(err)
	}
	if count != 1 || fromText != "9007199254740993" || toText != "9007199254740997" {
		t.Fatalf("ledger compaction left uncoalesced tombstones: count=%d range=[%s,%s]", count, fromText, toText)
	}
}

func TestPruneBoundsRetirementAuthorityAndFailsClosedForEpochChurn(t *testing.T) {
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
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	oldSeen := now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	for index := 0; index < 8; index++ {
		epoch := contracts.CollectorEpoch(fmt.Sprintf("stale-range-epoch-%02d", index))
		if err := store.WithTransaction(ctx, func(tx *sql.Tx) error {
			if err := insertCoverageGapTx(ctx, tx, server.ID, epoch, 0, 1, "retention", "", ""); err != nil {
				return err
			}
			if err := insertMetricSampleTombstoneTx(ctx, tx, server.ID, epoch, 0, 0); err != nil {
				return err
			}
			if err := insertTrafficUsageTombstoneTx(ctx, tx, server.ID, epoch, "host", "combined", 0, 0); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO sequence_frontiers(server_id,collector_epoch,frontier,max_point) VALUES(?,?,?,0)`, string(server.ID), string(epoch), "1"); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO collector_epoch_metadata(server_id,collector_epoch,first_seen_at,last_seen_at) VALUES(?,?,?,?)`, string(server.ID), string(epoch), oldSeen, oldSeen)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	currentEpoch := contracts.CollectorEpoch("current-range-epoch-01")
	currentSeen := now.Add(10 * time.Minute).Format(time.RFC3339Nano)
	if err := store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if err := insertCoverageGapTx(ctx, tx, server.ID, currentEpoch, 0, 1, "retention", "", ""); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sequence_frontiers(server_id,collector_epoch,frontier,max_point) VALUES(?,?,?,0)`, string(server.ID), string(currentEpoch), "1"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO collector_epoch_metadata(server_id,collector_epoch,first_seen_at,last_seen_at) VALUES(?,?,?,?)`, string(server.ID), string(currentEpoch), currentSeen, currentSeen)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A recent epoch remains queued to model delayed post-processing. Even
	// when the metadata cap is exceeded, this stream and its raw payload must
	// remain untouched.
	recentEpoch := contracts.CollectorEpoch("recent-range-epoch-01")
	queued := contracts.MetricSample{ServerID: server.ID, CollectorEpoch: recentEpoch, Sequence: 0, ObservedAt: now, ReceivedAt: now, Values: map[string]float64{"cpu.utilization": 1}}
	if _, err := store.IngestSamples(ctx, server.ID, []contracts.MetricSample{queued}, nil); err != nil {
		t.Fatal(err)
	}
	oldRollupStart := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO metric_rollups(server_id,metric,bucket_start,bucket_seconds,sample_count,observed_seconds,minimum,maximum,weighted_mean,counter_delta,coverage) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, string(server.ID), "cpu.utilization", oldRollupStart, 60, 1, 60, 1, 1, 1, nil, "complete"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prune(ctx, now, RetentionPolicy{FullResolutionAge: 24 * time.Hour, MetadataAge: time.Hour, MetadataMaxRows: 4, BatchSize: 200}); !errors.Is(err, ErrCollectorEpochAuthorityFull) {
		t.Fatalf("prune error=%v, want bounded authority pressure", err)
	}
	// Only epochs that received an exact retirement marker may have their
	// detailed replay metadata removed. Capacity exhaustion leaves the rest
	// intact rather than weakening exactly-once protection.
	for _, table := range []string{"coverage_gaps", "metric_sample_tombstones", "traffic_usage_tombstones", "sequence_frontiers", "collector_epoch_metadata"} {
		var count int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE server_id=? AND collector_epoch LIKE 'stale-range-%'`, string(server.ID)).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 4 {
			t.Fatalf("authority pressure removed unmarked metadata from %s: %d rows", table, count)
		}
	}
	var currentCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM coverage_gaps WHERE server_id=? AND collector_epoch=?`, string(server.ID), string(currentEpoch)).Scan(&currentCount); err != nil {
		t.Fatal(err)
	}
	if currentCount != 1 {
		t.Fatalf("metadata cap retired the current epoch coverage: %d", currentCount)
	}
	var retiredCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM collector_epoch_retirements WHERE server_id=? AND collector_epoch LIKE 'stale-range-%'`, string(server.ID)).Scan(&retiredCount); err != nil {
		t.Fatal(err)
	}
	if retiredCount != 4 {
		t.Fatalf("retirement authority exceeded configured bound: %d", retiredCount)
	}
	replay := contracts.MetricSample{ServerID: server.ID, CollectorEpoch: "stale-range-epoch-00", Sequence: 0, ObservedAt: now, ReceivedAt: now, Values: map[string]float64{"cpu.utilization": 1}}
	if _, err := store.IngestSamples(ctx, server.ID, []contracts.MetricSample{replay}, nil); err == nil {
		t.Fatal("replay from retired epoch was accepted")
	}
	if err := store.WithTransaction(ctx, func(tx *sql.Tx) error {
		applied, err := TrafficUsageAlreadyAppliedTx(ctx, tx, server.ID, replay.CollectorEpoch, replay.Sequence, "host", "combined")
		if err != nil {
			return err
		}
		if !applied {
			return fmt.Errorf("retired epoch usage was not treated as already applied")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	unseen := contracts.MetricSample{ServerID: server.ID, CollectorEpoch: "brand-new-after-authority-full", Sequence: 0, ObservedAt: now, ReceivedAt: now, Values: map[string]float64{"cpu.utilization": 1}}
	if _, err := store.IngestSamples(ctx, server.ID, []contracts.MetricSample{unseen}, nil); !errors.Is(err, ErrCollectorEpochAuthorityFull) {
		t.Fatalf("unseen epoch at authority cap error=%v", err)
	}
	var afterRejected int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM collector_epoch_retirements`).Scan(&afterRejected); err != nil {
		t.Fatal(err)
	}
	if afterRejected != retiredCount {
		t.Fatalf("rejected churn grew permanent authority: before=%d after=%d", retiredCount, afterRejected)
	}
	var oldRollups int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metric_rollups WHERE bucket_seconds=60 AND bucket_start=?`, oldRollupStart).Scan(&oldRollups); err != nil {
		t.Fatal(err)
	}
	if oldRollups != 0 {
		t.Fatalf("authority pressure prevented independent rollup retention: %d", oldRollups)
	}
	page, err := store.QueryMetrics(ctx, server.ID, time.Time{}, time.Time{}, 10, "")
	if err != nil || len(page.Samples) != 1 || page.Samples[0].CollectorEpoch != recentEpoch {
		t.Fatalf("queued epoch was changed by metadata GC: samples=%#v err=%v", page.Samples, err)
	}
	pending, err := store.ListPendingPostProcessSamples(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].CollectorEpoch != recentEpoch {
		t.Fatalf("metadata GC removed queued work: pending=%#v err=%v", pending, err)
	}
}

func TestPruneRetainsTrafficPeriodWhileDerivedSampleIsQueued(t *testing.T) {
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
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	period := contracts.TrafficPeriod{Scope: "host", From: now.Add(-72 * time.Hour), To: now.Add(-48 * time.Hour), Timezone: "UTC", AllowanceBytes: 1000, Direction: "combined", CountedBytes: 10, Continuity: "complete"}
	if err := store.UpsertTrafficPeriod(ctx, server.ID, period); err != nil {
		t.Fatal(err)
	}
	sample := contracts.MetricSample{ServerID: server.ID, CollectorEpoch: "queued-period-epoch-01", Sequence: 0, ObservedAt: period.From.Add(time.Hour), ReceivedAt: period.From.Add(time.Hour), Values: map[string]float64{"cpu.utilization": 1}}
	if _, err := store.IngestSamples(ctx, server.ID, []contracts.MetricSample{sample}, nil); err != nil {
		t.Fatal(err)
	}
	policy := DefaultRetentionPolicy()
	policy.FullResolutionAge = time.Hour
	policy.TrafficPeriodAge = time.Hour
	if _, err := store.Prune(ctx, now, policy); err != nil {
		t.Fatal(err)
	}
	var retained int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM traffic_periods WHERE server_id=?`, string(server.ID)).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 1 {
		t.Fatal("queued derived work lost its historical traffic period identity")
	}
	if err := store.AcknowledgePostProcessSample(ctx, sample); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prune(ctx, now, policy); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM traffic_periods WHERE server_id=?`, string(server.ID)).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 0 {
		t.Fatalf("drained queue kept expired traffic period: %d", retained)
	}
}

func TestFragmentedRangeMetadataFailsClosedAtFleetBound(t *testing.T) {
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
	epoch := contracts.CollectorEpoch("fragmented-range-epoch-01")
	if _, err := store.db.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i+1<?) INSERT INTO coverage_gaps(server_id,collector_epoch,from_sequence,to_sequence,reason) SELECT ?,?,CAST(i*2 AS TEXT),CAST(i*2+1 AS TEXT),'transport-disconnect' FROM n`, MaxRangeMetadataRows, string(server.ID), string(epoch)); err != nil {
		t.Fatal(err)
	}
	err = store.WithTransaction(ctx, func(tx *sql.Tx) error {
		return insertCoverageGapTx(ctx, tx, server.ID, epoch, uint64(MaxRangeMetadataRows*2), uint64(MaxRangeMetadataRows*2+1), "transport-disconnect", "", "")
	})
	if !errors.Is(err, ErrRangeMetadataFull) {
		t.Fatalf("fragmented range insertion error=%v, want ErrRangeMetadataFull", err)
	}
	var rows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM coverage_gaps`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != MaxRangeMetadataRows {
		t.Fatalf("range authority grew past bound: %d", rows)
	}
}
