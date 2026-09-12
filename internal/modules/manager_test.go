package modules

import (
	"archive/tar"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/trust"
)

func testManager(t *testing.T) (*Manager, ed25519.PrivateKey, contracts.Server) {
	t.Helper()
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	server := contracts.Server{ID: "server-modules-test01", Name: "Test", Role: "standalone", Architecture: "amd64", Platform: "linux", Capabilities: []string{"cgroup-v2", "tc", "nftables-counters", "traffic-accounting"}, Version: "0.1.0", ConnectionState: "connected", FreshnessState: "fresh"}
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := trust.NewRegistry(trust.Anchor{KeyID: "test-2026", PublicKey: pub, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return &Manager{Store: store, Trust: registry, RootDir: t.TempDir(), LocalServerID: server.ID, HealthCheck: func(context.Context, string, string) error { return nil }, Executor: moduleExecutorFunc(func(context.Context, ModuleInvocation) error { return nil })}, priv, server
}

type moduleExecutorFunc func(context.Context, ModuleInvocation) error

func (f moduleExecutorFunc) Invoke(ctx context.Context, invocation ModuleInvocation) error {
	return f(ctx, invocation)
}

func signedManifest(t *testing.T, priv ed25519.PrivateKey, moduleID string, archive []byte) (contracts.ModuleManifest, string) {
	t.Helper()
	sum := sha256.Sum256(archive)
	entry, curated := Lookup(moduleID)
	version := "0.1.0"
	var dependencies, privileges []string
	if curated {
		version = entry.LatestVersion
		dependencies = append([]string(nil), entry.Dependencies...)
		privileges = append([]string(nil), entry.RequiredPrivileges...)
	}
	manifest := contracts.ModuleManifest{
		Format: contracts.ModuleManifestFormat, ModuleID: moduleID, ModuleVersion: version,
		MinCore: "0.1.0", ProtocolMin: "v1", ProtocolMax: "v1", OS: "linux", Architecture: "amd64",
		Dependencies: dependencies, RequiredPrivileges: privileges,
		CompressedBytes: uint64(len(archive)), UnpackedBytes: 64, SHA256: hex.EncodeToString(sum[:]),
		SigningKeyID: "test-2026", CreatedAt: time.Now().UTC(),
	}
	_, sig, err := trust.Sign(priv, manifest)
	if err != nil {
		t.Fatal(err)
	}
	return manifest, sig
}

func resignManifest(t *testing.T, priv ed25519.PrivateKey, manifest contracts.ModuleManifest) string {
	t.Helper()
	_, signature, err := trust.Sign(priv, manifest)
	if err != nil {
		t.Fatal(err)
	}
	return signature
}

func TestManagerFullLifecycle(t *testing.T) {
	manager, priv, server := testManager(t)
	archive := buildTarGz(t, []tarEntry{{name: "bin/port-traffic", typeflag: tar.TypeReg, body: []byte("binary")}})
	manifest, sig := signedManifest(t, priv, "port-traffic", archive)

	installed, err := manager.Install(context.Background(), InstallRequest{ServerID: server.ID, ModuleID: "port-traffic", Manifest: manifest, ManifestSignatureB64: sig, Archive: archive, ExpectedRevision: 0})
	if err != nil {
		t.Fatal(err)
	}
	if installed.State != contracts.ModuleInstalledDisabled {
		t.Fatalf("expected installed-disabled after install, got %s", installed.State)
	}

	enabled, err := manager.Enable(context.Background(), server.ID, "port-traffic", installed.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if enabled.State != contracts.ModuleEnabled {
		t.Fatalf("expected enabled, got %s", enabled.State)
	}

	if _, err := manager.Remove(context.Background(), server.ID, "port-traffic", enabled.Revision); err == nil {
		t.Fatal("expected remove to be rejected while still enabled")
	}

	disabled, err := manager.Disable(context.Background(), server.ID, "port-traffic", enabled.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.State != contracts.ModuleInstalledDisabled {
		t.Fatalf("expected installed-disabled, got %s", disabled.State)
	}

	removed, err := manager.Remove(context.Background(), server.ID, "port-traffic", disabled.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if removed.State != contracts.ModuleUnavailable {
		t.Fatalf("expected unavailable after remove, got %s", removed.State)
	}
}

func TestManagerInstallRejectsUncuratedModule(t *testing.T) {
	manager, priv, server := testManager(t)
	archive := buildTarGz(t, []tarEntry{{name: "f", typeflag: tar.TypeReg}})
	manifest, sig := signedManifest(t, priv, "arbitrary-module", archive)
	if _, err := manager.Install(context.Background(), InstallRequest{ServerID: server.ID, ModuleID: "arbitrary-module", Manifest: manifest, ManifestSignatureB64: sig, Archive: archive, ExpectedRevision: 0}); err == nil {
		t.Fatal("expected uncurated module id to be rejected")
	}
}

func TestManagerInstallRejectsTamperedSignatureAndLeavesFailedState(t *testing.T) {
	manager, priv, server := testManager(t)
	archive := buildTarGz(t, []tarEntry{{name: "bin/port-traffic", typeflag: tar.TypeReg, body: []byte("binary")}})
	manifest, sig := signedManifest(t, priv, "port-traffic", archive)
	manifest.ModuleVersion = "9.9.9" // mutate after signing: signature no longer matches

	if _, err := manager.Install(context.Background(), InstallRequest{ServerID: server.ID, ModuleID: "port-traffic", Manifest: manifest, ManifestSignatureB64: sig, Archive: archive, ExpectedRevision: 0}); err == nil {
		t.Fatal("expected tampered manifest to fail verification")
	}
	installation, err := manager.Store.GetModuleInstallation(context.Background(), server.ID, "port-traffic")
	if err != nil {
		t.Fatal(err)
	}
	if installation.State != contracts.ModuleFailed {
		t.Fatalf("expected failed state to be durably recorded, got %s", installation.State)
	}
	if installation.Error == nil || installation.Error.Code != "manifest_verification_failed" {
		t.Fatalf("expected a recorded verification error, got %+v", installation.Error)
	}
}

func TestManagerInstallRejectsIneligibleServer(t *testing.T) {
	manager, priv, server := testManager(t)
	server.Architecture = "arm64"
	if err := manager.Store.UpsertServer(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	archive := buildTarGz(t, []tarEntry{{name: "bin/port-traffic", typeflag: tar.TypeReg, body: []byte("binary")}})
	manifest, sig := signedManifest(t, priv, "port-traffic", archive)
	if _, err := manager.Install(context.Background(), InstallRequest{ServerID: server.ID, ModuleID: "port-traffic", Manifest: manifest, ManifestSignatureB64: sig, Archive: archive, ExpectedRevision: 0}); err == nil {
		t.Fatal("expected architecture-ineligible server to be rejected before staging")
	}
}

func TestManagerRevisionConflictRejectsStaleRequest(t *testing.T) {
	manager, priv, server := testManager(t)
	archive := buildTarGz(t, []tarEntry{{name: "bin/port-traffic", typeflag: tar.TypeReg, body: []byte("binary")}})
	manifest, sig := signedManifest(t, priv, "port-traffic", archive)
	if _, err := manager.Install(context.Background(), InstallRequest{ServerID: server.ID, ModuleID: "port-traffic", Manifest: manifest, ManifestSignatureB64: sig, Archive: archive, ExpectedRevision: 0}); err != nil {
		t.Fatal(err)
	}
	// Reusing revision 0 again (as if a second browser tab raced the first
	// install) must be rejected, not silently restart the lifecycle.
	if _, err := manager.Install(context.Background(), InstallRequest{ServerID: server.ID, ModuleID: "port-traffic", Manifest: manifest, ManifestSignatureB64: sig, Archive: archive, ExpectedRevision: 0}); err != monitoring.ErrModuleRevisionConflict {
		t.Fatalf("expected ErrModuleRevisionConflict, got %v", err)
	}
}

func TestManagerInstallFailedHealthCheckRollsBackAndFails(t *testing.T) {
	manager, priv, server := testManager(t)
	manager.HealthCheck = func(context.Context, string, string) error { return errFailingHealthCheck }
	archive := buildTarGz(t, []tarEntry{{name: "bin/port-traffic", typeflag: tar.TypeReg, body: []byte("binary")}})
	manifest, sig := signedManifest(t, priv, "port-traffic", archive)
	if _, err := manager.Install(context.Background(), InstallRequest{ServerID: server.ID, ModuleID: "port-traffic", Manifest: manifest, ManifestSignatureB64: sig, Archive: archive, ExpectedRevision: 0}); err == nil {
		t.Fatal("expected failed health check to fail the install")
	}
	installation, err := manager.Store.GetModuleInstallation(context.Background(), server.ID, "port-traffic")
	if err != nil {
		t.Fatal(err)
	}
	if installation.State != contracts.ModuleFailed {
		t.Fatalf("expected failed state, got %s", installation.State)
	}
}

var errFailingHealthCheck = &staticError{"health check refused to start"}

type staticError struct{ msg string }

func (e *staticError) Error() string { return e.msg }

func TestManagerDisableRunsDeactivateHookAndBlocksOnFailure(t *testing.T) {
	manager, priv, server := testManager(t)
	archive := buildTarGz(t, []tarEntry{{name: "bin/cpu-controls", typeflag: tar.TypeReg, body: []byte("binary")}})
	manifest, sig := signedManifest(t, priv, "cpu-controls", archive)
	installed, err := manager.Install(context.Background(), InstallRequest{ServerID: server.ID, ModuleID: "cpu-controls", Manifest: manifest, ManifestSignatureB64: sig, Archive: archive, ExpectedRevision: 0})
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := manager.Enable(context.Background(), server.ID, "cpu-controls", installed.Revision)
	if err != nil {
		t.Fatal(err)
	}

	hookCalled := false
	manager.DeactivateHooks = map[string]func(context.Context, contracts.ServerID) error{
		"cpu-controls": func(context.Context, contracts.ServerID) error {
			hookCalled = true
			return errFailingHealthCheck
		},
	}
	if _, err := manager.Disable(context.Background(), server.ID, "cpu-controls", enabled.Revision-1); err != monitoring.ErrModuleRevisionConflict {
		t.Fatalf("expected stale disable conflict, got %v", err)
	}
	if hookCalled {
		t.Fatal("stale disable must not run destructive cleanup")
	}
	if _, err := manager.Disable(context.Background(), server.ID, "cpu-controls", enabled.Revision); err == nil {
		t.Fatal("expected a failing deactivate hook to block disable")
	}
	if !hookCalled {
		t.Fatal("expected the deactivate hook to run")
	}
	still, err := manager.Store.GetModuleInstallation(context.Background(), server.ID, "cpu-controls")
	if err != nil {
		t.Fatal(err)
	}
	if still.State != contracts.ModuleEnabled {
		t.Fatalf("expected the module to remain enabled after a failed cleanup, got %s", still.State)
	}

	manager.DeactivateHooks["cpu-controls"] = func(context.Context, contracts.ServerID) error { return nil }
	disabled, err := manager.Disable(context.Background(), server.ID, "cpu-controls", enabled.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.State != contracts.ModuleInstalledDisabled {
		t.Fatalf("expected installed-disabled once cleanup succeeds, got %s", disabled.State)
	}
}

func TestManagerRequiresConfiguredHealthCheck(t *testing.T) {
	manager, priv, server := testManager(t)
	manager.HealthCheck = nil
	manager.Executor = nil
	archive := buildTarGz(t, []tarEntry{{name: "bin/port-traffic", typeflag: tar.TypeReg, body: []byte("binary")}})
	manifest, signature := signedManifest(t, priv, "port-traffic", archive)
	if _, err := manager.Install(context.Background(), InstallRequest{ServerID: server.ID, ModuleID: "port-traffic", Manifest: manifest, ManifestSignatureB64: signature, Archive: archive}); err == nil {
		t.Fatal("expected install to fail closed without a health check")
	}
	if _, err := os.Stat(manager.installDir(server.ID, "port-traffic")); !os.IsNotExist(err) {
		t.Fatal("module files must not activate without a health check")
	}
}

func TestManagerRejectsStaleSignedManifest(t *testing.T) {
	manager, priv, server := testManager(t)
	archive := buildTarGz(t, []tarEntry{{name: "bin/port-traffic", typeflag: tar.TypeReg, body: []byte("binary")}})
	manifest, _ := signedManifest(t, priv, "port-traffic", archive)
	manifest.CreatedAt = time.Now().Add(-maxManifestAge - time.Hour)
	signature := resignManifest(t, priv, manifest)
	if _, err := manager.Install(context.Background(), InstallRequest{ServerID: server.ID, ModuleID: "port-traffic", Manifest: manifest, ManifestSignatureB64: signature, Archive: archive}); err == nil {
		t.Fatal("expected stale signed metadata to be rejected")
	}
}

func TestManagerRejectsManifestOmittingCatalogPrivilege(t *testing.T) {
	manager, priv, server := testManager(t)
	archive := buildTarGz(t, []tarEntry{{name: "bin/cpu-controls", typeflag: tar.TypeReg, body: []byte("binary")}})
	manifest, _ := signedManifest(t, priv, "cpu-controls", archive)
	manifest.RequiredPrivileges = nil
	signature := resignManifest(t, priv, manifest)
	if _, err := manager.Install(context.Background(), InstallRequest{ServerID: server.ID, ModuleID: "cpu-controls", Manifest: manifest, ManifestSignatureB64: signature, Archive: archive}); err == nil {
		t.Fatal("expected omitted catalog privilege to be rejected")
	}
}

func TestManagerRejectsRemoteNodeWithoutExecutor(t *testing.T) {
	manager, priv, server := testManager(t)
	server.Role = "node"
	if err := manager.Store.UpsertServer(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	archive := buildTarGz(t, []tarEntry{{name: "bin/port-traffic", typeflag: tar.TypeReg, body: []byte("binary")}})
	manifest, signature := signedManifest(t, priv, "port-traffic", archive)
	if _, err := manager.Install(context.Background(), InstallRequest{ServerID: server.ID, ModuleID: "port-traffic", Manifest: manifest, ManifestSignatureB64: signature, Archive: archive}); err == nil {
		t.Fatal("expected remote install to fail rather than modify hub-local files")
	}
}
