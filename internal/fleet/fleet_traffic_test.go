package fleet

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Real-kia/payesh/internal/monitoring"
)

func TestFleetTrafficSettingsRequireOwnerAndCSRF(t *testing.T) {
	client, closeStore := newTokenTestAPI(t, ":memory:")
	defer closeStore()

	const settings = "/api/v1/settings/fleet-traffic"
	const history = "/api/v1/fleet/traffic"
	check := func(method, path, body string, cookie *http.Cookie, csrf string, status int) []byte {
		t.Helper()
		response := client.call(method, path, body, cookie, csrf, "")
		if response.Code != status {
			t.Fatalf("%s %s: got %d, want %d: %s", method, path, response.Code, status, response.Body)
		}
		return response.Body.Bytes()
	}
	check("GET", history, "", nil, "", 401)
	check("GET", settings, "", nil, "", 401)
	check("POST", "/api/v1/setup", `{"setup_secret":"0123456789abcdef-bootstrap","username":"owner","password":"owner-password-long"}`, nil, "", 201)
	owner, ownerCSRF := client.login("owner", "owner-password-long")
	check("POST", "/api/v1/accounts", `{"username":"editor","role":"member","permission":"edit","password":"editor-password-long"}`, owner, ownerCSRF, 201)
	editor, editorCSRF := client.login("editor", "editor-password-long")
	check("PUT", settings, `{"enabled":true}`, editor, editorCSRF, 403)
	check("PUT", settings, `{"enabled":true}`, owner, "", 403)
	check("PUT", settings, `{}`, owner, ownerCSRF, 400)

	assertHistory := func(enabled bool) {
		t.Helper()
		var result monitoring.FleetTrafficHistory
		raw := check("GET", history, "", editor, "", 200)
		if err := json.Unmarshal(raw, &result); err != nil || result.Enabled != enabled || result.Hours == nil || len(result.Hours) != 0 {
			t.Fatalf("unexpected history: %s (%v)", raw, err)
		}
	}
	assertHistory(false)
	check("PUT", settings, `{"enabled":true}`, owner, ownerCSRF, 200)
	assertHistory(true)
	check("PUT", settings, `{"enabled":false}`, owner, ownerCSRF, 200)
	assertHistory(false)
}
