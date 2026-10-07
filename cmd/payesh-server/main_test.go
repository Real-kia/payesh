package main

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/alerts"
	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func TestInstalledArtifactPathsAndReadiness(t *testing.T) {
	dir := t.TempDir()
	paths := installedArtifactPaths(dir)
	for _, name := range []string{"payesh-install", "payesh", "payesh-agent", "payesh-privd"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if !installArtifactsReady(paths) {
		t.Fatal("complete node artifact set was not detected")
	}
	if err := os.Remove(filepath.Join(dir, "payesh-privd")); err != nil {
		t.Fatal(err)
	}
	if installArtifactsReady(paths) {
		t.Fatal("incomplete node artifact set was accepted")
	}
}

func TestLoopbackListenAddress(t *testing.T) {
	for _, address := range []string{"127.0.0.1:8787", "[::1]:8787", "localhost:8787"} {
		if !loopbackListenAddress(address) {
			t.Fatalf("loopback address rejected: %s", address)
		}
	}
	for _, address := range []string{"0.0.0.0:8787", ":8787", "192.0.2.10:8787", "[::]:8787"} {
		if loopbackListenAddress(address) {
			t.Fatalf("non-loopback address accepted: %s", address)
		}
	}
}

func TestEnvironmentBool(t *testing.T) {
	t.Setenv("PAYESH_TEST_BOOL", "true")
	if value, err := environmentBool("PAYESH_TEST_BOOL", false); err != nil || !value {
		t.Fatalf("environment bool value=%t err=%v", value, err)
	}
	t.Setenv("PAYESH_TEST_BOOL", "not-a-bool")
	if _, err := environmentBool("PAYESH_TEST_BOOL", false); err == nil {
		t.Fatal("invalid environment boolean was accepted")
	}
	t.Setenv("PAYESH_TEST_BOOL", "")
	if value, err := environmentBool("PAYESH_TEST_BOOL", true); err != nil || !value {
		t.Fatalf("environment bool fallback=%t err=%v", value, err)
	}
}

func TestNodeTransportConfigSupportsAutomaticTLSOrCompleteCustomTLS(t *testing.T) {
	if config, err := newNodeTransportConfig("", "", ""); err != nil || config.Enabled() {
		t.Fatalf("empty node transport should be disabled: config=%+v err=%v", config, err)
	}
	for _, values := range [][3]string{
		{"127.0.0.1:9797", "", "/tmp/key"},
		{"127.0.0.1:9797", "/tmp/cert", ""},
		{"not-an-address", "/tmp/cert", "/tmp/key"},
		{"", "/tmp/cert", "/tmp/key"},
	} {
		if _, err := newNodeTransportConfig(values[0], values[1], values[2]); err == nil {
			t.Fatalf("accepted unsafe node transport config: %q", values)
		}
	}
	if config, err := newNodeTransportConfig("127.0.0.1:9797", "", ""); err != nil || !config.Enabled() {
		t.Fatalf("automatic TLS listener rejected: config=%+v err=%v", config, err)
	}
	config, err := newNodeTransportConfig("127.0.0.1:9797", "/tmp/cert", "/tmp/key")
	if err != nil || !config.Enabled() || config.CertFile != "/tmp/cert" || config.KeyFile != "/tmp/key" {
		t.Fatalf("valid node transport config rejected: config=%+v err=%v", config, err)
	}
}

func TestNodeTransportTLSAllowsBootstrapHandshakeButRequiresRouteAuth(t *testing.T) {
	config := nodeTransportTLSConfig(tls.Certificate{})
	if config.MinVersion != tls.VersionTLS13 {
		t.Fatalf("node transport allowed pre-TLS 1.3 handshake: %v", config.MinVersion)
	}
	if config.ClientAuth != tls.RequestClientCert {
		t.Fatalf("node transport must permit bootstrap handshake with route-level auth, got %v", config.ClientAuth)
	}
}

func TestUnavailableCPUServiceFailsClosed(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/servers/server-local-0001/cpu-policies/service/svc/apply", nil)
	unavailableCPUService{}.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", recorder.Code)
	}
}

func TestEvaluateUnreachableCreatesLivenessAlert(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-liveness-012345")
	at := time.Now().UTC().Add(time.Second)
	lastHeartbeat := at.Add(-2 * time.Minute)
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Liveness", Role: "node", Architecture: "amd64", Platform: "linux", LastHeartbeat: &lastHeartbeat, ConnectionState: "connected", FreshnessState: "fresh"}); err != nil {
		t.Fatal(err)
	}
	engine, err := alerts.NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RefreshServerStates(ctx, at); err != nil {
		t.Fatal(err)
	}
	if err := evaluateUnreachable(ctx, store, engine, at); err != nil {
		t.Fatal(err)
	}
	if err := evaluateUnreachable(ctx, store, engine, at.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	page, err := store.ListAlertStates(ctx, serverID, 200, "")
	if err != nil {
		t.Fatal(err)
	}
	firing := false
	for _, state := range page.Items {
		if state.State == "firing" {
			firing = true
			break
		}
	}
	if !firing {
		t.Fatalf("liveness evaluation did not fire an alert: %+v", page.Items)
	}
}
