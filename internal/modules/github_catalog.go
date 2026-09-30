package modules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Real-kia/payesh/internal/contracts"
	"io"
	"net/http"
	"sync"
	"time"
)

const CatalogPath = "packages/catalog.json"

type CatalogDocument struct {
	Format string                         `json:"format"`
	Items  []contracts.ModuleCatalogEntry `json:"items"`
}
type CatalogResult struct {
	Items   []contracts.ModuleCatalogEntry `json:"items"`
	Source  string                         `json:"source"`
	Stale   bool                           `json:"stale"`
	Warning string                         `json:"warning,omitempty"`
}
type GitHubCatalog struct {
	Sources SourceLoader
	mu      sync.Mutex
	items   []contracts.ModuleCatalogEntry
	etag    string
	checked time.Time
	warning string
}

// Fetch revalidates the repository's default-branch catalog independently of
// core releases. A short cache coalesces repeated page requests; Refresh forces
// revalidation. Repository metadata never changes executable trust anchors.
func (c *GitHubCatalog) Fetch(ctx context.Context, refresh bool) CatalogResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !refresh && !c.checked.IsZero() && time.Since(c.checked) < 30*time.Second {
		return c.result()
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	api := c.Sources.GitHubAPI
	if api == "" {
		api = "https://api.github.com"
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, api+"/repos/Real-kia/payesh/contents/"+CatalogPath, nil)
	if e == nil {
		req.Header.Set("Accept", "application/vnd.github.raw+json")
		req.Header.Set("User-Agent", "Payesh-package-catalog")
		if c.Sources.GitHubToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.Sources.GitHubToken)
		}
		if c.etag != "" {
			req.Header.Set("If-None-Match", c.etag)
		}
		resp, err := c.Sources.client().Do(req)
		e = err
		if err == nil {
			defer resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusNotModified:
				if c.items == nil {
					e = errors.New("catalog returned no cached content")
				}
			case http.StatusOK:
				raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
				e = err
				if e == nil && len(raw) > 1<<20 {
					e = errors.New("catalog exceeds its size limit")
				}
				if e == nil {
					var document CatalogDocument
					e = json.Unmarshal(raw, &document)
					if e == nil {
						e = validateCatalog(document)
					}
					if e == nil {
						c.items = document.Items
						c.etag = resp.Header.Get("ETag")
					}
				}
			default:
				e = fmt.Errorf("GitHub catalog returned HTTP %d", resp.StatusCode)
			}
		}
	}
	c.checked = time.Now()
	c.warning = ""
	if e != nil {
		c.warning = "GitHub catalog unavailable; showing saved packages."
	}
	return c.result()
}
func (c *GitHubCatalog) result() CatalogResult {
	items := c.items
	source := "github"
	if items == nil {
		items = Catalog
		source = "bundled"
	}
	copyItems := append([]contracts.ModuleCatalogEntry{}, items...)
	for i := range copyItems {
		entry, supported := Lookup(copyItems[i].ID)
		supported = supported && entry.LatestVersion == copyItems[i].LatestVersion
		copyItems[i].InstallSupported = &supported
	}
	return CatalogResult{Items: copyItems, Source: source, Stale: c.warning != "", Warning: c.warning}
}
func validateCatalog(d CatalogDocument) error {
	if d.Format != "payesh.package-catalog.v1" || d.Items == nil || len(d.Items) > 200 {
		return errors.New("invalid catalog format or size")
	}
	seen := map[string]bool{}
	for _, entry := range d.Items {
		if !moduleIDPattern.MatchString(entry.ID) || seen[entry.ID] || entry.Name == "" || len(entry.Name) > 128 || len(entry.Description) > 1024 || !semverPattern.MatchString(entry.LatestVersion) || len(entry.Dependencies) > 32 || len(entry.RequiredPrivileges) > 32 {
			return errors.New("invalid package entry")
		}
		seen[entry.ID] = true
		if entry.Repository != "" && !repositoryPattern.MatchString(entry.Repository) {
			return errors.New("invalid package repository")
		}
		if entry.Release != "" && entry.Release != "latest" && !sourceVersionPattern.MatchString(entry.Release) {
			return errors.New("invalid package release")
		}
		if entry.ResourceEstimateSource != "unmeasured" && entry.ResourceEstimateSource != "release-benchmark" {
			return errors.New("invalid resource estimate")
		}
		for _, name := range append(append([]string{}, entry.Dependencies...), entry.RequiredPrivileges...) {
			if !moduleIDPattern.MatchString(name) {
				return errors.New("invalid package requirement")
			}
		}
	}
	return nil
}
