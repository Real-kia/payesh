package alerts

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func TestConcurrentEnginesUseAlertStateCAS(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engineOne, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	engineTwo, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	rule := contracts.AlertRule{ID: "cas-rule-01", Name: "CAS", Expression: "cpu.utilization > 90", ServerID: serverID, Enabled: true, DurationSeconds: 60, ReminderSeconds: 3600, IdempotencyKey: "cas-rule-idem-01", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := engineOne.AddRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	base := rule.CreatedAt
	obs := Observation{ServerID: serverID, ObservedAt: base, Values: map[string]float64{"cpu.utilization": 95}, Validity: map[string]string{"cpu.utilization": "valid"}, Coverage: 1}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, engine := range []*Engine{engineOne, engineTwo} {
		wg.Add(1)
		go func(e *Engine) {
			defer wg.Done()
			<-start
			_, evalErr := e.Evaluate(ctx, rule, obs, base)
			if evalErr != nil && !errors.Is(evalErr, monitoring.ErrAlertStateConflict) {
				errs <- evalErr
			}
		}(engine)
	}
	close(start)
	wg.Wait()
	close(errs)
	for evalErr := range errs {
		t.Fatal(evalErr)
	}
	state, found, err := store.GetAlertState(ctx, alertStateID(rule.ID, serverID))
	if err != nil || !found || state.Revision == 0 {
		t.Fatalf("CAS state=%+v found=%v err=%v", state, found, err)
	}
	history, err := store.ListAlertHistory(ctx, time.Time{}, time.Time{}, 10, "")
	if err != nil || len(history.Items) != 1 {
		t.Fatalf("concurrent evaluation duplicated history: %+v err=%v", history.Items, err)
	}
}

func TestStaleEvaluationCannotCommitAfterRuleMutation(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := contracts.AlertRule{ID: "stale-evaluation-rule-01", Name: "Stale", Expression: "cpu.utilization > 90", ServerID: serverID, Enabled: true, DurationSeconds: 1, ReminderSeconds: 3600, IdempotencyKey: "stale-evaluation-idem-01", CreatedAt: base}
	if err := engine.AddRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	updated := rule
	updated.Expression = "cpu.utilization > 95"
	if err := store.SaveAlertRule(ctx, updated); err != nil {
		t.Fatal(err)
	}
	_, err = engine.Evaluate(ctx, rule, Observation{ServerID: serverID, ObservedAt: base, Values: map[string]float64{"cpu.utilization": 99}, Validity: map[string]string{"cpu.utilization": "valid"}, Coverage: 1}, base)
	if !errors.Is(err, monitoring.ErrAlertRuleChanged) {
		t.Fatalf("stale evaluation error=%v, want ErrAlertRuleChanged", err)
	}
	if _, found, err := store.GetAlertState(ctx, alertStateID(rule.ID, serverID)); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("stale evaluation created alert state")
	}
}

func TestZeroReminderDisablesSustainedNotifications(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := contracts.AlertRule{ID: "zero-reminder-rule-01", Name: "No reminders", Expression: "cpu.utilization > 90", ServerID: serverID, Enabled: true, DurationSeconds: 1, ReminderSeconds: 0, IdempotencyKey: "zero-reminder-idem-01", CreatedAt: base}
	if err := engine.AddRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	observation := func(at time.Time) Observation {
		return Observation{ServerID: serverID, ObservedAt: at, Values: map[string]float64{"cpu.utilization": 95}, Validity: map[string]string{"cpu.utilization": "valid"}, Coverage: 1}
	}
	if _, err := engine.Evaluate(ctx, rule, observation(base), base); err != nil {
		t.Fatal(err)
	}
	firing, err := engine.Evaluate(ctx, rule, observation(base.Add(time.Second)), base.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if firing.Notification != "firing" {
		t.Fatalf("initial notification=%q, want firing", firing.Notification)
	}
	sustained, err := engine.Evaluate(ctx, rule, observation(base.Add(2*time.Second)), base.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if sustained.Notification != "" {
		t.Fatalf("zero-reminder sustained notification=%q", sustained.Notification)
	}
	history, err := store.ListAlertHistory(ctx, time.Time{}, time.Time{}, 20, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Items) != 2 {
		t.Fatalf("history events=%d, want pending+firing only: %+v", len(history.Items), history.Items)
	}
	if sustained.State.LastNotifiedAt == nil || !sustained.State.LastNotifiedAt.Equal(base.Add(time.Second)) {
		t.Fatalf("last notification timestamp=%v", sustained.State.LastNotifiedAt)
	}
}

func TestZeroReminderDeliversInitialFiringAfterMaintenance(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := contracts.AlertRule{ID: "zero-reminder-maintenance-01", Name: "No reminder maintenance", Expression: "cpu.utilization > 90", ServerID: serverID, Enabled: true, DurationSeconds: 1, ReminderSeconds: 0, IdempotencyKey: "zero-reminder-maintenance-idem-01", CreatedAt: base}
	if err := engine.AddRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	window := contracts.MaintenanceWindow{ID: "zero-reminder-maintenance-window-01", IdempotencyKey: "zero-reminder-maintenance-window-idem-01", StartsAt: base, EndsAt: base.Add(2 * time.Second), ServerIDs: []contracts.ServerID{serverID}}
	if err := store.SaveMaintenanceWindow(ctx, window); err != nil {
		t.Fatal(err)
	}
	observation := func(at time.Time) Observation {
		return Observation{ServerID: serverID, ObservedAt: at, Values: map[string]float64{"cpu.utilization": 95}, Validity: map[string]string{"cpu.utilization": "valid"}, Coverage: 1}
	}
	if _, err := engine.Evaluate(ctx, rule, observation(base), base); err != nil {
		t.Fatal(err)
	}
	suppressed, err := engine.Evaluate(ctx, rule, observation(base.Add(time.Second)), base.Add(time.Second))
	if err != nil || !suppressed.Suppressed || suppressed.Notify || suppressed.State.LastNotifiedAt != nil || suppressed.State.LastSuppressedAt == nil {
		t.Fatalf("suppressed initial firing=%+v err=%v", suppressed, err)
	}
	released, err := engine.Evaluate(ctx, rule, observation(base.Add(2*time.Second)), base.Add(2*time.Second))
	if err != nil || !released.Notify || released.Notification != "firing" || released.State.LastSuppressedAt != nil {
		t.Fatalf("released initial firing=%+v err=%v", released, err)
	}
	sustained, err := engine.Evaluate(ctx, rule, observation(base.Add(3*time.Second)), base.Add(3*time.Second))
	if err != nil || sustained.Notify || sustained.Notification != "" {
		t.Fatalf("zero reminder emitted sustained notification: %+v err=%v", sustained, err)
	}
}

func TestClosedDeliveryQueueLeavesInitialFiringRetryable(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	queue := NewDeliveryQueue(Notifier{}, 1)
	queue.Close()
	engine.ConfigureDelivery(queue, func(context.Context, contracts.ServerID) []NotificationDestination {
		return []NotificationDestination{{Kind: "webhook", URL: "https://example.test/hook", Enabled: true}}
	})
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := contracts.AlertRule{ID: "closed-queue-rule-01", Name: "Closed queue", Expression: "cpu.utilization > 90", ServerID: serverID, Enabled: true, DurationSeconds: 1, ReminderSeconds: 0, IdempotencyKey: "closed-queue-idem-01", CreatedAt: base}
	if err := engine.AddRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	observation := func(at time.Time) Observation {
		return Observation{ServerID: serverID, ObservedAt: at, Values: map[string]float64{"cpu.utilization": 95}, Validity: map[string]string{"cpu.utilization": "valid"}, Coverage: 1}
	}
	if _, err := engine.Evaluate(ctx, rule, observation(base), base); err != nil {
		t.Fatal(err)
	}
	first, err := engine.Evaluate(ctx, rule, observation(base.Add(time.Second)), base.Add(time.Second))
	if err != nil || first.Notify || first.Notification != "firing" || first.State.LastNotifiedAt != nil {
		t.Fatalf("closed-queue firing=%+v err=%v", first, err)
	}
	retry, err := engine.Evaluate(ctx, rule, observation(base.Add(2*time.Second)), base.Add(2*time.Second))
	if err != nil || retry.Notify || retry.Notification != "firing" || retry.State.LastNotifiedAt != nil {
		t.Fatalf("closed-queue retry=%+v err=%v", retry, err)
	}
}

func TestTerminalDeliveryFailureLeavesCadenceRetryable(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	queue := NewDeliveryQueue(Notifier{}, 1)
	queue.send = func(context.Context, NotificationDestination, contracts.AlertHistoryEvent) error {
		return errors.New("retry policy exhausted")
	}
	defer queue.Close()
	engine.ConfigureDelivery(queue, func(context.Context, contracts.ServerID) []NotificationDestination {
		return []NotificationDestination{{Kind: "webhook", URL: "https://example.test/hook", Secret: "memory-only", Enabled: true}}
	})
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := contracts.AlertRule{ID: "failed-delivery-rule-01", Name: "Failed delivery", Expression: "cpu.utilization > 90", ServerID: serverID, Enabled: true, DurationSeconds: 1, ReminderSeconds: 0, IdempotencyKey: "failed-delivery-idem-01", CreatedAt: base}
	if err := engine.AddRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	observation := func(at time.Time) Observation {
		return Observation{ServerID: serverID, ObservedAt: at, Values: map[string]float64{"cpu.utilization": 95}, Validity: map[string]string{"cpu.utilization": "valid"}, Coverage: 1}
	}
	if _, err := engine.Evaluate(ctx, rule, observation(base), base); err != nil {
		t.Fatal(err)
	}
	first, err := engine.Evaluate(ctx, rule, observation(base.Add(time.Second)), base.Add(time.Second))
	if err != nil || !first.Notify || first.State.LastNotifiedAt != nil {
		t.Fatalf("accepted failing delivery=%+v err=%v", first, err)
	}
	select {
	case <-queue.Failures():
	case <-time.After(time.Second):
		t.Fatal("terminal notifier failure was not reported")
	}
	state, found, err := store.GetAlertState(ctx, alertStateID(rule.ID, serverID))
	if err != nil || !found || state.LastNotifiedAt != nil {
		t.Fatalf("failed delivery consumed cadence: state=%+v found=%v err=%v", state, found, err)
	}
	retry, err := engine.Evaluate(ctx, rule, observation(base.Add(2*time.Second)), base.Add(2*time.Second))
	if err != nil || !retry.Notify || retry.Notification != "firing" || retry.State.LastNotifiedAt != nil {
		t.Fatalf("terminal failure retry=%+v err=%v", retry, err)
	}
}

func TestSuccessfulDeliveryCommitsCadenceAndPreventsDuplicate(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	sent := make(chan contracts.AlertHistoryEvent, 2)
	queue := NewDeliveryQueue(Notifier{}, 1)
	queue.send = func(_ context.Context, _ NotificationDestination, event contracts.AlertHistoryEvent) error {
		sent <- event
		return nil
	}
	defer queue.Close()
	engine.ConfigureDelivery(queue, func(context.Context, contracts.ServerID) []NotificationDestination {
		return []NotificationDestination{{Kind: "webhook", URL: "https://example.test/hook", Enabled: true}}
	})
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := contracts.AlertRule{ID: "successful-delivery-rule-01", Name: "Successful delivery", Expression: "cpu.utilization > 90", ServerID: serverID, Enabled: true, DurationSeconds: 1, ReminderSeconds: 0, IdempotencyKey: "successful-delivery-idem-01", CreatedAt: base}
	if err := engine.AddRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	observation := func(at time.Time) Observation {
		return Observation{ServerID: serverID, ObservedAt: at, Values: map[string]float64{"cpu.utilization": 95}, Validity: map[string]string{"cpu.utilization": "valid"}, Coverage: 1}
	}
	if _, err := engine.Evaluate(ctx, rule, observation(base), base); err != nil {
		t.Fatal(err)
	}
	deliveredAt := base.Add(time.Second)
	if result, err := engine.Evaluate(ctx, rule, observation(deliveredAt), deliveredAt); err != nil || !result.Notify {
		t.Fatalf("successful firing=%+v err=%v", result, err)
	}
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("notification worker did not run")
	}
	deadline := time.Now().Add(time.Second)
	for {
		state, found, stateErr := store.GetAlertState(ctx, alertStateID(rule.ID, serverID))
		if stateErr != nil {
			t.Fatal(stateErr)
		}
		if found && state.LastNotifiedAt != nil && state.LastNotifiedAt.Equal(deliveredAt) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("successful delivery did not commit cadence: %+v", state)
		}
		time.Sleep(time.Millisecond)
	}
	retry, err := engine.Evaluate(ctx, rule, observation(base.Add(2*time.Second)), base.Add(2*time.Second))
	if err != nil || retry.Notify || retry.Notification != "" {
		t.Fatalf("successful delivery duplicated: %+v err=%v", retry, err)
	}
	select {
	case duplicate := <-sent:
		t.Fatalf("duplicate delivery=%+v", duplicate)
	default:
	}
}

func TestHistoricalQueuedSampleCannotPrecedeRuleEffectiveAt(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	observedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := contracts.AlertRule{ID: "effective-at-rule-01", Name: "Effective boundary", Expression: "cpu.utilization > 90", ServerID: serverID, Enabled: true, DurationSeconds: 1, ReminderSeconds: 60, IdempotencyKey: "effective-at-rule-idem-01", CreatedAt: observedAt.Add(time.Hour)}
	if err := engine.AddRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	queued := contracts.MetricSample{ServerID: serverID, CollectorEpoch: "effective-at-epoch-01", Sequence: 0, ObservedAt: observedAt, ReceivedAt: rule.CreatedAt.Add(time.Minute), Values: map[string]float64{"cpu.utilization": 99}, Validity: map[string]string{"cpu.utilization": "valid"}}
	results, err := engine.EvaluateSample(ctx, queued, 1)
	if err != nil || len(results) != 1 || results[0].Reason != "observation_before_rule_effective_at" || results[0].Notify {
		t.Fatalf("historical queued evaluation=%+v err=%v", results, err)
	}
	if _, found, err := store.GetAlertState(ctx, alertStateID(rule.ID, serverID)); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("historical queued observation mutated alert state")
	}
	history, err := store.ListAlertHistory(ctx, time.Time{}, time.Time{}, 10, "")
	if err != nil || len(history.Items) != 0 {
		t.Fatalf("historical queued observation emitted history: %+v err=%v", history.Items, err)
	}
}

func alertServer(t *testing.T, store *monitoring.Store) contracts.ServerID {
	t.Helper()
	id := contracts.ServerID("server-alerts-0123")
	err := store.EnsureServer(context.Background(), contracts.Server{ID: id, Name: "Alerts", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestEngineUsesTimestampDurationHysteresisAndPersistence(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	recovery := 85.0
	rule := contracts.AlertRule{ID: "cpu-high-01", Name: "CPU high", Expression: "cpu.utilization > 90", ServerID: serverID, Enabled: true, DurationSeconds: 300, RecoveryThreshold: &recovery, ReminderSeconds: 3600, GroupKey: "cpu", IdempotencyKey: "rule-idem-01", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := engine.AddRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	base := rule.CreatedAt
	observation := func(at time.Time, value float64) Observation {
		return Observation{ServerID: serverID, ObservedAt: at, Values: map[string]float64{"cpu.utilization": value}, Validity: map[string]string{"cpu.utilization": "valid"}, Coverage: 1}
	}
	evaluation, err := engine.Evaluate(ctx, rule, observation(base, 95), base)
	if err != nil || evaluation.State.State != "pending" || evaluation.Notify {
		t.Fatalf("pending=%+v err=%v", evaluation, err)
	}
	evaluation, err = engine.Evaluate(ctx, rule, observation(base.Add(299*time.Second), 96), base.Add(299*time.Second))
	if err != nil || evaluation.State.State != "pending" {
		t.Fatalf("early=%+v err=%v", evaluation, err)
	}
	evaluation, err = engine.Evaluate(ctx, rule, observation(base.Add(5*time.Minute), 96), base.Add(5*time.Minute))
	if err != nil || evaluation.State.State != "firing" || !evaluation.Notify || evaluation.Notification != "firing" {
		t.Fatalf("firing=%+v err=%v", evaluation, err)
	}
	if evaluation.State.IncidentID == "" {
		t.Fatal("firing alert has no incident")
	}
	incident, found, err := store.GetIncident(ctx, evaluation.State.IncidentID)
	if err != nil || !found || incident.State != "open" {
		t.Fatalf("incident=%+v found=%v err=%v", incident, found, err)
	}
	// 88 is still above the configured recovery threshold and must not flap.
	evaluation, err = engine.Evaluate(ctx, rule, observation(base.Add(6*time.Minute), 88), base.Add(6*time.Minute))
	if err != nil || evaluation.State.State != "firing" {
		t.Fatalf("hysteresis recovered too early: %+v err=%v", evaluation, err)
	}
	evaluation, err = engine.Evaluate(ctx, rule, observation(base.Add(7*time.Minute), 84), base.Add(7*time.Minute))
	if err != nil || evaluation.State.State != "recovered" || !evaluation.Notify || evaluation.Notification != "recovered" {
		t.Fatalf("recovery=%+v err=%v", evaluation, err)
	}
	incident, found, err = store.GetIncident(ctx, evaluation.State.IncidentID)
	if err != nil || !found || incident.State != "recovered" || incident.EndedAt == nil {
		t.Fatalf("closed incident=%+v found=%v err=%v", incident, found, err)
	}
	if _, err := engine.Evaluate(ctx, rule, observation(base.Add(8*time.Minute), 96), base.Add(8*time.Minute)); err != nil {
		t.Fatal("re-fire pending:", err)
	}
	evaluation, err = engine.Evaluate(ctx, rule, observation(base.Add(13*time.Minute), 96), base.Add(13*time.Minute))
	if err != nil || evaluation.State.State != "firing" {
		t.Fatalf("re-fire=%+v err=%v", evaluation, err)
	}
	incident, found, err = store.GetIncident(ctx, evaluation.State.IncidentID)
	if err != nil || !found || incident.State != "open" || incident.EndedAt != nil {
		t.Fatalf("reopened incident=%+v found=%v err=%v", incident, found, err)
	}
	history, err := store.ListAlertHistory(ctx, time.Time{}, time.Time{}, 20, "")
	if err != nil || len(history.Items) < 3 {
		t.Fatalf("history=%+v err=%v", history, err)
	}
}

func TestMaintenanceSuppressesNotificationButNotState(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	rule := contracts.AlertRule{ID: "disk-high-01", Name: "Disk high", Expression: "disk.root.used_percent > 90", ServerID: serverID, Enabled: true, DurationSeconds: 60, ReminderSeconds: 3600, IdempotencyKey: "rule-idem-02", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := engine.AddRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	base := rule.CreatedAt
	window := contracts.MaintenanceWindow{ID: "maintenance-01", IdempotencyKey: "maintenance-idem-01", StartsAt: base, EndsAt: base.Add(90 * time.Second), ServerIDs: []contracts.ServerID{serverID}, Reason: "planned disk work"}
	if err := store.SaveMaintenanceWindow(ctx, window); err != nil {
		t.Fatal(err)
	}
	obs := func(at time.Time) Observation {
		return Observation{ServerID: serverID, ObservedAt: at, Values: map[string]float64{"disk.root.used_percent": 95}, Validity: map[string]string{"disk.root.used_percent": "valid"}, Coverage: 1}
	}
	if _, err := engine.Evaluate(ctx, rule, obs(base), base); err != nil {
		t.Fatal(err)
	}
	evaluation, err := engine.Evaluate(ctx, rule, obs(base.Add(time.Minute)), base.Add(time.Minute))
	if err != nil || evaluation.State.State != "firing" || evaluation.Notify || !evaluation.Suppressed {
		t.Fatalf("maintenance evaluation=%+v err=%v", evaluation, err)
	}
	history, err := store.ListAlertHistory(ctx, time.Time{}, time.Time{}, 20, "")
	if err != nil || len(history.Items) == 0 || history.Items[len(history.Items)-1].State != "suppressed" {
		t.Fatalf("history=%+v err=%v", history, err)
	}
	evaluation, err = engine.Evaluate(ctx, rule, obs(base.Add(2*time.Minute)), base.Add(2*time.Minute))
	if err != nil || !evaluation.Notify || evaluation.Notification != "firing" {
		t.Fatalf("suppressed firing consumed reminder clock: %+v err=%v", evaluation, err)
	}
}

func TestTrafficThresholdUsesAccountingCoverage(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	rule := contracts.AlertRule{ID: "traffic-rule-01", Name: "Traffic", Expression: "traffic.usage_percent > 90", ServerID: serverID, Enabled: true, DurationSeconds: 1, ReminderSeconds: 60, GroupKey: "traffic", IdempotencyKey: "traffic-rule-idem", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := engine.AddRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	period := contracts.TrafficPeriod{Scope: "host", From: rule.CreatedAt, To: rule.CreatedAt.Add(31 * 24 * time.Hour), Timezone: "UTC", AllowanceBytes: 1000, Direction: "combined", CountedBytes: 950, Continuity: "gap"}
	evaluations, err := engine.EvaluateTraffic(ctx, serverID, period, rule.CreatedAt.Add(time.Minute))
	if err != nil || len(evaluations) != 1 || evaluations[0].State.State != "recovered" || evaluations[0].Reason == "" {
		t.Fatalf("gap traffic evaluation=%+v err=%v", evaluations, err)
	}
	period.Continuity = "complete"
	evaluations, err = engine.EvaluateTraffic(ctx, serverID, period, rule.CreatedAt.Add(2*time.Minute))
	if err != nil || len(evaluations) != 1 || evaluations[0].State.State != "pending" {
		t.Fatalf("complete traffic pending evaluation=%+v err=%v", evaluations, err)
	}
	evaluations, err = engine.EvaluateTraffic(ctx, serverID, period, rule.CreatedAt.Add(2*time.Minute+time.Second))
	if err != nil || len(evaluations) != 1 || evaluations[0].State.State != "firing" {
		t.Fatalf("complete traffic firing evaluation=%+v err=%v", evaluations, err)
	}
}

func TestTrafficRulesStayScopedToTheirAllowance(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	host := contracts.TrafficAllowance{Scope: "host", Direction: "combined", AllowanceBytes: 1000, ResetDay: 1, Timezone: "UTC", WarningPercentages: []uint8{80}}
	iface := contracts.TrafficAllowance{Scope: "eth0", Direction: "combined", AllowanceBytes: 1000, ResetDay: 1, Timezone: "UTC", WarningPercentages: []uint8{80}}
	if err := engine.EnsureTrafficRules(ctx, serverID, host); err != nil {
		t.Fatal(err)
	}
	if err := engine.EnsureTrafficRules(ctx, serverID, iface); err != nil {
		t.Fatal(err)
	}
	generated, found, err := store.GetAlertRule(ctx, TrafficAlertRules(serverID, host)[0].ID)
	if err != nil || !found {
		t.Fatalf("generated host rule found=%v err=%v", found, err)
	}
	base := generated.EffectiveAt
	period := contracts.TrafficPeriod{Scope: "host", From: base, To: base.Add(31 * 24 * time.Hour), Timezone: "UTC", AllowanceBytes: 1000, Direction: "combined", CountedBytes: 900, Continuity: "complete"}
	evaluations, err := engine.EvaluateTraffic(ctx, serverID, period, base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluations) != 1 || evaluations[0].State.RuleID != TrafficAlertRules(serverID, host)[0].ID {
		t.Fatalf("host evaluation crossed allowance scope: %+v", evaluations)
	}
	ifacePeriod := period
	ifacePeriod.Scope = "eth0"
	evaluations, err = engine.EvaluateTraffic(ctx, serverID, ifacePeriod, base.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluations) != 1 || evaluations[0].State.RuleID != TrafficAlertRules(serverID, iface)[0].ID {
		t.Fatalf("interface evaluation crossed allowance scope: %+v", evaluations)
	}
}

func TestOwnerTrafficRuleStillEvaluatesAlongsideScopedDefaults(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	allowance := contracts.TrafficAllowance{Scope: "host", Direction: "combined", AllowanceBytes: 1000, ResetDay: 1, Timezone: "UTC", WarningPercentages: []uint8{80}}
	if err := engine.EnsureTrafficRules(ctx, serverID, allowance); err != nil {
		t.Fatal(err)
	}
	ownerRule := contracts.AlertRule{ID: "owner-traffic-rule-01", Name: "Owner traffic", Expression: "traffic.usage_percent >= 80", ServerID: serverID, Enabled: true, DurationSeconds: 1, ReminderSeconds: 60, GroupKey: "traffic", IdempotencyKey: "owner-traffic-idem-01", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := engine.AddRule(ctx, ownerRule); err != nil {
		t.Fatal(err)
	}
	period := contracts.TrafficPeriod{Scope: "host", From: ownerRule.CreatedAt, To: ownerRule.CreatedAt.Add(31 * 24 * time.Hour), Timezone: "UTC", AllowanceBytes: 1000, Direction: "combined", CountedBytes: 900, Continuity: "complete"}
	evaluations, err := engine.EvaluateTraffic(ctx, serverID, period, ownerRule.CreatedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	foundOwner := false
	for _, evaluation := range evaluations {
		if evaluation.State.RuleID == ownerRule.ID {
			foundOwner = true
		}
	}
	if !foundOwner {
		t.Fatalf("owner-created unscoped traffic rule was suppressed: %+v", evaluations)
	}
}

func TestEnsureTrafficRulesReenablesUntouchedGeneratedRule(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	allowance := contracts.TrafficAllowance{Scope: "host", Direction: "combined", AllowanceBytes: 1000, ResetDay: 1, Timezone: "UTC", WarningPercentages: []uint8{80}}
	if err := engine.EnsureTrafficRules(ctx, serverID, allowance); err != nil {
		t.Fatal(err)
	}
	rule := TrafficAlertRules(serverID, allowance)[0]
	rule.Enabled = false
	rule.DisableReason = "threshold_removed"
	if err := store.SaveAlertRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	if err := engine.EnsureTrafficRules(ctx, serverID, allowance); err != nil {
		t.Fatal(err)
	}
	updated, found, err := store.GetAlertRule(ctx, rule.ID)
	if err != nil || !found || !updated.Enabled {
		t.Fatalf("untouched generated rule was not restored: rule=%+v found=%v err=%v", updated, found, err)
	}
}

func TestSparseObservationWarnsWithoutFiringAndOutageSuppressesSecondary(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := contracts.AlertRule{ID: "sparse-rule-01", Name: "Sparse", Expression: "cpu.utilization > 90", ServerID: serverID, Enabled: true, DurationSeconds: 300, ReminderSeconds: 3600, IdempotencyKey: "sparse-idem-01", CreatedAt: base}
	if err := engine.AddRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	obs := func(at time.Time) Observation {
		return Observation{ServerID: serverID, ObservedAt: at, Values: map[string]float64{"cpu.utilization": 95}, Validity: map[string]string{"cpu.utilization": "valid"}, Coverage: 1}
	}
	if _, err := engine.Evaluate(ctx, rule, obs(base), base); err != nil {
		t.Fatal(err)
	}
	evaluation, err := engine.Evaluate(ctx, rule, obs(base.Add(10*time.Minute)), base.Add(10*time.Minute))
	if err != nil || evaluation.State.State != "pending" || evaluation.Reason != "sampling_interval_exceeds_rule_duration" {
		t.Fatalf("sparse evaluation=%+v err=%v", evaluation, err)
	}
	metricRule := contracts.AlertRule{ID: "secondary-rule-01", Name: "Secondary", Expression: "memory.used_percent > 90", ServerID: serverID, Enabled: true, DurationSeconds: 1, ReminderSeconds: 1, GroupKey: "memory", IdempotencyKey: "secondary-idem-01", CreatedAt: base}
	if err := engine.AddRule(ctx, metricRule); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.EvaluateUnreachable(ctx, serverID, base.Add(20*time.Minute), true); err != nil {
		t.Fatal(err)
	}
	secondaryObs := Observation{ServerID: serverID, ObservedAt: base.Add(20 * time.Minute), Values: map[string]float64{"memory.used_percent": 95}, Validity: map[string]string{"memory.used_percent": "valid"}, Coverage: 1}
	if _, err := engine.Evaluate(ctx, metricRule, secondaryObs, secondaryObs.ObservedAt); err != nil {
		t.Fatal(err)
	}
	evaluation, err = engine.Evaluate(ctx, metricRule, Observation{ServerID: serverID, ObservedAt: base.Add(20*time.Minute + time.Second), Values: map[string]float64{"memory.used_percent": 95}, Validity: map[string]string{"memory.used_percent": "valid"}, Coverage: 1}, base.Add(20*time.Minute+time.Second))
	if err != nil || !evaluation.Suppressed || evaluation.Notify {
		t.Fatalf("secondary outage notification was not suppressed: %+v err=%v", evaluation, err)
	}
}

func TestAlertDurationUsesObservationTimestamps(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := contracts.AlertRule{ID: "delayed-rule-01", Name: "Delayed", Expression: "cpu.utilization > 90", ServerID: serverID, Enabled: true, DurationSeconds: 5 * 60, ReminderSeconds: 3600, IdempotencyKey: "delayed-idem-01", CreatedAt: base}
	if err := engine.AddRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	obs := func(at time.Time) Observation {
		return Observation{ServerID: serverID, ObservedAt: at, Values: map[string]float64{"cpu.utilization": 95}, Validity: map[string]string{"cpu.utilization": "valid"}, Coverage: 1}
	}
	if _, err := engine.Evaluate(ctx, rule, obs(base), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	evaluation, err := engine.Evaluate(ctx, rule, obs(base.Add(time.Minute)), base.Add(time.Hour+time.Minute))
	if err != nil || evaluation.State.State != "pending" {
		t.Fatalf("delayed samples fired too early: %+v err=%v", evaluation, err)
	}
}

func TestUncertainObservationRestartsPendingEvidence(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := contracts.AlertRule{ID: "uncertain-rule-01", Name: "Uncertain", Expression: "cpu.utilization > 90", ServerID: serverID, Enabled: true, DurationSeconds: 5 * 60, ReminderSeconds: 3600, IdempotencyKey: "uncertain-idem-01", CreatedAt: base}
	if err := engine.AddRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	valid := func(at time.Time, coverage float64) Observation {
		return Observation{ServerID: serverID, ObservedAt: at, Values: map[string]float64{"cpu.utilization": 95}, Validity: map[string]string{"cpu.utilization": "valid"}, Coverage: coverage}
	}
	if _, err := engine.Evaluate(ctx, rule, valid(base, 1), base); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Evaluate(ctx, rule, valid(base.Add(time.Minute), 0.2), base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	evaluation, err := engine.Evaluate(ctx, rule, valid(base.Add(5*time.Minute+59*time.Second), 1), base.Add(5*time.Minute+59*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.State.State != "pending" || evaluation.Notify {
		t.Fatalf("uncertain gap allowed firing: %+v", evaluation)
	}
}

func TestStarterRulesDoNotOverwriteOwnerEdits(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.EnsureStarterRules(ctx, serverID); err != nil {
		t.Fatal(err)
	}
	rules, err := store.ListAlertRules(ctx, monitoring.MaxPageItems, "")
	if err != nil || len(rules.Items) == 0 {
		t.Fatalf("starter rules=%+v err=%v", rules, err)
	}
	edited := rules.Items[0]
	edited.Name = "Owner-customized"
	edited.DurationSeconds = 600
	if err := store.SaveAlertRule(ctx, edited); err != nil {
		t.Fatal(err)
	}
	if err := engine.EnsureStarterRules(ctx, serverID); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.GetAlertRule(ctx, edited.ID)
	if err != nil || !found || got.Name != edited.Name || got.DurationSeconds != edited.DurationSeconds {
		t.Fatalf("starter reconciliation overwrote edit: got=%+v found=%v err=%v", got, found, err)
	}
}

func TestStarterReconciliationDisablesLegacyTrafficAndScopedRuleEvaluates(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	legacy := contracts.AlertRule{ID: "traffic-high-legacy01", Name: "Owner edited legacy traffic", Expression: "traffic.usage_percent >= 70", ServerID: serverID, Enabled: true, DurationSeconds: 1, ReminderSeconds: 0, GroupKey: "traffic", IdempotencyKey: "starter-legacy-traffic01", CreatedAt: base}
	if err := engine.AddRule(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	period := contracts.TrafficPeriod{Scope: "host", From: base, To: base.Add(31 * 24 * time.Hour), Timezone: "UTC", AllowanceBytes: 1000, Direction: "combined", CountedBytes: 900, Continuity: "complete"}
	if _, err := engine.Evaluate(ctx, legacy, Observation{ServerID: serverID, ObservedAt: base, Values: map[string]float64{"traffic.usage_percent": 90}, Validity: map[string]string{"traffic.usage_percent": "valid"}, Coverage: 1}, base); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Evaluate(ctx, legacy, Observation{ServerID: serverID, ObservedAt: base.Add(time.Second), Values: map[string]float64{"traffic.usage_percent": 90}, Validity: map[string]string{"traffic.usage_percent": "valid"}, Coverage: 1}, base.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := engine.EnsureStarterRules(ctx, serverID); err != nil {
		t.Fatal(err)
	}
	gotLegacy, found, err := store.GetAlertRule(ctx, legacy.ID)
	if err != nil || !found || gotLegacy.Enabled || gotLegacy.DisableReason != "superseded_by_allowance_scoped_traffic_rules" {
		t.Fatalf("legacy traffic starter=%+v found=%v err=%v", gotLegacy, found, err)
	}
	state, found, err := store.GetAlertState(ctx, alertStateID(legacy.ID, serverID))
	if err != nil || !found || state.State != "recovered" {
		t.Fatalf("legacy traffic state=%+v found=%v err=%v", state, found, err)
	}
	for _, starter := range StarterRules(serverID) {
		metric, _, _, parseErr := ParseExpression(starter.Expression)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		if metric == "traffic" || strings.HasPrefix(metric, "traffic.") {
			t.Fatalf("generic starter still includes dead traffic rule: %+v", starter)
		}
	}
	allowance := contracts.TrafficAllowance{Scope: "host", Direction: "combined", AllowanceBytes: 1000, ResetDay: 1, Timezone: "UTC", WarningPercentages: []uint8{80}}
	if err := engine.EnsureTrafficRules(ctx, serverID, allowance); err != nil {
		t.Fatal(err)
	}
	scoped, found, err := store.GetAlertRule(ctx, TrafficAlertRules(serverID, allowance)[0].ID)
	if err != nil || !found || !scoped.Enabled {
		t.Fatalf("scoped traffic rule=%+v found=%v err=%v", scoped, found, err)
	}
	period.From = scoped.EffectiveAt
	period.To = scoped.EffectiveAt.Add(31 * 24 * time.Hour)
	evaluations, err := engine.EvaluateTraffic(ctx, serverID, period, scoped.EffectiveAt)
	if err != nil || len(evaluations) != 1 || evaluations[0].State.RuleID != scoped.ID {
		t.Fatalf("scoped traffic evaluation=%+v err=%v", evaluations, err)
	}
}

func TestRuleEffectiveAtAdvancesOnReenableAndMaterialEdit(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := contracts.AlertRule{ID: "policy-boundary-rule-01", Name: "Policy boundary", Expression: "cpu.utilization > 90", ServerID: serverID, Enabled: true, DurationSeconds: 60, ReminderSeconds: 60, IdempotencyKey: "policy-boundary-idem-01", CreatedAt: base}
	if err := engine.AddRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	rule, _, err = store.GetAlertRule(ctx, rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	first := Observation{ServerID: serverID, ObservedAt: base.Add(time.Hour), Values: map[string]float64{"cpu.utilization": 95}, Validity: map[string]string{"cpu.utilization": "valid"}, Coverage: 1}
	if _, err := engine.Evaluate(ctx, rule, first, first.ObservedAt); err != nil {
		t.Fatal(err)
	}
	disabled := rule
	disabled.Enabled = false
	if err := store.SaveAlertRule(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	reenabled := disabled
	reenabled.Enabled = true
	if err := store.SaveAlertRule(ctx, reenabled); err != nil {
		t.Fatal(err)
	}
	reenabled, _, err = store.GetAlertRule(ctx, rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	queuedDuringDisable := contracts.MetricSample{ServerID: serverID, CollectorEpoch: "policy-boundary-epoch-01", Sequence: 0, ObservedAt: base.Add(2 * time.Hour), ReceivedAt: reenabled.EffectiveAt.Add(time.Second), Values: map[string]float64{"cpu.utilization": 99}, Validity: map[string]string{"cpu.utilization": "valid"}}
	results, err := engine.EvaluateSample(ctx, queuedDuringDisable, 1)
	if err != nil || len(results) != 1 || results[0].Reason != "observation_before_rule_effective_at" {
		t.Fatalf("disabled-interval queued sample=%+v err=%v", results, err)
	}
	beforeEdit := reenabled.EffectiveAt
	reenabled.Expression = "cpu.utilization > 98"
	if err := store.SaveAlertRule(ctx, reenabled); err != nil {
		t.Fatal(err)
	}
	edited, _, err := store.GetAlertRule(ctx, rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !edited.EffectiveAt.After(beforeEdit) {
		t.Fatalf("material edit did not advance effective_at: before=%v after=%v", beforeEdit, edited.EffectiveAt)
	}
	queuedOldPolicy := contracts.MetricSample{ServerID: serverID, CollectorEpoch: "policy-boundary-epoch-01", Sequence: 1, ObservedAt: beforeEdit, ReceivedAt: edited.EffectiveAt.Add(time.Second), Values: map[string]float64{"cpu.utilization": 99}, Validity: map[string]string{"cpu.utilization": "valid"}}
	results, err = engine.EvaluateSample(ctx, queuedOldPolicy, 1)
	if err != nil || len(results) != 1 || results[0].Reason != "observation_before_rule_effective_at" {
		t.Fatalf("old-policy queued sample=%+v err=%v", results, err)
	}
	state, found, err := store.GetAlertState(ctx, alertStateID(rule.ID, serverID))
	if err != nil || !found || state.State != "recovered" || state.LastObservation != nil || state.LastValue != nil {
		t.Fatalf("policy edit did not reset evidence: state=%+v found=%v err=%v", state, found, err)
	}
}

func TestDisabledRuleIsSkippedWithoutBlockingSampleProcessing(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	rule := contracts.AlertRule{
		ID: "disabled-sample-rule-01", Name: "Disabled sample", Expression: "cpu.utilization > 90",
		ServerID: serverID, Enabled: true, DurationSeconds: 1, ReminderSeconds: 60,
		IdempotencyKey: "disabled-sample-rule-idem-01", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := engine.AddRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	rule, found, err := store.GetAlertRule(ctx, rule.ID)
	if err != nil || !found {
		t.Fatalf("created rule found=%v err=%v", found, err)
	}
	rule.Enabled = false
	if err := store.SaveAlertRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	rule, found, err = store.GetAlertRule(ctx, rule.ID)
	if err != nil || !found || rule.Enabled {
		t.Fatalf("disabled rule found=%v rule=%+v err=%v", found, rule, err)
	}
	sample := contracts.MetricSample{
		ServerID: serverID, CollectorEpoch: "disabled-sample-epoch-01", Sequence: 0,
		ObservedAt: rule.CreatedAt.Add(time.Hour), ReceivedAt: rule.CreatedAt.Add(time.Hour),
		Values: map[string]float64{"cpu.utilization": 95}, Validity: map[string]string{"cpu.utilization": "valid"},
	}
	results, err := engine.EvaluateSample(ctx, sample, 1)
	if err != nil || len(results) != 1 || results[0].Reason != "rule_disabled" {
		t.Fatalf("disabled sample evaluation=%+v err=%v", results, err)
	}
	if _, found, err := store.GetAlertState(ctx, alertStateID(rule.ID, serverID)); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("disabled rule created alert state while processing sample")
	}
}

func TestGroupedIncidentStaysOpenUntilEveryAlertRecovers(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rules := []contracts.AlertRule{
		{ID: "disk-capacity-group-01", Name: "Disk capacity", Expression: "disk.root.used_percent > 90", ServerID: serverID, Enabled: true, DurationSeconds: 1, ReminderSeconds: 3600, GroupKey: "disk", IdempotencyKey: "disk-capacity-group-idem", CreatedAt: base},
		{ID: "disk-inodes-group-01", Name: "Disk inodes", Expression: "disk.root.inodes_used_percent > 90", ServerID: serverID, Enabled: true, DurationSeconds: 1, ReminderSeconds: 3600, GroupKey: "disk", IdempotencyKey: "disk-inodes-group-idem", CreatedAt: base},
	}
	for _, rule := range rules {
		if err := engine.AddRule(ctx, rule); err != nil {
			t.Fatal(err)
		}
	}
	observation := func(at time.Time, metric string, value float64) Observation {
		return Observation{ServerID: serverID, ObservedAt: at, Values: map[string]float64{metric: value}, Validity: map[string]string{metric: "valid"}, Coverage: 1}
	}
	for _, rule := range rules {
		metric, _, _, _ := ParseExpression(rule.Expression)
		if _, err := engine.Evaluate(ctx, rule, observation(base, metric, 95), base); err != nil {
			t.Fatal(err)
		}
	}
	var first, second Evaluation
	metric, _, _, _ := ParseExpression(rules[0].Expression)
	first, err = engine.Evaluate(ctx, rules[0], observation(base.Add(time.Second), metric, 95), base.Add(time.Second))
	if err != nil || !first.Notify {
		t.Fatalf("first firing=%+v err=%v", first, err)
	}
	metric, _, _, _ = ParseExpression(rules[1].Expression)
	second, err = engine.Evaluate(ctx, rules[1], observation(base.Add(time.Second), metric, 95), base.Add(time.Second))
	if err != nil || second.State.IncidentID != first.State.IncidentID || !second.Notify {
		t.Fatalf("second firing=%+v first=%+v err=%v", second, first, err)
	}
	metric, _, _, _ = ParseExpression(rules[0].Expression)
	if _, err := engine.Evaluate(ctx, rules[0], observation(base.Add(2*time.Second), metric, 0), base.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	incident, found, err := store.GetIncident(ctx, first.State.IncidentID)
	if err != nil || !found || incident.State != "open" {
		t.Fatalf("incident closed while sibling was firing: %+v found=%v err=%v", incident, found, err)
	}
	metric, _, _, _ = ParseExpression(rules[1].Expression)
	if _, err := engine.Evaluate(ctx, rules[1], observation(base.Add(2*time.Second), metric, 0), base.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	incident, found, err = store.GetIncident(ctx, first.State.IncidentID)
	if err != nil || !found || incident.State != "recovered" {
		t.Fatalf("incident did not recover after both alerts: %+v found=%v err=%v", incident, found, err)
	}
}

func TestEvaluationRejectsStaleRuleSnapshotAfterDisable(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := contracts.AlertRule{ID: "stale-disable-rule-01", Name: "Stale disable", Expression: "cpu.utilization > 90", ServerID: serverID, Enabled: true, DurationSeconds: 1, ReminderSeconds: 3600, IdempotencyKey: "stale-disable-rule-idem", CreatedAt: base}
	if err := engine.AddRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	disabled := rule
	disabled.Enabled = false
	if err := store.SaveAlertRule(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	_, err = engine.Evaluate(ctx, rule, Observation{ServerID: serverID, ObservedAt: base, Values: map[string]float64{"cpu.utilization": 95}, Validity: map[string]string{"cpu.utilization": "valid"}, Coverage: 1}, base)
	if !errors.Is(err, monitoring.ErrAlertRuleChanged) {
		t.Fatalf("stale evaluation error=%v, want ErrAlertRuleChanged", err)
	}
	if _, found, err := store.GetAlertState(ctx, alertStateID(rule.ID, serverID)); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("stale evaluation created state after rule disable")
	}
}

func TestGroupedIncidentConcurrentRecoveryClosesAfterBothStatesCommit(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	engineOne, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	engineTwo, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rules := []contracts.AlertRule{
		{ID: "disk-capacity-race-01", Name: "Disk capacity", Expression: "disk.root.used_percent > 90", ServerID: serverID, Enabled: true, DurationSeconds: 1, ReminderSeconds: 3600, GroupKey: "disk-race", IdempotencyKey: "disk-capacity-race-idem", CreatedAt: base},
		{ID: "disk-inodes-race-01", Name: "Disk inodes", Expression: "disk.root.inodes_used_percent > 90", ServerID: serverID, Enabled: true, DurationSeconds: 1, ReminderSeconds: 3600, GroupKey: "disk-race", IdempotencyKey: "disk-inodes-race-idem", CreatedAt: base},
	}
	for _, rule := range rules {
		if err := engineOne.AddRule(ctx, rule); err != nil {
			t.Fatal(err)
		}
	}
	observation := func(at time.Time, metric string, value float64) Observation {
		return Observation{ServerID: serverID, ObservedAt: at, Values: map[string]float64{metric: value}, Validity: map[string]string{metric: "valid"}, Coverage: 1}
	}
	for _, rule := range rules {
		metric, _, _, _ := ParseExpression(rule.Expression)
		if _, err := engineOne.Evaluate(ctx, rule, observation(base, metric, 95), base); err != nil {
			t.Fatal(err)
		}
	}
	for _, rule := range rules {
		metric, _, _, _ := ParseExpression(rule.Expression)
		if _, err := engineOne.Evaluate(ctx, rule, observation(base.Add(time.Second), metric, 95), base.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	state, found, err := store.GetAlertState(ctx, alertStateID(rules[0].ID, serverID))
	if err != nil || !found || state.IncidentID == "" {
		t.Fatalf("first firing state=%+v found=%v err=%v", state, found, err)
	}
	incidentID := state.IncidentID
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for index, engine := range []*Engine{engineOne, engineTwo} {
		wg.Add(1)
		go func(index int, engine *Engine) {
			defer wg.Done()
			<-start
			metric, _, _, _ := ParseExpression(rules[index].Expression)
			_, evalErr := engine.Evaluate(ctx, rules[index], observation(base.Add(2*time.Second), metric, 0), base.Add(2*time.Second))
			errs <- evalErr
		}(index, engine)
	}
	close(start)
	wg.Wait()
	close(errs)
	for evalErr := range errs {
		if evalErr != nil {
			t.Fatal(evalErr)
		}
	}
	incident, found, err := store.GetIncident(ctx, incidentID)
	if err != nil || !found || incident.State != "recovered" || incident.EndedAt == nil || len(incident.Events) < 4 {
		t.Fatalf("concurrent recovery incident=%+v found=%v err=%v", incident, found, err)
	}
}

func TestAlertEvaluationRollsBackStateWhenHistoryFails(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	observed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	state := contracts.AlertState{ID: "alert-transaction-state", RuleID: "missing-rule", ServerID: serverID, State: "firing", FiringSince: &observed, LastObservation: &observed}
	event := contracts.AlertHistoryEvent{ID: "event-transaction-state", AlertID: "missing-rule", ServerID: serverID, State: "firing", OccurredAt: observed}
	if err := store.SaveAlertEvaluation(ctx, state, &event, nil); err == nil {
		t.Fatal("expected history foreign-key failure")
	}
	if _, found, err := store.GetAlertState(ctx, state.ID); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("alert state committed without its history event")
	}
}
