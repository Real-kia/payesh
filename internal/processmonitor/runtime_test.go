package processmonitor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestProcessHTTPFilteringAndErrors(t *testing.T) {
	root := fixture(t)
	counters(t, root, 1000, 10, 50, 100, 200)
	r := &Runtime{Collector: NewCollector(root), ServerID: "server-process-012345"}
	r.Sample(context.Background())
	handler := r.Handler()
	for _, test := range []struct {
		method, path string
		status       int
	}{{"GET", "/api/v1/servers/server-process-012345/processes?sort=memory&search=42&limit=1", 200}, {"GET", "/api/v1/servers/other-server-012345/processes", 404}, {"POST", "/api/v1/servers/server-process-012345/processes", 405}, {"GET", "/api/v1/servers/server-process-012345/processes?sort=command", 400}, {"GET", "/api/v1/servers/server-process-012345/processes?limit=1001", 400}} {
		t.Run(test.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(test.method, test.path, nil))
			if w.Code != test.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if w.Code == 200 {
				var result Snapshot
				if e := json.Unmarshal(w.Body.Bytes(), &result); e != nil {
					t.Fatal(e)
				}
				if len(result.Items) != 1 || result.Items[0].PID != 42 {
					t.Fatal("incorrect filter")
				}
			}
		})
	}
	r.snapshot.SampledAt = time.Now().Add(-time.Minute)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/servers/server-process-012345/processes", nil))
	if w.Code != 503 {
		t.Fatal("stale samples must not look live")
	}
	write(t, filepath.Join(root, "stat"), "not a counter")
	r.Sample(context.Background())
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/servers/server-process-012345/processes", nil))
	if w.Code != 503 {
		t.Fatal("sample failures must be visible")
	}
}
func TestSortUnavailableAfterMeasuredZero(t *testing.T) {
	zero := float64(0)
	items := []Process{{PID: 1}, {PID: 2, CPUPercent: &zero}}
	if e := Sort(items, "cpu"); e != nil {
		t.Fatal(e)
	}
	if items[0].PID != 2 {
		t.Fatal("unavailable value ranked above measured zero")
	}
}
