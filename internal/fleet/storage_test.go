package fleet

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/Real-kia/payesh/internal/monitoring"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	if status.RecoverySnapshotBytes != 0 {
		t.Fatal("fresh store reported recovery snapshots")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["recovery_snapshot_bytes"]) != "0" {
		t.Fatal("fresh status omitted additive recovery field")
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

func TestStorageSettingsAPISeparatesRecoveryUsageAndPreservesPressure(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "monitor.sqlite")
	store, err := monitoring.OpenStore(ctx, path, monitoring.StoreOptions{MaxBytes: monitoring.DefaultDatabaseLimit})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE recovery_evidence(value BLOB);INSERT INTO recovery_evidence VALUES(zeroblob(1048576));UPDATE schema_meta SET version=4`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = monitoring.OpenStore(ctx, path, monitoring.StoreOptions{MaxBytes: monitoring.DefaultDatabaseLimit})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entries, err := os.ReadDir(path + ".schema-backups")
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	recoveryBytes := info.Size()
	api, err := NewAPI(store, "test-bootstrap-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.sessions.SetupWithUsername("test-bootstrap-secret", "owner", "owner-password-long"); err != nil {
		t.Fatal(err)
	}
	token, _, err := api.sessions.LoginWithUsername("storage-owner-session", "owner", "owner-password-long")
	if err != nil {
		t.Fatal(err)
	}
	liveBytes, err := store.DatabaseBytes()
	if err != nil {
		t.Fatal(err)
	}
	limit := liveBytes + recoveryBytes/2
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE storage_settings SET max_bytes=? WHERE singleton=1`, limit); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.IngestSamples(ctx, "server-storage-budget", nil, nil); !errors.Is(err, monitoring.ErrStoragePressure) {
		t.Fatalf("actual admission ignored snapshot: %v", err)
	}
	call := func(authenticated bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/storage", nil)
		if authenticated {
			req.AddCookie(&http.Cookie{Name: api.sessions.SessionCookieName(), Value: token})
		}
		w := httptest.NewRecorder()
		api.Handler().ServeHTTP(w, req)
		return w
	}
	if w := call(false); w.Code != http.StatusUnauthorized {
		t.Fatalf("recovery usage leaked without session: %d", w.Code)
	}
	w := call(true)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	var status monitoring.StorageStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	liveBytes, err = store.DatabaseBytes()
	if err != nil {
		t.Fatal(err)
	}
	if status.DatabaseBytes != liveBytes || status.RecoverySnapshotBytes != recoveryBytes {
		t.Fatalf("API did not separate physical/live recovery: %+v live=%d recovery=%d", status, liveBytes, recoveryBytes)
	}
	if status.DatabaseBytes >= limit || status.DatabaseBytes+status.RecoverySnapshotBytes <= limit || status.Settings.PressureState != "saving" {
		t.Fatalf("status obscured recovery pressure: %+v", status)
	}
}
