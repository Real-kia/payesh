package fleet

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Real-kia/payesh/internal/monitoring"
)

func logoutRequest(api *API, method, path string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	r.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	api.Handler().ServeHTTP(w, r)
	return w
}

func TestSessionLogoutRevokesOwnerAndReadOnlySessionsPersistently(t *testing.T) {
	for _, username := range []string{"owner", "viewer"} {
		t.Run(username, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "payesh.db")
			store, err := monitoring.OpenStore(context.Background(), path, monitoring.StoreOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			api, err := NewAPIWithOptions(store, "test-bootstrap-secret", Options{SecureCookies: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := api.sessions.SetupWithUsername("test-bootstrap-secret", "owner", "owner-password-long"); err != nil {
				t.Fatal(err)
			}
			if err := api.sessions.SaveAccount("viewer", "member", "read", "viewer-password-long"); err != nil {
				t.Fatal(err)
			}
			login := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"username":"`+username+`","password":"`+username+`-password-long"}`))
			w := httptest.NewRecorder()
			api.Handler().ServeHTTP(w, login)
			if w.Code != http.StatusNoContent || len(w.Result().Cookies()) != 1 {
				t.Fatalf("login: %d %s", w.Code, w.Body.String())
			}
			cookie, csrf := w.Result().Cookies()[0], w.Header().Get("X-CSRF-Token")
			w = logoutRequest(api, http.MethodDelete, "/api/v1/session", cookie, csrf)
			if w.Code != http.StatusNoContent {
				t.Fatalf("logout: %d %s", w.Code, w.Body.String())
			}
			cleared := w.Result().Cookies()
			if len(cleared) != 1 || cleared[0].Name != cookie.Name || cleared[0].Value != "" || cleared[0].MaxAge >= 0 || cleared[0].Path != "/" || !cleared[0].HttpOnly || !cleared[0].Secure || cleared[0].SameSite != http.SameSiteLaxMode {
				t.Fatalf("logout did not expire the secure browser cookie: %+v", cleared)
			}
			if w := logoutRequest(api, http.MethodGet, "/api/v1/servers", cookie, ""); w.Code != http.StatusUnauthorized {
				t.Fatalf("logged-out cookie replay: %d", w.Code)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = monitoring.OpenStore(context.Background(), path, monitoring.StoreOptions{})
			if err != nil {
				t.Fatal(err)
			}
			api, err = NewAPI(store, "different-bootstrap-secret")
			if err != nil {
				t.Fatal(err)
			}
			if w := logoutRequest(api, http.MethodGet, "/api/v1/servers", cookie, ""); w.Code != http.StatusUnauthorized {
				t.Fatalf("logged-out session resurrected after store reopen: %d", w.Code)
			}
		})
	}
}

func TestSessionLogoutRequiresItsCSRFTokenWithoutRevokingOnFailure(t *testing.T) {
	store, err := monitoring.OpenStore(context.Background(), ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api, err := NewAPI(store, "test-bootstrap-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.sessions.SetupWithUsername("test-bootstrap-secret", "owner", "owner-password-long"); err != nil {
		t.Fatal(err)
	}
	token, csrf, err := api.sessions.LoginWithUsername("test-session", "owner", "owner-password-long")
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: api.sessions.SessionCookieName(), Value: token}
	for _, bad := range []string{"", "wrong-csrf-token"} {
		w := logoutRequest(api, http.MethodDelete, "/api/v1/session", cookie, bad)
		if w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 {
			t.Fatalf("invalid CSRF logout: status=%d cookies=%v", w.Code, w.Result().Cookies())
		}
		if w := logoutRequest(api, http.MethodGet, "/api/v1/servers", cookie, ""); w.Code != http.StatusOK {
			t.Fatalf("failed CSRF logout revoked session: %d", w.Code)
		}
	}
	if w := logoutRequest(api, http.MethodDelete, "/api/v1/session", nil, csrf); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous logout: %d", w.Code)
	}
	if w := logoutRequest(api, http.MethodDelete, "/api/v1/session", cookie, csrf); w.Code != http.StatusNoContent {
		t.Fatalf("valid logout after invalid attempts: %d", w.Code)
	}
}
