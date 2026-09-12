package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/transport"
)

func TestEnrollmentProducerCreatesIdempotentDurableJob(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	server := contracts.Server{ID: "server-enroll-test01", Name: "Node", Role: "node", Architecture: "amd64", Platform: "linux", Capabilities: []string{"tc"}, ConnectionState: "connected", FreshnessState: "fresh"}
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	ca, err := transport.NewCertificateAuthority(now)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := ca.IssueEnrollment(server.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewEnrollmentService(store, ca)
	if err != nil {
		t.Fatal(err)
	}
	service.Now = func() time.Time { return now }
	body := enrollmentRequest{Token: enrollment.Token, IdempotencyKey: "enrollment-request-1"}
	requestBody, _ := json.Marshal(body)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/servers/"+string(server.ID)+"/enrollment", bytes.NewReader(requestBody))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", response.Code, response.Body.String())
	}
	var job contracts.Job
	if err := json.Unmarshal(response.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Kind != "enrollment" || job.State != contracts.JobQueued || job.TargetServerID != server.ID || job.Revision != 0 {
		t.Fatalf("unexpected enrollment job: %+v", job)
	}
	retry := httptest.NewRequest(http.MethodPost, "/api/v1/servers/"+string(server.ID)+"/enrollment", bytes.NewReader(requestBody))
	retryResponse := httptest.NewRecorder()
	service.Handler().ServeHTTP(retryResponse, retry)
	if retryResponse.Code != http.StatusAccepted {
		t.Fatalf("expected idempotent retry 202, got %d: %s", retryResponse.Code, retryResponse.Body.String())
	}
	var replay contracts.Job
	if err := json.Unmarshal(retryResponse.Body.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if replay.ID != job.ID || replay.Revision != job.Revision {
		t.Fatalf("retry created a different job: first=%+v retry=%+v", job, replay)
	}
}

func TestEnrollmentProducerRejectsInvalidTokenWithoutLeakingIt(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	server := contracts.Server{ID: "server-enroll-test02", Name: "Node", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "connected", FreshnessState: "fresh"}
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	ca, err := transport.NewCertificateAuthority(now)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewEnrollmentService(store, ca)
	if err != nil {
		t.Fatal(err)
	}
	service.Now = func() time.Time { return now }
	body := enrollmentRequest{Token: "invalid-token-value-1234", IdempotencyKey: "enrollment-invalid-1"}
	encoded, _ := json.Marshal(body)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/servers/"+string(server.ID)+"/enrollment", bytes.NewReader(encoded))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || bytes.Contains(response.Body.Bytes(), []byte(body.Token)) {
		t.Fatalf("invalid token response leaked or had wrong status: %d %s", response.Code, response.Body.String())
	}
}

func TestEnrollmentTokenIssuanceIsOwnerCSRFProtectedAndRetrySafe(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	server := contracts.Server{ID: "server-token-api-01", Name: "Node", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	ca, err := transport.NewCertificateAuthority(now)
	if err != nil {
		t.Fatal(err)
	}
	api, err := NewAPIWithOptions(store, "0123456789abcdef-bootstrap", Options{EnrollmentAuthority: ca})
	if err != nil {
		t.Fatal(err)
	}
	setup := httptest.NewRequest(http.MethodPost, "/api/v1/setup", bytes.NewBufferString(`{"setup_secret":"0123456789abcdef-bootstrap","password":"long-enough-password"}`))
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, setup)
	if response.Code != http.StatusCreated {
		t.Fatalf("setup=%d", response.Code)
	}
	login := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"password":"long-enough-password"}`))
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, login)
	if response.Code != http.StatusNoContent {
		t.Fatalf("login=%d", response.Code)
	}
	cookie, csrf := response.Result().Cookies()[0], response.Header().Get("X-CSRF-Token")
	path := "/api/v1/servers/" + string(server.ID) + "/enrollment-token"
	body := `{"idempotency_key":"operator-token-1"}`
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("missing csrf status=%d", response.Code)
	}
	request = httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", csrf)
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("issue status=%d body=%s", response.Code, response.Body.String())
	}
	var first enrollmentTokenResponse
	if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.ServerID != server.ID || len(first.Token) < 16 || first.ExpiresAt.Before(time.Now().UTC()) || first.ExpiresAt.Sub(time.Now().UTC()) > defaultEnrollmentTTL {
		t.Fatalf("invalid token response=%+v", first)
	}
	request = httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", csrf)
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("retry status=%d body=%s", response.Code, response.Body.String())
	}
	var replay enrollmentTokenResponse
	if err := json.Unmarshal(response.Body.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Token != first.Token || !replay.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatal("idempotent retry returned a different token")
	}
	// A different idempotency key cannot mint a second active token for the
	// same server, and the response never echoes the original token.
	request = httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{"idempotency_key":"operator-token-2"}`))
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", csrf)
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict || bytes.Contains(response.Body.Bytes(), []byte(first.Token)) {
		t.Fatalf("second token issue status=%d body=%s", response.Code, response.Body.String())
	}
}
