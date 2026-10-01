package transport

// This file contains the live node transport.  The HTTP server is expected to
// be wrapped in TLS with client certificates enabled; the handler deliberately
// rejects plaintext requests and still re-verifies the presented certificate
// through Hub.Open before accepting a node hello.

import (
	"bufio"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

const (
	webSocketVersion      = "13"
	webSocketGUID         = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	maxWebSocketHeader    = 16 << 10
	webSocketReadDeadline = 2 * time.Minute
)

var (
	ErrWebSocketHandshake       = errors.New("transport: websocket handshake failed")
	ErrWebSocketClosed          = errors.New("transport: websocket closed")
	ErrAgentNotConfigured       = errors.New("transport: agent is not configured")
	ErrIdentityRecoveryRequired = errors.New("transport: node identity recovery required")
	errIdentityRenewed          = errors.New("transport: node identity renewed")
)

// BootstrapRequest is sent over the server-authenticated bootstrap channel.
// The pairing token is intentionally accepted only in memory and is never
// included in a durable job or an identity file.
type BootstrapRequest struct {
	JobID string `json:"job_id"`
	Token string `json:"token"`
}

// RenewalRequest identifies the currently authenticated node. The server
// authenticates the request from the TLS certificate and Hub connection; the
// field is only an explicit consistency check against a confused client.
type RenewalRequest struct {
	ServerID contracts.ServerID `json:"server_id"`
}

// WebSocketHandler upgrades /node/v1 connections and dispatches the typed
// node envelopes into Hub.  The surrounding HTTP server must use TLS with
// ClientAuth set to RequireAnyClientCert (Hub performs the authoritative CA
// check, including revocation and single-certificate ownership).
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
		conn, rw, err := upgradeWebSocket(w, r)
		if err != nil {
			// upgradeWebSocket owns an HTTP error response until it returns a
			// connection; do not write a second response here.
			return
		}
		defer conn.Close()
		certificatePEM := pemCertificate(r.TLS.PeerCertificates[0])
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
		conn, rw, err := upgradeWebSocket(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		serveBootstrapWebSocket(r.Context(), rw, hub)
	})
}

func serveBootstrapWebSocket(ctx context.Context, ws *webSocket, hub *Hub) {
	if err := ws.SetReadDeadline(time.Now().Add(webSocketReadDeadline)); err != nil {
		return
	}
	payload, opcode, err := ws.ReadMessage()
	if err != nil || opcode == wsOpcodeClose || opcode == wsOpcodePing {
		if opcode == wsOpcodePing {
			_ = ws.WriteMessage(wsOpcodePong, payload)
		}
		return
	}
	var envelope contracts.Envelope
	if err := decodeEnvelope(payload, &envelope); err != nil || envelope.Protocol != contracts.NodeProtocol || envelope.Message != "bootstrap" {
		_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: errorBody("invalid_bootstrap", "a bootstrap request is required", false)})
		return
	}
	var request BootstrapRequest
	if err := decodeBody(envelope.Body, &request); err != nil || !validBootstrapRequest(request) {
		_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: errorBody("invalid_bootstrap", "bootstrap job and token are invalid", false)})
		return
	}
	identity, _, err := hub.ConsumeEnrollmentJob(ctx, request.JobID, request.Token, time.Now().UTC())
	if err != nil {
		_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: errorBody("bootstrap_failed", "bootstrap enrollment failed", false)})
		return
	}
	_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "bootstrap_response", Request: envelope.Request, SentAt: time.Now().UTC(), Body: mustJSON(identity)})
}

func validBootstrapRequest(request BootstrapRequest) bool {
	return request.JobID != "" && len(request.JobID) <= 128 && request.Token != "" && len(request.Token) <= maxBootstrapToken
}

const maxBootstrapToken = 256

func serveNodeWebSocket(ctx context.Context, ws *webSocket, hub *Hub, certificatePEM []byte) {
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
		if err := ws.SetReadDeadline(time.Now().Add(webSocketReadDeadline)); err != nil {
			return
		}
		payload, opcode, err := ws.ReadMessage()
		if err != nil || opcode == wsOpcodeClose {
			return
		}
		if opcode != wsOpcodeText && opcode != wsOpcodeBinary {
			if opcode == wsOpcodePing {
				_ = ws.WriteMessage(wsOpcodePong, payload)
			}
			continue
		}
		var envelope contracts.Envelope
		if err := decodeEnvelope(payload, &envelope); err != nil {
			_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", SentAt: time.Now().UTC(), Body: errorBody("malformed_envelope", err.Error(), false)})
			return
		}
		if envelope.Protocol != contracts.NodeProtocol {
			_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: errorBody("unsupported_protocol", "node protocol is required", false)})
			return
		}
		switch envelope.Message {
		case "hello":
			if connection != nil {
				_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: errorBody("duplicate_hello", "hello was already accepted", false)})
				return
			}
			var hello contracts.Hello
			if err := decodeBody(envelope.Body, &hello); err != nil {
				_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: errorBody("invalid_hello", err.Error(), false)})
				return
			}
			opened, err := hub.Open(ctx, certificatePEM, hello, time.Now().UTC())
			if err != nil {
				_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: errorBody("authentication_failed", err.Error(), false)})
				return
			}
			connection = opened
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
		case "heartbeat":
			if connection == nil {
				return
			}
			var heartbeat contracts.Heartbeat
			if err := decodeBody(envelope.Body, &heartbeat); err != nil || connection.Heartbeat(ctx, heartbeat, time.Now().UTC()) != nil {
				return
			}
		case "renewal_request":
			if connection == nil {
				return
			}
			var request RenewalRequest
			if err := decodeBody(envelope.Body, &request); err != nil || request.ServerID != connection.ServerID {
				_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: errorBody("invalid_renewal", "renewal identity does not match authenticated node", false)})
				return
			}
			renewed, err := connection.Renew(ctx, time.Now().UTC())
			if err != nil {
				_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: errorBody("renewal_failed", "certificate renewal failed; authenticated recovery is required", false)})
				return
			}
			if err := ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "renewal_response", Request: envelope.Request, SentAt: time.Now().UTC(), Body: mustJSON(renewed)}); err != nil {
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
			if err := decodeBody(envelope.Body, &batch); err != nil {
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
			body, _ := json.Marshal(ack)
			if ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "acknowledgement", Request: envelope.Request, SentAt: time.Now().UTC(), Body: body}) != nil {
				return
			}
		case "action_response":
			if connection == nil {
				return
			}
			var response contracts.ActionResponse
			if err := decodeBody(envelope.Body, &response); err != nil {
				_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: errorBody("invalid_action_response", err.Error(), false)})
				continue
			}
			if response.RequestID == "" {
				response.RequestID = envelope.Request
			}
			lease, owned := connection.leaseFor(response.RequestID)
			if !owned {
				_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: errorBody("action_lease_lost", "action response does not belong to this connection", false)})
				continue
			}
			if _, err := hub.Store.CompleteActionJob(ctx, lease.Job.ID, lease.Token, response, time.Now().UTC()); err != nil {
				// A duplicate result after a reconnect is safe: the durable job is
				// already terminal and the node can forget its response. Other
				// failures are reported but do not tear down the authenticated
				// channel, so unrelated samples/actions continue to flow.
				if !errors.Is(err, monitoring.ErrActionJobNotFound) && !errors.Is(err, monitoring.ErrJobLeaseLost) {
					_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: errorBody("action_result_rejected", err.Error(), true)})
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
			if err := decodeBody(envelope.Body, &cancellation); err != nil {
				_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: errorBody("invalid_cancellation", err.Error(), false)})
			}
		default:
			_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "error", Request: envelope.Request, SentAt: time.Now().UTC(), Body: errorBody("unsupported_message", "message type is not supported", false)})
			return
		}
	}
}

// dispatchNodeJobs is intentionally a separate writer loop. The reader must
// remain blocked on the websocket so the hub can receive heartbeats/samples,
// while queued jobs need to be pushed proactively even when a node is idle.
func dispatchNodeJobs(ctx context.Context, ws *webSocket, hub *Hub, connection *Connection) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if job, ok := connection.NextJob(now); ok {
				if err := ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "action_request", Request: job.RequestID, SentAt: now.UTC(), Body: mustJSON(job)}); err != nil {
					return
				}
				continue
			}
			lease, ok, err := hub.LeaseNextJob(ctx, connection, now.UTC())
			if err != nil || !ok {
				continue
			}
			connection.rememberLease(lease)
			if err := ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "action_request", Request: lease.Action.RequestID, SentAt: now.UTC(), Body: mustJSON(lease.Action)}); err != nil {
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

func decodeEnvelope(payload []byte, out *contracts.Envelope) error {
	if len(payload) == 0 || len(payload) > contracts.MaxEnvelopeBytes {
		return ErrFrameTooLarge
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return err
	}
	return out.Validate()
}

func decodeBody(body json.RawMessage, out any) error {
	if len(body) == 0 || len(body) > contracts.MaxEnvelopeBytes {
		return ErrFrameMalformed
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("trailing body data")
	}
	return nil
}

func errorBody(code, message string, retryable bool) json.RawMessage {
	body, _ := json.Marshal(contracts.Error{Code: code, Message: message, Retryable: retryable})
	return body
}

func pemCertificate(cert *x509.Certificate) []byte {
	if cert == nil {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

// AgentIdentityFile stores only the node's private identity material. Writes
// are atomic and restricted to the owner, so a torn write cannot erase a
// usable certificate during a restart.
func SaveNodeIdentity(path string, identity NodeIdentity) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || identity.ServerID == "" || len(identity.CertificatePEM) == 0 || len(identity.PrivateKeyPEM) == 0 {
		return ErrAgentNotConfigured
	}
	body, err := json.Marshal(identity)
	if err != nil {
		return fmt.Errorf("encode node identity: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create identity directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".identity-*")
	if err != nil {
		return fmt.Errorf("create identity temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace node identity: %w", err)
	}
	return os.Chmod(path, 0o600)
}

func LoadNodeIdentity(path string) (NodeIdentity, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return NodeIdentity{}, ErrAgentNotConfigured
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return NodeIdentity{}, err
	}
	var identity NodeIdentity
	if err := json.Unmarshal(body, &identity); err != nil || identity.ServerID == "" || len(identity.CertificatePEM) == 0 || len(identity.PrivateKeyPEM) == 0 {
		return NodeIdentity{}, errors.New("invalid persisted node identity")
	}
	return identity, nil
}

// AgentClient maintains one authenticated connection and treats the local
// spool as the source of truth. A batch is appended before transmission, and
// removed only after the hub's durable cumulative acknowledgement.
type AgentClient struct {
	URL      string
	Identity NodeIdentity
	// IdentityPath is optional. When configured, a successful in-band
	// renewal is persisted atomically before the client reconnects with the
	// replacement certificate.
	IdentityPath string
	TrustPEM     []byte
	Spool        *Spool
	Hello        contracts.Hello
	TLSConfig    *tls.Config
	Backoff      ReconnectBackoff
	RenewBefore  time.Duration
	// ActionHandler executes authenticated typed jobs locally. The transport
	// does not interpret action arguments as shell commands.
	ActionHandler  func(context.Context, contracts.ActionRequest) contracts.ActionResponse
	SamplingPolicy func(int)
	actionMu       sync.Mutex
	actionResult   map[string]contracts.ActionResponse
}

// Only advertise an extension to peers that explicitly support it. Older
// agents reject unknown acknowledgement fields rather than ignoring them.
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

type inboundEnvelope struct {
	envelope contracts.Envelope
	opcode   byte
}

// Run consumes batches until ctx is cancelled or batches is closed. It
// reconnects with bounded jitter and replays pending spool records oldest-first.
func (a *AgentClient) Run(ctx context.Context, batches <-chan contracts.SampleBatch) error {
	if a == nil || a.URL == "" || a.Identity.ServerID == "" || a.Spool == nil {
		return ErrAgentNotConfigured
	}
	backoff := a.Backoff
	if backoff.Base <= 0 {
		backoff.Base = time.Second
	}
	if backoff.Max <= 0 {
		backoff.Max = 60 * time.Second
	}
	for {
		ws, err := a.dial(ctx)
		if err == nil {
			backoff.Reset()
			err = a.session(ctx, ws, batches)
			_ = ws.Close()
			if err == nil {
				return nil
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		if batches == nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff.Next()):
		}
	}
}

func (a *AgentClient) session(ctx context.Context, ws *webSocket, batches <-chan contracts.SampleBatch) error {
	incoming := make(chan inboundEnvelope, 16)
	readErr := make(chan error, 1)
	go a.readIncoming(ctx, ws, incoming, readErr)
	if err := ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "hello", SentAt: time.Now().UTC(), Body: mustJSON(a.Hello)}); err != nil {
		return err
	}
	if err := a.replay(ctx, ws, incoming, readErr); err != nil {
		return err
	}
	renewBefore := a.RenewBefore
	if renewBefore <= 0 {
		renewBefore = 30 * 24 * time.Hour
	}
	renewalTimer := time.NewTimer(identityRenewalDelay(a.Identity, time.Now().UTC(), renewBefore))
	defer renewalTimer.Stop()
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-renewalTimer.C:
			renewed, err := a.renewOverWebSocket(ctx, ws, incoming, readErr)
			if err != nil {
				return err
			}
			if a.IdentityPath != "" {
				if err := SaveNodeIdentity(a.IdentityPath, renewed); err != nil {
					// The CA has already revoked the predecessor by this point.
					// Do not continue with a certificate that exists only in
					// memory; require explicit authenticated recovery instead.
					return fmt.Errorf("%w: persist renewed node identity: %v", ErrIdentityRecoveryRequired, err)
				}
			}
			a.Identity = renewed
			// Hub revocation intentionally closes the old authenticated
			// connection. Reconnect immediately with the replacement cert.
			return errIdentityRenewed
		case batch, ok := <-batches:
			if !ok {
				return nil
			}
			if _, err := a.Spool.Append(batch); err != nil {
				return err
			}
			if err := a.replay(ctx, ws, incoming, readErr); err != nil {
				return err
			}
		case message := <-incoming:
			if err := a.handleIncoming(ctx, ws, message); err != nil {
				return err
			}
		case err := <-readErr:
			return err
		case <-heartbeat.C:
			if err := ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "heartbeat", SentAt: time.Now().UTC(), Body: mustJSON(contracts.Heartbeat{ServerID: a.Identity.ServerID, SentAt: time.Now().UTC(), Health: "healthy"})}); err != nil {
				return err
			}
		}
	}
}

func (a *AgentClient) readIncoming(ctx context.Context, ws *webSocket, incoming chan<- inboundEnvelope, readErr chan<- error) {
	for {
		if err := ws.SetReadDeadline(time.Now().Add(webSocketReadDeadline)); err != nil {
			readErr <- err
			return
		}
		payload, opcode, err := ws.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				readErr <- ctx.Err()
			} else {
				readErr <- err
			}
			return
		}
		if opcode == wsOpcodePing {
			if err := ws.WriteMessage(wsOpcodePong, payload); err != nil {
				readErr <- err
				return
			}
			continue
		}
		if opcode == wsOpcodeClose {
			readErr <- ErrWebSocketClosed
			return
		}
		if opcode != wsOpcodeText && opcode != wsOpcodeBinary {
			continue
		}
		var envelope contracts.Envelope
		if err := decodeEnvelope(payload, &envelope); err != nil {
			readErr <- err
			return
		}
		select {
		case incoming <- inboundEnvelope{envelope: envelope, opcode: opcode}:
		case <-ctx.Done():
			readErr <- ctx.Err()
			return
		}
	}
}

func (a *AgentClient) handleIncoming(ctx context.Context, ws *webSocket, message inboundEnvelope) error {
	envelope := message.envelope
	if envelope.Message != "action_request" {
		return nil
	}
	var request contracts.ActionRequest
	if err := decodeBody(envelope.Body, &request); err != nil {
		return err
	}
	if envelope.Request != "" && request.RequestID != envelope.Request {
		return errors.New("action request id mismatch")
	}
	response := a.executeAction(ctx, request)
	return ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "action_response", Request: request.RequestID, SentAt: time.Now().UTC(), Body: mustJSON(response)})
}

func (a *AgentClient) executeAction(ctx context.Context, request contracts.ActionRequest) contracts.ActionResponse {
	response := contracts.ActionResponse{RequestID: request.RequestID}
	if err := request.Validate(time.Now().UTC()); err != nil {
		response.Error = &contracts.Error{Code: "invalid_action", Message: err.Error(), Retryable: false}
		return response
	}
	a.actionMu.Lock()
	if a.actionResult == nil {
		a.actionResult = make(map[string]contracts.ActionResponse)
	}
	if cached, ok := a.actionResult[request.IdempotencyKey]; ok {
		a.actionMu.Unlock()
		return cached
	}
	a.actionMu.Unlock()
	if a.ActionHandler == nil {
		response.Error = &contracts.Error{Code: "action_unsupported", Message: "node action handler is not configured", Retryable: false}
	} else {
		actionCtx := ctx
		cancel := func() {}
		if !request.Deadline.IsZero() {
			actionCtx, cancel = context.WithDeadline(ctx, request.Deadline)
		}
		response = a.ActionHandler(actionCtx, request)
		cancel()
		// The transport correlation belongs to the delivered request; a local
		// executor cannot redirect a result to another job.
		response.RequestID = request.RequestID
	}
	a.actionMu.Lock()
	if len(a.actionResult) >= 1024 {
		for key := range a.actionResult {
			delete(a.actionResult, key)
			break
		}
	}
	a.actionResult[request.IdempotencyKey] = response
	a.actionMu.Unlock()
	return response
}

func identityRenewalDelay(identity NodeIdentity, now time.Time, renewBefore time.Duration) time.Duration {
	expiresAt := identity.NotAfter
	if expiresAt.IsZero() {
		if block, _ := pem.Decode(identity.CertificatePEM); block != nil {
			if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
				expiresAt = cert.NotAfter
			}
		}
	}
	if expiresAt.IsZero() {
		// An identity without an expiry cannot be safely renewed. The timer is
		// kept dormant and normal certificate verification remains authoritative.
		return 365 * 24 * time.Hour
	}
	delay := expiresAt.Add(-renewBefore).Sub(now)
	if delay < 0 {
		return 0
	}
	return delay
}

func (a *AgentClient) renewOverWebSocket(ctx context.Context, ws *webSocket, incoming <-chan inboundEnvelope, readErr <-chan error) (NodeIdentity, error) {
	requestID := randomRequestID()
	if err := ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "renewal_request", Request: requestID, SentAt: time.Now().UTC(), Body: mustJSON(RenewalRequest{ServerID: a.Identity.ServerID})}); err != nil {
		return NodeIdentity{}, err
	}
	for {
		select {
		case <-ctx.Done():
			return NodeIdentity{}, ctx.Err()
		case err := <-readErr:
			return NodeIdentity{}, err
		case message := <-incoming:
			envelope := message.envelope
			if envelope.Request != requestID {
				if envelope.Message == "action_request" {
					if err := a.handleIncoming(ctx, ws, message); err != nil {
						return NodeIdentity{}, err
					}
				}
				continue
			}
			if envelope.Message == "error" {
				return NodeIdentity{}, ErrIdentityRecoveryRequired
			}
			if envelope.Message != "renewal_response" {
				continue
			}
			var renewed NodeIdentity
			if err := decodeBody(envelope.Body, &renewed); err != nil {
				return NodeIdentity{}, fmt.Errorf("decode renewed identity: %w", err)
			}
			if err := validateNodeIdentity(renewed, a.Identity.ServerID); err != nil {
				return NodeIdentity{}, fmt.Errorf("invalid renewed identity: %w", err)
			}
			return renewed, nil
		}
	}
}

// BootstrapAgent performs the one-time protected enrollment exchange. The
// caller must persist the returned identity using SaveNodeIdentity only after
// this method succeeds; this function never writes credentials itself.
func BootstrapAgent(ctx context.Context, endpoint, jobID, token string, trustPEM []byte, tlsConfig *tls.Config) (NodeIdentity, error) {
	if endpoint == "" || jobID == "" || token == "" || len(token) > maxBootstrapToken {
		return NodeIdentity{}, ErrAgentNotConfigured
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "wss" || u.Host == "" {
		return NodeIdentity{}, fmt.Errorf("%w: bootstrap URL must use wss", ErrAgentNotConfigured)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/node/bootstrap/v1"
	}
	config := tlsConfig
	if config == nil {
		config = &tls.Config{MinVersion: tls.VersionTLS13, ServerName: u.Hostname()}
	} else {
		config = config.Clone()
		if config.MinVersion < tls.VersionTLS13 {
			config.MinVersion = tls.VersionTLS13
		}
	}
	if len(trustPEM) == 0 && config.RootCAs == nil {
		return NodeIdentity{}, errors.New("bootstrap trust anchor is required")
	}
	if len(trustPEM) > 0 && config.RootCAs == nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(trustPEM) {
			return NodeIdentity{}, errors.New("invalid hub trust anchor")
		}
		config.RootCAs = pool
	}
	configureTLSValidation(config, u)
	conn, err := (&tls.Dialer{Config: config}).DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return NodeIdentity{}, err
	}
	ws, err := clientUpgrade(conn, u.RequestURI())
	if err != nil {
		_ = conn.Close()
		return NodeIdentity{}, err
	}
	defer ws.Close()
	requestID := randomRequestID()
	if err := ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "bootstrap", Request: requestID, SentAt: time.Now().UTC(), Body: mustJSON(BootstrapRequest{JobID: jobID, Token: token})}); err != nil {
		return NodeIdentity{}, err
	}
	for {
		payload, opcode, err := ws.ReadMessage()
		if err != nil {
			return NodeIdentity{}, err
		}
		if opcode == wsOpcodePing {
			if err := ws.WriteMessage(wsOpcodePong, payload); err != nil {
				return NodeIdentity{}, err
			}
			continue
		}
		if opcode == wsOpcodeClose {
			return NodeIdentity{}, ErrWebSocketClosed
		}
		var envelope contracts.Envelope
		if err := decodeEnvelope(payload, &envelope); err != nil || envelope.Request != requestID {
			continue
		}
		if envelope.Message == "error" {
			return NodeIdentity{}, ErrIdentityRecoveryRequired
		}
		if envelope.Message != "bootstrap_response" {
			continue
		}
		var identity NodeIdentity
		if err := decodeBody(envelope.Body, &identity); err != nil {
			return NodeIdentity{}, fmt.Errorf("decode bootstrap identity: %w", err)
		}
		if err := validateNodeIdentity(identity, ""); err != nil {
			return NodeIdentity{}, fmt.Errorf("invalid bootstrap identity: %w", err)
		}
		return identity, nil
	}
}

func validateNodeIdentity(identity NodeIdentity, expectedServerID contracts.ServerID) error {
	if identity.ServerID == "" || len(identity.CertificatePEM) == 0 || len(identity.PrivateKeyPEM) == 0 {
		return errors.New("missing node identity material")
	}
	if expectedServerID != "" && identity.ServerID != expectedServerID {
		return errors.New("server identity mismatch")
	}
	cert, err := tls.X509KeyPair(identity.CertificatePEM, identity.PrivateKeyPEM)
	if err != nil || len(cert.Certificate) == 0 {
		return errors.New("certificate and private key do not match")
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil || parsed.Subject.CommonName != string(identity.ServerID) {
		return errors.New("certificate subject does not match server identity")
	}
	if !identity.NotAfter.IsZero() && !identity.NotAfter.Equal(parsed.NotAfter) {
		return errors.New("certificate expiry does not match identity")
	}
	if identity.Fingerprint != "" && identity.Fingerprint != fingerprint(parsed.Raw) {
		return errors.New("certificate fingerprint does not match identity")
	}
	return nil
}

func (a *AgentClient) replay(ctx context.Context, ws *webSocket, incoming <-chan inboundEnvelope, readErr <-chan error) error {
	for {
		records, err := a.Spool.Pending(1)
		if err != nil {
			return err
		}
		if len(records) == 0 {
			return nil
		}
		batch := records[0].Batch
		requestID := randomRequestID()
		if err := ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "sample_batch", Request: requestID, SentAt: time.Now().UTC(), Body: mustJSON(batch)}); err != nil {
			return err
		}
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case err := <-readErr:
				return err
			case message := <-incoming:
				envelope := message.envelope
				if envelope.Message == "action_request" {
					if err := a.handleIncoming(ctx, ws, message); err != nil {
						return err
					}
					continue
				}
				if envelope.Message != "acknowledgement" || envelope.Request != requestID {
					continue
				}
				var ack contracts.Acknowledgement
				if err := decodeBody(envelope.Body, &ack); err != nil {
					return err
				}
				if a.SamplingPolicy != nil && ack.SamplingIntervalSeconds >= 5 && ack.SamplingIntervalSeconds <= 3600 {
					a.SamplingPolicy(ack.SamplingIntervalSeconds)
				}
				if !ack.Accepted {
					if ack.Error != nil && ack.Error.Retryable {
						return errors.New(ack.Error.Code)
					}
					return errors.New("hub rejected sample batch")
				}
				epoch := batchEpoch(batch)
				if err := a.Spool.Acknowledge(epoch, ack.ThroughSequence); err != nil {
					return err
				}
				break
			}
			break
		}
	}
}

func (a *AgentClient) dial(ctx context.Context) (*webSocket, error) {
	u, err := url.Parse(a.URL)
	if err != nil || u.Scheme != "wss" || u.Host == "" {
		return nil, fmt.Errorf("%w: URL must use wss", ErrAgentNotConfigured)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/node/v1"
	}
	config := a.TLSConfig
	if config == nil {
		config = &tls.Config{MinVersion: tls.VersionTLS13, ServerName: u.Hostname()}
	} else {
		config = config.Clone()
		if config.MinVersion < tls.VersionTLS13 {
			config.MinVersion = tls.VersionTLS13
		}
	}
	if len(a.TrustPEM) > 0 && config.RootCAs == nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(a.TrustPEM) {
			return nil, errors.New("invalid hub trust anchor")
		}
		config.RootCAs = pool
	}
	configureTLSValidation(config, u)
	if len(a.Identity.CertificatePEM) == 0 || len(a.Identity.PrivateKeyPEM) == 0 {
		return nil, errors.New("node identity is missing certificate or key")
	}
	cert, err := tls.X509KeyPair(a.Identity.CertificatePEM, a.Identity.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("load node identity: %w", err)
	}
	config.Certificates = []tls.Certificate{cert}
	dialer := tls.Dialer{Config: config}
	conn, err := dialer.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	ws, err := clientUpgrade(conn, u.RequestURI())
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return ws, nil
}

func configureTLSValidation(config *tls.Config, u *url.URL) {
	if config == nil || u == nil {
		return
	}
	if net.ParseIP(u.Hostname()) != nil {
		config.InsecureSkipVerify = true
		config.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("no server certificate presented")
			}
			cert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("invalid server certificate: %w", err)
			}
			opts := x509.VerifyOptions{
				Roots:         config.RootCAs,
				CurrentTime:   time.Now(),
				Intermediates: x509.NewCertPool(),
			}
			for _, raw := range rawCerts[1:] {
				if ic, err := x509.ParseCertificate(raw); err == nil {
					opts.Intermediates.AddCert(ic)
				}
			}
			_, err = cert.Verify(opts)
			return err
		}
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func randomRequestID() string {
	return fmt.Sprintf("agent-%d-%d", time.Now().UnixNano(), os.Getpid())
}

// websocket implementation -------------------------------------------------

const (
	wsOpcodeContinuation = 0x0
	wsOpcodeText         = 0x1
	wsOpcodeBinary       = 0x2
	wsOpcodeClose        = 0x8
	wsOpcodePing         = 0x9
	wsOpcodePong         = 0xA
)

type webSocket struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
	mu   sync.Mutex
	mask bool
	// readMask is true on the server side. RFC 6455 requires client frames
	// to be masked and server frames to be unmasked.
	readMask bool
}

func upgradeWebSocket(w http.ResponseWriter, r *http.Request) (net.Conn, *webSocket, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !headerContainsToken(r.Header, "Connection", "upgrade") || r.Header.Get("Sec-WebSocket-Version") != webSocketVersion {
		http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
		return nil, nil, ErrWebSocketHandshake
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" || len(key) > 128 {
		http.Error(w, "missing websocket key", http.StatusBadRequest)
		return nil, nil, ErrWebSocketHandshake
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket unsupported", http.StatusHTTPVersionNotSupported)
		return nil, nil, ErrWebSocketHandshake
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	acceptHash := sha1.Sum([]byte(key + webSocketGUID))
	response := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(acceptHash[:]) + "\r\n\r\n"
	if _, err := rw.WriteString(response); err != nil || rw.Flush() != nil {
		_ = conn.Close()
		return nil, nil, ErrWebSocketHandshake
	}
	return conn, &webSocket{conn: conn, br: rw.Reader, bw: rw.Writer, readMask: true}, nil
}

func clientUpgrade(conn net.Conn, requestURI string) (*webSocket, error) {
	keyBytes := make([]byte, 16)
	if _, err := cryptorand.Read(keyBytes); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	request := "GET " + requestURI + " HTTP/1.1\r\nHost: " + conn.RemoteAddr().String() + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + key + "\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(conn, maxWebSocketHeader)
	response, err := http.ReadResponse(br, nil)
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		return nil, ErrWebSocketHandshake
	}
	acceptHash := sha1.Sum([]byte(key + webSocketGUID))
	if response.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(acceptHash[:]) {
		return nil, ErrWebSocketHandshake
	}
	return &webSocket{conn: conn, br: br, bw: bufio.NewWriter(conn), mask: true}, nil
}

func (ws *webSocket) Close() error                      { return ws.conn.Close() }
func (ws *webSocket) SetReadDeadline(t time.Time) error { return ws.conn.SetReadDeadline(t) }

func (ws *webSocket) ReadMessage() ([]byte, byte, error) {
	var message []byte
	var opcode byte
	for {
		first, err := ws.br.ReadByte()
		if err != nil {
			return nil, 0, err
		}
		second, err := ws.br.ReadByte()
		if err != nil {
			return nil, 0, err
		}
		fin := first&0x80 != 0
		currentOpcode := first & 0x0f
		if first&0x70 != 0 || currentOpcode == 0x3 || currentOpcode == 0x4 || currentOpcode == 0x5 || currentOpcode == 0x6 || currentOpcode == 0x7 || currentOpcode > wsOpcodePong {
			return nil, 0, ErrFrameMalformed
		}
		masked := second&0x80 != 0
		length := uint64(second & 0x7f)
		if length == 126 {
			var b [2]byte
			if _, err := io.ReadFull(ws.br, b[:]); err != nil {
				return nil, 0, err
			}
			length = uint64(b[0])<<8 | uint64(b[1])
		} else if length == 127 {
			var b [8]byte
			if _, err := io.ReadFull(ws.br, b[:]); err != nil {
				return nil, 0, err
			}
			for _, v := range b {
				length = length<<8 | uint64(v)
			}
		}
		if (currentOpcode == wsOpcodeClose || currentOpcode == wsOpcodePing || currentOpcode == wsOpcodePong) && (!fin || length > 125) {
			return nil, 0, ErrFrameMalformed
		}
		if length > contracts.MaxEnvelopeBytes || masked != ws.readMask {
			return nil, 0, ErrFrameMalformed
		}
		var maskKey [4]byte
		if masked {
			if _, err := io.ReadFull(ws.br, maskKey[:]); err != nil {
				return nil, 0, err
			}
		}
		payload := make([]byte, int(length))
		if _, err := io.ReadFull(ws.br, payload); err != nil {
			return nil, 0, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= maskKey[i%4]
			}
		}
		if currentOpcode == wsOpcodePing || currentOpcode == wsOpcodePong || currentOpcode == wsOpcodeClose {
			return payload, currentOpcode, nil
		}
		if currentOpcode != wsOpcodeContinuation {
			if opcode != 0 {
				return nil, 0, ErrFrameMalformed
			}
			opcode = currentOpcode
		} else if opcode == 0 {
			return nil, 0, ErrFrameMalformed
		}
		message = append(message, payload...)
		if len(message) > contracts.MaxEnvelopeBytes {
			return nil, 0, ErrFrameTooLarge
		}
		if fin {
			return message, opcode, nil
		}
	}
}

func (ws *webSocket) WriteEnvelope(envelope contracts.Envelope) error {
	if err := envelope.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(envelope)
	if err != nil || len(payload) > contracts.MaxEnvelopeBytes {
		return ErrFrameTooLarge
	}
	return ws.WriteMessage(wsOpcodeText, payload)
}

func (ws *webSocket) WriteMessage(opcode byte, payload []byte) error {
	if len(payload) > contracts.MaxEnvelopeBytes {
		return ErrFrameTooLarge
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	first := byte(0x80 | opcode)
	if err := ws.bw.WriteByte(first); err != nil {
		return err
	}
	if ws.mask {
		// Client frames must be masked. crypto/rand is unnecessary for the
		// masking key's security, but the per-frame key still must be unique
		// enough to avoid intermediaries treating it as a static transform.
		var key [4]byte
		if _, err := cryptorand.Read(key[:]); err != nil {
			return err
		}
		if len(payload) < 126 {
			_ = ws.bw.WriteByte(0x80 | byte(len(payload)))
		} else if len(payload) <= 65535 {
			_ = ws.bw.WriteByte(0x80 | 126)
			_ = ws.bw.WriteByte(byte(len(payload) >> 8))
			_ = ws.bw.WriteByte(byte(len(payload)))
		} else {
			_ = ws.bw.WriteByte(0x80 | 127)
			for shift := 56; shift >= 0; shift -= 8 {
				_ = ws.bw.WriteByte(byte(uint64(len(payload)) >> shift))
			}
		}
		if _, err := ws.bw.Write(key[:]); err != nil {
			return err
		}
		masked := append([]byte(nil), payload...)
		for i := range masked {
			masked[i] ^= key[i%4]
		}
		if _, err := ws.bw.Write(masked); err != nil {
			return err
		}
	} else {
		if len(payload) < 126 {
			if err := ws.bw.WriteByte(byte(len(payload))); err != nil {
				return err
			}
		} else if len(payload) <= 65535 {
			if err := ws.bw.WriteByte(126); err != nil {
				return err
			}
			if err := ws.bw.WriteByte(byte(len(payload) >> 8)); err != nil {
				return err
			}
			if err := ws.bw.WriteByte(byte(len(payload))); err != nil {
				return err
			}
		} else {
			if err := ws.bw.WriteByte(127); err != nil {
				return err
			}
			for shift := 56; shift >= 0; shift -= 8 {
				if err := ws.bw.WriteByte(byte(uint64(len(payload)) >> shift)); err != nil {
					return err
				}
			}
		}
		if _, err := ws.bw.Write(payload); err != nil {
			return err
		}
	}
	return ws.bw.Flush()
}

func headerContainsToken(header http.Header, name, wanted string) bool {
	for _, value := range header.Values(name) {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), wanted) {
				return true
			}
		}
	}
	return false
}
