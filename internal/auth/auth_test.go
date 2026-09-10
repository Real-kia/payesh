package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSetupIsOneTimeAndSessionNeedsCSRF(t *testing.T) {
	m, err := New("0123456789abcdef-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Setup("bad", "long-enough-password"); err == nil {
		t.Fatal("bad setup secret accepted")
	}
	if err := m.Setup("0123456789abcdef-bootstrap", "long-enough-password"); err != nil {
		t.Fatal(err)
	}
	if err := m.Setup("0123456789abcdef-bootstrap", "another-long-password"); err == nil {
		t.Fatal("setup was reusable")
	}
	session, csrf, err := m.Login("198.51.100.10", "long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	route := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	request := httptest.NewRequest(http.MethodPost, "/api/v1/servers", nil)
	request.AddCookie(&http.Cookie{Name: "payesh_session", Value: session})
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("csrf status=%d", response.Code)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/servers", nil)
	request.AddCookie(&http.Cookie{Name: "payesh_session", Value: session})
	request.Header.Set("X-CSRF-Token", csrf)
	response = httptest.NewRecorder()
	route.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("valid session status=%d", response.Code)
	}
}

func TestLoginThrottlesFailures(t *testing.T) {
	m, _ := New("0123456789abcdef-bootstrap")
	_ = m.Setup("0123456789abcdef-bootstrap", "long-enough-password")
	for i := 0; i < maxLoginFailures; i++ {
		if _, _, err := m.Login("key", "wrong-password"); err == nil {
			t.Fatal("wrong password accepted")
		}
	}
	if _, _, err := m.Login("key", "long-enough-password"); err == nil || err.Error() != "auth.rate_limited" {
		t.Fatalf("expected throttle, got %v", err)
	}
	m.mu.Lock()
	m.now = func() time.Time { return time.Now().Add(loginWindow + time.Second) }
	m.mu.Unlock()
	if _, _, err := m.Login("key", "long-enough-password"); err != nil {
		t.Fatalf("throttle did not expire: %v", err)
	}
}

func TestLoginThrottleEntriesExpireAndRemainBounded(t *testing.T) {
	m, err := New("0123456789abcdef-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	// Avoid doing password work for this state-boundary test. An unconfigured
	// manager still records invalid attempts, which is the attacker-controlled
	// path whose memory use must be bounded.
	for i := 0; i < maxThrottleEntries+10; i++ {
		_, _, _ = m.Login(fmt.Sprintf("key-%d", i), "wrong-password")
	}
	m.mu.Lock()
	entries := len(m.throttle)
	m.now = func() time.Time { return time.Now().Add(loginWindow + time.Second) }
	m.mu.Unlock()
	if entries > maxThrottleEntries {
		t.Fatalf("throttle map exceeded bound: %d", entries)
	}
	_, _, _ = m.Login("fresh-key", "wrong-password")
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.throttle) != 1 {
		t.Fatalf("expired throttle entries were retained: %d", len(m.throttle))
	}
}
