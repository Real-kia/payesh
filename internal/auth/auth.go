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
	maxSessions        = 256
)

// Repository persists the complete bounded browser-authentication state.
// Implementations must replace the previous value atomically.
type Repository interface {
	LoadAuthState() (data []byte, found bool, err error)
	SaveAuthState(data []byte) error
}

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
	username    string
	configured  bool
	setupDigest []byte
	sessions    map[string]sessionRecord // keyed by a SHA-256 session digest
	throttle    map[string]throttleRecord
	now         func() time.Time
	repository  Repository
}

func New(setupSecret string) (*Manager, error) {
	return NewPersistent(setupSecret, nil)
}

// NewPersistent restores owner, session, and throttling state when a
// repository is supplied. The setup secret itself is never persisted.
func NewPersistent(setupSecret string, repository Repository) (*Manager, error) {
	if len(setupSecret) < 16 {
		return nil, errors.New("setup secret must contain at least 16 characters")
	}
	d := sha256.Sum256([]byte(setupSecret))
	m := &Manager{setupDigest: d[:], sessions: make(map[string]sessionRecord), throttle: make(map[string]throttleRecord), now: time.Now, repository: repository}
	if repository == nil {
		return m, nil
	}
	data, found, err := repository.LoadAuthState()
	if err != nil {
		return nil, fmt.Errorf("load authentication state: %w", err)
	}
	if found {
		if err := m.restore(data); err != nil {
			return nil, fmt.Errorf("restore authentication state: %w", err)
		}
	}
	return m, nil
}

// Setup consumes the one-time bootstrap secret and creates the sole owner.
func (m *Manager) Setup(secret, password string) error {
	return m.SetupWithUsername(secret, "admin", password)
}

// SetupWithUsername creates the owner account with an explicit username.
func (m *Manager) SetupWithUsername(secret, username, password string) error {
	if len(username) < 3 || len(username) > 128 {
		return errors.New("username must contain 3..128 characters")
	}
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
	previousDigest := append([]byte(nil), m.setupDigest...)
	m.owner, m.username, m.configured = r, username, true
	for i := range m.setupDigest {
		m.setupDigest[i] = 0
	}
	if err := m.persistLocked(); err != nil {
		m.owner, m.username, m.configured, m.setupDigest = passwordRecord{}, "", false, previousDigest
		return fmt.Errorf("persist owner setup: %w", err)
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
	return m.LoginWithUsername(key, "admin", password)
}

func (m *Manager) LoginWithUsername(key, username, password string) (session, csrf string, err error) {
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
	ok := m.configured && subtle.ConstantTimeCompare([]byte(username), []byte(m.username)) == 1 && verifyPassword(m.owner, password)
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
		if err := m.persistLocked(); err != nil {
			return "", "", fmt.Errorf("persist login throttle: %w", err)
		}
		return "", "", errors.New("invalid_credentials")
	}
	delete(m.throttle, key)
	m.pruneExpiredSessions(now)
	m.makeSessionRoom()
	session = randomToken(32)
	csrf = randomToken(24)
	d := sha256.Sum256([]byte(session))
	sessionKey := base64.RawURLEncoding.EncodeToString(d[:])
	m.sessions[sessionKey] = sessionRecord{digest: d[:], expires: now.Add(sessionTTL), csrf: csrf}
	if err := m.persistLocked(); err != nil {
		delete(m.sessions, sessionKey)
		return "", "", fmt.Errorf("persist session: %w", err)
	}
	return session, csrf, nil
}

func (m *Manager) pruneExpiredSessions(now time.Time) {
	for key, record := range m.sessions {
		if !now.Before(record.expires) {
			delete(m.sessions, key)
		}
	}
}

func (m *Manager) makeSessionRoom() {
	for len(m.sessions) >= maxSessions {
		var oldestKey string
		var oldestExpiry time.Time
		for key, record := range m.sessions {
			if oldestKey == "" || record.expires.Before(oldestExpiry) {
				oldestKey, oldestExpiry = key, record.expires
			}
		}
		delete(m.sessions, oldestKey)
	}
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

func (m *Manager) Logout(session string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := sha256.Sum256([]byte(session))
	delete(m.sessions, base64.RawURLEncoding.EncodeToString(d[:]))
	return m.persistLocked()
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
		_ = m.persistLocked()
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

type persistedState struct {
	Configured bool                         `json:"configured"`
	OwnerSalt  []byte                       `json:"owner_salt,omitempty"`
	OwnerHash  []byte                       `json:"owner_hash,omitempty"`
	Username   string                       `json:"username,omitempty"`
	Sessions   map[string]persistedSession  `json:"sessions,omitempty"`
	Throttle   map[string]persistedThrottle `json:"throttle,omitempty"`
}

type persistedSession struct {
	Digest  []byte    `json:"digest"`
	Expires time.Time `json:"expires"`
	CSRF    string    `json:"csrf"`
}

type persistedThrottle struct {
	Failures     int       `json:"failures"`
	Since        time.Time `json:"since"`
	BlockedUntil time.Time `json:"blocked_until,omitempty"`
}

func (m *Manager) persistLocked() error {
	if m.repository == nil {
		return nil
	}
	state := persistedState{Configured: m.configured, OwnerSalt: m.owner.salt, OwnerHash: m.owner.digest, Username: m.username, Sessions: make(map[string]persistedSession, len(m.sessions)), Throttle: make(map[string]persistedThrottle, len(m.throttle))}
	for key, record := range m.sessions {
		state.Sessions[key] = persistedSession{Digest: record.digest, Expires: record.expires, CSRF: record.csrf}
	}
	for key, record := range m.throttle {
		state.Throttle[key] = persistedThrottle{Failures: record.failures, Since: record.since, BlockedUntil: record.blockedUntil}
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return m.repository.SaveAuthState(data)
}

func (m *Manager) restore(data []byte) error {
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	if !state.Configured || len(state.OwnerSalt) != 16 || len(state.OwnerHash) != 32 {
		return errors.New("invalid persisted owner record")
	}
	if len(state.Sessions) > maxSessions || len(state.Throttle) > maxThrottleEntries {
		return errors.New("persisted authentication state exceeds bounds")
	}
	m.configured = true
	m.username = state.Username
	if m.username == "" {
		m.username = "admin"
	}
	m.owner = passwordRecord{salt: append([]byte(nil), state.OwnerSalt...), digest: append([]byte(nil), state.OwnerHash...)}
	for i := range m.setupDigest {
		m.setupDigest[i] = 0
	}
	now := m.now().UTC()
	for key, record := range state.Sessions {
		if len(record.Digest) != sha256.Size || record.CSRF == "" || !now.Before(record.Expires) {
			continue
		}
		m.sessions[key] = sessionRecord{digest: append([]byte(nil), record.Digest...), expires: record.Expires, csrf: record.CSRF}
	}
	for key, record := range state.Throttle {
		value := throttleRecord{failures: record.Failures, since: record.Since, blockedUntil: record.BlockedUntil}
		if key != "" && value.failures > 0 && now.Before(throttleExpiry(value)) {
			m.throttle[key] = value
		}
	}
	return nil
}
