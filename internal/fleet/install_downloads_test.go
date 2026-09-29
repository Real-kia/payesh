package fleet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Real-kia/payesh/internal/monitoring"
)

func TestInstallBundleUsesJobTokenInsteadOfBrowserSession(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewInstallService(store)
	if err != nil {
		t.Fatal(err)
	}
	api, err := NewAPIWithOptions(store, "0123456789abcdef-bootstrap", Options{InstallService: service})
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{}
	root := t.TempDir()
	for _, name := range []string{"payesh-install", "payesh-agent", "payesh-privd", "payesh"} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(name), 0755); err != nil {
			t.Fatal(err)
		}
		paths[name] = path
	}
	download, err := service.Downloads.Publish(ctx, "https://hub.example", "node", paths)
	if err != nil {
		t.Fatal(err)
	}
	defer download.Close()
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, download.URL, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("node download needs session: %d %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/install-artifacts/"+strings.Repeat("0", 64)+"/bundle.tar.gz", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("invalid token accepted: %d", response.Code)
	}
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/installations", strings.NewReader(`{}`)))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("install authorization weakened: %d", response.Code)
	}
}
