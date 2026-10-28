package fleet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/nodehub"
	"github.com/Real-kia/payesh/internal/transport"
)

// An SSH installation that failed before connecting leaves a minted but unused
// certificate; the server must still be able to join with a command.
func TestJoinTokenReplacesUnusedInstallIdentity(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ca, err := transport.NewCertificateAuthority(now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nodehub.NewHub(ca, store); err != nil {
		t.Fatal(err)
	}
	svc, err := NewEnrollmentService(store, ca)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(id contracts.ServerID, key string) int {
		recorder := httptest.NewRecorder()
		svc.TokenHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/servers/"+string(id)+"/enrollment-token", strings.NewReader(`{"idempotency_key":"`+key+`"}`)))
		return recorder.Code
	}
	add := func(id contracts.ServerID, state string) {
		server := contracts.Server{ID: id, Name: string(id), Role: "node", Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics"}, ConnectionState: state, FreshnessState: "unknown"}
		if state == "connected" {
			server.FreshnessState, server.LastHeartbeat = "fresh", &now
		}
		if err := store.UpsertServer(ctx, server); err != nil {
			t.Fatal(err)
		}
		if _, err := ca.IssueNodeIdentity(id, now); err != nil {
			t.Fatal(err)
		}
	}

	add("server-failed-install-01", "never-connected")
	if code := issue("server-failed-install-01", "k1"); code != http.StatusCreated {
		t.Fatalf("never-connected server with an unused certificate: status %d", code)
	}
	rechecked, found, err := store.GetServer(ctx, "server-failed-install-01")
	if err != nil || !found {
		t.Fatalf("get server err=%v found=%v", err, found)
	}
	if rechecked.ConnectionState != "never-connected" {
		t.Fatalf("reissued unused certificate turned server into state %q, want never-connected", rechecked.ConnectionState)
	}

	add("server-live-node-000001", "connected")
	if code := issue("server-live-node-000001", "k2"); code != http.StatusConflict {
		t.Fatalf("connected server's certificate was replaced: status %d", code)
	}

	add("server-installing-00001", "never-connected")
	if _, _, err := store.CreateJob(ctx, contracts.Job{ID: "install-running-0001", Kind: sshInstallJobKind, State: contracts.JobRunning, IdempotencyKey: "i", TargetServerID: "server-installing-00001", ExpiresAt: now.Add(time.Hour)}, "h", now); err != nil {
		t.Fatal(err)
	}
	if code := issue("server-installing-00001", "k3"); code != http.StatusConflict {
		t.Fatalf("certificate replaced during a running installation: status %d", code)
	}
}
