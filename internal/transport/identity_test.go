package transport

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func TestEnrollmentIsSingleUseAndRevocable(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	ca, err := NewCertificateAuthority(now)
	if err != nil {
		t.Fatal(err)
	}
	e, err := ca.IssueEnrollment(contracts.ServerID("server-0123456789"), now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ca.Enroll(&e, e.Token, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.Enroll(&e, e.Token, now.Add(2*time.Second)); err == nil {
		t.Fatal("pairing token reused")
	}
	// The caller-owned value is not the source of truth for single-use state.
	// A copy made before the first enrollment must not be able to replay it.
	copyBeforeConsume, err := ca.IssueEnrollment(contracts.ServerID("server-copy-012345"), now)
	if err != nil {
		t.Fatal(err)
	}
	copyOfEnrollment := copyBeforeConsume
	if _, err := ca.Enroll(&copyBeforeConsume, copyBeforeConsume.Token, now.Add(time.Second)); err != nil {
		t.Fatal("first enrollment failed:", err)
	}
	if _, err := ca.Enroll(&copyOfEnrollment, copyOfEnrollment.Token, now.Add(2*time.Second)); err == nil {
		t.Fatal("copied enrollment replayed pairing token")
	}
	if got, err := ca.Verify(id.CertificatePEM, now.Add(time.Hour)); err != nil || got != id.ServerID {
		t.Fatalf("verify: %v %q", err, got)
	}
	ca.Revoke(id)
	if _, err := ca.Verify(id.CertificatePEM, now.Add(time.Hour)); err == nil {
		t.Fatal("revoked certificate accepted")
	}
}

func TestCertificateAuthoritySurvivesStoreReopen(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "payesh.db")
	store, err := monitoring.OpenStore(ctx, path, monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ca, err := NewPersistentCertificateAuthority(now, store)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := ca.IssueEnrollment(contracts.ServerID("server-pending-0123"), now)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := ca.IssueEnrollment(contracts.ServerID("server-issued-01234"), now)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := ca.Enroll(&issued, issued.Token, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	revokedEnrollment, err := ca.IssueEnrollment(contracts.ServerID("server-revoked-0123"), now)
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := ca.Enroll(&revokedEnrollment, revokedEnrollment.Token, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := ca.Revoke(revoked); err != nil {
		t.Fatal(err)
	}
	caPEM := string(ca.PEM())
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = monitoring.OpenStore(ctx, path, monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	restored, err := NewPersistentCertificateAuthority(now.Add(2*time.Second), store)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored.PEM()) != caPEM {
		t.Fatal("enrollment CA changed after restart")
	}
	if got, err := restored.Verify(identity.CertificatePEM, now.Add(time.Hour)); err != nil || got != identity.ServerID {
		t.Fatalf("restored issued identity: %q %v", got, err)
	}
	if _, err := restored.Verify(revoked.CertificatePEM, now.Add(time.Hour)); err == nil {
		t.Fatal("revocation was lost after restart")
	}
	if _, err := restored.IssueEnrollment(pending.ServerID, now.Add(2*time.Second)); err == nil {
		t.Fatal("pending enrollment ownership was lost after restart")
	}
	if _, err := restored.Enroll(&pending, pending.Token, now.Add(3*time.Second)); err != nil {
		t.Fatal("pending enrollment could not complete after restart:", err)
	}
}

func TestEnrollmentExpiry(t *testing.T) {
	now := time.Now().UTC()
	ca, _ := NewCertificateAuthority(now)
	e, _ := ca.IssueEnrollment(contracts.ServerID("server-0123456789"), now)
	if _, err := ca.Enroll(&e, e.Token, e.ExpiresAt); err == nil {
		t.Fatal("expired token accepted")
	}
}

func TestEnrollmentRejectsCompetingServerIdentity(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	ca, err := NewCertificateAuthority(now)
	if err != nil {
		t.Fatal(err)
	}
	serverID := contracts.ServerID("server-competing-01")
	e, err := ca.IssueEnrollment(serverID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.IssueEnrollment(serverID, now); err == nil {
		t.Fatal("issued two simultaneous enrollment tokens for one server")
	}
	identity, err := ca.Enroll(&e, e.Token, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.IssueEnrollment(serverID, now.Add(2*time.Second)); err == nil {
		t.Fatal("issued a second certificate enrollment for one server")
	}

	// Revocation is the explicit recovery boundary that permits a fresh
	// certificate without allowing the old certificate to remain trusted.
	ca.Revoke(identity)
	recovery, err := ca.IssueEnrollment(serverID, now.Add(3*time.Second))
	if err != nil {
		t.Fatal("revoked server could not re-enroll:", err)
	}
	if _, err := ca.Enroll(&recovery, recovery.Token, now.Add(4*time.Second)); err != nil {
		t.Fatal("recovery enrollment failed:", err)
	}
}

func TestCertificateRenewalReplacesAndRevokesPreviousIdentity(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ca, err := NewCertificateAuthority(now)
	if err != nil {
		t.Fatal(err)
	}
	serverID := contracts.ServerID("server-renewal-012345")
	enrollment, err := ca.IssueEnrollment(serverID, now)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := ca.Enroll(&enrollment, enrollment.Token, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := ca.Renew(identity, now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Fingerprint == identity.Fingerprint || renewed.ServerID != serverID {
		t.Fatalf("renewal did not replace identity: old=%q new=%q", identity.Fingerprint, renewed.Fingerprint)
	}
	if _, err := ca.Verify(identity.CertificatePEM, now.Add(24*time.Hour)); err == nil || err.Error() != "certificate_revoked" {
		t.Fatalf("old certificate verification error=%v, want certificate_revoked", err)
	}
	if got, err := ca.Verify(renewed.CertificatePEM, now.Add(24*time.Hour)); err != nil || got != serverID {
		t.Fatalf("renewed certificate verification: id=%q err=%v", got, err)
	}
	if _, err := ca.Renew(identity, now.Add(48*time.Hour)); err == nil {
		t.Fatal("revoked certificate was renewed")
	}
}

func TestConsumeEnrollmentTokenIsSingleUseWithoutCallerEnrollmentState(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ca, err := NewCertificateAuthority(now)
	if err != nil {
		t.Fatal(err)
	}
	serverID := contracts.ServerID("server-consume-012345")
	enrollment, err := ca.IssueEnrollment(serverID, now)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := ca.ConsumeEnrollmentToken(enrollment.Token, now.Add(time.Second))
	if err != nil || identity.ServerID != serverID {
		t.Fatalf("consume identity=%+v err=%v", identity, err)
	}
	if _, err := ca.ConsumeEnrollmentToken(enrollment.Token, now.Add(2*time.Second)); err == nil {
		t.Fatal("pairing token was consumed twice")
	}
}
