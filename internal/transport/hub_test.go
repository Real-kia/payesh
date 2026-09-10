package transport

import (
	"context"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func TestHubAcknowledgesOnlyAuthenticatedDurableBatch(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id := contracts.ServerID("server-0123456789")
	if err := store.EnsureServer(ctx, contracts.Server{ID: id, Name: "Node", Role: "node", Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics"}, ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	ca, _ := NewCertificateAuthority(now)
	enrollment, _ := ca.IssueEnrollment(id, now)
	identity, err := ca.Enroll(&enrollment, enrollment.Token, now)
	if err != nil {
		t.Fatal(err)
	}
	hub, _ := NewHub(ca, store)
	hello := contracts.Hello{Version: "0.1", ProtocolMin: "v1", ProtocolMax: "v1", Architecture: "amd64", Platform: "linux"}
	connection, err := hub.Open(ctx, identity.CertificatePEM, hello, now)
	if err != nil {
		t.Fatal(err)
	}
	batch := contracts.SampleBatch{Samples: []contracts.NodeMetricSample{{ServerID: id, CollectorEpoch: "epoch-0123456789", Sequence: 0, ObservedAt: now, Values: map[string]float64{"cpu.utilization": 0.1}}}}
	ack, err := connection.ReceiveBatch(ctx, batch, now.Add(time.Second))
	if err != nil || !ack.Accepted || ack.ThroughSequence != 0 {
		t.Fatalf("ack=%+v err=%v", ack, err)
	}
	// A later out-of-order sample is durably retained but cannot advance the
	// cumulative acknowledgement across the missing sequence one.
	outOfOrder := batch
	outOfOrder.Samples = []contracts.NodeMetricSample{{ServerID: id, CollectorEpoch: "epoch-0123456789", Sequence: 2, ObservedAt: now, Values: map[string]float64{"cpu.utilization": 0.2}}}
	ack, err = connection.ReceiveBatch(ctx, outOfOrder, now.Add(1500*time.Millisecond))
	if err != nil || !ack.Accepted || ack.ThroughSequence != 0 {
		t.Fatalf("out-of-order ack=%+v err=%v", ack, err)
	}
	missing := outOfOrder
	missing.Samples = []contracts.NodeMetricSample{{ServerID: id, CollectorEpoch: "epoch-0123456789", Sequence: 1, ObservedAt: now, Values: map[string]float64{"cpu.utilization": 0.15}}}
	ack, err = connection.ReceiveBatch(ctx, missing, now.Add(2*time.Second))
	if err != nil || !ack.Accepted || ack.ThroughSequence != 2 {
		t.Fatalf("contiguous ack=%+v err=%v", ack, err)
	}
	// A fresh epoch must not start its cumulative acknowledgement at the first
	// out-of-order sample; sequence zero is the only valid stream anchor.
	firstOutOfOrder := contracts.SampleBatch{Samples: []contracts.NodeMetricSample{{ServerID: id, CollectorEpoch: "epoch-fresh-012345", Sequence: 9, ObservedAt: now, Values: map[string]float64{"cpu.utilization": 0.3}}}}
	ack, err = connection.ReceiveBatch(ctx, firstOutOfOrder, now.Add(3*time.Second))
	if err != nil || ack.Accepted || ack.Error == nil || ack.Error.Code != "sequence_gap" {
		t.Fatalf("first out-of-order sample was acknowledged: ack=%+v err=%v", ack, err)
	}
	gapOnly := contracts.SampleBatch{Gaps: []contracts.CoverageGap{{CollectorEpoch: "epoch-gap-only-01", FromSequence: 5, ToSequence: 8, Reason: "spool-eviction"}}}
	ack, err = connection.ReceiveBatch(ctx, gapOnly, now.Add(3500*time.Millisecond))
	if err != nil || ack.Accepted || ack.Error == nil || ack.Error.Code != "sequence_gap" {
		t.Fatalf("gap-only batch acknowledged an uncovered prefix: ack=%+v err=%v", ack, err)
	}
	job := contracts.ActionRequest{Protocol: contracts.HelperProtocol, RequestID: "request-0123456789", Action: "service.restart", TargetServerID: id, IdempotencyKey: "idem-0123456789", ExpectedRevision: 1, Deadline: now.Add(time.Minute)}
	if err := connection.QueueJob(job, now); err == nil {
		t.Fatal("queued a job for a conflicting configuration revision")
	}
	job.ExpectedRevision = 0
	if err := connection.QueueJob(job, now); err != nil {
		t.Fatal("queue valid job:", err)
	}
	if _, ok := connection.NextJob(now.Add(2 * time.Minute)); ok {
		t.Fatal("expired queued job was dispatched")
	}
	job.RequestID = "request-0123456700"
	job.IdempotencyKey = "idem-0123456700"
	job.Deadline = now.Add(4 * time.Minute)
	if err := connection.QueueJob(job, now); err != nil {
		t.Fatal("queue second valid job:", err)
	}
	if got, ok := connection.NextJob(now.Add(2 * time.Minute)); !ok || got.RequestID != job.RequestID {
		t.Fatalf("valid queued job was not dispatched: got=%+v ok=%v", got, ok)
	}
	if _, err := hub.Open(ctx, identity.CertificatePEM, hello, now); err != ErrAlreadyConnected {
		t.Fatalf("duplicate connection: %v", err)
	}
	connection.Close()
	if _, err := connection.ReceiveBatch(ctx, batch, now.Add(2*time.Second)); err != ErrConnectionClosed {
		t.Fatalf("closed connection accepted batch: %v", err)
	}
	if err := connection.Heartbeat(ctx, contracts.Heartbeat{ServerID: id, SentAt: now}, now.Add(2*time.Second)); err != ErrConnectionClosed {
		t.Fatalf("closed connection accepted heartbeat: %v", err)
	}
	if _, err := hub.Open(ctx, identity.CertificatePEM, hello, now); err != nil {
		t.Fatalf("connection not released: %v", err)
	}
}

func TestRevokingIdentityClosesLiveConnection(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id := contracts.ServerID("server-revoke-012345")
	if err := store.EnsureServer(ctx, contracts.Server{ID: id, Name: "Revocable", Role: "node", Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics"}, ConnectionState: "never-connected", FreshnessState: "unknown"}); err != nil {
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
	identity, err := ca.Enroll(&enrollment, enrollment.Token, now)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := NewHub(ca, store)
	if err != nil {
		t.Fatal(err)
	}
	hello := contracts.Hello{Version: "0.1", ProtocolMin: "v1", ProtocolMax: "v1", Architecture: "amd64", Platform: "linux"}
	connection, err := hub.Open(ctx, identity.CertificatePEM, hello, now)
	if err != nil {
		t.Fatal(err)
	}
	ca.Revoke(identity)
	if _, err := connection.ReceiveBatch(ctx, contracts.SampleBatch{Samples: []contracts.NodeMetricSample{{ServerID: id, CollectorEpoch: "epoch-revocation", Sequence: 0, ObservedAt: now, Values: map[string]float64{"cpu.utilization": 0.1}}}}, now.Add(time.Second)); err != ErrConnectionClosed {
		t.Fatalf("revoked connection accepted a batch: %v", err)
	}
	if err := connection.Heartbeat(ctx, contracts.Heartbeat{ServerID: id, SentAt: now}, now.Add(time.Second)); err != ErrConnectionClosed {
		t.Fatalf("revoked connection accepted heartbeat: %v", err)
	}

	// Revocation releases the server's ownership boundary, so a fresh
	// authenticated recovery certificate can take the one-live slot.
	recovery, err := ca.IssueEnrollment(id, now.Add(2*time.Second))
	if err != nil {
		t.Fatal("issue recovery enrollment:", err)
	}
	recoveryIdentity, err := ca.Enroll(&recovery, recovery.Token, now.Add(3*time.Second))
	if err != nil {
		t.Fatal("enroll recovery identity:", err)
	}
	if _, err := hub.Open(ctx, recoveryIdentity.CertificatePEM, hello, now.Add(4*time.Second)); err != nil {
		t.Fatal("recovery connection blocked by revoked channel:", err)
	}
}
