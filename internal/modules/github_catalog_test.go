package modules

import (
	"context"
	"encoding/json"
	"github.com/Real-kia/payesh/internal/contracts"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestGitHubCatalogRefreshDiscoveryAndFallback(t *testing.T) {
	calls := 0
	mode := "first"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/repos/Real-kia/payesh/contents/packages/catalog.json" || r.Header.Get("Accept") != "application/vnd.github.raw+json" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("incorrect GitHub request")
		}
		if mode == "error" {
			w.WriteHeader(503)
			return
		}
		if mode == "unchanged" {
			if r.Header.Get("If-None-Match") != "catalog-two" {
				t.Error("missing ETag")
			}
			w.WriteHeader(304)
			return
		}
		items := append([]contracts.ModuleCatalogEntry{}, Catalog...)
		if mode == "second" {
			items = append(items, contracts.ModuleCatalogEntry{ID: "future-package", Name: "Future Package", LatestVersion: "1.0.0", ResourceEstimateSource: "unmeasured"})
		}
		w.Header().Set("ETag", "catalog-two")
		_ = json.NewEncoder(w).Encode(CatalogDocument{Format: "payesh.package-catalog.v1", Items: items})
	}))
	defer server.Close()
	catalog := &GitHubCatalog{Sources: SourceLoader{Client: server.Client(), GitHubAPI: server.URL, GitHubToken: "test-token"}}
	first := catalog.Fetch(context.Background(), false)
	if first.Source != "github" || first.Stale || len(first.Items) != len(Catalog) {
		t.Fatalf("first: %+v", first)
	}
	mode = "second"
	_ = catalog.Fetch(context.Background(), false)
	if calls != 1 {
		t.Fatal("short cache did not coalesce")
	}
	second := catalog.Fetch(context.Background(), true)
	if len(second.Items) != len(Catalog)+1 || second.Items[len(Catalog)].InstallSupported == nil || *second.Items[len(Catalog)].InstallSupported {
		t.Fatal("new package not discovered or falsely marked executable")
	}
	mode = "unchanged"
	if result := catalog.Fetch(context.Background(), true); result.Stale || len(result.Items) != len(second.Items) {
		t.Fatal("conditional revalidation lost catalog")
	}
	mode = "error"
	fallback := catalog.Fetch(context.Background(), true)
	if !fallback.Stale || fallback.Source != "github" || len(fallback.Items) != len(second.Items) {
		t.Fatal("last known catalog lost during outage")
	}
	empty := &GitHubCatalog{Sources: catalog.Sources}
	if result := empty.Fetch(context.Background(), true); result.Source != "bundled" || !result.Stale {
		t.Fatal("missing initial fallback")
	}
}
func TestCatalogRejectsInvalidMetadata(t *testing.T) {
	base := CatalogDocument{Format: "payesh.package-catalog.v1", Items: append([]contracts.ModuleCatalogEntry{}, Catalog...)}
	if e := validateCatalog(base); e != nil {
		t.Fatal(e)
	}
	base.Items = append(base.Items, base.Items[0])
	if validateCatalog(base) == nil {
		t.Fatal("duplicate accepted")
	}
	base.Items = base.Items[:1]
	base.Items[0].Repository = "https://evil.example"
	if validateCatalog(base) == nil {
		t.Fatal("invalid repo accepted")
	}
}
func TestPublishedCatalogFile(t *testing.T) {
	raw, e := os.ReadFile("../../packages/catalog.json")
	if e != nil {
		t.Fatal(e)
	}
	var d CatalogDocument
	if e = json.Unmarshal(raw, &d); e != nil {
		t.Fatal(e)
	}
	if e = validateCatalog(d); e != nil {
		t.Fatal(e)
	}
}

func TestCatalogNewVersionRequiresExecutorCompatibility(t *testing.T) {
	c := &GitHubCatalog{items: append([]contracts.ModuleCatalogEntry{}, Catalog...)}
	c.items[0].LatestVersion = "9.0.0"
	result := c.result()
	if result.Items[0].InstallSupported == nil || *result.Items[0].InstallSupported {
		t.Fatal("unsupported release was offered for installation")
	}
}
