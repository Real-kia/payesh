package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWithWebAssets(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html>dashboard"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app-abc.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("api")) })
	handler := withWebAssets(dir, api)

	for _, tc := range []struct {
		method, path, want, cache string
	}{
		{http.MethodGet, "/", "dashboard", "no-cache"},
		{http.MethodGet, "/servers/server-1", "dashboard", "no-cache"},
		{http.MethodGet, "/assets/app-abc.js", "console.log(1)", "immutable"},
		{http.MethodGet, "/../../etc/passwd", "invalid URL path", ""},
		{http.MethodGet, "/api/v1/servers", "api", ""},
		{http.MethodGet, "/node/v1", "api", ""},
		{http.MethodGet, "/node/bootstrap/v1", "api", ""},
		{http.MethodGet, "/healthz", "api", ""},
		{http.MethodPost, "/anything", "api", ""},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
		if !strings.Contains(recorder.Body.String(), tc.want) || !strings.Contains(recorder.Header().Get("Cache-Control"), tc.cache) {
			t.Errorf("%s %s = %q (Cache-Control %q), want %q (%q)", tc.method, tc.path, recorder.Body, recorder.Header().Get("Cache-Control"), tc.want, tc.cache)
		}
	}
	if withWebAssets(t.TempDir(), api) == nil || withWebAssets("", api) == nil {
		t.Fatal("handler missing without a dashboard build")
	}
}
