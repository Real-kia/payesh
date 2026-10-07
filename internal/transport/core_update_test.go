package transport

import (
	"bufio"
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestCoreUpdateDoesNotBlockMeasurementsAndSuppressesConcurrentRedelivery(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	_ = left.SetDeadline(time.Now().Add(5 * time.Second))
	_ = right.SetDeadline(time.Now().Add(5 * time.Second))
	clientWS := &webSocket{conn: left, br: bufio.NewReader(left), bw: bufio.NewWriter(left), mask: true}
	serverWS := &webSocket{conn: right, br: bufio.NewReader(right), bw: bufio.NewWriter(right), readMask: true}
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	agent := &AgentClient{ActionHandler: func(ctx context.Context, req contracts.ActionRequest) contracts.ActionResponse {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
			return contracts.ActionResponse{RequestID: req.RequestID, Accepted: true, CoreUpdate: &contracts.CoreUpdateResult{JobID: "update-core-pipe-test01", Release: "1.2.0", State: "succeeded"}}
		case <-ctx.Done():
			return contracts.ActionResponse{RequestID: req.RequestID}
		}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := contracts.ActionRequest{Protocol: contracts.HelperProtocol, RequestID: "action-core-pipe-test01", Action: "core.update", TargetServerID: "node-core-pipe-test01", Target: "payesh-core", IdempotencyKey: "action-core-pipe-test01", Arguments: mustJSON(contracts.CoreUpdateIntent{JobID: "update-core-pipe-test01", Release: "1.2.0"}), Deadline: time.Now().Add(time.Minute)}
	incoming := inboundEnvelope{envelope: contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "action_request", Request: req.RequestID, Body: mustJSON(req)}}
	returned := make(chan error, 1)
	go func() { returned <- agent.handleIncoming(ctx, clientWS, incoming) }()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("core activation blocked measurement/control reader")
	}
	<-started
	if err := agent.handleIncoming(ctx, clientWS, incoming); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("redelivery started simultaneous core update")
	}
	sent := make(chan error, 1)
	go func() {
		sent <- clientWS.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "sample_batch", SentAt: time.Now().UTC(), Body: []byte(`{}`)})
	}()
	payload, _, err := serverWS.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var envelope contracts.Envelope
	if err := decodeEnvelope(payload, &envelope); err != nil || envelope.Message != "sample_batch" {
		t.Fatalf("envelope=%+v err=%v", envelope, err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	close(release)
	payload, _, err = serverWS.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeEnvelope(payload, &envelope); err != nil || envelope.Message != "action_response" {
		t.Fatalf("envelope=%+v err=%v", envelope, err)
	}
	var response contracts.ActionResponse
	if err := decodeBody(envelope.Body, &response); err != nil || !response.Accepted || response.CoreUpdate == nil {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}
