package bandwidth

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/porttraffic"
)

func TestPreviewSeparatesKernelPathsAndProtectsManagement(t *testing.T) {
	request := Request{Scope: Scope{TargetKind: "interface", Interface: "eth0", Direction: porttraffic.Inbound}, Action: Throttle, BitsPerSecond: 10_000_000}
	management := []ManagementFlow{{Name: "ssh-custom", Interface: "eth0", Protocol: porttraffic.TCP, LocalPort: 2222, PortRole: "local-service"}}
	preview, err := BuildPreview(request, management)
	if err != nil {
		t.Fatal(err)
	}
	if preview.KernelPath != "ingress-policer-or-ifb" || !preview.AffectsNetwork || len(preview.Exclusions) != 1 {
		t.Fatalf("preview=%+v", preview)
	}
	request.Scope = Scope{TargetKind: "local-port", Interface: "eth0", Direction: porttraffic.Outbound, Protocol: porttraffic.TCP, LocalPort: 2222}
	if _, err := BuildPreview(request, management); err == nil {
		t.Fatal("policy targeting the management port was accepted")
	}
	request.Scope = Scope{TargetKind: "interface", Interface: "eth0", Direction: porttraffic.Outbound}
	management[0].SharedPort = true
	if _, err := BuildPreview(request, management); err == nil {
		t.Fatal("shared proxy management flow was accepted")
	}
}

func TestPreviewRequiresExplicitActionAndSafeRate(t *testing.T) {
	scope := Scope{TargetKind: "interface", Interface: "eth0", Direction: porttraffic.Outbound}
	if _, err := BuildPreview(Request{Scope: scope}, nil); err == nil {
		t.Fatal("implicit action was accepted")
	}
	if _, err := BuildPreview(Request{Scope: scope, Action: Throttle, BitsPerSecond: 1000}, nil); err == nil {
		t.Fatal("unsafe tiny rate was accepted")
	}
	preview, err := BuildPreview(Request{Scope: scope, Action: WarnOnly}, nil)
	if err != nil || preview.AffectsNetwork {
		t.Fatalf("warn-only preview=%+v err=%v", preview, err)
	}
}

type fakeKernel struct {
	ownership Ownership
	previous  json.RawMessage
	applyErr  error
	verifyErr error
	events    *[]string
}

func (k *fakeKernel) Inspect(context.Context, Preview) (Ownership, json.RawMessage, error) {
	*k.events = append(*k.events, "inspect")
	return k.ownership, k.previous, nil
}
func (k *fakeKernel) Apply(context.Context, Preview) error {
	*k.events = append(*k.events, "apply")
	return k.applyErr
}
func (k *fakeKernel) Verify(context.Context, Preview) error {
	*k.events = append(*k.events, "verify")
	return k.verifyErr
}
func (k *fakeKernel) RevertOwned(context.Context, Checkpoint) error {
	*k.events = append(*k.events, "revert")
	return nil
}

type fakeGuard struct {
	events     *[]string
	checkpoint Checkpoint
	confirmErr error
}

func (g *fakeGuard) Arm(_ context.Context, checkpoint Checkpoint) error {
	*g.events = append(*g.events, "arm")
	g.checkpoint = checkpoint
	return nil
}
func (g *fakeGuard) Confirm(context.Context, string) error {
	*g.events = append(*g.events, "confirm")
	return g.confirmErr
}

func TestApplyArmsBeforeMutationAndConfirmsAfterRoundTrip(t *testing.T) {
	events := []string{}
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	kernel := &fakeKernel{ownership: Ownership{Compatible: true, Owner: "payesh"}, previous: json.RawMessage(`{"qdisc":"none"}`), events: &events}
	guard := &fakeGuard{events: &events}
	manager := Manager{Kernel: kernel, Guard: guard, Now: func() time.Time { return now }, ManagementRoundTrip: func(context.Context) error {
		events = append(events, "roundtrip")
		return nil
	}}
	request := Request{Scope: Scope{TargetKind: "interface", Interface: "eth0", Direction: porttraffic.Outbound}, Action: Throttle, BitsPerSecond: 20_000_000}
	if _, err := manager.Apply(context.Background(), "checkpoint-1", request, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"inspect", "arm", "apply", "verify", "roundtrip", "confirm"}
	if len(events) != len(want) {
		t.Fatalf("events=%v", events)
	}
	for index := range want {
		if events[index] != want[index] {
			t.Fatalf("events=%v", events)
		}
	}
	if !guard.checkpoint.ExpiresAt.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("rollback deadline=%s", guard.checkpoint.ExpiresAt)
	}
}

func TestApplyRollsBackOnVerificationOrManagementFailure(t *testing.T) {
	for _, test := range []struct {
		name         string
		verifyErr    error
		roundtripErr error
	}{
		{name: "verification", verifyErr: errors.New("rate mismatch")},
		{name: "management", roundtripErr: errors.New("hub unreachable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := []string{}
			kernel := &fakeKernel{ownership: Ownership{Compatible: true}, verifyErr: test.verifyErr, events: &events}
			guard := &fakeGuard{events: &events}
			manager := Manager{Kernel: kernel, Guard: guard, ManagementRoundTrip: func(context.Context) error { events = append(events, "roundtrip"); return test.roundtripErr }}
			request := Request{Scope: Scope{TargetKind: "interface", Interface: "eth0", Direction: porttraffic.Outbound}, Action: Block}
			if _, err := manager.Apply(context.Background(), "checkpoint-1", request, nil); err == nil {
				t.Fatal("failed apply reported success")
			}
			if len(events) < 2 || events[len(events)-2] != "revert" || events[len(events)-1] != "confirm" {
				t.Fatalf("rollback sequence=%v", events)
			}
		})
	}
}

func TestApplyRefusesForeignNetworkOwnershipBeforeGuard(t *testing.T) {
	events := []string{}
	kernel := &fakeKernel{ownership: Ownership{Compatible: false, Owner: "firewalld", Reason: "root qdisc is foreign"}, events: &events}
	guard := &fakeGuard{events: &events}
	manager := Manager{Kernel: kernel, Guard: guard, ManagementRoundTrip: func(context.Context) error { return nil }}
	request := Request{Scope: Scope{TargetKind: "interface", Interface: "eth0", Direction: porttraffic.Outbound}, Action: Block}
	if _, err := manager.Apply(context.Background(), "checkpoint-1", request, nil); !errors.Is(err, ErrForeignNetworkState) {
		t.Fatalf("foreign ownership error=%v", err)
	}
	if len(events) != 1 || events[0] != "inspect" {
		t.Fatalf("foreign state reached mutation path: %v", events)
	}
}
