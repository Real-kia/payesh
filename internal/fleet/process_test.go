package fleet

import (
	"bytes"
	"context"
	"github.com/Real-kia/payesh/internal/monitoring"
	"net/http"
	"net/http/httptest"
	"testing"
)

type processTestService struct{ calls int }

func (s *processTestService) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.calls++; w.WriteHeader(200) })
}
func TestProcessViewRequiresBrowserSession(t *testing.T) {
	store, e := monitoring.OpenStore(context.Background(), ":memory:", monitoring.StoreOptions{})
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	service := &processTestService{}
	api, e := NewAPIWithOptions(store, "0123456789abcdef-bootstrap", Options{ProcessMonitoringService: service})
	if e != nil {
		t.Fatal(e)
	}
	path := "/api/v1/servers/server-process-012345/processes"
	w := httptest.NewRecorder()
	api.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != 401 || service.calls != 0 {
		t.Fatal("process route bypassed browser authentication")
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
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.AddCookie(w.Result().Cookies()[0])
	w = httptest.NewRecorder()
	api.Handler().ServeHTTP(w, request)
	if w.Code != 200 || service.calls != 1 {
		t.Fatalf("authenticated route failed: %d", w.Code)
	}
}
