package monitoring

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBearerAPIDelegatesOnlyAuthenticatedTrafficForecastReads(t *testing.T) {
	store, err := OpenStore(context.Background(), ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api, err := NewAPI(store, "local-forecast-token")
	if err != nil {
		t.Fatal(err)
	}
	api.SetTrafficForecastHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("delegated method=%s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/servers/server-forecast-local-01/traffic/forecast", nil)
	request.Header.Set("Authorization", "Bearer local-forecast-token")
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delegated forecast status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/servers/server-forecast-local-01/traffic/forecast", nil)
	response = httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated forecast status=%d", response.Code)
	}
}
