package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func TestServiceProvidesBoundedRuleAndMaintenanceRoutes(t *testing.T) {
	store, err := monitoring.OpenStore(context.Background(), ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	handler := service.Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/rules", bytes.NewBufferString(`{"name":"CPU","expression":"cpu.utilization > 90","idempotency_key":"rule-http-1"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("rule create status=%d body=%s", response.Code, response.Body.String())
	}
	var rule contracts.AlertRule
	if err := json.Unmarshal(response.Body.Bytes(), &rule); err != nil || rule.ID == "" {
		t.Fatalf("rule=%+v err=%v", rule, err)
	}
	// The same idempotency key maps to the same durable rule and does not create
	// a second row.
	request = httptest.NewRequest(http.MethodPost, "/api/v1/alerts/rules", bytes.NewBufferString(`{"name":"CPU","expression":"cpu.utilization > 90","idempotency_key":"rule-http-1"}`))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("idempotent rule status=%d", response.Code)
	}
	request = httptest.NewRequest(http.MethodPatch, "/api/v1/alerts/rules/"+rule.ID, bytes.NewBufferString(`{"enabled":false,"duration_seconds":600}`))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("rule update status=%d body=%s", response.Code, response.Body.String())
	}
	var updated contracts.AlertRule
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil || updated.Enabled || updated.DurationSeconds != 600 {
		t.Fatalf("updated rule=%+v err=%v", updated, err)
	}
	window := `{"idempotency_key":"maintenance-http-1","starts_at":"2026-01-01T00:00:00Z","ends_at":"2026-01-01T01:00:00Z","reason":"upgrade"}`
	request = httptest.NewRequest(http.MethodPost, "/api/v1/maintenance-windows", bytes.NewBufferString(window))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("maintenance create status=%d body=%s", response.Code, response.Body.String())
	}
	paused, err := store.IsMaintenance(context.Background(), contracts.ServerID("server-alerts-0123"), time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC))
	if err != nil || !paused {
		t.Fatalf("maintenance lookup paused=%v err=%v", paused, err)
	}
}

func TestDisablingFiringGroupedAlertRecoversItsStateAndIncident(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	serverID := alertServer(t, store)
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	rules := []contracts.AlertRule{
		{ID: "disable-group-capacity-01", Name: "Capacity", Expression: "disk.root.used_percent > 90", ServerID: serverID, Enabled: true, DurationSeconds: 1, ReminderSeconds: 3600, GroupKey: "disable-group", IdempotencyKey: "disable-group-capacity-idem", CreatedAt: base},
		{ID: "disable-group-inodes-01", Name: "Inodes", Expression: "disk.root.inodes_used_percent > 90", ServerID: serverID, Enabled: true, DurationSeconds: 1, ReminderSeconds: 3600, GroupKey: "disable-group", IdempotencyKey: "disable-group-inodes-idem", CreatedAt: base},
	}
	for _, rule := range rules {
		if err := service.Engine.AddRule(ctx, rule); err != nil {
			t.Fatal(err)
		}
	}
	observation := func(at time.Time, metric string, value float64) Observation {
		return Observation{ServerID: serverID, ObservedAt: at, Values: map[string]float64{metric: value}, Validity: map[string]string{metric: "valid"}, Coverage: 1}
	}
	for _, rule := range rules {
		metric, _, _, _ := ParseExpression(rule.Expression)
		if _, err := service.Engine.Evaluate(ctx, rule, observation(base, metric, 95), base); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Engine.Evaluate(ctx, rule, observation(base.Add(time.Second), metric, 95), base.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	firstState, found, err := store.GetAlertState(ctx, alertStateID(rules[0].ID, serverID))
	if err != nil || !found || firstState.State != "firing" || firstState.IncidentID == "" {
		t.Fatalf("first state=%+v found=%v err=%v", firstState, found, err)
	}
	incidentID := firstState.IncidentID
	patchRule := func(ruleID string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/alerts/rules/"+ruleID, bytes.NewBufferString(`{"enabled":false}`))
		resp := httptest.NewRecorder()
		service.Handler().ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("disable %s status=%d body=%s", ruleID, resp.Code, resp.Body.String())
		}
	}
	patchRule(rules[0].ID)
	firstState, found, err = store.GetAlertState(ctx, alertStateID(rules[0].ID, serverID))
	if err != nil || !found || firstState.State != "recovered" || firstState.RecoveredAt == nil {
		t.Fatalf("disabled state=%+v found=%v err=%v", firstState, found, err)
	}
	incident, found, err := store.GetIncident(ctx, incidentID)
	if err != nil || !found || incident.State != "open" {
		t.Fatalf("group closed while sibling was firing: %+v found=%v err=%v", incident, found, err)
	}
	patchRule(rules[1].ID)
	secondState, found, err := store.GetAlertState(ctx, alertStateID(rules[1].ID, serverID))
	if err != nil || !found || secondState.State != "recovered" {
		t.Fatalf("second disabled state=%+v found=%v err=%v", secondState, found, err)
	}
	incident, found, err = store.GetIncident(ctx, incidentID)
	if err != nil || !found || incident.State != "recovered" || incident.EndedAt == nil {
		t.Fatalf("group did not recover after disabling both members: %+v found=%v err=%v", incident, found, err)
	}
	history, err := store.ListAlertHistory(ctx, time.Time{}, time.Time{}, 20, "")
	if err != nil {
		t.Fatal(err)
	}
	disabledRecoveries := 0
	for _, event := range history.Items {
		if event.Reason == "rule_disabled" && event.State == "recovered" {
			disabledRecoveries++
		}
	}
	if disabledRecoveries != 2 {
		t.Fatalf("disable recovery history events=%d, want 2: %+v", disabledRecoveries, history.Items)
	}
}
