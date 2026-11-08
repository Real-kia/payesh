package fleet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/install"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/netproxy"
)

// A join link is the reusable form of a join command. Running it on any
// number of servers adds each one to the hub: every fetch mints a server, its
// own single-use enrollment and, when a relay is chosen, its own relay
// credential, then serves the ordinary per-server join script. The link
// itself only grants the right to mint, so it is bounded by an expiry and a
// use limit and can be stopped at any time.

const (
	maxJoinLinks       = 16
	defaultJoinLinkUse = 20
	maxJoinLinkUses    = 200
	maxJoinServerName  = 64
)

type joinLink struct {
	ID        string
	Command   string
	Relay     string // relay name, for display
	RelayID   contracts.ServerID
	NodeRoute string
	BaseURL   string
	Transport string
	TrustPEM  string
	HubWeb    string
	HubNode   string
	// SharedUser is the relay credential the first download goes through
	// before the hub knows which server is calling.
	SharedUser string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	MaxUses    int
	Uses       int
	Revoked    bool
	// latest is when the last script minted from this link expires; the shared
	// bundles must stay available until then.
	latest  time.Time
	minted  map[string]contracts.ServerID // requested name -> still-pending server
	bundles map[string]install.ArtifactDownload
}

type joinLinkRequest struct {
	RelayServerID string `json:"relay_server_id,omitempty"`
	NodeRoute     string `json:"node_route,omitempty"`
	MaxUses       int    `json:"max_uses,omitempty"`
}

type joinLinkView struct {
	ID        string    `json:"id"`
	Command   string    `json:"command"`
	Relay     string    `json:"relay,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	MaxUses   int       `json:"max_uses"`
	Uses      int       `json:"uses"`
}

func (l *joinLink) view() joinLinkView {
	return joinLinkView{ID: l.ID, Command: l.Command, Relay: l.Relay, CreatedAt: l.CreatedAt, ExpiresAt: l.ExpiresAt, MaxUses: l.MaxUses, Uses: l.Uses}
}

func (l *joinLink) closeBundles() {
	for _, bundle := range l.bundles {
		if bundle.Close != nil {
			bundle.Close()
		}
	}
}

// JoinLinksHandler serves /api/v1/join-links (POST create, GET list) and
// /api/v1/join-links/{id} (DELETE stop), behind the session middleware.
func (s *InstallService) JoinLinksHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/"), "/api/v1/join-links")
		id = strings.TrimPrefix(id, "/")
		switch {
		case id == "" && r.Method == http.MethodPost:
			var request joinLinkRequest
			if !decode(w, r, &request) {
				return
			}
			link, err := s.CreateJoinLink(r.Context(), request, s.requestBaseURL(r))
			if err != nil {
				writeFleetError(w, http.StatusBadRequest, "join_link_failed", err.Error(), false)
				return
			}
			writeStorageJSON(w, http.StatusCreated, link)
		case id == "" && r.Method == http.MethodGet:
			writeStorageJSON(w, http.StatusOK, map[string]any{"items": s.ListJoinLinks()})
		case id != "" && r.Method == http.MethodDelete:
			if !s.RevokeJoinLink(r.Context(), id) {
				writeFleetError(w, http.StatusNotFound, "not_found", "join link not found", false)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("Allow", "GET, POST, DELETE")
			writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
		}
	})
}

// putSharedLinkGrant gives the link one relay credential, limited to the hub's
// web address and to the link's lifetime, for the first download.
func (s *InstallService) putSharedLinkGrant(ctx context.Context, link *joinLink, host string, port int, now time.Time) (*netproxy.Proxy, error) {
	user, password := "join-"+link.ID[:16], randomRelaySecret()
	grant := monitoring.RelayGrantRecord{RelayServerID: link.RelayID, Username: user, PasswordSHA256: netproxy.HashPassword(password), Destinations: []string{link.HubWeb}, ExpiresAt: link.ExpiresAt}
	if err := s.Store.PutRelayGrant(ctx, grant, now); err != nil {
		return nil, fmt.Errorf("configure relay: %w", err)
	}
	link.SharedUser = user
	return &netproxy.Proxy{Scheme: "http", Host: net.JoinHostPort(host, fmt.Sprint(port)), Username: user, Password: password}, nil
}

func (s *InstallService) joinTTL(ctx context.Context) time.Duration {
	if settings, ok, err := s.Store.StorageSettings(ctx); err == nil && ok && settings.JoinCommandExpiryHours > 0 {
		return time.Duration(settings.JoinCommandExpiryHours) * time.Hour
	}
	return defaultEnrollmentTTL
}

// CreateJoinLink registers a reusable join command.
func (s *InstallService) CreateJoinLink(ctx context.Context, request joinLinkRequest, baseURL string) (joinLinkView, error) {
	if s.Authority == nil {
		return joinLinkView{}, errors.New("this hub cannot enroll servers yet")
	}
	switch request.NodeRoute {
	case "", "auto", "proxy", "direct":
	default:
		return joinLinkView{}, errors.New("node_route must be auto, proxy or direct")
	}
	if request.NodeRoute == "" {
		request.NodeRoute = "auto"
	}
	if request.MaxUses == 0 {
		request.MaxUses = defaultJoinLinkUse
	}
	if request.MaxUses < 1 || request.MaxUses > maxJoinLinkUses {
		return joinLinkView{}, fmt.Errorf("max_uses must be between 1 and %d", maxJoinLinkUses)
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return joinLinkView{}, errors.New("open the dashboard over HTTPS to create a join command")
	}
	transportURL, trustPEM := s.TransportURL, s.HubTrustPEM
	if s.ResolveTransport != nil {
		if transportURL, trustPEM, err = s.ResolveTransport(); err != nil {
			return joinLinkView{}, err
		}
	}
	hubNode, err := transportHostPort(transportURL)
	if err != nil {
		return joinLinkView{}, err
	}
	if len(trustPEM) == 0 {
		return joinLinkView{}, errors.New("the hub's node trust certificate is not configured")
	}
	webPort := base.Port()
	if webPort == "" {
		webPort = "443"
	}
	now := s.now()
	link := &joinLink{
		NodeRoute: request.NodeRoute, BaseURL: strings.TrimRight(base.String(), "/"), Transport: transportURL, TrustPEM: string(trustPEM),
		HubWeb: net.JoinHostPort(base.Hostname(), webPort), HubNode: hubNode,
		CreatedAt: now, ExpiresAt: now.Add(s.joinTTL(ctx)), MaxUses: request.MaxUses,
		minted: map[string]contracts.ServerID{}, bundles: map[string]install.ArtifactDownload{},
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return joinLinkView{}, err
	}
	link.ID = hex.EncodeToString(raw)

	s.mu.Lock()
	open := 0
	for _, existing := range s.joinLinks {
		if !existing.Revoked && existing.ExpiresAt.After(now) {
			open++
		}
	}
	s.mu.Unlock()
	if open >= maxJoinLinks {
		return joinLinkView{}, errors.New("too many open join links; stop one you no longer need")
	}

	fetch := "curl -fsSL"
	if request.RelayServerID != "" {
		link.RelayID = contracts.ServerID(request.RelayServerID)
		if err := s.validateRelay(ctx, link.RelayID, ""); err != nil {
			return joinLinkView{}, err
		}
		relay, _, err := s.Store.GetServer(ctx, link.RelayID)
		if err != nil {
			return joinLinkView{}, err
		}
		link.Relay = relay.Name
		host, err := relayHost(relay)
		if err != nil {
			return joinLinkView{}, err
		}
		port, err := s.Store.EnsureRelayNode(ctx, link.RelayID, monitoring.DefaultRelayPort, now)
		if err != nil {
			return joinLinkView{}, fmt.Errorf("configure relay: %w", err)
		}
		proxy, err := s.putSharedLinkGrant(ctx, link, host, port, now)
		if err != nil {
			return joinLinkView{}, err
		}
		if err := s.waitForRelay(ctx, proxy, link.HubWeb, link.Relay, port); err != nil {
			_ = s.Store.DeleteRelayGrant(ctx, link.RelayID, link.SharedUser)
			return joinLinkView{}, err
		}
		fetch += " -x '" + proxy.String() + "'"
	}
	// $(hostname) expands on the server, so each one is named after itself.
	link.Command = fetch + ` "` + link.BaseURL + "/api/v1/join-scripts/" + link.ID + `?name=$(hostname)" | sudo sh`

	s.mu.Lock()
	if s.joinLinks == nil {
		s.joinLinks = map[string]*joinLink{}
	}
	s.joinLinks[link.ID] = link
	s.mu.Unlock()
	return link.view(), nil
}

// ListJoinLinks returns the links that can still add servers, newest first.
func (s *InstallService) ListJoinLinks() []joinLinkView {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	items := []joinLinkView{}
	for _, link := range s.joinLinks {
		if !link.Revoked && link.ExpiresAt.After(now) {
			items = append(items, link.view())
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	return items
}

// RevokeJoinLink stops a link from adding servers. Servers already added keep
// their own commands until those expire.
func (s *InstallService) RevokeJoinLink(ctx context.Context, id string) bool {
	s.mu.Lock()
	link, ok := s.joinLinks[id]
	if !ok || link.Revoked {
		s.mu.Unlock()
		return false
	}
	link.Revoked = true
	relayID, user := link.RelayID, link.SharedUser
	s.mu.Unlock()
	if relayID != "" && user != "" {
		_ = s.Store.DeleteRelayGrant(ctx, relayID, user)
	}
	return true
}

// mintFromLink adds one server for a request to a join link and returns the
// single-use script that installs it.
func (s *InstallService) mintFromLink(ctx context.Context, link *joinLink, requestedName string) (*joinScript, error) {
	name := joinServerName(requestedName)
	now := s.now()
	s.mu.Lock()
	if link.Revoked || !link.ExpiresAt.After(now) {
		s.mu.Unlock()
		return nil, errors.New("this join command has expired; create a new one in the dashboard")
	}
	if link.Uses >= link.MaxUses {
		s.mu.Unlock()
		return nil, errors.New("this join command has reached its server limit; create a new one in the dashboard")
	}
	link.Uses++
	previous := link.minted[name]
	s.mu.Unlock()

	var serverID contracts.ServerID
	var revision uint64
	var relayGranted bool
	failed := true
	defer func() {
		if !failed {
			return
		}
		s.mu.Lock()
		link.Uses--
		s.mu.Unlock()
		if serverID == "" {
			return
		}
		if relayGranted {
			_ = s.Store.DeleteRelayGrant(context.WithoutCancel(ctx), link.RelayID, "node-"+string(serverID))
		}
		_ = s.Authority.RevokeServer(serverID)
		_ = s.Store.DeleteServer(context.WithoutCancel(ctx), serverID, revision)
	}()

	// Running the command again on the same machine replaces its unused entry
	// instead of leaving a duplicate behind.
	if previous != "" {
		s.discardPendingServer(ctx, link, previous)
	}
	if name == "" {
		name = "node-" + randomHex(3)
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, err
	}
	serverID = contracts.ServerID("server-" + hex.EncodeToString(idBytes))
	server := contracts.Server{ID: serverID, Name: name, Address: "joins by command", Role: "node", Platform: "unknown", Architecture: "unknown", Capabilities: []string{"metrics", "traffic"}, ConnectionState: "never-connected", FreshnessState: "unknown", FreshnessReason: "awaiting automatic platform detection"}
	if err := s.Store.EnsureServer(ctx, server); err != nil {
		serverID = ""
		return nil, errors.New("could not add the server")
	}
	enrollment, err := s.Authority.IssueEnrollmentWithTTL(serverID, now, s.joinTTL(ctx))
	if err != nil {
		return nil, errors.New("could not issue an enrollment for this server")
	}
	key := "join-link-" + randomHex(8)
	job, _, err := s.Store.CreateJob(ctx, contracts.Job{
		ID: newEnrollmentJobID(), Kind: enrollmentJobKind, State: contracts.JobQueued,
		IdempotencyKey: key, TargetServerID: serverID, ExpiresAt: enrollment.ExpiresAt,
	}, enrollmentRequestHash(serverID, enrollmentRequest{Token: enrollment.Token, IdempotencyKey: key}), now)
	if err != nil {
		return nil, errors.New("could not create the enrollment job")
	}
	script := &joinScript{
		ServerID: serverID, JobID: job.ID, Token: enrollment.Token, TransportURL: link.Transport, TrustPEM: link.TrustPEM,
		BaseURL: link.BaseURL, NodeRoute: link.NodeRoute, ExpiresAt: enrollment.ExpiresAt, bundles: link.bundles, shared: true,
	}
	if link.RelayID != "" {
		proxy, _, err := s.issueNodeRelayGrant(ctx, link.RelayID, serverID, []string{link.HubNode, link.HubWeb})
		if err != nil {
			return nil, err
		}
		relayGranted = true
		if err := s.waitForRelay(ctx, proxy, link.HubWeb, link.Relay, relayPortOf(proxy)); err != nil {
			return nil, err
		}
		script.Proxy = proxy.String()
	}
	scriptID := make([]byte, 24)
	if _, err := rand.Read(scriptID); err != nil {
		return nil, err
	}
	script.ID = hex.EncodeToString(scriptID)

	s.mu.Lock()
	if s.joinScripts == nil {
		s.joinScripts = map[string]*joinScript{}
	}
	s.joinScripts[script.ID] = script
	link.minted[name] = serverID
	if enrollment.ExpiresAt.After(link.latest) {
		link.latest = enrollment.ExpiresAt
	}
	s.mu.Unlock()
	failed = false
	return script, nil
}

// discardPendingServer removes a server this link added earlier if it has
// still never connected.
func (s *InstallService) discardPendingServer(ctx context.Context, link *joinLink, id contracts.ServerID) {
	server, found, err := s.Store.GetServer(ctx, id)
	if err != nil || !found || server.LastHeartbeat != nil || server.ConnectionState != "never-connected" {
		return
	}
	if link.RelayID != "" {
		_ = s.Store.DeleteRelayGrant(ctx, link.RelayID, "node-"+string(id))
	}
	_ = s.Authority.RevokeServer(id)
	_ = s.Store.DeleteServer(ctx, id, server.ConfigurationRevision)
}

// joinServerName keeps a hostname usable as a display name.
func joinServerName(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		}
		if b.Len() >= maxJoinServerName {
			break
		}
	}
	return strings.Trim(b.String(), "._-")
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// sweepJoinLinks forgets links once nothing minted from them can still be
// downloading their shared bundles.
func (s *InstallService) sweepJoinLinks(now time.Time) {
	for key, link := range s.joinLinks {
		done := link.ExpiresAt
		if link.latest.After(done) {
			done = link.latest
		}
		if (link.Revoked || !link.ExpiresAt.After(now)) && !done.After(now) {
			link.closeBundles()
			delete(s.joinLinks, key)
		}
	}
}
