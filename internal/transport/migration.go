package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

var errTransportMigrated = errors.New("node transport migrated")

type transportMigration struct {
	URL string `json:"url"`
}
type transportMigrationResult struct {
	Error string `json:"error"`
}
type agentEndpoint struct {
	Configured string `json:"configured"`
	Current    string `json:"current"`
	Previous   string `json:"previous,omitempty"`
}

func validMigration(current, target string) bool {
	old, e1 := url.Parse(current)
	next, e2 := url.Parse(target)
	if e2 != nil {
		return false
	}
	port, err := strconv.Atoi(next.Port())
	if err != nil || port < 1 || port > 65535 {
		return false
	}
	return e1 == nil && e2 == nil && next.Scheme == "wss" && old.Hostname() == next.Hostname() && next.User == nil && next.Path == "/node/v1" && next.RawQuery == "" && next.Fragment == "" && next.Port() != ""
}
func (a *AgentClient) loadEndpoint() error {
	a.configuredURL = a.URL
	if a.IdentityPath == "" {
		return nil
	}
	data, err := os.ReadFile(a.IdentityPath + ".transport.json")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved agentEndpoint
	if err = json.Unmarshal(data, &saved); err != nil {
		return err
	}
	// A new enrollment or explicit configuration for another endpoint takes precedence.
	if saved.Configured != a.configuredURL {
		return nil
	}
	if saved.Current != a.configuredURL && !validMigration(a.configuredURL, saved.Current) {
		return errors.New("invalid persisted node endpoint")
	}
	if saved.Previous != "" && saved.Previous != a.configuredURL && !validMigration(a.configuredURL, saved.Previous) {
		return errors.New("invalid fallback node endpoint")
	}
	a.URL = saved.Current
	a.previousURL = saved.Previous
	return nil
}
func (a *AgentClient) saveEndpoint(current, previous string) error {
	if a.IdentityPath == "" {
		return errors.New("node endpoint persistence is unavailable")
	}
	data, err := json.Marshal(agentEndpoint{Configured: a.configuredURL, Current: current, Previous: previous})
	if err != nil {
		return err
	}
	path := a.IdentityPath + ".transport.json"
	return persistTransportFile(path, data, a.endpointSync)
}

// CheckEndpoint checks the same durable endpoint used by Run, without opening
// a controller session or changing persisted state. An explicitly changed
// configured URL takes precedence over a migration saved for another URL.
func (a *AgentClient) CheckEndpoint(ctx context.Context) error {
	if a == nil || a.URL == "" {
		return ErrAgentNotConfigured
	}
	if err := a.loadEndpoint(); err != nil {
		return err
	}
	return CheckNodeEndpoint(ctx, a.URL, a.Identity, a.TrustPEM)
}

// CheckNodeEndpoint verifies the node-to-hub TLS/WebSocket path without
// opening a controller session, collecting samples, or changing local state.
func CheckNodeEndpoint(ctx context.Context, endpoint string, identity NodeIdentity, trustPEM []byte) error {
	if len(trustPEM) == 0 {
		return errors.New("hub trust anchor is required")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := &AgentClient{URL: endpoint, Identity: identity, TrustPEM: trustPEM}
	ws, err := client.dial(probeCtx)
	if err != nil {
		return err
	}
	return ws.Close()
}

func (a *AgentClient) migrate(ctx context.Context, ws *webSocket, target string) error {
	if target == a.URL {
		return nil
	}
	failure := func(message string) error {
		return ws.WriteEnvelope(contracts.Envelope{Protocol: contracts.NodeProtocol, Message: "transport_result", SentAt: time.Now().UTC(), Body: mustJSON(transportMigrationResult{Error: message})})
	}
	if !validMigration(a.URL, target) {
		return failure("The proposed endpoint must use the same trusted hub host and a valid node transport port.")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// The authenticated WebSocket upgrade verifies reachability and TLS trust
	// without claiming a second active controller slot (no hello is sent).
	probe, err := a.dialURL(probeCtx, target)
	if err != nil {
		return failure("Cannot reach the new node port with the installed TLS trust; check its firewall and TLS configuration.")
	}
	_ = probe.Close()
	if err = a.saveEndpoint(target, a.URL); err != nil {
		return failure("Could not save the new node endpoint; the current connection is retained.")
	}
	a.previousURL = a.URL
	a.URL = target
	return errTransportMigrated
}
func (p *NodePorts) migrationURL(c *Connection) string {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	supported := false
	for _, cap := range c.hello.Capabilities {
		if cap == MigrationCapability {
			supported = true
		}
	}
	if !supported || c.closed {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.Nodes[c.ServerID].Port == p.currentLocked() {
		return ""
	}
	return p.publicURL(p.currentLocked())
}
func localPort(ws *webSocket) string {
	_, port, _ := net.SplitHostPort(ws.conn.LocalAddr().String())
	return port
}
