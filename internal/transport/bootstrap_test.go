package transport

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func TestBootstrapWebSocketConsumesDurableJobAndReturnsValidatedIdentity(t *testing.T) {
	ctx := context.Background()
	// The WebSocket handler intentionally uses the process clock at the
	// protocol boundary. Keep the fixture anchored to the current UTC minute so
	// the short-lived enrollment cannot expire when the suite is run later.
	now := time.Now().UTC().Truncate(time.Second)
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id := contracts.ServerID("server-bootstrap-ws01")
	if err := store.EnsureServer(ctx, contracts.Server{ID: id, Name: "node", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	ca, err := NewCertificateAuthority(now)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := ca.IssueEnrollment(id, now)
	if err != nil {
		t.Fatal(err)
	}
	job, created, err := store.CreateJob(ctx, contracts.Job{ID: "enrollment-bootstrap-ws01", Kind: "enrollment", State: contracts.JobQueued, IdempotencyKey: "bootstrap-ws-key", TargetServerID: id, ExpiresAt: now.Add(time.Minute)}, "redacted-request-hash", now)
	if err != nil || !created {
		t.Fatalf("create job=%+v created=%v err=%v", job, created, err)
	}
	hub, err := NewHub(ca, store)
	if err != nil {
		t.Fatal(err)
	}
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	serverWS := &webSocket{conn: serverConn, br: bufio.NewReader(serverConn), bw: bufio.NewWriter(serverConn), readMask: true}
	clientWS := &webSocket{conn: clientConn, br: bufio.NewReader(clientConn), bw: bufio.NewWriter(clientConn), mask: true}
	serverDone := make(chan struct{})
	go func() {
		serveBootstrapWebSocket(ctx, serverWS, hub)
		close(serverDone)
	}()
	requestID := "bootstrap-request-01"
	if err := clientWS.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "bootstrap", Request: requestID, SentAt: now, Body: mustJSON(BootstrapRequest{JobID: job.ID, Token: enrollment.Token})}); err != nil {
		t.Fatal("write bootstrap:", err)
	}
	payload, opcode, err := clientWS.ReadMessage()
	if err != nil || opcode != wsOpcodeText {
		t.Fatalf("read bootstrap response opcode=%d err=%v", opcode, err)
	}
	var envelope contracts.Envelope
	if err := decodeEnvelope(payload, &envelope); err != nil || envelope.Request != requestID || envelope.Message != "bootstrap_response" {
		t.Fatalf("unexpected bootstrap envelope=%+v err=%v", envelope, err)
	}
	var identity NodeIdentity
	if err := decodeBody(envelope.Body, &identity); err != nil {
		t.Fatal("decode identity:", err)
	}
	if identity.ServerID != id || identity.Fingerprint == "" || len(identity.PrivateKeyPEM) == 0 {
		t.Fatalf("unexpected identity: %+v", identity)
	}
	<-serverDone
	completed, found, err := store.GetJob(ctx, job.ID)
	if err != nil || !found || completed.State != contracts.JobSucceeded {
		t.Fatalf("bootstrap job=%+v found=%v err=%v", completed, found, err)
	}
	if _, err := ca.ConsumeEnrollmentToken(enrollment.Token, now.Add(time.Second)); err == nil {
		t.Fatal("bootstrap token replay succeeded")
	}
}

func TestRenewalSchedulerPersistsBeforeReplacingIdentityAndRequiresRecovery(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ca, err := NewCertificateAuthority(now)
	if err != nil {
		t.Fatal(err)
	}
	id := contracts.ServerID("server-renewal-scheduler01")
	enrollment, err := ca.IssueEnrollment(id, now)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := ca.Enroll(&enrollment, enrollment.Token, now)
	if err != nil {
		t.Fatal(err)
	}
	var persisted NodeIdentity
	scheduler := &RenewalScheduler{Identity: identity, RenewBefore: 30 * 24 * time.Hour, Now: func() time.Time { return now.Add(70 * 24 * time.Hour) }, Renew: func(_ context.Context, old NodeIdentity) (NodeIdentity, error) {
		return ca.Renew(old, now.Add(70*24*time.Hour))
	}, Persist: func(next NodeIdentity) error { persisted = next; return nil }}
	if err := scheduler.Check(context.Background()); err != nil {
		t.Fatal("renewal check:", err)
	}
	if persisted.Fingerprint == "" || scheduler.Identity.Fingerprint != persisted.Fingerprint || persisted.Fingerprint == identity.Fingerprint {
		t.Fatalf("replacement was not durably adopted: persisted=%q current=%q old=%q", persisted.Fingerprint, scheduler.Identity.Fingerprint, identity.Fingerprint)
	}
	old := scheduler.Identity
	scheduler.Recover = func(_ context.Context, _ NodeIdentity, _ error) (NodeIdentity, error) {
		return NodeIdentity{}, context.DeadlineExceeded
	}
	// An identity at expiry is not silently accepted. A failed recovery is
	// surfaced as the explicit recovery-required error.
	scheduler.Now = func() time.Time { return old.NotAfter }
	scheduler.Renew = func(_ context.Context, _ NodeIdentity) (NodeIdentity, error) {
		return NodeIdentity{}, errors.New("certificate_expired")
	}
	if err := scheduler.Check(context.Background()); err == nil || !strings.Contains(err.Error(), ErrIdentityRecoveryRequired.Error()) {
		t.Fatalf("missing recovery-required error: %v", err)
	}
}
