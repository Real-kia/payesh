package alerts

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func TestIncidentCapturesBoundedMetricAndLogContext(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := alertServer(t, store)
	file, err := os.CreateTemp("", "payesh-incident-*.log")
	if err != nil {
		t.Fatal(err)
	}
	path := file.Name()
	defer os.Remove(path)
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	if _, err := file.WriteString(base.Add(time.Minute).Format(time.RFC3339Nano) + " [ERROR] disk context captured\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterLogSource(ctx, monitoring.LogSource{ServerID: serverID, ID: "service", Label: "Service", Path: path}); err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	rule := contracts.AlertRule{ID: "context-rule-01", Name: "Context", Expression: "cpu.utilization > 90", ServerID: serverID, Enabled: true, DurationSeconds: 60, ReminderSeconds: 3600, IdempotencyKey: "context-rule-idem-01", CreatedAt: base}
	if err := engine.AddRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	observation := func(at time.Time) Observation {
		return Observation{ServerID: serverID, ObservedAt: at, Values: map[string]float64{"cpu.utilization": 95}, Validity: map[string]string{"cpu.utilization": "valid"}, Coverage: 1}
	}
	if _, err := engine.Evaluate(ctx, rule, observation(base), base); err != nil {
		t.Fatal(err)
	}
	evaluation, err := engine.Evaluate(ctx, rule, observation(base.Add(time.Minute)), base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.State.IncidentID == "" {
		t.Fatal("firing evaluation did not create an incident")
	}
	incident, found, err := store.GetIncident(ctx, evaluation.State.IncidentID)
	if err != nil || !found {
		t.Fatalf("incident=%+v found=%v err=%v", incident, found, err)
	}
	joined := strings.Join(incident.Evidence, "\n")
	if !strings.Contains(joined, "metric cpu.utilization=95") || !strings.Contains(joined, "disk context captured") {
		t.Fatalf("incident evidence=%v", incident.Evidence)
	}
}
