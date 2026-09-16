// Package fleet supplies the web-role boundary: bootstrap setup, owner login,
// logout, and authenticated access to the bounded monitoring read API.
package fleet

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/Real-kia/payesh/internal/alerts"
	"github.com/Real-kia/payesh/internal/auth"
	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/modules"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/traffic"
	"github.com/Real-kia/payesh/internal/transport"
	"github.com/Real-kia/payesh/internal/updater"
)

const maxBodyBytes = 1 << 20

type API struct {
	sessions   *auth.Manager
	store      *monitoring.Store
	monitoring http.Handler
	// secureCookies is false only for the explicitly loopback-bound HTTP
	// mode. Public deployments should construct the API with true behind TLS.
	secureCookies   bool
	trustedProxies  []*net.IPNet
	alerts          http.Handler
	traffic         http.Handler
	modules         http.Handler
	cpuControl      http.Handler
	bandwidth       http.Handler
	portTraffic     http.Handler
	jobs            http.Handler
	updates         http.Handler
	install         http.Handler
	enrollment      http.Handler
	enrollmentToken http.Handler
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
	// ModuleService optionally owns the catalog and the per-server
	// install/enable/disable/remove lifecycle. Routes are unavailable (fall
	// through to the monitoring 404) when this is nil.
	ModuleService *modules.Service
	// CPUControlService optionally owns cpu-controls preview/apply/revert.
	// Routes are unavailable (fall through to the monitoring 404) when nil.
	CPUControlService interface{ Handler() http.Handler }
	// BandwidthService owns Package 09 preview/apply/revert routes.
	BandwidthService interface{ Handler() http.Handler }
	// PortTrafficService owns durable Package 07 scope configuration routes.
	// The service only persists pending desired scopes; nftables activation
	// remains a privileged module operation.
	PortTrafficService interface{ Handler() http.Handler }
	// EnrollmentAuthority enables the authenticated pairing-token enrollment
	// job producer. The authority remains CA-owned and never exposes its key.
	EnrollmentAuthority *transport.CertificateAuthority
	// UpdateScheduler enables the authenticated, durable update producer. The
	// scheduler's executor is intentionally owned by the worker process.
	UpdateScheduler *updater.Scheduler
	// InstallService enables the authenticated durable SSH-install producer.
	// Its credential handoff remains process-memory only.
	InstallService *InstallService
}

func NewAPIWithOptions(store *monitoring.Store, setupSecret string, options Options) (*API, error) {
	trustedProxies, err := parseTrustedProxyNetworks(append(append([]string{}, options.TrustedProxyCIDRs...), options.TrustedProxies...))
	if err != nil {
		return nil, err
	}
	sessions, err := auth.NewPersistent(setupSecret, store)
	if err != nil {
		return nil, err
	}
	// Installers may provide one-time generated owner credentials. Initialize
	// them on first boot; the persisted auth record prevents reinitialization.
	if !sessions.Configured() {
		if username, password := strings.TrimSpace(os.Getenv("PAYESH_OWNER_USERNAME")), os.Getenv("PAYESH_OWNER_PASSWORD"); username != "" && password != "" {
			if err := sessions.SetupWithUsername(setupSecret, username, password); err != nil {
				return nil, fmt.Errorf("initialize generated owner: %w", err)
			}
		}
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
	var moduleHandler http.Handler
	if options.ModuleService != nil {
		moduleHandler = sessions.Middleware(options.ModuleService.Handler())
	}
	var cpuControlHandler http.Handler
	if options.CPUControlService != nil {
		cpuControlHandler = sessions.Middleware(options.CPUControlService.Handler())
	}
	var bandwidthHandler http.Handler
	if options.BandwidthService != nil {
		bandwidthHandler = sessions.Middleware(options.BandwidthService.Handler())
	}
	var portTrafficHandler http.Handler
	if options.PortTrafficService != nil {
		portTrafficHandler = sessions.Middleware(options.PortTrafficService.Handler())
	}
	var enrollmentHandler http.Handler
	var enrollmentTokenHandler http.Handler
	if options.EnrollmentAuthority != nil {
		enrollmentService, enrollmentErr := NewEnrollmentService(store, options.EnrollmentAuthority)
		if enrollmentErr != nil {
			return nil, enrollmentErr
		}
		enrollmentHandler = sessions.Middleware(enrollmentService.Handler())
		enrollmentTokenHandler = sessions.Middleware(enrollmentService.TokenHandler())
	}
	var updateHandler http.Handler
	if options.UpdateScheduler != nil {
		updateService, updateErr := NewUpdateService(store, options.UpdateScheduler)
		if updateErr != nil {
			return nil, updateErr
		}
		updateHandler = sessions.Middleware(updateService.Handler())
	}
	var installHandler http.Handler
	if options.InstallService != nil {
		if options.InstallService.Store != store {
			return nil, errors.New("install service store does not match API store")
		}
		installHandler = sessions.Middleware(options.InstallService.Handler())
	}
	return &API{sessions: sessions, store: store, monitoring: sessions.Middleware(readAPI.Handler()), alerts: alertHandler, traffic: trafficHandler, modules: moduleHandler, cpuControl: cpuControlHandler, bandwidth: bandwidthHandler, portTraffic: portTrafficHandler, jobs: sessions.Middleware(newJobHTTP(store)), updates: updateHandler, install: installHandler, enrollment: enrollmentHandler, enrollmentToken: enrollmentTokenHandler, secureCookies: options.SecureCookies, trustedProxies: trustedProxies}, nil
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
	case "/api/v1/servers":
		if r.Method == http.MethodPost {
			a.sessions.Middleware(http.HandlerFunc(a.createPendingServer)).ServeHTTP(w, r)
			return
		}
	}
	if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/servers/") {
		a.sessions.Middleware(http.HandlerFunc(a.deleteServer)).ServeHTTP(w, r)
		return
	}
	if a.alerts != nil && (isFleetResourcePath(r.URL.Path, "/api/v1/alerts") || isFleetResourcePath(r.URL.Path, "/api/v1/maintenance-windows") || strings.HasPrefix(r.URL.Path, "/api/v1/incidents/")) {
		a.alerts.ServeHTTP(w, r)
		return
	}
	trimmedPath := strings.TrimRight(r.URL.Path, "/")
	if a.updates != nil && trimmedPath == "/api/v1/updates" {
		a.updates.ServeHTTP(w, r)
		return
	}
	if a.install != nil && (trimmedPath == "/api/v1/installations" || (strings.HasPrefix(trimmedPath, "/api/v1/servers/") && strings.HasSuffix(trimmedPath, "/install"))) {
		a.install.ServeHTTP(w, r)
		return
	}
	if strings.HasPrefix(trimmedPath, "/api/v1/jobs/") {
		a.jobs.ServeHTTP(w, r)
		return
	}
	if a.enrollment != nil && strings.HasPrefix(trimmedPath, "/api/v1/servers/") && strings.HasSuffix(trimmedPath, "/enrollment") {
		a.enrollment.ServeHTTP(w, r)
		return
	}
	if a.enrollmentToken != nil && strings.HasPrefix(trimmedPath, "/api/v1/servers/") && strings.HasSuffix(trimmedPath, "/enrollment-token") {
		a.enrollmentToken.ServeHTTP(w, r)
		return
	}
	if a.traffic != nil && strings.HasPrefix(trimmedPath, "/api/v1/servers/") && (strings.HasSuffix(trimmedPath, "/traffic") || strings.HasSuffix(trimmedPath, "/traffic/forecast")) {
		a.traffic.ServeHTTP(w, r)
		return
	}
	if a.modules != nil && (trimmedPath == "/api/v1/modules" || (strings.HasPrefix(trimmedPath, "/api/v1/servers/") && strings.Contains(trimmedPath, "/modules"))) {
		a.modules.ServeHTTP(w, r)
		return
	}
	if a.cpuControl != nil && strings.HasPrefix(trimmedPath, "/api/v1/servers/") && strings.Contains(trimmedPath, "/cpu-policies") {
		a.cpuControl.ServeHTTP(w, r)
		return
	}
	if a.bandwidth != nil && strings.HasPrefix(trimmedPath, "/api/v1/servers/") && strings.Contains(trimmedPath, "/bandwidth-policies") {
		a.bandwidth.ServeHTTP(w, r)
		return
	}
	if a.portTraffic != nil && strings.HasPrefix(trimmedPath, "/api/v1/servers/") && strings.Contains(trimmedPath, "/port-traffic-scopes") {
		a.portTraffic.ServeHTTP(w, r)
		return
	}
	a.monitoring.ServeHTTP(w, r)
}

type deleteServerRequest struct {
	ExpectedRevision uint64 `json:"expected_revision,string"`
}

func (a *API) deleteServer(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/servers/"), "/")
	if strings.Contains(id, "/") || len(id) < 16 || len(id) > 128 {
		writeFleetError(w, http.StatusNotFound, "not_found", "server not found", false)
		return
	}
	var request deleteServerRequest
	if !decode(w, r, &request) {
		return
	}
	err := a.store.DeleteServer(r.Context(), contracts.ServerID(id), request.ExpectedRevision)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, sql.ErrNoRows):
		writeFleetError(w, http.StatusNotFound, "not_found", "server not found", false)
	case err.Error() == "configuration_revision_conflict":
		writeFleetError(w, http.StatusConflict, "revision_conflict", "server changed; reload before deleting", false)
	case err.Error() == "server_role_not_deletable":
		writeFleetError(w, http.StatusConflict, "server_role_not_deletable", "the local hub or standalone server cannot be deleted here", false)
	default:
		writeFleetError(w, http.StatusInternalServerError, "storage_error", "could not delete server", true)
	}
}

type createServerRequest struct {
	Name         string `json:"name"`
	Address      string `json:"address"`
	Platform     string `json:"platform"`
	Architecture string `json:"architecture"`
}

func (a *API) createPendingServer(w http.ResponseWriter, r *http.Request) {
	var request createServerRequest
	if !decode(w, r, &request) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	request.Address = strings.TrimSpace(request.Address)
	if request.Name == "" || len(request.Name) > 128 {
		writeFleetError(w, http.StatusBadRequest, "invalid_request", "server name must contain 1..128 characters", false)
		return
	}
	if request.Address == "" || len(request.Address) > 255 {
		writeFleetError(w, http.StatusBadRequest, "invalid_request", "server address must contain 1..255 characters", false)
		return
	}
	if request.Platform == "" {
		request.Platform = "unknown"
	}
	if request.Architecture == "" {
		request.Architecture = "unknown"
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		writeFleetError(w, http.StatusInternalServerError, "internal_error", "could not create server identity", true)
		return
	}
	server := contracts.Server{ID: contracts.ServerID("server-" + hex.EncodeToString(random)), Name: request.Name, Address: request.Address, Role: "node", Platform: request.Platform, Architecture: request.Architecture, Capabilities: []string{"metrics", "traffic"}, ConnectionState: "never-connected", FreshnessState: "unknown", FreshnessReason: "awaiting automatic platform detection"}
	if err := a.store.EnsureServer(r.Context(), server); err != nil {
		writeFleetError(w, http.StatusInternalServerError, "storage_error", "could not create server", true)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(server)
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
	Username string `json:"username"`
}
type loginRequest struct {
	Username string `json:"username"`
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
	username := request.Username
	if username == "" {
		username = "admin"
	}
	if err := a.sessions.SetupWithUsername(secret, username, request.Password); err != nil {
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
	username := request.Username
	if username == "" {
		username = "admin"
	}
	session, csrf, err := a.sessions.LoginWithUsername(a.loginKey(r), username, request.Password)
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
	if err := a.sessions.Logout(cookie.Value); err != nil {
		writeFleetError(w, http.StatusServiceUnavailable, "auth_storage_unavailable", "authentication storage unavailable", true)
		return
	}
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
