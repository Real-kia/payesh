package transport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestEndpointDurabilitySyncOrderingAndFailure(t *testing.T) {
	for _, fail := range []string{"", "file", "directory"} {
		t.Run(fail, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "identity.transport.json")
			a := AgentClient{IdentityPath: filepath.Join(dir, "identity"), configuredURL: "wss://hub.example.test/node/v1"}
			events := []string{}
			injected := errors.New("sync refused")
			a.endpointSync = func(file *os.File) error {
				info, err := file.Stat()
				if err != nil {
					return err
				}
				stage := "file"
				if info.IsDir() {
					stage = "directory"
				}
				events = append(events, stage)
				_, readErr := os.ReadFile(path)
				if stage == "file" && !errors.Is(readErr, os.ErrNotExist) {
					t.Error("endpoint published before file sync")
				}
				if stage == "directory" && readErr != nil && len(events) == 2 {
					t.Error("parent sync preceded rename")
				}
				if fail == stage {
					return injected
				}
				return file.Sync()
			}
			err := a.saveEndpoint("wss://hub.example.test:9797/node/v1", a.configuredURL)
			if fail != "" && !errors.Is(err, injected) {
				t.Fatalf("sync failure not refused: %v", err)
			}
			if fail == "" && err != nil {
				t.Fatal(err)
			}
			want := []string{"file", "directory"}
			if fail == "directory" {
				want = append(want, "directory")
			}
			if fail == "file" {
				want = want[:1]
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("sync order %v, want %v", events, want)
			}
			matches, _ := filepath.Glob(filepath.Join(dir, ".transport-*"))
			if len(matches) != 0 {
				t.Fatalf("temporary files remain: %v", matches)
			}
		})
	}
}

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

func TestAgentAcceptedEndpointRefusesFallbackRemovalOnSyncFailure(t *testing.T) {
	dir := t.TempDir()
	a := AgentClient{IdentityPath: filepath.Join(dir, "identity"), configuredURL: "wss://hub.example.test/node/v1", URL: "wss://hub.example.test:9797/node/v1", previousURL: "wss://hub.example.test/node/v1"}
	if err := a.saveEndpoint(a.URL, a.previousURL); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("directory sync refused")
	failed := false
	a.endpointSync = func(file *os.File) error {
		info, _ := file.Stat()
		if info.IsDir() && !failed {
			failed = true
			return injected
		}
		return file.Sync()
	}
	err := a.handleIncoming(context.Background(), nil, inboundEnvelope{envelope: contracts.Envelope{Message: "hello_accepted"}})
	if !errors.Is(err, injected) {
		t.Fatalf("fallback removal acknowledged on unsynced write: %v", err)
	}
	if a.previousURL == "" {
		t.Fatal("fallback cleared despite sync failure")
	}
	b := AgentClient{IdentityPath: a.IdentityPath, URL: a.configuredURL}
	if err := b.loadEndpoint(); err != nil || b.previousURL != a.previousURL {
		t.Fatalf("restored endpoint lost fallback: %q %v", b.previousURL, err)
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

func TestHubDoesNotAcknowledgeHelloBeforeDurableObservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	store := migrationStore(t)
	id := contracts.ServerID("node-sync-hello-001")
	addMigrationNode(t, store, id, true)
	now := time.Now().UTC()
	ca, err := NewCertificateAuthority(now)
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
	client := &webSocket{conn: left, br: bufio.NewReader(left), bw: bufio.NewWriter(left), mask: true}
	server := &webSocket{conn: right, br: bufio.NewReader(right), bw: bufio.NewWriter(right), readMask: true}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer right.Close()
		serveNodeWebSocket(ctx, server, hub, identity.CertificatePEM)
	}()
	hello := contracts.Hello{Version: "test", Architecture: "amd64", Platform: "linux", ProtocolMin: contracts.ProtocolVersion, ProtocolMax: contracts.ProtocolVersion, Capabilities: []string{"metrics", MigrationCapability}}
	if err := client.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "hello", SentAt: now, Body: mustJSON(hello)}); err != nil {
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
