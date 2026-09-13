package fleet

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/porttraffic"
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

func TestAuthenticatedOwnerCreatesPendingServer(t *testing.T) {
	store, err := monitoring.OpenStore(context.Background(), ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api, err := NewAPI(store, "0123456789abcdef-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	setup := httptest.NewRequest(http.MethodPost, "/api/v1/setup", bytes.NewBufferString(`{"setup_secret":"0123456789abcdef-bootstrap","username":"owner","password":"long-enough-password"}`))
	setup.Header.Set("Content-Type", "application/json")
	setupResponse := httptest.NewRecorder()
	api.Handler().ServeHTTP(setupResponse, setup)
	login := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"username":"owner","password":"long-enough-password"}`))
	login.Header.Set("Content-Type", "application/json")
	loginResponse := httptest.NewRecorder()
	api.Handler().ServeHTTP(loginResponse, login)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/servers", bytes.NewBufferString(`{"name":"Edge node","platform":"linux","architecture":"arm64"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", loginResponse.Header().Get("X-CSRF-Token"))
	request.AddCookie(loginResponse.Result().Cookies()[0])
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create pending server=%d body=%s", response.Code, response.Body.String())
	}
}

func TestOwnerAndSessionSurviveStoreReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "payesh.db")
	store, err := monitoring.OpenStore(ctx, path, monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	api, err := NewAPI(store, "0123456789abcdef-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	setup := httptest.NewRequest(http.MethodPost, "/api/v1/setup", bytes.NewBufferString(`{"setup_secret":"0123456789abcdef-bootstrap","password":"long-enough-password"}`))
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
	cookie := response.Result().Cookies()[0]
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = monitoring.OpenStore(ctx, path, monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	restarted, err := NewAPI(store, "different-secret-still-long-enough")
	if err != nil {
		t.Fatal(err)
	}
	repeatedSetup := httptest.NewRequest(http.MethodPost, "/api/v1/setup", bytes.NewBufferString(`{"setup_secret":"different-secret-still-long-enough","password":"replacement-password"}`))
	response = httptest.NewRecorder()
	restarted.Handler().ServeHTTP(response, repeatedSetup)
	if response.Code != http.StatusConflict {
		t.Fatalf("setup after restart=%d", response.Code)
	}
	read := httptest.NewRequest(http.MethodGet, "/api/v1/servers", nil)
	read.AddCookie(cookie)
	response = httptest.NewRecorder()
	restarted.Handler().ServeHTTP(response, read)
	if response.Code != http.StatusOK {
		t.Fatalf("restored session read=%d", response.Code)
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

func TestAuthenticatedPortTrafficScopeRouteUsesCSRFAndDurableCAS(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-port-api-0001")
	if err := store.UpsertServer(ctx, contracts.Server{ID: serverID, Name: "local", Role: "standalone", Platform: "linux", Architecture: "amd64", ConnectionState: "connected", FreshnessState: "fresh"}); err != nil {
		t.Fatal(err)
	}
	api, err := NewAPIWithOptions(store, "0123456789abcdef-bootstrap", Options{PortTrafficService: porttraffic.NewService(&porttraffic.Manager{Store: store})})
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
	cookie := response.Result().Cookies()[0]
	csrf := response.Header().Get("X-CSRF-Token")
	body := `{"idempotency_key":"scope-1","expected_revision":"0","scope":{"id":"web","protocol":"tcp","interface":"eth0","local_port":443,"direction":"inbound","tuple":"translated"}}`
	path := "/api/v1/servers/" + string(serverID) + "/port-traffic-scopes"
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", csrf)
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"state":"pending"`)) {
		t.Fatalf("scope route status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("scope route without CSRF=%d body=%s", response.Code, response.Body.String())
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
