package webupdate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallationScopeAllowsUnknownInitOnlyForCLI(t *testing.T) {
	for _, role := range []string{"cli-only", "hub", "standalone", "node"} {
		t.Run(role, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "etc"), 0755); err != nil {
				t.Fatal(err)
			}
			scope := InstallationScope{Role: role, Init: "unknown"}
			err := WriteInstallationScope(root, scope)
			if role != "cli-only" {
				if err == nil {
					t.Fatal("service role accepted unknown init")
				}
				if _, err := os.Lstat(scopePath(root)); !os.IsNotExist(err) {
					t.Fatalf("invalid scope wrote a file: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CLI scope on a host without a service manager: %v", err)
			}
			got, err := ReadInstallationScope(root)
			if err != nil || got != scope {
				t.Fatalf("CLI scope round trip: %+v %v", got, err)
			}
			calls := 0
			txn := InstallationTransaction{Root: root, JobID: "cli-scope-rejection01", Services: func(context.Context, string, string, string) error { calls++; return nil }}
			if err := txn.Prepare(context.Background()); err == nil || !strings.Contains(err.Error(), "CLI role cannot receive fleet activation") || calls != 0 {
				t.Fatalf("CLI scope permitted fleet activation: err=%v services=%d", err, calls)
			}
			if _, err := os.Lstat(filepath.Join(txn.dir(), "journal.json")); !os.IsNotExist(err) {
				t.Fatalf("CLI scope produced an activation journal: %v", err)
			}
		})
	}
}
