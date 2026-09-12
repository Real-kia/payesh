package transport

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestWebSocketMaskedClientFramesRoundTrip(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	server := &webSocket{conn: serverConn, br: bufio.NewReader(serverConn), bw: bufio.NewWriter(serverConn), readMask: true}
	client := &webSocket{conn: clientConn, br: bufio.NewReader(clientConn), bw: bufio.NewWriter(clientConn), mask: true}
	want := contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "heartbeat", SentAt: time.Now().UTC(), Body: mustJSON(contracts.Heartbeat{ServerID: "server-frame-roundtrip01", SentAt: time.Now().UTC(), Health: "healthy"})}
	errCh := make(chan error, 1)
	go func() { errCh <- client.WriteEnvelope(want) }()
	payload, opcode, err := server.ReadMessage()
	if err != nil || opcode != wsOpcodeText {
		t.Fatalf("read frame opcode=%d err=%v", opcode, err)
	}
	var got contracts.Envelope
	if err := decodeEnvelope(payload, &got); err != nil || got.Message != want.Message {
		t.Fatalf("decoded envelope=%+v err=%v", got, err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestNodeIdentityPersistenceIsAtomicAndRestricted(t *testing.T) {
	now := time.Now().UTC()
	ca, err := NewCertificateAuthority(now)
	if err != nil {
		t.Fatal(err)
	}
	id := contracts.ServerID("server-identity-persist01")
	enrollment, err := ca.IssueEnrollment(id, now)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := ca.Enroll(&enrollment, enrollment.Token, now)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/identity.json"
	if err := SaveNodeIdentity(path, identity); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadNodeIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ServerID != identity.ServerID || string(loaded.CertificatePEM) != string(identity.CertificatePEM) || string(loaded.PrivateKeyPEM) != string(identity.PrivateKeyPEM) {
		t.Fatal("persisted node identity changed")
	}
}

func TestAgentActionExecutionIsIdempotent(t *testing.T) {
	var calls int
	client := &AgentClient{ActionHandler: func(_ context.Context, request contracts.ActionRequest) contracts.ActionResponse {
		calls++
		return contracts.ActionResponse{RequestID: request.RequestID, Accepted: true, Revision: 4}
	}}
	now := time.Now().UTC()
	request := contracts.ActionRequest{Protocol: contracts.HelperProtocol, RequestID: "action-agent-012345", Action: "service.restart", TargetServerID: "server-agent-012345", Target: "payesh-agent", IdempotencyKey: "idem-agent-012345", Deadline: now.Add(time.Minute)}
	first := client.executeAction(context.Background(), request)
	second := client.executeAction(context.Background(), request)
	if !first.Accepted || !second.Accepted || calls != 1 {
		t.Fatalf("action execution was not idempotent: first=%+v second=%+v calls=%d", first, second, calls)
	}
}
