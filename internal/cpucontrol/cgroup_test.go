package cpucontrol

import (
	"os"
	"path/filepath"
	"testing"
)

func testFS(t *testing.T) FSCgroup {
	t.Helper()
	return FSCgroup{Root: t.TempDir(), OwnershipRoot: t.TempDir(), AllowPlainFilesystem: true}
}

func TestGroupPathMustLiveUnderPayeshSubtree(t *testing.T) {
	fs := testFS(t)
	if err := fs.EnsureDedicatedGroup("other/thing"); err == nil {
		t.Fatal("expected a group path outside payesh/ to be rejected")
	}
	if err := fs.EnsureDedicatedGroup("payesh/../escape"); err == nil {
		t.Fatal("expected a traversal group path to be rejected")
	}
	if err := fs.EnsureDedicatedGroup(""); err == nil {
		t.Fatal("expected an empty group path to be rejected")
	}
}

func TestEnsureDedicatedGroupCreatesAndMarksOwnership(t *testing.T) {
	fs := testFS(t)
	group := GroupPath("payesh/svc-nginx")
	if err := fs.EnsureDedicatedGroup(group); err != nil {
		t.Fatal(err)
	}
	owned, err := fs.IsDedicatedGroup(group)
	if err != nil {
		t.Fatal(err)
	}
	if !owned {
		t.Fatal("expected a freshly created group to be marked owned")
	}
	if _, err := os.Stat(filepath.Join(fs.Root, "payesh", "svc-nginx", "payesh.owned")); !os.IsNotExist(err) {
		t.Fatal("ownership metadata must not be created inside cgroupfs")
	}
	// Idempotent: calling it again on an already-owned group must not error.
	if err := fs.EnsureDedicatedGroup(group); err != nil {
		t.Fatalf("expected re-ensuring an owned group to succeed, got %v", err)
	}
}

func TestAttachAndVerifyProcessMembership(t *testing.T) {
	fs := testFS(t)
	group := GroupPath("payesh/process-worker")
	if err := fs.EnsureDedicatedGroup(group); err != nil {
		t.Fatal(err)
	}
	if err := fs.AttachProcess(group, 4242); err != nil {
		t.Fatal(err)
	}
	contained, err := fs.ContainsProcess(group, 4242)
	if err != nil || !contained {
		t.Fatalf("expected process membership, contained=%v err=%v", contained, err)
	}
}

func TestEnsureDedicatedGroupRefusesToAdoptForeignDirectory(t *testing.T) {
	fs := testFS(t)
	group := GroupPath("payesh/existing")
	dir := filepath.Join(fs.Root, "payesh", "existing")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fs.EnsureDedicatedGroup(group); err == nil {
		t.Fatal("expected a pre-existing, unmarked directory to be refused")
	}
}

func TestEnsureDedicatedGroupRefusesChildBelowForeignPayeshParent(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "payesh"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs := FSCgroup{Root: root, OwnershipRoot: t.TempDir(), AllowPlainFilesystem: true}
	if err := fs.EnsureDedicatedGroup(GroupPath("payesh/process-group-child")); err == nil {
		t.Fatal("expected an unowned Payesh parent to be refused")
	}
	if _, err := os.Stat(filepath.Join(root, "payesh", "process-group-child")); !os.IsNotExist(err) {
		t.Fatalf("child was created below foreign parent: %v", err)
	}
}

func TestOwnershipRecordDoesNotAuthorizeRecreatedDirectory(t *testing.T) {
	fs := testFS(t)
	group := GroupPath("payesh/recreated")
	if err := fs.EnsureDedicatedGroup(group); err != nil {
		t.Fatal(err)
	}
	dir, err := fs.dir(group)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an external actor deleting and recreating the entire owned
	// directory, including its attached ownership token.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fs.EnsureDedicatedGroup(group); err == nil {
		t.Fatal("expected stale ownership metadata to reject a recreated directory")
	}
}

func TestWriteQuotaRefusesUnownedGroup(t *testing.T) {
	fs := testFS(t)
	group := GroupPath("payesh/not-created-yet")
	if err := fs.WriteQuota(group, 1000, false); err == nil {
		t.Fatal("expected writing quota to a non-existent/unowned group to fail")
	}
}

func TestWriteAndReadQuotaRoundTrip(t *testing.T) {
	fs := testFS(t)
	group := GroupPath("payesh/svc-app")
	if err := fs.EnsureDedicatedGroup(group); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteQuota(group, 1500, false); err != nil {
		t.Fatal(err)
	}
	millicores, unlimited, err := fs.ReadQuota(group)
	if err != nil {
		t.Fatal(err)
	}
	if unlimited || millicores != 1500 {
		t.Fatalf("got millicores=%d unlimited=%v", millicores, unlimited)
	}
	if err := fs.WriteQuota(group, 0, true); err != nil {
		t.Fatal(err)
	}
	millicores, unlimited, err = fs.ReadQuota(group)
	if err != nil {
		t.Fatal(err)
	}
	if !unlimited || millicores != 0 {
		t.Fatalf("expected unlimited after restore, got millicores=%d unlimited=%v", millicores, unlimited)
	}
}

func TestReadQuotaOnMissingGroupReportsUnlimitedNotError(t *testing.T) {
	fs := testFS(t)
	// No group created at all: cpu.max simply does not exist yet.
	millicores, unlimited, err := fs.ReadQuota("payesh/never-created")
	if err != nil {
		t.Fatal(err)
	}
	if !unlimited || millicores != 0 {
		t.Fatalf("expected unlimited for a missing cpu.max, got millicores=%d unlimited=%v", millicores, unlimited)
	}
}

func TestParentQuotaDetectsStricterAncestor(t *testing.T) {
	fs := testFS(t)
	parent := GroupPath("payesh/parent-scope")
	child := GroupPath("payesh/parent-scope/child")
	if err := fs.EnsureDedicatedGroup(parent); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteQuota(parent, 500, false); err != nil {
		t.Fatal(err)
	}
	if err := fs.EnsureDedicatedGroup(child); err != nil {
		t.Fatal(err)
	}
	millicores, unlimited, found, err := fs.ParentQuota(child)
	if err != nil {
		t.Fatal(err)
	}
	if !found || unlimited || millicores != 500 {
		t.Fatalf("expected to find the parent's 500m quota, got millicores=%d unlimited=%v found=%v", millicores, unlimited, found)
	}
}

func TestParentQuotaReportsNoneWhenAncestorsAreUnlimited(t *testing.T) {
	fs := testFS(t)
	child := GroupPath("payesh/standalone/child")
	if err := fs.EnsureDedicatedGroup("payesh/standalone"); err != nil {
		t.Fatal(err)
	}
	if err := fs.EnsureDedicatedGroup(child); err != nil {
		t.Fatal(err)
	}
	_, _, found, err := fs.ParentQuota(child)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("expected no stricter ancestor quota to be found")
	}
}

func TestIsEmptyAndRemoveGroup(t *testing.T) {
	fs := testFS(t)
	group := GroupPath("payesh/svc-empty")
	if err := fs.EnsureDedicatedGroup(group); err != nil {
		t.Fatal(err)
	}
	empty, err := fs.IsEmpty(group)
	if err != nil || !empty {
		t.Fatalf("expected a freshly created group to be empty, got empty=%v err=%v", empty, err)
	}
	if err := fs.RemoveGroup(group); err != nil {
		t.Fatal(err)
	}
	if owned, _ := fs.IsDedicatedGroup(group); owned {
		t.Fatal("expected the group to be gone after removal")
	}
}

func TestRemoveGroupRefusesNonEmptyGroup(t *testing.T) {
	fs := testFS(t)
	group := GroupPath("payesh/svc-busy")
	if err := fs.EnsureDedicatedGroup(group); err != nil {
		t.Fatal(err)
	}
	dir, err := fs.dir(group)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte("4242\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.RemoveGroup(group); err == nil {
		t.Fatal("expected removal of a non-empty group to be refused")
	}
}

func TestRemoveGroupRefusesUnownedGroup(t *testing.T) {
	fs := testFS(t)
	group := GroupPath("payesh/foreign")
	if err := os.MkdirAll(filepath.Join(fs.Root, "payesh", "foreign"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fs.RemoveGroup(group); err == nil {
		t.Fatal("expected removal of an unowned directory to be refused")
	}
}
