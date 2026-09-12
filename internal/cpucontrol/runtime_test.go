package cpucontrol

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func TestEnsureCPUHierarchyDelegatesRootAndPayesh(t *testing.T) {
	root := t.TempDir()
	fs := FSCgroup{Root: root, OwnershipRoot: filepath.Join(t.TempDir(), "ownership"), AllowPlainFilesystem: true}
	for _, path := range []string{filepath.Join(root, "cgroup.controllers"), filepath.Join(root, "cgroup.subtree_control")} {
		if err := os.WriteFile(path, []byte("cpu memory\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := fs.EnsureDedicatedGroup(GroupPath("payesh")); err != nil {
		t.Fatal(err)
	}
	payesh := filepath.Join(root, "payesh")
	if err := os.WriteFile(filepath.Join(payesh, "cgroup.controllers"), []byte("cpu\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(payesh, "cgroup.subtree_control"), []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.EnsureCPUHierarchy(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "cgroup.subtree_control"), filepath.Join(payesh, "cgroup.subtree_control")} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !containsController(string(data), "cpu") {
			t.Fatalf("cpu was not delegated at %s: %q", path, data)
		}
	}
}

func TestEnsureCPUHierarchyFailsClosedWhenControllerMissing(t *testing.T) {
	root := t.TempDir()
	fs := FSCgroup{Root: root, OwnershipRoot: t.TempDir()}
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.EnsureCPUHierarchy(); err == nil || !strings.Contains(err.Error(), "cpu controller is unavailable") {
		t.Fatalf("expected unavailable controller error, got %v", err)
	}
}

func TestLocalServicePreparerRequiresExactOwnedGroup(t *testing.T) {
	root := t.TempDir()
	fs := FSCgroup{Root: root, OwnershipRoot: filepath.Join(t.TempDir(), "ownership"), AllowPlainFilesystem: true}
	target := Target{Kind: TargetKindService, Name: "payesh-worker.service"}
	group := target.GroupPath()
	if err := fs.EnsureDedicatedGroup(group); err != nil {
		t.Fatal(err)
	}
	dir, err := fs.dir(group)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte("123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := func(_ context.Context, command string, args ...string) ([]byte, error) {
		if command == "systemctl" && len(args) == 1 && args[0] == "--version" {
			return []byte("systemd 252"), nil
		}
		if command == "systemctl" && len(args) > 2 && args[0] == "show" && args[1] == "payesh-worker.service" {
			if strings.Contains(strings.Join(args, " "), "ActiveState") {
				return []byte("active\n"), nil
			}
			return []byte("/" + string(group) + "\n"), nil
		}
		return nil, os.ErrNotExist
	}
	preparer := LocalServicePreparer{FS: fs, Runner: runner}
	if err := preparer.Prepare(context.Background(), target, group); err != nil {
		t.Fatalf("expected owned active service to pass: %v", err)
	}
	badRunner := func(_ context.Context, command string, args ...string) ([]byte, error) {
		if command == "systemctl" && len(args) == 1 {
			return []byte("systemd"), nil
		}
		if strings.Contains(strings.Join(args, " "), "ActiveState") {
			return []byte("active\n"), nil
		}
		return []byte("/system.slice/payesh-worker.service\n"), nil
	}
	if err := (LocalServicePreparer{FS: fs, Runner: badRunner}).Prepare(context.Background(), target, group); err == nil {
		t.Fatal("accepted a service in a non-Payesh cgroup")
	}
}

func TestRuntimeAuthTokenProtectsHandler(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, filepath.Join(t.TempDir(), "payesh.db"), monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime, err := NewRuntime(RuntimeOptions{Store: store, ServerID: contracts.ServerID("server-runtime-test01"), CgroupRoot: "/sys/fs/cgroup", OwnershipRoot: filepath.Join(t.TempDir(), "ownership"), SocketPath: filepath.Join(t.TempDir(), "cpu.sock"), AuthToken: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/servers/server-runtime-test01/cpu-policies", nil)
	response := httptest.NewRecorder()
	runtime.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status=%d", response.Code)
	}
	request.Header.Set("X-Payesh-Module-Token", "secret")
	response = httptest.NewRecorder()
	runtime.Handler().ServeHTTP(response, request)
	if response.Code == http.StatusUnauthorized {
		t.Fatal("valid module token was rejected")
	}
}

func TestListenCPUSocketUsesPermissionBoundary(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "cpu-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	path := filepath.Join(root, "cpu.sock")
	listener, err := listenCPUSocket(path)
	if err != nil {
		if errors.Is(err, syscall.EPERM) || strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("Unix socket bind unavailable in test sandbox: %v", err)
		}
		t.Fatal(err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(path)
	}()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o660 {
		t.Fatalf("socket permissions = %o, want 660", got)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("path is not a Unix socket: %v", info.Mode())
	}
}
