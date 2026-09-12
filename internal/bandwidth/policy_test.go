package bandwidth

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/porttraffic"
)

func bandwidthPolicyManager(t *testing.T) (*Manager, contracts.ServerID, *[]string) {
	t.Helper()
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	serverID := contracts.ServerID("server-bandwidth-0001")
	server := contracts.Server{ID: serverID, Name: "local", Role: "standalone", Architecture: "amd64", Platform: "linux", Capabilities: []string{"tc", "nftables-counters"}, ConnectionState: "connected", FreshnessState: "fresh"}
	if err := store.UpsertServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionModuleInstallation(ctx, serverID, ModuleID, 0, contracts.ModuleInstallation{State: contracts.ModuleEnabled, Version: "0.1.0"}); err != nil {
		t.Fatal(err)
	}
	events := []string{}
	kernel := &fakeKernel{ownership: Ownership{Compatible: true}, previous: []byte(`{"absent":true}`), events: &events}
	manager := &Manager{Store: store, LocalServerID: serverID, Kernel: kernel, Guard: &fakeGuard{events: &events}, ManagementDiscovery: func(context.Context, Scope) ([]ManagementFlow, error) { return nil, nil }, Now: func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }, ManagementRoundTrip: func(context.Context) error { events = append(events, "roundtrip"); return nil }}
	return manager, serverID, &events
}

func TestApplyAndRevertPolicyPersistSharedControlState(t *testing.T) {
	manager, serverID, _ := bandwidthPolicyManager(t)
	request := Request{Scope: Scope{TargetKind: "interface", Interface: "eth0", Direction: porttraffic.Outbound}, Action: Throttle, BitsPerSecond: 10_000_000}
	policy, err := manager.ApplyPolicy(context.Background(), serverID, "checkpoint-1", request, 0)
	if err != nil {
		t.Fatal(err)
	}
	if policy.State != contracts.ControlPolicyApplied || policy.Revision != 1 || policy.TargetName != "eth0:outbound" {
		t.Fatalf("applied policy=%+v", policy)
	}
	reverted, err := manager.RevertPolicy(context.Background(), serverID, request.Scope, 1)
	if err != nil {
		t.Fatal(err)
	}
	if reverted.State != contracts.ControlPolicyReverted || reverted.Revision != 2 {
		t.Fatalf("reverted policy=%+v", reverted)
	}
}

func TestWarnOnlyPolicyPersistsWithoutKernelMutation(t *testing.T) {
	manager, serverID, events := bandwidthPolicyManager(t)
	request := Request{Scope: Scope{TargetKind: "local-port", Interface: "eth0", Protocol: porttraffic.TCP, LocalPort: 443, Direction: porttraffic.Inbound}, Action: WarnOnly, QuotaBytes: 1_000_000, UsageScope: "https"}
	policy, err := manager.ApplyPolicy(context.Background(), serverID, "unused-checkpoint", request, 0)
	if err != nil {
		t.Fatal(err)
	}
	if policy.State != contracts.ControlPolicyApplied || len(*events) != 0 {
		t.Fatalf("warn policy=%+v events=%v", policy, *events)
	}
	if _, err := manager.RevertPolicy(context.Background(), serverID, request.Scope, policy.Revision); err != nil {
		t.Fatal(err)
	}
}

func TestTCPlansDistinctEgressShapingAndIngressPolicing(t *testing.T) {
	egress := Preview{Request: Request{Scope: Scope{TargetKind: "local-port", Interface: "eth0", Protocol: porttraffic.TCP, LocalPort: 443, Direction: porttraffic.Outbound}, Action: Throttle, BitsPerSecond: 20_000_000}, AffectsNetwork: true}
	commands, err := planTC(egress)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) < 6 || commands[0][1] != "add" || commands[0][len(commands[0])-2] != "default" {
		t.Fatalf("egress commands=%v", commands)
	}
	joined := joinCommands(commands)
	if !containsAll(joined, "htb", "src_port 443", "classid 7a00:10") {
		t.Fatalf("egress plan=%s", joined)
	}

	ingress := Preview{Request: Request{Scope: Scope{TargetKind: "interface", Interface: "eth0", Direction: porttraffic.Inbound}, Action: Throttle, BitsPerSecond: 20_000_000}, AffectsNetwork: true}
	commands, err = planTC(ingress)
	if err != nil {
		t.Fatal(err)
	}
	joined = joinCommands(commands)
	if !containsAll(joined, "clsact", "ingress", "matchall", "police rate 20000000bit", "conform-exceed drop/pipe") {
		t.Fatalf("ingress plan=%s", joined)
	}
}

func TestEnforcerActivatesOnlyCompleteDueTrafficPeriod(t *testing.T) {
	manager, serverID, events := bandwidthPolicyManager(t)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	request := Request{Scope: Scope{TargetKind: "interface", Interface: "eth0", Direction: porttraffic.Inbound}, Action: Block, QuotaBytes: 1_000, UsageScope: "external"}
	policy, err := manager.ApplyPolicy(context.Background(), serverID, "configured", request, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(*events) != 0 {
		t.Fatalf("quota policy applied before threshold: %v", *events)
	}
	period := contracts.TrafficPeriod{Scope: "external", From: now.Add(-time.Hour), To: now.Add(time.Hour), Timezone: "UTC", Direction: "inbound", CountedBytes: 999, Continuity: "complete"}
	if err := manager.Store.UpsertTrafficPeriod(context.Background(), serverID, period); err != nil {
		t.Fatal(err)
	}
	enforcer := Enforcer{Store: manager.Store, Manager: manager, ServerID: serverID}
	if err := enforcer.Tick(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if len(*events) != 0 {
		t.Fatalf("quota activated below threshold: %v", *events)
	}
	period.CountedBytes = 1_000
	if err := manager.Store.UpsertTrafficPeriod(context.Background(), serverID, period); err != nil {
		t.Fatal(err)
	}
	if err := enforcer.Tick(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if len(*events) == 0 {
		t.Fatal("quota did not activate at threshold")
	}
	stored, err := manager.Store.GetControlPolicy(context.Background(), serverID, ModuleID, policy.TargetKind, policy.TargetName)
	if err != nil {
		t.Fatal(err)
	}
	var parameters PolicyParameters
	if json.Unmarshal(stored.Parameters, &parameters) != nil || parameters.EnforcementState != "active" || parameters.CountedBytes != 1_000 || !parameters.PeriodStart.Equal(period.From) {
		t.Fatalf("activated parameters=%+v", parameters)
	}
	next := contracts.TrafficPeriod{Scope: "external", From: period.To, To: period.To.Add(time.Hour), Timezone: "UTC", Direction: "inbound", CountedBytes: 0, Continuity: "complete"}
	if err := manager.Store.UpsertTrafficPeriod(context.Background(), serverID, next); err != nil {
		t.Fatal(err)
	}
	if err := enforcer.Tick(context.Background(), next.From.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	stored, err = manager.Store.GetControlPolicy(context.Background(), serverID, ModuleID, policy.TargetKind, policy.TargetName)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(stored.Parameters, &parameters) != nil || parameters.EnforcementState != "waiting" || !parameters.PeriodStart.Equal(next.From) {
		t.Fatalf("rollover parameters=%+v", parameters)
	}
}

func TestEnforcerFailsClosedOnAccountingGap(t *testing.T) {
	manager, serverID, events := bandwidthPolicyManager(t)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	request := Request{Scope: Scope{TargetKind: "interface", Interface: "eth0", Direction: porttraffic.Outbound}, Action: Block, QuotaBytes: 100, UsageScope: "external"}
	policy, err := manager.ApplyPolicy(context.Background(), serverID, "configured", request, 0)
	if err != nil {
		t.Fatal(err)
	}
	period := contracts.TrafficPeriod{Scope: "external", From: now.Add(-time.Hour), To: now.Add(time.Hour), Timezone: "UTC", Direction: "outbound", CountedBytes: 1_000, Continuity: "gap"}
	if err := manager.Store.UpsertTrafficPeriod(context.Background(), serverID, period); err != nil {
		t.Fatal(err)
	}
	if err := (&Enforcer{Store: manager.Store, Manager: manager, ServerID: serverID}).Tick(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	stored, err := manager.Store.GetControlPolicy(context.Background(), serverID, ModuleID, policy.TargetKind, policy.TargetName)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != contracts.ControlPolicyFailed || stored.Error == nil || stored.Error.Code != "traffic_accounting_gap" || len(*events) != 0 {
		t.Fatalf("gap policy=%+v events=%v", stored, *events)
	}
}

func joinCommands(commands [][]string) string {
	result := ""
	for _, command := range commands {
		result += " " + strings.Join(command, " ")
	}
	return result
}
func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
