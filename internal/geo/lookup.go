// Package geo finds the country of a server's public address for display.
//
// Lookups use public HTTPS services that need no API key. Every result is
// cached by the caller, so each server is normally looked up once; the
// services' per-IP rate limits are therefore far out of reach, and a
// rate-limited provider is skipped until its cool-down ends.
package geo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// ErrRateLimited reports that every provider is cooling down or refused the
// request with HTTP 429.
var ErrRateLimited = errors.New("country lookup providers are rate limited")

// Provider resolves an address to an ISO 3166-1 alpha-2 country code. An
// invalid (zero) address asks for the caller's own public address.
type Provider struct {
	Name string
	// URL builds the request URL; self is true when ip is the zero value.
	URL func(ip netip.Addr, self bool) string
	// Parse extracts the country code from a successful response body.
	Parse func(body []byte) (string, error)
}

// CountryIs uses country.is, an open-source lookup service.
func CountryIs(base string) Provider {
	if base == "" {
		base = "https://api.country.is"
	}
	return Provider{
		Name: "country.is",
		URL: func(ip netip.Addr, self bool) string {
			if self {
				return base + "/"
			}
			return base + "/" + ip.String()
		},
		Parse: func(body []byte) (string, error) {
			var payload struct {
				Country string `json:"country"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				return "", errors.New("unexpected response format")
			}
			return payload.Country, nil
		},
	}
}

// IPInfo uses ipinfo.io's keyless plain-text country endpoint.
func IPInfo(base string) Provider {
	if base == "" {
		base = "https://ipinfo.io"
	}
	return Provider{
		Name: "ipinfo.io",
		URL: func(ip netip.Addr, self bool) string {
			if self {
				return base + "/country"
			}
			return base + "/" + ip.String() + "/country"
		},
		Parse: func(body []byte) (string, error) { return strings.TrimSpace(string(body)), nil },
	}
}

// Client tries providers in order until one returns a valid country code.
type Client struct {
	HTTP      *http.Client
	Providers []Provider
	UserAgent string
	// CoolDown is how long a provider that answered 429 is skipped.
	CoolDown time.Duration
	Now      func() time.Time

	mu        sync.Mutex
	coolUntil map[string]time.Time
}

// NewClient returns a client with the default providers and timeouts.
func NewClient(userAgent string) *Client {
	return &Client{
		HTTP:      &http.Client{Timeout: 8 * time.Second},
		Providers: []Provider{CountryIs(""), IPInfo("")},
		UserAgent: userAgent,
		CoolDown:  time.Hour,
	}
}

// Country looks up ip, or the caller's own public address when ip is the
// zero value. It returns an error rather than a guess whenever no provider
// gives a well-formed answer.
func (c *Client) Country(ctx context.Context, ip netip.Addr) (string, error) {
	self := !ip.IsValid()
	if !self && !Public(ip) {
		return "", errors.New("address is not publicly routable")
	}
	var failures []string
	limited := 0
	for _, provider := range c.Providers {
		if c.coolingDown(provider.Name) {
			limited++
			continue
		}
		code, err := c.query(ctx, provider, ip, self)
		if err == nil {
			return code, nil
		}
		if errors.Is(err, ErrRateLimited) {
			limited++
			c.startCoolDown(provider.Name)
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		failures = append(failures, provider.Name+": "+err.Error())
	}
	if limited == len(c.Providers) {
		return "", ErrRateLimited
	}
	return "", errors.New(strings.Join(failures, "; "))
}

func (c *Client) query(ctx context.Context, provider Provider, ip netip.Addr, self bool) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, provider.URL(ip, self), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json, text/plain")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("request failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", errors.New("reading response failed")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return "", ErrRateLimited
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	code, err := provider.Parse(body)
	if err != nil {
		return "", err
	}
	code = strings.ToUpper(strings.TrimSpace(code))
	if !ValidCountryCode(code) {
		return "", errors.New("response did not contain a country code")
	}
	return code, nil
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) coolingDown(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now().Before(c.coolUntil[name])
}

func (c *Client) startCoolDown(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.coolUntil == nil {
		c.coolUntil = map[string]time.Time{}
	}
	wait := c.CoolDown
	if wait <= 0 {
		wait = time.Hour
	}
	c.coolUntil[name] = c.now().Add(wait)
}

// ValidCountryCode accepts two upper-case ASCII letters, excluding the
// user-assigned codes services use for "unknown" (XX, ZZ) and EU.
func ValidCountryCode(code string) bool {
	if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
		return false
	}
	switch code {
	case "XX", "ZZ", "EU", "AP":
		return false
	}
	return true
}

var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// Public reports whether ip can meaningfully be geolocated.
func Public(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsValid() && ip.IsGlobalUnicast() && !ip.IsPrivate() && !sharedAddressSpace.Contains(ip)
}
