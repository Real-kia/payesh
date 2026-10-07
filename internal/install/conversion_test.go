package install

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/transport"
)

type conversionRemover struct{ services []string }

func (r *conversionRemover) Remove(_ context.Context, _, _ string, services []string, _ bool) error {
	r.services = append([]string(nil), services...)
	return nil
}

func conversionFixture(t *testing.T) (string, string, ConversionConfig, *sql.DB) {
	t.Helper()
	root, artifacts := installFixture(t, "systemd")
	if err := os.MkdirAll(filepath.Join(root, "var/lib/payesh"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := saveState(filepath.Join(root, "var/lib/payesh/install-state.json"), installState{Role: "hub", Init: "systemd"}); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-node"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	identity, err := json.Marshal(transport.NodeIdentity{ServerID: "test-node", CertificatePEM: certificate, PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})})
	if err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(t.TempDir(), "node.json")
	caPath := filepath.Join(t.TempDir(), "hub-ca.pem")
	if err := os.WriteFile(identityPath, identity, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, certificate, 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "var/lib/payesh/payesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{`CREATE TABLE schema_meta(version INTEGER NOT NULL)`, `INSERT INTO schema_meta VALUES(6)`, `CREATE TABLE servers (id TEXT, role TEXT)`, `CREATE TABLE fleet_identity_state (singleton INTEGER, state_json BLOB)`} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	return root, artifacts, ConversionConfig{FromRole: "hub", TransportURL: "wss://example.test/node/v1", NodeIdentityFile: identityPath, HubCAFile: caPath}, db
}

func TestConversionRequiresNodeMigration(t *testing.T) {
	root, _, cfg, db := conversionFixture(t)
	if _, err := db.Exec(`INSERT INTO servers(id, role) VALUES('old-node', 'node')`); err != nil {
		t.Fatal(err)
	}
	if err := CheckHubToNode(context.Background(), root, cfg); err == nil || !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("expected migration refusal, got %v", err)
	}
	if _, err := db.Exec(`DELETE FROM servers`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO fleet_identity_state(singleton, state_json) VALUES(1, '{"certificates":{"old-node":"fingerprint"}}')`); err != nil {
		t.Fatal(err)
	}
	if err := CheckHubToNode(context.Background(), root, cfg); err == nil || !strings.Contains(err.Error(), "certificates") {
		t.Fatalf("expected certificate refusal, got %v", err)
	}
}

func TestConversionStopsHubAndStartsNode(t *testing.T) {
	root, artifacts, cfg, _ := conversionFixture(t)
	for _, name := range []string{"payesh-server", "web-assets"} {
		path := artifactDestination(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if name == "web-assets" {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "etc/payesh"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/payesh/payesh.env"), []byte("PAYESH_BOOTSTRAP_SECRET=old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(servicePath(root, "systemd", "payesh-server")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(servicePath(root, "systemd", "payesh-server"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	remover := &conversionRemover{}
	result, err := Install(context.Background(), InstallOptions{Root: root, Role: "node", ArtifactDir: artifacts, Verify: acceptArtifact, AccountManager: &testAccountManager{}, ServiceRemover: remover, ServiceManager: &testServiceManager{}, Conversion: &cfg, ProbeConversion: func(context.Context, ConversionConfig) error { return nil }, Start: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Started || strings.Join(remover.services, ",") != "payesh-agent,payesh-server" {
		t.Fatalf("unexpected conversion result: %+v, removed %v", result, remover.services)
	}
	state, err := loadState(filepath.Join(root, "var/lib/payesh/install-state.json"))
	if err != nil || state.Role != "node" {
		t.Fatalf("role after conversion: %+v, %v", state, err)
	}
	if _, err := os.Stat(servicePath(root, "systemd", "payesh-server")); !os.IsNotExist(err) {
		t.Fatalf("old server service remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "usr/bin/payesh-server")); !os.IsNotExist(err) {
		t.Fatalf("old server binary remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "var/lib/payesh/payesh.db")); err != nil {
		t.Fatalf("database was removed: %v", err)
	}
	env, err := os.ReadFile(filepath.Join(root, "etc/payesh/payesh.env"))
	if err != nil || !strings.Contains(string(env), "PAYESH_TRANSPORT_URL=wss://example.test/node/v1") {
		t.Fatalf("node env: %s, %v", env, err)
	}
}

func TestConversionDestinationFailurePreservesWorkingHost(t *testing.T) {
	for _, reason := range []string{"port unreachable", "untrusted TLS"} {
		t.Run(reason, func(t *testing.T) {
			root, artifacts, cfg, _ := conversionFixture(t)
			paths := map[string]string{
				"usr/bin/payesh-agent": "old-agent", "usr/bin/payesh-server": "old-server",
				"etc/payesh/payesh.env":                    "existing-environment\n",
				"etc/systemd/system/payesh-server.service": "existing-unit\n",
				"usr/share/payesh/web-assets/index.html":   "existing-dashboard",
			}
			for path, data := range paths {
				full := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			accounts, remover, services := &testAccountManager{}, &conversionRemover{}, &testServiceManager{}
			failure := errors.New(reason)
			result, err := Install(context.Background(), InstallOptions{Root: root, Role: "node", ArtifactDir: artifacts,
				Verify: acceptArtifact, AccountManager: accounts, ServiceRemover: remover, ServiceManager: services,
				Conversion: &cfg, ProbeConversion: func(context.Context, ConversionConfig) error { return failure }, Start: true})
			if !errors.Is(err, failure) || len(result.Installed) != 0 || accounts.calls != 0 || len(remover.services) != 0 || len(services.services) != 0 {
				t.Fatalf("failed destination mutated host: result=%+v err=%v accounts=%d removed=%v services=%v", result, err, accounts.calls, remover.services, services.services)
			}
			for path, expected := range paths {
				actual, err := os.ReadFile(filepath.Join(root, path))
				if err != nil || string(actual) != expected {
					t.Fatalf("changed %s: %v", path, err)
				}
			}
			state, err := loadState(filepath.Join(root, "var/lib/payesh/install-state.json"))
			if err != nil || state.Role != "hub" {
				t.Fatalf("changed role: %+v, %v", state, err)
			}
			if _, err := os.Stat(filepath.Join(root, "var/lib/payesh/node-identity.json")); !os.IsNotExist(err) {
				t.Fatalf("identity installed after failed probe: %v", err)
			}
		})
	}
}

type failNodeStart struct{ calls [][]string }

func (m *failNodeStart) Apply(_ context.Context, _, _ string, services []string, _ bool) error {
	m.calls = append(m.calls, append([]string(nil), services...))
	if len(services) == 1 {
		return errors.New("node startup failed")
	}
	return nil
}
func TestConversionStartupFailureRestoresPriorRoleWithoutRevertingHistory(t *testing.T) {
	root, artifacts, cfg, db := conversionFixture(t)
	expected := map[string]string{"usr/bin/payesh-agent": "old-agent", "usr/bin/payesh-server": "old-server", "etc/payesh/payesh.env": "old-env\n", "usr/share/payesh/web-assets/index.html": "old-web", "etc/systemd/system/payesh-server.service": "old-unit"}
	for path, content := range expected {
		full := filepath.Join(root, path)
		os.MkdirAll(filepath.Dir(full), 0755)
		if err := os.WriteFile(full, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	manager := &failNodeStart{}
	_, err := Install(context.Background(), InstallOptions{Root: root, Role: "node", ArtifactDir: artifacts, Verify: acceptArtifact, AccountManager: &testAccountManager{}, ServiceRemover: &conversionRemover{}, ServiceManager: manager, Conversion: &cfg, ProbeConversion: func(context.Context, ConversionConfig) error { return nil }, Start: true})
	if err == nil {
		t.Fatal("startup failure accepted")
	}
	for path, want := range expected {
		got, e := os.ReadFile(filepath.Join(root, path))
		if e != nil || string(got) != want {
			t.Fatalf("prior %s lost: %q %v", path, got, e)
		}
	}
	state, e := loadState(filepath.Join(root, "var/lib/payesh/install-state.json"))
	if e != nil || state.Role != "hub" {
		t.Fatalf("role not restored: %+v %v", state, e)
	}
	if len(manager.calls) != 2 || strings.Join(manager.calls[1], ",") != "payesh-agent,payesh-server" {
		t.Fatalf("prior role not restarted: %v", manager.calls)
	}
	if _, e := db.Exec("INSERT INTO servers VALUES('history-still-writable','standalone')"); e != nil {
		t.Fatal(e)
	}
}

func TestInterruptedConversionRecoveryPreservesNewHistory(t *testing.T) {
	root, _, _, db := conversionFixture(t)
	path := filepath.Join(root, "usr/bin/payesh-agent")
	os.WriteFile(path, []byte("original"), 0755)
	tx, err := beginConversion(root, installState{Role: "hub", Init: "systemd"})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("interrupted-candidate"), 0755)
	if _, err := db.Exec("INSERT INTO servers VALUES('post-snapshot-history','standalone')"); err != nil {
		t.Fatal(err)
	}
	if err := RecoverConversion(root, InstallOptions{ServiceRemover: &conversionRemover{}, ServiceManager: &testServiceManager{}, Start: true}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "original" {
		t.Fatalf("prior binary not recovered: %q %v", data, err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM servers WHERE id='post-snapshot-history'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("new history reverted: %d %v", count, err)
	}
	if _, err := os.Stat(tx.dir); !os.IsNotExist(err) {
		t.Fatalf("recovery journal remains: %v", err)
	}
}

func TestConversionSnapshotRefusesSymlinkTree(t *testing.T) {
	root, _, _, _ := conversionFixture(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "untouched"), []byte("outside"), 0600)
	web := filepath.Join(root, "usr/share/payesh/web-assets")
	os.MkdirAll(filepath.Dir(web), 0755)
	if err := os.Symlink(outside, web); err != nil {
		t.Fatal(err)
	}
	if _, err := beginConversion(root, installState{Role: "hub", Init: "systemd"}); !errors.Is(err, ErrUnsafeArtifactPath) {
		t.Fatalf("symlink snapshot accepted: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(outside, "untouched"))
	if err != nil || string(got) != "outside" {
		t.Fatal("external tree changed")
	}
}
func TestConversionLockRejectsConcurrentOperation(t *testing.T) {
	root := t.TempDir()
	first, err := lockConversion(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := lockConversion(root); err == nil {
		second.Close()
		t.Fatal("concurrent conversion lock accepted")
	}
}

func TestConversionRecoveryRejectsIncompleteOrUnprotectedJournalBeforeStopping(t *testing.T) {
	for _, kind := range []string{"incomplete", "writable", "symlink", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "var/backups/payesh-conversion")
			os.MkdirAll(dir, 0700)
			journal := filepath.Join(dir, "journal.json")
			os.WriteFile(journal, []byte(`{"Role":"hub","Init":"systemd","Present":{}}`), 0600)
			switch kind {
			case "writable":
				os.Chmod(dir, 0777)
			case "symlink":
				os.Remove(journal)
				os.Symlink(filepath.Join(t.TempDir(), "foreign"), journal)
			case "oversized":
				os.WriteFile(journal, make([]byte, 65537), 0600)
			}
			remover := &conversionRemover{}
			if err := RecoverConversion(root, InstallOptions{ServiceRemover: remover}); err == nil {
				t.Fatal("unsafe journal accepted")
			}
			if len(remover.services) != 0 {
				t.Fatal("unsafe recovery stopped services")
			}
		})
	}
}

func TestConversionRollbackRestartsOriginallyActiveRoleWhenCandidateStartDisabled(t *testing.T) {
	root, artifacts, cfg, _ := conversionFixture(t)
	os.MkdirAll(filepath.Join(root, "etc/payesh"), 0755)
	os.WriteFile(filepath.Join(root, "etc/payesh/payesh.env"), []byte("old"), 0600)
	manager := &testServiceManager{}
	_, err := Install(context.Background(), InstallOptions{Root: root, Role: "node", ArtifactDir: artifacts, Verify: acceptArtifact, AccountManager: &testAccountManager{}, ServiceRemover: &conversionRemover{}, ServiceManager: manager, Conversion: &cfg, Start: false, ServiceActive: func(context.Context, string, string, string) (bool, error) { return true, nil }, ProbeConversion: func(_ context.Context, frozen ConversionConfig) error { os.Remove(frozen.NodeIdentityFile); return nil }})
	if err == nil {
		t.Fatal("missing identity accepted")
	}
	if strings.Join(manager.services, ",") != "payesh-agent,payesh-server" || !manager.start {
		t.Fatalf("active original not restarted: %+v", manager)
	}
}
func TestConversionSnapshotBudgetRejectsBeforeStopping(t *testing.T) {
	root, artifacts, cfg, _ := conversionFixture(t)
	file, err := os.Create(filepath.Join(root, "usr/bin/payesh-agent"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(513 << 20); err != nil {
		t.Fatal(err)
	}
	file.Close()
	remover := &conversionRemover{}
	_, err = Install(context.Background(), InstallOptions{Root: root, Role: "node", ArtifactDir: artifacts, Verify: acceptArtifact, Conversion: &cfg, ServiceRemover: remover, ProbeConversion: func(context.Context, ConversionConfig) error { return nil }})
	if err == nil || !strings.Contains(err.Error(), "budget") || len(remover.services) != 0 {
		t.Fatalf("overbudget mutated host: %v %v", err, remover.services)
	}
}

func TestConversionRollbackPreservesExactActiveServiceSubset(t *testing.T) {
	root, artifacts, cfg, _ := conversionFixture(t)
	os.MkdirAll(filepath.Join(root, "etc/payesh"), 0755)
	os.WriteFile(filepath.Join(root, "etc/payesh/payesh.env"), []byte("old"), 0600)
	manager := &testServiceManager{}
	_, err := Install(context.Background(), InstallOptions{Root: root, Role: "node", ArtifactDir: artifacts, Verify: acceptArtifact, AccountManager: &testAccountManager{}, ServiceRemover: &conversionRemover{}, ServiceManager: manager, Conversion: &cfg, Start: false, ServiceActive: func(_ context.Context, _, _, service string) (bool, error) { return service == "payesh-server", nil }, ProbeConversion: func(_ context.Context, frozen ConversionConfig) error { os.Remove(frozen.NodeIdentityFile); return nil }})
	if err == nil {
		t.Fatal("missing identity accepted")
	}
	if strings.Join(manager.services, ",") != "payesh-server" {
		t.Fatalf("previously inactive agent enabled: %v", manager.services)
	}
}
func TestConversionRefusesWritableBackupAncestor(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "var/backups"), 0700)
	os.Chmod(filepath.Join(root, "var"), 0777)
	if lock, err := lockConversion(root); err == nil {
		lock.Close()
		t.Fatal("writable backup ancestor accepted")
	}
}

func TestConversionReplacesCustomIdentityTrustOverridesWithCopiedIdentity(t *testing.T) {
	root, _, cfg, _ := conversionFixture(t)
	os.MkdirAll(filepath.Join(root, "etc/payesh"), 0755)
	env := filepath.Join(root, "etc/payesh/payesh.env")
	os.WriteFile(env, []byte("PAYESH_NODE_IDENTITY_FILE=/custom/other-node.json\nPAYESH_HUB_TRUST_FILE=/custom/other-ca.pem\nPAYESH_TRANSPORT_URL=wss://old.example.test/node/v1\nKEEP_SETTING=yes\n"), 0640)
	if err := writeNodeConversionConfig(root, cfg, Account{UID: -1, GID: -1}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(env)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "/custom/") || !strings.Contains(string(data), "PAYESH_NODE_IDENTITY_FILE=/var/lib/payesh/node-identity.json\n") || !strings.Contains(string(data), "PAYESH_HUB_TRUST_FILE=/var/lib/payesh/hub-ca.pem\n") || !strings.Contains(string(data), "KEEP_SETTING=yes\n") {
		t.Fatalf("conversion retained mismatched identity: %s", data)
	}
	original, err := transport.LoadNodeIdentity(cfg.NodeIdentityFile)
	if err != nil {
		t.Fatal(err)
	}
	copied, err := transport.LoadNodeIdentity(filepath.Join(root, "var/lib/payesh/node-identity.json"))
	if err != nil || copied.ServerID != original.ServerID || string(copied.CertificatePEM) != string(original.CertificatePEM) {
		t.Fatalf("copied identity differs from probed identity: %v", err)
	}
}

func TestConversionCopyEnforcesAggregateGrowthBudget(t *testing.T) {
	source := t.TempDir()
	os.WriteFile(filepath.Join(source, "first"), []byte("1234"), 0600)
	os.WriteFile(filepath.Join(source, "second"), []byte("5678"), 0600)
	budget := &conversionCopyBudget{remaining: 6}
	dest := filepath.Join(t.TempDir(), "snapshot")
	if err := copyConversionPath(source, dest, budget); err == nil {
		t.Fatal("aggregate copy exceeded budget")
	}
}

func TestConversionUsesSameFrozenEnrollmentAfterProbeSourceChanges(t *testing.T) {
	root, artifacts, cfg, _ := conversionFixture(t)
	original, err := os.ReadFile(cfg.NodeIdentityFile)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(root, "etc/payesh"), 0755)
	os.WriteFile(filepath.Join(root, "etc/payesh/payesh.env"), []byte("KEEP=yes\n"), 0640)
	_, err = Install(context.Background(), InstallOptions{Root: root, Role: "node", ArtifactDir: artifacts, Verify: acceptArtifact, AccountManager: &testAccountManager{}, ServiceRemover: &conversionRemover{}, ServiceManager: &testServiceManager{}, Conversion: &cfg, Start: true, ProbeConversion: func(_ context.Context, frozen ConversionConfig) error {
		probed, e := os.ReadFile(frozen.NodeIdentityFile)
		if e != nil || string(probed) != string(original) {
			t.Fatal("wrong probe identity")
		}
		return os.WriteFile(cfg.NodeIdentityFile, []byte("changed-source-after-probe"), 0600)
	}})
	if err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(filepath.Join(root, "var/lib/payesh/node-identity.json"))
	if err != nil || string(copied) != string(original) {
		t.Fatal("conversion used unprobed source")
	}
}

func TestConversionDirectorySwapRejectedBeforeEnumeration(t *testing.T) {
	parent := t.TempDir()
	inside := filepath.Join(parent, "tree")
	os.Mkdir(inside, 0700)
	os.WriteFile(filepath.Join(inside, "original"), []byte("original"), 0600)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "foreign"), []byte("must-not-copy"), 0600)
	confined, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer confined.Close()
	expected, err := confined.Lstat("tree")
	if err != nil {
		t.Fatal(err)
	}
	os.Rename(inside, filepath.Join(parent, "moved"))
	os.Symlink(outside, inside)
	if opened, err := openConversionDirectory(confined, "tree", expected); err == nil {
		opened.Close()
		t.Fatal("swapped external directory accepted")
	}
}
