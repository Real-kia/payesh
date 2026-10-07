package modules

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestPackageSourceInstallTransports(t *testing.T) {
	for _, testCase := range []struct{ kind, role, moduleID string }{
		{"local", "standalone", "port-traffic"}, {"url", "standalone", "port-traffic"}, {"github", "standalone", "port-traffic"},
		{"local", "hub", "process-monitoring"}, {"url", "hub", "process-monitoring"}, {"github", "hub", "process-monitoring"},
	} {
		kind, moduleID := testCase.kind, testCase.moduleID
		t.Run(testCase.role+"/"+kind, func(t *testing.T) {
			service, priv, server := newTestService(t)
			server.Role = testCase.role
			if err := service.Store.UpsertServer(context.Background(), server); err != nil {
				t.Fatal(err)
			}
			archive := buildTarGz(t, []tarEntry{{name: "bin/" + moduleID, typeflag: tar.TypeReg, body: []byte("binary")}})
			manifest, sig := signedManifest(t, priv, moduleID, archive)
			raw, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			base := moduleID + "-linux-" + server.Architecture
			files := map[string][]byte{base + ".tar.gz": archive, base + ".manifest.json": raw, base + ".manifest.sig": []byte(sig)}
			source := PackageSource{Kind: kind}
			if kind == "local" {
				dir, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				for name, data := range files {
					if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
						t.Fatal(err)
					}
				}
				source.Location = filepath.Join(dir, base+".tar.gz")
			} else {
				var baseURL string
				remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/repos/example/packages/releases/latest" {
						assets := []map[string]string{}
						for name := range files {
							assets = append(assets, map[string]string{"name": name, "browser_download_url": baseURL + "/" + name})
						}
						json.NewEncoder(w).Encode(map[string]any{"assets": assets})
						return
					}
					if data, ok := files[filepath.Base(r.URL.Path)]; ok {
						w.Write(data)
						return
					}
					http.NotFound(w, r)
				}))
				defer remote.Close()
				baseURL = remote.URL
				service.Sources.Client = remote.Client()
				source.Location = remote.URL + "/" + base + ".tar.gz"
				if kind == "github" {
					service.Sources.GitHubAPI = remote.URL
					source.Location = "example/packages"
					source.Version = "latest"
				}
			}
			body := lifecycleRequest{IdempotencyKey: "source-install", Source: &source}
			path := "/api/v1/servers/" + string(server.ID) + "/modules/" + moduleID + "/install"
			result := doJSON(t, service.Handler(), http.MethodPost, path, body)
			if result.Code != http.StatusOK {
				t.Fatalf("install: %d %s", result.Code, result.Body.String())
			}
			installedBytes, err := os.ReadFile(filepath.Join(service.Manager.RootDir, string(server.ID), moduleID, "active", "bin", moduleID))
			if err != nil || !bytes.Equal(installedBytes, []byte("binary")) {
				t.Fatalf("signed source payload not installed: %q %v", installedBytes, err)
			}
			var installed contracts.ModuleInstallation
			if err := json.Unmarshal(result.Body.Bytes(), &installed); err != nil {
				t.Fatal(err)
			}
			if installed.State != contracts.ModuleInstalledDisabled {
				t.Fatalf("unexpected installed state: %s", installed.State)
			}
			replay := doJSON(t, service.Handler(), http.MethodPost, path, body)
			if replay.Code != http.StatusOK {
				t.Fatalf("replay: %d %s", replay.Code, replay.Body.String())
			}
		})
	}
}

func TestPackageSourceRejectsChangedArchive(t *testing.T) {
	service, priv, server := newTestService(t)
	archive := buildTarGz(t, []tarEntry{{name: "bin/port-traffic", typeflag: tar.TypeReg, body: []byte("binary")}})
	manifest, sig := signedManifest(t, priv, "port-traffic", archive)
	raw, _ := json.Marshal(manifest)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "package.tar.gz")
	archive[0] ^= 1
	for name, data := range map[string][]byte{path: archive, filepath.Join(dir, "package.manifest.json"): raw, filepath.Join(dir, "package.manifest.sig"): []byte(sig)} {
		if err := os.WriteFile(name, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	result := doJSON(t, service.Handler(), http.MethodPost, "/api/v1/servers/"+string(server.ID)+"/modules/port-traffic/install", lifecycleRequest{IdempotencyKey: "tampered", Source: &PackageSource{Kind: "local", Location: path}})
	if result.Code != http.StatusBadRequest {
		t.Fatalf("tampered package accepted: %d %s", result.Code, result.Body.String())
	}
}

func TestPackageSourceRejectsUnsupportedTargetsBeforeLoading(t *testing.T) {
	for _, tc := range []struct {
		name, role, moduleID string
		nonlocal             bool
	}{
		{"node-process", "node", "process-monitoring", false},
		{"nonlocal-hub-process", "hub", "process-monitoring", true},
		{"nonlocal-standalone-process", "standalone", "process-monitoring", true},
		{"hub-port-traffic", "hub", "port-traffic", false},
		{"hub-cpu-controls", "hub", "cpu-controls", false},
		{"hub-bandwidth-controls", "hub", "bandwidth-controls", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, _, server := newTestService(t)
			server.Role = tc.role
			if err := service.Store.UpsertServer(context.Background(), server); err != nil {
				t.Fatal(err)
			}
			if tc.nonlocal {
				service.Manager.LocalServerID = "different-local-server"
			}
			source := PackageSource{Kind: "local", Location: filepath.Join(t.TempDir(), "missing.tar.gz")}
			result := doJSON(t, service.Handler(), http.MethodPost, "/api/v1/servers/"+string(server.ID)+"/modules/"+tc.moduleID+"/install", lifecycleRequest{IdempotencyKey: "unsupported", Source: &source})
			if result.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected target refusal before missing source load, got %d %s", result.Code, result.Body.String())
			}
			if _, err := os.Stat(filepath.Join(service.Manager.RootDir, string(server.ID))); !os.IsNotExist(err) {
				t.Fatalf("unsupported target changed installation files: %v", err)
			}
		})
	}
}

func TestHubProcessSourceRejectsInvalidSignature(t *testing.T) {
	service, key, server := newTestService(t)
	server.Role = "hub"
	if err := service.Store.UpsertServer(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	archive := buildTarGz(t, []tarEntry{{name: "bin/process-monitoring", typeflag: tar.TypeReg, body: []byte("binary")}})
	manifest, _ := signedManifest(t, key, ProcessModuleID, archive)
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"package.tar.gz": archive, "package.manifest.json": raw, "package.manifest.sig": []byte("invalid-signature")} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	result := doJSON(t, service.Handler(), http.MethodPost, "/api/v1/servers/"+string(server.ID)+"/modules/process-monitoring/install", lifecycleRequest{IdempotencyKey: "invalid-signature", Source: &PackageSource{Kind: "local", Location: filepath.Join(dir, "package.tar.gz")}})
	if result.Code != http.StatusBadRequest {
		t.Fatalf("invalid signature accepted or source path refused: %d %s", result.Code, result.Body.String())
	}
	if _, err := os.Stat(filepath.Join(service.Manager.RootDir, string(server.ID), ProcessModuleID, "active")); !os.IsNotExist(err) {
		t.Fatalf("invalid signature activated payload: %v", err)
	}
}
