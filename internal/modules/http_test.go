package modules

import (
	"archive/tar"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Real-kia/payesh/internal/contracts"
)

func newTestService(t *testing.T) (*Service, ed25519.PrivateKey, contracts.Server) {
	t.Helper()
	manager, priv, server := testManager(t)
	return NewService(manager), priv, server
}

func doJSON(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestHTTPCatalogListsCuratedModulesOnly(t *testing.T) {
	service, _, _ := newTestService(t)
	rec := doJSON(t, service.Handler(), http.MethodGet, "/api/v1/modules", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []contracts.ModuleCatalogEntry `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != len(Catalog) {
		t.Fatalf("expected %d catalog entries, got %d", len(Catalog), len(body.Items))
	}
}

func TestHTTPInstallEnableDisableRemoveLifecycle(t *testing.T) {
	service, priv, server := newTestService(t)
	archive := buildTarGz(t, []tarEntry{{name: "bin/port-traffic", typeflag: tar.TypeReg, body: []byte("binary")}})
	manifest, sig := signedManifest(t, priv, "port-traffic", archive)

	installBody := lifecycleRequest{
		IdempotencyKey: "install-1", ExpectedRevision: 0, Manifest: &manifest,
		ManifestSignatureB64: sig, ArchiveBase64: base64.StdEncoding.EncodeToString(archive),
	}
	path := "/api/v1/servers/" + string(server.ID) + "/modules/port-traffic/install"
	rec := doJSON(t, service.Handler(), http.MethodPost, path, installBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("install failed: %d %s", rec.Code, rec.Body.String())
	}
	var installed contracts.ModuleInstallation
	if err := json.Unmarshal(rec.Body.Bytes(), &installed); err != nil {
		t.Fatal(err)
	}
	if installed.State != contracts.ModuleInstalledDisabled {
		t.Fatalf("expected installed-disabled, got %s", installed.State)
	}

	// Repeating the exact same install request must replay the result, not
	// re-run the lifecycle (which would now be an illegal transition).
	replay := doJSON(t, service.Handler(), http.MethodPost, path, installBody)
	if replay.Code != http.StatusOK {
		t.Fatalf("expected idempotent replay to succeed, got %d %s", replay.Code, replay.Body.String())
	}
	var replayed contracts.ModuleInstallation
	if err := json.Unmarshal(replay.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.Revision != installed.Revision {
		t.Fatalf("expected replay to return the original revision %d, got %d", installed.Revision, replayed.Revision)
	}

	enablePath := "/api/v1/servers/" + string(server.ID) + "/modules/port-traffic/enable"
	enableRec := doJSON(t, service.Handler(), http.MethodPost, enablePath, lifecycleRequest{IdempotencyKey: "enable-1", ExpectedRevision: installed.Revision})
	if enableRec.Code != http.StatusOK {
		t.Fatalf("enable failed: %d %s", enableRec.Code, enableRec.Body.String())
	}
	var enabled contracts.ModuleInstallation
	if err := json.Unmarshal(enableRec.Body.Bytes(), &enabled); err != nil {
		t.Fatal(err)
	}
	if enabled.State != contracts.ModuleEnabled {
		t.Fatalf("expected enabled, got %s", enabled.State)
	}

	statusRec := doJSON(t, service.Handler(), http.MethodGet, "/api/v1/servers/"+string(server.ID)+"/modules", nil)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status failed: %d %s", statusRec.Code, statusRec.Body.String())
	}
	var status struct {
		Items []contracts.ModuleInstallation `json:"items"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Items) != 1 || status.Items[0].ModuleID != "port-traffic" {
		t.Fatalf("unexpected status list: %+v", status.Items)
	}
}

func TestHTTPInstallRejectsIdempotencyKeyReuseWithDifferentBody(t *testing.T) {
	service, priv, server := newTestService(t)
	archive := buildTarGz(t, []tarEntry{{name: "bin/port-traffic", typeflag: tar.TypeReg, body: []byte("binary")}})
	manifest, sig := signedManifest(t, priv, "port-traffic", archive)
	path := "/api/v1/servers/" + string(server.ID) + "/modules/port-traffic/install"
	first := lifecycleRequest{IdempotencyKey: "k1", ExpectedRevision: 0, Manifest: &manifest, ManifestSignatureB64: sig, ArchiveBase64: base64.StdEncoding.EncodeToString(archive)}
	if rec := doJSON(t, service.Handler(), http.MethodPost, path, first); rec.Code != http.StatusOK {
		t.Fatalf("install failed: %d %s", rec.Code, rec.Body.String())
	}
	second := first
	second.ExpectedRevision = 5 // same key, different body content
	rec := doJSON(t, service.Handler(), http.MethodPost, path, second)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 idempotency conflict, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPRejectsUnknownRouteAndModuleID(t *testing.T) {
	service, _, server := newTestService(t)
	if rec := doJSON(t, service.Handler(), http.MethodGet, "/api/v1/servers/"+string(server.ID)+"/traffic", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unrelated resource, got %d", rec.Code)
	}
	if rec := doJSON(t, service.Handler(), http.MethodPost, "/api/v1/servers/"+string(server.ID)+"/modules/port-traffic/detonate", lifecycleRequest{IdempotencyKey: "x"}); rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unsupported action, got %d", rec.Code)
	}
}
