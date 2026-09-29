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
	for _, statement := range []string{`CREATE TABLE servers (id TEXT, role TEXT)`, `CREATE TABLE fleet_identity_state (singleton INTEGER, state_json BLOB)`} {
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
	result, err := Install(context.Background(), InstallOptions{Root: root, Role: "node", ArtifactDir: artifacts, Verify: acceptArtifact, AccountManager: &testAccountManager{}, ServiceRemover: remover, ServiceManager: &testServiceManager{}, Conversion: &cfg, Start: true})
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
