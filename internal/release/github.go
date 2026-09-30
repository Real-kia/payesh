package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/updater"
)

const DefaultRepository = "Real-kia/payesh"

type GitHubRelease struct {
	Version     string    `json:"version"`
	URL         string    `json:"url"`
	PublishedAt time.Time `json:"published_at"`
}

type GitHubClient struct {
	Client     *http.Client
	Token      string
	Repository string
	APIBase    string // injectable for tests; defaults to GitHub's HTTPS API
}

func (g GitHubClient) Latest(ctx context.Context) (GitHubRelease, error) {
	repo := g.Repository
	if repo == "" {
		repo = DefaultRepository
	}
	if repo != DefaultRepository {
		return GitHubRelease{}, errors.New("unsupported GitHub repository")
	}
	base := g.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	if !strings.HasPrefix(base, "https://") && g.APIBase == "" {
		return GitHubRelease{}, errors.New("GitHub API requires HTTPS")
	}
	client := g.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return GitHubRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return GitHubRelease{}, fmt.Errorf("query GitHub release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return GitHubRelease{}, fmt.Errorf("GitHub release check returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		TagName     string    `json:"tag_name"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return GitHubRelease{}, fmt.Errorf("decode GitHub release: %w", err)
	}
	version := strings.TrimPrefix(payload.TagName, "v")
	if !updater.ValidRelease(version) {
		return GitHubRelease{}, errors.New("GitHub release has an invalid version tag")
	}
	return GitHubRelease{Version: version, URL: "https://github.com/" + repo + "/releases/tag/v" + version, PublishedAt: payload.PublishedAt}, nil
}

func (g GitHubClient) History(ctx context.Context) ([]GitHubRelease, error) {
	repo := g.Repository
	if repo == "" {
		repo = DefaultRepository
	}
	if repo != DefaultRepository {
		return nil, errors.New("unsupported GitHub repository")
	}
	base := g.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	if !strings.HasPrefix(base, "https://") && g.APIBase == "" {
		return nil, errors.New("GitHub API requires HTTPS")
	}
	client := g.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/repos/"+repo+"/releases?per_page=20", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query GitHub release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub release check returned HTTP %d", resp.StatusCode)
	}
	var payload []struct {
		Draft       bool      `json:"draft"`
		Prerelease  bool      `json:"prerelease"`
		TagName     string    `json:"tag_name"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode GitHub release: %w", err)
	}
	items := make([]GitHubRelease, 0, len(payload))
	for _, item := range payload {
		v := strings.TrimPrefix(item.TagName, "v")
		if item.Draft || item.Prerelease || !updater.ValidRelease(v) {
			continue
		}
		items = append(items, GitHubRelease{Version: v, URL: "https://github.com/" + repo + "/releases/tag/v" + v, PublishedAt: item.PublishedAt})
	}
	return items, nil
}
