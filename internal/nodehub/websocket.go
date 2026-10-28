package nodehub

// The hub side of the live node transport. The HTTP server is expected to be
// wrapped in TLS with client certificates enabled; the handlers reject
// plaintext requests and re-verify the presented certificate through Hub.Open
// before accepting a node hello.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/transport"
)

func WebSocketHandler(hub *Hub) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hub == nil || r.URL.Path != "/node/v1" {
			http.NotFound(w, r)
			return
		}
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "client certificate required", http.StatusUnauthorized)
			return
		}
		if _, err := hub.CA.Verify(transport.PEMCertificate(r.TLS.PeerCertificates[0]), time.Now().UTC()); err != nil {
			http.Error(w, "invalid node identity", http.StatusUnauthorized)
			return
		}
		conn, rw, err := transport.UpgradeWebSocket(w, r)
		if err != nil {
			// transport.UpgradeWebSocket owns an HTTP error response until it returns a
			// connection; do not write a second response here.
			return
		}
		defer conn.Close()
		certificatePEM := transport.PEMCertificate(r.TLS.PeerCertificates[0])
		serveNodeWebSocket(r.Context(), rw, hub, certificatePEM)
	})
}

// BootstrapWebSocketHandler upgrades /node/bootstrap/v1 using server TLS
// authentication only. It is deliberately separate from WebSocketHandler:
// the node has no client certificate until this one-time exchange succeeds.
// The surrounding HTTP server must pin the enrollment CA (or another
// operator-selected hub trust anchor) on the node side.
func BootstrapWebSocketHandler(hub *Hub) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hub == nil || r.URL.Path != "/node/bootstrap/v1" {
			http.NotFound(w, r)
			return
		}
		if r.TLS == nil {
			http.Error(w, "TLS is required", http.StatusUnauthorized)
			return
		}
		conn, rw, err := transport.UpgradeWebSocket(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		serveBootstrapWebSocket(r.Context(), rw, hub)
	})
}

func serveBootstrapWebSocket(ctx context.Context, ws *transport.WebSocket, hub *Hub) {
	if err := ws.SetReadDeadline(time.Now().Add(transport.WebSocketReadDeadline)); err != nil {
		return
	}
	payload, opcode, err := ws.ReadMessage()
	if err != nil || opcode == transport.WSOpcodeClose || opcode == transport.WSOpcodePing {
		if opcode == transport.WSOpcodePing {
			_ = ws.WriteMessage(transport.WSOpcodePong, payload)
		}
		return
	}
	var envelope contracts.Envelope
	if err := transport.DecodeEnvelope(payload, &envelope); err != nil || envelope.Protocol != contracts.NodeProtocol || envelope.Message != "bootstrap" {
		_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: transport.ErrorBody("invalid_bootstrap", "a bootstrap request is required", false)})
		return
	}
	var request transport.BootstrapRequest
	if err := transport.DecodeBody(envelope.Body, &request); err != nil || !validBootstrapRequest(request) {
		_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: transport.ErrorBody("invalid_bootstrap", "bootstrap job and token are invalid", false)})
		return
	}
	identity, _, err := hub.ConsumeEnrollmentJob(ctx, request.JobID, request.Token, time.Now().UTC())
	if err != nil {
		_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: transport.ErrorBody("bootstrap_failed", fmt.Sprintf("bootstrap enrollment failed: %v", err), false)})
		return
	}
	_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "bootstrap_response", Request: envelope.Request, SentAt: time.Now().UTC(), Body: transport.MustJSON(identity)})
}

func validBootstrapRequest(request transport.BootstrapRequest) bool {
	return request.JobID != "" && len(request.JobID) <= 128 && request.Token != "" && len(request.Token) <= transport.MaxBootstrapToken
}

func serveNodeWebSocket(ctx context.Context, ws *transport.WebSocket, hub *Hub, certificatePEM []byte) {
	var connection *Connection
	var dispatchCancel context.CancelFunc
	var dispatchDone <-chan struct{}
	defer func() {
		if dispatchCancel != nil {
			dispatchCancel()
			<-dispatchDone
		}
		if connection != nil {
			connection.Close()
		}
	}()
	for {
		if err := ws.SetReadDeadline(time.Now().Add(transport.WebSocketReadDeadline)); err != nil {
			return
		}
		payload, opcode, err := ws.ReadMessage()
		if err != nil || opcode == transport.WSOpcodeClose {
			return
		}
		if opcode != transport.WSOpcodeText && opcode != transport.WSOpcodeBinary {
			if opcode == transport.WSOpcodePing {
				_ = ws.WriteMessage(transport.WSOpcodePong, payload)
			}
			continue
		}
		var envelope contracts.Envelope
		if err := transport.DecodeEnvelope(payload, &envelope); err != nil {
			_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", SentAt: time.Now().UTC(), Body: transport.ErrorBody("malformed_envelope", err.Error(), false)})
			return
		}
		if envelope.Protocol != contracts.NodeProtocol {
			_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: transport.ErrorBody("unsupported_protocol", "node protocol is required", false)})
			return
		}
		switch envelope.Message {
		case "hello":
			if connection != nil {
				_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: transport.ErrorBody("duplicate_hello", "hello was already accepted", false)})
				return
			}
			var hello contracts.Hello
			if err := transport.DecodeBody(envelope.Body, &hello); err != nil {
				_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: transport.ErrorBody("invalid_hello", err.Error(), false)})
				return
			}
			opened, err := hub.Open(ctx, certificatePEM, hello, time.Now().UTC())
			if err != nil {
				_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: transport.ErrorBody("authentication_failed", err.Error(), false)})
				return
			}
			connection = opened
			hub.recordPeer(ctx, connection, ws)
			if hub.Ports != nil {
				if err := hub.Ports.Observe(connection.ServerID, localPort(ws)); err != nil {
					return
				}
			}
			for _, capability := range hello.Capabilities {
				if capability == transport.MigrationCapability {
					if err := ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "hello_accepted", SentAt: time.Now().UTC()}); err != nil {
						return
					}
					break
				}
			}
			var dispatchCtx context.Context
			var cancel context.CancelFunc
			dispatchCtx, cancel = context.WithCancel(ctx)
			// Keep the cancel function behind the connection-scoped cleanup
			// closure; this also makes the lifetime explicit to static analysis.
			dispatchCancel = func() { cancel() }
			done := make(chan struct{})
			dispatchDone = done
			go func() {
				defer close(done)
				dispatchNodeJobs(dispatchCtx, ws, hub, connection)
			}()
		case "transport_result":
			if connection == nil {
				return
			}
			var result transport.TransportMigrationResult
			if err := transport.DecodeBody(envelope.Body, &result); err != nil {
				return
			}
			if hub.Ports != nil {
				hub.Ports.ReportFailure(connection.ServerID, result.Error)
			}
		case "heartbeat":
			if connection == nil {
				return
			}
			var heartbeat contracts.Heartbeat
			if err := transport.DecodeBody(envelope.Body, &heartbeat); err != nil || connection.Heartbeat(ctx, heartbeat, time.Now().UTC()) != nil {
				return
			}
		case "renewal_request":
			if connection == nil {
				return
			}
			var request transport.RenewalRequest
			if err := transport.DecodeBody(envelope.Body, &request); err != nil || request.ServerID != connection.ServerID {
				_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: transport.ErrorBody("invalid_renewal", "renewal identity does not match authenticated node", false)})
				return
			}
			renewed, err := connection.Renew(ctx, time.Now().UTC())
			if err != nil {
				_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: transport.ErrorBody("renewal_failed", "certificate renewal failed; authenticated recovery is required", false)})
				return
			}
			if err := ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "renewal_response", Request: envelope.Request, SentAt: time.Now().UTC(), Body: transport.MustJSON(renewed)}); err != nil {
				return
			}
			// CA renewal revokes the predecessor and the hub callback marks this
			// connection closed. End the old socket after the response is flushed;
			// the client reconnects with its replacement identity.
			return
		case "sample_batch":
			if connection == nil {
				return
			}
			var batch contracts.SampleBatch
			ack := contracts.Acknowledgement{RequestID: envelope.Request}
			if err := transport.DecodeBody(envelope.Body, &batch); err != nil {
				ack.Error = &contracts.Error{Code: "invalid_batch", Message: err.Error()}
			} else {
				accepted, receiveErr := connection.ReceiveBatch(ctx, batch, time.Now().UTC())
				ack = accepted
				ack.RequestID = envelope.Request
				if receiveErr != nil && ack.Error == nil {
					ack.Error = &contracts.Error{Code: "batch_rejected", Message: receiveErr.Error(), Retryable: true}
				}
			}
			ack.SamplingIntervalSeconds = connection.samplingPolicySeconds(ctx)
			connection.touchRelayUse(ctx, time.Now().UTC())
			ack.Relay = connection.relayPolicy(ctx)
			ack.Addresses = connection.supports(transport.AddressesCapability)
			body, _ := json.Marshal(ack)
			if ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "acknowledgement", Request: envelope.Request, SentAt: time.Now().UTC(), Body: body}) != nil {
				return
			}
		case "addresses":
			if connection == nil || !connection.supports(transport.AddressesCapability) {
				return
			}
			var reported contracts.NodeAddresses
			if err := transport.DecodeBody(envelope.Body, &reported); err != nil || len(reported.Addresses) > 4*monitoring.MaxNodeAddresses {
				return
			}
			if connection.allowAddressReport(time.Now()) {
				writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_ = hub.Store.RecordServerAddresses(writeCtx, connection.ServerID, reported.Addresses)
				cancel()
			}
		case "action_response":
			if connection == nil {
				return
			}
			var response contracts.ActionResponse
			if err := transport.DecodeBody(envelope.Body, &response); err != nil {
				_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: transport.ErrorBody("invalid_action_response", err.Error(), false)})
				continue
			}
			if response.RequestID == "" {
				response.RequestID = envelope.Request
			}
			lease, owned := connection.leaseFor(response.RequestID)
			if !owned {
				_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: transport.ErrorBody("action_lease_lost", "action response does not belong to this connection", false)})
				continue
			}
			if _, err := hub.Store.CompleteActionJob(ctx, lease.Job.ID, lease.Token, response, time.Now().UTC()); err != nil {
				// A duplicate result after a reconnect is safe: the durable job is
				// already terminal and the node can forget its response. Other
				// failures are reported but do not tear down the authenticated
				// channel, so unrelated samples/actions continue to flow.
				if !errors.Is(err, monitoring.ErrActionJobNotFound) && !errors.Is(err, monitoring.ErrJobLeaseLost) {
					_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: transport.ErrorBody("action_result_rejected", err.Error(), true)})
				}
			} else {
				connection.forgetLease(response.RequestID)
			}
		case "cancellation":
			// Cancellation is owner/API initiated and represented durably in the
			// jobs table. Nodes may send the typed message as a safety signal; it
			// is acknowledged at the transport layer without changing state.
			if connection == nil {
				return
			}
			var cancellation contracts.Cancellation
			if err := transport.DecodeBody(envelope.Body, &cancellation); err != nil {
				_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: transport.ErrorBody("invalid_cancellation", err.Error(), false)})
			}
		default:
			_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: transport.ErrorBody("unsupported_message", "message type is not supported", false)})
			return
		}
	}
}

// dispatchNodeJobs is intentionally a separate writer loop. The reader must
// remain blocked on the websocket so the hub can receive heartbeats/samples,
// while queued jobs need to be pushed proactively even when a node is idle.
// nodeJobPollInterval bounds how often each connection asks the store for
// queued action jobs. Jobs are operator-initiated, so a second of latency is
// invisible, while polling every tick made every node a constant database load.
const nodeJobPollInterval = time.Second

func dispatchNodeJobs(ctx context.Context, ws *transport.WebSocket, hub *Hub, connection *Connection) {
	lastMigration := time.Time{}
	lastLease := time.Time{}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if hub.Ports != nil && now.Sub(lastMigration) >= 30*time.Second {
				lastMigration = now
				if target := hub.Ports.migrationURL(connection); target != "" {
					if err := ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "transport_migrate", SentAt: now.UTC(), Body: transport.MustJSON(transport.TransportMigration{URL: target})}); err != nil {
						return
					}
				}
			}
			if job, ok := connection.NextJob(now); ok {
				if err := ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "action_request", Request: job.RequestID, SentAt: now.UTC(), Body: transport.MustJSON(job)}); err != nil {
					return
				}
				continue
			}
			if now.Sub(lastLease) < nodeJobPollInterval {
				continue
			}
			lastLease = now
			lease, ok, err := hub.LeaseNextJob(ctx, connection, now.UTC())
			if err != nil || !ok {
				continue
			}
			// A delivered job may have a successor; look again on the next tick.
			lastLease = time.Time{}
			connection.rememberLease(lease)
			if err := ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "action_request", Request: lease.Action.RequestID, SentAt: now.UTC(), Body: transport.MustJSON(lease.Action)}); err != nil {
				// The lease remains durable until expiry; a reconnect can reclaim
				// it and idempotency protects a node that completed before the
				// socket write failed.
				return
			}
		}
	}
}

func (c *Connection) rememberLease(lease monitoring.JobLease) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if !c.closed {
		c.leases[lease.Action.RequestID] = lease
	}
}

func (c *Connection) leaseFor(requestID string) (monitoring.JobLease, bool) {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	lease, ok := c.leases[requestID]
	return lease, ok
}

func (c *Connection) forgetLease(requestID string) {
	c.stateMu.Lock()
	delete(c.leases, requestID)
	c.stateMu.Unlock()
}

func (c *Connection) samplingPolicySeconds(ctx context.Context) int {
	c.stateMu.RLock()
	supported := false
	for _, capability := range c.hello.Capabilities {
		if capability == "sampling-policy" {
			supported = true
			break
		}
	}
	c.stateMu.RUnlock()
	if !supported {
		return 0
	}
	return c.hub.Store.SamplingSeconds(ctx)
}

// supports reports whether the agent advertised capability.
func (c *Connection) supports(capability string) bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	for _, advertised := range c.hello.Capabilities {
		if advertised == capability {
			return true
		}
	}
	return false
}

// allowAddressReport accepts at most one address report per minute per
// connection, so a misbehaving agent cannot turn it into a write load.
func (c *Connection) allowAddressReport(now time.Time) bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if !c.addressesAt.IsZero() && now.Sub(c.addressesAt) < time.Minute {
		return false
	}
	c.addressesAt = now
	return true
}

// relayPolicy is sent only to agents that understand it; older agents reject
// unknown acknowledgement fields. A lookup error returns nil, which agents
// treat as "keep the current relay", so a transient failure never cuts off
// the servers that depend on this relay.
func (c *Connection) relayPolicy(ctx context.Context) *contracts.RelayPolicy {
	c.stateMu.RLock()
	supported := false
	for _, capability := range c.hello.Capabilities {
		if capability == transport.RelayCapability {
			supported = true
			break
		}
	}
	c.stateMu.RUnlock()
	if !supported {
		return nil
	}
	policy, err := c.hub.Store.RelayPolicy(ctx, c.ServerID, time.Now().UTC())
	if err != nil {
		return nil
	}
	return policy
}
