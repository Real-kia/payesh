package webtls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeDomain(t *testing.T) {
	for input, want := range map[string]string{
		"Panel.Example.com":         "panel.example.com",
		" panel.example.com. ":      "panel.example.com",
		"https://panel.example.com": "panel.example.com",
		"a-b.c-d.example.co.uk":     "a-b.c-d.example.co.uk",
	} {
		got, err := NormalizeDomain(input)
		if err != nil || got != want {
			t.Errorf("NormalizeDomain(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "localhost", "*.example.com", "1.2.3.4", "exa mple.com", "-bad.example.com", "panel.example.com/path", "panel.example.com:8787"} {
		if _, err := NormalizeDomain(input); !errors.Is(err, ErrInvalidDomain) {
			t.Errorf("NormalizeDomain(%q) accepted an invalid domain", input)
		}
	}
}

// writeSelfSigned stores a certificate for domain as if it had been issued.
func writeSelfSigned(t *testing.T, dir, domain string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: domain}, DNSNames: []string{domain}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	must(t, os.WriteFile(filepath.Join(dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	must(t, writeJSON(filepath.Join(dir, "config.json"), Config{Domain: domain, Method: MethodHTTP01}))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func serve(t *testing.T, manager *Manager) string {
	t.Helper()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	_, port, _ := net.SplitHostPort(inner.Addr().String())
	manager.Port = port
	listener := NewListener(inner, manager)
	server := &http.Server{Handler: manager.RedirectToHTTPS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			_, _ = io.WriteString(w, "secure")
			return
		}
		_, _ = io.WriteString(w, "plain")
	}))}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return inner.Addr().String()
}

func noRedirect() *http.Client {
	return &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: "panel.example.com"}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func TestListenerServesPlainHTTPWithoutCertificate(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	must(t, err)
	addr := serve(t, manager)
	response, err := noRedirect().Get("http://" + addr + "/")
	must(t, err)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "plain" {
		t.Fatalf("body = %q, want plain HTTP response", body)
	}
	if _, err := noRedirect().Get("https://" + addr + "/"); err == nil {
		t.Fatal("HTTPS succeeded without a certificate")
	}
}

func TestListenerServesHTTPSAndRedirectsOnSamePort(t *testing.T) {
	dir := t.TempDir()
	writeSelfSigned(t, dir, "panel.example.com")
	manager, err := NewManager(dir)
	must(t, err)
	if status := manager.Status(); status.State != StateActive || status.ExpiresAt == nil {
		t.Fatalf("status = %+v, want active with expiry", status)
	}
	addr := serve(t, manager)
	_, port, _ := net.SplitHostPort(addr)

	response, err := noRedirect().Get("https://" + addr + "/")
	must(t, err)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "secure" {
		t.Fatalf("HTTPS body = %q", body)
	}

	response, err = noRedirect().Get("http://" + addr + "/servers?x=1")
	must(t, err)
	response.Body.Close()
	if want := "https://panel.example.com:" + port + "/servers?x=1"; response.StatusCode != http.StatusTemporaryRedirect || response.Header.Get("Location") != want {
		t.Fatalf("redirect = %d %q, want 307 %q", response.StatusCode, response.Header.Get("Location"), want)
	}

	for _, path := range []string{"/healthz", "/api/v1/settings/https"} {
		response, err = noRedirect().Get("http://" + addr + path)
		must(t, err)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s over HTTP = %d, want 200 (not redirected)", path, response.StatusCode)
		}
	}
	request, _ := http.NewRequest(http.MethodPut, "http://"+addr+"/api/v1/settings/https", strings.NewReader("{}"))
	response, err = noRedirect().Do(request)
	must(t, err)
	response.Body.Close()
	if response.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("PUT settings over HTTP = %d, want 307 to HTTPS", response.StatusCode)
	}
}

func TestLoadIgnoresCertificateForAnotherDomain(t *testing.T) {
	dir := t.TempDir()
	writeSelfSigned(t, dir, "old.example.com")
	must(t, writeJSON(filepath.Join(dir, "config.json"), Config{Domain: "new.example.com"}))
	manager, err := NewManager(dir)
	must(t, err)
	if manager.Active() {
		t.Fatal("served a certificate that does not match the configured domain")
	}
}

func TestDisableReturnsToHTTP(t *testing.T) {
	dir := t.TempDir()
	writeSelfSigned(t, dir, "panel.example.com")
	manager, err := NewManager(dir)
	must(t, err)
	must(t, manager.Disable())
	if manager.Active() || manager.Status().State != StateDisabled {
		t.Fatalf("still active after Disable: %+v", manager.Status())
	}
	if _, err := os.Stat(filepath.Join(dir, "key.pem")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("private key was not removed")
	}
}

func TestPort80InUseWithoutTokenExplainsDNSOption(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer busy.Close()
	manager, err := NewManager(t.TempDir())
	must(t, err)
	manager.HTTPChallengeAddr = busy.Addr().String()
	err = manager.Configure(context.Background(), Config{Domain: "panel.example.com"})
	if !errors.Is(err, ErrPort80InUse) {
		t.Fatalf("err = %v, want ErrPort80InUse", err)
	}
	if status := manager.Status(); status.State != StateFailed || !strings.Contains(status.Error, "Cloudflare") {
		t.Fatalf("status = %+v, want failed with Cloudflare guidance", status)
	}
}

func TestCloudflarePublishesAndRemovesTXT(t *testing.T) {
	var created, deleted bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"success":false,"errors":[{"message":"bad token"}]}`)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/zones":
			if r.URL.Query().Get("name") == "example.com" {
				_, _ = io.WriteString(w, `{"success":true,"result":[{"id":"zone1"}]}`)
			} else {
				_, _ = io.WriteString(w, `{"success":true,"result":[]}`)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/zones/zone1/dns_records":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["type"] != "TXT" || body["name"] != "_acme-challenge.panel.example.com" || body["content"] != "value" {
				t.Errorf("unexpected record %v", body)
			}
			created = true
			_, _ = io.WriteString(w, `{"success":true,"result":{"id":"rec1"}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/zones/zone1/dns_records/rec1":
			deleted = true
			_, _ = io.WriteString(w, `{"success":true,"result":{"id":"rec1"}}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}))
	defer api.Close()
	previous := cloudflareAPI
	cloudflareAPI = api.URL
	defer func() { cloudflareAPI = previous }()

	cleanup, err := cloudflare{token: "secret-token"}.publishTXT(context.Background(), "_acme-challenge.panel.example.com", "value")
	must(t, err)
	cleanup()
	if !created || !deleted {
		t.Fatalf("created=%t deleted=%t", created, deleted)
	}
	if _, err := (cloudflare{token: "wrong"}).publishTXT(context.Background(), "_acme-challenge.panel.example.com", "value"); err == nil || !strings.Contains(err.Error(), "bad token") {
		t.Fatalf("bad token error = %v", err)
	}
}

func TestSettingsHandler(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	must(t, err)
	// The accepted PUT starts a background order; keep it off the network.
	manager.DirectoryURL = "http://127.0.0.1:1/directory"
	manager.HTTPChallengeAddr = "127.0.0.1:0"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := manager.Handler(ctx)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/settings/https", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"state":"disabled"`) {
		t.Fatalf("GET = %d %s", recorder.Code, recorder.Body)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/api/v1/settings/https", strings.NewReader(`{"domain":"not a domain"}`)))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_domain") {
		t.Fatalf("PUT invalid = %d %s", recorder.Code, recorder.Body)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/api/v1/settings/https", strings.NewReader(`{"domain":"panel.example.com","cloudflare_api_token":"tok"}`)))
	if recorder.Code != http.StatusAccepted || strings.Contains(recorder.Body.String(), "tok") {
		t.Fatalf("PUT = %d %s (token must never be echoed)", recorder.Code, recorder.Body)
	}
	// Let the background order fail against the dead directory before the
	// temporary state directory is removed.
	deadline := time.Now().Add(10 * time.Second)
	for manager.busy.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if status := manager.Status(); status.State != StateFailed || strings.Contains(status.Error, "tok") {
		t.Fatalf("status after background order = %+v", status)
	}
}
