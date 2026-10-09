package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	// TokenPrefix marks Payesh API tokens so secret scanners and people can
	// recognize a leaked credential.
	TokenPrefix      = "pyt_"
	maxTokens        = 256
	maxTokensPerUser = 20
	MaxTokenLifetime = 365 * 24 * time.Hour
)

// APIToken describes a bearer credential without its secret. Tokens act as the
// account that created them, capped at the token's own permission.
type APIToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Username   string     `json:"username"`
	Permission string     `json:"permission"`
	Hint       string     `json:"hint"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	LastUsedIP string     `json:"last_used_ip,omitempty"`
	Status     string     `json:"status"`
	ServerIDs  []string   `json:"server_ids"`
	Actions    []string   `json:"actions"`
}

type tokenRecord struct {
	APIToken
	digest   []byte
	Activity []TokenActivity
}

// Principal is the authenticated caller of one request. TokenID is set only
// when the request authenticated with an API token instead of a session.
type Principal struct {
	Account
	TokenID   string
	ServerIDs []string
	Actions   []string
}

// CreateToken issues a bearer token for an existing account. The secret is
// returned once and only its SHA-256 digest is retained. A token cannot grant
// more than the account's current permission.
func (m *Manager) CreateToken(username, name, permission string, lifetime time.Duration) (string, APIToken, error) {
	return m.CreateScopedToken(username, name, permission, lifetime, TokenOptions{})
}

func (m *Manager) CreateScopedToken(username, name, permission string, lifetime time.Duration, options TokenOptions) (string, APIToken, error) {
	if err := validateTokenOptions(options); err != nil {
		return "", APIToken{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return "", APIToken{}, errors.New("token name must contain 1..64 characters")
	}
	if permission != "read" && permission != "edit" {
		return "", APIToken{}, errors.New("permission must be read or edit")
	}
	if lifetime <= 0 || lifetime > MaxTokenLifetime {
		return "", APIToken{}, errors.New("token lifetime must be between 1 day and 365 days")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	account, ok := m.accountLocked(username)
	if !ok {
		return "", APIToken{}, errors.New("account not found")
	}
	if permission == "edit" && account.Permission != "edit" {
		return "", APIToken{}, errors.New("a read-only account can only create read tokens")
	}
	now := m.now().UTC()
	m.pruneExpiredTokensLocked(now)
	owned := 0
	for _, record := range m.tokens {
		if record.Username == username && now.Before(record.ExpiresAt) {
			owned++
		}
	}
	if owned >= maxTokensPerUser || !m.makeTokenRoomLocked(now) {
		return "", APIToken{}, errors.New("token limit reached")
	}
	secret := TokenPrefix + randomToken(32)
	d := sha256.Sum256([]byte(secret))
	key := base64.RawURLEncoding.EncodeToString(d[:])
	record := tokenRecord{APIToken: APIToken{ID: randomToken(9), Name: name, Username: username, Permission: permission, Hint: secret[:len(TokenPrefix)+4], CreatedAt: now, ExpiresAt: now.Add(lifetime), ServerIDs: slices.Clone(options.ServerIDs), Actions: slices.Clone(options.Actions)}, digest: d[:]}
	m.tokens[key] = record
	if err := m.persistLocked(); err != nil {
		delete(m.tokens, key)
		return "", APIToken{}, fmt.Errorf("%w: %v", ErrTokenPersistence, err)
	}
	return secret, tokenMetadata(record.APIToken, now), nil
}

// Tokens lists retained tokens, newest first. An empty username lists all.
func (m *Manager) Tokens(username string) []APIToken {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	result := []APIToken{}
	for _, record := range m.tokens {
		if username == "" || record.Username == username {
			result = append(result, tokenMetadata(record.APIToken, now))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result
}

// RevokeToken deletes a token by ID. A non-empty username restricts the
// revocation to that account's tokens.
func (m *Manager) RevokeToken(username, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, record := range m.tokens {
		if record.ID != id || (username != "" && record.Username != username) {
			continue
		}
		delete(m.tokens, key)
		if err := m.persistLocked(); err != nil {
			m.tokens[key] = record
			return fmt.Errorf("%w: %v", ErrTokenPersistence, err)
		}
		return nil
	}
	return ErrTokenNotFound
}

// Authenticate resolves the caller from an API token (Authorization: Bearer)
// or, when no bearer token is presented, from the browser session cookie. It
// does not check CSRF; Middleware does that for cookie sessions.
func (m *Manager) Authenticate(r *http.Request) (Principal, bool) {
	if token, ok := bearerToken(r); ok {
		return m.tokenPrincipal(token)
	}
	c, err := r.Cookie(m.SessionCookieName())
	if err != nil {
		return Principal{}, false
	}
	account, ok := m.SessionAccount(c.Value)
	return Principal{Account: account}, ok
}

func (m *Manager) tokenPrincipal(secret string) (Principal, bool) {
	if !strings.HasPrefix(secret, TokenPrefix) {
		return Principal{}, false
	}
	d := sha256.Sum256([]byte(secret))
	key := base64.RawURLEncoding.EncodeToString(d[:])
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.tokens[key]
	if !ok || subtle.ConstantTimeCompare(d[:], record.digest) != 1 {
		return Principal{}, false
	}
	now := m.now().UTC()
	if !now.Before(record.ExpiresAt) {
		return Principal{}, false
	}
	account, ok := m.accountLocked(record.Username)
	if !ok {
		return Principal{}, false
	}
	if record.Permission == "read" {
		account.Permission = "read"
	}
	return Principal{Account: account, TokenID: record.ID, ServerIDs: slices.Clone(record.ServerIDs), Actions: slices.Clone(record.Actions)}, true
}

// HasBearerToken reports whether the request presents an API token.
func HasBearerToken(r *http.Request) bool {
	_, ok := bearerToken(r)
	return ok
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if len(header) < 7 || !strings.EqualFold(header[:7], "Bearer ") {
		return "", false
	}
	return strings.TrimSpace(header[7:]), true
}

func (m *Manager) accountLocked(username string) (Account, bool) {
	if m.configured && username == m.username {
		return Account{Username: m.username, Role: "owner", Permission: "edit"}, true
	}
	a, ok := m.accounts[username]
	return a.Account, ok
}

// renameTokensLocked moves tokens with an account rename and revokes them on
// a password reset, matching how sessions are treated.
func (m *Manager) renameTokensLocked(oldName, newName string, revoke bool) {
	for key, record := range m.tokens {
		if record.Username != oldName {
			continue
		}
		if revoke {
			delete(m.tokens, key)
		} else {
			record.Username = newName
			m.tokens[key] = record
		}
	}
}

func (m *Manager) deleteTokensLocked(username string) {
	for key, record := range m.tokens {
		if record.Username == username {
			delete(m.tokens, key)
		}
	}
}

// expiredTokenRetention keeps expired tokens visible, so they can be extended
// or their activity read, before they are removed for good.
const expiredTokenRetention = 30 * 24 * time.Hour

func (m *Manager) pruneExpiredTokensLocked(now time.Time) {
	for key, record := range m.tokens {
		if now.Sub(record.ExpiresAt) >= expiredTokenRetention {
			delete(m.tokens, key)
		}
	}
}

// makeTokenRoomLocked frees a slot under the global token bound by dropping
// the longest-expired token. Active tokens are never evicted.
func (m *Manager) makeTokenRoomLocked(now time.Time) bool {
	for len(m.tokens) >= maxTokens {
		oldestKey := ""
		for key, record := range m.tokens {
			if now.Before(record.ExpiresAt) {
				continue
			}
			if oldestKey == "" || record.ExpiresAt.Before(m.tokens[oldestKey].ExpiresAt) {
				oldestKey = key
			}
		}
		if oldestKey == "" {
			return false
		}
		delete(m.tokens, oldestKey)
	}
	return true
}

func (m *Manager) copyTokensLocked() map[string]tokenRecord {
	tokens := make(map[string]tokenRecord, len(m.tokens))
	for key, value := range m.tokens {
		tokens[key] = value
	}
	return tokens
}

var ErrTokenNotFound = errors.New("token not found")
var ErrTokenPersistence = errors.New("could not persist token state")

const maxTokenActivity = 100
const maxActivityPathBytes = 256

type TokenOptions struct {
	ServerIDs []string `json:"server_ids"`
	Actions   []string `json:"actions"`
}
type TokenUpdate struct {
	Name          *string   `json:"name"`
	Permission    *string   `json:"permission"`
	ExpiresInDays *int      `json:"expires_in_days"`
	ServerIDs     *[]string `json:"server_ids"`
	Actions       *[]string `json:"actions"`
}
type TokenActivity struct {
	ID     string    `json:"id"`
	At     time.Time `json:"at"`
	Method string    `json:"method"`
	Path   string    `json:"path"`
	Status int       `json:"status"`
	IP     string    `json:"ip"`
}

func validateTokenOptions(o TokenOptions) error {
	if len(o.ServerIDs) > 200 || len(o.Actions) > 3 {
		return errors.New("too many token scopes")
	}
	for _, id := range o.ServerIDs {
		if len(id) < 16 || len(id) > 128 {
			return errors.New("invalid server ID")
		}
		for _, c := range id {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return errors.New("invalid server ID")
			}
		}
	}
	for _, action := range o.Actions {
		if action != "monitoring" && action != "read" && action != "write" {
			return errors.New("actions must be monitoring, read, or write")
		}
	}
	return nil
}
func tokenMetadata(t APIToken, now time.Time) APIToken {
	t.ServerIDs = append([]string{}, t.ServerIDs...)
	t.Actions = append([]string{}, t.Actions...)
	if t.LastUsedAt != nil {
		v := *t.LastUsedAt
		t.LastUsedAt = &v
	}
	t.Status = "active"
	if !now.Before(t.ExpiresAt) {
		t.Status = "expired"
	} else if t.ExpiresAt.Sub(now) <= 7*24*time.Hour {
		t.Status = "expiring"
	}
	return t
}
func (m *Manager) UpdateToken(username, id string, u TokenUpdate) (APIToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, record := range m.tokens {
		if record.ID != id || username != "" && record.Username != username {
			continue
		}
		previous := record
		if u.Name != nil {
			record.Name = strings.TrimSpace(*u.Name)
		}
		if record.Name == "" || len(record.Name) > 64 {
			return APIToken{}, errors.New("token name must contain 1..64 characters")
		}
		if u.Permission != nil {
			record.Permission = *u.Permission
		}
		account, ok := m.accountLocked(record.Username)
		if !ok || record.Permission != "read" && record.Permission != "edit" || record.Permission == "edit" && account.Permission != "edit" && previous.Permission != "edit" {
			return APIToken{}, errors.New("invalid token permission for account")
		}
		if u.ExpiresInDays != nil {
			if *u.ExpiresInDays < 1 || *u.ExpiresInDays > 365 {
				return APIToken{}, errors.New("token lifetime must be between 1 day and 365 days")
			}
			record.ExpiresAt = m.now().UTC().Add(time.Duration(*u.ExpiresInDays) * 24 * time.Hour)
		}
		if u.ServerIDs != nil {
			record.ServerIDs = slices.Clone(*u.ServerIDs)
		}
		if u.Actions != nil {
			record.Actions = slices.Clone(*u.Actions)
		}
		if err := validateTokenOptions(TokenOptions{record.ServerIDs, record.Actions}); err != nil {
			return APIToken{}, err
		}
		m.tokens[key] = record
		if err := m.persistLocked(); err != nil {
			m.tokens[key] = previous
			return APIToken{}, fmt.Errorf("%w: %v", ErrTokenPersistence, err)
		}
		return tokenMetadata(record.APIToken, m.now().UTC()), nil
	}
	return APIToken{}, ErrTokenNotFound
}

// RotateToken replaces the secret in one durable state change; the token ID,
// scopes, creation time and activity history are preserved.
func (m *Manager) RotateToken(username, id string, days int) (string, APIToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if days < 0 || days > 365 {
		return "", APIToken{}, errors.New("invalid token lifetime")
	}
	for key, record := range m.tokens {
		if record.ID != id || username != "" && record.Username != username {
			continue
		}
		previous := record
		if days > 0 {
			record.ExpiresAt = m.now().UTC().Add(time.Duration(days) * 24 * time.Hour)
		}
		if !m.now().Before(record.ExpiresAt) {
			return "", APIToken{}, errors.New("extend an expired token before rotating it")
		}
		secret := TokenPrefix + randomToken(32)
		d := sha256.Sum256([]byte(secret))
		newKey := base64.RawURLEncoding.EncodeToString(d[:])
		record.digest = d[:]
		record.Hint = secret[:len(TokenPrefix)+4]
		delete(m.tokens, key)
		m.tokens[newKey] = record
		if err := m.persistLocked(); err != nil {
			delete(m.tokens, newKey)
			m.tokens[key] = previous
			return "", APIToken{}, fmt.Errorf("%w: %v", ErrTokenPersistence, err)
		}
		return secret, tokenMetadata(record.APIToken, m.now().UTC()), nil
	}
	return "", APIToken{}, ErrTokenNotFound
}
func (m *Manager) RevokeAllTokens(username string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if username == "" {
		return 0, errors.New("username is required")
	}
	if _, ok := m.accountLocked(username); !ok {
		return 0, errors.New("account not found")
	}
	previous := m.copyTokensLocked()
	count := 0
	for key, r := range m.tokens {
		if r.Username == username {
			delete(m.tokens, key)
			count++
		}
	}
	if err := m.persistLocked(); err != nil {
		m.tokens = previous
		return 0, fmt.Errorf("%w: %v", ErrTokenPersistence, err)
	}
	return count, nil
}
func (m *Manager) TokenActivity(username, id string) ([]TokenActivity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.tokens {
		if r.ID == id && (username == "" || r.Username == username) {
			items := append([]TokenActivity{}, r.Activity...)
			slices.Reverse(items)
			return items, nil
		}
	}
	return nil, ErrTokenNotFound
}

type requestScopeKey struct{}
type requestScope struct {
	Principal
	IP string
}

// WithClientIP accepts an address resolved by the trusted proxy boundary.
func WithClientIP(r *http.Request, ip string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), clientIPKey{}, ip))
}

type clientIPKey struct{}

func requestIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey{}).(string); ok {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	if ip := net.ParseIP(r.RemoteAddr); ip != nil {
		return ip.String()
	}
	return ""
}

// AllowedServer limits collections using the same scope as direct requests.
func AllowedServer(r *http.Request, id string) bool {
	s, ok := r.Context().Value(requestScopeKey{}).(requestScope)
	return !ok || len(s.ServerIDs) == 0 || slices.Contains(s.ServerIDs, id)
}
func tokenAllows(p Principal, r *http.Request) bool {
	path := r.URL.Path
	if path == "/api/v1/account/me" && safeMethod(r.Method) {
		return true
	}
	if len(p.ServerIDs) > 0 {
		if path == "/api/v1/servers" && safeMethod(r.Method) {
		} else if strings.HasPrefix(path, "/api/v1/servers/") {
			id := strings.Split(strings.TrimPrefix(path, "/api/v1/servers/"), "/")[0]
			if !slices.Contains(p.ServerIDs, id) {
				return false
			}
		} else {
			return false
		}
	}
	if len(p.Actions) == 0 {
		return true
	}
	if safeMethod(r.Method) {
		if slices.Contains(p.Actions, "read") {
			return true
		}
		if slices.Contains(p.Actions, "monitoring") {
			if path == "/api/v1/servers" {
				return true
			}
			parts := strings.Split(strings.TrimPrefix(path, "/api/v1/servers/"), "/")
			if !strings.HasPrefix(path, "/api/v1/servers/") {
				return false
			}
			if len(parts) == 1 {
				return true
			}
			switch parts[1] {
			case "metrics", "traffic", "logs", "processes":
				return true
			}
		}
		return false
	}
	return slices.Contains(p.Actions, "write")
}

// activityFlushDelay batches token activity: requests update memory, and the
// authentication record is written at most once per delay. Recording every
// request synchronously would rewrite the whole record twice per call while
// holding the lock that every session check needs. A crash can lose up to
// this much activity; credentials and scopes are always written immediately.
const activityFlushDelay = 30 * time.Second

func (m *Manager) recordTokenActivity(id string, event TokenActivity) {
	if len(event.Path) > maxActivityPathBytes {
		event.Path = event.Path[:maxActivityPathBytes]
	}
	// JSON escaping can expand attacker-controlled paths. Keep the serialized
	// entry bounded as well as its input so snapshots fit the storage limit.
	for {
		encoded, _ := json.Marshal(event.Path)
		if len(encoded) <= maxActivityPathBytes+2 {
			break
		}
		event.Path = event.Path[:len(event.Path)/2]
	}
	if len(event.Method) > 16 {
		event.Method = event.Method[:16]
	}
	if len(event.IP) > 64 {
		event.IP = ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, r := range m.tokens {
		if r.ID != id {
			continue
		}
		at := event.At
		r.LastUsedAt = &at
		r.LastUsedIP = event.IP
		r.Activity = append(slices.Clone(r.Activity), event)
		if len(r.Activity) > maxTokenActivity {
			r.Activity = r.Activity[len(r.Activity)-maxTokenActivity:]
		}
		m.tokens[key] = r
		m.scheduleActivityFlushLocked()
		return
	}
	// The token was revoked during the request; never recreate it.
}

func (m *Manager) scheduleActivityFlushLocked() {
	if m.activityPending {
		return
	}
	m.activityPending = true
	m.activityTimer = time.AfterFunc(activityFlushDelay, func() { _ = m.FlushTokenActivity() })
}

// FlushTokenActivity writes batched token activity now. The server calls it
// on shutdown; it is otherwise run automatically.
func (m *Manager) FlushTokenActivity() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.activityPending {
		return nil
	}
	if m.activityTimer != nil {
		m.activityTimer.Stop()
	}
	m.activityPending = false
	if err := m.persistLocked(); err != nil {
		// Keep the activity in memory and try again later.
		m.scheduleActivityFlushLocked()
		return err
	}
	return nil
}

type activityWriter struct {
	http.ResponseWriter
	status int
}

func (w *activityWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *activityWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}
func (w *activityWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *activityWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// AllowedServerIDs returns a copy of the request's server allowlist.
func AllowedServerIDs(r *http.Request) []string {
	s, ok := r.Context().Value(requestScopeKey{}).(requestScope)
	if !ok {
		return nil
	}
	return slices.Clone(s.ServerIDs)
}
