package webtls

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
)

// obtain runs one ACME order for cfg.Domain and returns PEM chain and key.
func (m *Manager) obtain(ctx context.Context, cfg Config) ([]byte, []byte, error) {
	accountKey, err := m.accountKey()
	if err != nil {
		return nil, nil, err
	}
	client := &acme.Client{Key: accountKey, DirectoryURL: m.DirectoryURL, UserAgent: "payesh"}
	account := &acme.Account{}
	if cfg.Email != "" {
		account.Contact = []string{"mailto:" + cfg.Email}
	}
	if _, err := client.Register(ctx, account, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, nil, fmt.Errorf("register with Let's Encrypt: %w", err)
	}
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(cfg.Domain))
	if err != nil {
		return nil, nil, fmt.Errorf("start certificate order: %w", explain(err))
	}
	for _, authzURL := range order.AuthzURLs {
		authz, err := client.GetAuthorization(ctx, authzURL)
		if err != nil {
			return nil, nil, err
		}
		if authz.Status == acme.StatusValid {
			continue
		}
		if err := m.solve(ctx, client, authz, cfg); err != nil {
			return nil, nil, err
		}
	}
	// Order polling responses carry no Location header, so keep the URL.
	orderURL := order.URI
	order, err = client.WaitOrder(ctx, orderURL)
	if err != nil {
		return nil, nil, fmt.Errorf("wait for certificate order: %w", explain(err))
	}
	key, keyPEM, err := newKey()
	if err != nil {
		return nil, nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: cfg.Domain}, DNSNames: []string{cfg.Domain}}, key)
	if err != nil {
		return nil, nil, err
	}
	chain, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		// CreateOrderCert follows the finalize response's Location header to
		// wait for issuance; CAs that omit it still finish the order, so poll
		// the known order URL and download the certificate directly.
		finished, waitErr := client.WaitOrder(ctx, orderURL)
		if waitErr != nil || finished.Status != acme.StatusValid || finished.CertURL == "" {
			return nil, nil, fmt.Errorf("finalize certificate: %w", explain(err))
		}
		if chain, err = client.FetchCert(ctx, finished.CertURL, true); err != nil {
			return nil, nil, fmt.Errorf("download certificate: %w", explain(err))
		}
	}
	var certPEM []byte
	for _, der := range chain {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	return certPEM, keyPEM, nil
}

func (m *Manager) solve(ctx context.Context, client *acme.Client, authz *acme.Authorization, cfg Config) error {
	want := "http-01"
	if cfg.Method == MethodCloudflare {
		want = "dns-01"
	}
	var challenge *acme.Challenge
	for _, candidate := range authz.Challenges {
		if candidate.Type == want {
			challenge = candidate
		}
	}
	if challenge == nil {
		return fmt.Errorf("Let's Encrypt did not offer a %s challenge", want)
	}
	switch want {
	case "http-01":
		stop, err := m.serveHTTPChallenge(client, challenge.Token)
		if err != nil {
			return err
		}
		defer stop()
	case "dns-01":
		value, err := client.DNS01ChallengeRecord(challenge.Token)
		if err != nil {
			return err
		}
		cf := cloudflare{token: cfg.CloudflareToken}
		cleanup, err := cf.publishTXT(ctx, "_acme-challenge."+cfg.Domain, value)
		if err != nil {
			return fmt.Errorf("Cloudflare DNS: %w", err)
		}
		defer cleanup()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.DNSPropagationDelay):
		}
	}
	if _, err := client.Accept(ctx, challenge); err != nil {
		return fmt.Errorf("start domain check: %w", explain(err))
	}
	if _, err := client.WaitAuthorization(ctx, authz.URI); err != nil {
		return fmt.Errorf("domain check for %s failed: %w", cfg.Domain, explain(err))
	}
	return nil
}

// serveHTTPChallenge answers only the ACME challenge path on port 80 and
// stops as soon as the authorization completes.
func (m *Manager) serveHTTPChallenge(client *acme.Client, token string) (func(), error) {
	response, err := client.HTTP01ChallengeResponse(token)
	if err != nil {
		return nil, err
	}
	path := client.HTTP01ChallengePath(token)
	listener, err := net.Listen("tcp", m.HTTPChallengeAddr)
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrPort80InUse, err)
	}
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != path {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(response))
		}),
	}
	go func() { _ = server.Serve(listener) }()
	return func() { _ = server.Close() }, nil
}

func (m *Manager) accountKey() (crypto.Signer, error) {
	path := m.path("account.key")
	if b, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(b)
		if block == nil {
			return nil, errors.New("ACME account key is not PEM")
		}
		return x509.ParseECPrivateKey(block.Bytes)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, keyPEM, err := newKey()
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, keyPEM, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// explain turns common ACME problems into operator guidance.
func explain(err error) error {
	var acmeErr *acme.Error
	if errors.As(err, &acmeErr) {
		detail := acmeErr.Detail
		switch {
		case strings.Contains(acmeErr.ProblemType, "dns"):
			return fmt.Errorf("%s (check that the domain's DNS record points to this server)", detail)
		case strings.Contains(acmeErr.ProblemType, "connection"), strings.Contains(acmeErr.ProblemType, "unauthorized"):
			return fmt.Errorf("%s (check that the domain points to this server and port 80 is reachable from the internet)", detail)
		case strings.Contains(acmeErr.ProblemType, "rateLimited"):
			return fmt.Errorf("%s (Let's Encrypt rate limit; try again later)", detail)
		}
		if detail != "" {
			return errors.New(detail)
		}
	}
	var authzErr *acme.AuthorizationError
	if errors.As(err, &authzErr) && len(authzErr.Errors) > 0 {
		return explain(authzErr.Errors[0])
	}
	return err
}
