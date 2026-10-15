package nodehub

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Real-kia/payesh/internal/transport"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestPortObservationAndRetirementRefuseSyncFailure(t *testing.T) {
	store := migrationStore(t)
	id := contracts.ServerID("node-durable-sync")
	addMigrationNode(t, store, id, true)
	p, err := NewNodePorts(filepath.Join(t.TempDir(), "ports.json"), "127.0.0.1:"+freeNodePort(t), store, func(port string) string { return "wss://hub.example.test:" + port + "/node/v1" }, func(_ net.Listener) {})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	old := p.currentLocked()
	if err := p.Change(parsePersistencePort(t, freeNodePort(t))); err != nil {
		t.Fatal(err)
	}
	current := p.currentLocked()
	injected := errors.New("sync refused")
	p.syncFile = func(file *os.File) error { return injected }
	if err := p.Observe(id, current); !errors.Is(err, injected) {
		t.Fatalf("observation acknowledged after sync failure: %v", err)
	}
	if p.state.Nodes[id].Port == current {
		t.Fatal("failed observation marked migrated")
	}
	p.syncFile = nil
	if err := p.Observe(id, current); err != nil {
		t.Fatal(err)
	}
	p.syncFile = func(file *os.File) error {
		info, _ := file.Stat()
		if info.IsDir() {
			return injected
		}
		return file.Sync()
	}
	if err := p.Retire(context.Background()); !errors.Is(err, injected) {
		t.Fatalf("retirement succeeded after parent sync failure: %v", err)
	}
	if p.listeners[old] == nil || len(p.state.Ports) != 2 {
		t.Fatal("failed retirement closed historical listener")
	}
	body, err := os.ReadFile(p.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved portState
	if err = json.Unmarshal(body, &saved); err != nil || len(saved.Ports) != 2 {
		t.Fatalf("prior disk listener history was not restored: %+v %v", saved, err)
	}
}

func parsePersistencePort(t *testing.T, value string) int {
	t.Helper()
	port, err := strconv.Atoi(value)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestHubDoesNotAcknowledgeHelloBeforeDurableObservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	store := migrationStore(t)
	id := contracts.ServerID("node-sync-hello-001")
	addMigrationNode(t, store, id, true)
	now := time.Now().UTC()
	ca, err := transport.NewCertificateAuthority(now)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := ca.IssueEnrollment(id, now)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := ca.Enroll(&enrollment, enrollment.Token, now)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := NewHub(ca, store)
	if err != nil {
		t.Fatal(err)
	}
	p := &NodePorts{path: filepath.Join(t.TempDir(), "ports.json"), state: portState{Ports: []string{"9797"}, Nodes: map[contracts.ServerID]endpointObservation{id: {Port: "8787"}}}}
	p.syncFile = func(file *os.File) error { return errors.New("observation sync refused") }
	hub.Ports = p
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	client := transport.NewWebSocket(left, false)
	server := transport.NewWebSocket(right, true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer right.Close()
		serveNodeWebSocket(ctx, server, hub, identity.CertificatePEM)
	}()
	hello := contracts.Hello{Version: "test", Architecture: "amd64", Platform: "linux", ProtocolMin: contracts.ProtocolVersion, ProtocolMax: contracts.ProtocolVersion, Capabilities: []string{"metrics", transport.MigrationCapability}}
	if err := client.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "hello", SentAt: now, Body: transport.MustJSON(hello)}); err != nil {
		t.Fatal(err)
	}
	if payload, _, err := client.ReadMessage(); err == nil {
		t.Fatalf("hub acknowledged unsynced observation: %s", payload)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("hub did not abort failed observation")
	}
}

func TestNewPortStateDirectoryRequiresAncestorSync(t *testing.T) {
	dir := t.TempDir()
	p := NodePorts{path: filepath.Join(dir, "new", "nested", "ports.json"), state: portState{Ports: []string{"9797"}}}
	injected := errors.New("ancestor sync refused")
	p.syncFile = func(file *os.File) error {
		if file.Name() == dir {
			return injected
		}
		return file.Sync()
	}
	if err := p.saveLocked(); !errors.Is(err, injected) {
		t.Fatalf("new state published without ancestor durability: %v", err)
	}
	if _, err := os.Lstat(p.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state written before ancestor sync: %v", err)
	}
}
