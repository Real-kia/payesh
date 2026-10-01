package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Real-kia/payesh/internal/version"
	"github.com/Real-kia/payesh/internal/webupdate"
)

func TestInstalledRoleRejectsUnknownAndReadsNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install-state.json")
	for _, role := range []string{"node", "unknown"} {
		data, _ := json.Marshal(map[string]string{"role": role})
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := installedRole(path)
		if role == "node" && (err != nil || got != role) {
			t.Fatalf("role=%q err=%v", got, err)
		}
		if role == "unknown" && err == nil {
			t.Fatal("unknown role accepted")
		}
	}
}

func TestWebUpdateRefusesDowngradeAndEqualVersions(t *testing.T) {
	old := version.Value
	version.Value = "1.2.3"
	defer func() { version.Value = old }()
	for _, target := range []string{"1.2.2", "1.2.3"} {
		dir := t.TempDir()
		done, err := runWebUpdate(context.Background(), dir, target)
		if err != nil || done {
			t.Fatalf("target=%s done=%v err=%v", target, done, err)
		}
		status, err := webupdate.ReadStatus(dir)
		if err != nil || status.State != webupdate.StateFailed {
			t.Fatalf("status=%+v err=%v", status, err)
		}
	}
}
