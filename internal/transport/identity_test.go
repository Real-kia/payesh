package transport

import (
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
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
