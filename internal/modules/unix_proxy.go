package modules

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"time"
)

// NewUnixModuleProxy returns a bounded transport bridge to a separately
// installed module. Browser authentication/CSRF remains owned by fleet.API;
// Unix socket permissions are the local process boundary.
func NewUnixModuleProxy(socketPath string) (http.Handler, error) {
	return newUnixModuleProxy(socketPath, "")
}

// NewAuthenticatedUnixModuleProxy adds the module-level shared-secret
// header. Unix socket ownership remains the first boundary; the token is a
// second boundary for deployments where the server and module do not share a
// trusted service-manager group.
func NewAuthenticatedUnixModuleProxy(socketPath, token string) (http.Handler, error) {
	if token == "" || len(token) > 4096 {
		return nil, errors.New("module proxy token must be non-empty and bounded")
	}
	return newUnixModuleProxy(socketPath, token)
}

func newUnixModuleProxy(socketPath, token string) (http.Handler, error) {
	if !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath {
		return nil, errors.New("module socket path must be clean and absolute")
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			dialer := net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
			return dialer.DialContext(ctx, "unix", socketPath)
		},
		MaxIdleConns: 4, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
	}
	target := &url.URL{Scheme: "http", Host: "module.local"}
	proxy := httputil.NewSingleHostReverseProxy(target)
	if token != "" {
		originalDirector := proxy.Director
		proxy.Director = func(request *http.Request) {
			originalDirector(request)
			request.Header.Set("X-Payesh-Module-Token", token)
		}
	}
	proxy.Transport = transport
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"module_executor_unavailable","message":"module is unavailable","retryable":true}` + "\n"))
	}
	return proxy, nil
}
