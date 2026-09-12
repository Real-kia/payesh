package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

// TestTwoAgentsBootstrapAndIngest exercises the complete disposable multi-node
// boundary: both nodes bootstrap over the server-authenticated endpoint, then
// reconnect with distinct client certificates and ingest independently over the
// mTLS node endpoint. It uses an ephemeral loopback listener and a temporary
// SQLite store; it never claims a fixed application port.
func TestTwoAgentsBootstrapAndIngest(t *testing.T) {
	if os.Getenv("PAYESH_LIVE_TRANSPORT_ACCEPTANCE") != "1" {
		t.Skip("set PAYESH_LIVE_TRANSPORT_ACCEPTANCE=1 to run the disposable TLS multi-node acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	now := time.Now().UTC().Truncate(time.Second)
	store, err := monitoring.OpenStore(ctx, filepath.Join(t.TempDir(), "hub.db"), monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ca, err := NewCertificateAuthority(now)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := NewHub(ca, store)
	if err != nil {
		t.Fatal(err)
	}

	ids := []contracts.ServerID{"server-multi-a-012345", "server-multi-b-012345"}
	jobs := make([]contracts.Job, 0, len(ids))
	tokens := make([]string, 0, len(ids))
	for i := range ids {
		id := ids[i]
		if err := store.EnsureServer(ctx, contracts.Server{ID: id, Name: "multi-node", Role: "node", Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics"}, ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
			t.Fatal(err)
		}
		enrollment, issueErr := ca.IssueEnrollment(id, now)
		if issueErr != nil {
			t.Fatal(issueErr)
		}
		job, created, createErr := store.CreateJob(ctx, contracts.Job{ID: "enroll-multi-" + string(rune('a'+i)), Kind: "enrollment", State: contracts.JobQueued, IdempotencyKey: "enroll-multi-key-" + string(rune('a'+i)), TargetServerID: id, ExpiresAt: now.Add(time.Minute)}, "request-hash", now)
		if createErr != nil || !created {
			t.Fatalf("create enrollment job: job=%+v created=%v err=%v", job, created, createErr)
		}
		jobs = append(jobs, job)
		tokens = append(tokens, enrollment.Token)
	}

	mux := http.NewServeMux()
	mux.Handle("/node/bootstrap/v1", BootstrapWebSocketHandler(hub))
	mux.Handle("/node/v1", WebSocketHandler(hub))
	// Some restricted CI sandboxes do not permit IPv6 loopback listeners. Use
	// an ephemeral IPv4 port explicitly; this also proves the test never claims
	// a fixed or application port.
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal("listen disposable transport port:", err)
	}
	serverCert, serverKey, err := disposableServerCertificate()
	if err != nil {
		_ = listener.Close()
		t.Fatal("generate disposable TLS certificate:", err)
	}
	serverTLSCert, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	tlsListener := tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequestClientCert, Certificates: []tls.Certificate{serverTLSCert}})
	server := &http.Server{Handler: mux}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(tlsListener) }()
	defer func() {
		_ = server.Close()
		_ = <-serveDone
	}()
	parsedServerCert, _ := x509.ParseCertificate(serverTLSCert.Certificate[0])
	serverTrust := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: parsedServerCert.Raw})
	endpoint := "wss://" + tlsListener.Addr().String()

	identities := make([]NodeIdentity, len(ids))
	for i := range ids {
		bootstrapEndpoint := endpoint + "/node/bootstrap/v1"
		identity, bootstrapErr := BootstrapAgent(ctx, bootstrapEndpoint, jobs[i].ID, tokens[i], serverTrust, nil)
		if bootstrapErr != nil {
			t.Fatalf("bootstrap node %s: %v", ids[i], bootstrapErr)
		}
		if identity.ServerID != ids[i] {
			t.Fatalf("bootstrap identity mismatch: got=%s want=%s", identity.ServerID, ids[i])
		}
		identities[i] = identity
	}

	clients := make([]*AgentClient, len(ids))
	for i := range ids {
		spool, spoolErr := OpenSpool(filepath.Join(t.TempDir(), "agent.spool"), MaxSpoolBytes)
		if spoolErr != nil {
			t.Fatal(spoolErr)
		}
		clients[i] = &AgentClient{URL: endpoint + "/node/v1", Identity: identities[i], TrustPEM: serverTrust, Spool: spool, Hello: contracts.Hello{Version: "test", ProtocolMin: contracts.ProtocolVersion, ProtocolMax: contracts.ProtocolVersion, Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics"}}}
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(clients))
	for i, client := range clients {
		wg.Add(1)
		go func(index int, c *AgentClient) {
			defer wg.Done()
			batches := make(chan contracts.SampleBatch, 1)
			batches <- contracts.SampleBatch{Samples: []contracts.NodeMetricSample{{ServerID: ids[index], CollectorEpoch: contracts.CollectorEpoch("epoch-multi-" + string(rune('a'+index))), Sequence: 0, ObservedAt: now, Values: map[string]float64{"cpu.utilization": float64(index + 1)}}}}
			close(batches)
			errs <- c.Run(ctx, batches)
		}(i, client)
	}
	wg.Wait()
	close(errs)
	for runErr := range errs {
		if runErr != nil {
			t.Fatalf("agent run: %v", runErr)
		}
	}

	for i, id := range ids {
		page, err := store.QueryMetrics(ctx, id, now.Add(-time.Minute), now.Add(time.Minute), 10, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Samples) != 1 || page.Samples[0].ServerID != id || page.Samples[0].Values["cpu.utilization"] != float64(i+1) {
			t.Fatalf("node %s durable samples=%+v", id, page.Samples)
		}
	}
}

// TestAgentRenewsAndDeliversAction exercises the live websocket behavior that
// unit tests cannot prove: an authenticated node renews and atomically persists
// its identity, reconnects after predecessor revocation, then receives and
// completes a durable typed action. The action handler is deliberately a
// side-effect-free no-op.
func TestAgentRenewsAndDeliversAction(t *testing.T) {
	if os.Getenv("PAYESH_LIVE_TRANSPORT_ACCEPTANCE") != "1" {
		t.Skip("set PAYESH_LIVE_TRANSPORT_ACCEPTANCE=1 to run the disposable TLS renewal/action acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	now := time.Now().UTC().Truncate(time.Second)
	store, err := monitoring.OpenStore(ctx, filepath.Join(t.TempDir(), "hub.db"), monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ca, err := NewCertificateAuthority(now)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := NewHub(ca, store)
	if err != nil {
		t.Fatal(err)
	}
	id := contracts.ServerID("server-renew-action-012345")
	if err := store.EnsureServer(ctx, contracts.Server{ID: id, Name: "renew-action", Role: "node", Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics", "actions"}, ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	enrollment, err := ca.IssueEnrollment(id, now)
	if err != nil {
		t.Fatal(err)
	}
	enrollmentJob, created, err := store.CreateJob(ctx, contracts.Job{ID: "enroll-renew-action-01", Kind: "enrollment", State: contracts.JobQueued, IdempotencyKey: "enroll-renew-action-key", TargetServerID: id, ExpiresAt: now.Add(time.Minute)}, "renew-action-enrollment", now)
	if err != nil || !created {
		t.Fatalf("create enrollment job: created=%v err=%v", created, err)
	}

	mux := http.NewServeMux()
	mux.Handle("/node/bootstrap/v1", BootstrapWebSocketHandler(hub))
	mux.Handle("/node/v1", WebSocketHandler(hub))
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverCert, serverKey, err := disposableServerCertificate()
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	serverTLSCert, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	tlsListener := tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequestClientCert, Certificates: []tls.Certificate{serverTLSCert}})
	server := &http.Server{Handler: mux}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(tlsListener) }()
	defer func() {
		_ = server.Close()
		_ = <-serveDone
	}()
	parsedServerCert, _ := x509.ParseCertificate(serverTLSCert.Certificate[0])
	serverTrust := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: parsedServerCert.Raw})
	endpoint := "wss://" + tlsListener.Addr().String()
	identity, err := BootstrapAgent(ctx, endpoint+"/node/bootstrap/v1", enrollmentJob.ID, enrollment.Token, serverTrust, nil)
	if err != nil {
		t.Fatal("bootstrap:", err)
	}
	oldFingerprint := identity.Fingerprint
	// Only the node-side scheduling hint is shortened. The authenticated
	// certificate remains CA-issued and valid, while the replacement retains
	// its normal validity and therefore does not create a renewal loop.
	identity.NotAfter = now.Add(-time.Second)
	identityPath := filepath.Join(t.TempDir(), "node-identity.json")
	if err := SaveNodeIdentity(identityPath, identity); err != nil {
		t.Fatal(err)
	}
	spool, err := OpenSpool(filepath.Join(t.TempDir(), "agent.spool"), MaxSpoolBytes)
	if err != nil {
		t.Fatal(err)
	}
	var actionCalls atomic.Int32
	client := &AgentClient{
		URL: endpoint + "/node/v1", Identity: identity, IdentityPath: identityPath, TrustPEM: serverTrust, Spool: spool,
		Hello: contracts.Hello{Version: "test", ProtocolMin: contracts.ProtocolVersion, ProtocolMax: contracts.ProtocolVersion, Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics", "actions"}},
		ActionHandler: func(_ context.Context, request contracts.ActionRequest) contracts.ActionResponse {
			actionCalls.Add(1)
			return contracts.ActionResponse{RequestID: request.RequestID, Accepted: true, Revision: 1}
		},
	}
	batches := make(chan contracts.SampleBatch)
	agentDone := make(chan error, 1)
	go func() { agentDone <- client.Run(ctx, batches) }()

	var renewed NodeIdentity
	for ctx.Err() == nil {
		renewed, err = LoadNodeIdentity(identityPath)
		if err == nil && renewed.Fingerprint != oldFingerprint {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if ctx.Err() != nil {
		t.Fatal("timed out waiting for persisted renewal")
	}
	if _, err := ca.Verify(identity.CertificatePEM, time.Now().UTC()); err == nil || err.Error() != "certificate_revoked" {
		t.Fatalf("predecessor certificate verification error=%v", err)
	}
	if got, err := ca.Verify(renewed.CertificatePEM, time.Now().UTC()); err != nil || got != id {
		t.Fatalf("replacement certificate verification id=%s err=%v", got, err)
	}

	actionNow := time.Now().UTC()
	request := contracts.ActionRequest{Protocol: contracts.HelperProtocol, RequestID: "request-renew-action-01", Action: "acceptance.noop", Target: "acceptance", TargetServerID: id, IdempotencyKey: "action-renew-idem-01", ExpectedRevision: 0, Deadline: actionNow.Add(5 * time.Second), Arguments: []byte(`{"mode":"no-op"}`)}
	job := contracts.Job{ID: "action-renew-delivery-01", Kind: "action", State: contracts.JobQueued, IdempotencyKey: request.IdempotencyKey, TargetServerID: id, ExpiresAt: request.Deadline}
	if _, created, err := hub.CreateActionJob(ctx, job, request, "renew-action-request", actionNow); err != nil || !created {
		t.Fatalf("create action job: created=%v err=%v", created, err)
	}
	for ctx.Err() == nil {
		completed, found, getErr := store.GetJob(ctx, job.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if found && completed.State == contracts.JobSucceeded {
			if completed.Result == nil || !completed.Result.Accepted || actionCalls.Load() != 1 {
				t.Fatalf("action result=%+v calls=%d", completed.Result, actionCalls.Load())
			}
			cancel()
			if runErr := <-agentDone; runErr != nil {
				t.Fatal("agent shutdown:", runErr)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("timed out waiting for durable action completion")
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

// Ensure the test's TLS trust setup is not accidentally weakened by a future
// change that makes the bootstrap client accept a system-wide root pool.
func TestBootstrapRequiresTrustWhenNoTLSConfig(t *testing.T) {
	_, err := BootstrapAgent(context.Background(), "wss://127.0.0.1:1/node/bootstrap/v1", "job-012345678901", "token", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "trust anchor") {
		t.Fatalf("expected explicit trust-anchor failure, got %v", err)
	}
}
