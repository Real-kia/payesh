package main

import (
	"database/sql"
	"flag"
	"fmt"
	"github.com/Real-kia/payesh/internal/monitoring"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDigestFlagsRequireUniqueLowercaseSHA256(t *testing.T) {
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	parsed, err := (digestFlags{"payesh=" + digest}).parse()
	if err != nil || parsed["payesh"] != digest {
		t.Fatalf("parsed=%v err=%v", parsed, err)
	}
	for _, values := range []digestFlags{
		{"payesh=short"},
		{"payesh=ABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCD"},
		{"payesh=" + digest, "payesh=" + digest},
	} {
		if _, err := values.parse(); err == nil {
			t.Fatalf("invalid digest flags accepted: %v", values)
		}
	}
}

func TestInstallerSchemaCapabilitySubprocess(t *testing.T) {
	if os.Getenv("PAYESH_SCHEMA_CAPABILITY_CHILD") != "1" {
		return
	}
	flag.CommandLine = flag.NewFlagSet("payesh-install", flag.ExitOnError)
	os.Args = []string{"payesh-install", "--check-schema", "--json", "--root", os.Getenv("PAYESH_SCHEMA_CAPABILITY_ROOT"), "--role", os.Getenv("PAYESH_SCHEMA_CAPABILITY_ROLE")}
	main()
}
func TestInstallerSchemaCapabilityChecksRealDatabaseWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		version    int
		role       string
		rootSuffix string
		ok         bool
	}{
		{"readable", 5, "node", "", true}, {"newer", monitoring.CurrentSchemaVersion + 1, "hub", "", false}, {"unknown-role", 5, "invalid", "", false}, {"unsafe-root", 5, "node", "/..", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "var/lib/payesh/payesh.db")
			if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`CREATE TABLE schema_meta(version INTEGER NOT NULL);INSERT INTO schema_meta VALUES(?)`, tc.version); err != nil {
				t.Fatal(err)
			}
			db.Close()
			before, _ := os.ReadFile(path)
			command := exec.Command(os.Args[0], "-test.run=^TestInstallerSchemaCapabilitySubprocess$")
			command.Env = append(os.Environ(), "PAYESH_SCHEMA_CAPABILITY_CHILD=1", "PAYESH_SCHEMA_CAPABILITY_ROOT="+root+tc.rootSuffix, "PAYESH_SCHEMA_CAPABILITY_ROLE="+tc.role)
			output, err := command.CombinedOutput()
			if (err == nil) != tc.ok {
				t.Fatalf("schema capability %s err=%v output=%s", tc.name, err, output)
			}
			if tc.ok && !strings.Contains(string(output), `"schema_compatible":true`) {
				t.Fatalf("no actual capability receipt: %s", output)
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Fatal("read-only candidate capability mutated database")
			}
			for _, name := range []string{"etc", "usr/bin", "var/lib/payesh/install-state.json"} {
				if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
					t.Fatal(fmt.Sprintf("read-only capability created %s", name))
				}
			}
		})
	}
}
