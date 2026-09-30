// Package webtls gives the dashboard listener an automatic Let's Encrypt
// certificate for an operator-chosen domain without using port 443 or
// touching another web server. HTTPS is served on the dashboard's own port.
//
// Domain validation uses ACME HTTP-01 on port 80 only while a challenge is
// pending, and only when port 80 is free. When something else (for example
// nginx) owns port 80, DNS-01 through a Cloudflare API token is used instead.
package webtls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultDir holds the account key, configuration, and issued certificate.
const DefaultDir = "/var/lib/payesh/tls"

// LetsEncryptURL is the production ACME directory. PAYESH_ACME_DIRECTORY
// overrides it (for example with the Let's Encrypt staging directory).
const LetsEncryptURL = "https://acme-v02.api.letsencrypt.org/directory"

const (
	MethodAuto       = "auto"
	MethodHTTP01     = "http-01"
	MethodCloudflare = "dns-cloudflare"

	StateDisabled = "disabled"
	StatePending  = "pending"
	StateActive   = "active"
	StateFailed   = "failed"

	renewBefore = 30 * 24 * time.Hour
)

var (
	ErrInvalidDomain = errors.New("enter a domain name such as panel.example.com")
	ErrBusy          = errors.New("a certificate request is already in progress")
	ErrPort80InUse   = errors.New("port 80 is in use by another program (for example nginx), so Let's Encrypt cannot check the domain there; provide a Cloudflare API token to verify through DNS instead")
	domainPattern    = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
)

// Config is the persisted operator choice. The Cloudflare token is stored
// owner-only on disk and never returned by Status.
type Config struct {
	Domain          string `json:"domain"`
	Email           string `json:"email,omitempty"`
	CloudflareToken string `json:"cloudflare_api_token,omitempty"`
	// Method records how the last certificate was validated so renewals use
	// the same route.
	Method string `json:"method,omitempty"`
}

// Status is the public view used by the API, CLI, and dashboard.
type Status struct {
	Domain    string     `json:"domain,omitempty"`
	State     string     `json:"state"`
	Method    string     `json:"method,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Error     string     `json:"error,omitempty"`
	// HTTPSPort is the dashboard port HTTPS is served on once active.
	HTTPSPort string `json:"https_port,omitempty"`
}

// Manager owns certificate state for one dashboard listener.
type Manager struct {
	Dir          string
	DirectoryURL string
	// HTTPChallengeAddr is where HTTP-01 challenges are answered (":80").
	HTTPChallengeAddr string
	// DNSPropagationDelay is how long to wait after publishing a DNS-01 record.
	DNSPropagationDelay time.Duration
	// Port is the dashboard port reported in Status.
	Port string

	portController *PortController
	cert           atomic.Pointer[tls.Certificate]
	busy           atomic.Bool
	mu             sync.Mutex
	config         Config
	state          string
	lastErr        string
}

// NewManager loads any saved configuration and certificate from dir.
func NewManager(dir string) (*Manager, error) {
	m := &Manager{Dir: dir, DirectoryURL: LetsEncryptURL, HTTPChallengeAddr: ":80", DNSPropagationDelay: 20 * time.Second, state: StateDisabled}
	if url := strings.TrimSpace(os.Getenv("PAYESH_ACME_DIRECTORY")); url != "" {
		m.DirectoryURL = url
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) path(name string) string { return filepath.Join(m.Dir, name) }

func (m *Manager) load() error {
	b, err := os.ReadFile(m.path("config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read TLS configuration: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("parse TLS configuration: %w", err)
	}
	m.config = cfg
	if cfg.Domain == "" {
		return nil
	}
	m.state = StatePending
	certificate, err := tls.LoadX509KeyPair(m.path("cert.pem"), m.path("key.pem"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		m.state, m.lastErr = StateFailed, "stored certificate is unreadable: "+err.Error()
		return nil
	}
	if leaf, err := x509.ParseCertificate(certificate.Certificate[0]); err == nil && leaf.VerifyHostname(cfg.Domain) == nil {
		certificate.Leaf = leaf
		m.cert.Store(&certificate)
		m.state = StateActive
	}
	return nil
}

// Active reports whether HTTPS can be served.
func (m *Manager) Active() bool { return m.cert.Load() != nil }

// Domain returns the configured domain, if any.
func (m *Manager) Domain() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.config.Domain
}

// Status returns the operator-visible state without secrets.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{Domain: m.config.Domain, State: m.state, Method: m.config.Method, Error: m.lastErr, HTTPSPort: m.Port}
	if c := m.cert.Load(); c != nil && c.Leaf != nil {
		expires := c.Leaf.NotAfter.UTC()
		s.ExpiresAt = &expires
	}
	return s
}

// GetCertificate serves the current certificate for tls.Config.
func (m *Manager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if c := m.cert.Load(); c != nil {
		return c, nil
	}
	return nil, errors.New("no certificate is configured")
}

// NormalizeDomain lower-cases and validates a public DNS name.
func NormalizeDomain(domain string) (string, error) {
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	domain = strings.TrimPrefix(strings.TrimPrefix(domain, "https://"), "http://")
	if len(domain) > 253 || !domainPattern.MatchString(domain) {
		return "", ErrInvalidDomain
	}
	return domain, nil
}

// Configure validates cfg, saves it, and obtains a certificate. It blocks
// until issuance finishes or fails; callers wanting a background request use
// ConfigureAsync.
func (m *Manager) Configure(ctx context.Context, cfg Config) error {
	cfg, err := m.prepare(cfg)
	if err != nil {
		return err
	}
	defer m.busy.Store(false)
	return m.issue(ctx, cfg)
}

// ConfigureAsync validates and saves cfg, then issues in the background.
func (m *Manager) ConfigureAsync(ctx context.Context, cfg Config) error {
	cfg, err := m.prepare(cfg)
	if err != nil {
		return err
	}
	go func() {
		defer m.busy.Store(false)
		_ = m.issue(ctx, cfg)
	}()
	return nil
}

func (m *Manager) prepare(cfg Config) (Config, error) {
	domain, err := NormalizeDomain(cfg.Domain)
	if err != nil {
		return Config{}, err
	}
	cfg.Domain = domain
	cfg.Email = strings.TrimSpace(cfg.Email)
	cfg.CloudflareToken = strings.TrimSpace(cfg.CloudflareToken)
	if !m.busy.CompareAndSwap(false, true) {
		return Config{}, ErrBusy
	}
	m.mu.Lock()
	// Keep a previously saved token when the caller changes only the domain.
	if cfg.CloudflareToken == "" && m.config.CloudflareToken != "" {
		cfg.CloudflareToken = m.config.CloudflareToken
	}
	m.state, m.lastErr = StatePending, ""
	m.mu.Unlock()
	return cfg, nil
}

// Disable removes the domain and certificate; the dashboard returns to HTTP.
func (m *Manager) Disable() error {
	if !m.busy.CompareAndSwap(false, true) {
		return ErrBusy
	}
	defer m.busy.Store(false)
	for _, name := range []string{"config.json", "cert.pem", "key.pem"} {
		if err := os.Remove(m.path(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	m.cert.Store(nil)
	m.mu.Lock()
	m.config, m.state, m.lastErr = Config{}, StateDisabled, ""
	m.mu.Unlock()
	return nil
}

// RenewLoop renews the certificate when it is within 30 days of expiry.
func (m *Manager) RenewLoop(ctx context.Context, logf func(string, ...any)) {
	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()
	for {
		if c := m.cert.Load(); c != nil && c.Leaf != nil && time.Until(c.Leaf.NotAfter) < renewBefore && m.busy.CompareAndSwap(false, true) {
			m.mu.Lock()
			cfg := m.config
			m.mu.Unlock()
			if err := m.issue(ctx, cfg); err != nil && logf != nil {
				logf("renew certificate for %s: %v", cfg.Domain, err)
			}
			m.busy.Store(false)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) fail(err error) error {
	m.mu.Lock()
	// A failed renewal keeps serving the still-valid certificate.
	if m.cert.Load() == nil {
		m.state = StateFailed
	}
	m.lastErr = err.Error()
	m.mu.Unlock()
	return err
}

func (m *Manager) issue(ctx context.Context, cfg Config) error {
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return m.fail(fmt.Errorf("create %s: %w", m.Dir, err))
	}
	method, err := m.chooseMethod(cfg)
	if err != nil {
		return m.fail(err)
	}
	cfg.Method = method
	if err := writeJSON(m.path("config.json"), cfg); err != nil {
		return m.fail(err)
	}
	m.mu.Lock()
	previousDomain := m.config.Domain
	m.config = cfg
	m.mu.Unlock()
	if previousDomain != cfg.Domain {
		// A certificate for another name must not keep being served.
		m.cert.Store(nil)
	}
	issueCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	certPEM, keyPEM, err := m.obtain(issueCtx, cfg)
	if err != nil {
		return m.fail(err)
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return m.fail(fmt.Errorf("issued certificate is unusable: %w", err))
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return m.fail(err)
	}
	certificate.Leaf = leaf
	if err := writeFileAtomic(m.path("key.pem"), keyPEM, 0o600); err != nil {
		return m.fail(err)
	}
	if err := writeFileAtomic(m.path("cert.pem"), certPEM, 0o644); err != nil {
		return m.fail(err)
	}
	m.cert.Store(&certificate)
	m.mu.Lock()
	m.state, m.lastErr = StateActive, ""
	m.mu.Unlock()
	return nil
}

func (m *Manager) chooseMethod(cfg Config) (string, error) {
	if cfg.Method == MethodCloudflare && cfg.CloudflareToken != "" {
		return MethodCloudflare, nil
	}
	if port80Free(m.HTTPChallengeAddr) {
		return MethodHTTP01, nil
	}
	if cfg.CloudflareToken != "" {
		return MethodCloudflare, nil
	}
	return "", ErrPort80InUse
}

func port80Free(addr string) bool {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

func newKey() (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func writeJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'), 0o600)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
