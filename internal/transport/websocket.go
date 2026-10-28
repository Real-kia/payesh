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
	"github.com/Real-kia/payesh/internal/netproxy"
)

const (
	webSocketVersion      = "13"
	webSocketGUID         = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	maxWebSocketHeader    = 16 << 10
	WebSocketReadDeadline = 2 * time.Minute
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
func DecodeEnvelope(payload []byte, out *contracts.Envelope) error {
	if len(payload) == 0 || len(payload) > contracts.MaxEnvelopeBytes {
		return ErrFrameTooLarge
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return err
	}
	return out.Validate()
}

func DecodeBody(body json.RawMessage, out any) error {
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

func ErrorBody(code, message string, retryable bool) json.RawMessage {
	body, _ := json.Marshal(contracts.Error{Code: code, Message: message, Retryable: retryable})
	return body
}

func PEMCertificate(cert *x509.Certificate) []byte {
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
	endpointSync  func(*os.File) error
	configuredURL string
	previousURL   string
	URL           string
	Identity      NodeIdentity
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
	// RelayPolicy receives the hub's relay configuration when the agent
	// advertises RelayCapability.
	RelayPolicy func(contracts.RelayPolicy)
	// Addresses lists the node's interface addresses. It is called only when
	// the hub asks for them, at most every addressCheckInterval.
	Addresses func() []string
	// addressState is per connection: what was last reported and when the
	// interfaces were last read.
	addressesSent    bool
	addressesKey     string
	addressesChecked time.Time
	// Proxy, when set, carries the hub connection through an HTTP CONNECT or
	// SOCKS5 proxy. TLS still runs end to end with the hub.
	Proxy *netproxy.Proxy
	// Logf, when set, receives rate-limited reports of failed connections and
	// the following recovery. Without it reconnects stay silent.
	Logf          func(format string, args ...any)
	actionMu      sync.Mutex
	actionResult  map[string]contracts.ActionResponse
	actionRunning map[string]bool
}

// Only advertise an extension to peers that explicitly support it. Older
// agents reject unknown acknowledgement fields rather than ignoring them.
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
	if err := a.LoadEndpoint(); err != nil {
		return err
	}
	backoff := a.Backoff
	if backoff.Base <= 0 {
		backoff.Base = time.Second
	}
	if backoff.Max <= 0 {
		backoff.Max = 60 * time.Second
	}
	reporter := connectionReporter{logf: a.Logf}
	for {
		dialCtx, cancelDial := context.WithTimeout(ctx, 15*time.Second)
		ws, err := a.dial(dialCtx)
		cancelDial()

		if err == nil {
			backoff.Reset()
			// A hub can accept the TLS dial and still end the session at once
			// (rejected hello, revoked identity); only a session that stays up
			// counts as a recovery worth reporting.
			endpoint := a.URL
			stable := time.AfterFunc(connectionStableAfter, func() { reporter.connected(endpoint) })
			err = a.session(ctx, ws, batches)
			stable.Stop()
			_ = ws.Close()
			if err == nil {
				return nil
			}
		}
		if errors.Is(err, errTransportMigrated) {
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		reporter.failed(a.URL, err)
		if err != nil && a.previousURL != "" {
			// Try both retained endpoints until one accepts our hello. Keep
			// the verified candidate on disk until authentication succeeds;
			// a transient failure must not strand us on a retired old port.
			a.URL, a.previousURL = a.previousURL, a.URL
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

func (a *AgentClient) session(ctx context.Context, ws *WebSocket, batches <-chan contracts.SampleBatch) error {
	ctx, cancelSession := context.WithCancel(ctx)
	defer cancelSession()
	incoming := make(chan inboundEnvelope, 16)
	readErr := make(chan error, 1)
	go a.readIncoming(ctx, ws, incoming, readErr)
	a.addressesSent = false
	if err := ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "hello", SentAt: time.Now().UTC(), Body: MustJSON(a.Hello)}); err != nil {
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
			if err := ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "heartbeat", SentAt: time.Now().UTC(), Body: MustJSON(contracts.Heartbeat{ServerID: a.Identity.ServerID, SentAt: time.Now().UTC(), Health: "healthy"})}); err != nil {
				return err
			}
		}
	}
}

func (a *AgentClient) readIncoming(ctx context.Context, ws *WebSocket, incoming chan<- inboundEnvelope, readErr chan<- error) {
	for {
		if err := ws.SetReadDeadline(time.Now().Add(WebSocketReadDeadline)); err != nil {
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
		if opcode == WSOpcodePing {
			if err := ws.WriteMessage(WSOpcodePong, payload); err != nil {
				readErr <- err
				return
			}
			continue
		}
		if opcode == WSOpcodeClose {
			readErr <- ErrWebSocketClosed
			return
		}
		if opcode != WSOpcodeText && opcode != WSOpcodeBinary {
			continue
		}
		var envelope contracts.Envelope
		if err := DecodeEnvelope(payload, &envelope); err != nil {
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

func (a *AgentClient) handleIncoming(ctx context.Context, ws *WebSocket, message inboundEnvelope) error {
	envelope := message.envelope
	if envelope.Message == "hello_accepted" {
		if a.previousURL != "" {
			if err := a.SaveEndpoint(a.URL, ""); err != nil {
				return err
			}
			a.previousURL = ""
		}
		return nil
	}
	if envelope.Message == "transport_migrate" {
		var migration TransportMigration
		if err := DecodeBody(envelope.Body, &migration); err != nil {
			return err
		}
		return a.migrate(ctx, ws, migration.URL)
	}
	if envelope.Message != "action_request" {
		return nil
	}
	var request contracts.ActionRequest
	if err := DecodeBody(envelope.Body, &request); err != nil {
		return err
	}
	if envelope.Request != "" && request.RequestID != envelope.Request {
		return errors.New("action request id mismatch")
	}
	if request.Action == "core.update" {
		// Core activation can restart this process and take several minutes.
		// Keep the measurement/control reader responsive while the node-owned
		// worker completes. Durable node intent handles reconnect redelivery.
		a.actionMu.Lock()
		if a.actionRunning == nil {
			a.actionRunning = map[string]bool{}
		}
		if a.actionRunning[request.IdempotencyKey] {
			a.actionMu.Unlock()
			return nil
		}
		a.actionRunning[request.IdempotencyKey] = true
		a.actionMu.Unlock()
		go func() {
			defer func() { a.actionMu.Lock(); delete(a.actionRunning, request.IdempotencyKey); a.actionMu.Unlock() }()
			response := a.executeAction(ctx, request)
			if ctx.Err() == nil && (response.Error == nil || response.Error.Code != "core_update_interrupted") {
				_ = ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "action_response", Request: request.RequestID, SentAt: time.Now().UTC(), Body: MustJSON(response)})
			}
		}()
		return nil
	}
	response := a.executeAction(ctx, request)
	return ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "action_response", Request: request.RequestID, SentAt: time.Now().UTC(), Body: MustJSON(response)})
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
	if request.Action == "core.update" {
		return response
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

func (a *AgentClient) renewOverWebSocket(ctx context.Context, ws *WebSocket, incoming <-chan inboundEnvelope, readErr <-chan error) (NodeIdentity, error) {
	requestID := randomRequestID()
	if err := ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "renewal_request", Request: requestID, SentAt: time.Now().UTC(), Body: MustJSON(RenewalRequest{ServerID: a.Identity.ServerID})}); err != nil {
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
				if envelope.Message == "action_request" || envelope.Message == "transport_migrate" || envelope.Message == "hello_accepted" {
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
			if err := DecodeBody(envelope.Body, &renewed); err != nil {
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
	return BootstrapAgentVia(ctx, nil, endpoint, jobID, token, trustPEM, tlsConfig)
}

// RelayCapability is advertised by agents that can run a relay.
const RelayCapability = "relay"

// AddressesCapability is advertised by agents that report their interface
// addresses when the hub asks.
const AddressesCapability = "addresses"

// dialTLS opens the hub connection directly or through proxy. Certificate
// checks are identical either way because TLS runs inside the tunnel.
func dialTLS(ctx context.Context, proxy *netproxy.Proxy, addr string, config *tls.Config) (net.Conn, error) {
	if proxy == nil {
		return (&tls.Dialer{Config: config}).DialContext(ctx, "tcp", addr)
	}
	raw, err := proxy.DialContext(ctx, addr)
	if err != nil {
		return nil, err
	}
	conn := tls.Client(raw, config)
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return conn, nil
}

// BootstrapAgentVia is BootstrapAgent through an optional proxy.
func BootstrapAgentVia(ctx context.Context, proxy *netproxy.Proxy, endpoint, jobID, token string, trustPEM []byte, tlsConfig *tls.Config) (NodeIdentity, error) {
	if endpoint == "" || jobID == "" || token == "" || len(token) > MaxBootstrapToken {
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
	conn, err := dialTLS(ctx, proxy, u.Host, config)
	if err != nil {
		return NodeIdentity{}, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	ws, err := clientUpgrade(conn, u.RequestURI())
	if err != nil {
		_ = conn.Close()
		return NodeIdentity{}, err
	}
	defer ws.Close()
	requestID := randomRequestID()
	if err := ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "bootstrap", Request: requestID, SentAt: time.Now().UTC(), Body: MustJSON(BootstrapRequest{JobID: jobID, Token: token})}); err != nil {
		return NodeIdentity{}, err
	}
	for {
		payload, opcode, err := ws.ReadMessage()
		if err != nil {
			return NodeIdentity{}, err
		}
		if opcode == WSOpcodePing {
			if err := ws.WriteMessage(WSOpcodePong, payload); err != nil {
				return NodeIdentity{}, err
			}
			continue
		}
		if opcode == WSOpcodeClose {
			return NodeIdentity{}, ErrWebSocketClosed
		}
		var envelope contracts.Envelope
		if err := DecodeEnvelope(payload, &envelope); err != nil || envelope.Request != requestID {
			continue
		}
		if envelope.Message == "error" {
			var errBody contracts.Error
			if err := DecodeBody(envelope.Body, &errBody); err == nil && errBody.Message != "" {
				return NodeIdentity{}, fmt.Errorf("%w: %s (%s)", ErrIdentityRecoveryRequired, errBody.Message, errBody.Code)
			}
			return NodeIdentity{}, ErrIdentityRecoveryRequired
		}
		if envelope.Message != "bootstrap_response" {
			continue
		}
		var identity NodeIdentity
		if err := DecodeBody(envelope.Body, &identity); err != nil {
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

func (a *AgentClient) replay(ctx context.Context, ws *WebSocket, incoming <-chan inboundEnvelope, readErr <-chan error) error {
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
		if err := ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "sample_batch", Request: requestID, SentAt: time.Now().UTC(), Body: MustJSON(batch)}); err != nil {
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
				if envelope.Message == "action_request" || envelope.Message == "transport_migrate" || envelope.Message == "hello_accepted" {
					if err := a.handleIncoming(ctx, ws, message); err != nil {
						return err
					}
					continue
				}
				if envelope.Message != "acknowledgement" || envelope.Request != requestID {
					continue
				}
				var ack contracts.Acknowledgement
				if err := DecodeBody(envelope.Body, &ack); err != nil {
					return err
				}
				if a.SamplingPolicy != nil && ack.SamplingIntervalSeconds >= 5 && ack.SamplingIntervalSeconds <= 3600 {
					a.SamplingPolicy(ack.SamplingIntervalSeconds)
				}
				if a.RelayPolicy != nil && ack.Relay != nil {
					a.RelayPolicy(*ack.Relay)
				}
				if ack.Addresses {
					if err := a.reportAddresses(ws, time.Now()); err != nil {
						return err
					}
				}
				if !ack.Accepted {
					if ack.Error != nil && ack.Error.Retryable {
						return errors.New(ack.Error.Code)
					}
					return errors.New("hub rejected sample batch")
				}
				epoch := BatchEpoch(batch)
				if err := a.Spool.Acknowledge(epoch, ack.ThroughSequence); err != nil {
					return err
				}
				break
			}
			break
		}
	}
}

// addressCheckInterval is how often a node re-reads its interfaces.
const addressCheckInterval = 10 * time.Minute

// reportAddresses sends the node's addresses once per connection and again
// when they change. Interfaces are read at most every addressCheckInterval.
func (a *AgentClient) reportAddresses(ws *WebSocket, now time.Time) error {
	if a.Addresses == nil || (a.addressesSent && now.Sub(a.addressesChecked) < addressCheckInterval) {
		return nil
	}
	a.addressesChecked = now
	addresses := a.Addresses()
	key := strings.Join(addresses, ",")
	if a.addressesSent && key == a.addressesKey {
		return nil
	}
	if addresses == nil {
		addresses = []string{}
	}
	if err := ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "addresses", SentAt: now.UTC(), Body: MustJSON(contracts.NodeAddresses{Addresses: addresses})}); err != nil {
		return err
	}
	a.addressesSent, a.addressesKey = true, key
	return nil
}

func (a *AgentClient) dial(ctx context.Context) (*WebSocket, error) {
	return a.dialURL(ctx, a.URL)
}

func (a *AgentClient) dialURL(ctx context.Context, endpoint string) (*WebSocket, error) {
	u, err := url.Parse(endpoint)
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
	conn, err := dialTLS(ctx, a.Proxy, u.Host, config)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	ws, err := clientUpgrade(conn, u.RequestURI())
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
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

func MustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func randomRequestID() string {
	return fmt.Sprintf("agent-%d-%d", time.Now().UnixNano(), os.Getpid())
}

// websocket implementation -------------------------------------------------

const (
	wsOpcodeContinuation = 0x0
	WSOpcodeText         = 0x1
	WSOpcodeBinary       = 0x2
	WSOpcodeClose        = 0x8
	WSOpcodePing         = 0x9
	WSOpcodePong         = 0xA
)

// NewWebSocket wraps an established connection that has already completed the
// HTTP upgrade. server selects the server side: it expects masked frames and
// sends unmasked ones, as RFC 6455 requires.
func NewWebSocket(conn net.Conn, server bool) *WebSocket {
	return &WebSocket{conn: conn, br: bufio.NewReader(conn), bw: bufio.NewWriter(conn), mask: !server, readMask: server}
}

// LocalAddr and RemoteAddr describe the underlying connection; they are nil
// when the socket has none.
func (ws *WebSocket) LocalAddr() net.Addr {
	if ws == nil || ws.conn == nil {
		return nil
	}
	return ws.conn.LocalAddr()
}

func (ws *WebSocket) RemoteAddr() net.Addr {
	if ws == nil || ws.conn == nil {
		return nil
	}
	return ws.conn.RemoteAddr()
}

type WebSocket struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
	mu   sync.Mutex
	mask bool
	// readMask is true on the server side. RFC 6455 requires client frames
	// to be masked and server frames to be unmasked.
	readMask bool
}

func UpgradeWebSocket(w http.ResponseWriter, r *http.Request) (net.Conn, *WebSocket, error) {
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
	return conn, &WebSocket{conn: conn, br: rw.Reader, bw: rw.Writer, readMask: true}, nil
}

func clientUpgrade(conn net.Conn, requestURI string) (*WebSocket, error) {
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
	return &WebSocket{conn: conn, br: br, bw: bufio.NewWriter(conn), mask: true}, nil
}

func (ws *WebSocket) Close() error                      { return ws.conn.Close() }
func (ws *WebSocket) SetReadDeadline(t time.Time) error { return ws.conn.SetReadDeadline(t) }

func (ws *WebSocket) ReadMessage() ([]byte, byte, error) {
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
		if first&0x70 != 0 || currentOpcode == 0x3 || currentOpcode == 0x4 || currentOpcode == 0x5 || currentOpcode == 0x6 || currentOpcode == 0x7 || currentOpcode > WSOpcodePong {
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
		if (currentOpcode == WSOpcodeClose || currentOpcode == WSOpcodePing || currentOpcode == WSOpcodePong) && (!fin || length > 125) {
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
		if currentOpcode == WSOpcodePing || currentOpcode == WSOpcodePong || currentOpcode == WSOpcodeClose {
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

func (ws *WebSocket) WriteEnvelope(envelope contracts.Envelope) error {
	if err := envelope.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(envelope)
	if err != nil || len(payload) > contracts.MaxEnvelopeBytes {
		return ErrFrameTooLarge
	}
	return ws.WriteMessage(WSOpcodeText, payload)
}

func (ws *WebSocket) WriteMessage(opcode byte, payload []byte) error {
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
