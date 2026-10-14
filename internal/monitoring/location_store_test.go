package monitoring

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestServerCountryBookkeeping(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	if err := store.UpsertServer(ctx, server); err != nil {
		t.Fatal(err)
	}

	got, _, err := store.GetServer(ctx, server.ID)
	if err != nil || got.CountryCode != "" {
		t.Fatalf("a server without a lookup must have no country: %q, %v", got.CountryCode, err)
	}

	if err := store.RecordServerPeerIP(ctx, server.ID, netip.MustParseAddr("::ffff:203.0.113.9")); err != nil {
		t.Fatal(err)
	}
	refresh := time.Now().UTC().Add(30 * 24 * time.Hour)
	if err := store.SaveServerCountry(ctx, server.ID, "DE", "203.0.113.9", refresh); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveServerCountry(ctx, server.ID, "de", "203.0.113.9", refresh); err == nil {
		t.Fatal("malformed country codes must be rejected")
	}
	page, err := store.QueryServerPage(ctx, 10, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].CountryCode != "DE" {
		t.Fatalf("server list did not include the country: %+v, %v", page.Items, err)
	}

	candidates, err := store.LocationCandidates(ctx, 0)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates = %+v, %v", candidates, err)
	}
	c := candidates[0]
	if c.PeerIP != "203.0.113.9" || c.LookupIP != "203.0.113.9" || c.Attempts != 0 || c.NextAttemptAt.Before(time.Now().Add(29*24*time.Hour)) {
		t.Fatalf("candidate bookkeeping = %+v", c)
	}

	// A failed retry keeps the known country; an address change clears it.
	if err := store.RecordServerCountryFailure(ctx, server.ID, "203.0.113.9", 1, time.Now().Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.GetServer(ctx, server.ID); got.CountryCode != "DE" {
		t.Fatalf("a failed refresh dropped the country: %q", got.CountryCode)
	}
	if err := store.RecordServerCountryFailure(ctx, server.ID, "198.51.100.1", 1, time.Now().Add(time.Hour), true); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.GetServer(ctx, server.ID); got.CountryCode != "" {
		t.Fatalf("an address change kept the old country: %q", got.CountryCode)
	}

	// Bookkeeping for unknown servers is ignored rather than orphaned.
	if err := store.SaveServerCountry(ctx, contracts.ServerID("server-missing-0001"), "FR", "203.0.113.1", refresh); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM server_locations`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("server_locations rows = %d, %v", rows, err)
	}
}
