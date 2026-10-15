package nodehub

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Real-kia/payesh/internal/transport"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
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
func TestNodePortsPersistRetireAndProtectPendingNodes(t *testing.T) {
	ctx := context.Background()
	store := migrationStore(t)
	path := filepath.Join(t.TempDir(), "ports.json")
	original := freeNodePort(t)
	public := func(port string) string { return "wss://hub.example.test:" + port + "/node/v1" }
	p, err := NewNodePorts(path, net.JoinHostPort("127.0.0.1", original), store, public, func(net.Listener) {})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { p.Close() }()
	if p.LegacyDashboard() {
		t.Fatal("fresh installation exposed node routes on dashboard")
	}
	addMigrationNode(t, store, "node-migration-00001", true)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, busy, _ := net.SplitHostPort(occupied.Addr().String())
	n, _ := strconv.Atoi(busy)
	if err = p.Change(n); err == nil {
		t.Fatal("occupied port accepted")
	}
	_ = occupied.Close()
	next := freeNodePort(t)
	n, _ = strconv.Atoi(next)
	if err = p.Change(n); err != nil {
		t.Fatal(err)
	}
	p.Close()
	p, err = NewNodePorts(path, net.JoinHostPort("127.0.0.1", original), store, public, func(net.Listener) {})
	if err != nil {
		t.Fatal(err)
	}
	status, _ := p.Status(ctx)
	if status.Port != next || status.Pending != 1 || len(status.PreviousPorts) != 1 {
		t.Fatalf("status=%+v", status)
	}
	if err = p.Retire(ctx); err == nil {
		t.Fatal("retired an offline node endpoint")
	}
	if err = p.Observe("node-migration-00001", next); err != nil {
		t.Fatal(err)
	}
	if err = p.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	p.Close()
	// A retired original port is not rebound on restart, even when occupied.
	blocked, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", original))
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()
	p, err = NewNodePorts(path, net.JoinHostPort("127.0.0.1", original), store, public, func(net.Listener) {})
	if err != nil {
		t.Fatal(err)
	}
	status, _ = p.Status(ctx)
	if status.Port != next || status.Pending != 0 || len(status.PreviousPorts) != 0 {
		t.Fatalf("restored=%+v", status)
	}
}
func TestLegacyNodesRequireUpgradeAndKeepDashboardAccess(t *testing.T) {
	store := migrationStore(t)
	addMigrationNode(t, store, "node-legacy-00000001", false)
	p, err := NewNodePorts(filepath.Join(t.TempDir(), "ports.json"), net.JoinHostPort("127.0.0.1", freeNodePort(t)), store, func(port string) string { return "wss://hub.example.test:" + port + "/node/v1" }, func(net.Listener) {})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	status, _ := p.Status(context.Background())
	if !status.LegacyDashboard || status.Nodes[0].State != "update-required" {
		t.Fatalf("status=%+v", status)
	}
	if err = p.Retire(context.Background()); err == nil {
		t.Fatal("legacy endpoint retired before migration")
	}
}
func TestLiveNodePortMigrationIncludesReturningOfflineNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store := migrationStore(t)
	ca, err := transport.NewCertificateAuthority(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	hub, _ := NewHub(ca, store)
	certPEM, keyPEM, err := disposableServerCertificate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: WebSocketHandler(hub)}
	defer server.Close()
	original := freeNodePort(t)
	ports, err := NewNodePorts(filepath.Join(t.TempDir(), "ports.json"), net.JoinHostPort("127.0.0.1", original), store, func(port string) string { return "wss://127.0.0.1:" + port + "/node/v1" }, func(l net.Listener) {
		go func() {
			_ = server.Serve(tls.NewListener(l, &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequestClientCert, Certificates: []tls.Certificate{cert}}))
		}()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ports.Close()
	hub.Ports = ports
	ports.Start()
	ids := []contracts.ServerID{"node-online-migrate-001", "node-offline-migrate-001"}
	identities := []transport.NodeIdentity{}
	for _, id := range ids {
		addMigrationNode(t, store, id, true)
		enrollment, _ := ca.IssueEnrollment(id, time.Now().UTC())
		identity, e := ca.Enroll(&enrollment, enrollment.Token, time.Now().UTC())
		if e != nil {
			t.Fatal(e)
		}
		identities = append(identities, identity)
	}
	oldURL := ports.URL()
	if err = transport.CheckNodeEndpoint(ctx, oldURL, identities[0], certPEM); err != nil {
		t.Fatal("open node endpoint rejected:", err)
	}
	statusAfterProbe, _ := ports.Status(ctx)
	if statusAfterProbe.Pending != len(ids) {
		t.Fatal("installation probe claimed a live controller session")
	}
	wrongTrust, _, _ := disposableServerCertificate()
	if err = transport.CheckNodeEndpoint(ctx, oldURL, identities[0], wrongTrust); err == nil {
		t.Fatal("installation probe accepted an untrusted TLS listener")
	}
	if err = transport.CheckNodeEndpoint(ctx, "wss://127.0.0.1:"+freeNodePort(t)+"/node/v1", identities[0], certPEM); err == nil {
		t.Fatal("installation probe accepted an unavailable port")
	}
	paths := []string{filepath.Join(t.TempDir(), "online.json"), filepath.Join(t.TempDir(), "offline.json")}
	done := make(chan error, 2)
	batches := []chan contracts.SampleBatch{make(chan contracts.SampleBatch, 2), make(chan contracts.SampleBatch, 2)}
	sample := func(i int, sequence uint64) contracts.SampleBatch {
		return contracts.SampleBatch{Samples: []contracts.NodeMetricSample{{ServerID: ids[i], CollectorEpoch: "epoch-port-migration-01", Sequence: sequence, ObservedAt: time.Now().UTC(), Values: map[string]float64{"cpu.utilization": 0.1}}}}
	}
	batches[0] <- sample(0, 0)
	batches[1] <- sample(1, 0)
	start := func(i int) {
		spool, e := transport.OpenSpool(filepath.Join(t.TempDir(), "spool"), transport.MaxSpoolBytes)
		if e != nil {
			t.Fatal(e)
		}
		agent := &transport.AgentClient{URL: oldURL, Identity: identities[i], IdentityPath: paths[i], TrustPEM: certPEM, Spool: spool, Backoff: transport.ReconnectBackoff{Base: 10 * time.Millisecond, Max: 100 * time.Millisecond}, Hello: contracts.Hello{Version: "test", Architecture: "amd64", Platform: "linux", ProtocolMin: contracts.ProtocolVersion, ProtocolMax: contracts.ProtocolVersion, Capabilities: []string{"metrics", transport.MigrationCapability}}}
		go func() { done <- agent.Run(ctx, batches[i]) }()
	}
	// Restart during a migration whose candidate is unreachable. The agent
	// must authenticate on the retained endpoint and keep collecting.
	failedCandidate := &transport.AgentClient{URL: oldURL, IdentityPath: paths[0]}
	if err = failedCandidate.LoadEndpoint(); err != nil {
		t.Fatal(err)
	}
	if err = failedCandidate.SaveEndpoint("wss://127.0.0.1:"+freeNodePort(t)+"/node/v1", oldURL); err != nil {
		t.Fatal(err)
	}
	start(0)
	wait := func(condition func() bool) {
		t.Helper()
		for !condition() {
			if ctx.Err() != nil {
				t.Fatal("timed out waiting for migration")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	wait(func() bool { status, _ := ports.Status(ctx); return status.Pending == 1 })
	next := freeNodePort(t)
	n, _ := strconv.Atoi(next)
	if err = ports.Change(n); err != nil {
		t.Fatal(err)
	}
	batches[0] <- sample(0, 1)
	wait(func() bool {
		status, _ := ports.Status(ctx)
		return status.Pending == 1 && (status.Nodes[0].State == "pending" || status.Nodes[1].State == "pending")
	})
	// The returning node still uses the original listener and receives migration.
	if err = ports.Retire(ctx); err == nil {
		t.Fatal("offline node's old port closed")
	}
	start(1)
	wait(func() bool { status, _ := ports.Status(ctx); return status.Pending == 0 })
	for _, path := range paths {
		wait(func() bool {
			data, e := os.ReadFile(path + ".transport.json")
			var saved transport.AgentEndpoint
			return e == nil && json.Unmarshal(data, &saved) == nil && saved.Current == ports.URL() && saved.Previous == ""
		})
	}
	for i, id := range ids {
		want := 1
		if i == 0 {
			want = 2
		}
		wait(func() bool {
			samples, _, queryErr := store.QueryMetricSamplesRange(ctx, id, time.Now().Add(-time.Hour), time.Now().Add(time.Minute), 10)
			return queryErr == nil && len(samples) == want
		})
	}
	if err = ports.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	conn, e := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", original), 100*time.Millisecond)
	if e == nil {
		_ = conn.Close()
		t.Fatal("retired port still listening")
	}
	// An installed agent retains its original configured URL after migration.
	// Its diagnostic must resolve the saved endpoint just as Run does, while
	// an explicit new installation/configuration must still take precedence.
	if err = transport.CheckNodeEndpoint(ctx, oldURL, identities[0], certPEM); err == nil {
		t.Fatal("retired configured endpoint unexpectedly passed the direct probe")
	}
	checker := &transport.AgentClient{URL: oldURL, IdentityPath: paths[0], Identity: identities[0], TrustPEM: certPEM}
	savedBefore, err := os.ReadFile(paths[0] + ".transport.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = checker.CheckEndpoint(ctx); err != nil {
		t.Fatal("transport diagnostic ignored the saved migration:", err)
	}
	if checker.URL != ports.URL() {
		t.Fatal("transport diagnostic checked the retired configured endpoint")
	}
	savedAfter, err := os.ReadFile(paths[0] + ".transport.json")
	if err != nil || string(savedBefore) != string(savedAfter) {
		t.Fatal("transport diagnostic changed persisted migration state")
	}
	stalePath := filepath.Join(t.TempDir(), "stale-identity.json")
	stale := &transport.AgentClient{URL: oldURL, IdentityPath: stalePath}
	if err = stale.LoadEndpoint(); err != nil {
		t.Fatal(err)
	}
	if err = stale.SaveEndpoint("wss://127.0.0.1:"+original+"/node/v1", ""); err != nil {
		t.Fatal(err)
	}
	explicit := &transport.AgentClient{URL: ports.URL(), IdentityPath: stalePath, Identity: identities[0], TrustPEM: certPEM}
	if err = explicit.CheckEndpoint(ctx); err != nil || explicit.URL != ports.URL() {
		t.Fatalf("explicit endpoint overridden: URL=%q err=%v", explicit.URL, err)
	}
	cancel()
	for range ids {
		if err = <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
}

func TestNodeStatusPaginationKeepsWholeFleetRetirementGuard(t *testing.T) {
	ctx := context.Background()
	store := migrationStore(t)
	original := freeNodePort(t)
	p, err := NewNodePorts(filepath.Join(t.TempDir(), "ports.json"), net.JoinHostPort("127.0.0.1", original), store, func(port string) string { return "wss://hub.example.test:" + port + "/node/v1" }, func(net.Listener) {})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	next := freeNodePort(t)
	n, _ := strconv.Atoi(next)
	if err := p.Change(n); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 225; i++ {
		id := contracts.ServerID(fmt.Sprintf("node-paging-%05d", i))
		addMigrationNode(t, store, id, true)
		if i < 224 {
			p.state.Nodes[id] = endpointObservation{Port: next}
		}
	}
	first, err := p.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Nodes) != 200 || first.NextCursor == "" || first.Total != 225 || first.Migrated != 224 || first.Pending != 1 {
		t.Fatalf("first page=%+v", first)
	}
	for _, node := range first.Nodes {
		if node.State != "migrated" {
			t.Fatal("pending node unexpectedly in first page")
		}
	}
	if err := p.Retire(ctx); err == nil {
		t.Fatal("retired endpoint needed by a pending node outside first page")
	}
	second, err := p.StatusPage(ctx, 200, first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Nodes) != 25 || second.NextCursor != "" || second.Total != 225 || second.Pending != 1 || second.Nodes[24].State != "pending" {
		t.Fatalf("second page=%+v", second)
	}
	seen := map[contracts.ServerID]bool{}
	cursor := ""
	for {
		page, err := p.StatusPage(ctx, 17, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Nodes) > 17 || page.Pending != 1 || page.Migrated != 224 {
			t.Fatalf("page=%+v", page)
		}
		for _, node := range page.Nodes {
			if seen[node.ID] {
				t.Fatalf("duplicate node %s", node.ID)
			}
			seen[node.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 225 {
		t.Fatalf("pagination covered %d nodes", len(seen))
	}
	for _, query := range []string{"limit=0", "limit=201", "limit=bad", "cursor=bad!", "cursor=" + strings.Repeat("a", 257), "cursor=bm9kZS11bmtub3du"} {
		w := httptest.NewRecorder()
		p.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/?"+query, nil))
		if w.Code != http.StatusBadRequest || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
			t.Fatalf("query=%q status=%d media=%q", query, w.Code, w.Header().Get("Content-Type"))
		}
	}
	w := httptest.NewRecorder()
	p.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/?limit=17", nil))
	var httpPage NodePortStatus
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &httpPage) != nil || len(httpPage.Nodes) != 17 || httpPage.Pending != 1 || httpPage.Total != 225 || w.Body.Len() > 1048576 {
		t.Fatalf("HTTP status=%d body=%s", w.Code, w.Body.String())
	}
	if err := p.Observe("node-paging-00224", next); err != nil {
		t.Fatal(err)
	}
	if err := p.Retire(ctx); err != nil {
		t.Fatalf("fully migrated fleet could not retire: %v", err)
	}
	final, err := p.Status(ctx)
	if err != nil || final.Pending != 0 || final.Migrated != 225 || len(final.PreviousPorts) != 0 {
		t.Fatalf("final=%+v err=%v", final, err)
	}
}

func TestNodeStatusResponseSizeIsEnforced(t *testing.T) {
	store := migrationStore(t)
	p, err := NewNodePorts(filepath.Join(t.TempDir(), "ports.json"), net.JoinHostPort("127.0.0.1", freeNodePort(t)), store, func(string) string { return "wss://hub.example.test/" + strings.Repeat("a", 1048576) }, func(net.Listener) {})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	w := httptest.NewRecorder()
	p.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 500 || w.Body.Len() > 1048576 || !strings.Contains(w.Body.String(), "exceeds response limit") {
		t.Fatalf("status=%d bytes=%d", w.Code, w.Body.Len())
	}
}
