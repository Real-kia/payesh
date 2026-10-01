package install

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Real-kia/payesh/internal/webupdate"
)

func TestWebRolesInstallAndRemoveUpdateWorker(t *testing.T) {
	for _, init := range []string{"systemd", "openrc"} {
		for _, role := range []string{"standalone", "hub", "node"} {
			t.Run(init+"/"+role, func(t *testing.T) {
				root, artifacts := installFixture(t, init)
				if role != "node" {
					if err := os.WriteFile(filepath.Join(artifacts, "payesh-server"), []byte("test-server"), 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.MkdirAll(filepath.Join(artifacts, "web-assets"), 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(artifacts, "web-assets", "index.html"), []byte("test-dashboard"), 0644); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := Install(context.Background(), InstallOptions{Root: root, Role: role, ArtifactDir: artifacts, Verify: acceptArtifact}); err != nil {
					t.Fatal(err)
				}
				if webupdate.Supported(root) != (role != "node") {
					t.Fatalf("worker installed for wrong role %s", role)
				}
				if role == "node" {
					return
				}
				body, err := os.ReadFile(servicePath(root, init, updateWorkerService))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(body), "/usr/bin/payesh") || !strings.Contains(string(body), "update-worker") {
					t.Fatalf("wrong worker command: %s", body)
				}
				if strings.Contains(string(body), "/etc/payesh/payesh.env") || strings.Contains(string(body), "command=\"/bin/sh\"") || strings.Contains(string(body), "/var/log/payesh") {
					t.Fatal("root worker must not source the service-account configuration")
				}
				if init == "systemd" && !strings.Contains(string(body), "EnvironmentFile=-/etc/payesh-update.env") {
					t.Fatal("wrong root-owned environment file")
				}
				if err := removeUpdateWorker(context.Background(), root, init, nil); err != nil {
					t.Fatal(err)
				}
				if webupdate.Supported(root) {
					t.Fatal("worker still installed after removal")
				}
			})
		}
	}
}
