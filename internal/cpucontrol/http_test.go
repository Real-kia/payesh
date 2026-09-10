package cpucontrol

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Real-kia/payesh/internal/contracts"
)

func newTestService(t *testing.T) (*Service, contracts.ServerID) {
	t.Helper()
	manager, serverID := testManager(t)
	return NewService(manager), serverID
}

func doCPUJSON(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestHTTPPreviewApplyRevertLifecycle(t *testing.T) {
	service, serverID := newTestService(t)
	base := "/api/v1/servers/" + string(serverID) + "/cpu-policies/service/nginx.service/"

	previewRec := doCPUJSON(t, service.Handler(), http.MethodPost, base+"preview", previewRequest{Millicores: 1000, TotalCores: 4})
	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview failed: %d %s", previewRec.Code, previewRec.Body.String())
	}

	applyRec := doCPUJSON(t, service.Handler(), http.MethodPost, base+"apply", applyRequest{IdempotencyKey: "apply-1", ExpectedRevision: 0, Millicores: 1000})
	if applyRec.Code != http.StatusOK {
		t.Fatalf("apply failed: %d %s", applyRec.Code, applyRec.Body.String())
	}
	var applied contracts.ControlPolicy
	if err := json.Unmarshal(applyRec.Body.Bytes(), &applied); err != nil {
		t.Fatal(err)
	}
	if applied.State != contracts.ControlPolicyApplied {
		t.Fatalf("expected applied, got %s", applied.State)
	}

	// Idempotent replay must return the same result, not re-run apply.
	replay := doCPUJSON(t, service.Handler(), http.MethodPost, base+"apply", applyRequest{IdempotencyKey: "apply-1", ExpectedRevision: 0, Millicores: 1000})
	if replay.Code != http.StatusOK {
		t.Fatalf("expected idempotent replay, got %d %s", replay.Code, replay.Body.String())
	}

	listRec := doCPUJSON(t, service.Handler(), http.MethodGet, "/api/v1/servers/"+string(serverID)+"/cpu-policies", nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list failed: %d %s", listRec.Code, listRec.Body.String())
	}
	var list struct {
		Items []contracts.ControlPolicy `json:"items"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].State != contracts.ControlPolicyApplied {
		t.Fatalf("unexpected list: %+v", list.Items)
	}

	revertRec := doCPUJSON(t, service.Handler(), http.MethodPost, base+"revert", revertRequest{IdempotencyKey: "revert-1", ExpectedRevision: applied.Revision})
	if revertRec.Code != http.StatusOK {
		t.Fatalf("revert failed: %d %s", revertRec.Code, revertRec.Body.String())
	}
	var reverted contracts.ControlPolicy
	if err := json.Unmarshal(revertRec.Body.Bytes(), &reverted); err != nil {
		t.Fatal(err)
	}
	if reverted.State != contracts.ControlPolicyReverted {
		t.Fatalf("expected reverted, got %s", reverted.State)
	}
}

func TestHTTPApplyProcessGroupRequiresPID(t *testing.T) {
	service, serverID := newTestService(t)
	path := "/api/v1/servers/" + string(serverID) + "/cpu-policies/process-group/batch-1/apply"
	rec := doCPUJSON(t, service.Handler(), http.MethodPost, path, applyRequest{IdempotencyKey: "k1", ExpectedRevision: 0, Millicores: 500})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without process_pid, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPRevisionConflictSurfacesAs409(t *testing.T) {
	service, serverID := newTestService(t)
	path := "/api/v1/servers/" + string(serverID) + "/cpu-policies/service/svc/apply"
	if rec := doCPUJSON(t, service.Handler(), http.MethodPost, path, applyRequest{IdempotencyKey: "k1", ExpectedRevision: 0, Millicores: 500}); rec.Code != http.StatusOK {
		t.Fatalf("first apply failed: %d %s", rec.Code, rec.Body.String())
	}
	rec := doCPUJSON(t, service.Handler(), http.MethodPost, path, applyRequest{IdempotencyKey: "k2", ExpectedRevision: 0, Millicores: 750})
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a stale revision, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPUnknownRouteIs404(t *testing.T) {
	service, serverID := newTestService(t)
	if rec := doCPUJSON(t, service.Handler(), http.MethodGet, "/api/v1/servers/"+string(serverID)+"/traffic", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unrelated resource, got %d", rec.Code)
	}
	if rec := doCPUJSON(t, service.Handler(), http.MethodPost, "/api/v1/servers/"+string(serverID)+"/cpu-policies/service/svc/explode", applyRequest{IdempotencyKey: "k"}); rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unsupported action, got %d", rec.Code)
	}
}
