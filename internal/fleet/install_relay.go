package fleet

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/install"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/netproxy"
)

const (
	// relayInstallGrantTTL bounds how long an installation credential works.
	relayInstallGrantTTL = 2 * time.Hour
	// relayReadyTimeout covers a few agent acknowledgements (15 s apart).
	relayReadyTimeout = 90 * time.Second
)

// validateRelay checks that relayID can relay for target before a job is
// queued, so a wrong choice fails immediately instead of after SSH setup.
func (s *InstallService) validateRelay(ctx context.Context, relayID, target contracts.ServerID) error {
	if len(relayID) < 16 || len(relayID) > 128 || !safeFleetID(string(relayID)) {
		return errors.New("relay_server_id is invalid")
	}
	if relayID == target {
		return errors.New("a server cannot be its own relay")
	}
	relay, found, err := s.Store.GetServer(ctx, relayID)
	if err != nil {
		return fmt.Errorf("relay lookup: %w", err)
	}
	if !found {
		return errors.New("relay server not found")
	}
	if relay.ConnectionState != "connected" {
		return fmt.Errorf("relay server %q is not connected", relay.Name)
	}
	if _, err := relayHost(relay); err != nil {
		return err
	}
	return nil
}

// relayHost is the address the hub and the target use to reach the relay.
func relayHost(relay contracts.Server) (string, error) {
	host := strings.TrimSpace(relay.Address)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" || strings.ContainsAny(host, " /@") || len(host) > 253 {
		return "", fmt.Errorf("relay server %q has no reachable address; set its address first", relay.Name)
	}
	return host, nil
}

// prepareRelay makes relayID a relay, issues one credential for this
// installation's SSH connection and one for the new node's hub connection,
// and waits until the relay accepts them. finish removes the installation
// credential, and the node credential too unless the installed node was
// configured to use it.
func (s *InstallService) prepareRelay(ctx context.Context, job contracts.Job, relayID contracts.ServerID, nodeRoute string, opts *install.SSHInstallOptions) (func(succeeded, nodeProxied bool), error) {
	if err := s.validateRelay(ctx, relayID, job.TargetServerID); err != nil {
		return nil, err
	}
	relay, _, err := s.Store.GetServer(ctx, relayID)
	if err != nil {
		return nil, err
	}
	host, err := relayHost(relay)
	if err != nil {
		return nil, err
	}
	isNode := opts.Role == "" || opts.Role == "node"
	hubAddr := ""
	if isNode {
		hubAddr, err = transportHostPort(opts.TransportURL)
		if err != nil {
			return nil, err
		}
	}
	now := s.now()
	_ = s.Store.PruneExpiredRelayGrants(ctx, now)
	port, err := s.Store.EnsureRelayNode(ctx, relayID, monitoring.DefaultRelayPort, now)
	if err != nil {
		return nil, fmt.Errorf("configure relay: %w", err)
	}
	relayAddr := net.JoinHostPort(host, strconv.Itoa(port))
	sshTarget := net.JoinHostPort(opts.Endpoint.Host, strconv.Itoa(opts.Endpoint.Port))

	installUser, installPassword := "install-"+job.ID, randomRelaySecret()
	nodeUser, nodePassword := "node-"+string(job.TargetServerID), randomRelaySecret()
	if err := s.Store.PutRelayGrant(ctx, monitoring.RelayGrantRecord{RelayServerID: relayID, Username: installUser, PasswordSHA256: netproxy.HashPassword(installPassword), Destinations: []string{sshTarget}, ExpiresAt: now.Add(relayInstallGrantTTL)}, now); err != nil {
		return nil, fmt.Errorf("configure relay: %w", err)
	}
	finish := func(succeeded, nodeProxied bool) {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Store.DeleteRelayGrant(cleanup, relayID, installUser)
		if isNode && (!succeeded || !nodeProxied) {
			_ = s.Store.DeleteRelayGrant(cleanup, relayID, nodeUser)
		}
	}
	if isNode {
		if err := s.Store.PutRelayGrant(ctx, monitoring.RelayGrantRecord{RelayServerID: relayID, Username: nodeUser, PasswordSHA256: netproxy.HashPassword(nodePassword), Destinations: []string{hubAddr}, TargetServerID: job.TargetServerID}, now); err != nil {
			finish(false, false)
			return nil, fmt.Errorf("configure relay: %w", err)
		}
	}
	installProxy := &netproxy.Proxy{Scheme: "http", Host: relayAddr, Username: installUser, Password: installPassword}
	nodeProxy := &netproxy.Proxy{Scheme: "http", Host: relayAddr, Username: nodeUser, Password: nodePassword}
	if err := s.waitForRelay(ctx, installProxy, sshTarget, relay.Name, port); err != nil {
		finish(false, false)
		return nil, err
	}
	opts.Proxy = installProxy
	opts.NodeProxy, opts.NodeProxyAuto = "", false
	if isNode && nodeRoute != "direct" {
		opts.NodeProxy, opts.NodeProxyAuto = nodeProxy.String(), nodeRoute != "proxy"
	}
	opts.DownloadArtifacts = nil
	if opts.SelectArtifacts == nil {
		opts.SelectArtifacts = func(arch, role string) (map[string]string, error) {
			return install.SelectLinuxArtifacts(s.InstallerPath, s.Artifacts, s.MatrixDir, arch, role, s.VerifyArtifact)
		}
	}
	return finish, nil
}

// waitForRelay retries until the relay has picked up the new credential
// (it arrives with the relay's next acknowledgement) and can reach target.
func (s *InstallService) waitForRelay(ctx context.Context, proxy *netproxy.Proxy, target, relayName string, port int) error {
	probe := s.relayReady
	if probe == nil {
		probe = func(ctx context.Context, proxy *netproxy.Proxy, target string) error {
			conn, err := proxy.DialContext(ctx, target)
			if err == nil {
				_ = conn.Close()
			}
			return err
		}
	}
	deadline := time.Now().Add(relayReadyTimeout)
	var lastErr error
	for {
		attempt, cancel := context.WithTimeout(ctx, 15*time.Second)
		lastErr = probe(attempt, proxy, target)
		cancel()
		if lastErr == nil {
			return nil
		}
		// The relay answered and could not reach the target: retrying won't help.
		if strings.Contains(lastErr.Error(), "502") {
			return fmt.Errorf("relay %q cannot reach %s; check that the target's SSH port is open to the relay", relayName, target)
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return fmt.Errorf("relay %q did not accept connections on port %d (%v); update its agent to a version with relay support and allow TCP port %d from the hub", relayName, port, lastErr, port)
}

// transportHostPort returns host:port of the hub's node transport endpoint,
// which is the only destination a node credential may reach.
func transportHostPort(transportURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(transportURL))
	if err != nil || u.Scheme != "wss" || u.Hostname() == "" {
		return "", errors.New("the hub's node transport address is not configured")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

func randomRelaySecret() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}
