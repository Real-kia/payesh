package fleet

import (
	"bytes"
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
	"text/template"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/install"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/netproxy"
)

// A join script lets an operator add a server the hub cannot reach by
// running one command on it. Everything (script, binaries, enrollment) comes
// from the hub, optionally through a relay node, so the server needs neither
// GitHub nor international access.

const maxJoinScripts = 32

type joinScript struct {
	ID           string
	ServerID     contracts.ServerID
	JobID        string
	Token        string
	TransportURL string
	TrustPEM     string
	BaseURL      string
	Proxy        string // full relay URL with credentials; empty for direct
	NodeRoute    string
	ExpiresAt    time.Time
	bundles      map[string]install.ArtifactDownload
}

type joinScriptRequest struct {
	JobID         string `json:"job_id"`
	Token         string `json:"token"`
	RelayServerID string `json:"relay_server_id,omitempty"`
	NodeRoute     string `json:"node_route,omitempty"`
}

type joinScriptResponse struct {
	Command   string    `json:"command"`
	ScriptURL string    `json:"script_url"`
	ExpiresAt time.Time `json:"expires_at"`
	Relay     string    `json:"relay,omitempty"`
}

// JoinScriptHandler serves POST /api/v1/servers/{id}/join-script (behind the
// session/token middleware).
func (s *InstallService) JoinScriptHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		serverID, ok := serverIDForSuffix(r.URL.Path, "/join-script")
		if !ok {
			writeFleetError(w, http.StatusNotFound, "not_found", "resource not found", false)
			return
		}
		var request joinScriptRequest
		if !decode(w, r, &request) {
			return
		}
		response, err := s.CreateJoinScript(r.Context(), serverID, request, s.requestBaseURL(r))
		if err != nil {
			writeFleetError(w, http.StatusBadRequest, "join_script_failed", err.Error(), false)
			return
		}
		writeStorageJSON(w, http.StatusCreated, response)
	})
}

// requestBaseURL is the dashboard origin as the operator reached it.
func (s *InstallService) requestBaseURL(r *http.Request) string {
	if s.DownloadBaseURL != "" {
		return strings.TrimRight(s.DownloadBaseURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if origin, err := url.Parse(r.Header.Get("Origin")); err == nil && origin.Host == r.Host && origin.Scheme == "https" {
		scheme = "https"
	}
	// A local reverse proxy (nginx on the same host) terminates TLS.
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback() && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// CreateJoinScript prepares the relay (if any) and registers a one-time
// script for an already issued enrollment job and token.
func (s *InstallService) CreateJoinScript(ctx context.Context, serverID contracts.ServerID, request joinScriptRequest, baseURL string) (joinScriptResponse, error) {
	if len(request.JobID) == 0 || len(request.JobID) > 128 || !safeFleetID(request.JobID) || len(request.Token) < 16 || len(request.Token) > 512 || strings.ContainsAny(request.Token, " '\"\\$`\n") {
		return joinScriptResponse{}, errors.New("job_id and token from the enrollment are required")
	}
	switch request.NodeRoute {
	case "", "auto", "proxy", "direct":
	default:
		return joinScriptResponse{}, errors.New("node_route must be auto, proxy or direct")
	}
	if request.NodeRoute == "" {
		request.NodeRoute = "auto"
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return joinScriptResponse{}, errors.New("open the dashboard over HTTPS to create a join command")
	}
	job, found, err := s.Store.GetJob(ctx, request.JobID)
	if err != nil {
		return joinScriptResponse{}, err
	}
	if !found || job.Kind != "enrollment" || job.TargetServerID != serverID || job.State != contracts.JobQueued || !job.ExpiresAt.After(s.now()) {
		return joinScriptResponse{}, errors.New("the enrollment for this server is missing or expired; create a new command")
	}
	transportURL, trustPEM := s.TransportURL, s.HubTrustPEM
	if s.ResolveTransport != nil {
		if transportURL, trustPEM, err = s.ResolveTransport(); err != nil {
			return joinScriptResponse{}, err
		}
	}
	hubTransport, err := transportHostPort(transportURL)
	if err != nil {
		return joinScriptResponse{}, err
	}
	if len(trustPEM) == 0 {
		return joinScriptResponse{}, errors.New("the hub's node trust certificate is not configured")
	}
	script := &joinScript{ServerID: serverID, JobID: job.ID, Token: request.Token, TransportURL: transportURL, TrustPEM: string(trustPEM), BaseURL: strings.TrimRight(base.String(), "/"), NodeRoute: request.NodeRoute, ExpiresAt: job.ExpiresAt, bundles: map[string]install.ArtifactDownload{}}
	response := joinScriptResponse{ExpiresAt: job.ExpiresAt}
	var proxy *netproxy.Proxy
	if request.RelayServerID != "" {
		relayID := contracts.ServerID(request.RelayServerID)
		webPort := base.Port()
		if webPort == "" {
			webPort = "443"
		}
		hubWeb := net.JoinHostPort(base.Hostname(), webPort)
		proxy, response.Relay, err = s.issueNodeRelayGrant(ctx, relayID, serverID, []string{hubTransport, hubWeb})
		if err != nil {
			return joinScriptResponse{}, err
		}
		// The command itself goes through the relay, so wait until the relay
		// has the credential.
		if err := s.waitForRelay(ctx, proxy, hubWeb, response.Relay, relayPortOf(proxy)); err != nil {
			return joinScriptResponse{}, err
		}
		script.Proxy = proxy.String()
	}
	id := make([]byte, 24)
	if _, err := rand.Read(id); err != nil {
		return joinScriptResponse{}, err
	}
	script.ID = hex.EncodeToString(id)
	s.mu.Lock()
	if s.joinScripts == nil {
		s.joinScripts = map[string]*joinScript{}
	}
	now := s.now()
	for key, existing := range s.joinScripts {
		if !existing.ExpiresAt.After(now) {
			existing.close()
			delete(s.joinScripts, key)
		}
	}
	if len(s.joinScripts) >= maxJoinScripts {
		s.mu.Unlock()
		return joinScriptResponse{}, errors.New("too many open join commands; wait for older ones to expire")
	}
	s.joinScripts[script.ID] = script
	s.mu.Unlock()

	response.ScriptURL = script.BaseURL + "/api/v1/join-scripts/" + script.ID
	fetch := "curl -fsSL"
	if proxy != nil {
		fetch += " -x '" + proxy.String() + "'"
	}
	response.Command = fetch + " " + response.ScriptURL + " | sudo sh"
	return response, nil
}

// issueNodeRelayGrant makes relayID a relay (if it is not already) and
// returns a permanent credential for serverID limited to destinations.
func (s *InstallService) issueNodeRelayGrant(ctx context.Context, relayID, serverID contracts.ServerID, destinations []string) (*netproxy.Proxy, string, error) {
	if err := s.validateRelay(ctx, relayID, serverID); err != nil {
		return nil, "", err
	}
	relay, _, err := s.Store.GetServer(ctx, relayID)
	if err != nil {
		return nil, "", err
	}
	host, err := relayHost(relay)
	if err != nil {
		return nil, "", err
	}
	now := s.now()
	port, err := s.Store.EnsureRelayNode(ctx, relayID, monitoring.DefaultRelayPort, now)
	if err != nil {
		return nil, "", fmt.Errorf("configure relay: %w", err)
	}
	user, password := "node-"+string(serverID), randomRelaySecret()
	if err := s.Store.PutRelayGrant(ctx, monitoring.RelayGrantRecord{RelayServerID: relayID, Username: user, PasswordSHA256: netproxy.HashPassword(password), Destinations: destinations, TargetServerID: serverID}, now); err != nil {
		return nil, "", fmt.Errorf("configure relay: %w", err)
	}
	return &netproxy.Proxy{Scheme: "http", Host: net.JoinHostPort(host, fmt.Sprint(port)), Username: user, Password: password}, relay.Name, nil
}

func relayPortOf(p *netproxy.Proxy) int {
	_, port, err := net.SplitHostPort(p.Host)
	if err != nil {
		return monitoring.DefaultRelayPort
	}
	var value int
	fmt.Sscan(port, &value)
	return value
}

func (j *joinScript) close() {
	for _, bundle := range j.bundles {
		if bundle.Close != nil {
			bundle.Close()
		}
	}
}

// JoinScriptDownloads serves GET /api/v1/join-scripts/{id} (the script) and
// /api/v1/join-scripts/{id}/{arch}.manifest (bundle address and pins). The
// unguessable id is the capability; it expires with the enrollment.
func (s *InstallService) JoinScriptDownloads() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/join-scripts/"), "/")
		if len(parts) < 1 || len(parts) > 2 || len(parts[0]) != 48 {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		script, ok := s.joinScripts[parts[0]]
		s.mu.Unlock()
		if !ok || !script.ExpiresAt.After(s.now()) {
			http.Error(w, "this join command has expired; create a new one in the dashboard", http.StatusNotFound)
			return
		}
		if len(parts) == 1 {
			body, err := renderJoinScript(script)
			if err != nil {
				http.Error(w, "could not build the join script", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
			_, _ = w.Write(body)
			return
		}
		arch := strings.TrimSuffix(parts[1], ".manifest")
		if arch != "amd64" && arch != "arm64" || parts[1] != arch+".manifest" {
			http.NotFound(w, r)
			return
		}
		manifest, err := s.joinManifest(r.Context(), script, arch)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(manifest))
	})
}

// joinManifest publishes (once per architecture) the node bundle and lists
// its address, checksum and per-artifact pins, one "key value" per line.
func (s *InstallService) joinManifest(ctx context.Context, script *joinScript, arch string) (string, error) {
	s.mu.Lock()
	bundle, ok := script.bundles[arch]
	s.mu.Unlock()
	if !ok {
		if s.Downloads == nil || s.VerifyArtifact == nil {
			return "", errors.New("this hub has no installation files to serve")
		}
		paths, err := install.SelectLinuxArtifacts(s.InstallerPath, s.Artifacts, s.MatrixDir, arch, "node", s.VerifyArtifact)
		if err != nil {
			return "", err
		}
		if bundle, err = s.Downloads.Publish(ctx, script.BaseURL, "node", paths); err != nil {
			return "", err
		}
		s.mu.Lock()
		if existing, raced := script.bundles[arch]; raced {
			s.mu.Unlock()
			bundle.Close()
			bundle = existing
		} else {
			script.bundles[arch] = bundle
			s.mu.Unlock()
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "url %s\nsha256 %s\n", bundle.URL, bundle.SHA256)
	names := make([]string, 0, len(bundle.Digests))
	for name := range bundle.Digests {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&out, "pin %s %s\n", name, bundle.Digests[name])
	}
	return out.String(), nil
}

var joinScriptTemplate = template.Must(template.New("join").Parse(`#!/bin/sh
# Payesh node join script, generated by the hub for one server.
# It expires at {{.ExpiresAt.UTC.Format "2006-01-02 15:04 UTC"}}.
set -eu
die() { echo "payesh join: $*" >&2; exit 1; }
say() { echo "payesh join: $*"; }

[ "$(id -u)" -eq 0 ] || die "run as root, e.g. ... | sudo sh"
command -v curl >/dev/null 2>&1 || die "curl is required"
command -v tar >/dev/null 2>&1 || die "tar is required"
sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

HUB='{{.BaseURL}}'
SCRIPT_URL="$HUB/api/v1/join-scripts/{{.ID}}"
TRANSPORT_URL='{{.TransportURL}}'
PROXY='{{.Proxy}}'
NODE_ROUTE='{{.NodeRoute}}'
case "$(uname -m)" in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*) die "unsupported architecture $(uname -m)" ;;
esac

# Decide how this node reaches the hub: directly when that works, otherwise
# through the relay.
transport_host="${TRANSPORT_URL#wss://}"; transport_host="${transport_host%%/*}"
USE_PROXY=0
if [ -n "$PROXY" ]; then
	case "$NODE_ROUTE" in
	proxy) USE_PROXY=1 ;;
	direct) USE_PROXY=0 ;;
	*) if curl -sk --max-time 8 -o /dev/null "https://$transport_host/"; then USE_PROXY=0; else USE_PROXY=1; fi ;;
	esac
fi

fetch() {
	if curl -fsSL --connect-timeout 15 --max-time 900 --retry 2 -o "$2" "$1" 2>/dev/null; then return 0; fi
	[ -n "$PROXY" ] || return 1
	curl -fsSL --connect-timeout 15 --max-time 900 --retry 2 -x "$PROXY" -o "$2" "$1"
}

WORK="$(mktemp -d /var/tmp/payesh-join.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT INT TERM
cd "$WORK"
say "downloading the Payesh node for linux/$ARCH from the hub"
fetch "$SCRIPT_URL/$ARCH.manifest" manifest || die "could not reach the hub"
BUNDLE_URL="$(awk '$1=="url"{print $2}' manifest)"
BUNDLE_SHA="$(awk '$1=="sha256"{print $2}' manifest)"
[ -n "$BUNDLE_URL" ] && [ -n "$BUNDLE_SHA" ] || die "the hub could not prepare installation files: $(cat manifest)"
fetch "$BUNDLE_URL" bundle.tar.gz || die "download failed"
[ "$(sha bundle.tar.gz)" = "$BUNDLE_SHA" ] || die "bundle checksum mismatch"
tar -xzf bundle.tar.gz
PINS=""
for line in $(awk '$1=="pin"{print $2"="$3}' manifest); do
	name="${line%%=*}"; digest="${line#*=}"
	if [ "$name" != web-assets ]; then
		[ "$(sha "$name")" = "$digest" ] || die "checksum mismatch for $name"
		chmod 700 "$name"
	fi
	[ "$name" = payesh-install ] || PINS="$PINS --artifact-sha256 $line"
done

say "installing"
# shellcheck disable=SC2086
./payesh-install --install --role node --artifact-dir "$WORK" $PINS --start >/dev/null

mkdir -p /etc/payesh /var/lib/payesh
umask 077
cat > /var/lib/payesh/hub-ca.pem <<'PAYESH_HUB_CA'
{{.TrustPEM}}PAYESH_HUB_CA
chmod 0644 /var/lib/payesh/hub-ca.pem
{
	if [ -f /etc/payesh/payesh.env ]; then
		grep -v -E '^(PAYESH_TRANSPORT_URL|PAYESH_TRANSPORT_PROXY|PAYESH_BOOTSTRAP_URL|PAYESH_BOOTSTRAP_JOB|PAYESH_BOOTSTRAP_TOKEN|PAYESH_HUB_TRUST_FILE|PAYESH_NODE_IDENTITY_FILE)=' /etc/payesh/payesh.env || true
	fi
	echo "PAYESH_TRANSPORT_URL=$TRANSPORT_URL"
	echo "PAYESH_BOOTSTRAP_URL=${TRANSPORT_URL%/node/v1}/node/bootstrap/v1"
	echo 'PAYESH_BOOTSTRAP_JOB={{.JobID}}'
	echo 'PAYESH_BOOTSTRAP_TOKEN={{.Token}}'
	echo 'PAYESH_HUB_TRUST_FILE=/var/lib/payesh/hub-ca.pem'
	echo 'PAYESH_NODE_IDENTITY_FILE=/var/lib/payesh/node-identity.json'
	if [ "$USE_PROXY" = 1 ]; then echo "PAYESH_TRANSPORT_PROXY=$PROXY"; fi
} > "$WORK/payesh.env"
cp "$WORK/payesh.env" /etc/payesh/payesh.env
if id payesh >/dev/null 2>&1; then
	chown root:payesh /etc/payesh/payesh.env
	chown -R payesh:payesh /var/lib/payesh 2>/dev/null || true
fi
chmod 0640 /etc/payesh/payesh.env
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
	systemctl restart payesh-agent
elif command -v rc-service >/dev/null 2>&1; then
	rc-service payesh-agent restart
fi
if [ "$USE_PROXY" = 1 ]; then route="through the relay"; else route="directly"; fi
say "done; this server connects to the hub $route and appears in the dashboard within a minute"
`))

func renderJoinScript(script *joinScript) ([]byte, error) {
	trust := script.TrustPEM
	if !strings.HasSuffix(trust, "\n") {
		trust += "\n"
	}
	copy := *script
	copy.TrustPEM = trust
	var out bytes.Buffer
	if err := joinScriptTemplate.Execute(&out, &copy); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// RelaysHandler serves GET /api/v1/relays: nodes already acting as relays,
// so the dashboard can offer them first.
func (s *InstallService) RelaysHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		relays, err := s.Store.ListRelays(r.Context())
		if err != nil {
			writeFleetError(w, http.StatusInternalServerError, "storage_error", "could not list relays", true)
			return
		}
		writeStorageJSON(w, http.StatusOK, map[string]any{"items": relays})
	})
}
