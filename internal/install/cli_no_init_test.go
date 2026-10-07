package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Real-kia/payesh/internal/webupdate"
)

func TestCLIOnlyInstallWithoutInitWritesTruthfulScopeAndResumes(t *testing.T) {
	root, artifacts := installFixture(t, "systemd")
	if err := os.RemoveAll(filepath.Join(root, "run")); err != nil {
		t.Fatal(err)
	}
	preflight, err := Check(root, "cli-only", "")
	if err != nil || !preflight.Supported || preflight.Init != "unknown" {
		t.Fatalf("CLI preflight without init: %+v %v", preflight, err)
	}
	pin, err := ArtifactDigest(filepath.Join(artifacts, "payesh"), false)
	if err != nil {
		t.Fatal(err)
	}
	services := &testServiceManager{}
	opts := InstallOptions{Root: root, Role: "cli-only", ArtifactDir: artifacts, Start: true, ServiceManager: services, AccountManager: &testAccountManager{}, Verify: func(name, path string) error {
		if name != "payesh" {
			t.Fatalf("CLI unexpectedly requested %s", name)
		}
		actual, err := ArtifactDigest(path, false)
		if err != nil {
			return err
		}
		if actual != pin {
			return fmt.Errorf("CLI artifact digest mismatch")
		}
		return nil
	}}
	for attempt := 0; attempt < 2; attempt++ {
		result, err := Install(context.Background(), opts)
		if err != nil {
			t.Fatalf("install attempt %d after supported preflight: %v", attempt, err)
		}
		if result.Started || len(result.Services) != 0 || len(services.services) != 0 || result.Resumed != (attempt == 1) {
			t.Fatalf("CLI service/resume behavior: %+v", result)
		}
		scope, err := webupdate.ReadInstallationScope(root)
		if err != nil || scope.Role != "cli-only" || scope.Init != "unknown" {
			t.Fatalf("CLI installation misrepresented its init: %+v %v", scope, err)
		}
		if actual, err := ArtifactDigest(filepath.Join(root, "usr/bin/payesh"), false); err != nil || actual != pin {
			t.Fatalf("CLI artifact mismatch: %s %v", actual, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "usr/bin/payesh-agent")); !os.IsNotExist(err) {
		t.Fatalf("CLI installed an agent: %v", err)
	}
}
