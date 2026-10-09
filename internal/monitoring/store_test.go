package monitoring

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func testServer() contracts.Server {
	return contracts.Server{ID: "server-local-0001", Name: "Local", Role: "standalone", Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics", "logs", "traffic"}, Version: "0.1.0", ConnectionState: "connected", FreshnessState: "fresh"}
}

func TestQueryMetricSamplesRangeKeepsNewestBoundedTail(t *testing.T) {
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
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	samples := make([]contracts.MetricSample, 0, 5)
	for sequence := uint64(0); sequence < 5; sequence++ {
		at := base.Add(time.Duration(sequence) * time.Minute)
		samples = append(samples, contracts.MetricSample{
			ServerID: server.ID, CollectorEpoch: "forecast-tail-epoch-01", Sequence: sequence,
			ObservedAt: at, ReceivedAt: at, Values: map[string]float64{"cpu.utilization": float64(sequence)},
		})
	}
	if _, err := store.IngestSamples(ctx, server.ID, samples, nil); err != nil {
		t.Fatal(err)
	}
	got, truncated, err := store.QueryMetricSamplesRange(ctx, server.ID, base, base.Add(4*time.Minute), 3)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(got) != 3 || got[0].Sequence != 2 || got[1].Sequence != 3 || got[2].Sequence != 4 {
		t.Fatalf("bounded metric tail=%+v truncated=%v", got, truncated)
	}
}

func TestCollectorEpochPredecessorIncludesRetiredAuthority(t *testing.T) {
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
	if _, err := store.db.ExecContext(ctx, `INSERT INTO collector_epoch_metadata(server_id,collector_epoch,first_seen_at,last_seen_at) VALUES(?,?,?,?)`, string(server.ID), "current-epoch-authority-01", "2026-01-02T00:00:00Z", "2026-01-02T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO collector_epoch_retirements(server_id,collector_epoch,retired_at) VALUES(?,?,?)`, string(server.ID), "retired-epoch-authority-1", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	var predecessor bool
	if err := store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		predecessor, err = CollectorEpochHasPredecessorTx(ctx, tx, server.ID, "current-epoch-authority-01")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !predecessor {
		t.Fatal("retired collector identity did not establish a reboot predecessor")
	}
}

func testNodeSample(sequence uint64, observed time.Time, value float64, counter string) contracts.NodeMetricSample {
	return contracts.NodeMetricSample{ServerID: "server-local-0001", CollectorEpoch: "epoch-local-0001", Sequence: sequence, ObservedAt: observed, Values: map[string]float64{"cpu.utilization": value}, Counters: map[string]string{"net.billing.rx_bytes": counter}, Validity: map[string]string{"cpu.utilization": "valid", "net.billing.rx_bytes": "valid"}, Units: map[string]string{"cpu.utilization": "percent", "net.billing.rx_bytes": "bytes"}}
}

func TestAlertRuleCreationLimitsCountDisabledRows(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newRule := func(id string, serverID contracts.ServerID) contracts.AlertRule {
		return contracts.AlertRule{ID: id, Name: "Limit", Expression: "cpu.utilization > 90", ServerID: serverID, Enabled: true, DurationSeconds: 60, ReminderSeconds: 0, GroupKey: "limit", IdempotencyKey: id + "-idem", CreatedAt: base}
	}
	t.Run("per server", func(t *testing.T) {
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
		seedDisabledAlertRules(t, store, server.ID, MaxAlertRulesPerServer, "server-limit")
		err = store.CreateAlertRule(ctx, newRule("server-limit-overflow", server.ID))
		if !errors.Is(err, ErrAlertRuleLimit) || !strings.Contains(err.Error(), "4096") || !strings.Contains(err.Error(), "enabled and disabled") {
			t.Fatalf("server limit error=%v", err)
		}
		err = store.WithTransaction(ctx, func(tx *sql.Tx) error {
			return store.SaveAlertRuleTx(ctx, tx, newRule("server-limit-reconcile-overflow", server.ID))
		})
		if !errors.Is(err, ErrAlertRuleLimit) {
			t.Fatalf("transactional reconciliation limit error=%v", err)
		}
	})
	t.Run("total", func(t *testing.T) {
		ctx := context.Background()
		store, err := OpenStore(ctx, ":memory:", StoreOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		seedDisabledAlertRules(t, store, "", MaxAlertRules, "total-limit")
		err = store.CreateAlertRule(ctx, newRule("total-limit-overflow", ""))
		if !errors.Is(err, ErrAlertRuleLimit) || !strings.Contains(err.Error(), "20000") || !strings.Contains(err.Error(), "enabled and disabled") {
			t.Fatalf("total limit error=%v", err)
		}
	})
}

func seedDisabledAlertRules(t *testing.T, store *Store, serverID contracts.ServerID, count int, prefix string) {
	t.Helper()
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	statement, err := tx.PrepareContext(ctx, `INSERT INTO alert_rules(id,name,expression,server_id,enabled,duration_seconds,recovery_threshold,reminder_seconds,group_key,idempotency_key,created_at,effective_at,disable_reason) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer statement.Close()
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	for index := 0; index < count; index++ {
		id := fmt.Sprintf("%s-%05d", prefix, index)
		var server any
		if serverID != "" {
			server = string(serverID)
		}
		if _, err := statement.ExecContext(ctx, id, "Disabled limit row", "cpu.utilization > 90", server, 0, 60, nil, 0, "limit", id+"-idem", created, created, "owner_disabled"); err != nil {
			t.Fatal(err)
		}
	}
	if err := statement.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreIngestIsDurableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertServer(ctx, testServer()); err != nil {
		t.Fatal(err)
	}
	observed := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	sample := testNodeSample(0, observed, 25, "9007199254740993")
	result, err := store.IngestNodeBatch(ctx, testServer().ID, []contracts.NodeMetricSample{sample}, []contracts.CoverageGap{{CollectorEpoch: "epoch-local-0001", FromSequence: 1, ToSequence: 4, Reason: "transport-disconnect"}}, observed.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if result.Inserted != 1 || result.Duplicate != 0 {
		t.Fatalf("unexpected first ingest result: %#v", result)
	}
	result, err = store.IngestNodeBatch(ctx, testServer().ID, []contracts.NodeMetricSample{sample}, nil, observed.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if result.Inserted != 0 || result.Duplicate != 1 {
		t.Fatalf("retry was not idempotent: %#v", result)
	}
	page, err := store.QueryMetrics(ctx, testServer().ID, observed.Add(-time.Minute), observed.Add(time.Minute), 200, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Samples) != 1 || page.Samples[0].Counters["net.billing.rx_bytes"] != "9007199254740993" || len(page.Gaps) != 1 {
		t.Fatalf("stored sample or gap changed: %#v", page)
	}
	period := contracts.TrafficPeriod{
		Scope: "host", From: observed.Add(-time.Hour), To: observed.Add(time.Hour), Timezone: "UTC",
		AllowanceBytes: 18446744073709551615, Direction: "combined", CountedBytes: 9007199254740993, Continuity: "complete",
	}
	if err := store.UpsertTrafficPeriod(ctx, testServer().ID, period); err != nil {
		t.Fatal(err)
	}
	traffic, err := store.QueryTrafficPeriods(ctx, testServer().ID, observed.Add(-2*time.Hour), observed.Add(2*time.Hour), "", 200, "")
	if err != nil || len(traffic.Periods) != 1 || traffic.Periods[0].CountedBytes != period.CountedBytes {
		t.Fatalf("stored traffic period changed: %#v, err=%v", traffic, err)
	}

	rollups := BuildRollupsWithGaps(testServer().ID, page.Samples, page.Gaps, 60)
	if err := store.PutRollups(ctx, rollups); err != nil {
		t.Fatal(err)
	}
	storedRollups, next, truncated, err := store.QueryRollups(ctx, testServer().ID, observed.Add(-time.Minute), observed.Add(time.Minute), 60, 200, "")
	if err != nil || next != "" || truncated || len(storedRollups) == 0 {
		t.Fatalf("durable rollups unavailable: %#v next=%q truncated=%v err=%v", storedRollups, next, truncated, err)
	}
	secondEpoch := testNodeSample(0, observed, 26, "9007199254740994")
	secondEpoch.CollectorEpoch = "epoch-local-0002"
	if _, err := store.IngestNodeBatch(ctx, testServer().ID, []contracts.NodeMetricSample{secondEpoch}, nil, observed.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	firstPage, err := store.QueryMetrics(ctx, testServer().ID, observed.Add(-time.Minute), observed.Add(time.Minute), 1, "")
	if err != nil || !firstPage.Truncated || firstPage.NextCursor == "" || len(firstPage.Samples) != 1 {
		t.Fatalf("metric cursor page was not bounded: %#v err=%v", firstPage, err)
	}
	secondPage, err := store.QueryMetrics(ctx, testServer().ID, observed.Add(-time.Minute), observed.Add(time.Minute), 1, firstPage.NextCursor)
	if err != nil || len(secondPage.Samples) != 1 || secondPage.Samples[0].CollectorEpoch != secondEpoch.CollectorEpoch {
		t.Fatalf("metric cursor lost same-time epoch: %#v err=%v", secondPage, err)
	}
}

func TestGapOnlyIngestNotifiesPostProcessObserver(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertServer(ctx, testServer()); err != nil {
		t.Fatal(err)
	}
	var calls int
	var lastBatch []contracts.MetricSample
	store.SetIngestionObserver(func(_ context.Context, samples []contracts.MetricSample) error {
		calls++
		lastBatch = append([]contracts.MetricSample(nil), samples...)
		return nil
	})
	observed := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	if _, err := store.IngestNodeBatch(ctx, testServer().ID, []contracts.NodeMetricSample{testNodeSample(1, observed, 25, "10")}, nil, observed); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(lastBatch) != 1 {
		t.Fatalf("sample ingest did not notify observer: calls=%d batch=%d", calls, len(lastBatch))
	}
	if _, err := store.IngestNodeBatch(ctx, testServer().ID, nil, []contracts.CoverageGap{{CollectorEpoch: "epoch-local-0001", FromSequence: 0, ToSequence: 1, Reason: "spool-eviction"}}, observed.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(lastBatch) != 0 {
		t.Fatalf("gap-only ingest did not notify observer with empty replay hint: calls=%d batch=%d", calls, len(lastBatch))
	}
}

func TestRollupsDoNotManufactureSamplesOrHideCounterResets(t *testing.T) {
	serverID := contracts.ServerID("server-local-0001")
	base := time.Date(2026, 9, 9, 10, 0, 10, 0, time.UTC)
	samples := []contracts.MetricSample{
		{ServerID: serverID, CollectorEpoch: "epoch-local-0001", Sequence: 0, ObservedAt: base, ReceivedAt: base, Values: map[string]float64{"cpu.utilization": 20}, Counters: map[string]string{"net.rx": "100"}, Validity: map[string]string{"cpu.utilization": "valid", "net.rx": "valid"}},
		{ServerID: serverID, CollectorEpoch: "epoch-local-0001", Sequence: 1, ObservedAt: base.Add(20 * time.Second), ReceivedAt: base.Add(20 * time.Second), Values: map[string]float64{"cpu.utilization": 40}, Counters: map[string]string{"net.rx": "160"}, Validity: map[string]string{"cpu.utilization": "valid", "net.rx": "valid"}},
		{ServerID: serverID, CollectorEpoch: "epoch-local-0001", Sequence: 2, ObservedAt: base.Add(40 * time.Second), ReceivedAt: base.Add(40 * time.Second), Values: map[string]float64{"cpu.utilization": 60}, Counters: map[string]string{"net.rx": "4"}, Validity: map[string]string{"cpu.utilization": "valid", "net.rx": "valid"}},
	}
	rollups := BuildRollups(serverID, samples, 60)
	if len(rollups) != 2 {
		t.Fatalf("expected one gauge and one counter rollup, got %d: %#v", len(rollups), rollups)
	}
	for _, rollup := range rollups {
		if rollup.Metric == "cpu.utilization" && (rollup.WeightedMean == nil || *rollup.WeightedMean <= 0 || rollup.SampleCount != 3) {
			t.Fatalf("gauge rollup lost weighted data: %#v", rollup)
		}
		if rollup.Metric == "net.rx" && (rollup.CounterDelta != "" || rollup.Coverage != "uncertain") {
			t.Fatalf("counter reset was treated as exact: %#v", rollup)
		}
	}
	rollups = BuildRollups(serverID, []contracts.MetricSample{
		{ServerID: serverID, CollectorEpoch: "epoch-local-0001", Sequence: 0, ObservedAt: base, ReceivedAt: base, Values: map[string]float64{}, Counters: map[string]string{"net.rx": "100"}, Validity: map[string]string{"net.rx": "valid"}},
		{ServerID: serverID, CollectorEpoch: "epoch-local-0001", Sequence: 1, ObservedAt: base.Add(20 * time.Second), ReceivedAt: base.Add(20 * time.Second), Values: map[string]float64{}, Counters: map[string]string{"net.rx": "4"}, Validity: map[string]string{"net.rx": "valid"}},
		{ServerID: serverID, CollectorEpoch: "epoch-local-0001", Sequence: 2, ObservedAt: base.Add(40 * time.Second), ReceivedAt: base.Add(40 * time.Second), Values: map[string]float64{}, Counters: map[string]string{"net.rx": "200"}, Validity: map[string]string{"net.rx": "valid"}},
	}, 60)
	if len(rollups) != 1 || rollups[0].CounterDelta != "" || rollups[0].Coverage != "uncertain" {
		t.Fatalf("counter reset was hidden by a later high value: %#v", rollups)
	}
}

func TestRollupsCarryCounterIntervalsAcrossBucketBoundaries(t *testing.T) {
	serverID := contracts.ServerID("server-local-0001")
	base := time.Date(2026, 9, 9, 10, 0, 50, 0, time.UTC)
	samples := []contracts.MetricSample{
		{ServerID: serverID, CollectorEpoch: "epoch-local-0001", Sequence: 0, ObservedAt: base, ReceivedAt: base, Values: map[string]float64{}, Counters: map[string]string{"net.rx": "100"}, Validity: map[string]string{"net.rx": "valid"}},
		{ServerID: serverID, CollectorEpoch: "epoch-local-0001", Sequence: 1, ObservedAt: base.Add(20 * time.Second), ReceivedAt: base.Add(20 * time.Second), Values: map[string]float64{}, Counters: map[string]string{"net.rx": "160"}, Validity: map[string]string{"net.rx": "valid"}},
		{ServerID: serverID, CollectorEpoch: "epoch-local-0001", Sequence: 2, ObservedAt: base.Add(40 * time.Second), ReceivedAt: base.Add(40 * time.Second), Values: map[string]float64{}, Counters: map[string]string{"net.rx": "200"}, Validity: map[string]string{"net.rx": "valid"}},
	}
	rollups := BuildRollups(serverID, samples, 60)
	var first, second *Rollup
	for index := range rollups {
		if rollups[index].Metric != "net.rx" {
			continue
		}
		if rollups[index].BucketStart.Equal(base.Truncate(time.Minute)) {
			first = &rollups[index]
		} else if rollups[index].BucketStart.Equal(base.Add(time.Minute).Truncate(time.Minute)) {
			second = &rollups[index]
		}
	}
	if first == nil || first.Coverage != "uncertain" || first.CounterDelta != "" {
		t.Fatalf("first bucket should expose its baseline uncertainty: %#v", first)
	}
	if second == nil || second.Coverage != "complete" || second.CounterDelta != "100" {
		t.Fatalf("cross-bucket counter usage was lost: %#v", second)
	}
}

func TestRollupsCarryGaugeIntervalsAcrossBucketBoundaries(t *testing.T) {
	serverID := contracts.ServerID("server-local-0001")
	base := time.Date(2026, 9, 9, 10, 0, 50, 0, time.UTC)
	rollups := BuildRollups(serverID, []contracts.MetricSample{
		{ServerID: serverID, CollectorEpoch: "epoch-local-0001", Sequence: 0, ObservedAt: base, ReceivedAt: base, Values: map[string]float64{"cpu.utilization": 10}, Validity: map[string]string{"cpu.utilization": "valid"}},
		{ServerID: serverID, CollectorEpoch: "epoch-local-0001", Sequence: 1, ObservedAt: base.Add(20 * time.Second), ReceivedAt: base.Add(20 * time.Second), Values: map[string]float64{"cpu.utilization": 20}, Validity: map[string]string{"cpu.utilization": "valid"}},
		{ServerID: serverID, CollectorEpoch: "epoch-local-0001", Sequence: 2, ObservedAt: base.Add(40 * time.Second), ReceivedAt: base.Add(40 * time.Second), Values: map[string]float64{"cpu.utilization": 30}, Validity: map[string]string{"cpu.utilization": "valid"}},
		{ServerID: serverID, CollectorEpoch: "epoch-local-0001", Sequence: 3, ObservedAt: base.Add(70 * time.Second), ReceivedAt: base.Add(70 * time.Second), Values: map[string]float64{"cpu.utilization": 40}, Validity: map[string]string{"cpu.utilization": "valid"}},
	}, 60)
	for _, rollup := range rollups {
		if rollup.Metric == "cpu.utilization" && rollup.BucketStart.Equal(base.Add(time.Minute).Truncate(time.Minute)) {
			if rollup.ObservedSeconds != 60 || rollup.WeightedMean == nil || *rollup.WeightedMean < 23.333 || *rollup.WeightedMean > 23.334 {
				t.Fatalf("gauge interval was not carried across bucket boundary: %#v", rollup)
			}
			return
		}
	}
	t.Fatalf("target gauge bucket was not persisted: %#v", rollups)
}

func TestCoverageIncludesValidityOnlyMetricsAndGaps(t *testing.T) {
	samples := []contracts.MetricSample{{
		Values:   map[string]float64{"memory.used_percent": 10},
		Validity: map[string]string{"memory.used_percent": "valid", "cpu.utilization": "unavailable"},
	}}
	coverage := CoverageForSamples(samples, nil, []contracts.CoverageGap{{CollectorEpoch: "epoch-local-0001", FromSequence: 1, ToSequence: 2, Reason: "transport-disconnect"}})
	if coverage["cpu.utilization"] != 0 || coverage["memory.used_percent"] != 0.5 {
		t.Fatalf("coverage lost unavailable or gap state: %#v", coverage)
	}
}

func TestRollupsTreatExplicitNoneTimestampUncertaintyAsCertain(t *testing.T) {
	serverID := contracts.ServerID("server-local-0001")
	base := time.Date(2026, 9, 9, 10, 0, 10, 0, time.UTC)
	rollups := BuildRollups(serverID, []contracts.MetricSample{
		{ServerID: serverID, CollectorEpoch: "epoch-local-0001", Sequence: 0, ObservedAt: base, ReceivedAt: base, TimestampUncertainty: "none", Values: map[string]float64{"cpu.utilization": 20}, Counters: map[string]string{"net.rx": "100"}, Validity: map[string]string{"cpu.utilization": "valid", "net.rx": "valid"}},
		{ServerID: serverID, CollectorEpoch: "epoch-local-0001", Sequence: 1, ObservedAt: base.Add(20 * time.Second), ReceivedAt: base.Add(20 * time.Second), TimestampUncertainty: "none", Values: map[string]float64{"cpu.utilization": 40}, Counters: map[string]string{"net.rx": "160"}, Validity: map[string]string{"cpu.utilization": "valid", "net.rx": "valid"}},
	}, 60)
	for _, rollup := range rollups {
		if rollup.Metric == "cpu.utilization" && rollup.Coverage != "complete" {
			t.Fatalf("explicit uncertainty=none reduced gauge coverage: %#v", rollup)
		}
		if rollup.Metric == "net.rx" && (rollup.Coverage != "complete" || rollup.CounterDelta != "60") {
			t.Fatalf("explicit uncertainty=none reduced counter coverage: %#v", rollup)
		}
	}
}

func TestIntegerCapacityCountersRollUpAsGauges(t *testing.T) {
	serverID := contracts.ServerID("server-local-0001")
	base := time.Date(2026, 9, 9, 10, 0, 10, 0, time.UTC)
	rollups := BuildRollups(serverID, []contracts.MetricSample{
		{ServerID: serverID, CollectorEpoch: "epoch-local-0001", Sequence: 0, ObservedAt: base, ReceivedAt: base, Counters: map[string]string{"memory.used_bytes": "9007199254740993", "disk.root.total_bytes": "1000"}, Validity: map[string]string{"memory.used_bytes": "valid", "disk.root.total_bytes": "valid"}},
		{ServerID: serverID, CollectorEpoch: "epoch-local-0001", Sequence: 1, ObservedAt: base.Add(20 * time.Second), ReceivedAt: base.Add(20 * time.Second), Counters: map[string]string{"memory.used_bytes": "9007199254741093", "disk.root.total_bytes": "1100"}, Validity: map[string]string{"memory.used_bytes": "valid", "disk.root.total_bytes": "valid"}},
	}, 60)
	if len(rollups) != 2 {
		t.Fatalf("capacity metrics were not represented as gauges: %#v", rollups)
	}
	for _, rollup := range rollups {
		if rollup.CounterDelta != "" || rollup.Minimum == nil || rollup.Maximum == nil || rollup.WeightedMean == nil {
			t.Fatalf("capacity metric was treated as a counter: %#v", rollup)
		}
	}
}

func TestIngestRecomputesLateSampleEndpointBucket(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	if err := store.UpsertServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 9, 10, 0, 50, 0, time.UTC)
	first := testNodeSample(0, base, 1, "100")
	endpoint := testNodeSample(2, base.Add(2*time.Hour), 2, "300")
	if _, err := store.IngestNodeBatch(ctx, server.ID, []contracts.NodeMetricSample{first, endpoint}, nil, base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	late := testNodeSample(1, base.Add(20*time.Second), 3, "200")
	if _, err := store.IngestNodeBatch(ctx, server.ID, []contracts.NodeMetricSample{late}, nil, base.Add(2*time.Hour+time.Second)); err != nil {
		t.Fatal(err)
	}
	rollups, _, _, err := store.QueryRollups(ctx, server.ID, base.Add(2*time.Hour).Truncate(time.Minute), base.Add(2*time.Hour+time.Minute), 60, MaxPageItems, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, rollup := range rollups {
		if rollup.Metric == "net.billing.rx_bytes" {
			if rollup.Coverage != "complete" || rollup.CounterDelta != "100" {
				t.Fatalf("late sample did not recompute endpoint bucket: %#v", rollup)
			}
			return
		}
	}
	t.Fatalf("endpoint bucket rollup was not persisted: %#v", rollups)
}

func TestIngestAcknowledgesDurableSamplesWhenRollupsArePressured(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "pressure.db")
	store, err := OpenStore(ctx, dbPath, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	if err := store.UpsertServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	usage, err := store.StorageBytes()
	if err != nil {
		t.Fatal(err)
	}
	store.maxBytes = usage + 1
	base := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	result, err := store.IngestNodeBatch(ctx, server.ID, []contracts.NodeMetricSample{testNodeSample(0, base, 1, "100")}, nil, base.Add(time.Second))
	if err != nil || result.Inserted != 1 || !result.RollupsPending {
		t.Fatalf("durable ingest was reported failed under rollup pressure: result=%#v err=%v", result, err)
	}
	page, err := store.QueryMetrics(ctx, server.ID, base.Add(-time.Minute), base.Add(time.Minute), MaxPageItems, "")
	if err != nil || len(page.Samples) != 1 {
		t.Fatalf("durable raw sample was not retained: %#v err=%v", page, err)
	}
	store.maxBytes = 1 << 62
	rollups, _, _, err := store.QueryRollups(ctx, server.ID, base.Truncate(time.Minute), base.Add(time.Minute), 60, MaxPageItems, "")
	if err != nil {
		t.Fatalf("queued rollup retry failed: %v", err)
	}
	if len(rollups) == 0 {
		t.Fatalf("queued rollup was not recovered after storage headroom returned")
	}
}

func TestTrafficPeriodCurrentUpdateSurvivesStoragePressure(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "traffic-pressure.db")
	store, err := OpenStore(ctx, dbPath, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	if err := store.UpsertServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	usage, err := store.StorageBytes()
	if err != nil {
		t.Fatal(err)
	}
	store.maxBytes = usage + 1
	now := time.Now().UTC()
	period := contracts.TrafficPeriod{Scope: "host", From: now.Add(-time.Hour), To: now.Add(time.Hour), Timezone: "UTC", Direction: "combined", CountedBytes: 123, Continuity: "complete"}
	if err := store.UpsertTrafficPeriod(ctx, server.ID, period); err != nil {
		t.Fatalf("current billing period was rejected under history pressure: %v", err)
	}
}

func TestIngestMaterializesPersistedRollups(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	if err := store.UpsertServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 9, 10, 0, 50, 0, time.UTC)
	samples := []contracts.NodeMetricSample{
		testNodeSample(0, base, 0, "100"),
		testNodeSample(1, base.Add(20*time.Second), 0, "160"),
		testNodeSample(2, base.Add(40*time.Second), 0, "200"),
	}
	if _, err := store.IngestNodeBatch(ctx, server.ID, samples, nil, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	rollups, next, truncated, err := store.QueryRollups(ctx, server.ID, base.Truncate(time.Minute), base.Add(2*time.Minute), 60, MaxPageItems, "")
	if err != nil || next != "" || truncated {
		t.Fatalf("persisted rollup query failed: %#v next=%q truncated=%v err=%v", rollups, next, truncated, err)
	}
	var found bool
	for _, rollup := range rollups {
		if rollup.Metric == "net.billing.rx_bytes" && rollup.BucketStart.Equal(base.Add(time.Minute).Truncate(time.Minute)) {
			found = rollup.Coverage == "complete" && rollup.CounterDelta == "100"
		}
	}
	if !found {
		t.Fatalf("ingestion did not materialize the cross-bucket counter rollup: %#v", rollups)
	}
	if _, err := store.IngestNodeBatch(ctx, server.ID, nil, []contracts.CoverageGap{{CollectorEpoch: "epoch-local-0001", FromSequence: 1, ToSequence: 2, Reason: "transport-disconnect"}}, base.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	rollups, _, _, err = store.QueryRollups(ctx, server.ID, base.Truncate(time.Minute), base.Add(2*time.Minute), 60, MaxPageItems, "")
	if err != nil {
		t.Fatal(err)
	}
	invalidated := false
	for _, rollup := range rollups {
		if rollup.Metric == "net.billing.rx_bytes" && rollup.BucketStart.Equal(base.Add(time.Minute).Truncate(time.Minute)) {
			invalidated = rollup.Coverage == "uncertain" && rollup.CounterDelta == ""
		}
	}
	if !invalidated {
		t.Fatalf("gap-only ingestion did not invalidate the persisted counter rollup: %#v", rollups)
	}
}

func TestAPIRequiresTokenAndBoundsQueries(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertServer(ctx, testServer()); err != nil {
		t.Fatal(err)
	}
	api, err := NewAPI(store, "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/servers", nil)
	recorder := httptest.NewRecorder()
	api.Handler().ServeHTTP(recorder, request)
	response := recorder.Result()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token status: %d", response.StatusCode)
	}
	_ = response.Body.Close()
	request = httptest.NewRequest(http.MethodGet, "/api/v1/servers/server-local-0001/metrics?from=2026-09-09T00:00:00Z&to=2026-09-10T00:00:00Z&limit=201", nil)
	request.Header.Set("Authorization", "Bearer secret-token")
	recorder = httptest.NewRecorder()
	api.Handler().ServeHTTP(recorder, request)
	response = recorder.Result()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unbounded limit status: %d", response.StatusCode)
	}
	_ = response.Body.Close()
	request = httptest.NewRequest(http.MethodGet, "/api/v1/servers/server-local-0001", nil)
	request.Header.Set("Authorization", "Bearer secret-token")
	recorder = httptest.NewRecorder()
	api.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("server detail status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	_ = recorder.Result().Body.Close()
	request = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recorder = httptest.NewRecorder()
	api.Handler().ServeHTTP(recorder, request)
	response = recorder.Result()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status: %d", response.StatusCode)
	}
	_ = response.Body.Close()
	if !strings.Contains(response.Header.Get("Content-Type"), "application/json") {
		t.Fatal("health response is not JSON")
	}
}

func TestAPIReturnsPersistedRollupsForRollupResolution(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	if err := store.UpsertServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	observed := time.Date(2026, 9, 9, 10, 0, 10, 0, time.UTC)
	minimum, maximum, mean := 20.0, 20.0, 20.0
	if err := store.PutRollups(ctx, []Rollup{{ServerID: server.ID, Metric: "cpu.utilization", BucketStart: observed.Truncate(time.Minute), BucketSeconds: 60, SampleCount: 1, ObservedSeconds: 1, Minimum: &minimum, Maximum: &maximum, WeightedMean: &mean, Coverage: "complete"}}); err != nil {
		t.Fatal(err)
	}
	api, err := NewAPI(store, "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/servers/server-local-0001/metrics?from=2026-09-09T09:59:00Z&to=2026-09-09T10:02:00Z&resolution=minute", nil)
	request.Header.Set("Authorization", "Bearer secret-token")
	recorder := httptest.NewRecorder()
	api.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("rollup query status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	var page MetricPage
	if err := json.NewDecoder(recorder.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Samples) != 0 || len(page.Rollups) != 1 || page.Rollups[0].Metric != "cpu.utilization" || page.Coverage["cpu.utilization"] != 1 {
		t.Fatalf("API did not return persisted rollup: %#v", page)
	}
}

func TestAPILiveTailReadsOnlyRegisteredSources(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertServer(ctx, testServer()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "service.log")
	if err := os.WriteFile(path, []byte("2026-09-09T10:00:00Z [INFO] live\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterLogSource(ctx, LogSource{ServerID: testServer().ID, ID: "service", Label: "Service", Path: path}); err != nil {
		t.Fatal(err)
	}
	api, err := NewAPI(store, "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/servers/server-local-0001/logs/live?source=service&max_duration_seconds=1", nil)
	request.Header.Set("Authorization", "Bearer secret-token")
	recorder := httptest.NewRecorder()
	api.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"sequence":"`) || !strings.Contains(recorder.Header().Get("Content-Type"), "application/x-ndjson") {
		t.Fatalf("unexpected live tail response: status=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
}

func TestAPILiveTailFindsSourceBeyondFirstPage(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	if err := store.UpsertServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	logDir := t.TempDir()
	for index := 0; index < MaxPageItems; index++ {
		id := fmt.Sprintf("source-%03d", index)
		if err := store.RegisterLogSource(ctx, LogSource{ServerID: server.ID, ID: id, Label: id, Path: filepath.Join(logDir, id+".log")}); err != nil {
			t.Fatal(err)
		}
	}
	lastPath := filepath.Join(logDir, "zz-last.log")
	if err := os.WriteFile(lastPath, []byte("2026-09-09T10:00:00Z [INFO] last\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterLogSource(ctx, LogSource{ServerID: server.ID, ID: "zz-last", Label: "Last", Path: lastPath}); err != nil {
		t.Fatal(err)
	}
	api, err := NewAPI(store, "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/servers/server-local-0001/logs/live?source=zz-last&max_duration_seconds=1", nil)
	request.Header.Set("Authorization", "Bearer secret-token")
	recorder := httptest.NewRecorder()
	api.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "last") {
		t.Fatalf("source after first page was not tailable: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestServerListingAllowsNeverConnectedHeartbeat(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	server.ConnectionState = "never-connected"
	server.FreshnessState = "unknown"
	server.LastHeartbeat = nil
	if err := store.UpsertServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	page, err := store.QueryServerPage(ctx, 10, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].LastHeartbeat != nil {
		t.Fatalf("NULL heartbeat was not decoded as absent: %#v err=%v", page, err)
	}
}

func TestCoverageGapPaginationHasIndependentCursor(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	if err := store.UpsertServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	gaps := make([]contracts.CoverageGap, 0, MaxPageItems+1)
	for index := 0; index < MaxPageItems+1; index++ {
		gaps = append(gaps, contracts.CoverageGap{CollectorEpoch: contracts.CollectorEpoch(fmt.Sprintf("epoch-%03d", index)), FromSequence: 0, ToSequence: 1, Reason: "transport-disconnect"})
	}
	for _, gap := range gaps {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO coverage_gaps(server_id,collector_epoch,from_sequence,to_sequence,reason) VALUES(?,?,?,?,?)`, string(server.ID), string(gap.CollectorEpoch), "0", "1", gap.Reason); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.QueryMetricsWithGapCursor(ctx, server.ID, time.Time{}, time.Time{}, MaxPageItems, "", "")
	if err != nil || len(first.Gaps) != MaxPageItems || first.GapsNextCursor == "" || !first.Truncated {
		t.Fatalf("coverage gaps were not independently paginated: %#v err=%v", first, err)
	}
	second, err := store.QueryMetricsWithGapCursor(ctx, server.ID, time.Time{}, time.Time{}, MaxPageItems, "", first.GapsNextCursor)
	if err != nil || len(second.Gaps) != 1 || second.Gaps[0].CollectorEpoch == first.Gaps[0].CollectorEpoch || second.GapsNextCursor != "" {
		t.Fatalf("coverage gap cursor repeated or lost the final gap: %#v err=%v", second, err)
	}
}

func TestMetricJSONFieldsDecodeIndependentlyAndLogsRedactAtStoreBoundary(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	if err := store.UpsertServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	observed := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	if _, err := store.IngestNodeBatch(ctx, server.ID, []contracts.NodeMetricSample{{ServerID: server.ID, CollectorEpoch: "epoch-local-0001", Sequence: 0, ObservedAt: observed, Values: map[string]float64{}, Counters: map[string]string{}, Units: map[string]string{}, Validity: map[string]string{}}}, nil, observed.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	page, err := store.QueryMetrics(ctx, server.ID, observed.Add(-time.Minute), observed.Add(time.Minute), 10, "")
	if err != nil || len(page.Samples) != 1 || page.Samples[0].Values == nil || page.Samples[0].Counters == nil || page.Samples[0].Units == nil || page.Samples[0].Validity == nil {
		t.Fatalf("metric JSON fields were merged or lost: %#v err=%v", page, err)
	}
	logDir := t.TempDir()
	if err := store.RegisterLogSource(ctx, LogSource{ServerID: server.ID, ID: "service", Label: "Service", Path: filepath.Join(logDir, "service.log")}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertLogEntries(ctx, []LogEntry{{ServerID: server.ID, SourceID: "service", Cursor: "1", Timestamp: observed, Text: "password=raw-secret"}}); err != nil {
		t.Fatal(err)
	}
	logs, err := store.QueryLogs(ctx, server.ID, "service", time.Time{}, time.Time{}, 10, "")
	if err != nil || len(logs.Entries) != 1 || strings.Contains(logs.Entries[0].Text, "raw-secret") || !logs.Entries[0].Redacted {
		t.Fatalf("store boundary did not redact log text: %#v err=%v", logs, err)
	}
	if _, err := store.InsertLogEntries(ctx, []LogEntry{{ServerID: server.ID, SourceID: "service", Cursor: "2", Timestamp: observed.Add(time.Second), Severity: "ERROR", Text: "disk failed"}}); err != nil {
		t.Fatal(err)
	}
	filtered, err := store.QueryLogsFiltered(ctx, server.ID, "service", time.Time{}, time.Time{}, "ERROR", "failed", 10, "")
	if err != nil || len(filtered.Entries) != 1 || filtered.Entries[0].Text != "disk failed" {
		t.Fatalf("log severity/text filters failed: %#v err=%v", filtered, err)
	}
}

func TestPruneHonorsTrafficAgeAndLogBudget(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertServer(ctx, testServer()); err != nil {
		t.Fatal(err)
	}
	logDir := t.TempDir()
	if err := store.RegisterLogSource(ctx, LogSource{ServerID: testServer().ID, ID: "service", Label: "Service", Path: filepath.Join(logDir, "service.log")}); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterLogSource(ctx, LogSource{ServerID: testServer().ID, ID: "system", Label: "System", Path: filepath.Join(logDir, "system.log")}); err != nil {
		t.Fatal(err)
	}
	sourcePage, err := store.QueryLogSourcePage(ctx, testServer().ID, 1, "")
	if err != nil || !sourcePage.Truncated || sourcePage.NextCursor == "" || len(sourcePage.Items) != 1 {
		t.Fatalf("log-source cursor page was not bounded: %#v err=%v", sourcePage, err)
	}
	nextSourcePage, err := store.QueryLogSourcePage(ctx, testServer().ID, 1, sourcePage.NextCursor)
	if err != nil || len(nextSourcePage.Items) != 1 || nextSourcePage.Items[0].ID == sourcePage.Items[0].ID {
		t.Fatalf("log-source cursor skipped or duplicated: %#v err=%v", nextSourcePage, err)
	}
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	for _, timestamp := range []time.Time{now.Add(-48 * time.Hour), now.Add(-time.Hour)} {
		if _, err := store.InsertLogEntries(ctx, []LogEntry{{ServerID: testServer().ID, SourceID: "service", Cursor: timestamp.Format(time.RFC3339Nano), Timestamp: timestamp, Text: "abcd"}}); err != nil {
			t.Fatal(err)
		}
	}
	oldPeriod := contracts.TrafficPeriod{Scope: "host", From: now.Add(-96 * time.Hour), To: now.Add(-95 * time.Hour), Timezone: "UTC", Direction: "combined", Continuity: "complete"}
	if err := store.UpsertTrafficPeriod(ctx, testServer().ID, oldPeriod); err != nil {
		t.Fatal(err)
	}
	oldSample := testNodeSample(9, now.Add(-48*time.Hour), 10, "10")
	if _, err := store.IngestNodeBatch(ctx, testServer().ID, []contracts.NodeMetricSample{oldSample}, nil, now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	acknowledgeAllQueuedSamples(t, ctx, store)
	stats, err := store.Prune(ctx, now, RetentionPolicy{TrafficPeriodAge: time.Hour, LogAge: 24 * time.Hour, LogMaxBytes: 5, BatchSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if stats.TrafficPeriods != 1 || stats.MetricSamples != 1 || stats.LogEntries < 1 {
		t.Fatalf("retention limits were not applied: %#v", stats)
	}
	page, err := store.QueryMetrics(ctx, testServer().ID, time.Time{}, time.Time{}, 200, "")
	if err != nil || len(page.Gaps) != 1 || page.Gaps[0].Reason != "retention" {
		t.Fatalf("retention deletion did not leave an explicit gap: %#v err=%v", page, err)
	}
}

func TestPruneDeletesTheRowsUsedToCreateRetentionGaps(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	if err := store.UpsertServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	// Insert the later observation first so rowid order differs from the
	// observed_at order used by retention selection.
	late := testNodeSample(1, now, 1, "200")
	early := testNodeSample(0, now.Add(-72*time.Hour), 1, "100")
	if _, err := store.IngestNodeBatch(ctx, server.ID, []contracts.NodeMetricSample{late, early}, nil, now); err != nil {
		t.Fatal(err)
	}
	acknowledgeAllQueuedSamples(t, ctx, store)
	stats, err := store.Prune(ctx, now, RetentionPolicy{FullResolutionAge: time.Hour, BatchSize: 1})
	if err != nil || stats.MetricSamples != 1 {
		t.Fatalf("unexpected retention result: %#v err=%v", stats, err)
	}
	page, err := store.QueryMetrics(ctx, server.ID, time.Time{}, time.Time{}, 10, "")
	if err != nil || len(page.Samples) != 1 || page.Samples[0].Sequence != 1 || len(page.Gaps) != 1 || page.Gaps[0].FromSequence != 0 {
		t.Fatalf("retention deleted a different row than the one used for the gap: %#v err=%v", page, err)
	}
}

func TestPruneRecordsOnlyContiguousRetentionGaps(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	if err := store.UpsertServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	old0 := testNodeSample(0, now.Add(-72*time.Hour), 1, "100")
	recent1 := testNodeSample(1, now, 2, "200")
	old2 := testNodeSample(2, now.Add(-71*time.Hour), 3, "300")
	if _, err := store.IngestNodeBatch(ctx, server.ID, []contracts.NodeMetricSample{old0, recent1, old2}, nil, now); err != nil {
		t.Fatal(err)
	}
	acknowledgeAllQueuedSamples(t, ctx, store)
	if _, err := store.Prune(ctx, now, RetentionPolicy{FullResolutionAge: time.Hour, BatchSize: 200}); err != nil {
		t.Fatal(err)
	}
	page, err := store.QueryMetrics(ctx, server.ID, time.Time{}, time.Time{}, MaxPageItems, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Samples) != 1 || page.Samples[0].Sequence != 1 || len(page.Gaps) != 2 {
		t.Fatalf("non-contiguous retention rows were merged: samples=%#v gaps=%#v", page.Samples, page.Gaps)
	}
	for _, gap := range page.Gaps {
		if gap.ToSequence-gap.FromSequence != 1 {
			t.Fatalf("retention gap swallowed a retained sequence: %#v", gap)
		}
	}
}

func TestMetricGapPageIsScopedToRequestedTime(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	if err := store.UpsertServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	oldStart := testNodeSample(0, old, 1, "10")
	oldEnd := testNodeSample(2, old.Add(time.Minute), 2, "20")
	if _, err := store.IngestNodeBatch(ctx, server.ID, []contracts.NodeMetricSample{oldStart, oldEnd}, []contracts.CoverageGap{{CollectorEpoch: oldStart.CollectorEpoch, FromSequence: 1, ToSequence: 2, Reason: "transport-disconnect"}}, old.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	recent := old.Add(24 * time.Hour)
	if _, err := store.IngestNodeBatch(ctx, server.ID, []contracts.NodeMetricSample{testNodeSample(3, recent, 3, "30")}, nil, recent.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	page, err := store.QueryMetrics(ctx, server.ID, recent.Add(-time.Minute), recent.Add(time.Minute), 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Samples) != 1 || len(page.Gaps) != 0 {
		t.Fatalf("old gap leaked into recent query: %#v", page)
	}
}

func TestEnsureServerPreservesStateAndTouchUpdatesFreshness(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	server.Name = "Owner label"
	server.ConfigurationRevision = 9
	if err := store.UpsertServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	bootstrap := testServer()
	bootstrap.Name = "Local server"
	bootstrap.ConfigurationRevision = 0
	bootstrap.ConnectionState = "never-connected"
	bootstrap.FreshnessState = "unknown"
	if err := store.EnsureServer(ctx, bootstrap); err != nil {
		t.Fatal(err)
	}
	page, err := store.QueryServerPage(ctx, 10, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].Name != "Owner label" || page.Items[0].ConfigurationRevision != 9 {
		t.Fatalf("ensure overwrote durable server state: %#v err=%v", page, err)
	}
	heartbeat := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	if err := store.TouchServer(ctx, server.ID, heartbeat); err != nil {
		t.Fatal(err)
	}
	page, err = store.QueryServerPage(ctx, 10, "")
	if err != nil || page.Items[0].ConnectionState != "connected" || page.Items[0].FreshnessState != "fresh" || page.Items[0].LastHeartbeat == nil {
		t.Fatalf("touch did not update freshness: %#v err=%v", page, err)
	}
}

func TestHighestContiguousCoverageIncludesExplicitGaps(t *testing.T) {
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
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	makeSample := func(sequence uint64) contracts.NodeMetricSample {
		return contracts.NodeMetricSample{ServerID: server.ID, CollectorEpoch: "epoch-frontier-01", Sequence: sequence, ObservedAt: now, Values: map[string]float64{"cpu.utilization": 1}}
	}
	if _, err := store.IngestNodeBatch(ctx, server.ID, []contracts.NodeMetricSample{makeSample(0), makeSample(4)}, []contracts.CoverageGap{{CollectorEpoch: "epoch-frontier-01", FromSequence: 1, ToSequence: 4, Reason: "spool-eviction"}}, now); err != nil {
		t.Fatal(err)
	}
	through, found, err := store.HighestContiguousCoverageSequence(ctx, server.ID, "epoch-frontier-01")
	if err != nil || !found || through != 4 {
		t.Fatalf("frontier=%d found=%v err=%v", through, found, err)
	}
	if _, err := store.IngestNodeBatch(ctx, server.ID, []contracts.NodeMetricSample{makeSample(9)}, []contracts.CoverageGap{{CollectorEpoch: "epoch-frontier-01", FromSequence: 5, ToSequence: 8, Reason: "spool-eviction"}}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	through, found, err = store.HighestContiguousCoverageSequence(ctx, server.ID, "epoch-frontier-01")
	if err != nil || !found || through != 7 {
		t.Fatalf("frontier crossed missing sequence: %d found=%v err=%v", through, found, err)
	}
}

func TestTrafficAllowanceCountIsBoundedTransactionally(t *testing.T) {
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
	for index := 0; index < MaxTrafficAllowances; index++ {
		allowance := contracts.TrafficAllowance{Scope: fmt.Sprintf("scope-%02d", index), Direction: "combined", AllowanceBytes: 1000, ResetDay: 1, Timezone: "UTC", WarningPercentages: []uint8{80}}
		if err := store.UpsertTrafficAllowance(ctx, server.ID, allowance); err != nil {
			t.Fatalf("allowance %d: %v", index, err)
		}
	}
	if err := store.UpsertTrafficAllowance(ctx, server.ID, contracts.TrafficAllowance{Scope: "scope-overflow", Direction: "combined", AllowanceBytes: 1000, ResetDay: 1, Timezone: "UTC", WarningPercentages: []uint8{80}}); err == nil || !errors.Is(err, ErrTrafficAllowanceLimit) {
		t.Fatalf("expected allowance limit, got %v", err)
	}
	// Updating an existing identity remains permitted at the cap.
	if err := store.UpsertTrafficAllowance(ctx, server.ID, contracts.TrafficAllowance{Scope: "scope-00", Direction: "combined", AllowanceBytes: 2000, ResetDay: 1, Timezone: "UTC", WarningPercentages: []uint8{80}}); err != nil {
		t.Fatalf("existing allowance update failed at cap: %v", err)
	}
}

func TestRetentionHandlesMaxUintSequenceWithoutCrossingMissingPrefix(t *testing.T) {
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
	max := ^uint64(0)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	wire := func(sequence uint64) contracts.NodeMetricSample {
		return contracts.NodeMetricSample{ServerID: server.ID, CollectorEpoch: "epoch-max-retention-01", Sequence: sequence, ObservedAt: old, Values: map[string]float64{"cpu.utilization": 1}}
	}
	if _, err := store.IngestNodeBatch(ctx, server.ID, []contracts.NodeMetricSample{wire(max - 1), wire(max)}, []contracts.CoverageGap{{CollectorEpoch: "epoch-max-retention-01", FromSequence: 0, ToSequence: max, Reason: "spool-eviction"}}, now); err != nil {
		t.Fatal(err)
	}
	acknowledgeAllQueuedSamples(t, ctx, store)
	if ready, err := store.SequenceFrontierReady(ctx, server.ID, "epoch-max-retention-01", max); err != nil || !ready {
		t.Fatalf("max sequence was not initially covered: ready=%v err=%v", ready, err)
	}
	if _, err := store.Prune(ctx, now, RetentionPolicy{FullResolutionAge: time.Hour, TrafficPeriodAge: 2 * 365 * 24 * time.Hour, BatchSize: 200}); err != nil {
		t.Fatal(err)
	}
	if ready, err := store.SequenceFrontierReady(ctx, server.ID, "epoch-max-retention-01", max-1); err != nil || !ready {
		t.Fatalf("retained prefix became unavailable: ready=%v err=%v", ready, err)
	}
	if ready, err := store.SequenceFrontierReady(ctx, server.ID, "epoch-max-retention-01", max); err != nil || ready {
		t.Fatalf("deleted max sequence was still acknowledged: ready=%v err=%v", ready, err)
	}
	through, found, err := store.HighestContiguousCoverageSequence(ctx, server.ID, "epoch-max-retention-01")
	if err != nil || !found || through != max-1 {
		t.Fatalf("max retention frontier=%d found=%v err=%v", through, found, err)
	}
}

func TestIsBusyErrorDetection(t *testing.T) {
	if IsBusyError(nil) {
		t.Fatal("nil error must not be busy error")
	}
	if IsBusyError(errors.New("something else")) {
		t.Fatal("generic error must not be busy error")
	}
	if !IsBusyError(errors.New("payesh: database is locked (5) (SQLITE_BUSY)")) {
		t.Fatal("SQLITE_BUSY string must be detected as busy error")
	}
	if !IsBusyError(errors.New("database table is locked: SQLITE_LOCKED")) {
		t.Fatal("SQLITE_LOCKED string must be detected as busy error")
	}
}
