package transport_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/nodehub"
	"github.com/Real-kia/payesh/internal/transport"
)

func freeNodePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	_ = l.Close()
	return port
}
func migrationStore(t *testing.T) *monitoring.Store {
	t.Helper()
	store, err := monitoring.OpenStore(context.Background(), filepath.Join(t.TempDir(), "test.db"), monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
func addMigrationNode(t *testing.T, store *monitoring.Store, id contracts.ServerID, supported bool) {
	t.Helper()
	caps := []string{"metrics"}
	if supported {
		caps = append(caps, transport.MigrationCapability)
	}
	if err := store.EnsureServer(context.Background(), contracts.Server{ID: id, Name: string(id), Role: "node", Architecture: "amd64", Platform: "linux", Capabilities: caps, ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
}
func disposableServerCertificate() ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "payesh-test-hub"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{netip.MustParseAddr("127.0.0.1").AsSlice()}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

func TestMigrationFailuresKeepWorkingEndpoint(t *testing.T) {
	for _, kind := range []string{"unreachable", "untrusted", "cannot-persist", "file-sync", "directory-sync"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			now := time.Now().UTC()
			ca, _ := transport.NewCertificateAuthority(now)
			enrollment, _ := ca.IssueEnrollment("node-failed-migration-001", now)
			identity, err := ca.Enroll(&enrollment, enrollment.Token, now)
			if err != nil {
				t.Fatal(err)
			}
			store := migrationStore(t)
			addMigrationNode(t, store, identity.ServerID, true)
			hub, _ := nodehub.NewHub(ca, store)
			certPEM, keyPEM, _ := disposableServerCertificate()
			certificate, _ := tls.X509KeyPair(certPEM, keyPEM)
			original := "wss://127.0.0.1:8787/node/v1"
			target := "wss://127.0.0.1:" + freeNodePort(t) + "/node/v1"
			if kind != "unreachable" {
				l, e := net.Listen("tcp", "127.0.0.1:0")
				if e != nil {
					t.Fatal(e)
				}
				server := &http.Server{Handler: nodehub.WebSocketHandler(hub)}
				defer server.Close()
				go func() {
					_ = server.Serve(tls.NewListener(l, &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequestClientCert, Certificates: []tls.Certificate{certificate}}))
				}()
				target = "wss://" + l.Addr().String() + "/node/v1"
			}
			trust := certPEM
			if kind == "untrusted" {
				trust, _, _ = disposableServerCertificate()
			}
			path := filepath.Join(t.TempDir(), "identity.json")
			if kind == "cannot-persist" {
				path = filepath.Join(t.TempDir(), "missing", "identity.json")
			}
			agent := &transport.AgentClient{URL: original, Identity: identity, IdentityPath: path, TrustPEM: trust}
			agent.SetConfiguredURL(original)
			if kind == "file-sync" || kind == "directory-sync" {
				agent.SetEndpointSync(func(file *os.File) error {
					info, err := file.Stat()
					if err != nil {
						return err
					}
					if (kind == "file-sync" && !info.IsDir()) || (kind == "directory-sync" && info.IsDir()) {
						return errors.New("injected transport sync failure")
					}
					return file.Sync()
				})
			}
			serverConn, clientConn := net.Pipe()
			defer serverConn.Close()
			defer clientConn.Close()
			_ = serverConn.SetDeadline(time.Now().Add(3 * time.Second))
			_ = clientConn.SetDeadline(time.Now().Add(3 * time.Second))
			ws := transport.NewWebSocket(clientConn, false)
			reader := transport.NewWebSocket(serverConn, true)
			done := make(chan error, 1)
			go func() { done <- agent.Migrate(ctx, ws, target) }()
			payload, _, err := reader.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			var result contracts.Envelope
			if err = transport.DecodeEnvelope(payload, &result); err != nil || result.Message != "transport_result" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			if agent.URL != original || agent.PreviousURL() != "" {
				t.Fatal("failed migration replaced working endpoint")
			}
			if _, err = os.Stat(path + ".transport.json"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed migration saved a new endpoint")
			}
		})
	}
}
