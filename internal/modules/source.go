package modules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

const maxSourceArchiveBytes = 64 << 20

// PackageSource identifies transport only. Every transport feeds the same
// manifest signature, checksum, archive, and platform verification in Manager.
type PackageSource struct {
	Kind              string `json:"kind"`
	Location          string `json:"location,omitempty"`
	Version           string `json:"version,omitempty"`
	ManifestLocation  string `json:"manifest_location,omitempty"`
	SignatureLocation string `json:"signature_location,omitempty"`
}

type SourceLoader struct {
	Client      *http.Client
	GitHubToken string
	GitHubAPI   string // test seam; production uses api.github.com
}

func (l SourceLoader) client() *http.Client {
	if l.Client != nil {
		return l.Client
	}
	return &http.Client{Timeout: 2 * time.Minute}
}

func (l SourceLoader) Load(ctx context.Context, source PackageSource, moduleID, architecture string) (contracts.ModuleManifest, string, []byte, error) {
	var manifest contracts.ModuleManifest
	if len(source.Location) > 4096 || len(source.ManifestLocation) > 4096 || len(source.SignatureLocation) > 4096 || len(source.Version) > 128 {
		return manifest, "", nil, errors.New("package source location exceeds its limit")
	}
	if source.Kind != "github" && source.Kind != "local" && source.Kind != "url" {
		return manifest, "", nil, errors.New("package source must be github, local, or url")
	}
	archiveLocation, manifestLocation, signatureLocation := source.Location, source.ManifestLocation, source.SignatureLocation
	if source.Kind == "github" {
		locations, err := l.githubAssets(ctx, source, moduleID, architecture)
		if err != nil {
			return manifest, "", nil, err
		}
		archiveLocation, manifestLocation, signatureLocation = locations[0], locations[1], locations[2]
	} else {
		if archiveLocation == "" {
			return manifest, "", nil, errors.New("package path or URL is required")
		}
		base := strings.TrimSuffix(archiveLocation, ".tar.gz")
		if manifestLocation == "" {
			manifestLocation = base + ".manifest.json"
		}
		if signatureLocation == "" {
			signatureLocation = base + ".manifest.sig"
		}
	}
	raw, err := l.read(ctx, source.Kind, manifestLocation, 1<<20)
	if err != nil {
		return manifest, "", nil, fmt.Errorf("read package manifest: %w", err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return manifest, "", nil, fmt.Errorf("read package manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return manifest, "", nil, err
	}
	if manifest.ModuleID != moduleID || manifest.Architecture != architecture {
		return manifest, "", nil, errors.New("package does not match the selected module and server architecture")
	}
	if manifest.CompressedBytes > maxSourceArchiveBytes || manifest.UnpackedBytes > 256<<20 {
		return manifest, "", nil, errors.New("package exceeds the installation size limit")
	}
	sig, err := l.read(ctx, source.Kind, signatureLocation, 4096)
	if err != nil {
		return manifest, "", nil, fmt.Errorf("read package signature: %w", err)
	}
	archive, err := l.read(ctx, source.Kind, archiveLocation, int64(manifest.CompressedBytes))
	if err != nil {
		return manifest, "", nil, fmt.Errorf("read package archive: %w", err)
	}
	return manifest, strings.TrimSpace(string(sig)), archive, nil
}

func (l SourceLoader) read(ctx context.Context, kind, location string, limit int64) ([]byte, error) {
	var reader io.ReadCloser
	if kind == "local" {
		if !filepath.IsAbs(location) {
			return nil, errors.New("local package path must be absolute on the Payesh server")
		}
		// Reject links in every component, as well as devices, sockets, and pipes.
		for path := filepath.Clean(location); ; path = filepath.Dir(path) {
			info, err := os.Lstat(path)
			if err != nil {
				return nil, err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return nil, errors.New("local package paths must not contain symlinks")
			}
			if path == filepath.Clean(location) && !info.Mode().IsRegular() {
				return nil, errors.New("local package must be a regular file")
			}
			if path == filepath.Dir(path) {
				break
			}
		}
		file, err := os.Open(location)
		if err != nil {
			return nil, err
		}
		reader = file
	} else {
		u, err := url.Parse(location)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return nil, errors.New("package URL must use HTTP or HTTPS without embedded credentials")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
		if err != nil {
			return nil, err
		}
		if kind == "github" {
			req.Header.Set("Accept", "application/octet-stream")
			if l.GitHubToken != "" {
				req.Header.Set("Authorization", "Bearer "+l.GitHubToken)
			}
		}
		resp, err := l.client().Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("package source returned HTTP %d", resp.StatusCode)
		}
		reader = resp.Body
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("package source exceeds its size limit")
	}
	return data, nil
}

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var sourceVersionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+([-.][A-Za-z0-9.-]+)?$`)

func (l SourceLoader) githubAssets(ctx context.Context, source PackageSource, moduleID, arch string) ([3]string, error) {
	var locations [3]string
	repo := source.Location
	if repo == "" {
		repo = "Real-kia/payesh"
	}
	if !repositoryPattern.MatchString(repo) {
		return locations, errors.New("GitHub repository must be owner/repository")
	}
	endpoint := "latest"
	if source.Version != "" && source.Version != "latest" {
		if !sourceVersionPattern.MatchString(source.Version) {
			return locations, errors.New("GitHub release version is invalid")
		}
		endpoint = "tags/v" + strings.TrimPrefix(source.Version, "v")
	}
	api := l.GitHubAPI
	if api == "" {
		api = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api+"/repos/"+repo+"/releases/"+endpoint, nil)
	if err != nil {
		return locations, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if l.GitHubToken != "" {
		req.Header.Set("Authorization", "Bearer "+l.GitHubToken)
	}
	resp, err := l.client().Do(req)
	if err != nil {
		return locations, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return locations, fmt.Errorf("GitHub release returned HTTP %d", resp.StatusCode)
	}
	var release struct {
		Assets []struct {
			Name        string `json:"name"`
			URL         string `json:"url"`
			DownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&release); err != nil {
		return locations, err
	}
	base := moduleID + "-linux-" + arch
	for i, name := range []string{base + ".tar.gz", base + ".manifest.json", base + ".manifest.sig"} {
		for _, asset := range release.Assets {
			if asset.Name == name {
				locations[i] = asset.DownloadURL
				if l.GitHubToken != "" {
					locations[i] = asset.URL
				}
				break
			}
		}
		if locations[i] == "" {
			return locations, fmt.Errorf("GitHub release is missing %s", name)
		}
		u, err := url.Parse(locations[i])
		if err != nil || (l.GitHubAPI == "" && (u.Scheme != "https" || (u.Host != "github.com" && u.Host != "api.github.com"))) {
			return locations, errors.New("GitHub asset location is invalid")
		}
	}
	return locations, nil
}
