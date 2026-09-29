package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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
