package processmonitor

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Runtime struct {
	Collector   *Collector
	ServerID    string
	mu          sync.RWMutex
	snapshot    Snapshot
	sampleError error
}

func (r *Runtime) Sample(ctx context.Context) {
	s, e := r.Collector.Sample(ctx, time.Now())
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sampleError = e
	if e == nil {
		r.snapshot = s
	}
}
func (r *Runtime) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fail := func(status int, code, message string) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": message, "retryable": status == 503})
		}
		if request.URL.Path != "/api/v1/servers/"+r.ServerID+"/processes" {
			fail(404, "not_found", "resource not found")
			return
		}
		if request.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			fail(405, "method_not_allowed", "process monitoring is read-only")
			return
		}
		query := request.URL.Query()
		sortBy := query.Get("sort")
		if sortBy == "" {
			sortBy = "cpu"
		}
		limit := 200
		if text := query.Get("limit"); text != "" {
			v, e := strconv.Atoi(text)
			if e != nil || v < 1 || v > 1000 {
				fail(400, "invalid_limit", "limit must be 1..1000")
				return
			}
			limit = v
		}
		search := strings.ToLower(strings.TrimSpace(query.Get("search")))
		if len(search) > 128 {
			fail(400, "invalid_search", "search exceeds its limit")
			return
		}
		r.mu.RLock()
		snapshot := r.snapshot
		e := r.sampleError
		items := append([]Process{}, snapshot.Items...)
		r.mu.RUnlock()
		if e != nil || snapshot.SampledAt.IsZero() || time.Since(snapshot.SampledAt) > 15*time.Second {
			fail(503, "samples_unavailable", "process samples are unavailable")
			return
		}
		filtered := items[:0]
		for _, p := range items {
			if search == "" || strings.Contains(strings.ToLower(p.Name), search) || strings.Contains(strconv.Itoa(p.PID), search) {
				filtered = append(filtered, p)
			}
		}
		if e := Sort(filtered, sortBy); e != nil {
			fail(400, "invalid_sort", "unsupported process sort")
			return
		}
		matched := len(filtered)
		if matched > limit {
			filtered = filtered[:limit]
		}
		snapshot.Items = filtered
		_ = json.NewEncoder(w).Encode(struct {
			Snapshot
			Matched int `json:"matched"`
		}{snapshot, matched})
	})
}

func (r *Runtime) Serve(ctx context.Context, socket string) error {
	if info, e := os.Lstat(socket); e == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("socket path is not a socket")
		}
		conn, e := net.DialTimeout("unix", socket, time.Second)
		if e == nil {
			conn.Close()
			return errors.New("module is already running")
		}
		if e := os.Remove(socket); e != nil {
			return e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	listener, e := net.Listen("unix", socket)
	if e != nil {
		return e
	}
	defer listener.Close()
	defer os.Remove(socket)
	if e := os.Chmod(socket, 0600); e != nil {
		return e
	}
	r.Sample(ctx)
	sampleCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-sampleCtx.Done():
				return
			case <-ticker.C:
				r.Sample(sampleCtx)
			}
		}
	}()
	server := &http.Server{Handler: r.Handler(), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 8192}
	go func() { <-sampleCtx.Done(); _ = server.Close() }()
	e = server.Serve(listener)
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}
