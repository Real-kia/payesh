package fleet

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Real-kia/payesh/internal/monitoring"
)

func TestSetupLoginAndProtectedRead(t *testing.T) {
	store, err := monitoring.OpenStore(context.Background(), ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api, err := NewAPI(store, "0123456789abcdef-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/servers", nil)
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated read=%d", response.Code)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/setup", bytes.NewBufferString(`{"secret":"0123456789abcdef-bootstrap","password":"long-enough-password"}`))
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("setup=%d", response.Code)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"password":"long-enough-password"}`))
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("login=%d", response.Code)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("no session cookie")
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/servers", nil)
	request.AddCookie(cookies[0])
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("authenticated read=%d", response.Code)
	}
	if cookies[0].Secure {
		t.Fatal("loopback HTTP session cookie unexpectedly requires TLS")
	}
}

func TestSecureCookieOption(t *testing.T) {
	store, err := monitoring.OpenStore(context.Background(), ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api, err := NewAPIWithOptions(store, "0123456789abcdef-bootstrap", Options{SecureCookies: true})
	if err != nil {
		t.Fatal(err)
	}
	setup := httptest.NewRequest(http.MethodPost, "/api/v1/setup", bytes.NewBufferString(`{"secret":"0123456789abcdef-bootstrap","password":"long-enough-password"}`))
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, setup)
	if response.Code != http.StatusCreated {
		t.Fatalf("setup=%d", response.Code)
	}
	login := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"password":"long-enough-password"}`))
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, login)
	if response.Code != http.StatusNoContent {
		t.Fatalf("login=%d", response.Code)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure {
		t.Fatalf("secure cookie option not applied: %+v", cookies)
	}
}

func TestTrustedProxyLoginKeyUsesValidatedForwardedClient(t *testing.T) {
	trusted, err := parseTrustedProxyNetworks([]string{"10.0.0.0/8", "2001:db8:ffff::1"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/session", nil)
	request.RemoteAddr = "10.1.2.3:443"
	request.Header.Set("X-Forwarded-For", "198.51.100.10, 10.2.3.4")
	if got := loginKeyWithTrustedProxies(request, trusted); got != "198.51.100.10" {
		t.Fatalf("forwarded client key=%q", got)
	}

	// An untrusted direct peer cannot spoof a forwarded address.
	request.RemoteAddr = "192.0.2.10:443"
	if got := loginKeyWithTrustedProxies(request, trusted); got != "192.0.2.10" {
		t.Fatalf("untrusted forwarded client key=%q", got)
	}

	request.RemoteAddr = "10.1.2.3:443"
	request.Header.Del("X-Forwarded-For")
	request.Header.Set("Forwarded", `for="[2001:db8::1]:8443";proto=https`)
	if got := loginKeyWithTrustedProxies(request, trusted); got != "2001:db8::1" {
		t.Fatalf("RFC 7239 client key=%q", got)
	}
	request.Header.Set("X-Forwarded-For", "not-an-ip, 198.51.100.10")
	request.Header.Del("Forwarded")
	if got := loginKeyWithTrustedProxies(request, trusted); got != "10.1.2.3" {
		t.Fatalf("malformed forwarded address was trusted: %q", got)
	}
	request.Header.Del("X-Forwarded-For")
	request.Header.Add("Forwarded", "for=203.0.113.77")
	request.Header.Add("Forwarded", "for=198.51.100.10")
	if got := loginKeyWithTrustedProxies(request, trusted); got != "198.51.100.10" {
		t.Fatalf("forwarded header chain was not evaluated from the trusted edge: %q", got)
	}
}

func TestTrustedProxyConfigurationRejectsInvalidAddresses(t *testing.T) {
	if _, err := parseTrustedProxyNetworks([]string{"proxy.example.invalid"}); err == nil {
		t.Fatal("hostname trusted proxy was accepted")
	}
	if _, err := parseTrustedProxyNetworks([]string{"10.0.0.0/99"}); err == nil {
		t.Fatal("invalid trusted proxy CIDR was accepted")
	}
}

func TestTrustedProxyThrottleSeparatesForwardedClients(t *testing.T) {
	store, err := monitoring.OpenStore(context.Background(), ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api, err := NewAPIWithOptions(store, "0123456789abcdef-bootstrap", Options{TrustedProxyCIDRs: []string{"10.0.0.0/8"}})
	if err != nil {
		t.Fatal(err)
	}
	setup := httptest.NewRequest(http.MethodPost, "/api/v1/setup", bytes.NewBufferString(`{"secret":"0123456789abcdef-bootstrap","password":"long-enough-password"}`))
	setup.RemoteAddr = "10.1.2.3:443"
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, setup)
	if response.Code != http.StatusCreated {
		t.Fatalf("setup=%d", response.Code)
	}
	for attempt := 0; attempt < 5; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"password":"wrong-password"}`))
		request.RemoteAddr = "10.1.2.3:443"
		request.Header.Set("X-Forwarded-For", "198.51.100.10")
		response := httptest.NewRecorder()
		api.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("failed attempt %d status=%d", attempt, response.Code)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"password":"wrong-password"}`))
	request.RemoteAddr = "10.1.2.3:443"
	request.Header.Set("X-Forwarded-For", "198.51.100.10")
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("forwarded client was not throttled: %d", response.Code)
	}

	// A distinct forwarded client remains independently eligible to attempt a
	// login even though the reverse proxy peer is shared.
	request = httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"password":"wrong-password"}`))
	request.RemoteAddr = "10.1.2.3:443"
	request.Header.Set("X-Forwarded-For", "198.51.100.11")
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("distinct forwarded client shared throttle: %d", response.Code)
	}
}
