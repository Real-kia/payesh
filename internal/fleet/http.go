// Package fleet supplies the web-role boundary: bootstrap setup, owner login,
// logout, and authenticated access to the bounded monitoring read API.
package fleet

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/Real-kia/payesh/internal/alerts"
	"github.com/Real-kia/payesh/internal/auth"
	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/traffic"
)

const maxBodyBytes = 1 << 20

type API struct {
	sessions   *auth.Manager
	monitoring http.Handler
	// secureCookies is false only for the explicitly loopback-bound HTTP
	// mode. Public deployments should construct the API with true behind TLS.
	secureCookies  bool
	trustedProxies []*net.IPNet
	alerts         http.Handler
	traffic        http.Handler
}

func NewAPI(store *monitoring.Store, setupSecret string) (*API, error) {
	return NewAPIWithOptions(store, setupSecret, Options{})
}

// Options controls deployment-sensitive browser cookie behavior. The default
// is safe for the server's loopback HTTP listener; set SecureCookies when the
// listener is reached through HTTPS (directly or via a TLS reverse proxy).
type Options struct {
	SecureCookies bool
	// TrustedProxyCIDRs identifies the reverse proxies allowed to supply
	// Forwarded/X-Forwarded-For client addresses. Forwarded headers are ignored
	// unless the immediate peer is inside one of these networks.
	TrustedProxyCIDRs []string
	// TrustedProxies is kept as a readable alias for callers that configure
	// individual proxy addresses rather than CIDR ranges. Both fields are
	// validated and combined.
	TrustedProxies []string
	// AlertService is optional for callers that only expose package-03 reads.
	// When supplied, its routes are wrapped in the same browser session/CSRF
	// boundary as the monitoring API.
	AlertService *alerts.Service
	// TrafficService optionally owns allowance configuration and the same
	// bounded period reads. GET remains available through package-03 when this
	// is nil; production browser mode supplies the package-05 service so POST
	// configuration has a real owner.
	TrafficService *traffic.Service
}

func NewAPIWithOptions(store *monitoring.Store, setupSecret string, options Options) (*API, error) {
	trustedProxies, err := parseTrustedProxyNetworks(append(append([]string{}, options.TrustedProxyCIDRs...), options.TrustedProxies...))
	if err != nil {
		return nil, err
	}
	sessions, err := auth.New(setupSecret)
	if err != nil {
		return nil, err
	}
	readAPI, err := monitoring.NewAPIWithAuthorizer(store, func(http.ResponseWriter, *http.Request) bool { return true })
	if err != nil {
		return nil, err
	}
	var alertHandler http.Handler
	if options.AlertService != nil {
		alertHandler = sessions.Middleware(options.AlertService.Handler())
	}
	var trafficHandler http.Handler
	if options.TrafficService != nil {
		trafficHandler = sessions.Middleware(options.TrafficService.Handler())
	}
	return &API{sessions: sessions, monitoring: sessions.Middleware(readAPI.Handler()), alerts: alertHandler, traffic: trafficHandler, secureCookies: options.SecureCookies, trustedProxies: trustedProxies}, nil
}

func (a *API) Handler() http.Handler { return http.HandlerFunc(a.serveHTTP) }
func (a *API) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		a.monitoring.ServeHTTP(w, r)
		return
	}
	switch r.URL.Path {
	case "/api/v1/setup":
		if r.Method == http.MethodPost {
			a.setup(w, r)
			return
		}
	case "/api/v1/session":
		if r.Method == http.MethodPost {
			a.login(w, r)
			return
		}
		if r.Method == http.MethodDelete {
			a.logout(w, r)
			return
		}
	}
	if a.alerts != nil && (isFleetResourcePath(r.URL.Path, "/api/v1/alerts") || isFleetResourcePath(r.URL.Path, "/api/v1/maintenance-windows") || strings.HasPrefix(r.URL.Path, "/api/v1/incidents/")) {
		a.alerts.ServeHTTP(w, r)
		return
	}
	trimmedPath := strings.TrimRight(r.URL.Path, "/")
	if a.traffic != nil && strings.HasPrefix(trimmedPath, "/api/v1/servers/") && (strings.HasSuffix(trimmedPath, "/traffic") || strings.HasSuffix(trimmedPath, "/traffic/forecast")) {
		a.traffic.ServeHTTP(w, r)
		return
	}
	a.monitoring.ServeHTTP(w, r)
}

func isFleetResourcePath(path, base string) bool {
	return path == base || strings.HasPrefix(path, base+"/")
}

type setupRequest struct {
	SetupSecret string `json:"setup_secret"`
	// Secret is accepted for compatibility with the pre-OpenAPI checkpoint;
	// new clients must use setup_secret.
	Secret   string `json:"secret,omitempty"`
	Password string `json:"password"`
}
type loginRequest struct {
	Password string `json:"password"`
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeFleetError(w, http.StatusBadRequest, "invalid_request", "invalid request", false)
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeFleetError(w, http.StatusBadRequest, "invalid_request", "invalid request", false)
		return false
	}
	return true
}
func (a *API) setup(w http.ResponseWriter, r *http.Request) {
	var request setupRequest
	if !decode(w, r, &request) {
		return
	}
	secret := request.SetupSecret
	if secret == "" {
		secret = request.Secret
	}
	if err := a.sessions.Setup(secret, request.Password); err != nil {
		if err.Error() == "setup_already_complete" {
			writeFleetError(w, http.StatusConflict, "setup_already_complete", "setup already complete", false)
		} else {
			writeFleetError(w, http.StatusBadRequest, "setup_rejected", "setup rejected", false)
		}
		return
	}
	w.WriteHeader(http.StatusCreated)
}
func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var request loginRequest
	if !decode(w, r, &request) {
		return
	}
	session, csrf, err := a.sessions.Login(a.loginKey(r), request.Password)
	if err != nil {
		if err.Error() == "auth.rate_limited" {
			w.Header().Set("Retry-After", "900")
			writeFleetError(w, http.StatusTooManyRequests, "auth.rate_limited", "too many login attempts", true)
		} else {
			writeFleetError(w, http.StatusUnauthorized, "invalid_credentials", "invalid credentials", false)
		}
		return
	}
	a.sessions.SetSessionCookieWithSecurity(w, session, a.secureCookies || r.TLS != nil)
	w.Header().Set("X-CSRF-Token", csrf)
	w.WriteHeader(http.StatusNoContent)
}
func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(a.sessions.SessionCookieName())
	if err != nil {
		writeFleetError(w, http.StatusUnauthorized, "unauthorized", "authentication required", true)
		return
	}
	csrf, ok := a.sessions.Validate(cookie.Value)
	if !ok || r.Header.Get("X-CSRF-Token") == "" || !constantEqual(csrf, r.Header.Get("X-CSRF-Token")) {
		writeFleetError(w, http.StatusForbidden, "csrf_required", "csrf token required", false)
		return
	}
	a.sessions.Logout(cookie.Value)
	a.sessions.ClearSessionCookie(w, a.secureCookies || r.TLS != nil)
	w.WriteHeader(http.StatusNoContent)
}
func (a *API) loginKey(r *http.Request) string {
	return loginKeyWithTrustedProxies(r, a.trustedProxies)
}

func loginKey(r *http.Request) string {
	return loginKeyWithTrustedProxies(r, nil)
}

func loginKeyWithTrustedProxies(r *http.Request, trustedProxies []*net.IPNet) string {
	remote := strings.TrimSpace(r.RemoteAddr)
	if peer, ok := parseIPAddress(remote); ok && ipInNetworks(peer, trustedProxies) {
		if forwarded := forwardedClientKey(r.Header, trustedProxies); forwarded != "" {
			return forwarded
		}
	}
	if peer, ok := parseIPAddress(remote); ok {
		return peer.String()
	}
	return remote
}

func parseTrustedProxyNetworks(values []string) ([]*net.IPNet, error) {
	networks := make([]*net.IPNet, 0, len(values))
	for _, raw := range values {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return nil, errors.New("trusted proxy address must not be empty")
		}
		if host, _, err := net.SplitHostPort(raw); err == nil {
			raw = host
		}
		if strings.Contains(raw, "/") {
			_, network, err := net.ParseCIDR(raw)
			if err != nil {
				return nil, errors.New("invalid trusted proxy CIDR")
			}
			networks = append(networks, network)
			continue
		}
		ip := net.ParseIP(strings.Trim(raw, "[]"))
		if ip == nil {
			return nil, errors.New("invalid trusted proxy address")
		}
		bits := 128
		if ip4 := ip.To4(); ip4 != nil {
			ip = ip4
			bits = 32
		}
		networks = append(networks, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return networks, nil
}

func parseIPAddress(value string) (net.IP, bool) {
	value = strings.TrimSpace(value)
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	value = strings.Trim(value, "[]")
	ip := net.ParseIP(value)
	return ip, ip != nil
}

func ipInNetworks(ip net.IP, networks []*net.IPNet) bool {
	for _, network := range networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func forwardedClientKey(header http.Header, trustedProxies []*net.IPNet) string {
	var forwardedCandidates []net.IP
	forwardedValid := true
	for _, value := range header.Values("Forwarded") {
		candidates := parseForwardedHeader(value)
		if len(candidates) == 0 {
			forwardedValid = false
			break
		}
		forwardedCandidates = append(forwardedCandidates, candidates...)
	}
	if forwardedValid && len(forwardedCandidates) > 0 {
		if client := firstUntrustedForwarded(forwardedCandidates, trustedProxies); client != "" {
			return client
		}
	}

	var xForwardedCandidates []net.IP
	xForwardedValid := true
	for _, value := range header.Values("X-Forwarded-For") {
		candidates := parseAddressList(value)
		if len(candidates) == 0 {
			xForwardedValid = false
			break
		}
		xForwardedCandidates = append(xForwardedCandidates, candidates...)
	}
	if xForwardedValid && len(xForwardedCandidates) > 0 {
		if client := firstUntrustedForwarded(xForwardedCandidates, trustedProxies); client != "" {
			return client
		}
	}
	for _, value := range header.Values("X-Real-IP") {
		if ip, ok := parseIPAddress(value); ok {
			return ip.String()
		}
	}
	return ""
}

func parseForwardedHeader(value string) []net.IP {
	var candidates []net.IP
	for _, element := range strings.Split(value, ",") {
		foundFor := false
		for _, parameter := range strings.Split(element, ";") {
			key, raw, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(key), "for") {
				continue
			}
			foundFor = true
			if ip, ok := parseForwardedAddress(raw); ok {
				candidates = append(candidates, ip)
			} else {
				return nil
			}
			break
		}
		if !foundFor {
			return nil
		}
	}
	return candidates
}

func parseAddressList(value string) []net.IP {
	var candidates []net.IP
	for _, raw := range strings.Split(value, ",") {
		if ip, ok := parseForwardedAddress(raw); ok {
			candidates = append(candidates, ip)
		} else {
			return nil
		}
	}
	return candidates
}

func parseForwardedAddress(raw string) (net.IP, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		raw = strings.TrimSpace(raw[1 : len(raw)-1])
	}
	if raw == "" || strings.HasPrefix(raw, "_") || strings.EqualFold(raw, "unknown") {
		return nil, false
	}
	return parseIPAddress(raw)
}

func firstUntrustedForwarded(candidates []net.IP, trustedProxies []*net.IPNet) string {
	for index := len(candidates) - 1; index >= 0; index-- {
		if !ipInNetworks(candidates[index], trustedProxies) {
			return candidates[index].String()
		}
	}
	if len(candidates) > 0 {
		return candidates[0].String()
	}
	return ""
}
func constantEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var value byte
	for i := range a {
		value |= a[i] ^ b[i]
	}
	return value == 0
}

func writeFleetError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	data, err := json.Marshal(contracts.Error{Code: code, Message: message, Retryable: retryable})
	if err != nil {
		data = []byte(`{"code":"internal_error","message":"request failed","retryable":false}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
