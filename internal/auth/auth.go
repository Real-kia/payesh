// Package auth owns browser accounts, sessions, CSRF, and role permissions.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
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
	digest   []byte
	expires  time.Time
	csrf     string
	username string
}
type Account struct {
	Username   string `json:"username"`
	Role       string `json:"role"`
	Permission string `json:"permission"`
	TokenCount int    `json:"token_count"`
}
type accountRecord struct {
	Account
	password passwordRecord
}
type throttleRecord struct {
	failures     int
	since        time.Time
	blockedUntil time.Time
}

// Manager owns the owner account, delegated accounts, and browser sessions. It is safe for
// concurrent use. No cleartext password, setup secret, or session token is
// retained after the operation that consumes it.
type Manager struct {
	mu          sync.Mutex
	owner       passwordRecord
	username    string
	accounts    map[string]accountRecord
	configured  bool
	setupDigest []byte
	sessions    map[string]sessionRecord // keyed by a SHA-256 session digest
	tokens      map[string]tokenRecord   // keyed by a SHA-256 API token digest
	// activityPending marks token activity not yet written; see
	// activityFlushDelay.
	activityPending bool
	activityTimer   *time.Timer
	throttle        map[string]throttleRecord
	now             func() time.Time
	repository      Repository
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
	m := &Manager{setupDigest: d[:], sessions: make(map[string]sessionRecord), tokens: make(map[string]tokenRecord), accounts: make(map[string]accountRecord), throttle: make(map[string]throttleRecord), now: time.Now, repository: repository}
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
	record, member := m.accounts[username]
	ok := m.configured && ((subtle.ConstantTimeCompare([]byte(username), []byte(m.username)) == 1 && verifyPassword(m.owner, password)) || (member && verifyPassword(record.password, password)))
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
	m.sessions[sessionKey] = sessionRecord{digest: d[:], expires: now.Add(sessionTTL), csrf: csrf, username: username}
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

func (m *Manager) SessionAccount(session string) (Account, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := sha256.Sum256([]byte(session))
	s, ok := m.sessions[base64.RawURLEncoding.EncodeToString(d[:])]
	if !ok || !m.now().UTC().Before(s.expires) {
		return Account{}, false
	}
	if s.username == m.username {
		return Account{Username: m.username, Role: "owner", Permission: "edit"}, true
	}
	a, ok := m.accounts[s.username]
	return a.Account, ok
}

func (m *Manager) Accounts() []Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := []Account{{Username: m.username, Role: "owner", Permission: "edit"}}
	for _, a := range m.accounts {
		result = append(result, a.Account)
	}
	for i := range result {
		for _, r := range m.tokens {
			if r.Username == result[i].Username && m.now().Before(r.ExpiresAt) {
				result[i].TokenCount++
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Username < result[j].Username })
	return result
}

func (m *Manager) SaveAccount(username, role, permission, password string) error {
	if len(username) < 3 || len(username) > 128 || strings.ContainsAny(username, " \t\r\n/") {
		return errors.New("invalid username")
	}
	if role != "admin" && role != "member" {
		return errors.New("role must be admin or member")
	}
	if permission != "read" && permission != "edit" {
		return errors.New("permission must be read or edit")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if username == m.username {
		return errors.New("owner account cannot be changed here")
	}
	old, exists := m.accounts[username]
	if exists {
		return errors.New("username already exists")
	}
	if len(m.accounts) >= 100 {
		return errors.New("account limit reached")
	}
	if password == "" {
		return errors.New("password is required")
	}
	if len(password) < 12 || len(password) > 256 {
		return errors.New("password must contain 12..256 characters")
	}
	var err error
	old.password, err = newPasswordRecord(password)
	if err != nil {
		return err
	}
	old.Account = Account{Username: username, Role: role, Permission: permission}
	m.accounts[username] = old
	if err := m.persistLocked(); err != nil {
		delete(m.accounts, username)
		return err
	}
	return nil
}

func (m *Manager) DeleteAccount(username string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if username == m.username {
		return errors.New("owner account cannot be deleted")
	}
	old, exists := m.accounts[username]
	if !exists {
		return errors.New("account not found")
	}
	delete(m.accounts, username)
	previousSessions := make(map[string]sessionRecord, len(m.sessions))
	for key, s := range m.sessions {
		previousSessions[key] = s
	}
	for key, s := range m.sessions {
		if s.username == username {
			delete(m.sessions, key)
		}
	}
	previousTokens := m.copyTokensLocked()
	m.deleteTokensLocked(username)
	if err := m.persistLocked(); err != nil {
		m.accounts[username] = old
		m.sessions = previousSessions
		m.tokens = previousTokens
		return err
	}
	return nil
}

// UpdateAccount changes an account and its sessions in one persisted state.
func (m *Manager) UpdateAccount(oldName, newName, role, permission, password string) error {
	if newName == "" {
		newName = oldName
	}
	if len(newName) < 3 || len(newName) > 128 || strings.ContainsAny(newName, " \t\r\n/") {
		return errors.New("invalid username")
	}
	if password != "" && (len(password) < 12 || len(password) > 256) {
		return errors.New("password must contain 12..256 characters")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	owner := oldName == m.username
	if !owner && (role != "admin" && role != "member" || permission != "read" && permission != "edit") {
		return errors.New("invalid role or permission")
	}
	if owner && (role != "" || permission != "") {
		return errors.New("owner role cannot change")
	}
	if !owner {
		if _, ok := m.accounts[oldName]; !ok {
			return errors.New("account not found")
		}
	}
	if newName != oldName {
		if newName == m.username {
			return errors.New("username already exists")
		}
		if _, ok := m.accounts[newName]; ok {
			return errors.New("username already exists")
		}
	}
	previousUsername, previousOwner := m.username, m.owner
	previousAccounts := make(map[string]accountRecord, len(m.accounts))
	for key, value := range m.accounts {
		previousAccounts[key] = value
	}
	previousSessions := make(map[string]sessionRecord, len(m.sessions))
	for key, value := range m.sessions {
		previousSessions[key] = value
	}
	previousTokens := m.copyTokensLocked()
	if owner {
		m.username = newName
		if password != "" {
			record, err := newPasswordRecord(password)
			if err != nil {
				m.username = previousUsername
				return err
			}
			m.owner = record
		}
	} else {
		account := m.accounts[oldName]
		delete(m.accounts, oldName)
		account.Username, account.Role, account.Permission = newName, role, permission
		if password != "" {
			record, err := newPasswordRecord(password)
			if err != nil {
				m.accounts = previousAccounts
				return err
			}
			account.password = record
		}
		m.accounts[newName] = account
	}
	for key, session := range m.sessions {
		if session.username != oldName {
			continue
		}
		if password != "" {
			delete(m.sessions, key)
		} else {
			session.username = newName
			m.sessions[key] = session
		}
	}
	m.renameTokensLocked(oldName, newName, password != "")
	if err := m.persistLocked(); err != nil {
		m.username, m.owner, m.accounts, m.sessions, m.tokens = previousUsername, previousOwner, previousAccounts, previousSessions, previousTokens
		return err
	}
	return nil
}

// Middleware protects browser routes with an HttpOnly SameSite cookie. Mutating
// requests additionally require the per-session CSRF header.
func (m *Manager) Middleware(next http.Handler) http.Handler {
	return m.middleware(next, true)
}

// SelfServiceMiddleware is Middleware for routes where read-only accounts may
// still change their own credentials. It keeps the session and CSRF checks and
// leaves the permission decision to the handler.
func (m *Manager) SelfServiceMiddleware(next http.Handler) http.Handler {
	return m.middleware(next, false)
}

func (m *Manager) middleware(next http.Handler, requireEdit bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/api/v1/setup" || (r.URL.Path == "/api/v1/session" && r.Method == http.MethodPost) {
			next.ServeHTTP(w, r)
			return
		}
		// API tokens are sent explicitly by the client rather than attached by
		// the browser, so they are not exposed to CSRF and skip that check.
		if token, ok := bearerToken(r); ok {
			principal, ok := m.tokenPrincipal(token)
			if !ok {
				unauthorized(w)
				return
			}
			// Every token request, including denied ones, is recorded with its
			// final status once the handler returns.
			aw := &activityWriter{ResponseWriter: w}
			event := TokenActivity{ID: randomToken(9), At: m.now().UTC(), Method: r.Method, Path: r.URL.Path, IP: requestIP(r)}
			defer func() {
				panicValue := recover()
				event.Status = aw.status
				if panicValue != nil {
					event.Status = http.StatusInternalServerError
				} else if event.Status == 0 {
					event.Status = http.StatusOK
				}
				m.recordTokenActivity(principal.TokenID, event)
				if panicValue != nil {
					panic(panicValue)
				}
			}()
			if requireEdit && !safeMethod(r.Method) && principal.Permission != "edit" {
				writeAuthError(aw, http.StatusForbidden, "read_only", "this API token has read-only access", false)
				return
			}
			if !tokenAllows(principal, r) {
				writeAuthError(aw, http.StatusForbidden, "token_scope", "request is outside this token's scope", false)
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), requestScopeKey{}, requestScope{Principal: principal, IP: event.IP}))
			next.ServeHTTP(aw, r)
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
		if !safeMethod(r.Method) {
			if subtle.ConstantTimeCompare([]byte(csrf), []byte(r.Header.Get("X-CSRF-Token"))) != 1 {
				writeAuthError(w, http.StatusForbidden, "csrf_required", "csrf token required", false)
				return
			}
			account, ok := m.SessionAccount(c.Value)
			if !ok || requireEdit && account.Permission != "edit" {
				writeAuthError(w, http.StatusForbidden, "read_only", "this account has read-only access", false)
				return
			}
		}
		// Let a restored HttpOnly session bootstrap browser mutations after a
		// reload without exposing the session cookie itself to JavaScript.
		w.Header().Set("X-CSRF-Token", csrf)
		next.ServeHTTP(w, r)
	})
}

func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Add("WWW-Authenticate", `Bearer realm="payesh"`)
	w.Header().Add("WWW-Authenticate", `Cookie realm="payesh"`)
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
	Accounts   map[string]persistedAccount  `json:"accounts,omitempty"`
	Sessions   map[string]persistedSession  `json:"sessions,omitempty"`
	Throttle   map[string]persistedThrottle `json:"throttle,omitempty"`
	Tokens     map[string]persistedToken    `json:"tokens,omitempty"`
}

type persistedToken struct {
	APIToken
	Digest   []byte          `json:"digest"`
	Activity []TokenActivity `json:"activity,omitempty"`
}

type persistedSession struct {
	Digest   []byte    `json:"digest"`
	Expires  time.Time `json:"expires"`
	CSRF     string    `json:"csrf"`
	Username string    `json:"username,omitempty"`
}
type persistedAccount struct {
	Role       string `json:"role"`
	Permission string `json:"permission"`
	Salt       []byte `json:"salt"`
	Hash       []byte `json:"hash"`
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
	state := persistedState{Configured: m.configured, OwnerSalt: m.owner.salt, OwnerHash: m.owner.digest, Username: m.username, Accounts: make(map[string]persistedAccount, len(m.accounts)), Sessions: make(map[string]persistedSession, len(m.sessions)), Throttle: make(map[string]persistedThrottle, len(m.throttle)), Tokens: make(map[string]persistedToken, len(m.tokens))}
	for name, a := range m.accounts {
		state.Accounts[name] = persistedAccount{Role: a.Role, Permission: a.Permission, Salt: a.password.salt, Hash: a.password.digest}
	}
	for key, record := range m.sessions {
		state.Sessions[key] = persistedSession{Digest: record.digest, Expires: record.expires, CSRF: record.csrf, Username: record.username}
	}
	for key, record := range m.throttle {
		state.Throttle[key] = persistedThrottle{Failures: record.failures, Since: record.since, BlockedUntil: record.blockedUntil}
	}
	for key, record := range m.tokens {
		state.Tokens[key] = persistedToken{APIToken: record.APIToken, Digest: record.digest, Activity: record.Activity}
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := m.repository.SaveAuthState(data); err != nil {
		return err
	}
	m.activityPending = false
	return nil
}

func (m *Manager) restore(data []byte) error {
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	if !state.Configured || len(state.OwnerSalt) != 16 || len(state.OwnerHash) != 32 {
		return errors.New("invalid persisted owner record")
	}
	if len(state.Sessions) > maxSessions || len(state.Throttle) > maxThrottleEntries || len(state.Tokens) > maxTokens {
		return errors.New("persisted authentication state exceeds bounds")
	}
	m.configured = true
	m.username = state.Username
	if m.username == "" {
		m.username = "admin"
	}
	m.owner = passwordRecord{salt: append([]byte(nil), state.OwnerSalt...), digest: append([]byte(nil), state.OwnerHash...)}
	if len(state.Accounts) > 100 {
		return errors.New("too many accounts")
	}
	for name, a := range state.Accounts {
		if name == m.username || len(name) < 3 || len(name) > 128 || strings.ContainsAny(name, " \t\r\n/") || len(a.Salt) != 16 || len(a.Hash) != 32 || (a.Role != "admin" && a.Role != "member") || (a.Permission != "read" && a.Permission != "edit") {
			return errors.New("invalid persisted account")
		}
		m.accounts[name] = accountRecord{Account: Account{Username: name, Role: a.Role, Permission: a.Permission}, password: passwordRecord{salt: a.Salt, digest: a.Hash}}
	}
	for i := range m.setupDigest {
		m.setupDigest[i] = 0
	}
	now := m.now().UTC()
	for key, record := range state.Sessions {
		if len(record.Digest) != sha256.Size || record.CSRF == "" || !now.Before(record.Expires) {
			continue
		}
		name := record.Username
		if name == "" {
			name = m.username
		}
		if name != m.username {
			if _, exists := m.accounts[name]; !exists {
				continue
			}
		}
		m.sessions[key] = sessionRecord{digest: append([]byte(nil), record.Digest...), expires: record.Expires, csrf: record.CSRF, username: name}
	}
	for key, record := range state.Throttle {
		value := throttleRecord{failures: record.Failures, since: record.Since, blockedUntil: record.BlockedUntil}
		if key != "" && value.failures > 0 && now.Before(throttleExpiry(value)) {
			m.throttle[key] = value
		}
	}
	for key, record := range state.Tokens {
		if len(record.Digest) != sha256.Size || record.ID == "" || (record.Permission != "read" && record.Permission != "edit") {
			continue
		}
		if _, exists := m.accountLocked(record.Username); !exists {
			continue
		}
		// A damaged token is dropped rather than failing startup, which would
		// lock every account out of the dashboard.
		if err := validateTokenOptions(TokenOptions{record.ServerIDs, record.Actions}); err != nil {
			continue
		}
		activity := make([]TokenActivity, 0, min(len(record.Activity), maxTokenActivity))
		for _, event := range record.Activity[max(0, len(record.Activity)-maxTokenActivity):] {
			if len(event.Method) > 16 || len(event.IP) > 64 {
				continue
			}
			if len(event.Path) > maxActivityPathBytes {
				event.Path = event.Path[:maxActivityPathBytes]
			}
			activity = append(activity, event)
		}
		m.tokens[key] = tokenRecord{APIToken: record.APIToken, digest: append([]byte(nil), record.Digest...), Activity: activity}
	}
	return nil
}
