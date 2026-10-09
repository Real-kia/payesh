package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type tokenTestRepository struct {
	data []byte
	fail bool
}

func (r *tokenTestRepository) LoadAuthState() ([]byte, bool, error) {
	return bytes.Clone(r.data), len(r.data) > 0, nil
}

func (r *tokenTestRepository) SaveAuthState(data []byte) error {
	if r.fail {
		return errors.New("repository unavailable")
	}
	r.data = bytes.Clone(data)
	return nil
}

func persistentTokenManager(t *testing.T) (*Manager, *tokenTestRepository) {
	t.Helper()
	r := &tokenTestRepository{}
	m, err := NewPersistent("0123456789abcdef-bootstrap", r)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetupWithUsername("0123456789abcdef-bootstrap", "owner", "long-enough-password"); err != nil {
		t.Fatal(err)
	}
	return m, r
}

func restoreTokenManager(t *testing.T, repository Repository) *Manager {
	t.Helper()
	m, err := NewPersistent("0123456789abcdef-bootstrap", repository)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTokenLifecycleSurvivesRestart(t *testing.T) {
	m, repository := persistentTokenManager(t)
	now := time.Now().UTC().Truncate(time.Second)
	m.now = func() time.Time { return now }
	secret, token, err := m.CreateToken("owner", "agent", "edit", 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if token.Status != "active" || token.CreatedAt != now {
		t.Fatalf("creation metadata: %+v", token)
	}
	handler := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	r := bearerRequest(http.MethodGet, secret)
	r.RemoteAddr = "192.0.2.42:54321"
	r.Header.Set("X-Forwarded-For", "192.0.2.99")
	r.URL.RawQuery = "sensitive=not-in-history"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("request: %d", w.Code)
	}
	if err := m.FlushTokenActivity(); err != nil {
		t.Fatal(err)
	}
	restored := restoreTokenManager(t, repository)
	items := restored.Tokens("owner")
	if len(items) != 1 || items[0].LastUsedAt == nil || !items[0].LastUsedAt.Equal(now) || items[0].LastUsedIP != "192.0.2.42" {
		t.Fatalf("persisted usage: %+v", items)
	}
	activity, err := restored.TokenActivity("owner", token.ID)
	if err != nil || len(activity) != 1 || activity[0].Status != http.StatusAccepted || activity[0].Path != "/api/v1/servers" || activity[0].IP != "192.0.2.42" {
		t.Fatalf("persisted activity: %+v, %v", activity, err)
	}
	now = token.ExpiresAt.Add(-7 * 24 * time.Hour)
	if got := m.Tokens("owner")[0].Status; got != "expiring" {
		t.Fatalf("expiry warning: %s", got)
	}
	now = token.ExpiresAt
	if _, ok := m.Authenticate(bearerRequest(http.MethodGet, secret)); ok {
		t.Fatal("token accepted at exact expiry")
	}
	if got := m.Tokens("owner"); len(got) != 1 || got[0].Status != "expired" {
		t.Fatalf("expired token missing: %+v", got)
	}
	// Trigger a durable state change and verify restore retains expired tokens.
	name := "expired-agent"
	if _, err := m.UpdateToken("owner", token.ID, TokenUpdate{Name: &name}); err != nil {
		t.Fatal(err)
	}
	restored = restoreTokenManager(t, repository)
	restored.now = func() time.Time { return now }
	if got := restored.Tokens("owner"); len(got) != 1 || got[0].Status != "expired" {
		t.Fatalf("expired token lost on restart: %+v", got)
	}
}

func TestTokenRotationIsAtomicAndPreservesIdentity(t *testing.T) {
	m, repository := persistentTokenManager(t)
	oldSecret, before, err := m.CreateScopedToken("owner", "agent", "edit", 24*time.Hour, TokenOptions{ServerIDs: []string{"server-1234567890"}, Actions: []string{"read"}})
	if err != nil {
		t.Fatal(err)
	}
	m.recordTokenActivity(before.ID, TokenActivity{ID: "request-1", At: time.Now(), Method: "GET", Path: "/api/v1/servers", Status: 200})
	repository.fail = true
	if secret, _, err := m.RotateToken("owner", before.ID, 90); !errors.Is(err, ErrTokenPersistence) || secret != "" {
		t.Fatalf("failed rotate leaked secret or wrong error: %q, %v", secret, err)
	}
	if _, ok := m.Authenticate(bearerRequest(http.MethodGet, oldSecret)); !ok {
		t.Fatal("failed rotation revoked original secret")
	}
	repository.fail = false
	newSecret, after, err := m.RotateToken("owner", before.ID, 90)
	if err != nil || after.ID != before.ID || !after.CreatedAt.Equal(before.CreatedAt) || len(after.ServerIDs) != 1 || after.ServerIDs[0] != before.ServerIDs[0] || len(after.Actions) != 1 || after.Actions[0] != "read" {
		t.Fatalf("rotation metadata: %+v, %v", after, err)
	}
	if newSecret == oldSecret || !after.ExpiresAt.After(before.ExpiresAt) {
		t.Fatal("rotation did not replace secret and extend expiry")
	}
	restored := restoreTokenManager(t, repository)
	if _, ok := restored.Authenticate(bearerRequest(http.MethodGet, oldSecret)); ok {
		t.Fatal("old secret accepted after restart")
	}
	if _, ok := restored.Authenticate(bearerRequest(http.MethodGet, newSecret)); !ok {
		t.Fatal("new secret rejected after restart")
	}
	if activity, err := restored.TokenActivity("owner", before.ID); err != nil || len(activity) != 1 {
		t.Fatalf("rotation lost history: %+v, %v", activity, err)
	}
}

func TestTokenUpdateAndBulkRevocationRollback(t *testing.T) {
	m, repository := persistentTokenManager(t)
	if err := m.SaveAccount("viewer", "member", "read", "viewer-password-long"); err != nil {
		t.Fatal(err)
	}
	ownerSecret, owner, _ := m.CreateToken("owner", "owner-agent", "edit", 24*time.Hour)
	viewerSecret, viewer, _ := m.CreateToken("viewer", "viewer-agent", "read", 24*time.Hour)
	edit := "edit"
	if _, err := m.UpdateToken("viewer", viewer.ID, TokenUpdate{Permission: &edit}); err == nil {
		t.Fatal("read account escalated token permission")
	}
	name := "changed"
	if _, err := m.UpdateToken("viewer", owner.ID, TokenUpdate{Name: &name}); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("cross-account update: %v", err)
	}
	read := "read"
	days := 30
	servers := []string{"server-1234567890"}
	actions := []string{"monitoring"}
	update := TokenUpdate{Name: &name, Permission: &read, ExpiresInDays: &days, ServerIDs: &servers, Actions: &actions}
	repository.fail = true
	if _, err := m.UpdateToken("owner", owner.ID, update); !errors.Is(err, ErrTokenPersistence) {
		t.Fatalf("update persistence failure: %v", err)
	}
	if got := m.Tokens("owner")[0]; got.Name != owner.Name || got.Permission != owner.Permission || !got.ExpiresAt.Equal(owner.ExpiresAt) || len(got.ServerIDs) != 0 {
		t.Fatalf("failed update changed token: %+v", got)
	}
	if _, err := m.RevokeAllTokens("viewer"); !errors.Is(err, ErrTokenPersistence) {
		t.Fatalf("bulk revoke persistence failure: %v", err)
	}
	if _, ok := m.Authenticate(bearerRequest(http.MethodGet, viewerSecret)); !ok {
		t.Fatal("failed bulk revoke disabled credential")
	}
	repository.fail = false
	if _, err := m.UpdateToken("owner", owner.ID, update); err != nil {
		t.Fatal(err)
	}
	servers[0] = "other-12345678901"
	actions[0] = "write"
	if got := m.Tokens("owner")[0]; got.ServerIDs[0] != "server-1234567890" || got.Actions[0] != "monitoring" {
		t.Fatal("update retained caller-owned scope slices")
	}
	if count, err := m.RevokeAllTokens("viewer"); err != nil || count != 1 {
		t.Fatalf("bulk revoke: %d, %v", count, err)
	}
	restored := restoreTokenManager(t, repository)
	if _, ok := restored.Authenticate(bearerRequest(http.MethodGet, viewerSecret)); ok {
		t.Fatal("bulk-revoked secret survived restart")
	}
	if _, ok := restored.Authenticate(bearerRequest(http.MethodGet, ownerSecret)); !ok {
		t.Fatal("bulk revoke affected another account")
	}
}

func TestTokenScopesAreEnforcedAndDenialsAudited(t *testing.T) {
	m := newTokenManager(t)
	secret, token, err := m.CreateScopedToken("owner", "monitor", "edit", 24*time.Hour, TokenOptions{ServerIDs: []string{"server-1234567890"}, Actions: []string{"monitoring"}})
	if err != nil {
		t.Fatal(err)
	}
	handler := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !AllowedServer(r, "server-1234567890") || AllowedServer(r, "other-12345678901") {
			t.Error("collection visibility bypassed token scope")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/api/v1/servers", 204},
		{"GET", "/api/v1/servers/server-1234567890/metrics", 204},
		{"GET", "/api/v1/servers/other-12345678901/metrics", 403},
		{"POST", "/api/v1/servers/server-1234567890/control", 403},
		{"GET", "/api/v1/accounts", 403},
		{"GET", "/api/v1/servers/server-1234567890/modules", 403},
		{"GET", "/api/v1/jobs", 403},
	} {
		r := bearerRequest(tc.method, secret)
		r.URL.Path = tc.path
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("%s %s: got %d, want %d", tc.method, tc.path, w.Code, tc.status)
		}
	}
	activity, err := m.TokenActivity("owner", token.ID)
	if err != nil || len(activity) != 7 || activity[0].Status != 403 {
		t.Fatalf("denials not audited: %+v, %v", activity, err)
	}
}

func TestTokenActivityIsBatchedAndHistoryIsBounded(t *testing.T) {
	m, repository := persistentTokenManager(t)
	secret, token, err := m.CreateToken("owner", "agent", "edit", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	saved := repository.data
	called := false
	handler := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	// A request records activity in memory only, and an unavailable
	// repository does not block token requests.
	repository.fail = true
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, bearerRequest("POST", secret))
	if !called || w.Code != http.StatusOK || !bytes.Equal(repository.data, saved) {
		t.Fatalf("request: called=%v status=%d wrote=%v", called, w.Code, !bytes.Equal(repository.data, saved))
	}
	if err := m.FlushTokenActivity(); err == nil {
		t.Fatal("flush reported success while the repository failed")
	}
	repository.fail = false
	if err := m.FlushTokenActivity(); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	if got := restoreTokenManager(t, repository).Tokens("owner")[0]; got.LastUsedAt == nil {
		t.Fatal("flushed usage was not persisted")
	}
	for i := 0; i < maxTokenActivity+5; i++ {
		m.recordTokenActivity(token.ID, TokenActivity{ID: "bounded", Status: i + 1})
	}
	if err := m.FlushTokenActivity(); err != nil {
		t.Fatal(err)
	}
	activity, err := restoreTokenManager(t, repository).TokenActivity("owner", token.ID)
	if err != nil || len(activity) != maxTokenActivity || activity[0].Status != maxTokenActivity+5 || activity[len(activity)-1].Status != 6 {
		t.Fatalf("history bound/order: length=%d, %v", len(activity), err)
	}
}

func TestExpiredTokensFreeTheirSlotAndArePrunedLater(t *testing.T) {
	m := newTokenManager(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	for i := 0; i < maxTokensPerUser; i++ {
		if _, _, err := m.CreateToken("viewer", "agent", "read", 24*time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(48 * time.Hour)
	if _, _, err := m.CreateToken("viewer", "replacement", "read", 24*time.Hour); err != nil {
		t.Fatalf("expired tokens still count against the limit: %v", err)
	}
	if got := len(m.Tokens("viewer")); got != maxTokensPerUser+1 {
		t.Fatalf("recently expired tokens should stay visible: %d", got)
	}
	now = now.Add(expiredTokenRetention)
	if _, _, err := m.CreateToken("viewer", "later", "read", 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, token := range m.Tokens("viewer") {
		if token.Name == "agent" {
			t.Fatalf("long-expired token was not pruned: %+v", token)
		}
	}
}

func TestDamagedTokenRecordDoesNotBlockStartup(t *testing.T) {
	m, repository := persistentTokenManager(t)
	secret, _, err := m.CreateToken("owner", "good", "read", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.CreateToken("owner", "bad", "read", 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(repository.data, &state); err != nil {
		t.Fatal(err)
	}
	for _, raw := range state["tokens"].(map[string]any) {
		if token := raw.(map[string]any); token["name"] == "bad" {
			token["actions"] = []string{"delete-everything"}
		}
	}
	if repository.data, err = json.Marshal(state); err != nil {
		t.Fatal(err)
	}
	restored, err := NewPersistent("0123456789abcdef-bootstrap", repository)
	if err != nil {
		t.Fatalf("one damaged token blocked startup: %v", err)
	}
	if got := restored.Tokens(""); len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("restored tokens: %+v", got)
	}
	if _, ok := restored.Authenticate(bearerRequest(http.MethodGet, secret)); !ok {
		t.Fatal("valid token lost")
	}
}

func TestTokenActivityRecordsHandlerPanic(t *testing.T) {
	m, repository := persistentTokenManager(t)
	secret, token, err := m.CreateToken("owner", "agent", "edit", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, startedResponse := range []bool{false, true} {
		handler := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if startedResponse {
				w.WriteHeader(http.StatusOK)
			}
			panic("handler failed")
		}))
		func() {
			defer func() {
				if got := recover(); got != "handler failed" {
					t.Errorf("original handler panic was lost: %v", got)
				}
			}()
			handler.ServeHTTP(httptest.NewRecorder(), bearerRequest(http.MethodGet, secret))
		}()
		if err := m.FlushTokenActivity(); err != nil {
			t.Fatal(err)
		}
		activity, err := restoreTokenManager(t, repository).TokenActivity("owner", token.ID)
		if err != nil || len(activity) == 0 || activity[0].Status != http.StatusInternalServerError {
			t.Fatalf("panic recorded as successful response: %+v, %v", activity, err)
		}
	}
}

func TestTokenActivityBoundsUntrustedRequestMetadata(t *testing.T) {
	m, repository := persistentTokenManager(t)
	secret, token, err := m.CreateToken("owner", "agent", "edit", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r := bearerRequest(http.MethodGet, secret)
	r.Method = strings.Repeat("A", 50)
	r.URL.Path = "/api/v1/" + strings.Repeat("x", maxActivityPathBytes*4)
	r = WithClientIP(r, strings.Repeat("x", 500))
	m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })).ServeHTTP(httptest.NewRecorder(), r)
	if err := m.FlushTokenActivity(); err != nil {
		t.Fatal(err)
	}
	activity, err := restoreTokenManager(t, repository).TokenActivity("owner", token.ID)
	if err != nil || len(activity) != 1 {
		t.Fatalf("bounded metadata failed restore: %+v, %v", activity, err)
	}
	if len(activity[0].Path) > maxActivityPathBytes || len(activity[0].Method) > 16 || len(activity[0].IP) > 64 || activity[0].Status != http.StatusNotFound {
		t.Fatalf("request metadata exceeded persistence bounds: %+v", activity[0])
	}
}

func TestDowngradedAccountCanRenameTokenWithoutRegainingEditAccess(t *testing.T) {
	m := newTokenManager(t)
	if err := m.SaveAccount("operator", "member", "edit", "operator-password-long"); err != nil {
		t.Fatal(err)
	}
	secret, token, err := m.CreateToken("operator", "agent", "edit", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateAccount("operator", "", "member", "read", ""); err != nil {
		t.Fatal(err)
	}
	name := "renamed"
	if updated, err := m.UpdateToken("operator", token.ID, TokenUpdate{Name: &name}); err != nil || updated.Name != name {
		t.Fatalf("downgraded account could not rename credential: %+v, %v", updated, err)
	}
	w := httptest.NewRecorder()
	m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("downgraded account executed mutation")
	})).ServeHTTP(w, bearerRequest(http.MethodPost, secret))
	if w.Code != http.StatusForbidden {
		t.Fatalf("downgraded token regained edit access: %d", w.Code)
	}
}
