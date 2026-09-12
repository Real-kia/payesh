package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/install"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/updater"
)

func TestUpdateProducerIsAuthenticatedCSRFProtectedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	server := contracts.Server{ID: "server-update-api-01", Name: "node", Role: "node", Platform: "linux", Architecture: "amd64", ConnectionState: "connected", FreshnessState: "fresh"}
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	scheduler := updater.NewScheduler(store, nil)
	svc, err := NewUpdateService(store, scheduler)
	if err != nil {
		t.Fatal(err)
	}
	svc.Now = func() time.Time { return now }
	api, err := NewAPIWithOptions(store, "0123456789abcdef-bootstrap", Options{UpdateScheduler: scheduler})
	if err != nil {
		t.Fatal(err)
	}
	setupAndLogin := func() (*http.Cookie, string) {
		setup := httptest.NewRequest(http.MethodPost, "/api/v1/setup", bytes.NewBufferString(`{"secret":"0123456789abcdef-bootstrap","password":"long-enough-password"}`))
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
		return response.Result().Cookies()[0], response.Header().Get("X-CSRF-Token")
	}
	cookie, csrf := setupAndLogin()
	body := `{"release":"1.2.3","selected_server_ids":["server-update-api-01"],"idempotency_key":"update-api-1"}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/updates", bytes.NewBufferString(body))
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("update without csrf=%d", response.Code)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/updates", bytes.NewBufferString(body))
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", csrf)
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("update=%d body=%s", response.Code, response.Body.String())
	}
	var first contracts.Job
	if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/updates", bytes.NewBufferString(body))
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", csrf)
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("replay=%d body=%s", response.Code, response.Body.String())
	}
	var replay contracts.Job
	if err := json.Unmarshal(response.Body.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if replay.ID != first.ID {
		t.Fatalf("replay created a new job: first=%s replay=%s", first.ID, replay.ID)
	}
}

func TestInstallJobKeepsCredentialsOutOfDurableJobAndRunsFakeSSH(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	server := contracts.Server{ID: "server-install-api-01", Name: "node", Role: "node", Platform: "linux", Architecture: "amd64", ConnectionState: "connected", FreshnessState: "fresh"}
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	service, err := NewInstallService(store)
	if err != nil {
		t.Fatal(err)
	}
	service.Now = func() time.Time { return now }
	var got install.SSHInstallOptions
	var gotPassword string
	service.Executor = SSHInstallerFunc(func(_ context.Context, opts install.SSHInstallOptions) (install.SSHInstallResult, error) {
		got = opts
		gotPassword = string(opts.Auth.Password)
		return install.SSHInstallResult{Stage: "complete"}, nil
	})
	job, err := service.Enqueue(ctx, installRequest{ServerID: string(server.ID), Mode: "ssh", Host: "192.0.2.10", Port: 2222, User: "root", Password: "secret", Role: "node", IdempotencyKey: "install-api-1"})
	if err != nil {
		t.Fatal(err)
	}
	if job.Kind != sshInstallJobKind || job.State != contracts.JobQueued {
		t.Fatalf("unexpected job: %+v", job)
	}
	if job.Action != nil {
		t.Fatal("installation credentials/action unexpectedly persisted")
	}
	completed, err := service.RunOnce(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != contracts.JobSucceeded || gotPassword != "secret" || got.Endpoint.Port != 2222 {
		t.Fatalf("unexpected execution: job=%+v opts=%+v", completed, got)
	}
	if _, err := service.RunOnce(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	// A changed request under the same key is rejected without exposing the
	// original secret in the error or durable job state.
	_, err = service.Enqueue(ctx, installRequest{ServerID: string(server.ID), Mode: "ssh", Host: "other.example", Port: 2222, User: "root", Password: "secret", Role: "node", IdempotencyKey: "install-api-1"})
	if !errors.Is(err, monitoring.ErrJobIdempotencyConflict) {
		t.Fatalf("expected idempotency conflict, got %v", err)
	}
}

func TestInstallWorkerSettlesRestartedJobWithoutCredentials(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	server := contracts.Server{ID: "server-install-restart-1", Name: "node", Role: "node", Platform: "linux", Architecture: "amd64", ConnectionState: "connected", FreshnessState: "fresh"}
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	original, err := NewInstallService(store)
	if err != nil {
		t.Fatal(err)
	}
	original.Now = func() time.Time { return now }
	original.Executor = SSHInstallerFunc(func(context.Context, install.SSHInstallOptions) (install.SSHInstallResult, error) {
		return install.SSHInstallResult{}, nil
	})
	job, err := original.Enqueue(ctx, installRequest{ServerID: string(server.ID), Host: "192.0.2.20", Port: 22, User: "root", Password: "secret", IdempotencyKey: "restart-install-1"})
	if err != nil {
		t.Fatal(err)
	}
	// A fresh service models a process restart: the durable row remains but
	// the ephemeral credential handoff is intentionally gone.
	restarted, err := NewInstallService(store)
	if err != nil {
		t.Fatal(err)
	}
	restarted.Now = func() time.Time { return now }
	restarted.Executor = SSHInstallerFunc(func(context.Context, install.SSHInstallOptions) (install.SSHInstallResult, error) {
		return install.SSHInstallResult{}, nil
	})
	if err := restarted.ProcessPending(ctx); err != nil {
		t.Fatal(err)
	}
	settled, found, err := store.GetJob(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("settled job found=%v err=%v", found, err)
	}
	if settled.State != contracts.JobFailed || settled.Error == nil || settled.Error.Code != "credentials_unavailable" {
		t.Fatalf("restart settlement=%+v", settled)
	}
}

func TestUnconfiguredInstallEndpointReturnsServiceUnavailable(t *testing.T) {
	store, err := monitoring.OpenStore(context.Background(), ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewInstallService(store)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/installations", bytes.NewBufferString(`{"server_id":"server-install-api-01","host":"192.0.2.30","port":22,"user":"root","password":"secret","idempotency_key":"unconfigured-1"}`))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !bytes.Contains(response.Body.Bytes(), []byte("install_executor_unavailable")) {
		t.Fatalf("unconfigured install response=%d body=%s", response.Code, response.Body.String())
	}
}
