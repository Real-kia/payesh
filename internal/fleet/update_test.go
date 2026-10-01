package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Real-kia/payesh/internal/auth"
	"github.com/Real-kia/payesh/internal/release"
	"github.com/Real-kia/payesh/internal/version"
	"github.com/Real-kia/payesh/internal/webupdate"
)

type testReleaseChecker struct{}

func (testReleaseChecker) Latest(context.Context) (release.GitHubRelease, error) {
	return release.GitHubRelease{Version: "1.2.3", URL: "https://example.com/releases/v1.2.3"}, nil
}

func TestLatestReportsAheadOfStable(t *testing.T) {
	old := version.Value
	defer func() { version.Value = old }()
	for _, tc := range []struct {
		current          string
		available, ahead bool
	}{
		{"1.2.2", true, false}, {"1.2.3", false, false}, {"1.2.4", false, true}, {"1.2.3-beta.1", true, false}, {"1.2.4-beta.1", false, true},
	} {
		t.Run(tc.current, func(t *testing.T) {
			version.Value = tc.current
			service := &UpdateService{ReleaseChecker: testReleaseChecker{}, WebUpdateRoot: t.TempDir(), WebUpdateDir: t.TempDir()}
			w := httptest.NewRecorder()
			service.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/updates/latest", nil))
			var result struct {
				Available bool `json:"update_available"`
				Ahead     bool `json:"installed_ahead"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if w.Code != 200 || result.Available != tc.available || result.Ahead != tc.ahead {
				t.Fatalf("status=%d result=%+v", w.Code, result)
			}
		})
	}
}

func TestWebUpdateRequiresOwnerCSRFAndNewerVersion(t *testing.T) {
	old := version.Value
	version.Value = "1.2.3"
	defer func() { version.Value = old }()
	sessions, err := auth.New("test-bootstrap-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = sessions.Setup("test-bootstrap-secret", "test-password-long"); err != nil {
		t.Fatal(err)
	}
	if err = sessions.SaveAccount("editor", "admin", "edit", "editor-password-long"); err != nil {
		t.Fatal(err)
	}
	owner, csrf, err := sessions.Login("owner-test", "test-password-long")
	if err != nil {
		t.Fatal(err)
	}
	editor, editorCSRF, err := sessions.LoginWithUsername("editor-test", "editor", "editor-password-long")
	if err != nil {
		t.Fatal(err)
	}
	dir, root := t.TempDir(), t.TempDir()
	path := filepath.Join(root, "etc/systemd/system/payesh-update.service")
	if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("test worker"), 0644); err != nil {
		t.Fatal(err)
	}
	service := &UpdateService{Sessions: sessions, WebUpdateRoot: root, WebUpdateDir: dir}
	handler := sessions.Middleware(service.Handler())
	call := func(token, csrf, target string) int {
		r := httptest.NewRequest("POST", "/api/v1/updates/apply", bytes.NewBufferString(`{"version":"`+target+`"}`))
		if token != "" {
			r.AddCookie(&http.Cookie{Name: sessions.SessionCookieName(), Value: token})
		}
		r.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	for _, tc := range []struct {
		token, csrf, target string
		status              int
	}{
		{"", "", "1.2.4", 401}, {owner, "", "1.2.4", 403}, {editor, editorCSRF, "1.2.4", 403},
		{owner, csrf, "1.2.2", 409}, {owner, csrf, "1.2.3", 409}, {owner, csrf, "bad-version", 400}, {owner, csrf, "1.2.4", 202}, {owner, csrf, "1.2.5", 409},
	} {
		if got := call(tc.token, tc.csrf, tc.target); got != tc.status {
			t.Fatalf("target %s status=%d want %d", tc.target, got, tc.status)
		}
	}
	request, ok, err := webupdate.Take(dir)
	if err != nil || !ok || request.Version != "1.2.4" {
		t.Fatalf("request=%+v found=%v err=%v", request, ok, err)
	}
}
