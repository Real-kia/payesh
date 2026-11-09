package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Real-kia/payesh/internal/alerts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func TestNotificationDestinationRoutes(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	vault, err := alerts.OpenVault(filepath.Join(t.TempDir(), "notification.key"))
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan string, 4)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- r.Header.Get("Authorization") + "|" + string(body)
	}))
	defer receiver.Close()
	manager := alerts.NewDestinationManager(store, vault, []alerts.NotificationDestination{{Kind: "webhook", URL: "https://env.example.com/SECRET-ENV-PATH", Secret: "env-secret", Enabled: true}})
	api, err := NewAPIWithOptions(store, "0123456789abcdef-bootstrap", Options{NotificationDestinations: manager})
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
		t.Fatalf("setup: %d", w.Code)
	}
	login := func(user, password string) (*http.Cookie, string) {
		w := call("POST", "/api/v1/session", `{"username":"`+user+`","password":"`+password+`"}`, nil, "")
		if w.Code != 204 {
			t.Fatalf("login %s: %d", user, w.Code)
		}
		return w.Result().Cookies()[0], w.Header().Get("X-CSRF-Token")
	}
	oc, ocsrf := login("owner", "owner-password-long")
	for _, account := range []string{`{"username":"viewer","role":"member","permission":"read","password":"viewer-password-long"}`, `{"username":"editor","role":"admin","permission":"edit","password":"editor-password-long"}`} {
		if w := call("POST", "/api/v1/accounts", account, oc, ocsrf); w.Code != 201 {
			t.Fatalf("create account: %d %s", w.Code, w.Body.String())
		}
	}
	vc, vcsrf := login("viewer", "viewer-password-long")
	ec, ecsrf := login("editor", "editor-password-long")

	if w := call("GET", "/api/v1/alerts/destinations", "", nil, ""); w.Code != 401 {
		t.Fatalf("anonymous list: %d", w.Code)
	}
	create := `{"name":"Ops","kind":"webhook","url":"` + receiver.URL + `/hook/PATH-SECRET","secret":"HMAC-SECRET","headers":{"Authorization":"Bearer PLACEHOLDER"},"allow_insecure_http":true,"allow_private_network":true,"format":"slack","filter":{"tags":["flow"],"kinds":["firing"]}}`
	if w := call("POST", "/api/v1/alerts/destinations", create, oc, ""); w.Code != 403 {
		t.Fatalf("create without CSRF: %d", w.Code)
	}
	if w := call("POST", "/api/v1/alerts/destinations", create, vc, vcsrf); w.Code != 403 {
		t.Fatalf("viewer create: %d", w.Code)
	}
	if w := call("POST", "/api/v1/alerts/destinations", create, ec, ecsrf); w.Code != 403 || !strings.Contains(w.Body.String(), "owner_required") {
		t.Fatalf("editor (non-owner) create: %d %s", w.Code, w.Body.String())
	}
	w := call("POST", "/api/v1/alerts/destinations", create, oc, ocsrf)
	if w.Code != 201 {
		t.Fatalf("owner create: %d %s", w.Code, w.Body.String())
	}
	for _, secret := range []string{"PATH-SECRET", "HMAC-SECRET", "PLACEHOLDER"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("create response leaked %q: %s", secret, w.Body.String())
		}
	}
	var created alerts.DestinationView
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID == "" || created.Format != "slack" || !created.HasSecret {
		t.Fatalf("created=%+v err=%v", created, err)
	}

	list := call("GET", "/api/v1/alerts/destinations", "", vc, "")
	if list.Code != 200 {
		t.Fatalf("viewer list: %d", list.Code)
	}
	for _, secret := range []string{"PATH-SECRET", "HMAC-SECRET", "PLACEHOLDER", "SECRET-ENV-PATH", "env-secret"} {
		if strings.Contains(list.Body.String(), secret) {
			t.Fatalf("list leaked %q: %s", secret, list.Body.String())
		}
	}
	var page struct {
		Items []alerts.DestinationView `json:"items"`
		Limit int                      `json:"limit"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil || len(page.Items) != 2 || !page.Items[0].Managed || page.Items[1].ID != created.ID || page.Limit != 20 {
		t.Fatalf("page=%+v err=%v", page, err)
	}

	if w := call("PATCH", "/api/v1/alerts/destinations/"+created.ID, `{"enabled":false,"name":"Ops chat"}`, oc, ocsrf); w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	if w := call("PATCH", "/api/v1/alerts/destinations/"+created.ID, `{"format":"template"}`, oc, ocsrf); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_destination") {
		t.Fatalf("invalid patch: %d %s", w.Code, w.Body.String())
	}
	if w := call("PATCH", "/api/v1/alerts/destinations/"+created.ID, `{"unknown_field":1}`, oc, ocsrf); w.Code != 400 {
		t.Fatalf("unknown field: %d", w.Code)
	}
	if w := call("PATCH", "/api/v1/alerts/destinations/env-webhook", `{"enabled":false}`, oc, ocsrf); w.Code != 409 || !strings.Contains(w.Body.String(), "managed_by_environment") {
		t.Fatalf("managed patch: %d %s", w.Code, w.Body.String())
	}
	if w := call("DELETE", "/api/v1/alerts/destinations/env-webhook", "", oc, ocsrf); w.Code != 409 {
		t.Fatalf("managed delete: %d", w.Code)
	}

	// Send test: owner only, delivered with the stored header, status recorded.
	if w := call("POST", "/api/v1/alerts/destinations/"+created.ID+"/test", "", ec, ecsrf); w.Code != 403 {
		t.Fatalf("editor test: %d", w.Code)
	}
	w = call("POST", "/api/v1/alerts/destinations/"+created.ID+"/test", "", oc, ocsrf)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("test send: %d %s", w.Code, w.Body.String())
	}
	if got := <-received; !strings.HasPrefix(got, "Bearer PLACEHOLDER|") || !strings.Contains(got, "test notification") {
		t.Fatalf("received=%q", got)
	}
	if w := call("POST", "/api/v1/alerts/destinations/"+created.ID+"/test", "", oc, ocsrf); w.Code != 429 {
		t.Fatalf("rapid test: %d", w.Code)
	}
	if w := call("GET", "/api/v1/alerts/destinations/"+created.ID, "", vc, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"last_delivery":{`) || !strings.Contains(w.Body.String(), "HTTP 200") {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}

	// Template preview.
	preview := call("POST", "/api/v1/alerts/destinations/preview", `{"format":"template","template":"{\"t\":{{json .Rule}}}"}`, ec, ecsrf)
	if preview.Code != 200 || !strings.Contains(preview.Body.String(), "High CPU") {
		t.Fatalf("preview: %d %s", preview.Code, preview.Body.String())
	}
	if w := call("POST", "/api/v1/alerts/destinations/preview", `{"format":"template","template":"{{.Missing}}"}`, ec, ecsrf); w.Code != 400 {
		t.Fatalf("bad preview: %d", w.Code)
	}

	if w := call("DELETE", "/api/v1/alerts/destinations/"+created.ID, "", vc, vcsrf); w.Code != 403 {
		t.Fatalf("viewer delete: %d", w.Code)
	}
	if w := call("DELETE", "/api/v1/alerts/destinations/"+created.ID, "", oc, ocsrf); w.Code != 204 {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := call("GET", "/api/v1/alerts/destinations/"+created.ID, "", oc, ""); w.Code != 404 {
		t.Fatalf("get after delete: %d", w.Code)
	}
	if w := call("PUT", "/api/v1/alerts/destinations", "{}", oc, ocsrf); w.Code != 405 {
		t.Fatalf("method: %d", w.Code)
	}
}

func TestNotificationDestinationRoutesUnavailableWithoutManager(t *testing.T) {
	store, err := monitoring.OpenStore(context.Background(), ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api, err := NewAPI(store, "0123456789abcdef-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/v1/alerts/destinations", nil)
	w := httptest.NewRecorder()
	api.Handler().ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatalf("code=%d", w.Code)
	}
}
