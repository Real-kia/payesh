package geo

import (
	"context"
	"errors"
	"log"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

// Store is the slice of the monitoring store the resolver uses.
type Store interface {
	LocationCandidates(ctx context.Context, limit int) ([]monitoring.LocationCandidate, error)
	SaveServerCountry(ctx context.Context, serverID contracts.ServerID, countryCode, lookupIP string, refreshAt time.Time) error
	RecordServerCountryFailure(ctx context.Context, serverID contracts.ServerID, lookupIP string, attempts int, nextAttempt time.Time, clear bool) error
}

// selfTarget marks a lookup of the hub's own public address.
const selfTarget = "self"

// Resolver periodically fills in missing or stale server countries.
type Resolver struct {
	Store   Store
	Lookup  func(ctx context.Context, ip netip.Addr) (string, error)
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	Logf    func(format string, args ...any)
	Now     func() time.Time
	Sleep   func(ctx context.Context, d time.Duration) error

	// Interval between passes, the pause between two lookups, and the most
	// lookups one pass may make. Defaults keep traffic to a trickle.
	Interval   time.Duration
	Spacing    time.Duration
	MaxPerPass int
	// Refresh is how long a found country is trusted before re-checking.
	Refresh time.Duration
}

// NewResolver wires a resolver to the default lookup client.
func NewResolver(store Store, userAgent string) *Resolver {
	client := NewClient(userAgent)
	return &Resolver{Store: store, Lookup: client.Country}
}

// Run blocks until ctx ends. Errors never stop the loop; they are logged
// once per server and retried with exponential back-off.
func (r *Resolver) Run(ctx context.Context) {
	if err := r.sleep(ctx, 20*time.Second); err != nil {
		return
	}
	for {
		r.Pass(ctx)
		if err := r.sleep(ctx, r.interval()); err != nil {
			return
		}
	}
}

// Pass performs one bounded round of lookups.
func (r *Resolver) Pass(ctx context.Context) {
	candidates, err := r.Store.LocationCandidates(ctx, 0)
	if err != nil {
		if ctx.Err() == nil {
			r.logf("country lookup: list servers: %v", err)
		}
		return
	}
	done := 0
	for _, c := range candidates {
		if ctx.Err() != nil || done >= r.maxPerPass() {
			return
		}
		target, ip, ok := r.target(ctx, c)
		if !ok {
			continue
		}
		changed := target != c.LookupIP
		if !changed && r.now().Before(c.NextAttemptAt) {
			continue
		}
		if done > 0 {
			if err := r.sleep(ctx, r.spacing()); err != nil {
				return
			}
		}
		done++
		code, lookupErr := r.Lookup(ctx, ip)
		if ctx.Err() != nil {
			return
		}
		if lookupErr == nil {
			if err := r.Store.SaveServerCountry(ctx, c.ServerID, code, target, r.now().Add(r.refresh())); err != nil {
				r.logf("country lookup: save %s: %v", c.ServerID, err)
			} else if c.Attempts > 0 {
				r.logf("country lookup: %s resolved after %d failed attempts", c.ServerID, c.Attempts)
			}
			continue
		}
		attempts := 1
		if !changed {
			attempts = c.Attempts + 1
		}
		if attempts == 1 {
			r.logf("country lookup: %s failed, will retry with back-off: %v", c.ServerID, lookupErr)
		}
		if err := r.Store.RecordServerCountryFailure(ctx, c.ServerID, target, attempts, r.now().Add(backoff(attempts)), changed); err != nil {
			r.logf("country lookup: record failure for %s: %v", c.ServerID, err)
		}
		if errors.Is(lookupErr, ErrRateLimited) {
			return // every provider is cooling down; try again next pass
		}
	}
}

// target picks the address to look up: the configured address (resolving a
// hostname), then the node's last public peer address, then, for the hub
// itself, its own public address. Private addresses are never sent out.
func (r *Resolver) target(ctx context.Context, c monitoring.LocationCandidate) (string, netip.Addr, bool) {
	if ip, ok := r.publicAddress(ctx, c.Address); ok {
		return ip.String(), ip, true
	}
	if ip, err := netip.ParseAddr(c.PeerIP); err == nil && Public(ip) {
		return ip.Unmap().String(), ip.Unmap(), true
	}
	if c.Role == "standalone" || c.Role == "hub" {
		return selfTarget, netip.Addr{}, true
	}
	return "", netip.Addr{}, false
}

func (r *Resolver) publicAddress(ctx context.Context, address string) (netip.Addr, bool) {
	host := strings.TrimSpace(address)
	if host == "" {
		return netip.Addr{}, false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap(), Public(ip)
	}
	resolve := r.Resolve
	if resolve == nil {
		resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := resolve(lookupCtx, host)
	if err != nil {
		return netip.Addr{}, false
	}
	// Round-robin DNS returns addresses in varying order; sorting keeps the
	// chosen address stable so it does not look like an address change.
	slices.SortFunc(addrs, func(a, b netip.Addr) int { return a.Compare(b) })
	for _, ip := range addrs {
		if Public(ip) {
			return ip.Unmap(), true
		}
	}
	return netip.Addr{}, false
}

// backoff doubles from 15 minutes up to one day.
func backoff(attempts int) time.Duration {
	wait := 15 * time.Minute
	for i := 1; i < attempts && wait < 24*time.Hour; i++ {
		wait *= 2
	}
	if wait > 24*time.Hour {
		wait = 24 * time.Hour
	}
	return wait
}

func (r *Resolver) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

func (r *Resolver) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r *Resolver) interval() time.Duration {
	if r.Interval > 0 {
		return r.Interval
	}
	return 5 * time.Minute
}

func (r *Resolver) spacing() time.Duration {
	if r.Spacing > 0 {
		return r.Spacing
	}
	return 2 * time.Second
}

func (r *Resolver) maxPerPass() int {
	if r.MaxPerPass > 0 {
		return r.MaxPerPass
	}
	return 20
}

func (r *Resolver) refresh() time.Duration {
	if r.Refresh > 0 {
		return r.Refresh
	}
	return 30 * 24 * time.Hour
}
