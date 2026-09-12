package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type acceptanceServiceRemover struct {
	root, init string
	services   []string
	stop       bool
}

func (m *acceptanceServiceRemover) Remove(_ context.Context, root, init string, services []string, stop bool) error {
	m.root, m.init, m.services, m.stop = root, init, append([]string(nil), services...), stop
	return nil
}

// TestDisposableInstallUpgradeRecoveryUninstallAcceptance is the repository
// side clean-host gate. It deliberately uses a fresh filesystem fixture and
// injected supervisor/account boundaries, then runs the same direct installer
// through first install, replacement upgrade, failed verification recovery,
// retry/resume, and data-preserving uninstall.
func TestDisposableInstallUpgradeRecoveryUninstallAcceptance(t *testing.T) {
	root, artifactDir := installFixture(t, "systemd")
	if err := os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("ID=ubuntu\nVERSION_ID=22.04\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "payesh-agent"), []byte("v1-agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(root, "var/lib/payesh/server-id")
	if err := os.MkdirAll(filepath.Dir(identity), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity, []byte("stable-acceptance-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	services := &testServiceManager{}
	opts := InstallOptions{Root: root, Role: "node", ArtifactDir: artifactDir, Verify: acceptArtifact, ServiceManager: services, Start: true}
	first, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("clean install: %v", err)
	}
	if !first.Started || !services.start {
		t.Fatalf("install did not activate service: %+v", first)
	}
	installed := filepath.Join(root, "usr/bin/payesh-agent")
	if body, err := os.ReadFile(installed); err != nil || string(body) != "v1-agent" {
		t.Fatalf("initial artifact=%q err=%v", body, err)
	}

	// A failed verification must leave the active v1 artifact and state intact.
	if err := os.WriteFile(filepath.Join(artifactDir, "payesh-agent"), []byte("v2-agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	verifyErr := errors.New("simulated release digest mismatch")
	failed, err := Install(context.Background(), InstallOptions{
		Root: root, Role: "node", ArtifactDir: artifactDir,
		Verify:         func(string, string) error { return verifyErr },
		AccountManager: &testAccountManager{},
	})
	if !errors.Is(err, verifyErr) || len(failed.Installed) != 0 {
		t.Fatalf("failed upgrade result=%+v err=%v", failed, err)
	}
	if body, readErr := os.ReadFile(installed); readErr != nil || string(body) != "v1-agent" {
		t.Fatalf("failed upgrade changed active artifact=%q err=%v", body, readErr)
	}

	second, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("successful upgrade retry: %v", err)
	}
	if !second.Resumed {
		t.Fatal("upgrade retry did not resume persisted install state")
	}
	if body, readErr := os.ReadFile(installed); readErr != nil || string(body) != "v2-agent" {
		t.Fatalf("upgraded artifact=%q err=%v", body, readErr)
	}
	if body, readErr := os.ReadFile(identity); readErr != nil || string(body) != "stable-acceptance-id\n" {
		t.Fatalf("upgrade changed identity=%q err=%v", body, readErr)
	}

	remover := &acceptanceServiceRemover{}
	removed, err := Uninstall(context.Background(), UninstallOptions{Root: root, Role: "node", Stop: true, ServiceRemover: remover})
	if err != nil {
		t.Fatalf("safe uninstall: %v", err)
	}
	if !removed.DataPreserved || !remover.stop || !reflect.DeepEqual(remover.services, []string{"payesh-agent"}) {
		t.Fatalf("uninstall did not preserve data/stop exact service: %+v remover=%+v", removed, remover)
	}
	for _, path := range []string{"usr/bin/payesh-agent", "usr/bin/payesh-privd", "usr/bin/payesh", "etc/systemd/system/payesh-agent.service"} {
		if _, err := os.Stat(filepath.Join(root, path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned path %s remains: %v", path, err)
		}
	}
	if _, err := os.Stat(identity); err != nil {
		t.Fatalf("preserved identity missing: %v", err)
	}

	removed, err = Uninstall(context.Background(), UninstallOptions{Root: root, Role: "node", RemoveData: true, ServiceRemover: remover})
	if err != nil {
		t.Fatalf("explicit data removal: %v", err)
	}
	if removed.DataPreserved {
		t.Fatal("explicit data removal was reported as preserved")
	}
	for _, path := range []string{"var/lib/payesh", "etc/payesh", "var/log/payesh"} {
		if _, err := os.Stat(filepath.Join(root, path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("data path %s remains after explicit removal: %v", path, err)
		}
	}
}

func TestUninstallRefusesSymlinkOwnedPath(t *testing.T) {
	root, _ := installFixture(t, "systemd")
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("do not remove"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "usr/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "usr/bin/payesh-agent")); err != nil {
		t.Fatal(err)
	}
	_, err := Uninstall(context.Background(), UninstallOptions{Root: root, Role: "node", ServiceRemover: &acceptanceServiceRemover{}})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink removal err=%v", err)
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Fatalf("outside target was touched: %v", statErr)
	}
}
