package fleet

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestFleetRolloutTagSelectionVersionAndResume(t *testing.T) {
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scheduler := updater.NewScheduler(store, nil)
	api, err := NewAPIWithOptions(store, "test-bootstrap-secret", Options{UpdateScheduler: scheduler})
	if err != nil {
		t.Fatal(err)
	}
	if err := api.sessions.SetupWithUsername("test-bootstrap-secret", "owner", "owner-password-long"); err != nil {
		t.Fatal(err)
	}
	owner, csrf, err := api.sessions.LoginWithUsername("tag-rollout-owner", "owner", "owner-password-long")
	if err != nil {
		t.Fatal(err)
	}

	var ids []contracts.ServerID
	for i := 0; i < 22; i++ {
		node := contracts.Server{
			ID:   contracts.ServerID(fmt.Sprintf("node-tag-rollout-%03d", i)),
			Name: fmt.Sprintf("Node %d", 22-i), Role: "node", Platform: "linux", Architecture: "amd64",
			ConnectionState: "connected", FreshnessState: "fresh", Version: "1.1.0",
			Capabilities: []string{"node-version:1.0.5"},
		}
		if err := store.EnsureServer(ctx, node); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SetServerTags(ctx, node.ID, []string{"Canary Fleet"}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, node.ID)
	}
	request := func(method, path, body, csrf string, status int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: api.sessions.SessionCookieName(), Value: owner})
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		api.Handler().ServeHTTP(w, req)
		if w.Code != status {
			t.Fatalf("%s %s: got %d want %d, body=%s", method, path, w.Code, status, w.Body.String())
		}
		return w
	}
	jobFrom := func(response *httptest.ResponseRecorder) contracts.Job {
		t.Helper()
		var job contracts.Job
		if err := json.Unmarshal(response.Body.Bytes(), &job); err != nil {
			t.Fatal(err)
		}
		return job
	}
	body := `{"release":"1.2.0","tag":"canary fleet","idempotency_key":"tag-rollout","canary":1,"max_fail_pct":0}`
	parent := jobFrom(request("POST", "/updates", body, csrf, 202))
	targets, err := store.ListUpdateTargets(ctx, parent.ID)
	if err != nil || len(targets) != 22 || targets[0].TargetServerID != ids[0] {
		t.Fatalf("unexpected tag selection: %d targets, err=%v", len(targets), err)
	}
	if _, err := store.SetServerTags(ctx, ids[0], nil); err != nil {
		t.Fatal(err)
	}
	replayed := jobFrom(request("POST", "/updates", body, csrf, 202))
	if replayed.ID != parent.ID {
		t.Fatalf("tag membership change created a second job: %s", replayed.ID)
	}
	request("POST", "/updates", strings.Replace(body, "canary fleet", "other", 1), csrf, 409)
	request("POST", "/updates", strings.Replace(body, `"canary":1`, `"canary":1,"selected_server_ids":["node-tag-rollout-000"]`, 1), csrf, 400)

	paused, err := scheduler.RunOnce(ctx, parent.ID)
	if err != nil || paused.State != contracts.JobPaused {
		t.Fatalf("job=%+v err=%v", paused, err)
	}
	current := jobFrom(request("GET", "/jobs/"+parent.ID, "", "", 200))
	if !current.Resumable || current.State != contracts.JobPaused {
		t.Fatalf("paused state was not exposed: %+v", current)
	}
	url := "/updates/" + parent.ID + "/resume"
	payload := fmt.Sprintf(`{"expected_revision":"%d"}`, current.Revision)
	request("POST", url, payload, "", 403)
	request("POST", url, `{"expected_revision":"0"}`, csrf, 409)
	resumed := jobFrom(request("POST", url, payload, csrf, 202))
	if resumed.Resumable || resumed.State != contracts.JobRunning {
		t.Fatalf("unexpected resumed job: %+v", resumed)
	}

	selected, err := json.Marshal(ids)
	if err != nil {
		t.Fatal(err)
	}
	explicit := fmt.Sprintf(`{"release":"1.2.0","selected_server_ids":%s,"idempotency_key":"large-id-selection"}`, selected)
	request("POST", "/updates", explicit, csrf, 202)
	page := request("GET", "/servers", "", csrf, 200)
	var list monitoring.ServerPage
	if err := json.Unmarshal(page.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) == 0 || list.Items[0].AgentVersion != "1.0.5" || list.Items[0].Version != "1.1.0" {
		t.Fatalf("agent and release versions were not independently reported: %+v", list.Items)
	}
}
