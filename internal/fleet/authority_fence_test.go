package fleet

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/transport"
)

func fencedFleetFixture(t *testing.T) (*monitoring.Store, contracts.Server, *transport.CertificateAuthority, time.Time) {
	t.Helper()
	ctx := context.Background()
	store, err := monitoring.OpenStore(ctx, ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	server := contracts.Server{ID: "server-fence-fleet-001", Name: "Node", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "never-connected", FreshnessState: "unknown"}
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	ca, err := transport.NewCertificateAuthority(now)
	if err != nil {
		t.Fatal(err)
	}
	return store, server, ca, now
}

func freezeFleetServer(t *testing.T, store *monitoring.Store, server contracts.Server) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.InitializeServerAuthority(ctx, server.ID, "owner-hub-aaaa"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FreezeServerAuthority(ctx, monitoring.AuthorityTransition{ServerID: server.ID, CutoverID: "cutover-0001", RequestDigest: strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
}

func issueToken(service *EnrollmentService, server contracts.Server, key string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/servers/"+string(server.ID)+"/enrollment-token", bytes.NewBufferString(`{"idempotency_key":"`+key+`"}`))
	response := httptest.NewRecorder()
	service.TokenHandler().ServeHTTP(response, request)
	return response
}

func TestEnrollmentTokenRefusedWhenThisHubNoLongerOwnsTheServer(t *testing.T) {
	store, server, ca, now := fencedFleetFixture(t)
	service, err := NewEnrollmentService(store, ca)
	if err != nil {
		t.Fatal(err)
	}
	service.Now = func() time.Time { return now }
	freezeFleetServer(t, store, server)
	response := issueToken(service, server, "operator-token-fence")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "server_not_authoritative") {
		t.Fatalf("a hub that no longer owns the server issued a token: status=%d body=%s", response.Code, response.Body.String())
	}
	// Nothing was reserved in the CA, so the rightful owner is not blocked.
	if _, err := ca.IssueEnrollment(server.ID, now); err != nil {
		t.Fatalf("a refused request left an enrollment reservation: %v", err)
	}
}

func TestEnrollmentTokenStillIssuedToTheOwningHub(t *testing.T) {
	store, server, ca, now := fencedFleetFixture(t)
	service, err := NewEnrollmentService(store, ca)
	if err != nil {
		t.Fatal(err)
	}
	service.Now = func() time.Time { return now }
	if _, err := store.InitializeServerAuthority(context.Background(), server.ID, "owner-hub-aaaa"); err != nil {
		t.Fatal(err)
	}
	if response := issueToken(service, server, "operator-token-owner"); response.Code != http.StatusCreated {
		t.Fatalf("the owning hub could not issue a token: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestInstallDoesNotIssueANodeIdentityForAServerThisHubNoLongerOwns(t *testing.T) {
	store, server, ca, now := fencedFleetFixture(t)
	svc, err := NewInstallService(store)
	if err != nil {
		t.Fatal(err)
	}
	svc.Authority = ca
	svc.Now = func() time.Time { return now }
	owned := svc.optionsFor(installRequest{ServerID: string(server.ID), Role: "node"})
	if len(owned.NodeIdentityJSON) == 0 {
		t.Fatal("an unowned (legacy) server lost node identity issuance")
	}
	freezeFleetServer(t, store, server)
	// A fresh CA would issue an identity if nothing stopped it, so an empty
	// result can only come from the ownership check.
	fresh, err := transport.NewCertificateAuthority(now)
	if err != nil {
		t.Fatal(err)
	}
	svc.Authority = fresh
	fenced := svc.optionsFor(installRequest{ServerID: string(server.ID), Role: "node"})
	if len(fenced.NodeIdentityJSON) != 0 {
		t.Fatal("a node identity was issued for a server this hub no longer owns")
	}
}
