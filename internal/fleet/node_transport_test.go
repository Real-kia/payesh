package fleet

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/transport"
)

func TestNodeTransportSettingsRequireSessionCSRFAndEditPermission(t *testing.T) {
	store, err := monitoring.OpenStore(context.Background(), ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	calls := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) })
	api, err := NewAPIWithOptions(store, "test-bootstrap-secret", Options{NodeTransportSettings: handler})
	if err != nil {
		t.Fatal(err)
	}
	if err = api.sessions.SetupWithUsername("test-bootstrap-secret", "owner", "owner-password-long"); err != nil {
		t.Fatal(err)
	}
	owner, csrf, err := api.sessions.LoginWithUsername("owner-session", "owner", "owner-password-long")
	if err != nil {
		t.Fatal(err)
	}
	if err = api.sessions.SaveAccount("viewer", "member", "read", "viewer-password-long"); err != nil {
		t.Fatal(err)
	}
	viewer, viewerCSRF, err := api.sessions.LoginWithUsername("viewer-session", "viewer", "viewer-password-long")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, token, csrf string
		want                int
	}{
		{"GET", "", "", 401}, {"GET", viewer, "", 200}, {"PATCH", owner, "", 403}, {"PATCH", viewer, viewerCSRF, 403}, {"DELETE", viewer, viewerCSRF, 403}, {"PATCH", owner, csrf, 200}, {"DELETE", owner, csrf, 200},
	} {
		path := "/api/v1/settings/node-transport"
		if tc.method == http.MethodGet {
			path += "?limit=17&cursor=bm9kZS1hbmNob3I"
		}
		req := httptest.NewRequest(tc.method, path, nil)
		if tc.token != "" {
			req.AddCookie(&http.Cookie{Name: api.sessions.SessionCookieName(), Value: tc.token})
		}
		req.Header.Set("X-CSRF-Token", tc.csrf)
		response := httptest.NewRecorder()
		api.Handler().ServeHTTP(response, req)
		if response.Code != tc.want {
			t.Fatalf("%s expected %d got %d", tc.method, tc.want, response.Code)
		}
	}
	if calls != 3 {
		t.Fatalf("unauthorized requests reached node settings: calls=%d", calls)
	}
}

// Exercise the real settings handler through session middleware so the contract
// includes its distinct JSON success/conflict and plain-text malformed responses.
func TestNodeTransportSettingsHTTPContract(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	_ = reservation.Close()
	ports, err := transport.NewNodePorts(filepath.Join(t.TempDir(), "ports.json"), address, store,
		func(port string) string { return "wss://hub.example.test:" + port + "/node/v1" }, func(net.Listener) {})
	if err != nil {
		t.Fatal(err)
	}
	defer ports.Close()
	api, err := NewAPIWithOptions(store, "test-bootstrap-secret", Options{NodeTransportSettings: ports.Handler()})
	if err != nil {
		t.Fatal(err)
	}
	if err = api.sessions.SetupWithUsername("test-bootstrap-secret", "owner", "owner-password-long"); err != nil {
		t.Fatal(err)
	}
	owner, csrf, err := api.sessions.LoginWithUsername("owner-contract-session", "owner", "owner-password-long")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, body, media string
		want                      int
	}{
		{"status", "GET", "", "application/json", 200},
		{"malformed", "PATCH", "{", "text/plain", 400},
		{"unknown field", "PATCH", `{"port":9797,"extra":true}`, "text/plain", 400},
		{"multiple values", "PATCH", `{"port":9797} {"port":9798}`, "text/plain", 400},
		{"oversized", "PATCH", `{"port":9797}` + strings.Repeat(" ", 1024), "text/plain", 400},
		{"invalid port", "PATCH", `{"port":0}`, "application/json", 409},
		{"retire empty history", "DELETE", "", "application/json", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/v1/settings/node-transport", strings.NewReader(tc.body))
			req.AddCookie(&http.Cookie{Name: api.sessions.SessionCookieName(), Value: owner})
			req.Header.Set("X-CSRF-Token", csrf)
			response := httptest.NewRecorder()
			api.Handler().ServeHTTP(response, req)
			if response.Code != tc.want || !strings.HasPrefix(response.Header().Get("Content-Type"), tc.media) {
				t.Fatalf("status=%d content-type=%q body=%s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
			}
			if tc.want == 200 {
				var status transport.NodePortStatus
				if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
					t.Fatal(err)
				}
				_, port, _ := net.SplitHostPort(address)
				if status.Port != port || status.Pending != 0 || status.Nodes == nil || status.PreviousPorts == nil {
					t.Fatalf("status=%+v", status)
				}
			} else if tc.want == 409 {
				var failure struct {
					Code      string `json:"code"`
					Retryable bool   `json:"retryable"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
					t.Fatal(err)
				}
				if failure.Code != "node_port_unavailable" || failure.Retryable {
					t.Fatalf("failure=%+v", failure)
				}
			}
		})
	}
}
