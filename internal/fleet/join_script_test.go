package fleet

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/netproxy"
)

const fixtureTrustPEM = "-----BEGIN CERTIFICATE-----\nMIIBfixture\n-----END CERTIFICATE-----\n"

func joinFixture(t *testing.T) (*InstallService, contracts.ServerID, contracts.ServerID, contracts.Job) {
	t.Helper()
	svc, store, relayID, targetID := relayFixture(t)
	svc.TransportURL, svc.HubTrustPEM = "wss://hub.example.test:9797/node/v1", []byte(fixtureTrustPEM)
	svc.relayReady = func(context.Context, *netproxy.Proxy, string) error { return nil }
	now := time.Now().UTC()
	job, _, err := store.CreateJob(context.Background(), contracts.Job{ID: "enroll-join-0001", Kind: "enrollment", State: contracts.JobQueued, IdempotencyKey: "join", TargetServerID: targetID, ExpiresAt: now.Add(10 * time.Minute)}, "hash", now)
	if err != nil {
		t.Fatal(err)
	}
	return svc, relayID, targetID, job
}

func TestJoinScriptThroughRelay(t *testing.T) {
	svc, relayID, targetID, job := joinFixture(t)
	ctx := context.Background()
	response, err := svc.CreateJoinScript(ctx, targetID, joinScriptRequest{JobID: job.ID, Token: "tok_abcdefghijklmnop", RelayServerID: string(relayID)}, "https://dash.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(response.Command, "curl -fsSL -x 'http://node-"+string(targetID)+":") || !strings.Contains(response.Command, "@203.0.113.5:9798' https://dash.example.test/api/v1/join-scripts/") || !strings.HasSuffix(response.Command, " | sudo sh") {
		t.Fatalf("command = %q", response.Command)
	}
	policy, err := svc.Store.RelayPolicy(ctx, relayID, time.Now().UTC())
	if err != nil || len(policy.Grants) != 1 || strings.Join(policy.Grants[0].Destinations, ",") != "hub.example.test:9797,dash.example.test:443" || policy.Grants[0].ExpiresAt != nil {
		t.Fatalf("relay policy = %+v, %v", policy, err)
	}

	handler := svc.JoinScriptDownloads()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, strings.TrimPrefix(response.ScriptURL, "https://dash.example.test"), nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("script status %d: %s", recorder.Code, recorder.Body.String())
	}
	script := recorder.Body.String()
	for _, want := range []string{"PAYESH_BOOTSTRAP_JOB=" + job.ID, "PAYESH_BOOTSTRAP_TOKEN=tok_abcdefghijklmnop", "TRANSPORT_URL='wss://hub.example.test:9797/node/v1'", "PROXY='http://node-" + string(targetID), fixtureTrustPEM + "PAYESH_HUB_CA", "--role node"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script lacks %q:\n%s", want, script)
		}
	}
	path := filepath.Join(t.TempDir(), "join.sh")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("script is not valid shell: %v\n%s", err, out)
	}

	for _, bad := range []string{"/api/v1/join-scripts/" + strings.Repeat("0", 48), "/api/v1/join-scripts/short", strings.TrimPrefix(response.ScriptURL, "https://dash.example.test") + "/mips.manifest"} {
		recorder = httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, bad, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d", bad, recorder.Code)
		}
	}
}

func TestJoinScriptDirectAndValidation(t *testing.T) {
	svc, _, targetID, job := joinFixture(t)
	ctx := context.Background()
	response, err := svc.CreateJoinScript(ctx, targetID, joinScriptRequest{JobID: job.ID, Token: "tok_abcdefghijklmnop"}, "https://dash.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(response.Command, " -x ") || response.Relay != "" {
		t.Fatalf("direct command uses a proxy: %q", response.Command)
	}
	if _, err := svc.CreateJoinScript(ctx, targetID, joinScriptRequest{JobID: job.ID, Token: "tok_abcdefghijklmnop"}, "http://dash.example.test"); err == nil {
		t.Fatal("plain HTTP dashboard accepted")
	}
	if _, err := svc.CreateJoinScript(ctx, targetID, joinScriptRequest{JobID: job.ID, Token: "bad token with 'quote'"}, "https://dash.example.test"); err == nil {
		t.Fatal("unsafe token accepted")
	}
	if _, err := svc.CreateJoinScript(ctx, "server-other-target-01", joinScriptRequest{JobID: job.ID, Token: "tok_abcdefghijklmnop"}, "https://dash.example.test"); err == nil {
		t.Fatal("enrollment for another server accepted")
	}
	svc.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := svc.CreateJoinScript(ctx, targetID, joinScriptRequest{JobID: job.ID, Token: "tok_abcdefghijklmnop"}, "https://dash.example.test"); err == nil {
		t.Fatal("expired enrollment accepted")
	}
	// The script itself also stops working after expiry.
	recorder := httptest.NewRecorder()
	svc.JoinScriptDownloads().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, strings.TrimPrefix(response.ScriptURL, "https://dash.example.test"), nil))
	body, _ := io.ReadAll(recorder.Body)
	if recorder.Code != http.StatusNotFound || !strings.Contains(string(body), "expired") {
		t.Fatalf("expired script: %d %s", recorder.Code, body)
	}
}

func TestListRelaysShowsConfiguredRelays(t *testing.T) {
	svc, relayID, targetID, job := joinFixture(t)
	ctx := context.Background()
	if relays, _ := svc.Store.ListRelays(ctx); len(relays) != 0 {
		t.Fatalf("relays before use: %+v", relays)
	}
	if _, err := svc.CreateJoinScript(ctx, targetID, joinScriptRequest{JobID: job.ID, Token: "tok_abcdefghijklmnop", RelayServerID: string(relayID)}, "https://dash.example.test"); err != nil {
		t.Fatal(err)
	}
	relays, err := svc.Store.ListRelays(ctx)
	if err != nil || len(relays) != 1 || relays[0].ServerID != relayID || relays[0].Nodes != 1 || relays[0].Port != 9798 || relays[0].Address != "203.0.113.5" {
		t.Fatalf("relays = %+v, %v", relays, err)
	}
}
