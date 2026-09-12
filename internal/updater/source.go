package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

const (
	DefaultReleaseSourceTimeout = 30 * time.Second
	DefaultManifestMaxBytes     = 1 << 20
	DefaultSignatureMaxBytes    = 4 << 10
)

var (
	ErrReleaseUnavailable = errors.New("updater: release is unavailable")
	ErrReleaseSource      = errors.New("updater: release source is invalid")
)

// ReleaseBundle is the authenticated metadata pair returned by a source.
// The source does not verify the signature; VerifyManifest does that with the
// caller's pinned trust registry before any artifact request is made.
type ReleaseBundle struct {
	Manifest  contracts.ReleaseManifest
	Signature string
}

// ReleaseSource obtains a manifest and its detached signature for one exact
// semantic release. Implementations must not return a different release.
type ReleaseSource interface {
	Fetch(context.Context, string) (ReleaseBundle, error)
}

// HTTPReleaseSource reads release metadata from a conventional immutable
// release directory: <BaseURL>/<release>/manifest.json and .sig. BaseURL may
// point at a repository release prefix (for example a GitHub Releases
// download URL), but credentials in URLs are never accepted.
type HTTPReleaseSource struct {
	BaseURL      string
	Client       *http.Client
	Timeout      time.Duration
	MaxManifest  uint64
	MaxSignature uint64
}

func (s HTTPReleaseSource) Fetch(ctx context.Context, release string) (ReleaseBundle, error) {
	if !ValidRelease(release) {
		return ReleaseBundle{}, fmt.Errorf("%w: release is invalid", ErrReleaseSource)
	}
	base, err := validateSourceURL(s.BaseURL)
	if err != nil {
		return ReleaseBundle{}, err
	}
	manifestLimit := s.MaxManifest
	if manifestLimit == 0 {
		manifestLimit = DefaultManifestMaxBytes
	}
	signatureLimit := s.MaxSignature
	if signatureLimit == 0 {
		signatureLimit = DefaultSignatureMaxBytes
	}
	if manifestLimit == 0 || manifestLimit > 16<<20 || signatureLimit == 0 || signatureLimit > 64<<10 {
		return ReleaseBundle{}, fmt.Errorf("%w: metadata bounds are invalid", ErrReleaseSource)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = DefaultReleaseSourceTimeout
	}
	if timeout <= 0 || timeout > 10*time.Minute {
		return ReleaseBundle{}, fmt.Errorf("%w: source timeout is invalid", ErrReleaseSource)
	}
	client := s.Client
	if client == nil {
		client = &http.Client{}
	}
	clientCopy := *client
	userRedirect := clientCopy.CheckRedirect
	clientCopy.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if _, redirectErr := validateSourceURL(next.URL.String()); redirectErr != nil {
			return redirectErr
		}
		if len(via) >= 5 {
			return errors.New("too many release metadata redirects")
		}
		if userRedirect != nil {
			return userRedirect(next, via)
		}
		return nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	manifestURL := releaseURL(base, release, "manifest.json")
	signatureURL := releaseURL(base, release, "manifest.json.sig")
	manifestBody, err := fetchBounded(requestCtx, &clientCopy, manifestURL, manifestLimit)
	if err != nil {
		return ReleaseBundle{}, fmt.Errorf("%w: fetch manifest: %w", ErrReleaseUnavailable, err)
	}
	var manifest contracts.ReleaseManifest
	decoder := json.NewDecoder(strings.NewReader(string(manifestBody)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return ReleaseBundle{}, fmt.Errorf("%w: manifest is invalid: %v", ErrReleaseSource, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return ReleaseBundle{}, fmt.Errorf("%w: manifest has trailing data", ErrReleaseSource)
		}
		return ReleaseBundle{}, fmt.Errorf("%w: manifest has trailing data: %v", ErrReleaseSource, err)
	}
	if manifest.Release != release {
		return ReleaseBundle{}, fmt.Errorf("%w: manifest release %q does not match requested %q", ErrReleaseSource, manifest.Release, release)
	}
	signatureBody, err := fetchBounded(requestCtx, &clientCopy, signatureURL, signatureLimit)
	if err != nil {
		return ReleaseBundle{}, fmt.Errorf("%w: fetch detached signature: %w", ErrReleaseUnavailable, err)
	}
	if len(signatureBody) == 0 || strings.TrimSpace(string(signatureBody)) != string(signatureBody) {
		return ReleaseBundle{}, fmt.Errorf("%w: detached signature contains whitespace", ErrReleaseSource)
	}
	return ReleaseBundle{Manifest: manifest, Signature: string(signatureBody)}, nil
}

func validateSourceURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("%w: base URL must be an HTTP(S) URL without credentials", ErrReleaseSource)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

func releaseURL(base *url.URL, release, name string) string {
	u := *base
	// release is validated semantic version text, and PathEscape keeps this
	// construction safe even if the accepted prerelease grammar changes later.
	u.Path = path.Join(base.Path, url.PathEscape(release), name)
	return u.String()
}

func fetchBounded(ctx context.Context, client *http.Client, rawURL string, max uint64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("server returned HTTP %s", response.Status)
	}
	if response.ContentLength >= 0 && uint64(response.ContentLength) > max {
		return nil, fmt.Errorf("response exceeds %d bytes", max)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if uint64(len(body)) > max {
		return nil, fmt.Errorf("response exceeds %d bytes", max)
	}
	return body, nil
}
