package cpucontrol

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func testManager(t *testing.T) (*Manager, contracts.ServerID) {
	t.Helper()
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	server := contracts.Server{ID: "server-cpuctl-test01", Name: "Test", Role: "standalone", Architecture: "amd64", Platform: "linux", Capabilities: []string{"cgroup-v2"}, Version: "0.1.0", ConnectionState: "connected", FreshnessState: "fresh"}
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionModuleInstallation(ctx, server.ID, ModuleID, 0, contracts.ModuleInstallation{State: contracts.ModuleEnabled, Version: "0.1.0"}); err != nil {
		t.Fatal(err)
	}
	procRoot := t.TempDir()
	fs := FSCgroup{Root: t.TempDir(), OwnershipRoot: t.TempDir(), AllowPlainFilesystem: true}
	manager := &Manager{Store: store, FS: fs, ProcRoot: procRoot, LocalServerID: server.ID}
	manager.PrepareService = func(_ context.Context, _ Target, group GroupPath) error {
		dir, err := fs.dir(group)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte("4242\n"), 0o644)
	}
	return manager, server.ID
}

func TestManagerPreviewReportsCurrentAndParentStricter(t *testing.T) {
	manager, _ := testManager(t)
	target := Target{Kind: TargetKindService, Name: "nginx.service"}
	preview, err := manager.Preview(target, 500, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.CurrentUnlimited || preview.ProposedMillicores != 500 {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	if preview.Description == "" {
		t.Fatal("expected a human-readable description")
	}
}

func TestManagerApplyVerifiesAndPersists(t *testing.T) {
	manager, serverID := testManager(t)
	target := Target{Kind: TargetKindService, Name: "nginx.service"}
	policy, err := manager.Apply(context.Background(), serverID, target, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if policy.State != contracts.ControlPolicyApplied {
		t.Fatalf("expected applied, got %s", policy.State)
	}
	millicores, unlimited, err := manager.FS.ReadQuota(target.GroupPath())
	if err != nil {
		t.Fatal(err)
	}
	if unlimited || millicores != 1000 {
		t.Fatalf("expected effective quota 1000m, got millicores=%d unlimited=%v", millicores, unlimited)
	}
}

func TestManagerApplyThenRevertRestoresOriginalQuota(t *testing.T) {
	manager, serverID := testManager(t)
	target := Target{Kind: TargetKindService, Name: "nginx.service"}
	first, err := manager.Apply(context.Background(), serverID, target, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Apply(context.Background(), serverID, target, 2000, first.Revision)
	if err != nil {
		t.Fatal(err)
	}
	reverted, err := manager.Revert(context.Background(), serverID, target, second.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if reverted.State != contracts.ControlPolicyReverted {
		t.Fatalf("expected reverted, got %s", reverted.State)
	}
	// Updating a Payesh-owned policy must not replace the original baseline.
	// Disabling after any number of edits restores the pre-Payesh state.
	millicores, unlimited, err := manager.FS.ReadQuota(target.GroupPath())
	if err != nil {
		t.Fatal(err)
	}
	if !unlimited || millicores != 0 {
		t.Fatalf("expected original unlimited quota, got millicores=%d unlimited=%v", millicores, unlimited)
	}
}

func TestManagerRevertOnFirstApplyRemovesLimitEntirely(t *testing.T) {
	manager, serverID := testManager(t)
	target := Target{Kind: TargetKindService, Name: "nginx.service"}
	applied, err := manager.Apply(context.Background(), serverID, target, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Revert(context.Background(), serverID, target, applied.Revision); err != nil {
		t.Fatal(err)
	}
	_, unlimited, err := manager.FS.ReadQuota(target.GroupPath())
	if err != nil {
		t.Fatal(err)
	}
	if !unlimited {
		t.Fatal("expected reverting the very first apply to restore no limit")
	}
}

func TestManagerRejectsSharedForeignGroup(t *testing.T) {
	manager, serverID := testManager(t)
	target := Target{Kind: TargetKindService, Name: "shared-svc"}
	dir, err := manager.FS.(FSCgroup).dir(target.GroupPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Apply(context.Background(), serverID, target, 1000, 0); err == nil {
		t.Fatal("expected apply against a pre-existing, unmarked shared cgroup to be refused")
	}
	policy, err := manager.Store.GetControlPolicy(context.Background(), serverID, ModuleID, target.Kind, target.Name)
	if err != nil {
		t.Fatal(err)
	}
	if policy.State != contracts.ControlPolicyFailed {
		t.Fatalf("expected the refusal to be durably recorded as failed, got %s", policy.State)
	}
}

func TestManagerApplyRejectsReusedPID(t *testing.T) {
	manager, serverID := testManager(t)
	writeFakeStat(t, manager.ProcRoot, 555, "worker", 100)
	target := Target{Kind: TargetKindProcessGroup, Name: "worker-batch-1", Process: &ProcessIdentity{PID: 555, StartTicks: 100}}
	if err := manager.FS.EnsureDedicatedGroup(target.GroupPath()); err != nil {
		t.Fatal(err)
	}
	if err := manager.FS.AttachProcess(target.GroupPath(), 555); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Apply(context.Background(), serverID, target, 1000, 0); err != nil {
		t.Fatal(err)
	}
	// The process exits and the kernel reuses PID 555 for something else.
	writeFakeStat(t, manager.ProcRoot, 555, "unrelated", 999)
	staleTarget := Target{Kind: TargetKindProcessGroup, Name: "worker-batch-2", Process: &ProcessIdentity{PID: 555, StartTicks: 100}}
	if _, err := manager.Apply(context.Background(), serverID, staleTarget, 500, 0); err == nil {
		t.Fatal("expected apply against a stale (reused) PID to be rejected")
	}
}

func TestManagerApplyRejectsStaleRevision(t *testing.T) {
	manager, serverID := testManager(t)
	target := Target{Kind: TargetKindService, Name: "nginx.service"}
	if _, err := manager.Apply(context.Background(), serverID, target, 1000, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Apply(context.Background(), serverID, target, 2000, 0); err != monitoring.ErrControlPolicyRevisionConflict {
		t.Fatalf("expected ErrControlPolicyRevisionConflict, got %v", err)
	}
}

func TestManagerRevertAllForServerSweepsAppliedPolicies(t *testing.T) {
	manager, serverID := testManager(t)
	a := Target{Kind: TargetKindService, Name: "svc-a"}
	b := Target{Kind: TargetKindService, Name: "svc-b"}
	if _, err := manager.Apply(context.Background(), serverID, a, 1000, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Apply(context.Background(), serverID, b, 500, 0); err != nil {
		t.Fatal(err)
	}
	if err := manager.RevertAllForServer(context.Background(), serverID); err != nil {
		t.Fatal(err)
	}
	for _, target := range []Target{a, b} {
		_, unlimited, err := manager.FS.ReadQuota(target.GroupPath())
		if err != nil {
			t.Fatal(err)
		}
		if !unlimited {
			t.Fatalf("expected %s to be reverted to unlimited", target.Name)
		}
		policy, err := manager.Store.GetControlPolicy(context.Background(), serverID, ModuleID, target.Kind, target.Name)
		if err != nil {
			t.Fatal(err)
		}
		if policy.State != contracts.ControlPolicyReverted {
			t.Fatalf("expected %s to be recorded reverted, got %s", target.Name, policy.State)
		}
	}
}

func TestManagerRejectsApplyWhenModuleIsDisabled(t *testing.T) {
	manager, serverID := testManager(t)
	if _, err := manager.Store.TransitionModuleInstallation(context.Background(), serverID, ModuleID, 1, contracts.ModuleInstallation{State: contracts.ModuleInstalledDisabled, Version: "0.1.0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Apply(context.Background(), serverID, Target{Kind: TargetKindService, Name: "svc"}, 500, 0); err == nil {
		t.Fatal("expected a disabled module to reject policy application")
	}
}

func TestManagerRequiresSupervisorAdapterForService(t *testing.T) {
	manager, serverID := testManager(t)
	manager.PrepareService = nil
	if _, err := manager.Apply(context.Background(), serverID, Target{Kind: TargetKindService, Name: "svc"}, 500, 0); err == nil {
		t.Fatal("expected service apply to fail without a supervisor adapter")
	}
}

func TestManagerRevertRefusesExternalQuotaChange(t *testing.T) {
	manager, serverID := testManager(t)
	target := Target{Kind: TargetKindService, Name: "svc"}
	applied, err := manager.Apply(context.Background(), serverID, target, 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.FS.WriteQuota(target.GroupPath(), 750, false); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Revert(context.Background(), serverID, target, applied.Revision); err == nil {
		t.Fatal("expected external quota drift to block revert")
	}
	failed, err := manager.Store.GetControlPolicy(context.Background(), serverID, ModuleID, target.Kind, target.Name)
	if err != nil || failed.State != contracts.ControlPolicyFailed {
		t.Fatalf("expected recovery-required failed state, policy=%+v err=%v", failed, err)
	}
	if err := manager.FS.WriteQuota(target.GroupPath(), 500, false); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Revert(context.Background(), serverID, target, failed.Revision); err != nil {
		t.Fatalf("expected revert retry after external revalidation: %v", err)
	}
}
