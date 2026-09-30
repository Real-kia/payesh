package modules

import (
	"archive/tar"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestProcessModuleTargetAndStateGate(t *testing.T) {
	m, _, server := testManager(t)
	p := &ProcessRuntime{Store: m.Store, Root: m.RootDir, ServerID: server.ID, Context: context.Background()}
	for _, test := range []struct {
		method, path string
		status       int
	}{{"GET", "/api/v1/servers/" + string(server.ID) + "/processes", 409}, {"GET", "/api/v1/servers/other-server-012345/processes", 404}, {"POST", "/api/v1/servers/" + string(server.ID) + "/processes", 405}} {
		w := httptest.NewRecorder()
		p.Handler().ServeHTTP(w, httptest.NewRequest(test.method, test.path, nil))
		if w.Code != test.status {
			t.Fatalf("%s: %d %s", test.path, w.Code, w.Body.String())
		}
	}
	if e := p.Invoke(context.Background(), ModuleInvocation{ServerID: server.ID, ModuleID: ProcessModuleID, Operation: "health", InstallDir: "/tmp/untrusted"}); e == nil {
		t.Fatal("untrusted executable path accepted")
	}
}

// Production-path test on Linux: actual signed staging, separate executable,
// Unix socket, sampling, lifecycle stop, and restart restoration. No host
// services or files outside t.TempDir are changed.
func TestProcessModuleExecutableLifecycle(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires the Linux proc filesystem")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, key, server := testManager(t)
	m.LifecycleMu = nil
	// Keep Unix socket paths below sockaddr_un's length limit on every runner.
	root, e := os.MkdirTemp("", "pm-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(root)
	m.RootDir = root
	binary := filepath.Join(root, "process-binary")
	command := exec.Command("go", "build", "-o", binary, "../../cmd/payesh-process-module")
	if out, e := command.CombinedOutput(); e != nil {
		t.Fatalf("build process module: %v %s", e, out)
	}
	data, e := os.ReadFile(binary)
	if e != nil {
		t.Fatal(e)
	}
	archive := buildTarGz(t, []tarEntry{{name: "bin/process-monitoring", typeflag: tar.TypeReg, mode: 0755, body: data}})
	manifest, _ := signedManifest(t, key, ProcessModuleID, archive)
	manifest.UnpackedBytes = uint64(len(data))
	signature := resignManifest(t, key, manifest)
	p := &ProcessRuntime{Context: ctx, Store: m.Store, Root: root, ServerID: server.ID}
	m.Executor = p
	m.HealthCheck = nil
	installed, e := m.Install(ctx, InstallRequest{ServerID: server.ID, ModuleID: ProcessModuleID, Manifest: manifest, ManifestSignatureB64: signature, Archive: archive})
	if e != nil {
		t.Fatal(e)
	}
	enabled, e := m.Enable(ctx, server.ID, ProcessModuleID, installed.Revision)
	if e != nil {
		t.Fatal(e)
	}
	defer p.stop(context.Background())
	w := httptest.NewRecorder()
	p.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/servers/"+string(server.ID)+"/processes?limit=1", nil))
	if w.Code != 200 {
		t.Fatalf("process data: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Items []json.RawMessage `json:"items"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &body); e != nil || len(body.Items) == 0 {
		t.Fatalf("no real process sample: %v", e)
	}
	// Simulate restarting payesh-server while durable state is still enabled.
	stopCtx, stopCancel := context.WithTimeout(ctx, 5*time.Second)
	defer stopCancel()
	if e := p.stop(stopCtx); e != nil {
		t.Fatal(e)
	}
	if e := p.Restore(ctx); e != nil {
		t.Fatal(e)
	}
	disabled, e := m.Disable(ctx, server.ID, ProcessModuleID, enabled.Revision)
	if e != nil {
		t.Fatal(e)
	}
	w = httptest.NewRecorder()
	p.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/servers/"+string(server.ID)+"/processes", nil))
	if w.Code != 409 {
		t.Fatal("disabled module served data")
	}
	if _, e := m.Remove(ctx, server.ID, ProcessModuleID, disabled.Revision); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(root, string(server.ID), ProcessModuleID, "active")); !os.IsNotExist(e) {
		t.Fatal("removed executable retained")
	}
}

func TestProcessModuleHubLifecycle(t *testing.T) {
	m, key, server := testManager(t)
	server.Role = "hub"
	if e := m.Store.UpsertServer(context.Background(), server); e != nil {
		t.Fatal(e)
	}
	archive := buildTarGz(t, []tarEntry{{name: "bin/process-monitoring", typeflag: tar.TypeReg, body: []byte("fixture")}})
	manifest, sig := signedManifest(t, key, ProcessModuleID, archive)
	installed, e := m.Install(context.Background(), InstallRequest{ServerID: server.ID, ModuleID: ProcessModuleID, Manifest: manifest, ManifestSignatureB64: sig, Archive: archive})
	if e != nil {
		t.Fatal(e)
	}
	enabled, e := m.Enable(context.Background(), server.ID, ProcessModuleID, installed.Revision)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := m.Disable(context.Background(), server.ID, ProcessModuleID, enabled.Revision); e != nil {
		t.Fatal(e)
	}
	if _, e := m.Enable(context.Background(), server.ID, "port-traffic", 0); e == nil {
		t.Fatal("other modules unexpectedly permitted on hub")
	}
}
