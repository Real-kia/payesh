package bandwidth

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBandwidthHTTPApplyIsDurableAndIdempotent(t *testing.T) {
	manager, serverID, events := bandwidthPolicyManager(t)
	service := NewService(manager)
	body := "{\"idempotency_key\":\"apply-1\",\"expected_revision\":\"0\",\"interface\":\"eth0\",\"direction\":\"outbound\",\"action\":\"block\"}"
	path := "/api/v1/servers/" + string(serverID) + "/bandwidth-policies/interface/eth0:outbound/apply"
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("apply status=%d body=%s", response.Code, response.Body.String())
	}
	eventCount := len(*events)
	request = httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	response = httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || len(*events) != eventCount {
		t.Fatalf("replay status=%d events=%v body=%s", response.Code, *events, response.Body.String())
	}
}

func TestBandwidthHTTPDoesNotAcceptBrowserManagementExclusions(t *testing.T) {
	manager, serverID, _ := bandwidthPolicyManager(t)
	service := NewService(manager)
	body := "{\"interface\":\"eth0\",\"direction\":\"outbound\",\"action\":\"block\",\"management_flows\":[]}"
	path := "/api/v1/servers/" + string(serverID) + "/bandwidth-policies/interface/eth0:outbound/preview"
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("browser exclusions accepted: status=%d body=%s", response.Code, response.Body.String())
	}
}
