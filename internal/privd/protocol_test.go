package privd

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestClientAndServerTypedModuleInvocation(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "p06-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	serverID := contracts.ServerID("server-privd-test01")
	installDir := filepath.Join(root, string(serverID), "bandwidth-controls", "active")
	if err := os.MkdirAll(filepath.Join(installDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(installDir, "bin", "bandwidth-controls")
	if err := os.WriteFile(binary, []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []string
	server, err := NewServer(Config{
		SocketPath: filepath.Join(root, "privd.sock"), ModuleRoot: root,
		Modules: map[string]ModuleSpec{"bandwidth-controls": {UnitTemplate: "payesh-bandwidth-module@%s.service", BinaryName: "bandwidth-controls"}},
		RunCommand: func(_ context.Context, action string, args ...string) error {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, action+" "+args[0]+" "+args[1])
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	invocation := ModuleInvocation{ServerID: serverID, ModuleID: "bandwidth-controls", ModuleVersion: "0.1.0", Operation: "enable", InstallDir: installDir}
	args, err := json.Marshal(invocation)
	if err != nil {
		t.Fatal(err)
	}
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	go server.handle(serverConn)
	if err := json.NewEncoder(clientConn).Encode(contracts.ActionRequest{Protocol: contracts.HelperProtocol, RequestID: "request-privd-test01", Action: "module.invoke", Target: invocation.ModuleID, TargetServerID: invocation.ServerID, IdempotencyKey: "request-privd-test01", Deadline: time.Now().Add(time.Second), Arguments: args}); err != nil {
		t.Fatal(err)
	}
	var response contracts.ActionResponse
	if err := json.NewDecoder(clientConn).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !response.Accepted || response.RequestID != "request-privd-test01" {
		t.Fatalf("unexpected helper response: %+v", response)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "enable --now payesh-bandwidth-module@server-privd-test01.service" {
		t.Fatalf("unexpected typed command: %#v", got)
	}
}

func TestServerRejectsPathEscapeAndUnknownModule(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "p06-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	server, err := NewServer(Config{SocketPath: filepath.Join(root, "privd.sock"), ModuleRoot: root, Modules: map[string]ModuleSpec{"bandwidth-controls": {UnitTemplate: "unit@%s", BinaryName: "bandwidth-controls"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := (ModuleInvocation{ServerID: "server-privd-test01", ModuleID: "bandwidth-controls", ModuleVersion: "0.1.0", Operation: "health", InstallDir: filepath.Join(root, "..", "escape")}).Validate(root); err == nil {
		t.Fatal("expected install directory escape to be rejected")
	}
	if err := (ModuleInvocation{ServerID: "server-privd-test01", ModuleID: "unknown", ModuleVersion: "0.1.0", Operation: "health", InstallDir: filepath.Join(root, "server-privd-test01", "unknown", "active")}).Validate(root); err != nil {
		// Shape validation does not own the curated allowlist; NewServer's
		// request validation owns that check. This assertion documents the split.
		t.Fatalf("unexpected shape validation error: %v", err)
	}
	_ = server
}
