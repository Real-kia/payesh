package geo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func testClient(providers ...Provider) *Client {
	return &Client{HTTP: &http.Client{Timeout: 2 * time.Second}, Providers: providers, CoolDown: time.Hour}
}

func server(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts.URL
}

var publicIP = netip.MustParseAddr("8.8.8.8")

func TestCountryParsesBothProviders(t *testing.T) {
	countryIs := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/8.8.8.8" {
			t.Errorf("country.is path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"ip":"8.8.8.8","country":"us"}`))
	})
	code, err := testClient(CountryIs(countryIs)).Country(context.Background(), publicIP)
	if err != nil || code != "US" {
		t.Fatalf("country.is: got %q, %v", code, err)
	}
	ipinfo := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/country" {
			t.Errorf("ipinfo self path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte("DE\n"))
	})
	code, err = testClient(IPInfo(ipinfo)).Country(context.Background(), netip.Addr{})
	if err != nil || code != "DE" {
		t.Fatalf("ipinfo self: got %q, %v", code, err)
	}
}

func TestCountryFallsBackAndRejectsBadAnswers(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"server error": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) },
		"html":         func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>busy</html>")) },
		"unknown code": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"country":"XX"}`)) },
		"empty":        func(w http.ResponseWriter, r *http.Request) {},
	} {
		t.Run(name, func(t *testing.T) {
			bad := server(t, handler)
			good := server(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("NL")) })
			code, err := testClient(CountryIs(bad), IPInfo(good)).Country(context.Background(), publicIP)
			if err != nil || code != "NL" {
				t.Fatalf("fallback: got %q, %v", code, err)
			}
			if _, err := testClient(CountryIs(bad)).Country(context.Background(), publicIP); err == nil {
				t.Fatal("a bad answer must be an error, not a country")
			}
		})
	}
}

func TestRateLimitedProviderCoolsDown(t *testing.T) {
	var hits atomic.Int32
	limited := server(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	})
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	client := testClient(CountryIs(limited))
	client.Now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if _, err := client.Country(context.Background(), publicIP); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("attempt %d: got %v, want ErrRateLimited", i, err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("provider was called %d times during its cool-down", hits.Load())
	}
	now = now.Add(2 * time.Hour)
	_, _ = client.Country(context.Background(), publicIP)
	if hits.Load() != 2 {
		t.Fatal("provider was not retried after its cool-down")
	}
}

func TestPrivateAddressesAreNeverSent(t *testing.T) {
	called := false
	url := server(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	for _, raw := range []string{"10.0.0.1", "192.168.1.5", "127.0.0.1", "100.64.1.1", "fd00::1", "169.254.1.1"} {
		if _, err := testClient(CountryIs(url)).Country(context.Background(), netip.MustParseAddr(raw)); err == nil {
			t.Errorf("%s: expected an error", raw)
		}
	}
	if called {
		t.Fatal("a private address was sent to a provider")
	}
}

type fakeStore struct {
	candidates []monitoring.LocationCandidate
	saved      map[contracts.ServerID]string
	failures   map[contracts.ServerID]int
	cleared    map[contracts.ServerID]bool
	nextAt     map[contracts.ServerID]time.Time
}

func (f *fakeStore) LocationCandidates(context.Context, int) ([]monitoring.LocationCandidate, error) {
	return f.candidates, nil
}

func (f *fakeStore) SaveServerCountry(_ context.Context, id contracts.ServerID, code, _ string, _ time.Time) error {
	f.saved[id] = code
	return nil
}

func (f *fakeStore) RecordServerCountryFailure(_ context.Context, id contracts.ServerID, _ string, attempts int, next time.Time, clear bool) error {
	f.failures[id], f.nextAt[id], f.cleared[id] = attempts, next, clear
	return nil
}

func newFake(candidates ...monitoring.LocationCandidate) *fakeStore {
	return &fakeStore{candidates: candidates, saved: map[contracts.ServerID]string{}, failures: map[contracts.ServerID]int{}, cleared: map[contracts.ServerID]bool{}, nextAt: map[contracts.ServerID]time.Time{}}
}

func TestResolverPicksTargetsAndSkipsFreshEntries(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	store := newFake(
		monitoring.LocationCandidate{ServerID: "hub", Role: "standalone"},
		monitoring.LocationCandidate{ServerID: "by-address", Role: "node", Address: "203.0.113.10:22"},
		monitoring.LocationCandidate{ServerID: "by-host", Role: "node", Address: "node.example"},
		monitoring.LocationCandidate{ServerID: "by-peer", Role: "node", Address: "10.0.0.4", PeerIP: "198.51.100.7"},
		monitoring.LocationCandidate{ServerID: "unlocatable", Role: "node", Address: "192.168.0.2"},
		monitoring.LocationCandidate{ServerID: "fresh", Role: "node", Address: "203.0.113.20", LookupIP: "203.0.113.20", CountryCode: "FR", NextAttemptAt: now.Add(time.Hour)},
	)
	var asked []string
	r := &Resolver{Store: store, Now: func() time.Time { return now }, Sleep: func(context.Context, time.Duration) error { return nil },
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("10.1.1.1"), netip.MustParseAddr("192.0.2.50")}, nil
		},
		Lookup: func(_ context.Context, ip netip.Addr) (string, error) {
			if !ip.IsValid() {
				asked = append(asked, "self")
			} else {
				asked = append(asked, ip.String())
			}
			return "SE", nil
		}}
	r.Pass(context.Background())
	if got := strings.Join(asked, ","); got != "self,203.0.113.10,192.0.2.50,198.51.100.7" {
		t.Fatalf("lookups = %s", got)
	}
	if len(store.saved) != 4 || store.saved["unlocatable"] != "" || store.saved["fresh"] != "" {
		t.Fatalf("saved = %v", store.saved)
	}
}

func TestResolverBacksOffAndClearsOnAddressChange(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	store := newFake(
		monitoring.LocationCandidate{ServerID: "retry", Role: "node", Address: "203.0.113.10", LookupIP: "203.0.113.10", Attempts: 3, NextAttemptAt: now.Add(-time.Minute)},
		monitoring.LocationCandidate{ServerID: "moved", Role: "node", Address: "203.0.113.30", LookupIP: "203.0.113.29", CountryCode: "DE", Attempts: 5, NextAttemptAt: now.Add(time.Hour)},
	)
	var logs []string
	r := &Resolver{Store: store, Now: func() time.Time { return now }, Sleep: func(context.Context, time.Duration) error { return nil },
		Logf:   func(format string, args ...any) { logs = append(logs, format) },
		Lookup: func(context.Context, netip.Addr) (string, error) { return "", errors.New("offline") }}
	r.Pass(context.Background())
	if store.failures["retry"] != 4 || store.cleared["retry"] || !store.nextAt["retry"].Equal(now.Add(2*time.Hour)) {
		t.Fatalf("retry: attempts=%d cleared=%v next=%v", store.failures["retry"], store.cleared["retry"], store.nextAt["retry"])
	}
	if store.failures["moved"] != 1 || !store.cleared["moved"] {
		t.Fatalf("moved: an address change must retry now and drop the old country")
	}
	if len(logs) != 1 {
		t.Fatalf("only the first failure per server should be logged, got %d logs", len(logs))
	}
}

func TestResolverStopsPassWhenRateLimited(t *testing.T) {
	store := newFake(
		monitoring.LocationCandidate{ServerID: "a", Role: "node", Address: "203.0.113.1"},
		monitoring.LocationCandidate{ServerID: "b", Role: "node", Address: "203.0.113.2"},
	)
	calls := 0
	r := &Resolver{Store: store, Sleep: func(context.Context, time.Duration) error { return nil }, Logf: func(string, ...any) {},
		Lookup: func(context.Context, netip.Addr) (string, error) { calls++; return "", ErrRateLimited }}
	r.Pass(context.Background())
	if calls != 1 {
		t.Fatalf("pass continued after every provider was rate limited (%d calls)", calls)
	}
}

func TestBackoffCapsAtOneDay(t *testing.T) {
	if backoff(1) != 15*time.Minute || backoff(2) != 30*time.Minute || backoff(30) != 24*time.Hour {
		t.Fatalf("backoff = %v %v %v", backoff(1), backoff(2), backoff(30))
	}
}
