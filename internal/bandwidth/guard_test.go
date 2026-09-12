package bandwidth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/porttraffic"
)

func TestFileGuardPersistsAndRecoversDueCheckpoint(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	directory := filepath.Join(t.TempDir(), "rollback")
	guard := FileGuard{Dir: directory}
	checkpoint := Checkpoint{ID: "policy-1", Scope: Scope{TargetKind: "interface", Interface: "eth0", Direction: porttraffic.Outbound}, Action: Throttle, Previous: json.RawMessage(`{"owner":"none"}`), ExpiresAt: now.Add(time.Minute)}
	if err := guard.Arm(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(directory, "policy-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("checkpoint mode=%v", info.Mode().Perm())
	}
	events := []string{}
	kernel := &fakeKernel{ownership: Ownership{Compatible: true}, events: &events}
	recovered, err := guard.RecoverDue(context.Background(), kernel, now.Add(30*time.Second))
	if err != nil || len(recovered) != 0 || len(events) != 0 {
		t.Fatalf("early recovery=%v events=%v err=%v", recovered, events, err)
	}
	recovered, err = guard.RecoverDue(context.Background(), kernel, now.Add(2*time.Minute))
	if err != nil || len(recovered) != 1 || recovered[0] != "policy-1" || len(events) != 1 || events[0] != "revert" {
		t.Fatalf("due recovery=%v events=%v err=%v", recovered, events, err)
	}
	if _, err := os.Stat(filepath.Join(directory, "policy-1.json")); !os.IsNotExist(err) {
		t.Fatalf("recovered checkpoint remains: %v", err)
	}
}

func TestFileGuardRetainsCheckpointWhenOwnedRevertFails(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	directory := filepath.Join(t.TempDir(), "rollback")
	guard := FileGuard{Dir: directory}
	checkpoint := Checkpoint{ID: "policy-2", Scope: Scope{TargetKind: "interface", Interface: "eth0", Direction: porttraffic.Inbound}, Action: Block, ExpiresAt: now}
	if err := guard.Arm(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
	kernel := failingRevertKernel{fakeKernel: fakeKernel{events: &[]string{}}}
	if _, err := guard.RecoverDue(context.Background(), &kernel, now.Add(time.Second)); err == nil {
		t.Fatal("failed owned revert reported recovery")
	}
	if _, err := os.Stat(filepath.Join(directory, "policy-2.json")); err != nil {
		t.Fatalf("recovery checkpoint was removed after failure: %v", err)
	}
}

type failingRevertKernel struct{ fakeKernel }

func (failingRevertKernel) RevertOwned(context.Context, Checkpoint) error {
	return os.ErrPermission
}
