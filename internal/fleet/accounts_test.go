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

func TestAccountPermissionsAndPasswordReset(t *testing.T) {
	store, err := monitoring.OpenStore(context.Background(), ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api, err := NewAPI(store, "0123456789abcdef-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		w := httptest.NewRecorder()
		api.Handler().ServeHTTP(w, r)
		return w
	}
	if w := call("POST", "/api/v1/setup", `{"setup_secret":"0123456789abcdef-bootstrap","username":"owner","password":"owner-password-long"}`, nil, ""); w.Code != 201 {
		t.Fatalf("setup: %d %s", w.Code, w.Body.String())
	}
	owner := call("POST", "/api/v1/session", `{"username":"owner","password":"owner-password-long"}`, nil, "")
	if owner.Code != 204 {
		t.Fatalf("owner login: %d", owner.Code)
	}
	oc, csrf := owner.Result().Cookies()[0], owner.Header().Get("X-CSRF-Token")
	if w := call("POST", "/api/v1/accounts", `{"username":"viewer","role":"member","permission":"read","password":"viewer-password-long"}`, oc, csrf); w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	viewer := call("POST", "/api/v1/session", `{"username":"viewer","password":"viewer-password-long"}`, nil, "")
	if viewer.Code != 204 {
		t.Fatalf("viewer login: %d", viewer.Code)
	}
	vc, vcsrf := viewer.Result().Cookies()[0], viewer.Header().Get("X-CSRF-Token")
	if w := call("GET", "/api/v1/servers", "", vc, ""); w.Code != 200 {
		t.Fatalf("viewer read: %d", w.Code)
	}
	if w := call("POST", "/api/v1/servers", `{"name":"blocked","address":"192.0.2.1"}`, vc, vcsrf); w.Code != 403 {
		t.Fatalf("viewer write: %d", w.Code)
	}
	if w := call("GET", "/api/v1/accounts", "", vc, ""); w.Code != 403 {
		t.Fatalf("viewer account list: %d", w.Code)
	}
	if w := call("PATCH", "/api/v1/accounts/viewer", `{"username":"editor","role":"admin","permission":"edit","password":"new-password-long"}`, oc, csrf); w.Code != 204 {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", "/api/v1/servers", "", vc, ""); w.Code != 401 {
		t.Fatalf("old viewer session: %d", w.Code)
	}
	if w := call("POST", "/api/v1/session", `{"username":"viewer","password":"viewer-password-long"}`, nil, ""); w.Code != 401 {
		t.Fatalf("old credentials: %d", w.Code)
	}
	editor := call("POST", "/api/v1/session", `{"username":"editor","password":"new-password-long"}`, nil, "")
	if editor.Code != 204 {
		t.Fatalf("editor login: %d", editor.Code)
	}
	ec, ecsrf := editor.Result().Cookies()[0], editor.Header().Get("X-CSRF-Token")
	if w := call("POST", "/api/v1/servers", `{"name":"allowed","address":"192.0.2.2"}`, ec, ecsrf); w.Code != 201 {
		t.Fatalf("editor write: %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", "/api/v1/accounts", "", ec, ""); w.Code != 403 {
		t.Fatalf("editor account list: %d", w.Code)
	}
	if w := call("DELETE", "/api/v1/accounts/editor", "", oc, csrf); w.Code != 204 {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := call("GET", "/api/v1/servers", "", ec, ""); w.Code != 401 {
		t.Fatalf("deleted session: %d", w.Code)
	}
	if w := call("PATCH", "/api/v1/accounts/owner", `{"username":"primary","password":"rotated-password-long"}`, oc, csrf); w.Code != 204 {
		t.Fatalf("owner update: %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", "/api/v1/servers", "", oc, ""); w.Code != 401 {
		t.Fatalf("old owner session: %d", w.Code)
	}
	if w := call("POST", "/api/v1/session", `{"username":"primary","password":"rotated-password-long"}`, nil, ""); w.Code != 204 {
		t.Fatalf("new owner login: %d", w.Code)
	}
}

func TestDelegatedAccountSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payesh.db")
	store, err := monitoring.OpenStore(context.Background(), path, monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	api, err := NewAPI(store, "0123456789abcdef-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		w := httptest.NewRecorder()
		api.Handler().ServeHTTP(w, r)
		return w
	}
	if w := request("POST", "/api/v1/setup", `{"setup_secret":"0123456789abcdef-bootstrap","password":"owner-password-long"}`, nil, ""); w.Code != 201 {
		t.Fatalf("setup: %d", w.Code)
	}
	owner := request("POST", "/api/v1/session", `{"password":"owner-password-long"}`, nil, "")
	if owner.Code != 204 {
		t.Fatalf("owner login: %d", owner.Code)
	}
	if w := request("POST", "/api/v1/accounts", `{"username":"auditor","role":"member","permission":"read","password":"auditor-password-long"}`, owner.Result().Cookies()[0], owner.Header().Get("X-CSRF-Token")); w.Code != 201 {
		t.Fatalf("create: %d", w.Code)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = monitoring.OpenStore(context.Background(), path, monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api, err = NewAPI(store, "different-secret-still-long-enough")
	if err != nil {
		t.Fatal(err)
	}
	member := request("POST", "/api/v1/session", `{"username":"auditor","password":"auditor-password-long"}`, nil, "")
	if member.Code != 204 {
		t.Fatalf("restored login: %d %s", member.Code, member.Body.String())
	}
	if w := request("GET", "/api/v1/account/me", "", member.Result().Cookies()[0], ""); w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"permission":"read"`)) {
		t.Fatalf("restored account: %d %s", w.Code, w.Body.String())
	}
}
