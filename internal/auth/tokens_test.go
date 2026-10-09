package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTokenManager(t *testing.T) *Manager {
	t.Helper()
	m, err := New("0123456789abcdef-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetupWithUsername("0123456789abcdef-bootstrap", "owner", "long-enough-password"); err != nil {
		t.Fatal(err)
	}
	if err := m.SaveAccount("viewer", "member", "read", "viewer-password-long"); err != nil {
		t.Fatal(err)
	}
	return m
}

func bearerRequest(method, token string) *http.Request {
	r := httptest.NewRequest(method, "/api/v1/servers", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func TestTokenPermissionIsCappedByAccountAndToken(t *testing.T) {
	m := newTokenManager(t)
	if _, _, err := m.CreateToken("viewer", "agent", "edit", time.Hour); err == nil {
		t.Fatal("read-only account created an edit token")
	}
	if _, _, err := m.CreateToken("ghost", "agent", "read", time.Hour); err == nil {
		t.Fatal("token created for a missing account")
	}
	if _, _, err := m.CreateToken("owner", "agent", "read", MaxTokenLifetime+time.Hour); err == nil {
		t.Fatal("token lifetime above the maximum accepted")
	}
	secret, token, err := m.CreateToken("owner", "agent", "read", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if token.Hint != secret[:8] || token.Username != "owner" {
		t.Fatalf("token metadata: %+v", token)
	}
	principal, ok := m.Authenticate(bearerRequest(http.MethodGet, secret))
	if !ok || principal.Role != "owner" || principal.Permission != "read" || principal.TokenID != token.ID {
		t.Fatalf("principal: %+v %v", principal, ok)
	}
	route := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	w := httptest.NewRecorder()
	route.ServeHTTP(w, bearerRequest(http.MethodPost, secret))
	if w.Code != http.StatusForbidden {
		t.Fatalf("read token mutation: %d", w.Code)
	}
	// Edit tokens mutate without CSRF, and a later account downgrade applies
	// to tokens that already exist.
	if err := m.SaveAccount("operator", "admin", "edit", "operator-password-long"); err != nil {
		t.Fatal(err)
	}
	edit, _, err := m.CreateToken("operator", "editor", "edit", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	route.ServeHTTP(w, bearerRequest(http.MethodPost, edit))
	if w.Code != http.StatusNoContent {
		t.Fatalf("edit token mutation without csrf: %d", w.Code)
	}
	if err := m.UpdateAccount("operator", "", "admin", "read", ""); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	route.ServeHTTP(w, bearerRequest(http.MethodPost, edit))
	if w.Code != http.StatusForbidden {
		t.Fatalf("edit token after account downgrade: %d", w.Code)
	}
}

func TestTokensExpireAndFollowAccountChanges(t *testing.T) {
	m := newTokenManager(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	short, _, err := m.CreateToken("viewer", "short", "read", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	long, _, err := m.CreateToken("viewer", "long", "read", 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	if _, ok := m.Authenticate(bearerRequest(http.MethodGet, short)); ok {
		t.Fatal("expired token accepted")
	}
	if err := m.UpdateAccount("viewer", "reader", "member", "edit", ""); err != nil {
		t.Fatal(err)
	}
	principal, ok := m.Authenticate(bearerRequest(http.MethodGet, long))
	if !ok || principal.Username != "reader" || principal.Permission != "read" {
		t.Fatalf("renamed account token: %+v %v", principal, ok)
	}
	if err := m.DeleteAccount("reader"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Authenticate(bearerRequest(http.MethodGet, long)); ok || len(m.Tokens("")) != 0 {
		t.Fatal("token outlived its account")
	}
}

func TestTokenLimitPerAccount(t *testing.T) {
	m := newTokenManager(t)
	for i := 0; i < maxTokensPerUser; i++ {
		if _, _, err := m.CreateToken("viewer", "agent", "read", time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := m.CreateToken("viewer", "agent", "read", time.Hour); err == nil {
		t.Fatal("token limit not enforced")
	}
	if _, _, err := m.CreateToken("owner", "agent", "read", time.Hour); err != nil {
		t.Fatalf("limit leaked across accounts: %v", err)
	}
}

func TestNonBearerAuthorizationFallsBackToSession(t *testing.T) {
	m := newTokenManager(t)
	session, _, err := m.LoginWithUsername("198.51.100.10", "owner", "long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/servers", nil)
	r.Header.Set("Authorization", "Basic b3duZXI6eA==")
	r.AddCookie(&http.Cookie{Name: "payesh_session", Value: session})
	principal, ok := m.Authenticate(r)
	if !ok || principal.TokenID != "" || principal.Role != "owner" {
		t.Fatalf("session principal: %+v %v", principal, ok)
	}
}
