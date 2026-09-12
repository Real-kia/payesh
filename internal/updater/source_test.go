package updater

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestHTTPReleaseSourceFetchesExactMetadataPair(t *testing.T) {
	manifest := contracts.ReleaseManifest{Format: contracts.ReleaseFormat, Release: "1.2.3", MinCore: "1.0.0", SigningKeyID: "release-test"}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/releases/1.2.3/manifest.json" {
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(string(body))), ContentLength: int64(len(body)), Header: make(http.Header), Request: r}, nil
		}
		if r.URL.Path == "/releases/1.2.3/manifest.json.sig" {
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader("detached-signature")), ContentLength: 18, Header: make(http.Header), Request: r}, nil
		}
		return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: r}, nil
	})}
	bundle, err := (HTTPReleaseSource{BaseURL: "https://example.invalid/releases", Client: client}).Fetch(t.Context(), "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Manifest.Release != "1.2.3" || bundle.Signature != "detached-signature" {
		t.Fatalf("unexpected bundle: %+v", bundle)
	}
}

func TestHTTPReleaseSourceRejectsSignatureWhitespaceAndManifestMismatch(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, ".sig") {
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader("signature\n")), Header: make(http.Header), Request: r}, nil
		}
		manifest := contracts.ReleaseManifest{Format: contracts.ReleaseFormat, Release: "9.9.9"}
		body, _ := json.Marshal(manifest)
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(string(body))), ContentLength: int64(len(body)), Header: make(http.Header), Request: r}, nil
	})}
	_, err := (HTTPReleaseSource{BaseURL: "https://example.invalid", Client: client}).Fetch(t.Context(), "1.2.3")
	if err == nil {
		t.Fatal("expected release mismatch")
	}
}

func TestHTTPReleaseSourceRejectsUnsafeBaseURL(t *testing.T) {
	_, err := (HTTPReleaseSource{BaseURL: "https://user:pass@example.invalid/releases"}).Fetch(t.Context(), "1.2.3")
	if err == nil {
		t.Fatal("expected credentials in URL to be rejected")
	}
}
