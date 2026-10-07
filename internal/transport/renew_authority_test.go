package transport

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

// A hub that no longer owns a server must not renew that server's certificate,
// and a refused renewal must not revoke the node's current identity.
func TestConnectionRenewRefusedAfterAuthorityLeavesTheHub(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ca, err := NewCertificateAuthority(now)
	if err != nil {
		t.Fatal(err)
	}
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverID := contracts.ServerID("server-renewal-012345")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "node", Role: "node", Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics"}, ConnectionState: "connected", FreshnessState: "fresh"}); err != nil {
		t.Fatal(err)
	}
	hub, err := NewHub(ca, store)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := ca.IssueEnrollment(serverID, now)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := ca.Enroll(&enrollment, enrollment.Token, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	connection := &Connection{hub: hub, ServerID: serverID, certificatePEM: identity.CertificatePEM, certificateFingerprint: identity.Fingerprint, seenJobKeys: map[string]struct{}{}, leases: map[string]monitoring.JobLease{}}
	if _, err := store.InitializeServerAuthority(ctx, serverID, "owner-hub-aaaa"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FreezeServerAuthority(ctx, monitoring.AuthorityTransition{ServerID: serverID, CutoverID: "cutover-0001", RequestDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Renew(ctx, now.Add(24*time.Hour)); !errors.Is(err, monitoring.ErrServerNotAuthoritative) {
		t.Fatalf("a hub that no longer owns the server renewed its certificate: %v", err)
	}
	if _, err := ca.Renew(identity, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("a refused renewal revoked the current identity: %v", err)
	}
}
