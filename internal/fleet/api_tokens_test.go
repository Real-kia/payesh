package fleet

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Real-kia/payesh/internal/monitoring"
)

type tokenTestClient struct {
	t   *testing.T
	api *API
}

func (c tokenTestClient) call(method, path, body string, cookie *http.Cookie, csrf, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	// Token routes require HTTPS; simulate TLS terminated by the server.
	r.TLS = &tls.ConnectionState{}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	c.api.Handler().ServeHTTP(w, r)
	return w
}

func (c tokenTestClient) login(username, password string) (*http.Cookie, string) {
	c.t.Helper()
	w := c.call("POST", "/api/v1/session", `{"username":"`+username+`","password":"`+password+`"}`, nil, "", "")
	if w.Code != 204 {
		c.t.Fatalf("login %s: %d", username, w.Code)
	}
	return w.Result().Cookies()[0], w.Header().Get("X-CSRF-Token")
}

func (c tokenTestClient) createToken(cookie *http.Cookie, csrf, body string) string {
	c.t.Helper()
	w := c.call("POST", "/api/v1/api-tokens", body, cookie, csrf, "")
	if w.Code != 201 {
		c.t.Fatalf("create token: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || !strings.HasPrefix(created.Token, "pyt_") || created.ID == "" {
		c.t.Fatalf("created token response: %s", w.Body.String())
	}
	return created.Token
}

func newTokenTestAPI(t *testing.T, path string) (tokenTestClient, func()) {
	t.Helper()
	store, err := monitoring.OpenStore(context.Background(), path, monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	api, err := NewAPI(store, "0123456789abcdef-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	return tokenTestClient{t: t, api: api}, func() { store.Close() }
}

func TestAPITokensAuthenticateWithoutCSRFAndKeepTheirBoundaries(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "hub.db")
	c, closeStore := newTokenTestAPI(t, dbPath)
	if w := c.call("POST", "/api/v1/setup", `{"setup_secret":"0123456789abcdef-bootstrap","username":"owner","password":"owner-password-long"}`, nil, "", ""); w.Code != 201 {
		t.Fatalf("setup: %d", w.Code)
	}
	oc, ocsrf := c.login("owner", "owner-password-long")
	if w := c.call("POST", "/api/v1/accounts", `{"username":"viewer","role":"member","permission":"read","password":"viewer-password-long"}`, oc, ocsrf, ""); w.Code != 201 {
		t.Fatalf("create viewer: %d", w.Code)
	}

	editToken := c.createToken(oc, ocsrf, `{"name":"agent","permission":"edit"}`)
	readToken := c.createToken(oc, ocsrf, `{"name":"reader","permission":"read","expires_in_days":7}`)

	// Tokens read and, with edit permission, mutate without a CSRF header.
	if w := c.call("GET", "/api/v1/servers", "", nil, "", readToken); w.Code != 200 {
		t.Fatalf("read token read: %d", w.Code)
	}
	if w := c.call("POST", "/api/v1/servers", `{"name":"node","address":"192.0.2.1"}`, nil, "", editToken); w.Code != 201 {
		t.Fatalf("edit token write: %d %s", w.Code, w.Body.String())
	}
	if w := c.call("POST", "/api/v1/servers", `{"name":"node","address":"192.0.2.2"}`, nil, "", readToken); w.Code != 403 {
		t.Fatalf("read token write: %d", w.Code)
	}
	if w := c.call("GET", "/api/v1/servers", "", nil, "", "pyt_not-a-real-token"); w.Code != 401 {
		t.Fatalf("unknown token: %d", w.Code)
	}
	// A bad bearer token is not rescued by a valid cookie on the same request.
	if w := c.call("GET", "/api/v1/servers", "", oc, "", "pyt_not-a-real-token"); w.Code != 401 {
		t.Fatalf("bad token with cookie: %d", w.Code)
	}
	var me struct{ Username, Permission string }
	w := c.call("GET", "/api/v1/account/me", "", nil, "", readToken)
	if json.Unmarshal(w.Body.Bytes(), &me); w.Code != 200 || me.Username != "owner" || me.Permission != "read" {
		t.Fatalf("token account: %d %s", w.Code, w.Body.String())
	}

	// Tokens cannot create credentials: no token or account management.
	if w := c.call("POST", "/api/v1/api-tokens", `{"name":"again"}`, nil, "", editToken); w.Code != 403 {
		t.Fatalf("token minting token: %d", w.Code)
	}
	if w := c.call("GET", "/api/v1/api-tokens", "", nil, "", editToken); w.Code != 403 {
		t.Fatalf("token listing tokens: %d", w.Code)
	}
	if w := c.call("POST", "/api/v1/accounts", `{"username":"intruder","role":"admin","permission":"edit","password":"intruder-password"}`, nil, "", editToken); w.Code != 403 {
		t.Fatalf("token creating account: %d", w.Code)
	}

	// Read-only accounts can create read tokens but not edit tokens, and only
	// see their own tokens.
	vc, vcsrf := c.login("viewer", "viewer-password-long")
	if w := c.call("POST", "/api/v1/api-tokens", `{"name":"escalate","permission":"edit"}`, vc, vcsrf, ""); w.Code != 400 {
		t.Fatalf("read account edit token: %d", w.Code)
	}
	if w := c.call("POST", "/api/v1/api-tokens", `{"name":"no-csrf"}`, vc, "", ""); w.Code != 403 {
		t.Fatalf("token creation without csrf: %d", w.Code)
	}
	viewerToken := c.createToken(vc, vcsrf, `{"name":"viewer-agent"}`)
	var list struct {
		Items []struct{ ID, Username, Name string } `json:"items"`
	}
	w = c.call("GET", "/api/v1/api-tokens", "", vc, "", "")
	if json.Unmarshal(w.Body.Bytes(), &list); w.Code != 200 || len(list.Items) != 1 || list.Items[0].Username != "viewer" || strings.Contains(w.Body.String(), viewerToken) {
		t.Fatalf("viewer token list: %d %s", w.Code, w.Body.String())
	}
	w = c.call("GET", "/api/v1/api-tokens", "", oc, "", "")
	if json.Unmarshal(w.Body.Bytes(), &list); len(list.Items) != 3 || strings.Contains(w.Body.String(), editToken) {
		t.Fatalf("owner token list: %s", w.Body.String())
	}
	var readTokenID string
	for _, item := range list.Items {
		if item.Name == "reader" {
			readTokenID = item.ID
		}
	}
	if w := c.call("DELETE", "/api/v1/api-tokens/"+readTokenID, "", vc, vcsrf, ""); w.Code != 404 {
		t.Fatalf("viewer revoking owner token: %d", w.Code)
	}

	// Tokens survive a restart; revoked tokens and password resets do not.
	closeStore()
	c, closeStore = newTokenTestAPI(t, dbPath)
	defer func() { closeStore() }()
	oc, ocsrf = c.login("owner", "owner-password-long")
	if w := c.call("GET", "/api/v1/servers", "", nil, "", readToken); w.Code != 200 {
		t.Fatalf("token after restart: %d", w.Code)
	}
	if w := c.call("DELETE", "/api/v1/api-tokens/"+readTokenID, "", oc, ocsrf, ""); w.Code != 204 {
		t.Fatalf("revoke: %d", w.Code)
	}
	if w := c.call("GET", "/api/v1/servers", "", nil, "", readToken); w.Code != 401 {
		t.Fatalf("revoked token: %d", w.Code)
	}
	if w := c.call("PATCH", "/api/v1/accounts/viewer", `{"password":"viewer-password-new"}`, oc, ocsrf, ""); w.Code != 204 {
		t.Fatalf("password reset: %d %s", w.Code, w.Body.String())
	}
	if w := c.call("GET", "/api/v1/servers", "", nil, "", viewerToken); w.Code != 401 {
		t.Fatalf("token after password reset: %d", w.Code)
	}
}

func TestMCPEndpointRequiresTokenAndCallsTheAPI(t *testing.T) {
	c, closeStore := newTokenTestAPI(t, ":memory:")
	defer closeStore()
	if w := c.call("POST", "/api/v1/setup", `{"setup_secret":"0123456789abcdef-bootstrap","username":"owner","password":"owner-password-long"}`, nil, "", ""); w.Code != 201 {
		t.Fatalf("setup: %d", w.Code)
	}
	oc, ocsrf := c.login("owner", "owner-password-long")
	readToken := c.createToken(oc, ocsrf, `{"name":"reader"}`)
	editToken := c.createToken(oc, ocsrf, `{"name":"agent","permission":"edit"}`)

	rpc := func(token, body string) (int, map[string]any) {
		w := c.call("POST", "/api/v1/mcp", body, oc, ocsrf, token)
		var message map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &message)
		return w.Code, message
	}
	toolText := func(message map[string]any) (string, bool) {
		result, _ := message["result"].(map[string]any)
		content, _ := result["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("tool result: %v", message)
		}
		isError, _ := result["isError"].(bool)
		return content[0].(map[string]any)["text"].(string), isError
	}

	// A browser session alone cannot reach the MCP endpoint.
	if w := c.call("POST", "/api/v1/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, oc, ocsrf, ""); w.Code != 401 {
		t.Fatalf("session on mcp: %d", w.Code)
	}
	if status, message := rpc(readToken, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`); status != 200 || message["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize: %d %v", status, message)
	}

	// A read token can call read tools through MCP; POST to /mcp is not a
	// mutation of Payesh state.
	if w := c.call("POST", "/api/v1/servers", `{"name":"web-1","address":"192.0.2.10"}`, nil, "", editToken); w.Code != 201 {
		t.Fatalf("seed server: %d", w.Code)
	}
	_, message := rpc(readToken, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_servers","arguments":{}}}`)
	if text, isError := toolText(message); isError || !strings.Contains(text, "web-1") {
		t.Fatalf("list_servers: %s", text)
	}

	// Mutations through api_request keep the token's permission.
	call := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"api_request","arguments":{"method":"POST","path":"/api/v1/servers","body":{"name":"web-2","address":"192.0.2.11"}}}}`
	_, message = rpc(readToken, call)
	if text, isError := toolText(message); !isError || !strings.Contains(text, "HTTP 403") {
		t.Fatalf("read token mutation via mcp: %s", text)
	}
	_, message = rpc(editToken, call)
	if text, isError := toolText(message); isError || !strings.Contains(text, "web-2") {
		t.Fatalf("edit token mutation via mcp: %s", text)
	}
	// The generic tool cannot reach credential management either.
	_, message = rpc(editToken, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"api_request","arguments":{"method":"POST","path":"/api/v1/api-tokens","body":{"name":"x"}}}}`)
	if text, isError := toolText(message); !isError || !strings.Contains(text, "HTTP 403") {
		t.Fatalf("token minting via mcp: %s", text)
	}
	_, message = rpc(readToken, `{"jsonrpc":"2.0","id":5,"method":"resources/read","params":{"uri":"payesh://openapi.yaml"}}`)
	if contents, _ := message["result"].(map[string]any)["contents"].([]any); len(contents) != 1 || !strings.Contains(contents[0].(map[string]any)["text"].(string), "openapi: 3.0.3") {
		t.Fatalf("openapi resource: %v", message)
	}
}

func TestAPITokensAndMCPRequireHTTPS(t *testing.T) {
	store, err := monitoring.OpenStore(context.Background(), ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api, err := NewAPIWithOptions(store, "0123456789abcdef-bootstrap", Options{TrustedProxyCIDRs: []string{"192.0.2.1/32"}})
	if err != nil {
		t.Fatal(err)
	}
	c := tokenTestClient{t: t, api: api}
	if w := c.call("POST", "/api/v1/setup", `{"setup_secret":"0123456789abcdef-bootstrap","username":"owner","password":"owner-password-long"}`, nil, "", ""); w.Code != 201 {
		t.Fatalf("setup: %d", w.Code)
	}
	oc, ocsrf := c.login("owner", "owner-password-long")
	token := c.createToken(oc, ocsrf, `{"name":"agent"}`)

	plain := func(method, path, remote string, header http.Header, withCookie bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		r.RemoteAddr = remote
		for key, values := range header {
			r.Header[key] = values
		}
		if withCookie {
			r.AddCookie(oc)
			r.Header.Set("X-CSRF-Token", ocsrf)
		}
		w := httptest.NewRecorder()
		api.Handler().ServeHTTP(w, r)
		return w
	}
	bearer := http.Header{"Authorization": {"Bearer " + token}}
	for _, check := range []struct {
		name, method, path string
		header             http.Header
		cookie             bool
	}{
		{"token on REST", "GET", "/api/v1/servers", bearer, false},
		{"MCP", "POST", "/api/v1/mcp", bearer, false},
		{"token management", "GET", "/api/v1/api-tokens", nil, true},
		{"token creation", "POST", "/api/v1/api-tokens", nil, true},
	} {
		w := plain(check.method, check.path, "203.0.113.5:4000", check.header, check.cookie)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "https_required") {
			t.Fatalf("%s over HTTP: %d %s", check.name, w.Code, w.Body.String())
		}
	}
	// Browser sessions keep working over HTTP; only tokens are restricted.
	if w := plain("GET", "/api/v1/servers", "203.0.113.5:4000", nil, true); w.Code != 200 {
		t.Fatalf("session over HTTP: %d", w.Code)
	}
	// Forwarded HTTPS counts only from a configured trusted proxy.
	https := http.Header{"Authorization": {"Bearer " + token}, "X-Forwarded-Proto": {"https"}}
	if w := plain("GET", "/api/v1/servers", "203.0.113.5:4000", https, false); w.Code != http.StatusForbidden {
		t.Fatalf("spoofed X-Forwarded-Proto: %d", w.Code)
	}
	if w := plain("GET", "/api/v1/servers", "192.0.2.1:4000", https, false); w.Code != 200 {
		t.Fatalf("trusted proxy X-Forwarded-Proto: %d %s", w.Code, w.Body.String())
	}
	forwarded := http.Header{"Authorization": {"Bearer " + token}, "Forwarded": {`for=198.51.100.7;proto=https`}}
	if w := plain("GET", "/api/v1/servers", "192.0.2.1:4000", forwarded, false); w.Code != 200 {
		t.Fatalf("trusted proxy Forwarded: %d", w.Code)
	}
	// MCP tool calls behind the proxy are replayed as HTTPS requests too.
	r := httptest.NewRequest("POST", "/api/v1/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami"}}`))
	r.RemoteAddr = "192.0.2.1:4000"
	r.Header = https.Clone()
	w := httptest.NewRecorder()
	api.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"isError":false`) {
		t.Fatalf("MCP tool call behind trusted proxy: %d %s", w.Code, w.Body.String())
	}
	downgraded := http.Header{"Authorization": {"Bearer " + token}, "X-Forwarded-Proto": {"https, http"}}
	if w := plain("GET", "/api/v1/servers", "192.0.2.1:4000", downgraded, false); w.Code != http.StatusForbidden {
		t.Fatalf("proxy reporting HTTP: %d", w.Code)
	}
}

func TestAPITokenManagementLifecycle(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "hub.db")
	c, closeStore := newTokenTestAPI(t, dbPath)
	defer func() { closeStore() }()
	if w := c.call("POST", "/api/v1/setup", `{"setup_secret":"0123456789abcdef-bootstrap","username":"owner","password":"owner-password-long"}`, nil, "", ""); w.Code != 201 {
		t.Fatalf("setup: %d", w.Code)
	}
	oc, csrf := c.login("owner", "owner-password-long")
	if w := c.call("POST", "/api/v1/accounts", `{"username":"viewer","role":"member","permission":"read","password":"viewer-password-long"}`, oc, csrf, ""); w.Code != 201 {
		t.Fatalf("create viewer: %d", w.Code)
	}
	vc, vcsrf := c.login("viewer", "viewer-password-long")
	viewerToken := c.createToken(vc, vcsrf, `{"name":"viewer-token"}`)
	secret := c.createToken(oc, csrf, `{"name":"original","permission":"edit"}`)
	var list struct {
		Items []struct {
			ID, Name, Username, Status string
			CreatedAt                  string `json:"created_at"`
			LastUsedAt                 string `json:"last_used_at"`
			LastUsedIP                 string `json:"last_used_ip"`
		} `json:"items"`
	}
	readList := func(path string, cookie *http.Cookie) {
		t.Helper()
		w := c.call("GET", path, "", cookie, "", "")
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil {
			t.Fatalf("list: %d %s", w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("token response is cacheable")
		}
	}
	readList("/api/v1/api-tokens?username=owner&search=ORIGINAL", oc)
	if len(list.Items) != 1 || list.Items[0].CreatedAt == "" || list.Items[0].Status == "" {
		t.Fatalf("filtered metadata: %+v", list.Items)
	}
	id := list.Items[0].ID
	for _, check := range []struct {
		method, path, body string
		code               int
	}{
		{"GET", "/api/v1/api-tokens?username=owner", "", 403},
		{"PATCH", "/api/v1/api-tokens/" + id, `{"name":"stolen"}`, 404},
		{"POST", "/api/v1/api-tokens/" + id + "/rotate", `{}`, 404},
		{"GET", "/api/v1/api-tokens/" + id + "/activity", "", 404},
		{"DELETE", "/api/v1/api-tokens?username=owner", "", 403},
	} {
		w := c.call(check.method, check.path, check.body, vc, vcsrf, "")
		if w.Code != check.code {
			t.Fatalf("ownership %s %s: %d %s", check.method, check.path, w.Code, w.Body.String())
		}
	}
	for _, days := range []string{"-1", "366", "9223372036854775807"} {
		for _, check := range []struct{ method, path, body string }{
			{"POST", "/api/v1/api-tokens", `{"name":"overflow","expires_in_days":` + days + `}`},
			{"PATCH", "/api/v1/api-tokens/" + id, `{"expires_in_days":` + days + `}`},
			{"POST", "/api/v1/api-tokens/" + id + "/rotate", `{"expires_in_days":` + days + `}`},
		} {
			if w := c.call(check.method, check.path, check.body, oc, csrf, ""); w.Code != 400 {
				t.Fatalf("invalid lifetime: %d %s", w.Code, w.Body.String())
			}
		}
	}
	if w := c.call("PATCH", "/api/v1/api-tokens/"+id, `{"name":"renamed","permission":"read","expires_in_days":180,"actions":["read"]}`, oc, csrf, ""); w.Code != 200 {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	if w := c.call("POST", "/api/v1/servers", `{"name":"denied","address":"192.0.2.1"}`, nil, "", secret); w.Code != 403 {
		t.Fatalf("downgraded token: %d", w.Code)
	}
	if w := c.call("GET", "/api/v1/servers", "", nil, "", secret); w.Code != 200 {
		t.Fatalf("token use: %d %s", w.Code, w.Body.String())
	}
	w := c.call("GET", "/api/v1/api-tokens/"+id+"/activity", "", oc, "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "/api/v1/servers") || strings.Contains(w.Body.String(), secret) {
		t.Fatalf("activity: %d %s", w.Code, w.Body.String())
	}
	// The server flushes batched token activity on shutdown.
	if err := c.api.FlushTokenActivity(); err != nil {
		t.Fatal(err)
	}
	closeStore()
	c, closeStore = newTokenTestAPI(t, dbPath)
	oc, csrf = c.login("owner", "owner-password-long")
	vc, vcsrf = c.login("viewer", "viewer-password-long")
	readList("/api/v1/api-tokens?username=owner", oc)
	if len(list.Items) != 1 || list.Items[0].Name != "renamed" || list.Items[0].LastUsedAt == "" || list.Items[0].LastUsedIP == "" {
		t.Fatalf("persisted metadata: %+v", list.Items)
	}
	w = c.call("POST", "/api/v1/api-tokens/"+id+"/rotate", `{"expires_in_days":90}`, oc, csrf, "")
	var rotated struct{ Token string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rotated) != nil || rotated.Token == "" {
		t.Fatalf("rotate: %d %s", w.Code, w.Body.String())
	}
	if w := c.call("GET", "/api/v1/servers", "", nil, "", secret); w.Code != 401 {
		t.Fatalf("old token after rotation: %d", w.Code)
	}
	if w := c.call("GET", "/api/v1/servers", "", nil, "", rotated.Token); w.Code != 200 {
		t.Fatalf("new token: %d", w.Code)
	}
	if w := c.call("DELETE", "/api/v1/api-tokens", "", oc, csrf, ""); w.Code != 400 {
		t.Fatalf("owner bulk revoke must specify user: %d", w.Code)
	}
	if w := c.call("DELETE", "/api/v1/api-tokens?username=viewer", "", oc, csrf, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"revoked":1`) {
		t.Fatalf("bulk revoke: %d %s", w.Code, w.Body.String())
	}
	if w := c.call("GET", "/api/v1/servers", "", nil, "", viewerToken); w.Code != 401 {
		t.Fatalf("bulk revoked token: %d", w.Code)
	}
	if w := c.call("GET", "/api/v1/servers", "", nil, "", rotated.Token); w.Code != 200 {
		t.Fatalf("other account unaffected: %d", w.Code)
	}
}

func TestScopedTokensFilterServersAndConstrainMCP(t *testing.T) {
	c, closeStore := newTokenTestAPI(t, ":memory:")
	defer closeStore()
	if w := c.call("POST", "/api/v1/setup", `{"setup_secret":"0123456789abcdef-bootstrap","username":"owner","password":"owner-password-long"}`, nil, "", ""); w.Code != 201 {
		t.Fatalf("setup: %d", w.Code)
	}
	oc, csrf := c.login("owner", "owner-password-long")
	var ids []string
	for _, name := range []string{"allowed", "hidden"} {
		w := c.call("POST", "/api/v1/servers", `{"name":"`+name+`","address":"192.0.2.1"}`, oc, csrf, "")
		var server struct{ ID string }
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &server) != nil || server.ID == "" {
			t.Fatalf("create server: %d %s", w.Code, w.Body.String())
		}
		ids = append(ids, server.ID)
	}
	token := c.createToken(oc, csrf, `{"name":"monitor","permission":"edit","server_ids":["`+ids[0]+`"],"actions":["monitoring"]}`)
	w := c.call("GET", "/api/v1/servers", "", nil, "", token)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "allowed") || strings.Contains(w.Body.String(), "hidden") {
		t.Fatalf("filtered list: %d %s", w.Code, w.Body.String())
	}
	if w := c.call("GET", "/api/v1/servers/"+ids[0], "", nil, "", token); w.Code != 200 {
		t.Fatalf("allowed server: %d %s", w.Code, w.Body.String())
	}
	if w := c.call("GET", "/api/v1/servers/"+ids[1], "", nil, "", token); w.Code != 403 {
		t.Fatalf("hidden server: %d %s", w.Code, w.Body.String())
	}
	if w := c.call("POST", "/api/v1/servers", `{"name":"forbidden","address":"192.0.2.3"}`, nil, "", token); w.Code != 403 {
		t.Fatalf("monitoring mutation: %d %s", w.Code, w.Body.String())
	}
	for _, check := range []struct {
		name, arguments string
		fail            bool
	}{
		{"list_servers", `{}`, false},
		{"api_request", `{"method":"GET","path":"/api/v1/servers/` + ids[1] + `"}`, true},
		{"api_request", `{"method":"POST","path":"/api/v1/servers","body":{"name":"forbidden","address":"192.0.2.4"}}`, true},
		{"api_request", `{"method":"GET","path":"/api/v1/accounts"}`, true},
	} {
		w := c.call("POST", "/api/v1/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+check.name+`","arguments":`+check.arguments+`}}`, nil, "", token)
		if w.Code != 200 {
			t.Fatalf("MCP transport: %d %s", w.Code, w.Body.String())
		}
		var rpc struct {
			Result struct {
				IsError bool `json:"isError"`
			}
		}
		if json.Unmarshal(w.Body.Bytes(), &rpc) != nil || rpc.Result.IsError != check.fail {
			t.Fatalf("MCP scopes: %s", w.Body.String())
		}
		if !check.fail && (!strings.Contains(w.Body.String(), "allowed") || strings.Contains(w.Body.String(), "hidden")) {
			t.Fatalf("MCP filtered list: %s", w.Body.String())
		}
	}
}

func TestOwnerCannotTakeOverAnotherAccountsToken(t *testing.T) {
	c, closeStore := newTokenTestAPI(t, ":memory:")
	defer closeStore()
	if w := c.call("POST", "/api/v1/setup", `{"setup_secret":"0123456789abcdef-bootstrap","username":"owner","password":"owner-password-long"}`, nil, "", ""); w.Code != 201 {
		t.Fatalf("setup: %d", w.Code)
	}
	oc, ocsrf := c.login("owner", "owner-password-long")
	if w := c.call("POST", "/api/v1/accounts", `{"username":"operator","role":"admin","permission":"edit","password":"operator-password-long"}`, oc, ocsrf, ""); w.Code != 201 {
		t.Fatalf("create account: %d", w.Code)
	}
	pc, pcsrf := c.login("operator", "operator-password-long")
	w := c.call("POST", "/api/v1/api-tokens", `{"name":"operator-agent","actions":["monitoring"]}`, pc, pcsrf, "")
	var created struct{ ID, Token string }
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatalf("create token: %d", w.Code)
	}
	// Rotating would hand the owner a secret that acts as the operator, and
	// editing could widen the operator's token without their knowledge.
	if w := c.call("POST", "/api/v1/api-tokens/"+created.ID+"/rotate", `{}`, oc, ocsrf, ""); w.Code != 404 {
		t.Fatalf("owner rotated another account's token: %d %s", w.Code, w.Body.String())
	}
	if w := c.call("PATCH", "/api/v1/api-tokens/"+created.ID, `{"actions":[]}`, oc, ocsrf, ""); w.Code != 404 {
		t.Fatalf("owner edited another account's token: %d", w.Code)
	}
	if w := c.call("GET", "/api/v1/servers", "", nil, "", created.Token); w.Code != 200 {
		t.Fatalf("operator token stopped working: %d", w.Code)
	}
	// Oversight remains: the owner can inspect and revoke it.
	if w := c.call("GET", "/api/v1/api-tokens/"+created.ID+"/activity", "", oc, "", ""); w.Code != 200 {
		t.Fatalf("owner activity view: %d", w.Code)
	}
	if w := c.call("DELETE", "/api/v1/api-tokens/"+created.ID, "", oc, ocsrf, ""); w.Code != 204 {
		t.Fatalf("owner revoke: %d", w.Code)
	}
}
