package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/netproxy"
)

// TestNodeReachesHubThroughRelayNode runs the relay path end to end: the hub
// makes one node a relay through its acknowledgements, and a second node that
// only knows the relay's address connects to the hub through it with mutual
// TLS intact.
func TestNodeReachesHubThroughRelayNode(t *testing.T) {
	if os.Getenv("PAYESH_LIVE_TRANSPORT_ACCEPTANCE") != "1" {
		t.Skip("set PAYESH_LIVE_TRANSPORT_ACCEPTANCE=1 to run the disposable TLS relay acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
	relayID, targetID := contracts.ServerID("server-relay-node-0001"), contracts.ServerID("server-iran-target-0001")
	identities := map[contracts.ServerID]NodeIdentity{}
	for _, id := range []contracts.ServerID{relayID, targetID} {
		if err := store.EnsureServer(ctx, contracts.Server{ID: id, Name: string(id), Role: "node", Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics"}, ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
			t.Fatal(err)
		}
		identity, err := ca.IssueNodeIdentity(id, now)
		if err != nil {
			t.Fatal(err)
		}
		identities[id] = identity
	}

	mux := http.NewServeMux()
	mux.Handle("/node/v1", WebSocketHandler(hub))
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverCert, serverKey, err := disposableServerCertificate()
	if err != nil {
		t.Fatal(err)
	}
	serverTLSCert, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	tlsListener := tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequestClientCert, Certificates: []tls.Certificate{serverTLSCert}})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(tlsListener) }()
	defer server.Close()
	parsed, _ := x509.ParseCertificate(serverTLSCert.Certificate[0])
	trust := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: parsed.Raw})
	hubAddr := tlsListener.Addr().String()

	// Pick a free port for the relay, then tell the hub about it.
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relayPort := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	if _, err := store.EnsureRelayNode(ctx, relayID, relayPort, now); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRelayGrant(ctx, monitoring.RelayGrantRecord{RelayServerID: relayID, Username: "node-target", PasswordSHA256: netproxy.HashPassword("node-secret"), Destinations: []string{hubAddr}, TargetServerID: targetID}, now); err != nil {
		t.Fatal(err)
	}

	sample := func(id contracts.ServerID, value float64) contracts.SampleBatch {
		return contracts.SampleBatch{Samples: []contracts.NodeMetricSample{{ServerID: id, CollectorEpoch: contracts.CollectorEpoch("epoch-" + string(id)), Sequence: 0, ObservedAt: now, Values: map[string]float64{"cpu.utilization": value}}}}
	}
	run := func(client *AgentClient, batch contracts.SampleBatch) error {
		batches := make(chan contracts.SampleBatch, 1)
		batches <- batch
		close(batches)
		return client.Run(ctx, batches)
	}
	hello := func(capabilities ...string) contracts.Hello {
		return contracts.Hello{Version: "test", ProtocolMin: contracts.ProtocolVersion, ProtocolMax: contracts.ProtocolVersion, Architecture: "amd64", Platform: "linux", Capabilities: append([]string{"metrics"}, capabilities...)}
	}
	newSpool := func() *Spool {
		spool, err := OpenSpool(filepath.Join(t.TempDir(), "agent.spool"), MaxSpoolBytes)
		if err != nil {
			t.Fatal(err)
		}
		return spool
	}

	// The relay node connects directly and receives its relay configuration.
	relay := &netproxy.RelayListener{}
	defer relay.Apply(0, nil)
	var relayPolicies atomic.Int32
	relayClient := &AgentClient{URL: "wss://" + hubAddr + "/node/v1", Identity: identities[relayID], TrustPEM: trust, Spool: newSpool(), Hello: hello(RelayCapability, AddressesCapability),
		Addresses: func() []string { return []string{"198.51.100.7", "10.0.0.2"} },
		RelayPolicy: func(policy contracts.RelayPolicy) {
			relayPolicies.Add(1)
			grants := make([]netproxy.Grant, 0, len(policy.Grants))
			for _, grant := range policy.Grants {
				grants = append(grants, netproxy.Grant{Username: grant.Username, PasswordSHA256: grant.PasswordSHA256, Destinations: grant.Destinations})
			}
			port := 0
			if policy.Enabled {
				port = policy.Port
			}
			if err := relay.Apply(port, grants); err != nil {
				t.Errorf("apply relay policy: %v", err)
			}
		}}
	if err := run(relayClient, sample(relayID, 1)); err != nil {
		t.Fatalf("relay agent: %v", err)
	}
	if relayPolicies.Load() == 0 || relay.Port() != relayPort {
		t.Fatalf("relay policy not applied: calls=%d port=%d", relayPolicies.Load(), relay.Port())
	}
	// The hub asked for and stored the relay node's interface addresses.
	var reported []string
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if server, ok, err := store.GetServer(ctx, relayID); err == nil && ok && len(server.Addresses) > 0 {
			reported = server.Addresses
			break
		}
	}
	if len(reported) < 2 || reported[len(reported)-2] != "198.51.100.7" || reported[len(reported)-1] != "10.0.0.2" {
		t.Fatalf("reported addresses = %v", reported)
	}

	// The target can only reach the hub through the relay.
	proxy, err := netproxy.Parse("http://node-target:node-secret@127.0.0.1:" + strconv.Itoa(relayPort))
	if err != nil {
		t.Fatal(err)
	}
	var targetPolicies atomic.Int32
	targetClient := &AgentClient{URL: "wss://" + hubAddr + "/node/v1", Identity: identities[targetID], TrustPEM: trust, Spool: newSpool(), Hello: hello(), Proxy: proxy,
		Addresses:   func() []string { targetPolicies.Add(100); return nil },
		RelayPolicy: func(contracts.RelayPolicy) { targetPolicies.Add(1) }}
	if err := run(targetClient, sample(targetID, 2)); err != nil {
		t.Fatalf("target agent through relay: %v", err)
	}
	if targetPolicies.Load() != 0 {
		t.Fatal("an agent without the relay or addresses capability received a relay policy or an address request")
	}
	page, err := store.QueryMetrics(ctx, targetID, now.Add(-time.Minute), now.Add(time.Minute), 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Samples) != 1 || page.Samples[0].Values["cpu.utilization"] != 2 {
		t.Fatalf("relayed sample not stored: %+v", page.Samples)
	}

	// The install-time port check uses the same proxy path.
	checker := &AgentClient{URL: "wss://" + hubAddr + "/node/v1", Identity: identities[targetID], TrustPEM: trust, Proxy: proxy}
	if err := checker.CheckEndpoint(ctx); err != nil {
		t.Fatalf("endpoint check through relay: %v", err)
	}

	// A wrong password is refused by the relay before reaching the hub.
	bad, _ := netproxy.Parse("http://node-target:wrong@127.0.0.1:" + strconv.Itoa(relayPort))
	badClient := &AgentClient{URL: "wss://" + hubAddr + "/node/v1", Identity: identities[targetID], TrustPEM: trust, Proxy: bad}
	if err := badClient.CheckEndpoint(ctx); err == nil {
		t.Fatal("relay accepted a wrong password")
	}

	// Removing the relay stops it at the next acknowledgement.
	if err := store.RemoveRelayNode(ctx, relayID); err != nil {
		t.Fatal(err)
	}
	batch := sample(relayID, 3)
	batch.Samples[0].Sequence = 1
	batch.Samples[0].ObservedAt = now.Add(15 * time.Second)
	if err := run(relayClient, batch); err != nil {
		t.Fatalf("relay agent second run: %v", err)
	}
	if relay.Port() != 0 {
		t.Fatalf("relay still listening on %d after removal", relay.Port())
	}
}
