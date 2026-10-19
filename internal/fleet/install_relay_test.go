package fleet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/install"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/netproxy"
	"github.com/Real-kia/payesh/internal/transport"
)

func relayFixture(t *testing.T) (*InstallService, *monitoring.Store, contracts.ServerID, contracts.ServerID) {
	t.Helper()
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := time.Now().UTC()
	relay := contracts.Server{ID: "server-relay-iran-0001", Name: "Iran relay", Address: "203.0.113.5", Role: "node", Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics"}, ConnectionState: "connected", FreshnessState: "fresh", LastHeartbeat: &now}
	target := contracts.Server{ID: "server-iran-target-0001", Name: "Iran target", Role: "node", Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics"}, ConnectionState: "never-connected", FreshnessState: "unknown"}
	for _, server := range []contracts.Server{relay, target} {
		if err := store.UpsertServer(ctx, server); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := NewInstallService(store)
	if err != nil {
		t.Fatal(err)
	}
	return svc, store, relay.ID, target.ID
}

func TestPrepareRelayIssuesScopedCredentials(t *testing.T) {
	svc, store, relayID, targetID := relayFixture(t)
	ctx := context.Background()
	var probed []string
	svc.relayReady = func(_ context.Context, proxy *netproxy.Proxy, target string) error {
		probed = append(probed, proxy.Host+" "+target)
		return nil
	}
	opts := install.SSHInstallOptions{Endpoint: install.SSHEndpoint{Host: "10.20.30.40", Port: 22, User: "root"}, TransportURL: "wss://hub.example.test:9797/node/v1",
		DownloadArtifacts: func(context.Context, string, string) (install.ArtifactDownload, error) {
			return install.ArtifactDownload{}, nil
		}}
	finish, err := svc.prepareRelay(ctx, contracts.Job{ID: "job-0001", TargetServerID: targetID}, relayID, "auto", &opts)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Proxy == nil || opts.Proxy.Host != "203.0.113.5:9798" || opts.DownloadArtifacts != nil {
		t.Fatalf("install proxy = %+v, download kept = %v", opts.Proxy, opts.DownloadArtifacts != nil)
	}
	node, err := netproxy.Parse(opts.NodeProxy)
	if err != nil || node.Host != "203.0.113.5:9798" || node.Password == "" || node.Password == opts.Proxy.Password {
		t.Fatalf("node proxy = %q, %v", opts.NodeProxy, err)
	}
	if len(probed) != 1 || probed[0] != "203.0.113.5:9798 10.20.30.40:22" {
		t.Fatalf("readiness probe = %q", probed)
	}

	policy, err := store.RelayPolicy(ctx, relayID, time.Now().UTC())
	if err != nil || !policy.Enabled || policy.Port != monitoring.DefaultRelayPort || len(policy.Grants) != 2 {
		t.Fatalf("relay policy = %+v, %v", policy, err)
	}
	for _, grant := range policy.Grants {
		switch {
		case strings.HasPrefix(grant.Username, "install-"):
			if strings.Join(grant.Destinations, ",") != "10.20.30.40:22" || grant.ExpiresAt == nil || grant.PasswordSHA256 != netproxy.HashPassword(opts.Proxy.Password) {
				t.Fatalf("install grant = %+v", grant)
			}
		case strings.HasPrefix(grant.Username, "node-"):
			if strings.Join(grant.Destinations, ",") != "hub.example.test:9797" || grant.ExpiresAt != nil || grant.PasswordSHA256 != netproxy.HashPassword(node.Password) {
				t.Fatalf("node grant = %+v", grant)
			}
		default:
			t.Fatalf("unexpected grant %+v", grant)
		}
	}

	if !opts.NodeProxyAuto {
		t.Fatal("auto node route was not passed to the installer")
	}
	finish(true, true)
	policy, _ = store.RelayPolicy(ctx, relayID, time.Now().UTC())
	if len(policy.Grants) != 1 || !strings.HasPrefix(policy.Grants[0].Username, "node-") {
		t.Fatalf("after success only the node credential should remain: %+v", policy.Grants)
	}
	// Deleting the target server removes its relay credential.
	if err := store.DeleteServer(ctx, targetID, 0); err != nil {
		t.Fatal(err)
	}
	policy, _ = store.RelayPolicy(ctx, relayID, time.Now().UTC())
	if len(policy.Grants) != 0 {
		t.Fatalf("deleted server kept its relay credential: %+v", policy.Grants)
	}
}

func TestPrepareRelayFailureRemovesCredentials(t *testing.T) {
	svc, store, relayID, targetID := relayFixture(t)
	ctx := context.Background()
	svc.relayReady = func(context.Context, *netproxy.Proxy, string) error {
		return errors.New("proxy refused the tunnel: 502 Bad Gateway")
	}
	opts := install.SSHInstallOptions{Endpoint: install.SSHEndpoint{Host: "10.20.30.40", Port: 22, User: "root"}, TransportURL: "wss://hub.example.test:9797/node/v1"}
	if _, err := svc.prepareRelay(ctx, contracts.Job{ID: "job-0002", TargetServerID: targetID}, relayID, "auto", &opts); err == nil || !strings.Contains(err.Error(), "cannot reach 10.20.30.40:22") {
		t.Fatalf("err = %v", err)
	}
	policy, _ := store.RelayPolicy(ctx, relayID, time.Now().UTC())
	if len(policy.Grants) != 0 {
		t.Fatalf("failed relay setup left credentials: %+v", policy.Grants)
	}
}

func TestValidateRelayRejectsUnusableRelays(t *testing.T) {
	svc, store, relayID, targetID := relayFixture(t)
	ctx := context.Background()
	if err := svc.validateRelay(ctx, targetID, relayID); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("disconnected relay: %v", err)
	}
	if err := svc.validateRelay(ctx, relayID, relayID); err == nil {
		t.Fatal("a server was accepted as its own relay")
	}
	relay, _, _ := store.GetServer(ctx, relayID)
	relay.Address = "joins by command"
	if err := store.UpsertServer(ctx, relay); err != nil {
		t.Fatal(err)
	}
	if err := svc.validateRelay(ctx, relayID, targetID); err == nil || !strings.Contains(err.Error(), "no reachable address") {
		t.Fatalf("relay without address: %v", err)
	}
}

func TestInstallRequestHashIgnoresProxyCredentials(t *testing.T) {
	svc, _, _, targetID := relayFixture(t)
	svc.Executor = SSHInstallerFunc(func(context.Context, install.SSHInstallOptions) (install.SSHInstallResult, error) {
		return install.SSHInstallResult{}, nil
	})
	ctx := context.Background()
	request := installRequest{ServerID: string(targetID), Host: "10.20.30.40", Port: 22, User: "root", Password: "pw", IdempotencyKey: "idem-proxy-0001", ProxyURL: "http://u:first@203.0.113.9:3128"}
	first, err := svc.Enqueue(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	request.ProxyURL = "http://u:second@203.0.113.9:3128"
	second, err := svc.Enqueue(ctx, request)
	if err != nil || second.ID != first.ID {
		t.Fatalf("retry with a new proxy password: %v (job %s vs %s)", err, second.ID, first.ID)
	}
	request.ProxyURL = "http://u:second@203.0.113.10:3128"
	if _, err := svc.Enqueue(ctx, request); !errors.Is(err, monitoring.ErrJobIdempotencyConflict) {
		t.Fatalf("different proxy host must be a different request: %v", err)
	}
	request.ProxyURL, request.RelayServerID, request.IdempotencyKey = "http://u:p@203.0.113.9:3128", "server-relay-iran-0001", "idem-proxy-0002"
	if _, err := svc.Enqueue(ctx, request); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("proxy and relay together: %v", err)
	}
	request.ProxyURL, request.RelayServerID = "ftp://nope:1", ""
	if _, err := svc.Enqueue(ctx, request); err == nil {
		t.Fatal("invalid proxy URL accepted")
	}
}

func TestRelayNodeCredentialRemovedWhenNodeIsDirect(t *testing.T) {
	svc, store, relayID, targetID := relayFixture(t)
	ctx := context.Background()
	svc.relayReady = func(context.Context, *netproxy.Proxy, string) error { return nil }
	opts := install.SSHInstallOptions{Endpoint: install.SSHEndpoint{Host: "10.20.30.40", Port: 22, User: "root"}, TransportURL: "wss://hub.example.test:9797/node/v1"}
	finish, err := svc.prepareRelay(ctx, contracts.Job{ID: "job-0003", TargetServerID: targetID}, relayID, "auto", &opts)
	if err != nil {
		t.Fatal(err)
	}
	finish(true, false)
	policy, _ := store.RelayPolicy(ctx, relayID, time.Now().UTC())
	if len(policy.Grants) != 0 {
		t.Fatalf("a direct node kept relay credentials: %+v", policy.Grants)
	}
	direct := install.SSHInstallOptions{Endpoint: install.SSHEndpoint{Host: "10.20.30.40", Port: 22, User: "root"}, TransportURL: "wss://hub.example.test:9797/node/v1"}
	if _, err := svc.prepareRelay(ctx, contracts.Job{ID: "job-0004", TargetServerID: targetID}, relayID, "direct", &direct); err != nil {
		t.Fatal(err)
	}
	if direct.NodeProxy != "" || direct.Proxy == nil {
		t.Fatalf("direct node route: proxy=%v nodeProxy=%q", direct.Proxy, direct.NodeProxy)
	}
}

func TestSeparateHubRelayDoesNotIssuePersistentNodeGrant(t *testing.T) {
	svc, store, relayID, targetID := relayFixture(t)
	svc.relayReady = func(context.Context, *netproxy.Proxy, string) error { return nil }
	opts := install.SSHInstallOptions{Role: "hub", Endpoint: install.SSHEndpoint{Host: "192.0.2.10", Port: 22, User: "root"}}
	finish, err := svc.prepareRelay(t.Context(), contracts.Job{ID: "hub-install-test", TargetServerID: targetID}, relayID, "auto", &opts)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := store.RelayPolicy(t.Context(), relayID, time.Now().UTC())
	if err != nil || len(policy.Grants) != 1 || opts.NodeProxy != "" {
		t.Fatalf("hub got node credentials: %+v %v", policy, err)
	}
	if !strings.HasPrefix(policy.Grants[0].Username, "install-") {
		t.Fatalf("unexpected grant: %+v", policy.Grants[0])
	}
	finish(true, false)
	policy, err = store.RelayPolicy(t.Context(), relayID, time.Now().UTC())
	if err != nil || len(policy.Grants) != 0 {
		t.Fatalf("hub installation left relay access: %+v %v", policy, err)
	}
}

func TestSeparateHubDoesNotReserveNodeIdentityOrReplaceConnectedNode(t *testing.T) {
	svc, _, connectedID, targetID := relayFixture(t)
	ca, err := transport.NewCertificateAuthority(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	svc.Authority = ca
	opts := svc.optionsFor(installRequest{ServerID: string(targetID), Role: "hub"})
	if len(opts.NodeIdentityJSON) != 0 {
		t.Fatal("hub received node identity")
	}
	if _, err := ca.IssueEnrollment(targetID, time.Now().UTC()); err != nil {
		t.Fatalf("hub reserved a node identity: %v", err)
	}
	svc.Executor = SSHInstallerFunc(func(context.Context, install.SSHInstallOptions) (install.SSHInstallResult, error) {
		return install.SSHInstallResult{}, nil
	})
	_, err = svc.Enqueue(t.Context(), installRequest{ServerID: string(connectedID), Host: "hub.example.test", Port: 22, User: "root", Password: "fixture", Role: "hub", IdempotencyKey: "separate-hub-role-guard"})
	if err == nil || !strings.Contains(err.Error(), "never-connected") {
		t.Fatalf("connected node can be overwritten: %v", err)
	}
}

func TestSeparateHubCompletionDoesNotNeedBrowserCleanup(t *testing.T) {
	svc, store, _, targetID := relayFixture(t)
	svc.Executor = SSHInstallerFunc(func(_ context.Context, opts install.SSHInstallOptions) (install.SSHInstallResult, error) {
		if opts.Role != "hub" || len(opts.NodeIdentityJSON) != 0 {
			t.Fatalf("wrong independent hub options: role=%s identity=%v", opts.Role, len(opts.NodeIdentityJSON) > 0)
		}
		return install.SSHInstallResult{Stage: "complete", ServicesVerified: true, Preflight: install.Preflight{Architecture: "amd64"}}, nil
	})
	job, err := svc.Enqueue(t.Context(), installRequest{ServerID: string(targetID), Host: "hub.example.test", Port: 22, User: "root", Password: "fixture", Role: "hub", Start: true, IdempotencyKey: "hub-install-cleanup"})
	if err != nil {
		t.Fatal(err)
	}
	job, err = svc.RunOnce(t.Context(), job.ID)
	if err != nil || job.State != contracts.JobSucceeded {
		t.Fatalf("hub failed: %+v %v", job, err)
	}
	if _, found, err := store.GetServer(t.Context(), targetID); err != nil || found {
		t.Fatalf("hub left Pending placeholder: found=%v err=%v", found, err)
	}
}
