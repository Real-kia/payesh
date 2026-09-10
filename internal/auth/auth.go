// Package auth provides the small, stateful authentication boundary used by
// the web role. It deliberately keeps credentials out of request handlers and
// stores only password/session digests in memory; a later persistence adapter
// can serialize the same records without changing the security semantics.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	passwordIterations = 210000
	sessionTTL         = 12 * time.Hour
	maxLoginFailures   = 5
	loginWindow        = 15 * time.Minute
	// Failed-login keys are attacker-controlled (normally source addresses),
	// so retain only a bounded number even when every request uses a new key.
	maxThrottleEntries = 4096
)

type passwordRecord struct{ salt, digest []byte }
type sessionRecord struct {
	digest  []byte
	expires time.Time
	csrf    string
}
type throttleRecord struct {
	failures     int
	since        time.Time
	blockedUntil time.Time
}

// Manager owns the one-owner account and browser sessions. It is safe for
// concurrent use. No cleartext password, setup secret, or session token is
// retained after the operation that consumes it.
type Manager struct {
	mu          sync.Mutex
	owner       passwordRecord
	configured  bool
	setupDigest []byte
	sessions    map[string]sessionRecord // keyed by a SHA-256 session digest
	throttle    map[string]throttleRecord
	now         func() time.Time
}

func New(setupSecret string) (*Manager, error) {
	if len(setupSecret) < 16 {
		return nil, errors.New("setup secret must contain at least 16 characters")
	}
	d := sha256.Sum256([]byte(setupSecret))
	return &Manager{setupDigest: d[:], sessions: make(map[string]sessionRecord), throttle: make(map[string]throttleRecord), now: time.Now}, nil
}

// Setup consumes the one-time bootstrap secret and creates the sole owner.
func (m *Manager) Setup(secret, password string) error {
	if len(password) < 12 || len(password) > 256 {
		return errors.New("password must contain 12..256 characters")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.configured {
		return errors.New("setup_already_complete")
	}
	d := sha256.Sum256([]byte(secret))
	if subtle.ConstantTimeCompare(d[:], m.setupDigest) != 1 {
		return errors.New("invalid_setup_secret")
	}
	r, err := newPasswordRecord(password)
	if err != nil {
		return err
	}
	m.owner, m.configured = r, true
	for i := range m.setupDigest {
		m.setupDigest[i] = 0
	}
	return nil
}

func newPasswordRecord(password string) (passwordRecord, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return passwordRecord{}, err
	}
	return passwordRecord{salt: salt, digest: pbkdf2SHA256([]byte(password), salt, passwordIterations, 32)}, nil
}

func verifyPassword(r passwordRecord, password string) bool {
	d := pbkdf2SHA256([]byte(password), r.salt, passwordIterations, len(r.digest))
	return subtle.ConstantTimeCompare(d, r.digest) == 1
}

// Login authenticates the owner and returns an opaque session and CSRF token.
// Failed attempts are throttled by caller-supplied key (normally remote IP).
func (m *Manager) Login(key, password string) (session, csrf string, err error) {
	if len(key) == 0 || len(key) > 128 {
		return "", "", errors.New("invalid_login_key")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	m.pruneExpiredThrottle(now)
	t := m.throttle[key]
	if !t.blockedUntil.IsZero() && now.Before(t.blockedUntil) {
		return "", "", fmt.Errorf("auth.rate_limited")
	}
	ok := m.configured && verifyPassword(m.owner, password)
	if !ok {
		if t.since.IsZero() || now.Sub(t.since) >= loginWindow {
			t = throttleRecord{since: now}
		}
		t.failures++
		if t.failures >= maxLoginFailures {
			t.blockedUntil = now.Add(loginWindow)
		}
		if _, exists := m.throttle[key]; !exists {
			m.makeThrottleRoom()
		}
		m.throttle[key] = t
		return "", "", errors.New("invalid_credentials")
	}
	delete(m.throttle, key)
	session = randomToken(32)
	csrf = randomToken(24)
	d := sha256.Sum256([]byte(session))
	sessionKey := base64.RawURLEncoding.EncodeToString(d[:])
	m.sessions[sessionKey] = sessionRecord{digest: d[:], expires: now.Add(sessionTTL), csrf: csrf}
	return session, csrf, nil
}

func throttleExpiry(record throttleRecord) time.Time {
	expires := record.since.Add(loginWindow)
	if record.blockedUntil.After(expires) {
		return record.blockedUntil
	}
	return expires
}

func (m *Manager) pruneExpiredThrottle(now time.Time) {
	for key, record := range m.throttle {
		if record.since.IsZero() || !now.Before(throttleExpiry(record)) {
			delete(m.throttle, key)
		}
	}
}

// makeThrottleRoom evicts the record that will expire soonest when a new
// attacker-controlled key would exceed the memory bound. Eviction can relax
// throttling for that one old key, but it keeps the authentication boundary
// available instead of allowing unbounded state growth.
func (m *Manager) makeThrottleRoom() {
	for len(m.throttle) >= maxThrottleEntries {
		var oldestKey string
		var oldestExpiry time.Time
		for key, record := range m.throttle {
			expires := throttleExpiry(record)
			if oldestKey == "" || expires.Before(oldestExpiry) {
				oldestKey, oldestExpiry = key, expires
			}
		}
		if oldestKey == "" {
			return
		}
		delete(m.throttle, oldestKey)
	}
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (m *Manager) Logout(session string) {
	m.mu.Lock()
	d := sha256.Sum256([]byte(session))
	delete(m.sessions, base64.RawURLEncoding.EncodeToString(d[:]))
	m.mu.Unlock()
}

// Validate checks a session and returns its CSRF token. Expired sessions are
// removed eagerly. It never compares attacker-controlled strings directly.
func (m *Manager) Validate(session string) (csrf string, ok bool) {
	if session == "" {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d := sha256.Sum256([]byte(session))
	key := base64.RawURLEncoding.EncodeToString(d[:])
	s, found := m.sessions[key]
	if !found {
		return "", false
	}
	if !m.now().UTC().Before(s.expires) {
		delete(m.sessions, key)
		return "", false
	}
	if subtle.ConstantTimeCompare(d[:], s.digest) != 1 {
		return "", false
	}
	return s.csrf, true
}

// Middleware protects browser routes with an HttpOnly SameSite cookie. Mutating
// requests additionally require the per-session CSRF header.
func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/api/v1/setup" || (r.URL.Path == "/api/v1/session" && r.Method == http.MethodPost) {
			next.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie("payesh_session")
		if err != nil {
			unauthorized(w)
			return
		}
		csrf, ok := m.Validate(c.Value)
		if !ok {
			unauthorized(w)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if subtle.ConstantTimeCompare([]byte(csrf), []byte(r.Header.Get("X-CSRF-Token"))) != 1 {
				writeAuthError(w, http.StatusForbidden, "csrf_required", "csrf token required", false)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Cookie realm="payesh"`)
	writeAuthError(w, http.StatusUnauthorized, "unauthorized", "authentication required", true)
}

func writeAuthError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	data, err := json.Marshal(struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	}{Code: code, Message: message, Retryable: retryable})
	if err != nil {
		data = []byte(`{"code":"internal_error","message":"request failed","retryable":false}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

func (m *Manager) SetSessionCookie(w http.ResponseWriter, session string) {
	m.SetSessionCookieWithSecurity(w, session, true)
}

// SetSessionCookieWithSecurity writes the browser session cookie with an
// explicit transport security policy. Secure cookies are the default for
// callers that serve over HTTPS. The fleet API uses the false setting only
// for its explicitly loopback-bound HTTP development/local mode.
func (m *Manager) SetSessionCookieWithSecurity(w http.ResponseWriter, session string, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: "payesh_session", Value: session, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL / time.Second)})
}

// ClearSessionCookie expires the browser session cookie using the same
// transport security policy as SetSessionCookieWithSecurity.
func (m *Manager) ClearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: m.SessionCookieName(), Value: "", Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

// PBKDF2-HMAC-SHA256, kept here to avoid a runtime dependency on a C-backed
// password library. Iteration count is intentionally explicit and reviewable.
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	var out []byte
	blocks := (keyLen + 31) / 32
	for i := 1; i <= blocks; i++ {
		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		mac.Write([]byte{byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)})
		u := mac.Sum(nil)
		t := append([]byte(nil), u...)
		for j := 1; j < iter; j++ {
			mac = hmac.New(sha256.New, password)
			mac.Write(u)
			u = mac.Sum(nil)
			for k := range t {
				t[k] ^= u[k]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func (m *Manager) Configured() bool          { m.mu.Lock(); defer m.mu.Unlock(); return m.configured }
func (m *Manager) SessionCookieName() string { return strings.TrimSpace("payesh_session") }
