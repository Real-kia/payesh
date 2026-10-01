package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/Real-kia/payesh/internal/monitoring"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStorageSettingsRequireOwnerAndCSRF(t *testing.T) {
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
	owner, csrf, err := api.sessions.LoginWithUsername("owner-session", "owner", "owner-password-long")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.sessions.SaveAccount("editor", "admin", "edit", "editor-password-long"); err != nil {
		t.Fatal(err)
	}
	editor, editorCSRF, err := api.sessions.LoginWithUsername("editor-session", "editor", "editor-password-long")
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, token, csrf string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		if token != "" {
			req.AddCookie(&http.Cookie{Name: api.sessions.SessionCookieName(), Value: token})
		}
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		api.Handler().ServeHTTP(w, req)
		return w
	}
	path := "/api/v1/settings/storage"
	if w := call("GET", path, "", "", nil); w.Code != 401 {
		t.Fatal("unauthenticated settings read", w.Code)
	}
	w := call("GET", path, owner, "", nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var status monitoring.StorageStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Settings.MaxDatabaseBytes != 1000000000 {
		t.Fatal("incorrect default")
	}
	status.Settings.SampleSeconds = 10
	body, _ := json.Marshal(status.Settings)
	if w := call("PUT", path, owner, "", body); w.Code != 403 {
		t.Fatal("CSRF bypass", w.Code)
	}
	if w := call("PUT", path, editor, editorCSRF, body); w.Code != 403 {
		t.Fatal("editor changed owner settings", w.Code)
	}
	if w := call("PUT", path, owner, csrf, body); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := call("PUT", path, owner, csrf, body); w.Code != 409 {
		t.Fatal("stale settings accepted", w.Code)
	}
	if w := call("GET", "/api/v1/notifications", "", "", nil); w.Code != 401 {
		t.Fatal("notifications leaked")
	}
	if w := call("POST", "/api/v1/notifications", owner, "", nil); w.Code != 403 {
		t.Fatal("notification mutation bypassed CSRF")
	}
	if w := call("POST", "/api/v1/notifications", owner, csrf, nil); w.Code != 204 {
		t.Fatal(w.Body.String())
	}
}
