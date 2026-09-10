package traffic

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/alerts"
	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func allowance(reset uint8, timezone string) contracts.TrafficAllowance {
	return contracts.TrafficAllowance{Scope: "host", Direction: "combined", AllowanceBytes: 1000, ResetDay: reset, Timezone: timezone, WarningPercentages: []uint8{80, 90, 100}}
}

func TestForecastUsesCadenceIntervalCrossingMinimumWindowEdge(t *testing.T) {
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	period := contracts.TrafficPeriod{Scope: "host", From: base, To: base.Add(31 * 24 * time.Hour), Timezone: "UTC", AllowanceBytes: 1 << 30, Direction: "outbound", Continuity: "complete"}
	asOf := base.Add(48 * time.Hour)
	windowStart := asOf.Add(-MinimumForecast)
	for _, cadence := range []time.Duration{5 * time.Second, 10 * time.Second} {
		t.Run(cadence.String(), func(t *testing.T) {
			observations := make([]contracts.TrafficObservation, 0, int(MinimumForecast/cadence)+2)
			counter := uint64(0)
			for at := windowStart.Add(-cadence / 2); at.Before(asOf); at = at.Add(cadence) {
				observations = append(observations, contracts.TrafficObservation{ObservedAt: at, OutboundBytes: counter, Valid: true, Coverage: 1})
				counter++
			}
			observations = append(observations, contracts.TrafficObservation{ObservedAt: asOf, OutboundBytes: counter, Valid: true, Coverage: 1})
			forecast, err := forecastWithObservedBytesWindow(period, observations, asOf, nil, &windowStart)
			if err != nil {
				t.Fatal(err)
			}
			if !forecast.Available || forecast.UsableDurationSeconds != int64(MinimumForecast/time.Second) || forecast.Coverage != 1 {
				t.Fatalf("%s cadence-edge forecast=%+v", cadence, forecast)
			}
		})
	}
}

func TestCollectorRebootSequenceZeroMarksTrafficContinuityUncertain(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-reboot-boundary-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Reboot", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	configured := allowance(1, "UTC")
	if err := store.UpsertTrafficAllowance(ctx, serverID, configured); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.January, 10, 0, 0, 0, 0, time.UTC)
	sample := func(epoch contracts.CollectorEpoch, at time.Time, counter string) contracts.MetricSample {
		return contracts.MetricSample{ServerID: serverID, CollectorEpoch: epoch, Sequence: 0, ObservedAt: at, ReceivedAt: at, Values: map[string]float64{}, Counters: map[string]string{"net.billing.rx_bytes": counter, "net.billing.tx_bytes": "0"}, Validity: map[string]string{"net.billing.rx_bytes": "valid", "net.billing.tx_bytes": "valid"}}
	}
	first := sample("collector-before-reboot-01", base, "100")
	if _, err := store.IngestSamples(ctx, serverID, []contracts.MetricSample{first}, nil); err != nil {
		t.Fatal(err)
	}
	period, err := manager.AddUsageFromStoredAllowance(ctx, first, nil, configured)
	if err != nil || period.Continuity != "complete" {
		t.Fatalf("first epoch baseline=%+v err=%v", period, err)
	}
	second := sample("collector-after-reboot-001", base.Add(time.Hour), "3")
	if _, err := store.IngestSamples(ctx, serverID, []contracts.MetricSample{second}, nil); err != nil {
		t.Fatal(err)
	}
	period, err = manager.AddUsageFromStoredAllowance(ctx, second, nil, configured)
	if err != nil {
		t.Fatal(err)
	}
	if period.Continuity != "uncertain" || period.CountedBytes != 0 {
		t.Fatalf("reboot boundary was treated as continuous: %+v", period)
	}
	forecast, err := Forecast(period, nil, second.ObservedAt)
	if err != nil || forecast.Available || forecast.Reason != "traffic_period_continuity_incomplete" {
		t.Fatalf("reboot-period forecast=%+v err=%v", forecast, err)
	}
}

func TestExplicitRestartAtomicallyClosesActivePeriod(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-explicit-handoff-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Handoff", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	configured := allowance(1, "UTC")
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	if _, err := manager.AddUsage(ctx, serverID, configured, base.Add(time.Hour), 10, "complete"); err != nil {
		t.Fatal(err)
	}
	restart := base.Add(10 * 24 * time.Hour)
	started, err := manager.ApplyAllowance(ctx, serverID, configured, restart, 0, "complete", true)
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.QueryTrafficPeriods(ctx, serverID, time.Time{}, time.Time{}, configured.Scope, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Periods) != 2 || !page.Periods[0].To.Equal(restart) || !page.Periods[1].From.Equal(restart) || !started.From.Equal(restart) {
		t.Fatalf("explicit period handoff=%+v started=%+v", page.Periods, started)
	}
	for index := 1; index < len(page.Periods); index++ {
		if page.Periods[index].From.Before(page.Periods[index-1].To) {
			t.Fatalf("overlapping periods after restart: %+v", page.Periods)
		}
	}
}

func TestQueuedSampleBeforeAllowanceCreationIsNeverRetroactivelyCharged(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-policy-after-sample-1")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Policy timing", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.January, 10, 0, 0, 0, 0, time.UTC)
	wire := func(sequence uint64, at time.Time, value string) contracts.MetricSample {
		return contracts.MetricSample{ServerID: serverID, CollectorEpoch: "policy-after-sample-epoch", Sequence: sequence, ObservedAt: at, ReceivedAt: at, Values: map[string]float64{}, Counters: map[string]string{"net.billing.rx_bytes": value, "net.billing.tx_bytes": "0"}, Validity: map[string]string{"net.billing.rx_bytes": "valid", "net.billing.tx_bytes": "valid"}}
	}
	if _, err := store.IngestSamples(ctx, serverID, []contracts.MetricSample{wire(0, base, "0"), wire(1, base.Add(time.Minute), "500")}, nil); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	configured := allowance(1, "UTC")
	createdAt := base.Add(2 * time.Minute)
	if _, err := manager.ApplyAllowance(ctx, serverID, configured, createdAt, 0, "complete", false); err != nil {
		t.Fatal(err)
	}
	processor, err := NewProcessor(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := processor.DrainPendingPostProcess(ctx); err != nil {
		t.Fatal(err)
	}
	period, found, err := store.GetActiveTrafficPeriod(ctx, serverID, configured.Scope, configured.Direction, createdAt)
	if err != nil || !found || period.CountedBytes != 0 {
		t.Fatalf("pre-policy sample was charged: period=%+v found=%v err=%v", period, found, err)
	}
	// A restarted worker sees the same durable decision and has no queued work
	// to reinterpret under the now-current allowance.
	restarted, err := NewProcessor(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.DrainPendingPostProcess(ctx); err != nil {
		t.Fatal(err)
	}
	period, found, err = store.GetActiveTrafficPeriod(ctx, serverID, configured.Scope, configured.Direction, createdAt)
	if err != nil || !found || period.CountedBytes != 0 {
		t.Fatalf("restart changed pre-policy decision: period=%+v found=%v err=%v", period, found, err)
	}
}

func TestQueuedSamplesUseAllowanceEffectiveBeforeLaterEdit(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-policy-edit-delay-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Policy edit", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.January, 10, 0, 0, 0, 0, time.UTC)
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	old := allowance(1, "UTC")
	old.Interfaces = []string{"eth0"}
	if _, err := manager.ApplyAllowance(ctx, serverID, old, base, 0, "complete", false); err != nil {
		t.Fatal(err)
	}
	wire := func(sequence uint64, at time.Time, oldValue, newValue string) contracts.MetricSample {
		return contracts.MetricSample{ServerID: serverID, CollectorEpoch: "policy-edit-delay-epoch", Sequence: sequence, ObservedAt: at, ReceivedAt: at, Values: map[string]float64{}, Counters: map[string]string{"net.eth0.rx_bytes": oldValue, "net.eth0.tx_bytes": "0", "net.eth1.rx_bytes": newValue, "net.eth1.tx_bytes": "0"}, Validity: map[string]string{"net.eth0.rx_bytes": "valid", "net.eth0.tx_bytes": "valid", "net.eth1.rx_bytes": "valid", "net.eth1.tx_bytes": "valid"}}
	}
	if _, err := store.IngestSamples(ctx, serverID, []contracts.MetricSample{wire(0, base.Add(time.Minute), "0", "0"), wire(1, base.Add(2*time.Minute), "100", "1000")}, nil); err != nil {
		t.Fatal(err)
	}
	proposed := old
	proposed.Interfaces = []string{"eth1"}
	editAt := base.Add(3 * time.Minute)
	if _, err := manager.ApplyAllowance(ctx, serverID, proposed, editAt, 0, "complete", true); err != nil {
		t.Fatal(err)
	}
	processor, err := NewProcessor(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := processor.DrainPendingPostProcess(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := store.QueryTrafficPeriods(ctx, serverID, time.Time{}, time.Time{}, old.Scope, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Periods) != 2 || page.Periods[0].CountedBytes != 100 || page.Periods[1].CountedBytes != 0 {
		t.Fatalf("delayed pre-edit samples used later interface policy: %+v", page.Periods)
	}
	restarted, err := NewProcessor(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.DrainPendingPostProcess(ctx); err != nil {
		t.Fatal(err)
	}
	again, err := store.QueryTrafficPeriods(ctx, serverID, time.Time{}, time.Time{}, old.Scope, 10, "")
	if err != nil || len(again.Periods) != 2 || again.Periods[0].CountedBytes != 100 || again.Periods[1].CountedBytes != 0 {
		t.Fatalf("retry/restart changed allowance ordering: %+v err=%v", again.Periods, err)
	}
}

func TestPeriodForClampsShortMonthsAndPreservesDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	period, err := PeriodFor(time.Date(2024, time.February, 15, 12, 0, 0, 0, loc), allowance(31, "America/New_York"))
	if err != nil {
		t.Fatal(err)
	}
	if got := period.From.In(loc); got.Day() != 31 || got.Month() != time.January {
		t.Fatalf("period start=%v", got)
	}
	if got := period.To.In(loc); got.Day() != 29 || got.Month() != time.February {
		t.Fatalf("period end=%v", got)
	}
	period, err = PeriodFor(time.Date(2024, time.February, 29, 12, 0, 0, 0, loc), allowance(31, "America/New_York"))
	if err != nil || period.From.In(loc).Day() != 29 {
		t.Fatalf("leap-month boundary=%v err=%v", period, err)
	}
	// The March boundary changes UTC offset; a fixed 30-day calculation would
	// produce the wrong elapsed duration.
	march, err := PeriodFor(time.Date(2024, time.March, 30, 12, 0, 0, 0, loc), allowance(31, "America/New_York"))
	if err != nil {
		t.Fatal(err)
	}
	if got := march.From.In(loc); got.Day() != 29 || got.Month() != time.February {
		t.Fatalf("march start=%v", got)
	}
	if got := march.To.In(loc); got.Day() != 31 || got.Month() != time.March {
		t.Fatalf("march end=%v", got)
	}
}

func TestAddUsageIsPersistentAndAtomic(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-traffic-0123")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Traffic", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)
	period, err := manager.AddUsage(ctx, serverID, allowance(1, "UTC"), at, 250, "complete")
	if err != nil {
		t.Fatal(err)
	}
	period, err = manager.AddUsage(ctx, serverID, allowance(1, "UTC"), at.Add(time.Hour), 125, "gap")
	if err != nil {
		t.Fatal(err)
	}
	if period.CountedBytes != 375 || period.Continuity != "gap" {
		t.Fatalf("period=%+v", period)
	}
	page, err := store.QueryTrafficPeriods(ctx, serverID, at.Add(-time.Hour), at.Add(time.Hour), "host", 10, "")
	if err != nil || len(page.Periods) != 1 || page.Periods[0].CountedBytes != 375 || page.Periods[0].Continuity != "gap" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}

func TestAddUsageDoesNotOverwriteConcurrentAllowanceEdit(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-traffic-config-race-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Config race", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	oldAllowance := allowance(1, "UTC")
	if err := store.UpsertTrafficAllowance(ctx, serverID, oldAllowance); err != nil {
		t.Fatal(err)
	}
	// This is the stale snapshot a processor may have obtained before the
	// configuration CAS transaction committed.
	snapshot := oldAllowance
	newAllowance := oldAllowance
	newAllowance.AllowanceBytes = 2000
	if err := store.UpsertTrafficAllowance(ctx, serverID, newAllowance); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)
	prior, err := (contracts.NodeMetricSample{ServerID: serverID, CollectorEpoch: "config-race-epoch-01", Sequence: 0, ObservedAt: at.Add(-time.Minute), Values: map[string]float64{}, Counters: map[string]string{"net.billing.rx_bytes": "0", "net.billing.tx_bytes": "0"}, Validity: map[string]string{"net.billing.rx_bytes": "valid", "net.billing.tx_bytes": "valid"}}).WithReceivedAt(at.Add(-time.Minute + time.Second))
	if err != nil {
		t.Fatal(err)
	}
	sample, err := (contracts.NodeMetricSample{ServerID: serverID, CollectorEpoch: "config-race-epoch-01", Sequence: 1, ObservedAt: at, Values: map[string]float64{}, Counters: map[string]string{"net.billing.rx_bytes": "100", "net.billing.tx_bytes": "0"}, Validity: map[string]string{"net.billing.rx_bytes": "valid", "net.billing.tx_bytes": "valid"}}).WithReceivedAt(at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	period, err := manager.AddUsageFromStoredAllowance(ctx, sample, &prior, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if period.AllowanceBytes != newAllowance.AllowanceBytes {
		t.Fatalf("usage used stale allowance: %+v", period)
	}
	persisted, found, err := store.GetTrafficAllowance(ctx, serverID, oldAllowance.Scope, oldAllowance.Direction)
	if err != nil || !found || persisted.AllowanceBytes != newAllowance.AllowanceBytes {
		t.Fatalf("concurrent allowance edit was overwritten: %+v found=%v err=%v", persisted, found, err)
	}
}

func TestScheduleChangeWaitsForExistingBoundary(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-schedule-0123")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Schedule", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	old := allowance(1, "UTC")
	newAllowance := allowance(15, "UTC")
	base := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	if _, err := manager.AddUsage(ctx, serverID, old, base, 100, "complete"); err != nil {
		t.Fatal(err)
	}
	active, err := manager.AddUsage(ctx, serverID, newAllowance, base.Add(time.Hour), 50, "complete")
	if err != nil {
		t.Fatal(err)
	}
	if active.From.Day() != 1 || active.AllowanceBytes != newAllowance.AllowanceBytes {
		t.Fatalf("schedule changed active period: %+v", active)
	}
	preview, err := PreviewChange(active, newAllowance, base.Add(time.Hour), false)
	if err != nil || preview.StartsAt.Day() != 1 || preview.Proposed.From.Day() != 1 || preview.Proposed.To.Day() != 15 {
		t.Fatalf("schedule preview=%+v err=%v", preview, err)
	}
	// The old Jan-1 period remains authoritative through Feb-1; the new
	// reset-day schedule starts at the first existing boundary after the edit.
	next, err := manager.AddUsage(ctx, serverID, newAllowance, time.Date(2026, 2, 2, 12, 0, 0, 0, time.UTC), 25, "complete")
	if err != nil {
		t.Fatal(err)
	}
	if next.From.Day() != 1 || next.To.Day() != 15 {
		t.Fatalf("new schedule did not take effect at boundary: %+v", next)
	}
	following, err := manager.AddUsage(ctx, serverID, newAllowance, time.Date(2026, 2, 16, 12, 0, 0, 0, time.UTC), 1, "complete")
	if err != nil || following.From.Day() != 15 {
		t.Fatalf("subsequent schedule boundary is wrong: %+v err=%v", following, err)
	}
}

func TestPreviewChangeReportsInterfaceSelectionChange(t *testing.T) {
	at := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	current := contracts.TrafficPeriod{Scope: "host", Interfaces: []string{"eth0"}, From: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), Timezone: "UTC", AllowanceBytes: 100, Direction: "combined", Continuity: "complete"}
	proposed := allowance(1, "UTC")
	proposed.Interfaces = []string{"eth1"}
	preview, err := PreviewChange(current, proposed, at, false)
	if err != nil {
		t.Fatal(err)
	}
	if preview.ScheduleEffect != "next_existing_boundary" || !preview.StartsAt.Equal(current.To) {
		t.Fatalf("interface change was not scheduled at boundary: %+v", preview)
	}
	current.Interfaces = []string{"eth0", "eth1"}
	proposed.Interfaces = []string{"eth1", "eth0"}
	preview, err = PreviewChange(current, proposed, at, false)
	if err != nil || preview.ScheduleEffect != "next_existing_boundary" || !preview.StartsAt.Equal(current.From) {
		t.Fatalf("interface reorder was treated as a schedule change: %+v err=%v", preview, err)
	}
}

func TestActivePeriodRestoresInterfacesForPreview(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-preview-active-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Preview", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	allowance := allowance(1, "UTC")
	allowance.Interfaces = []string{"eth0"}
	at := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	if _, err := manager.AddUsage(ctx, serverID, allowance, at, 1, "complete"); err != nil {
		t.Fatal(err)
	}
	current, found, err := store.GetActiveTrafficPeriod(ctx, serverID, allowance.Scope, allowance.Direction, at)
	if err != nil || !found || len(current.Interfaces) != 1 || current.Interfaces[0] != "eth0" {
		t.Fatalf("active period lost selected interfaces: %+v found=%v err=%v", current, found, err)
	}
	proposed := allowance
	proposed.Interfaces = []string{"eth0"}
	preview, err := PreviewChange(current, proposed, at, false)
	if err != nil || preview.ScheduleEffect != "next_existing_boundary" || !preview.StartsAt.Equal(current.From) {
		t.Fatalf("same active interface selection changed preview: %+v err=%v", preview, err)
	}
}

func TestTrafficPeriodPersistsHistoricalInterfaces(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-period-interfaces-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Historical interfaces", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	old := contracts.TrafficPeriod{Scope: "host", Interfaces: []string{"eth0"}, From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), Timezone: "UTC", AllowanceBytes: 100, Direction: "combined", CountedBytes: 10, Continuity: "complete"}
	if err := store.UpsertTrafficPeriod(ctx, serverID, old); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertTrafficAllowance(ctx, serverID, contracts.TrafficAllowance{Scope: "host", Interfaces: []string{"eth1"}, Direction: "combined", AllowanceBytes: 100, ResetDay: 1, Timezone: "UTC"}); err != nil {
		t.Fatal(err)
	}
	// An update assembled from the newer allowance must not rewrite the
	// historical period's interface identity.
	stale := old
	stale.Interfaces = []string{"eth1"}
	if _, err := store.AddTrafficUsage(ctx, serverID, stale, 5, "complete"); err != nil {
		t.Fatal(err)
	}
	active, found, err := store.GetActiveTrafficPeriod(ctx, serverID, "host", "combined", time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC))
	if err != nil || !found || len(active.Interfaces) != 1 || active.Interfaces[0] != "eth0" || active.CountedBytes != 15 {
		t.Fatalf("historical active interfaces changed with allowance: %+v found=%v err=%v", active, found, err)
	}
	page, err := store.QueryTrafficPeriods(ctx, serverID, time.Time{}, time.Time{}, "host", 10, "")
	if err != nil || len(page.Periods) != 1 || len(page.Periods[0].Interfaces) != 1 || page.Periods[0].Interfaces[0] != "eth0" {
		t.Fatalf("historical period interfaces were not durable: %+v err=%v", page.Periods, err)
	}
}

func TestCounterDeltaSplitsAcrossCalendarBoundary(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-split-0123")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Split", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	a := allowance(1, "UTC")
	start := time.Date(2026, 1, 31, 23, 0, 0, 0, time.UTC)
	previousNode := contracts.NodeMetricSample{ServerID: serverID, CollectorEpoch: "split-epoch-01", Sequence: 0, ObservedAt: start, Values: map[string]float64{}, Counters: map[string]string{"net.billing.rx_bytes": "0", "net.billing.tx_bytes": "0"}, Validity: map[string]string{"net.billing.rx_bytes": "valid", "net.billing.tx_bytes": "valid"}}
	currentNode := previousNode
	currentNode.Sequence = 1
	currentNode.ObservedAt = start.Add(2 * time.Hour)
	currentNode.Counters = map[string]string{"net.billing.rx_bytes": "100", "net.billing.tx_bytes": "0"}
	previous, err := previousNode.WithReceivedAt(start.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	current, err := currentNode.WithReceivedAt(currentNode.ObservedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AddUsageFromStoredAllowance(ctx, current, &previous, a); err != nil {
		t.Fatal(err)
	}
	page, err := store.QueryTrafficPeriods(ctx, serverID, start.Add(-time.Hour), current.ObservedAt.Add(time.Hour), "host", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	var total uint64
	var seen int
	for _, period := range page.Periods {
		if period.Scope == "host" {
			seen++
			total += period.CountedBytes
		}
	}
	if seen != 2 || total != 100 {
		t.Fatalf("split periods=%+v total=%d", page.Periods, total)
	}
}

func TestCounterDeltaEndingAtBoundaryStaysInPriorPeriod(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-boundary-0123")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Boundary", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	a := allowance(1, "UTC")
	start := time.Date(2026, 1, 31, 23, 0, 0, 0, time.UTC)
	previousNode := contracts.NodeMetricSample{ServerID: serverID, CollectorEpoch: "boundary-epoch-01", Sequence: 0, ObservedAt: start, Values: map[string]float64{}, Counters: map[string]string{"net.billing.rx_bytes": "0", "net.billing.tx_bytes": "0"}, Validity: map[string]string{"net.billing.rx_bytes": "valid", "net.billing.tx_bytes": "valid"}}
	currentNode := previousNode
	currentNode.Sequence = 1
	currentNode.ObservedAt = start.Add(time.Hour)
	currentNode.Counters = map[string]string{"net.billing.rx_bytes": "100", "net.billing.tx_bytes": "0"}
	previous, err := previousNode.WithReceivedAt(start.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	current, err := currentNode.WithReceivedAt(currentNode.ObservedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AddUsageFromStoredAllowance(ctx, current, &previous, a); err != nil {
		t.Fatal(err)
	}
	page, err := store.QueryTrafficPeriods(ctx, serverID, start.Add(-time.Hour), current.ObservedAt.Add(time.Hour), "host", 10, "")
	if err != nil || len(page.Periods) != 1 || page.Periods[0].CountedBytes != 100 || !page.Periods[0].To.Equal(current.ObservedAt) {
		t.Fatalf("boundary delta was assigned to the wrong period: periods=%+v err=%v", page.Periods, err)
	}
}

func TestCounterDeltaSplitUsesDSTElapsedDuration(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-dst-split-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "DST", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	a := allowance(9, "America/New_York")
	location, err := time.LoadLocation(a.Timezone)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 3, 8, 0, 0, 0, 0, location)
	end := time.Date(2026, 3, 10, 12, 0, 0, 0, location)
	previousNode := contracts.NodeMetricSample{ServerID: serverID, CollectorEpoch: "dst-split-epoch-01", Sequence: 0, ObservedAt: start, Values: map[string]float64{}, Counters: map[string]string{"net.billing.rx_bytes": "0", "net.billing.tx_bytes": "0"}, Validity: map[string]string{"net.billing.rx_bytes": "valid", "net.billing.tx_bytes": "valid"}}
	currentNode := previousNode
	currentNode.Sequence = 1
	currentNode.ObservedAt = end
	currentNode.Counters = map[string]string{"net.billing.rx_bytes": "5900", "net.billing.tx_bytes": "0"}
	previous, err := previousNode.WithReceivedAt(start.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	current, err := currentNode.WithReceivedAt(end.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AddUsageFromStoredAllowance(ctx, current, &previous, a); err != nil {
		t.Fatal(err)
	}
	page, err := store.QueryTrafficPeriods(ctx, serverID, start.Add(-time.Hour), end.Add(time.Hour), "host", 10, "")
	if err != nil || len(page.Periods) != 2 {
		t.Fatalf("DST split periods=%+v err=%v", page.Periods, err)
	}
	if page.Periods[0].CountedBytes != 2300 || page.Periods[1].CountedBytes != 3600 || page.Periods[0].CountedBytes+page.Periods[1].CountedBytes != 5900 {
		t.Fatalf("DST elapsed allocation=%+v", page.Periods)
	}
}

func TestCounterDeltaHonorsExplicitPeriodAcrossCalendarBoundary(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-explicit-split-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Explicit split", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	a := allowance(15, "UTC")
	start := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	first, err := manager.ApplyAllowance(ctx, serverID, a, start, 0, "complete", true)
	if err != nil {
		t.Fatal(err)
	}
	previousNode := contracts.NodeMetricSample{ServerID: serverID, CollectorEpoch: "explicit-split-epoch-01", Sequence: 0, ObservedAt: time.Date(2026, 1, 31, 23, 0, 0, 0, time.UTC), Values: map[string]float64{}, Counters: map[string]string{"net.billing.rx_bytes": "0", "net.billing.tx_bytes": "0"}, Validity: map[string]string{"net.billing.rx_bytes": "valid", "net.billing.tx_bytes": "valid"}}
	currentNode := previousNode
	currentNode.Sequence = 1
	currentNode.ObservedAt = time.Date(2026, 2, 20, 1, 0, 0, 0, time.UTC)
	currentNode.Counters = map[string]string{"net.billing.rx_bytes": "100", "net.billing.tx_bytes": "0"}
	previous, err := previousNode.WithReceivedAt(previousNode.ObservedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	current, err := currentNode.WithReceivedAt(currentNode.ObservedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AddUsageFromStoredAllowance(ctx, current, &previous, a); err != nil {
		t.Fatal(err)
	}
	page, err := store.QueryTrafficPeriods(ctx, serverID, time.Time{}, time.Time{}, "host", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Periods) != 2 || !page.Periods[0].From.Equal(first.From) || page.Periods[0].CountedBytes != 73 || page.Periods[1].CountedBytes != 27 || page.Periods[0].CountedBytes+page.Periods[1].CountedBytes != 100 {
		t.Fatalf("explicit period was split/re-derived: first=%+v periods=%+v", first, page.Periods)
	}
}

func TestCounterDeltaSplitsAtFutureExplicitPeriodStart(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-explicit-future-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Explicit future", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	oldAllowance := allowance(1, "UTC")
	newAllowance := allowance(15, "UTC")
	oldAt := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	if _, err := manager.AddUsage(ctx, serverID, oldAllowance, oldAt, 0, "complete"); err != nil {
		t.Fatal(err)
	}
	explicitAt := time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)
	if _, err := manager.ApplyAllowance(ctx, serverID, newAllowance, explicitAt, 0, "complete", true); err != nil {
		t.Fatal(err)
	}
	previousNode := contracts.NodeMetricSample{ServerID: serverID, CollectorEpoch: "explicit-future-epoch-01", Sequence: 0, ObservedAt: oldAt, Values: map[string]float64{}, Counters: map[string]string{"net.billing.rx_bytes": "0", "net.billing.tx_bytes": "0"}, Validity: map[string]string{"net.billing.rx_bytes": "valid", "net.billing.tx_bytes": "valid"}}
	currentNode := previousNode
	currentNode.Sequence = 1
	currentNode.ObservedAt = time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	currentNode.Counters = map[string]string{"net.billing.rx_bytes": "100", "net.billing.tx_bytes": "0"}
	previous, err := previousNode.WithReceivedAt(oldAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	current, err := currentNode.WithReceivedAt(currentNode.ObservedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AddUsageFromStoredAllowance(ctx, current, &previous, newAllowance); err != nil {
		t.Fatal(err)
	}
	page, err := store.QueryTrafficPeriods(ctx, serverID, time.Time{}, time.Time{}, "host", 10, "")
	if err != nil || len(page.Periods) != 2 {
		t.Fatalf("future explicit split periods=%+v err=%v", page.Periods, err)
	}
	if !page.Periods[0].From.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) || !page.Periods[1].From.Equal(explicitAt) || page.Periods[0].CountedBytes != 38 || page.Periods[1].CountedBytes != 62 {
		t.Fatalf("future explicit period was not honored: %+v", page.Periods)
	}
}

func TestExplicitInterfaceCutoverDoesNotFabricateCounterAttribution(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-explicit-interface-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Explicit interface", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	oldAllowance := allowance(1, "UTC")
	oldAllowance.Interfaces = []string{"eth0"}
	newAllowance := oldAllowance
	newAllowance.Interfaces = []string{"eth1"}
	oldAt := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	if _, err := manager.AddUsage(ctx, serverID, oldAllowance, oldAt, 7, "complete"); err != nil {
		t.Fatal(err)
	}
	explicitAt := time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)
	if _, err := manager.ApplyAllowance(ctx, serverID, newAllowance, explicitAt, 0, "complete", true); err != nil {
		t.Fatal(err)
	}
	previousNode := contracts.NodeMetricSample{
		ServerID: serverID, CollectorEpoch: "explicit-interface-epoch-01", Sequence: 0,
		ObservedAt: time.Date(2026, 1, 19, 0, 0, 0, 0, time.UTC), Values: map[string]float64{},
		Counters: map[string]string{"net.eth0.rx_bytes": "100", "net.eth0.tx_bytes": "0", "net.eth1.rx_bytes": "40", "net.eth1.tx_bytes": "0"},
		Validity: map[string]string{"net.eth0.rx_bytes": "valid", "net.eth0.tx_bytes": "valid", "net.eth1.rx_bytes": "valid", "net.eth1.tx_bytes": "valid"},
	}
	currentNode := previousNode
	currentNode.Sequence = 1
	currentNode.ObservedAt = time.Date(2026, 1, 21, 0, 0, 0, 0, time.UTC)
	currentNode.Counters = map[string]string{"net.eth0.rx_bytes": "150", "net.eth0.tx_bytes": "0", "net.eth1.rx_bytes": "90", "net.eth1.tx_bytes": "0"}
	previous, err := previousNode.WithReceivedAt(previousNode.ObservedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	current, err := currentNode.WithReceivedAt(currentNode.ObservedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	period, err := manager.AddUsageFromStoredAllowance(ctx, current, &previous, newAllowance)
	if err != nil {
		t.Fatal(err)
	}
	if period.Continuity != "uncertain" || !sameInterfaces(period.Interfaces, newAllowance.Interfaces) {
		t.Fatalf("cutover result was not marked uncertain/new: %+v", period)
	}
	page, err := store.QueryTrafficPeriods(ctx, serverID, time.Time{}, time.Time{}, "host", 10, "")
	if err != nil || len(page.Periods) != 2 {
		t.Fatalf("interface cutover periods=%+v err=%v", page.Periods, err)
	}
	var oldPeriod, newPeriod contracts.TrafficPeriod
	for _, candidate := range page.Periods {
		switch {
		case sameInterfaces(candidate.Interfaces, oldAllowance.Interfaces):
			oldPeriod = candidate
		case sameInterfaces(candidate.Interfaces, newAllowance.Interfaces):
			newPeriod = candidate
		}
	}
	if oldPeriod.CountedBytes != 7 || oldPeriod.Continuity != "uncertain" || newPeriod.CountedBytes != 0 || newPeriod.Continuity != "uncertain" {
		t.Fatalf("counter delta crossed interface cutover: old=%+v new=%+v all=%+v", oldPeriod, newPeriod, page.Periods)
	}
}

func TestPostProcessDrainSkipsBlockedStream(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverA := contracts.ServerID("server-starve-a-01")
	serverC := contracts.ServerID("server-starve-c-01")
	serverB := contracts.ServerID("server-starve-b-01")
	for _, id := range []contracts.ServerID{serverA, serverB, serverC} {
		if err := store.EnsureServer(ctx, contracts.Server{ID: id, Name: string(id), Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
			t.Fatal(err)
		}
	}
	// Leave the observer unset so all accepted samples remain in the durable
	// queue. A and C have missing prefixes and together block a full first
	// page; B is made ready by an explicit durable gap but sorts behind their
	// lower sequence numbers.
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	aSamples := make([]contracts.MetricSample, 0, monitoring.MaxPageItems/2)
	for sequence := uint64(1); sequence <= monitoring.MaxPageItems/2; sequence++ {
		aSamples = append(aSamples, contracts.MetricSample{ServerID: serverA, CollectorEpoch: "starve-epoch-a-01", Sequence: sequence, ObservedAt: base.Add(time.Duration(sequence) * time.Minute), ReceivedAt: base.Add(time.Duration(sequence)*time.Minute + time.Second), Values: map[string]float64{"cpu.utilization": 0}})
	}
	if _, err := store.IngestSamples(ctx, serverA, aSamples, nil); err != nil {
		t.Fatal(err)
	}
	cSamples := make([]contracts.MetricSample, 0, monitoring.MaxPageItems/2)
	for sequence := uint64(1); sequence <= monitoring.MaxPageItems/2; sequence++ {
		cSamples = append(cSamples, contracts.MetricSample{ServerID: serverC, CollectorEpoch: "starve-epoch-c-01", Sequence: sequence, ObservedAt: base.Add(time.Duration(sequence) * time.Minute), ReceivedAt: base.Add(time.Duration(sequence)*time.Minute + time.Second), Values: map[string]float64{"cpu.utilization": 0}})
	}
	if _, err := store.IngestSamples(ctx, serverC, cSamples, nil); err != nil {
		t.Fatal(err)
	}
	bSample := contracts.MetricSample{ServerID: serverB, CollectorEpoch: "starve-epoch-b-01", Sequence: 1000, ObservedAt: base, ReceivedAt: base.Add(time.Second), Values: map[string]float64{"cpu.utilization": 0}}
	gap := contracts.CoverageGap{CollectorEpoch: bSample.CollectorEpoch, FromSequence: 0, ToSequence: bSample.Sequence, Reason: "spool-eviction"}
	if _, err := store.IngestSamples(ctx, serverB, []contracts.MetricSample{bSample}, []contracts.CoverageGap{gap}); err != nil {
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
	if err := processor.ObserveIngestion(ctx, nil); !errors.Is(err, ErrUsageDeferred) {
		t.Fatalf("blocked stream drain error=%v, want deferred sentinel", err)
	}
	pending, err := store.ListPendingPostProcessSamples(ctx, monitoring.MaxPageItems)
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range pending {
		if sample.ServerID == serverB {
			t.Fatalf("ready server remained queued behind blocked stream: %+v", sample)
		}
	}
}

func TestPostProcessDrainFindsReadyTailBeyondLegacyRoundLimit(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-starve-many-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Many blocked streams", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(time.Hour)
	blockedCount := 9*monitoring.MaxPageItems + 1
	for offset := 0; offset < blockedCount; offset += contracts.MaxBatchSamples {
		end := offset + contracts.MaxBatchSamples
		if end > blockedCount {
			end = blockedCount
		}
		batch := make([]contracts.MetricSample, 0, end-offset)
		for index := offset; index < end; index++ {
			batch = append(batch, contracts.MetricSample{ServerID: serverID, CollectorEpoch: contracts.CollectorEpoch(fmt.Sprintf("many-blocked-epoch-%05d", index)), Sequence: 1, ObservedAt: base, ReceivedAt: base, Values: map[string]float64{"cpu.utilization": 0}})
		}
		if _, err := store.IngestSamples(ctx, serverID, batch, nil); err != nil {
			t.Fatal(err)
		}
	}
	ready := contracts.MetricSample{ServerID: serverID, CollectorEpoch: "zz-ready-tail-epoch-01", Sequence: 1000, ObservedAt: base, ReceivedAt: base, Values: map[string]float64{"cpu.utilization": 0}}
	gap := contracts.CoverageGap{CollectorEpoch: ready.CollectorEpoch, FromSequence: 0, ToSequence: ready.Sequence, Reason: "spool-eviction"}
	if _, err := store.IngestSamples(ctx, serverID, []contracts.MetricSample{ready}, []contracts.CoverageGap{gap}); err != nil {
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
	if err := processor.ObserveIngestion(ctx, nil); !errors.Is(err, ErrUsageDeferred) {
		t.Fatalf("large blocked drain error=%v, want deferred", err)
	}
	if len(processor.blockedStreams) != blockedCount {
		t.Fatalf("persistent blocked identities=%d, want %d", len(processor.blockedStreams), blockedCount)
	}
	remainingReady, err := store.ListPendingPostProcessSamplesSkipping(ctx, monitoring.MaxPageItems, processor.blockedStreams)
	if err != nil {
		t.Fatal(err)
	}
	if len(remainingReady) != 0 {
		t.Fatalf("ready tail starved behind %d streams: %+v", blockedCount, remainingReady)
	}
}

func TestPostProcessRetryEventuallyDrainsAfterPredecessor(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-post-process-retry-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Post-process retry", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	allowance := allowance(1, "UTC")
	if err := store.UpsertTrafficAllowance(ctx, serverID, allowance); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC)
	sample := func(sequence uint64, counter string, at time.Time) contracts.MetricSample {
		return contracts.MetricSample{ServerID: serverID, CollectorEpoch: "post-process-retry-epoch-01", Sequence: sequence, ObservedAt: at, ReceivedAt: at.Add(time.Second), Values: map[string]float64{"cpu.utilization": 10}, Counters: map[string]string{"net.billing.rx_bytes": counter, "net.billing.tx_bytes": "0"}, Validity: map[string]string{"cpu.utilization": "valid", "net.billing.rx_bytes": "valid", "net.billing.tx_bytes": "valid"}}
	}
	// Leave the observer unset so this test exercises the periodic worker rather
	// than the synchronous post-commit callback. The later point is durable but
	// must wait for sequence zero before traffic charging is allowed.
	second := sample(1, "200", base.Add(time.Minute))
	if _, err := store.IngestSamples(ctx, serverID, []contracts.MetricSample{second}, nil); err != nil {
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
	if err := processor.DrainPendingPostProcess(ctx); !errors.Is(err, ErrUsageDeferred) {
		t.Fatalf("startup drain error=%v, want explicit deferred status", err)
	}
	pending, err := store.ListPendingPostProcessSamples(ctx, monitoring.MaxPageItems)
	if err != nil || len(pending) != 1 || pending[0].Sequence != 1 {
		t.Fatalf("deferred sample was not retained: pending=%+v err=%v", pending, err)
	}
	retryCtx, cancel := context.WithCancel(ctx)
	done := processor.StartPostProcessRetry(retryCtx, 10*time.Millisecond, nil)
	defer func() {
		cancel()
		<-done
	}()
	// This predecessor arrives without an observer notification. The worker
	// must discover the now-ready durable queue on its next bounded pass.
	first := sample(0, "100", base)
	if _, err := store.IngestSamples(ctx, serverID, []contracts.MetricSample{first}, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		pending, err = store.ListPendingPostProcessSamples(ctx, monitoring.MaxPageItems)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("periodic retry left durable queue pending: %+v", pending)
		}
		time.Sleep(5 * time.Millisecond)
	}
	page, err := store.QueryTrafficPeriods(ctx, serverID, time.Time{}, time.Time{}, "host", monitoring.MaxPageItems, "")
	if err != nil || len(page.Periods) != 1 || page.Periods[0].CountedBytes != 100 {
		t.Fatalf("periodic retry traffic=%+v err=%v", page, err)
	}
	// Allow another tick to prove a replay cannot charge the same interval a
	// second time after the queue has been acknowledged.
	time.Sleep(30 * time.Millisecond)
	page, err = store.QueryTrafficPeriods(ctx, serverID, time.Time{}, time.Time{}, "host", monitoring.MaxPageItems, "")
	if err != nil || len(page.Periods) != 1 || page.Periods[0].CountedBytes != 100 {
		t.Fatalf("periodic retry charged traffic twice: %+v err=%v", page, err)
	}
}

func TestScheduleTransitionSurvivesManagerRestart(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-restart-traffic-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Restart", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	old := allowance(1, "UTC")
	proposed := allowance(20, "UTC")
	base := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	first, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.AddUsage(ctx, serverID, old, base, 1, "complete"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.AddUsage(ctx, serverID, proposed, base.Add(time.Minute), 1, "complete"); err != nil {
		t.Fatal(err)
	}
	second, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	active, err := second.AddUsage(ctx, serverID, proposed, time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC), 1, "complete")
	if err != nil || active.From.Day() != 1 {
		t.Fatalf("restart applied schedule too early: %+v err=%v", active, err)
	}
	boundary, err := second.AddUsage(ctx, serverID, proposed, time.Date(2026, 2, 2, 12, 0, 0, 0, time.UTC), 1, "complete")
	if err != nil || boundary.From.Day() != 1 || boundary.To.Day() != 20 {
		t.Fatalf("restart did not apply schedule at boundary: %+v err=%v", boundary, err)
	}
}

func TestExplicitPeriodIdentitySurvivesSubsequentSamples(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-explicit-period-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Explicit", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	a := allowance(15, "UTC")
	start := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	first, err := manager.ApplyAllowance(ctx, serverID, a, start, 10, "complete", true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.AddUsage(ctx, serverID, a, start.Add(2*time.Hour), 5, "complete")
	if err != nil {
		t.Fatal(err)
	}
	if !second.From.Equal(first.From) || second.CountedBytes != 15 {
		t.Fatalf("explicit period was re-derived: first=%+v second=%+v", first, second)
	}
	page, err := store.QueryTrafficPeriods(ctx, serverID, time.Time{}, time.Time{}, "host", 10, "")
	if err != nil || len(page.Periods) != 1 {
		t.Fatalf("overlapping explicit periods: page=%+v err=%v", page, err)
	}
}

func TestLateScheduleTransitionDoesNotSpanProposedBoundary(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-late-transition-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Late transition", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	old := allowance(1, "UTC")
	proposed := allowance(15, "UTC")
	base := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	if _, err := manager.AddUsage(ctx, serverID, old, base, 10, "complete"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AddUsage(ctx, serverID, proposed, base.Add(time.Hour), 1, "complete"); err != nil {
		t.Fatal(err)
	}
	// The first observation after the Feb-1 effective boundary arrives after
	// the proposed Feb-15 boundary. Its delta belongs to Feb-15 onward and
	// must not create a synthetic Feb-1..Mar-15 period.
	period, err := manager.AddUsage(ctx, serverID, proposed, time.Date(2026, 2, 20, 12, 0, 0, 0, time.UTC), 5, "complete")
	if err != nil {
		t.Fatal(err)
	}
	if period.From.Day() != 15 || period.To.Day() != 15 || period.From.Month() != time.February {
		t.Fatalf("late transition crossed proposed boundary: %+v", period)
	}
}

func TestDelayedUsagePreservesHistoricalPeriodPolicySnapshot(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-delayed-history-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Delayed history", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	oldAllowance := contracts.TrafficAllowance{Scope: "host", Direction: "combined", Interfaces: []string{"eth0"}, AllowanceBytes: 1000, ResetDay: 1, Timezone: "UTC"}
	newAllowance := contracts.TrafficAllowance{Scope: "host", Direction: "combined", Interfaces: []string{"eth1"}, AllowanceBytes: 9000, ResetDay: 1, Timezone: "Asia/Tehran"}
	janStart := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	jan := contracts.TrafficPeriod{Scope: "host", Direction: "combined", Interfaces: append([]string(nil), oldAllowance.Interfaces...), From: janStart, To: time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC), Timezone: oldAllowance.Timezone, AllowanceBytes: oldAllowance.AllowanceBytes, CountedBytes: 10, Continuity: "complete"}
	if err := store.UpsertTrafficPeriod(ctx, serverID, jan); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertTrafficAllowance(ctx, serverID, newAllowance); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	previous := contracts.MetricSample{ServerID: serverID, CollectorEpoch: "delayed-history-epoch-01", Sequence: 0, ObservedAt: janStart.Add(10 * time.Hour), ReceivedAt: janStart.Add(10*time.Hour + time.Second), Values: map[string]float64{}, Counters: map[string]string{"net.eth0.rx_bytes": "100", "net.eth0.tx_bytes": "0", "net.eth1.rx_bytes": "1000", "net.eth1.tx_bytes": "0"}, Validity: map[string]string{"net.eth0.rx_bytes": "valid", "net.eth0.tx_bytes": "valid", "net.eth1.rx_bytes": "valid", "net.eth1.tx_bytes": "valid"}}
	current := contracts.MetricSample{ServerID: serverID, CollectorEpoch: previous.CollectorEpoch, Sequence: 1, ObservedAt: janStart.Add(11 * time.Hour), ReceivedAt: time.Date(2026, time.February, 2, 0, 0, 0, 0, time.UTC), Values: map[string]float64{}, Counters: map[string]string{"net.eth0.rx_bytes": "125", "net.eth0.tx_bytes": "0", "net.eth1.rx_bytes": "5000", "net.eth1.tx_bytes": "0"}, Validity: map[string]string{"net.eth0.rx_bytes": "valid", "net.eth0.tx_bytes": "valid", "net.eth1.rx_bytes": "valid", "net.eth1.tx_bytes": "valid"}}
	if _, err := manager.AddUsageFromStoredAllowance(ctx, current, &previous, newAllowance); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.GetActiveTrafficPeriod(ctx, serverID, "host", "combined", current.ObservedAt)
	if err != nil || !found {
		t.Fatalf("historical period found=%v err=%v", found, err)
	}
	if got.CountedBytes != 35 || got.AllowanceBytes != jan.AllowanceBytes || got.Timezone != jan.Timezone || !got.From.Equal(jan.From) || !got.To.Equal(jan.To) || !sameInterfaces(got.Interfaces, jan.Interfaces) {
		t.Fatalf("delayed usage rewrote historical policy: got=%+v want identity=%+v", got, jan)
	}
}

func TestQueuedHistoricalUsageMaterializesAcceptedAllowanceVersion(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-queued-old-config-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Queued old config", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	oldAllowance := contracts.TrafficAllowance{Scope: "host", Direction: "combined", Interfaces: []string{"eth0"}, AllowanceBytes: 1000, ResetDay: 1, Timezone: "UTC"}
	newAllowance := contracts.TrafficAllowance{Scope: "host", Direction: "combined", Interfaces: []string{"eth1"}, AllowanceBytes: 9000, ResetDay: 15, Timezone: "Asia/Tehran"}
	oldEffective := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	newEffective := time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)
	if err := store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if err := txUpsertTrafficAllowance(ctx, tx, serverID, oldAllowance, oldEffective); err != nil {
			return err
		}
		return txUpsertTrafficAllowance(ctx, tx, serverID, newAllowance, newEffective)
	}); err != nil {
		t.Fatal(err)
	}
	observed := time.Date(2026, time.January, 10, 12, 0, 0, 0, time.UTC)
	sample := func(sequence uint64, value string, at time.Time) contracts.MetricSample {
		return contracts.MetricSample{ServerID: serverID, CollectorEpoch: "queued-old-config-epoch-01", Sequence: sequence, ObservedAt: at, ReceivedAt: at.Add(time.Second), Values: map[string]float64{"cpu.utilization": 0}, Counters: map[string]string{"net.eth0.rx_bytes": value, "net.eth0.tx_bytes": "0", "net.eth1.rx_bytes": "5000", "net.eth1.tx_bytes": "0"}, Validity: map[string]string{"cpu.utilization": "valid", "net.eth0.rx_bytes": "valid", "net.eth0.tx_bytes": "valid", "net.eth1.rx_bytes": "valid", "net.eth1.tx_bytes": "valid"}}
	}
	first := sample(0, "100", observed)
	second := sample(1, "125", observed.Add(time.Hour))
	if _, err := store.IngestSamples(ctx, serverID, []contracts.MetricSample{first, second}, nil); err != nil {
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
	if err := processor.ObserveIngestion(ctx, nil); err != nil {
		t.Fatal(err)
	}
	want, err := PeriodFor(second.ObservedAt, oldAllowance)
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := store.GetActiveTrafficPeriod(ctx, serverID, "host", "combined", second.ObservedAt)
	if err != nil || !found {
		t.Fatalf("materialized historical period found=%v err=%v", found, err)
	}
	if got.CountedBytes != 25 || got.AllowanceBytes != oldAllowance.AllowanceBytes || got.Timezone != oldAllowance.Timezone || !got.From.Equal(want.From) || !got.To.Equal(want.To) || !sameInterfaces(got.Interfaces, oldAllowance.Interfaces) {
		t.Fatalf("historical queue used current config: got=%+v want identity=%+v", got, want)
	}
}

func TestScheduleChangeAtBoundaryAppliesImmediately(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-boundary-schedule-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Boundary", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	old := allowance(1, "UTC")
	proposed := allowance(15, "UTC")
	base := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	if _, err := manager.AddUsage(ctx, serverID, old, base, 1, "complete"); err != nil {
		t.Fatal(err)
	}
	boundary := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	period, err := manager.AddUsage(ctx, serverID, proposed, boundary, 2, "complete")
	if err != nil || !period.From.Equal(boundary) || period.To.Day() != 15 {
		t.Fatalf("boundary schedule edit did not cut over immediately: %+v err=%v", period, err)
	}
}

func TestForecastRequiresCoverageAndLabelsEstimate(t *testing.T) {
	period := contracts.TrafficPeriod{Scope: "host", From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), Timezone: "UTC", AllowanceBytes: 100000, Direction: "outbound", Continuity: "complete", CountedBytes: 1000}
	observations := make([]contracts.TrafficObservation, 0, 3)
	for index, bytes := range []uint64{0, 240, 480} {
		observations = append(observations, contracts.TrafficObservation{ObservedAt: period.From.Add(time.Duration(index) * 12 * time.Hour), OutboundBytes: bytes, Valid: true, Coverage: 1})
	}
	forecast, err := Forecast(period, observations, period.From.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !forecast.Available || forecast.Label != "estimated_recent_rate" || forecast.EstimatedBytesAtEnd <= forecast.ObservedBytes {
		t.Fatalf("forecast=%+v", forecast)
	}
	observations[1].Coverage = 0.2
	forecast, err = Forecast(period, observations, period.From.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if forecast.Available {
		t.Fatalf("sparse forecast unexpectedly available: %+v", forecast)
	}
	// Future observations must not influence an as-of forecast.
	future := append([]contracts.TrafficObservation(nil), observations[:2]...)
	future = append(future, contracts.TrafficObservation{ObservedAt: period.From.Add(48 * time.Hour), OutboundBytes: 960, Valid: true, Coverage: 1})
	forecast, err = Forecast(period, future, period.From.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if forecast.Available {
		t.Fatalf("future observation was used in forecast: %+v", forecast)
	}
	// A complete 24-hour run that ended outside the as-of seven-day window is
	// stale evidence and must not be relabeled as a recent rate.
	stale := []contracts.TrafficObservation{
		{ObservedAt: period.From.Add(8 * 24 * time.Hour), OutboundBytes: 0, Valid: true, Coverage: 1},
		{ObservedAt: period.From.Add(9 * 24 * time.Hour), OutboundBytes: 240, Valid: true, Coverage: 1},
	}
	forecast, err = Forecast(period, stale, period.From.Add(17*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if forecast.Available {
		t.Fatalf("stale observations produced a forecast: %+v", forecast)
	}
	zeroRate := []contracts.TrafficObservation{
		{ObservedAt: period.From, OutboundBytes: 1000, Valid: true, Coverage: 1},
		{ObservedAt: period.From.Add(24 * time.Hour), OutboundBytes: 1000, Valid: true, Coverage: 1},
	}
	forecast, err = Forecast(period, zeroRate, period.From.Add(24*time.Hour))
	if err != nil || !forecast.Available || forecast.EstimatedBytesAtEnd != period.CountedBytes {
		t.Fatalf("valid zero-rate observations were hidden: forecast=%+v err=%v", forecast, err)
	}
	withInvalidGap := []contracts.TrafficObservation{
		{ObservedAt: period.From, OutboundBytes: 0, Valid: true, Coverage: 1},
		{ObservedAt: period.From.Add(12 * time.Hour), OutboundBytes: 100, Valid: false, Coverage: 0},
		{ObservedAt: period.From.Add(24 * time.Hour), OutboundBytes: 200, Valid: true, Coverage: 1},
	}
	forecast, err = Forecast(period, withInvalidGap, period.From.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if forecast.Available {
		t.Fatalf("invalid observation was bridged in forecast: %+v", forecast)
	}
	// A reset early in the target billing period permanently makes its durable
	// CountedBytes baseline incomplete. More than 24 hours of later clean rate
	// evidence cannot reconstruct the omitted bytes.
	incomplete := period
	incomplete.Continuity = "uncertain"
	cleanAfterReset := []contracts.TrafficObservation{
		{ObservedAt: period.From.Add(time.Hour), OutboundBytes: 5, Valid: true, Coverage: 1},
		{ObservedAt: period.From.Add(13 * time.Hour), OutboundBytes: 125, Valid: true, Coverage: 1},
		{ObservedAt: period.From.Add(25 * time.Hour), OutboundBytes: 245, Valid: true, Coverage: 1},
	}
	forecast, err = Forecast(incomplete, cleanAfterReset, period.From.Add(25*time.Hour))
	if err != nil || forecast.Available || forecast.Reason != "traffic_period_continuity_incomplete" || forecast.ObservedBytes != 0 {
		t.Fatalf("incomplete-period forecast=%+v err=%v", forecast, err)
	}
}

func TestForecastAdapterUsesHistoricalPeriodInterfaceSnapshots(t *testing.T) {
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	oldPeriod := contracts.TrafficPeriod{Scope: "host", Interfaces: []string{"eth0"}, From: base, To: base.Add(24 * time.Hour), Timezone: "UTC", AllowanceBytes: 1000, Direction: "combined", Continuity: "complete"}
	newPeriod := contracts.TrafficPeriod{Scope: "host", Interfaces: []string{"eth1"}, From: oldPeriod.To, To: base.Add(48 * time.Hour), Timezone: "UTC", AllowanceBytes: 1000, Direction: "combined", Continuity: "complete"}
	wire := func(sequence uint64, at time.Time, oldRX, newRX string) contracts.MetricSample {
		return contracts.MetricSample{
			CollectorEpoch: "forecast-period-epoch-01",
			Sequence:       sequence,
			ObservedAt:     at,
			Counters: map[string]string{
				"net.eth0.rx_bytes": oldRX, "net.eth0.tx_bytes": "0",
				"net.eth1.rx_bytes": newRX, "net.eth1.tx_bytes": "0",
			},
			Validity: map[string]string{
				"net.eth0.rx_bytes": "valid", "net.eth0.tx_bytes": "valid",
				"net.eth1.rx_bytes": "valid", "net.eth1.tx_bytes": "valid",
			},
		}
	}
	observations := trafficObservationsForPeriods([]contracts.MetricSample{
		wire(0, base, "0", "1000"),
		wire(1, base.Add(12*time.Hour), "100", "1100"),
		wire(2, oldPeriod.To, "200", "0"),
		wire(3, base.Add(36*time.Hour), "300", "50"),
	}, []contracts.TrafficPeriod{oldPeriod, newPeriod})
	if len(observations) != 4 {
		t.Fatalf("observations=%d, want 4", len(observations))
	}
	if !observations[0].Valid || observations[0].InboundBytes != 0 || !observations[1].Valid || observations[1].InboundBytes != 100 {
		t.Fatalf("old period observations=%+v", observations[:2])
	}
	if observations[2].Valid {
		t.Fatal("interface hand-off sample was used as a cross-series bridge")
	}
	if !observations[3].Valid || observations[3].InboundBytes != 50 {
		t.Fatalf("new period observation=%+v", observations[3])
	}
}

func TestLongCounterIntervalAcrossBoundedPeriodRowsBecomesUncertain(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-traffic-long-gap-01")
	a := allowance(1, "UTC")
	a.Interfaces = []string{"eth0"}
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Long gap", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertTrafficAllowance(ctx, serverID, a); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for index := 0; index < 65; index++ {
		from := base.Add(time.Duration(index) * time.Hour)
		period := contracts.TrafficPeriod{Scope: a.Scope, Interfaces: []string{"eth0"}, From: from, To: from.Add(time.Hour), Timezone: "UTC", AllowanceBytes: a.AllowanceBytes, Direction: a.Direction, Continuity: "complete"}
		if err := store.UpsertTrafficPeriod(ctx, serverID, period); err != nil {
			t.Fatalf("period %d: %v", index, err)
		}
	}
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	wire := func(sequence uint64, at time.Time, value string) contracts.MetricSample {
		return contracts.MetricSample{ServerID: serverID, CollectorEpoch: "long-gap-epoch-01", Sequence: sequence, ObservedAt: at, ReceivedAt: at, Values: map[string]float64{}, Counters: map[string]string{"net.eth0.rx_bytes": value, "net.eth0.tx_bytes": "0"}, Validity: map[string]string{"net.eth0.rx_bytes": "valid", "net.eth0.tx_bytes": "valid"}}
	}
	start := wire(0, base, "0")
	end := wire(1, base.Add(64*time.Hour+30*time.Minute), "100")
	result, err := manager.AddUsageFromStoredAllowance(ctx, end, &start, a)
	if err != nil {
		t.Fatalf("long interval was not acknowledgeable: %v", err)
	}
	if result.Continuity != "uncertain" || result.CountedBytes != 0 {
		t.Fatalf("result=%+v, want uncertain zero-byte usage", result)
	}
	page, err := store.QueryTrafficPeriods(ctx, serverID, time.Time{}, time.Time{}, a.Scope, 200, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Periods) != 65 {
		t.Fatalf("period rows=%d, want 65", len(page.Periods))
	}
	for _, period := range page.Periods {
		if period.CountedBytes != 0 || period.Continuity != "uncertain" {
			t.Fatalf("period %v fabricated/complete state: %+v", period.From, period)
		}
	}
}

func TestCounterDeltaHonorsDirectionSelectionAndResets(t *testing.T) {
	a := allowance(1, "UTC")
	a.Interfaces = []string{"eth0"}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	previous := contracts.MetricSample{ServerID: "server-traffic-0123", CollectorEpoch: "epoch-traffic-01", ObservedAt: base, Counters: map[string]string{"net.eth0.rx_bytes": "100", "net.eth0.tx_bytes": "200"}, Validity: map[string]string{"net.eth0.rx_bytes": "valid", "net.eth0.tx_bytes": "valid"}}
	current := previous
	current.Sequence = 1
	current.ObservedAt = base.Add(time.Minute)
	current.Counters = map[string]string{"net.eth0.rx_bytes": "125", "net.eth0.tx_bytes": "260"}
	if delta, continuity, err := CounterDelta(previous, current, a); err != nil || delta != 85 || continuity != "complete" {
		t.Fatalf("combined delta=%d continuity=%s err=%v", delta, continuity, err)
	}
	a.Direction = "outbound"
	if delta, continuity, err := CounterDelta(previous, current, a); err != nil || delta != 60 || continuity != "complete" {
		t.Fatalf("outbound delta=%d continuity=%s err=%v", delta, continuity, err)
	}
	current.Counters["net.eth0.tx_bytes"] = "2"
	if delta, continuity, err := CounterDelta(previous, current, a); err != nil || delta != 0 || continuity != "uncertain" {
		t.Fatalf("reset delta=%d continuity=%s err=%v", delta, continuity, err)
	}
	previous.Sequence = 1
	current.Sequence = 3
	current.Counters["net.eth0.tx_bytes"] = "260"
	if delta, continuity, err := CounterDelta(previous, current, a); err != nil || delta != 0 || continuity != "uncertain" {
		t.Fatalf("sequence gap delta=%d continuity=%s err=%v", delta, continuity, err)
	}
	current = previous
	current.Sequence = 1
	previous.Sequence = 2
	current.ObservedAt = base.Add(2 * time.Minute)
	current.Counters = map[string]string{"net.eth0.rx_bytes": "125", "net.eth0.tx_bytes": "260"}
	if delta, continuity, err := CounterDelta(previous, current, a); err != nil || delta != 0 || continuity != "uncertain" {
		t.Fatalf("out-of-order delta=%d continuity=%s err=%v", delta, continuity, err)
	}
}

func TestTrafficServiceConfiguresDurableAllowanceIdempotently(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-http-traffic-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "HTTP", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	service.Now = func() time.Time { return time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC) }
	payload := `{"idempotency_key":"traffic-http-1","expected_revision":"0","scope":"host","allowance_bytes":"1000","reset_day":1,"timezone":"UTC","direction":"combined"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/servers/"+string(serverID)+"/traffic", bytes.NewBufferString(payload))
	resp := httptest.NewRecorder()
	service.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("configure status=%d body=%s", resp.Code, resp.Body.String())
	}
	var first allowanceResult
	if err := json.Unmarshal(resp.Body.Bytes(), &first); err != nil || first.Period.Scope != "host" {
		t.Fatalf("result=%+v err=%v", first, err)
	}
	if first.ConfigurationRevision != 1 {
		t.Fatalf("configuration revision=%d, want 1", first.ConfigurationRevision)
	}
	// A retry returns the durable response and does not create another period.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/servers/"+string(serverID)+"/traffic", bytes.NewBufferString(payload))
	resp = httptest.NewRecorder()
	service.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("retry status=%d body=%s", resp.Code, resp.Body.String())
	}
	page, err := store.QueryTrafficPeriods(ctx, serverID, time.Time{}, time.Time{}, "host", 10, "")
	if err != nil || len(page.Periods) != 1 {
		t.Fatalf("periods=%+v err=%v", page, err)
	}
	amountChange := `{"idempotency_key":"traffic-http-amount","expected_revision":"1","scope":"host","allowance_bytes":"2000","reset_day":1,"timezone":"UTC","direction":"combined"}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/servers/"+string(serverID)+"/traffic", bytes.NewBufferString(amountChange))
	resp = httptest.NewRecorder()
	service.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("amount change status=%d body=%s", resp.Code, resp.Body.String())
	}
	var changed allowanceResult
	if err := json.Unmarshal(resp.Body.Bytes(), &changed); err != nil || changed.ConfigurationRevision != 2 || changed.Preview == nil || changed.Preview.Current.AllowanceBytes != 1000 || changed.Preview.Proposed.AllowanceBytes != 2000 {
		t.Fatalf("amount preview=%+v err=%v", changed, err)
	}
	// A different idempotency key cannot reuse the stale revision after the
	// first durable configuration has advanced it.
	secondConfig := `{"idempotency_key":"traffic-http-2","expected_revision":"0","scope":"host","allowance_bytes":"1000","reset_day":1,"timezone":"UTC","direction":"combined"}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/servers/"+string(serverID)+"/traffic", bytes.NewBufferString(secondConfig))
	resp = httptest.NewRecorder()
	service.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusConflict {
		t.Fatalf("stale revision status=%d body=%s", resp.Code, resp.Body.String())
	}
	payload = `{"idempotency_key":"traffic-http-1","expected_revision":"0","scope":"host","allowance_bytes":"2000","reset_day":1,"timezone":"UTC","direction":"combined"}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/servers/"+string(serverID)+"/traffic", bytes.NewBufferString(payload))
	resp = httptest.NewRecorder()
	service.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusConflict {
		t.Fatalf("conflicting retry status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestTrafficForecastEndpointUsesDurableCounterSamples(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-http-forecast-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Forecast", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	a := allowance(1, "UTC")
	if err := store.UpsertTrafficAllowance(ctx, serverID, a); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	period, err := manager.AddUsage(ctx, serverID, a, base, 100, "complete")
	if err != nil {
		t.Fatal(err)
	}
	// The durable total can advance after an as-of request. The forecast must
	// derive its observed value from samples at the requested boundary rather
	// than exposing this later period total.
	period.CountedBytes = 1000
	if err := store.UpsertTrafficPeriod(ctx, serverID, period); err != nil {
		t.Fatal(err)
	}
	wire := func(sequence uint64, value string, at time.Time) contracts.MetricSample {
		return contracts.MetricSample{ServerID: serverID, CollectorEpoch: "forecast-epoch-01", Sequence: sequence, ObservedAt: at, ReceivedAt: at.Add(time.Second), Values: map[string]float64{}, Counters: map[string]string{"net.billing.rx_bytes": value, "net.billing.tx_bytes": "0"}, Validity: map[string]string{"net.billing.rx_bytes": "valid", "net.billing.tx_bytes": "valid"}}
	}
	if _, err := store.IngestSamples(ctx, serverID, []contracts.MetricSample{wire(0, "0", base), wire(1, "240", base.Add(12*time.Hour)), wire(2, "480", base.Add(24*time.Hour)), wire(3, "1000", base.Add(36*time.Hour))}, nil); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	// as_of intentionally falls between durable samples. The endpoint uses
	// the latest effective observation rather than requiring a sample at the
	// exact request timestamp.
	asOf := base.Add(30 * time.Hour)
	service.Now = func() time.Time { return asOf }
	req := httptest.NewRequest(http.MethodGet, "/api/v1/servers/"+string(serverID)+"/traffic/forecast?scope=host&direction=combined&as_of="+asOf.Format(time.RFC3339Nano), nil)
	resp := httptest.NewRecorder()
	service.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("forecast status=%d body=%s", resp.Code, resp.Body.String())
	}
	var forecast contracts.TrafficForecast
	if err := json.Unmarshal(resp.Body.Bytes(), &forecast); err != nil {
		t.Fatal(err)
	}
	if !forecast.Available || forecast.Label != "estimated_recent_rate" || forecast.UsableDurationSeconds < int64(24*time.Hour/time.Second) || forecast.ObservedBytes != 480 {
		t.Fatalf("forecast=%+v", forecast)
	}
	// Without an explicit historical as_of, the durable active-period total is
	// authoritative and must not be replaced by the recent-window delta.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/servers/"+string(serverID)+"/traffic/forecast?scope=host&direction=combined", nil)
	resp = httptest.NewRecorder()
	service.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("current forecast status=%d body=%s", resp.Code, resp.Body.String())
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &forecast); err != nil {
		t.Fatal(err)
	}
	if !forecast.Available || forecast.ObservedBytes != 1000 {
		t.Fatalf("current forecast=%+v, want durable observed total", forecast)
	}
	period.Continuity = "uncertain"
	if err := store.UpsertTrafficPeriod(ctx, serverID, period); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/servers/"+string(serverID)+"/traffic/forecast?scope=host&direction=combined", nil)
	resp = httptest.NewRecorder()
	service.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("incomplete forecast status=%d body=%s", resp.Code, resp.Body.String())
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &forecast); err != nil {
		t.Fatal(err)
	}
	if forecast.Available || forecast.Reason != "traffic_period_continuity_incomplete" || forecast.ObservedBytes != 0 {
		t.Fatalf("incomplete HTTP forecast used counted baseline: %+v", forecast)
	}
	future := base.Add(31 * time.Hour)
	req = httptest.NewRequest(http.MethodGet, "/api/v1/servers/"+string(serverID)+"/traffic/forecast?scope=host&direction=combined&as_of="+future.Format(time.RFC3339Nano), nil)
	resp = httptest.NewRecorder()
	service.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("future as_of status=%d body=%s", resp.Code, resp.Body.String())
	}
	bad := httptest.NewRequest(http.MethodGet, "/api/v1/servers/"+string(serverID)+"/traffic/forecast?direction=combined", nil)
	badResp := httptest.NewRecorder()
	service.Handler().ServeHTTP(badResp, bad)
	if badResp.Code != http.StatusBadRequest {
		t.Fatalf("missing scope status=%d body=%s", badResp.Code, badResp.Body.String())
	}
}

func TestTrafficForecastUsesRequestedDirectionHistoricalSnapshot(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-http-forecast-direction-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Forecast direction", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	inbound := allowance(1, "UTC")
	inbound.Direction = "inbound"
	inbound.Interfaces = []string{"eth0"}
	outbound := allowance(1, "UTC")
	outbound.Direction = "outbound"
	outbound.Interfaces = []string{"eth1"}
	for _, configured := range []contracts.TrafficAllowance{inbound, outbound} {
		if err := store.UpsertTrafficAllowance(ctx, serverID, configured); err != nil {
			t.Fatal(err)
		}
		period := contracts.TrafficPeriod{Scope: configured.Scope, Interfaces: configured.Interfaces, From: base, To: base.Add(31 * 24 * time.Hour), Timezone: configured.Timezone, AllowanceBytes: configured.AllowanceBytes, Direction: configured.Direction, Continuity: "complete"}
		if err := store.UpsertTrafficPeriod(ctx, serverID, period); err != nil {
			t.Fatal(err)
		}
	}
	wire := func(sequence uint64, at time.Time, rx0, tx1 string) contracts.MetricSample {
		return contracts.MetricSample{ServerID: serverID, CollectorEpoch: "forecast-direction-epoch-01", Sequence: sequence, ObservedAt: at, ReceivedAt: at, Values: map[string]float64{}, Counters: map[string]string{"net.eth0.rx_bytes": rx0, "net.eth0.tx_bytes": "0", "net.eth1.rx_bytes": "0", "net.eth1.tx_bytes": tx1}, Validity: map[string]string{"net.eth0.rx_bytes": "valid", "net.eth0.tx_bytes": "valid", "net.eth1.rx_bytes": "valid", "net.eth1.tx_bytes": "valid"}}
	}
	if _, err := store.IngestSamples(ctx, serverID, []contracts.MetricSample{wire(0, base, "0", "0"), wire(1, base.Add(24*time.Hour), "100", "1000")}, nil); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	asOf := base.Add(24 * time.Hour)
	service.Now = func() time.Time { return asOf }
	requestForecast := func(direction string) contracts.TrafficForecast {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/servers/"+string(serverID)+"/traffic/forecast?scope=host&direction="+direction+"&as_of="+asOf.Format(time.RFC3339Nano), nil)
		resp := httptest.NewRecorder()
		service.Handler().ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("%s forecast status=%d body=%s", direction, resp.Code, resp.Body.String())
		}
		var result contracts.TrafficForecast
		if err := json.Unmarshal(resp.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := requestForecast("inbound"); !result.Available || result.ObservedBytes != 100 {
		t.Fatalf("inbound forecast=%+v", result)
	}
	if result := requestForecast("outbound"); !result.Available || result.ObservedBytes != 1000 {
		t.Fatalf("outbound forecast=%+v", result)
	}
}

func TestTrafficConfigurationInstallsWarningRules(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-traffic-warnings-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Warnings", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	service.Now = func() time.Time { return time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC) }
	body := `{"idempotency_key":"traffic-warning-setup","expected_revision":"0","scope":"host","allowance_bytes":"1000","reset_day":1,"timezone":"UTC","direction":"combined","warning_percentages":[80,90,100]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/servers/"+string(serverID)+"/traffic", bytes.NewBufferString(body))
	resp := httptest.NewRecorder()
	service.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("configure status=%d body=%s", resp.Code, resp.Body.String())
	}
	page, err := store.ListAlertRules(ctx, monitoring.MaxPageItems, "")
	if err != nil {
		t.Fatal(err)
	}
	thresholds := map[string]bool{}
	for _, rule := range page.Items {
		if strings.HasPrefix(rule.Expression, "traffic.usage_percent >= ") {
			thresholds[rule.Expression] = true
		}
	}
	for _, threshold := range []string{"80", "90", "100"} {
		if !thresholds["traffic.usage_percent >= "+threshold] {
			t.Fatalf("missing traffic warning rule at %s%%: %+v", threshold, page.Items)
		}
	}
	body = `{"idempotency_key":"traffic-warning-update","expected_revision":"1","scope":"host","allowance_bytes":"1000","reset_day":1,"timezone":"UTC","direction":"combined","warning_percentages":[90]}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/servers/"+string(serverID)+"/traffic", bytes.NewBufferString(body))
	resp = httptest.NewRecorder()
	service.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("warning update status=%d body=%s", resp.Code, resp.Body.String())
	}
	page, err = store.ListAlertRules(ctx, monitoring.MaxPageItems, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range page.Items {
		if rule.Expression == "traffic.usage_percent >= 80" && rule.Enabled {
			t.Fatal("removed generated warning remained enabled")
		}
	}
}

func TestProcessorChargesAcceptedCounterSamplesOnce(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-traffic-processor-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Processor", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	allowance := allowance(1, "UTC")
	if err := store.UpsertTrafficAllowance(ctx, serverID, allowance); err != nil {
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
	base := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	wireSample := func(sequence uint64, rx, tx string, at time.Time) contracts.NodeMetricSample {
		return contracts.NodeMetricSample{ServerID: serverID, CollectorEpoch: "processor-epoch-01", Sequence: sequence, ObservedAt: at, Values: map[string]float64{"cpu.utilization": 10}, Counters: map[string]string{"net.billing.rx_bytes": rx, "net.billing.tx_bytes": tx}, Validity: map[string]string{"net.billing.rx_bytes": "valid", "net.billing.tx_bytes": "valid", "cpu.utilization": "valid"}}
	}
	first := wireSample(0, "100", "200", base)
	storedFirst, err := first.WithReceivedAt(base.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	firstResult, err := store.IngestNodeBatch(ctx, serverID, []contracts.NodeMetricSample{first}, nil, base.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := processor.ProcessSample(ctx, storedFirst, nil, firstResult.Inserted == 1); err != nil {
		t.Fatal(err)
	}
	second := wireSample(1, "150", "250", base.Add(time.Minute))
	previous := storedFirst
	storedSecond, err := second.WithReceivedAt(base.Add(time.Minute + time.Second))
	if err != nil {
		t.Fatal(err)
	}
	secondResult, err := store.IngestNodeBatch(ctx, serverID, []contracts.NodeMetricSample{second}, nil, base.Add(time.Minute+time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := processor.ProcessSample(ctx, storedSecond, &previous, secondResult.Inserted == 1); err != nil {
		t.Fatal(err)
	}
	if _, err := processor.ProcessSample(ctx, storedSecond, &previous, false); err != nil {
		t.Fatal(err)
	}
	page, err := store.QueryTrafficPeriods(ctx, serverID, time.Time{}, time.Time{}, "host", 10, "")
	if err != nil || len(page.Periods) != 1 || page.Periods[0].CountedBytes != 100 {
		t.Fatalf("processor traffic=%+v err=%v", page, err)
	}
}

func TestProcessorReplaysDurableQueueExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-traffic-queue-replay-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Queue replay", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	a := allowance(1, "UTC")
	a.WarningPercentages = []uint8{80}
	if err := store.UpsertTrafficAllowance(ctx, serverID, a); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	wire := func(sequence uint64, rx string, at time.Time) contracts.NodeMetricSample {
		return contracts.NodeMetricSample{ServerID: serverID, CollectorEpoch: "queue-replay-epoch-01", Sequence: sequence, ObservedAt: at, Values: map[string]float64{"cpu.utilization": 10}, Counters: map[string]string{"net.billing.rx_bytes": rx, "net.billing.tx_bytes": "0"}, Validity: map[string]string{"net.billing.rx_bytes": "valid", "net.billing.tx_bytes": "valid", "cpu.utilization": "valid"}}
	}
	first, err := wire(0, "100", base).WithReceivedAt(base.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	second, err := wire(1, "200", base.Add(time.Minute)).WithReceivedAt(base.Add(time.Minute + time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.IngestSamples(ctx, serverID, []contracts.MetricSample{first, second}, nil); err != nil {
		t.Fatal(err)
	}
	// Simulate a process restart: the original observer was not installed and
	// a fresh processor drains the durable post-commit queue.
	engine, err := alerts.NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	processor, err := NewProcessor(store, engine)
	if err != nil {
		t.Fatal(err)
	}
	if err := processor.ObserveIngestion(ctx, nil); err != nil {
		t.Fatal(err)
	}
	page, err := store.QueryTrafficPeriods(ctx, serverID, time.Time{}, time.Time{}, "host", 10, "")
	if err != nil || len(page.Periods) != 1 || page.Periods[0].CountedBytes != 100 {
		t.Fatalf("replayed traffic=%+v err=%v", page, err)
	}
	if err := processor.ObserveIngestion(ctx, nil); err != nil {
		t.Fatal(err)
	}
	page, err = store.QueryTrafficPeriods(ctx, serverID, time.Time{}, time.Time{}, "host", 10, "")
	if err != nil || len(page.Periods) != 1 || page.Periods[0].CountedBytes != 100 {
		t.Fatalf("duplicate replay charged traffic again: %+v err=%v", page, err)
	}
}

func TestTrafficReplayAfterRawRetentionRemainsDeduplicated(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-traffic-retention-replay-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Retention replay", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	allowance := allowance(1, "UTC")
	allowance.WarningPercentages = []uint8{80}
	if err := store.UpsertTrafficAllowance(ctx, serverID, allowance); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	wire := func(sequence uint64, rx string, at time.Time) contracts.NodeMetricSample {
		return contracts.NodeMetricSample{ServerID: serverID, CollectorEpoch: "retention-replay-epoch-01", Sequence: sequence, ObservedAt: at, Values: map[string]float64{"cpu.utilization": 10}, Counters: map[string]string{"net.billing.rx_bytes": rx, "net.billing.tx_bytes": "0"}, Validity: map[string]string{"net.billing.rx_bytes": "valid", "net.billing.tx_bytes": "valid", "cpu.utilization": "valid"}}
	}
	first, err := wire(0, "100", base).WithReceivedAt(base.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	second, err := wire(1, "200", base.Add(time.Minute)).WithReceivedAt(base.Add(time.Minute + time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.IngestSamples(ctx, serverID, []contracts.MetricSample{first, second}, nil); err != nil {
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
	if err := processor.ObserveIngestion(ctx, nil); err != nil {
		t.Fatal(err)
	}
	page, err := store.QueryTrafficPeriods(ctx, serverID, time.Time{}, time.Time{}, "host", 10, "")
	if err != nil || len(page.Periods) != 1 || page.Periods[0].CountedBytes != 100 {
		t.Fatalf("initial traffic=%+v err=%v", page, err)
	}
	if _, err := store.Prune(ctx, base.Add(48*time.Hour), monitoring.RetentionPolicy{FullResolutionAge: time.Hour, MinuteRollupAge: 2 * 365 * 24 * time.Hour, HourRollupAge: 2 * 365 * 24 * time.Hour, TrafficPeriodAge: 2 * 365 * 24 * time.Hour, LogAge: 2 * 365 * 24 * time.Hour, BatchSize: 200}); err != nil {
		t.Fatal(err)
	}
	result, err := store.IngestSamples(ctx, serverID, []contracts.MetricSample{first, second}, nil)
	if err != nil || result.Inserted != 0 || result.Duplicate != 2 {
		t.Fatalf("retained retransmission was accepted: result=%+v err=%v", result, err)
	}
	if err := processor.ObserveIngestion(ctx, nil); err != nil {
		t.Fatal(err)
	}
	page, err = store.QueryTrafficPeriods(ctx, serverID, time.Time{}, time.Time{}, "host", 10, "")
	if err != nil || len(page.Periods) != 1 || page.Periods[0].CountedBytes != 100 {
		t.Fatalf("retention replay charged traffic again: %+v err=%v", page, err)
	}
}

func TestProcessorTrafficAlertProgressesAcrossSamples(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-traffic-alert-progress-01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Traffic alerts", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	a := allowance(1, "UTC")
	a.WarningPercentages = []uint8{80}
	if err := store.UpsertTrafficAllowance(ctx, serverID, a); err != nil {
		t.Fatal(err)
	}
	engine, err := alerts.NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	rule := alerts.TrafficAlertRules(serverID, a)[0]
	rule.DurationSeconds = 60
	if err := store.SaveAlertRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	processor, err := NewProcessor(store, engine)
	if err != nil {
		t.Fatal(err)
	}
	base := rule.CreatedAt
	wireSample := func(sequence uint64, rx, tx string, at time.Time) contracts.NodeMetricSample {
		return contracts.NodeMetricSample{ServerID: serverID, CollectorEpoch: "traffic-alert-epoch-01", Sequence: sequence, ObservedAt: at, Values: map[string]float64{"cpu.utilization": 10}, Counters: map[string]string{"net.billing.rx_bytes": rx, "net.billing.tx_bytes": tx}, Validity: map[string]string{"net.billing.rx_bytes": "valid", "net.billing.tx_bytes": "valid", "cpu.utilization": "valid"}}
	}
	process := func(sample contracts.NodeMetricSample, previous *contracts.MetricSample) []alerts.Evaluation {
		stored, err := sample.WithReceivedAt(sample.ObservedAt.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.IngestSamples(ctx, serverID, []contracts.MetricSample{stored}, nil); err != nil {
			t.Fatal(err)
		}
		results, err := processor.ProcessSample(ctx, stored, previous, true)
		if err != nil {
			t.Fatal(err)
		}
		return results
	}
	first := wireSample(0, "0", "0", base)
	storedFirst, err := first.WithReceivedAt(base.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	process(first, nil)
	second := wireSample(1, "800", "0", base.Add(30*time.Second))
	storedSecond, err := second.WithReceivedAt(second.ObservedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	results := process(second, &storedFirst)
	for _, result := range results {
		if result.State.RuleID == rule.ID && result.State.State != "pending" {
			t.Fatalf("traffic rule after first qualifying sample=%+v", result)
		}
	}
	third := wireSample(2, "800", "0", base.Add(90*time.Second))
	results = process(third, &storedSecond)
	for _, result := range results {
		if result.State.RuleID == rule.ID {
			if result.State.State != "firing" || !result.Notify {
				t.Fatalf("traffic rule did not fire after sustained samples=%+v", result)
			}
			return
		}
	}
	t.Fatalf("traffic rule evaluation missing: %+v", results)
}
