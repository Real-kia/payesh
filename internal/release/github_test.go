package release

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitHubLatestPublicAndPrivate(t *testing.T) {
	for _, token := range []string{"", "test-token"} {
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != "https://api.github.com/repos/Real-kia/payesh/releases/latest" {
				t.Errorf("url=%s", r.URL)
			}
			want := ""
			if token != "" {
				want = "Bearer test-token"
			}
			if got := r.Header.Get("Authorization"); got != want {
				t.Errorf("authorization=%q", got)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"tag_name":"v1.2.3","published_at":"2026-09-29T00:00:00Z"}`)), Header: make(http.Header)}, nil
		})}
		got, err := (GitHubClient{Client: client, Token: token}).Latest(context.Background())
		if err != nil || got.Version != "1.2.3" || got.URL != "https://github.com/Real-kia/payesh/releases/tag/v1.2.3" {
			t.Fatalf("release=%+v err=%v", got, err)
		}
	}
}
