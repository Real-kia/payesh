package fleet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/updater"
)

func TestFleetRolloutRequiresOwnerAndCSRFBeforeCreatingJobs(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	node := contracts.Server{ID: "node-rollout-auth-001", Name: "node", Role: "node", Platform: "linux", Architecture: "amd64", ConnectionState: "connected", FreshnessState: "fresh"}
	if err := store.EnsureServer(ctx, node); err != nil {
		t.Fatal(err)
	}
	api, err := NewAPIWithOptions(store, "test-bootstrap-secret", Options{UpdateScheduler: updater.NewScheduler(store, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if err = api.sessions.SetupWithUsername("test-bootstrap-secret", "owner", "owner-password-long"); err != nil {
		t.Fatal(err)
	}
	owner, csrf, err := api.sessions.LoginWithUsername("rollout-owner", "owner", "owner-password-long")
	if err != nil {
		t.Fatal(err)
	}
	if err = api.sessions.SaveAccount("editor", "admin", "edit", "editor-password-long"); err != nil {
		t.Fatal(err)
	}
	editor, editorCSRF, err := api.sessions.LoginWithUsername("rollout-editor", "editor", "editor-password-long")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"release":"1.2.0","selected_server_ids":["node-rollout-auth-001"],"idempotency_key":"rollout-auth-job"}`
	for _, tc := range []struct {
		name, token, csrf, code string
		want                    int
	}{
		{"anonymous", "", "", "unauthorized", 401}, {"missing CSRF", owner, "", "csrf_required", 403}, {"delegated editor", editor, editorCSRF, "owner_required", 403}, {"owner", owner, csrf, "", 202},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/v1/updates", strings.NewReader(body))
			if tc.token != "" {
				req.AddCookie(&http.Cookie{Name: api.sessions.SessionCookieName(), Value: tc.token})
			}
			req.Header.Set("X-CSRF-Token", tc.csrf)
			w := httptest.NewRecorder()
			api.Handler().ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tc.code != "" {
				var failure struct {
					Code string `json:"code"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &failure); err != nil || failure.Code != tc.code {
					t.Fatalf("failure=%+v err=%v", failure, err)
				}
			}
			jobs, err := store.ListJobsByKind(ctx, updater.UpdateJobKind)
			if err != nil {
				t.Fatal(err)
			}
			wantJobs := 0
			if tc.want == 202 {
				wantJobs = 1
			}
			if len(jobs) != wantJobs {
				t.Fatalf("created %d jobs before owner authorization", len(jobs))
			}
		})
	}
}
