package fleet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/transport"
)

func linkFixture(t *testing.T) (*InstallService, string) {
	t.Helper()
	svc, relayID, _, _ := joinFixture(t)
	ca, err := transport.NewCertificateAuthority(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	svc.Authority = ca
	return svc, string(relayID)
}

func fetchLinkScript(svc *InstallService, command, name string) (int, string) {
	start := strings.Index(command, "/api/v1/join-scripts/")
	path := command[start:]
	path = path[:strings.IndexAny(path, `"`)]
	path = strings.Replace(path, "$(hostname)", name, 1)
	recorder := httptest.NewRecorder()
	svc.JoinScriptDownloads().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder.Code, recorder.Body.String()
}

func TestJoinLinkAddsAServerPerUse(t *testing.T) {
	svc, _ := linkFixture(t)
	ctx := context.Background()
	link, err := svc.CreateJoinLink(ctx, joinLinkRequest{MaxUses: 3}, "https://dash.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(link.Command, `"https://dash.example.test/api/v1/join-scripts/`+link.ID+`?name=$(hostname)" | sudo sh`) || strings.Contains(link.Command, " -x ") {
		t.Fatalf("command = %q", link.Command)
	}
	before, _ := svc.Store.ListServers(ctx, 500)
	scripts := map[string]string{}
	for _, name := range []string{"berlin-1", "berlin-2"} {
		code, body := fetchLinkScript(svc, link.Command, name)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", name, code, body)
		}
		scripts[name] = body
	}
	// Each machine gets its own server, job and token.
	token := func(script string) string {
		i := strings.Index(script, "PAYESH_BOOTSTRAP_TOKEN=")
		return strings.SplitN(script[i:], "\n", 2)[0]
	}
	if token(scripts["berlin-1"]) == token(scripts["berlin-2"]) || !strings.Contains(scripts["berlin-1"], "PAYESH_BOOTSTRAP_JOB=enrollment-") {
		t.Fatal("two servers share an enrollment")
	}
	after, _ := svc.Store.ListServers(ctx, 500)
	if len(after) != len(before)+2 {
		t.Fatalf("servers %d -> %d", len(before), len(after))
	}
	names := map[string]bool{}
	for _, server := range after {
		names[server.Name] = true
	}
	if !names["berlin-1"] || !names["berlin-2"] {
		t.Fatalf("servers are not named after their hostnames: %v", names)
	}
	if views := svc.ListJoinLinks(); len(views) != 1 || views[0].Uses != 2 {
		t.Fatalf("links = %+v", views)
	}
}

func TestJoinLinkRerunReplacesPendingServer(t *testing.T) {
	svc, _ := linkFixture(t)
	ctx := context.Background()
	link, err := svc.CreateJoinLink(ctx, joinLinkRequest{}, "https://dash.example.test")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := svc.Store.ListServers(ctx, 500)
	for i := 0; i < 3; i++ {
		if code, body := fetchLinkScript(svc, link.Command, "same-host"); code != http.StatusOK {
			t.Fatalf("run %d: %d %s", i, code, body)
		}
	}
	after, _ := svc.Store.ListServers(ctx, 500)
	if len(after) != len(before)+1 {
		t.Fatalf("re-running on one machine left %d new servers", len(after)-len(before))
	}
}

func TestJoinLinkLimitsExpiryAndRevoke(t *testing.T) {
	svc, _ := linkFixture(t)
	ctx := context.Background()
	link, err := svc.CreateJoinLink(ctx, joinLinkRequest{MaxUses: 1}, "https://dash.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := fetchLinkScript(svc, link.Command, "a"); code != http.StatusOK {
		t.Fatalf("first use: %d", code)
	}
	if code, body := fetchLinkScript(svc, link.Command, "b"); code != http.StatusNotFound || !strings.Contains(body, "limit") {
		t.Fatalf("over the limit: %d %s", code, body)
	}

	stopped, err := svc.CreateJoinLink(ctx, joinLinkRequest{}, "https://dash.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if !svc.RevokeJoinLink(ctx, stopped.ID) || svc.RevokeJoinLink(ctx, stopped.ID) {
		t.Fatal("revoke should succeed once")
	}
	if code, _ := fetchLinkScript(svc, stopped.Command, "c"); code != http.StatusNotFound {
		t.Fatalf("revoked link served a script: %d", code)
	}

	expiring, err := svc.CreateJoinLink(ctx, joinLinkRequest{}, "https://dash.example.test")
	if err != nil {
		t.Fatal(err)
	}
	svc.Now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	if code, _ := fetchLinkScript(svc, expiring.Command, "d"); code != http.StatusNotFound {
		t.Fatalf("expired link served a script: %d", code)
	}
	svc.SweepExpired(svc.now())
	if len(svc.joinLinks) != 0 {
		t.Fatalf("%d links survived the sweep", len(svc.joinLinks))
	}
}

func TestJoinLinkValidationAndHTTPSurface(t *testing.T) {
	svc, relayID := linkFixture(t)
	ctx := context.Background()
	for _, bad := range []joinLinkRequest{{MaxUses: -1}, {MaxUses: maxJoinLinkUses + 1}, {NodeRoute: "sideways"}, {RelayServerID: "server-missing-relay-01"}} {
		if _, err := svc.CreateJoinLink(ctx, bad, "https://dash.example.test"); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if _, err := svc.CreateJoinLink(ctx, joinLinkRequest{}, "http://dash.example.test"); err == nil {
		t.Fatal("plain HTTP dashboard accepted")
	}

	handler := svc.JoinLinksHandler()
	request := httptest.NewRequest(http.MethodPost, "https://dash.example.test/api/v1/join-links", strings.NewReader(`{"relay_server_id":"`+relayID+`","max_uses":2}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated || !strings.Contains(recorder.Body.String(), `-x 'http://join-`) {
		t.Fatalf("create: %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/join-links", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"max_uses":2`) {
		t.Fatalf("list: %d %s", recorder.Code, recorder.Body.String())
	}
	id := svc.ListJoinLinks()[0].ID
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/api/v1/join-links/"+id, nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/api/v1/join-links/"+id, nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("revoke twice: %d", recorder.Code)
	}
}

func TestJoinLinkThroughRelayGivesEachServerItsOwnGrant(t *testing.T) {
	svc, relayID := linkFixture(t)
	ctx := context.Background()
	link, err := svc.CreateJoinLink(ctx, joinLinkRequest{RelayServerID: relayID}, "https://dash.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link.Command, "curl -fsSL -x 'http://join-") || link.Relay != "Iran relay" {
		t.Fatalf("command = %q relay = %q", link.Command, link.Relay)
	}
	code, body := fetchLinkScript(svc, link.Command, "tehran-1")
	if code != http.StatusOK || !strings.Contains(body, "PROXY='http://node-server-") {
		t.Fatalf("relayed script: %d\n%s", code, body)
	}
	policy, err := svc.Store.RelayPolicy(ctx, "server-relay-iran-0001", time.Now().UTC())
	if err != nil || len(policy.Grants) != 2 {
		t.Fatalf("want a shared and a per-server grant, got %+v, %v", policy, err)
	}
	if !svc.RevokeJoinLink(ctx, link.ID) {
		t.Fatal("revoke failed")
	}
	policy, _ = svc.Store.RelayPolicy(ctx, "server-relay-iran-0001", time.Now().UTC())
	if len(policy.Grants) != 1 || !strings.HasPrefix(policy.Grants[0].Username, "node-") {
		t.Fatalf("revoking should drop only the shared grant: %+v", policy.Grants)
	}
}
