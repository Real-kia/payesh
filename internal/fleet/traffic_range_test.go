package fleet

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/traffic"
)

func TestTrafficUsageRangeRequiresSessionAndValidBounds(t *testing.T) {
	store, err := monitoring.OpenStore(context.Background(), ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := traffic.NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	service.Now = func() time.Time { return time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC) }
	api, err := NewAPIWithOptions(store, "0123456789abcdef-bootstrap", Options{TrafficService: service})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/servers/server-test-traffic-01/traffic/usage?from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z"
	w := httptest.NewRecorder()
	api.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != 401 {
		t.Fatalf("range bypassed authentication: %d", w.Code)
	}
	w = httptest.NewRecorder()
	api.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/setup", bytes.NewBufferString(`{"secret":"0123456789abcdef-bootstrap","password":"long-enough-password"}`)))
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	w = httptest.NewRecorder()
	api.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"password":"long-enough-password"}`)))
	if w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	for _, tc := range []struct {
		query  string
		status int
	}{
		{path, 200},
		{path + "&to=invalid", 200}, // duplicate params use the first value
		{"/api/v1/servers/server-test-traffic-01/traffic/usage?from=invalid", 400},
		{"/api/v1/servers/server-test-traffic-01/traffic/usage?from=2026-01-01T00:30:00Z&to=2026-01-02T00:00:00Z", 400},
		{"/api/v1/servers/server-test-traffic-01/traffic/usage?from=2026-01-01T00:00:00Z&to=2026-01-03T00:00:00Z", 400},
	} {
		r := httptest.NewRequest(http.MethodGet, tc.query, nil)
		r.AddCookie(cookie)
		w = httptest.NewRecorder()
		api.Handler().ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s: status=%d body=%s", tc.query, w.Code, w.Body.String())
		}
	}
}
