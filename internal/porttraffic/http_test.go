package porttraffic

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestPortTrafficHTTPConfigureIsIdempotentAndRevertable(t *testing.T) {
	manager, serverID := portTrafficManager(t)
	service := NewService(manager)
	scope := Scope{ID: "web", Protocol: TCP, Interface: "eth0", LocalPort: 443, Direction: Inbound, Tuple: TranslatedTuple}
	body := map[string]any{"idempotency_key": "scope-1", "expected_revision": "0", "scope": scope}
	encoded, _ := json.Marshal(body)
	path := "/api/v1/servers/" + string(serverID) + "/port-traffic-scopes"
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("configure status=%d body=%s", response.Code, response.Body.String())
	}
	var policy contracts.ControlPolicy
	if err := json.NewDecoder(response.Body).Decode(&policy); err != nil || policy.State != contracts.ControlPolicyPending || policy.Revision != 1 {
		t.Fatalf("configured policy=%+v err=%v", policy, err)
	}
	request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	response = httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", response.Code, response.Body.String())
	}
	var replay contracts.ControlPolicy
	if err := json.NewDecoder(response.Body).Decode(&replay); err != nil || replay.Revision != 1 {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}

	revertPath := path + "/web/revert"
	revertBody := `{"idempotency_key":"revert-1","expected_revision":"1"}`
	request = httptest.NewRequest(http.MethodPost, revertPath, bytes.NewBufferString(revertBody))
	response = httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("revert status=%d body=%s", response.Code, response.Body.String())
	}
	var reverted contracts.ControlPolicy
	if err := json.NewDecoder(response.Body).Decode(&reverted); err != nil || reverted.State != contracts.ControlPolicyReverted || reverted.Revision != 2 {
		t.Fatalf("reverted=%+v err=%v", reverted, err)
	}

	request = httptest.NewRequest(http.MethodGet, path, nil)
	response = httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"state":"reverted"`)) {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
}
