package bandwidth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/porttraffic"
)

// TestBandwidthQuotaOvershootAcceptance exercises the same durable period and
// enforcer path used by the module with deliberately coarse accounting
// samples. It measures the bytes observed when the threshold is crossed and
// makes the expected cadence overshoot visible. It is opt-in because this is
// acceptance evidence, not a normal unit test.
//
// The probe is ledger-backed rather than a claim that every kernel byte can be
// attributed in userspace: the collector owns counter sampling and supplies
// these deltas in production. A real kernel traffic probe must therefore be
// read together with the throughput measurement.
func TestBandwidthQuotaOvershootAcceptance(t *testing.T) {
	if os.Getenv("PAYESH_LINUX_QUOTA_OVERSHOOT") != "1" {
		t.Skip("set PAYESH_LINUX_QUOTA_OVERSHOOT=1 on a disposable Linux host")
	}
	allowance := acceptanceUintEnv(t, "PAYESH_LINUX_QUOTA_BYTES", 64*1024, 1024, 32*1024*1024)
	sampleBytes := acceptanceUintEnv(t, "PAYESH_LINUX_QUOTA_SAMPLE_BYTES", 10*1024, 1, allowance)
	maxSamples := acceptanceUintEnv(t, "PAYESH_LINUX_QUOTA_MAX_SAMPLES", 128, 2, 4096)
	manager, serverID, _ := bandwidthPolicyManager(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	request := Request{
		Scope:          Scope{TargetKind: "interface", Interface: "eth0", Direction: porttraffic.Outbound},
		Action:         Block,
		QuotaBytes:     allowance,
		UsageScope:     "external",
		UsageDirection: string(porttraffic.Outbound),
	}
	policy, err := manager.ApplyPolicy(ctx, serverID, "quota-acceptance", request, 0)
	if err != nil {
		t.Fatal(err)
	}
	period := contracts.TrafficPeriod{
		Scope: "external", From: now.Add(-time.Hour), To: now.Add(time.Hour),
		Timezone: "UTC", AllowanceBytes: allowance, Direction: "outbound", Continuity: "complete",
	}
	if err := manager.Store.UpsertTrafficPeriod(ctx, serverID, period); err != nil {
		t.Fatal(err)
	}
	enforcer := Enforcer{Store: manager.Store, Manager: manager, ServerID: serverID}
	var counted uint64
	var samples uint64
	for samples = 1; samples <= maxSamples; samples++ {
		updated, addErr := manager.Store.AddTrafficUsage(ctx, serverID, period, sampleBytes, "complete")
		if addErr != nil {
			t.Fatal(addErr)
		}
		counted = updated.CountedBytes
		if err := enforcer.Tick(ctx, now); err != nil {
			t.Fatal(err)
		}
		stored, getErr := manager.Store.GetControlPolicy(ctx, serverID, ModuleID, policy.TargetKind, policy.TargetName)
		if getErr != nil {
			t.Fatal(getErr)
		}
		var parameters PolicyParameters
		if err := json.Unmarshal(stored.Parameters, &parameters); err != nil {
			t.Fatal(err)
		}
		if parameters.EnforcementState == "active" {
			break
		}
	}
	if counted < allowance {
		t.Fatalf("quota did not activate after %d samples: counted=%d allowance=%d", samples, counted, allowance)
	}
	overshoot := counted - allowance
	if overshoot >= sampleBytes {
		t.Fatalf("quota overshoot exceeded one accounting sample: overshoot=%d sample=%d", overshoot, sampleBytes)
	}
	fmt.Printf("bandwidth_quota_overshoot=PASS allowance_bytes=%d counted_bytes=%d overshoot_bytes=%d sample_bytes=%d samples=%d model=durable-ledger-sample\n", allowance, counted, overshoot, sampleBytes, samples)
}
