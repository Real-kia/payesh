package main

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// defaultWebDir is where payesh-install places the built dashboard.
const defaultWebDir = "/usr/share/payesh/web-assets"

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// withWebAssets serves the dashboard's static files on every path the API
// does not own. Unknown paths return index.html so client-side routes such
// as /servers/ID survive a reload. Without a built dashboard in dir the API
// handler is returned unchanged.
func withWebAssets(dir string, api http.Handler) http.Handler {
	if dir == "" {
		return api
	}
	index := filepath.Join(dir, "index.html")
	if info, err := os.Stat(index); err != nil || !info.Mode().IsRegular() {
		return api
	}
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/node/") || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			api.ServeHTTP(w, r)
			return
		}
		header := w.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "no-referrer")
		clean := path.Clean("/" + r.URL.Path)
		if clean != "/" {
			if info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(clean))); err == nil && info.Mode().IsRegular() {
				if strings.HasPrefix(clean, "/assets/") {
					// Vite fingerprints asset names, so they never change.
					header.Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		header.Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, index)
	})
}
