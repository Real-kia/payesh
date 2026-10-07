package webupdate

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// This fixture exercises Linux UID/mode checks on a disposable root-owned
// filesystem tree. It does not activate an installed release or any service.
func TestPrivilegedFilesystemProtection(t *testing.T) {
	dir := os.Getenv("PAYESH_PRIVILEGED_PATH_FIXTURE")
	if dir == "" {
		t.Skip("set PAYESH_PRIVILEGED_PATH_FIXTURE to a fresh /var/backups/payesh-acceptance-* directory for opt-in Linux root filesystem checks")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Fatal("privileged path fixture requires Linux UID 0")
	}
	if filepath.Clean(dir) != dir || filepath.Dir(dir) != "/var/backups" || !strings.HasPrefix(filepath.Base(dir), "payesh-acceptance-") || len(filepath.Base(dir)) <= len("payesh-acceptance-") {
		t.Fatal("fixture must be an exact fresh /var/backups/payesh-acceptance-* directory")
	}
	if err := safeExistingParents(filepath.Dir(dir), true); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatalf("fixture directory must not already exist: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove exact fixture directory: %v", err)
		}
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Errorf("fixture directory remains: %v", err)
		}
	})
	if err := safeExistingParents(dir, true); err != nil {
		t.Fatal(err)
	}
	t.Run("root-directory-mode-and-owner", func(t *testing.T) {
		path := filepath.Join(dir, "controlled")
		if err := safeDirectory(path, 0700, true); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0770); err != nil {
			t.Fatal(err)
		}
		if err := safeDirectory(path, 0700, true); err == nil {
			t.Error("group-writable directory accepted")
		}
		if err := safeExistingParents(path, true); err == nil {
			t.Error("group-writable parent accepted")
		}
		if err := os.Chmod(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(path, 65534, 65534); err != nil {
			t.Fatal(err)
		}
		if err := safeDirectory(path, 0700, true); err == nil {
			t.Error("non-root-owned directory accepted")
		}
		if err := safeExistingParents(path, true); err == nil {
			t.Error("non-root-owned parent accepted")
		}
		if err := os.Chown(path, 0, 0); err != nil {
			t.Fatal(err)
		}
		if err := safeDirectory(path, 0700, true); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("symlink-parent", func(t *testing.T) {
		link := filepath.Join(dir, "link")
		if err := os.Symlink(filepath.Join(dir, "controlled"), link); err != nil {
			t.Fatal(err)
		}
		if err := safeDirectory(link, 0700, true); err == nil {
			t.Error("symlink directory accepted")
		}
		if err := safeExistingParents(link, true); err == nil {
			t.Error("symlink parent accepted")
		}
	})
	t.Run("protected-scope", func(t *testing.T) {
		root := filepath.Join(dir, "scope-root")
		if err := safeDirectory(filepath.Join(root, "etc"), 0700, true); err != nil {
			t.Fatal(err)
		}
		want := InstallationScope{Role: "node", Init: "systemd"}
		if err := WriteInstallationScope(root, want); err != nil {
			t.Fatal(err)
		}
		path := scopePath(root)
		if err := safeExistingParents(path, true); err != nil {
			t.Fatal(err)
		}
		if got, err := ReadInstallationScope(root); err != nil || got != want {
			t.Fatalf("protected scope roundtrip: %+v, %v", got, err)
		}
		if err := os.Chmod(path, 0664); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadInstallationScope(root); err == nil {
			t.Error("writable scope accepted")
		}
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(path, 65534, 65534); err != nil {
			t.Fatal(err)
		}
		if err := safeExistingParents(path, true); err == nil {
			t.Error("forged non-root scope accepted by privileged gate")
		}
		if err := os.Chown(path, 0, 0); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("protected-results", func(t *testing.T) {
		inbox := filepath.Join(dir, "intent")
		result := Status{State: StateSucceeded, JobID: "fixture-job", Target: "v1.2.3", UpdatedAt: time.Now().UTC()}
		if err := publishFleetResult(inbox, result); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(fleetResultsDir(inbox), fleetResultName(result.JobID))
		if err := safeExistingParents(path, true); err != nil {
			t.Fatal(err)
		}
		if got, err := FleetResult(inbox, result.JobID); err != nil || got.State != StateSucceeded {
			t.Fatalf("protected result roundtrip: %+v, %v", got, err)
		}
		if err := os.Chown(path, 65534, 65534); err != nil {
			t.Fatal(err)
		}
		if err := safeExistingParents(path, true); err == nil {
			t.Error("forged non-root result accepted by privileged gate")
		}
		if err := os.Chown(path, 0, 0); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0664); err != nil {
			t.Fatal(err)
		}
		if err := safeExistingParents(path, true); err == nil {
			t.Error("group-writable result accepted by privileged gate")
		}
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		moved := path + ".original"
		if err := os.Rename(path, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(moved, path); err != nil {
			t.Fatal(err)
		}
		if _, err := FleetResult(inbox, result.JobID); err == nil {
			t.Error("symlink result accepted")
		}
	})
	t.Log("privileged filesystem checks completed; exact disposable directory cleanup registered")
}
