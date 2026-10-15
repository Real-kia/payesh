package monitoring

import (
	"bytes"
	"net/http"
	"sync"
	"time"
)

const (
	// metricCacheTTL matches the dashboard's 5-second window alignment, so
	// every tab and user polling the same server inside one window shares a
	// single database query.
	metricCacheTTL     = 5 * time.Second
	metricCacheEntries = 512
)

// responseCache is a small TTL cache with request coalescing for read-only
// JSON responses. Only 200 responses are stored. The zero value is ready.
type responseCache struct {
	mu       sync.Mutex
	entries  map[string]cachedResponse
	inflight map[string]*cacheCall
	now      func() time.Time
}

type cachedResponse struct {
	status  int
	header  http.Header
	body    []byte
	expires time.Time
}

type cacheCall struct {
	done   chan struct{}
	result cachedResponse
}

func (c *responseCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// serve answers from cache, joins an identical in-flight request, or runs
// produce and records its result for later callers.
func (c *responseCache) serve(w http.ResponseWriter, key string, produce func(http.ResponseWriter)) {
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]cachedResponse)
		c.inflight = make(map[string]*cacheCall)
	}
	now := c.clock()
	if entry, ok := c.entries[key]; ok && now.Before(entry.expires) {
		c.mu.Unlock()
		entry.write(w, "hit")
		return
	}
	if call, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		<-call.done
		// A leader that failed (for example, its client went away) does not
		// decide the answer for everyone else.
		if call.result.status == http.StatusOK {
			call.result.write(w, "shared")
			return
		}
		recorder := newResponseRecorder()
		produce(recorder)
		recorder.result().write(w, "miss")
		return
	}
	call := &cacheCall{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock()

	recorder := newResponseRecorder()
	func() {
		defer func() {
			c.mu.Lock()
			delete(c.inflight, key)
			if recorder.status == http.StatusOK {
				c.storeLocked(key, recorder.result(), c.clock())
			}
			call.result = recorder.result()
			c.mu.Unlock()
			close(call.done)
		}()
		produce(recorder)
	}()
	call.result.write(w, "miss")
}

func (c *responseCache) storeLocked(key string, result cachedResponse, now time.Time) {
	if len(c.entries) >= metricCacheEntries {
		for existing, entry := range c.entries {
			if !now.Before(entry.expires) {
				delete(c.entries, existing)
			}
		}
		if len(c.entries) >= metricCacheEntries {
			return
		}
	}
	result.expires = now.Add(metricCacheTTL)
	c.entries[key] = result
}

func (r cachedResponse) write(w http.ResponseWriter, state string) {
	for name, values := range r.header {
		w.Header()[name] = append([]string(nil), values...)
	}
	w.Header().Set("X-Payesh-Cache", state)
	status := r.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(r.body)
}

type responseRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{header: make(http.Header)}
}

func (r *responseRecorder) Header() http.Header { return r.header }

func (r *responseRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}

func (r *responseRecorder) result() cachedResponse {
	return cachedResponse{status: r.status, header: r.header.Clone(), body: append([]byte(nil), r.body.Bytes()...)}
}
