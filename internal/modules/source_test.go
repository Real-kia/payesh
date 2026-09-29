package modules

import (
	"archive/tar"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPackageSourceInstallTransports(t *testing.T) {
	for _, kind := range []string{"local", "url", "github"} {
		t.Run(kind, func(t *testing.T) {
			service, priv, server := newTestService(t)
			archive := buildTarGz(t, []tarEntry{{name: "bin/port-traffic", typeflag: tar.TypeReg, body: []byte("binary")}})
			manifest, sig := signedManifest(t, priv, "port-traffic", archive)
			raw, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			base := "port-traffic-linux-" + server.Architecture
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
			path := "/api/v1/servers/" + string(server.ID) + "/modules/port-traffic/install"
			result := doJSON(t, service.Handler(), http.MethodPost, path, body)
			if result.Code != http.StatusOK {
				t.Fatalf("install: %d %s", result.Code, result.Body.String())
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
